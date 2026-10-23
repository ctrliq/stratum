package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/cluster"
	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
	"github.com/ctrliq/stratum/internal/version"
)

// Protocol-boundary tests for the generic read toolset.

// TestMain also takes the worker path's host side effects away from every
// test in the package: cluster.PrepareWorker runs firewall-cmd, which on a
// test host with firewalld running would change its real trusted zone and
// write the production ownership record, and it loads modules and writes
// sysctls. A test that drives the worker path installs its own
// prepareWorkerFn and restores this refusal; one that forgets fails here and
// cannot reach the host.
func TestMain(m *testing.M) {
	prepareWorkerFn = func(context.Context, cluster.WorkerOptions) (*cluster.NodeState, error) {
		return nil, errors.New("cli tests: prepareWorkerFn is not stubbed, and cluster.PrepareWorker would change this host's firewall and kernel settings")
	}
	// The certs commands take the node maintenance lock
	// (internal/nodelock) at /run/stratum-upgrade.lock, which a non-root test
	// run cannot create. Point the whole package's lock path at a temp file,
	// the way internal/certs' and internal/etcdsnap's TestMain fixtures do.
	dir, err := os.MkdirTemp("", "cli-test-lock-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cli tests: temp dir for the node lock: %v\n", err)
		os.Exit(1)
	}
	version.NodeUpgradeLockPath = filepath.Join(dir, "node.lock")
	code := kubetest.Main(m)
	os.RemoveAll(dir)
	os.Exit(code)
}

// k8sSession is a live client session wired to a server whose tools address
// the envtest cluster, plus a typed client for arranging fixtures.
type k8sSession struct {
	cs        *sdk.ClientSession
	typed     kubernetes.Interface
	namespace string
}

func newK8sSession(t *testing.T) *k8sSession {
	t.Helper()
	cfg := kubetest.Start(t)
	kubetest.IsolateHome(t)
	path := kubetest.Kubeconfig(t, cfg, "alpha", "beta")

	conn := kube.NewConnector(kube.Options{Kubeconfig: path})
	srv := mcpsrv.NewServer(mcpsrv.Options{Version: "v0-test"})
	registerAll(newRegistrar(srv, false, false), rootConfig{displayVersion: "v0-test"}, conn, testConnector())

	serverT, clientT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(t.Context(), serverT) }()

	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).
		Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	s := &k8sSession{cs: cs, typed: typedFor(t, cfg)}
	s.namespace = s.createNamespace(t)
	return s
}

// namespaceSeq makes each namespace name unique even when two tests sanitize
// to the same prefix.
var namespaceSeq atomic.Int64

// createNamespace gives each test its own namespace, so list assertions are
// not perturbed by whatever another test left behind. envtest has no
// namespace controller, so deleting one never completes — the namespace is
// left in place and the unique name is what provides the isolation.
func (s *k8sSession) createNamespace(t *testing.T) string {
	t.Helper()
	name := nsName(t.Name())
	_, err := s.typed.CoreV1().Namespaces().Create(t.Context(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create namespace %s: %v", name, err)
	}
	return name
}

// nsName turns a test name into a valid RFC 1123 label.
func nsName(testName string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(testName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 45 {
		name = strings.Trim(name[:45], "-")
	}
	if name == "" {
		name = "test"
	}
	return fmt.Sprintf("%s-%d", name, namespaceSeq.Add(1))
}

// call invokes a tool and decodes its structured result.
func (s *k8sSession) call(t *testing.T, name string, args map[string]any, out any) {
	t.Helper()
	res, err := s.cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned an error result: %+v", name, res.Content)
	}
	if out == nil {
		return
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal %s result: %v", name, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s result %s: %v", name, raw, err)
	}
}

// callExpectingError invokes a tool that should fail, and returns the
// message a model would see.
func (s *k8sSession) callExpectingError(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	res, err := s.cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("%s succeeded; want an error", name)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func (s *k8sSession) createPod(t *testing.T, name string) {
	t.Helper()
	_, err := s.typed.CoreV1().Pods(s.namespace).Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
			Labels:    map[string]string{"app": "probe"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod %s: %v", name, err)
	}
}

// Every read tool must be advertised read-only, or the registry drops it
// from every tier including read — a tool silently missing is a poor
// symptom to debug from a client.
func TestK8sReadToolsAreAdvertisedReadOnly(t *testing.T) {
	s := newK8sSession(t)

	res, err := s.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	found := map[string]*sdk.Tool{}
	for _, tool := range res.Tools {
		found[tool.Name] = tool
	}

	for _, name := range engineReadTools {
		tool, ok := found[name]
		if !ok {
			t.Errorf("%s is not advertised", name)
			continue
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not annotated read-only", name)
		}
		if tool.Description == "" {
			t.Errorf("%s has no description; a model chooses tools by description", name)
		}
		// Schema inference is what makes typed inputs worth having.
		if tool.InputSchema == nil {
			t.Errorf("%s has no input schema", name)
		}
	}
}

func TestContextListOverProtocol(t *testing.T) {
	s := newK8sSession(t)

	var out mcpsrv.ContextListOut
	s.call(t, "context_list", map[string]any{}, &out)

	if len(out.Items) != 2 {
		t.Fatalf("got %d contexts, want the kubeconfig's 2: %+v", len(out.Items), out.Items)
	}
	if out.Items[0].Name != "alpha" || out.Items[1].Name != "beta" {
		t.Errorf("contexts = %+v, want alpha then beta", out.Items)
	}
	if out.Source != string(kube.SourceExplicit) {
		t.Errorf("source = %q, want %q", out.Source, kube.SourceExplicit)
	}
}

func TestNamespaceAndNodeListOverProtocol(t *testing.T) {
	s := newK8sSession(t)

	var ns mcpsrv.NamespaceListOut
	s.call(t, "namespace_list", map[string]any{}, &ns)
	var names []string
	for _, n := range ns.Items {
		names = append(names, n.Name)
	}
	for _, want := range []string{"default", "kube-system", s.namespace} {
		if !contains(names, want) {
			t.Errorf("%s missing from namespace_list: %v", want, names)
		}
	}

	// envtest runs no kubelet, so there are no nodes.
	var nodes mcpsrv.NodeListOut
	s.call(t, "node_list", map[string]any{}, &nodes)
	if nodes.Count != len(nodes.Items) {
		t.Errorf("count %d disagrees with %d items", nodes.Count, len(nodes.Items))
	}
}

func TestPodListOverProtocol(t *testing.T) {
	s := newK8sSession(t)
	s.createPod(t, "probe-one")
	s.createPod(t, "probe-two")

	var out mcpsrv.PodListOut
	s.call(t, "pod_list", map[string]any{"namespace": s.namespace}, &out)

	if out.Count != 2 || len(out.Items) != 2 {
		t.Fatalf("count=%d items=%d, want 2: %+v", out.Count, len(out.Items), out.Items)
	}
	if out.Truncated {
		t.Error("two pods reported as truncated")
	}
	for _, p := range out.Items {
		if p.Namespace != s.namespace {
			t.Errorf("pod %s in namespace %q, want %q", p.Name, p.Namespace, s.namespace)
		}
		// No kubelet, so nothing runs; Ready is still reported as a ratio
		// over the spec's containers rather than left blank.
		if p.Ready != "0/1" {
			t.Errorf("pod %s ready = %q, want 0/1", p.Name, p.Ready)
		}
		if p.Phase == "" {
			t.Errorf("pod %s has no phase", p.Name)
		}
	}

	t.Run("a label selector narrows it", func(t *testing.T) {
		var sel mcpsrv.PodListOut
		s.call(t, "pod_list", map[string]any{"namespace": s.namespace, "label_selector": "app=nope"}, &sel)
		if sel.Count != 0 {
			t.Errorf("selector matched %d pods, want 0", sel.Count)
		}
	})
}

func TestPodGetOverProtocol(t *testing.T) {
	s := newK8sSession(t)
	s.createPod(t, "probe-detail")

	var out mcpsrv.PodGetOut
	s.call(t, "pod_get", map[string]any{"name": "probe-detail", "namespace": s.namespace}, &out)

	if out.Summary.Name != "probe-detail" {
		t.Errorf("summary name = %q", out.Summary.Name)
	}
	if out.Summary.Namespace != s.namespace {
		t.Errorf("summary namespace = %q, want %q", out.Summary.Namespace, s.namespace)
	}
	if out.Summary.Ready != "0/1" {
		t.Errorf("ready = %q, want 0/1", out.Summary.Ready)
	}
	// Containers and Conditions are deliberately not asserted non-empty: envtest
	// runs no scheduler or kubelet, so a pod is never admitted and the API
	// server populates neither.

	t.Run("a missing pod is an error, not an empty result", func(t *testing.T) {
		msg := s.callExpectingError(t, "pod_get", map[string]any{"name": "absent", "namespace": s.namespace})
		if !strings.Contains(msg, "absent") {
			t.Errorf("error %q does not name the pod", msg)
		}
	})

	t.Run("a missing name is rejected", func(t *testing.T) {
		msg := s.callExpectingError(t, "pod_get", map[string]any{"namespace": s.namespace})
		if msg == "" {
			t.Error("no message explaining the rejection")
		}
	})
}

func TestResourceListAndGetOverProtocol(t *testing.T) {
	s := newK8sSession(t)
	s.createPod(t, "probe-generic")

	// Short name, resolved through discovery — the generic path all the way
	// down rather than a hardcoded pods client.
	var list mcpsrv.ResourceListOut
	s.call(t, "resource_list", map[string]any{"resource": "po", "namespace": s.namespace}, &list)
	if list.Count != 1 {
		t.Fatalf("resource_list po returned %d items, want 1: %+v", list.Count, list.Items)
	}
	if got := list.Items[0].Name; got != "probe-generic" {
		t.Errorf("name = %q", got)
	}
	if list.Items[0].Kind != "Pod" {
		t.Errorf("kind = %q, want Pod", list.Items[0].Kind)
	}
	if !strings.Contains(list.Resource, "pods") {
		t.Errorf("resource = %q, want the resolved pods GVR", list.Resource)
	}

	var got mcpsrv.ResourceGetOut
	s.call(t, "resource_get", map[string]any{
		"resource": "pods", "name": "probe-generic", "namespace": s.namespace,
	}, &got)
	if got.Object["kind"] != "Pod" {
		t.Errorf("object kind = %v", got.Object["kind"])
	}

	t.Run("an unknown resource names itself in the error", func(t *testing.T) {
		msg := s.callExpectingError(t, "resource_list", map[string]any{"resource": "wibbles"})
		if !strings.Contains(msg, "wibbles") {
			t.Errorf("error %q does not name the input", msg)
		}
	})
}

// The bound is the property every list tool copies, so it is asserted end to
// end rather than on the helper alone: over the protocol, against a real
// API server, with more objects than the limit allows.
func TestListBoundIsReportedNotSilent(t *testing.T) {
	s := newK8sSession(t)
	for i := range 5 {
		_, err := s.typed.CoreV1().ConfigMaps(s.namespace).Create(t.Context(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cm-%d", i), Namespace: s.namespace},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create configmap: %v", err)
		}
	}

	var out mcpsrv.ResourceListOut
	s.call(t, "resource_list", map[string]any{
		"resource": "configmaps", "namespace": s.namespace, "limit": 2,
	}, &out)

	if out.Count != 2 {
		t.Fatalf("returned %d items, want the requested 2", out.Count)
	}
	if !out.Truncated {
		t.Fatal("more objects existed than were returned, but truncated is false — " +
			"this is the silent-truncation failure the envelope exists to prevent")
	}
	if out.Continue == "" {
		t.Error("truncated with no continue token: the caller cannot page")
	}
	if out.Note == "" {
		t.Error("truncated with no note: a model needs to be told what to do")
	}
	if out.Limit != 2 {
		t.Errorf("limit = %d, want the requested 2", out.Limit)
	}

	t.Run("the continue token pages", func(t *testing.T) {
		var next mcpsrv.ResourceListOut
		s.call(t, "resource_list", map[string]any{
			"resource": "configmaps", "namespace": s.namespace, "limit": 2,
			"continue_token": out.Continue,
		}, &next)
		if next.Count == 0 {
			t.Fatal("paging with the continue token returned nothing")
		}
		for _, a := range out.Items {
			for _, b := range next.Items {
				if a.Name == b.Name {
					t.Errorf("%s appeared on both pages", a.Name)
				}
			}
		}
	})

	t.Run("a limit over the maximum is clamped, not rejected", func(t *testing.T) {
		var big mcpsrv.ResourceListOut
		s.call(t, "resource_list", map[string]any{
			"resource": "configmaps", "namespace": s.namespace, "limit": 10000,
		}, &big)
		if big.Limit != mcpsrv.MaxListLimit {
			t.Errorf("limit = %d, want it clamped to %d", big.Limit, mcpsrv.MaxListLimit)
		}
	})
}

func TestEventListOverProtocol(t *testing.T) {
	s := newK8sSession(t)

	// envtest has no controllers, so nothing generates events on its own.
	for i, kind := range []string{"Normal", "Warning"} {
		_, err := s.typed.CoreV1().Events(s.namespace).Create(t.Context(), &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: fmt.Sprintf("e-%d", i), Namespace: s.namespace},
			Type:           kind,
			Reason:         "Probe",
			Message:        "synthetic " + kind,
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "probe", Namespace: s.namespace},
			Count:          1,
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create event: %v", err)
		}
	}

	var all mcpsrv.EventListOut
	s.call(t, "event_list", map[string]any{"namespace": s.namespace}, &all)
	if all.Count != 2 {
		t.Fatalf("got %d events, want 2: %+v", all.Count, all.Items)
	}

	var warn mcpsrv.EventListOut
	s.call(t, "event_list", map[string]any{"namespace": s.namespace, "warnings_only": true}, &warn)
	if warn.Count != 1 {
		t.Fatalf("warnings_only returned %d events, want 1: %+v", warn.Count, warn.Items)
	}
	if warn.Items[0].Type != "Warning" {
		t.Errorf("type = %q, want Warning", warn.Items[0].Type)
	}
	if warn.Items[0].Object != "Pod/probe" {
		t.Errorf("object = %q, want Pod/probe", warn.Items[0].Object)
	}
}

// pod_logs cannot be exercised end to end: envtest runs no kubelet, so no
// container ever produces output. The byte cap is not covered here;
// internal/mcp.TestReadLogTail covers it.
func TestPodLogsOnAPodThatNeverRan(t *testing.T) {
	s := newK8sSession(t)
	s.createPod(t, "probe-logs")

	var out mcpsrv.PodLogsOut
	s.call(t, "pod_logs", map[string]any{"name": "probe-logs", "namespace": s.namespace}, &out)

	if out.Logs != "" {
		t.Errorf("logs = %q, want empty without a kubelet", out.Logs)
	}
	if out.Lines != 0 {
		t.Errorf("lines = %d, want 0", out.Lines)
	}
	if out.Note == "" {
		t.Fatal("an empty log came back with no explanation: a model cannot tell " +
			"\"nothing logged\" from \"never started\"")
	}
	for _, want := range []string{"no output", "pod_get"} {
		if !strings.Contains(out.Note, want) {
			t.Errorf("note %q is missing %q", out.Note, want)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestSecretsAreRefusedAtTheReadTier. The on-node fallback is admin.conf,
// which is cluster-admin, so the default server has the RIGHTS to read every
// Secret. A Secret's contents in a model's context cannot be taken back, so
// the refusal is policy here rather than something RBAC is trusted to do.
func TestSecretsAreRefusedAtTheReadTier(t *testing.T) {
	s := newK8sSession(t)

	if _, err := s.typed.CoreV1().Secrets(s.namespace).Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: s.namespace},
		StringData: map[string]string{"password": "hunter2"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create secret: %v", err)
	}

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"resource_get", map[string]any{"resource": "secrets", "name": "creds", "namespace": s.namespace}},
		{"resource_list", map[string]any{"resource": "secrets", "namespace": s.namespace}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			msg := s.callExpectingError(t, tc.tool, tc.args)
			if !strings.Contains(msg, "not readable") {
				t.Errorf("error %q does not refuse the read as policy", msg)
			}
			if strings.Contains(msg, "hunter2") {
				t.Errorf("the refusal leaked the secret value: %q", msg)
			}
		})
	}
}

package operator

// Wait-behaviour guards for the Helm v4 migration. Each test runs one reconcile
// of a HelmChart with the real newActionConfig against a fake apiserver, so
// Helm's own Kubernetes client, and the waiter newActionConfig gives it, do the
// work. Helm v4's own waiter for the operator's helmWaitStrategy does not wait
// as Helm v3 did with Wait = false: it watches every hook through
// namespace-wide list and watch requests, and it does not wait for the CRDs an
// install creates. These tests pin what Helm v3 did.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

// apiResource is one resource kind fakeAPIServer serves.
type apiResource struct {
	groupVersion, name, kind string
	namespaced               bool
}

var (
	coreAPIResources = []apiResource{
		{"v1", "configmaps", "ConfigMap", true},
		{"v1", "secrets", "Secret", true},
		{"v1", "namespaces", "Namespace", false},
	}
	crdAPIResource    = apiResource{"apiextensions.k8s.io/v1", "customresourcedefinitions", "CustomResourceDefinition", false}
	widgetAPIResource = apiResource{"example.com/v1", "widgets", "Widget", true}
)

// widgetsCRDName is the name of the CRD in widgetsCRD, the only object
// fakeAPIServer finds once it has been sent it.
const widgetsCRDName = "widgets.example.com"

// fakeAPIServer is a minimal Kubernetes apiserver. It serves discovery and
// OpenAPI v3 for core/v1 and apiextensions.k8s.io/v1, and for example.com/v1
// once the widgets CRD it was sent is Established. It accepts every create
// and update, lists and finds no object but that CRD, and records every
// request it receives.
type fakeAPIServer struct {
	// forbidListWatch names a resource, such as "configmaps", whose list and
	// watch requests get 403 Forbidden, as RBAC answers a requester who may
	// create that resource but not list or watch it.
	forbidListWatch string
	// crdDelay is how long after its creation the widgets CRD becomes
	// Established.
	crdDelay time.Duration

	mu         sync.Mutex
	requests   []apiRequest
	crdCreated time.Time
}

// apiRequest is what fakeAPIServer records of one request.
type apiRequest struct {
	method, path, query string
	user, uid           string
	groups              []string
}

func (r apiRequest) String() string {
	if r.query == "" {
		return r.method + " " + r.path
	}
	return r.method + " " + r.path + "?" + r.query
}

// start serves s until the test ends and returns a kubeconfig that points at it.
func (s *fakeAPIServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	// Keep the discovery cache, which would go under $HOME/.kube, to this test.
	t.Setenv("KUBECACHEDIR", t.TempDir())
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: "+srv.URL+"\ncontexts:\n- name: test\n  context:\n    cluster: test\ncurrent-context: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return kubeconfig
}

// all returns every request s has received.
func (s *fakeAPIServer) all() []apiRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// count returns how many requests with this method and path s has received.
func (s *fakeAPIServer) count(method, path string) int {
	n := 0
	for _, r := range s.all() {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

// crdState reports whether s has been sent the widgets CRD and whether that
// CRD is Established.
func (s *fakeAPIServer) crdState() (created, established bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	created = !s.crdCreated.IsZero()
	return created, created && time.Since(s.crdCreated) >= s.crdDelay
}

// served returns the resources s serves, by group version.
func (s *fakeAPIServer) served() map[string][]apiResource {
	gvs := map[string][]apiResource{
		"v1":                        coreAPIResources,
		crdAPIResource.groupVersion: {crdAPIResource},
	}
	if _, established := s.crdState(); established {
		gvs[widgetAPIResource.groupVersion] = []apiResource{widgetAPIResource}
	}
	return gvs
}

func (s *fakeAPIServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, apiRequest{
		method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
		user: r.Header.Get("Impersonate-User"), uid: r.Header.Get("Impersonate-Uid"), groups: r.Header.Values("Impersonate-Group"),
	})
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	reply := func(code int, v any) {
		out, err := json.Marshal(v)
		if err != nil {
			code, out = http.StatusInternalServerError, []byte(err.Error())
		}
		w.WriteHeader(code)
		_, _ = w.Write(out)
	}
	status := func(code int, reason metav1.StatusReason, msg string) {
		reply(code, metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusFailure, Code: int32(code), Reason: reason, Message: msg,
		})
	}
	gvs := s.served()

	switch p := r.URL.Path; {
	case p == "/version":
		reply(http.StatusOK, map[string]string{"major": "1", "minor": "35", "gitVersion": "v1.35.0"})
	case p == "/api":
		reply(http.StatusOK, map[string]any{"kind": "APIVersions", "versions": []string{"v1"},
			"serverAddressByClientCIDRs": []any{map[string]string{"clientCIDR": "0.0.0.0/0", "serverAddress": "127.0.0.1"}}})
	case p == "/apis":
		groups := []any{}
		for _, gv := range slices.Sorted(maps.Keys(gvs)) {
			if group, version, ok := strings.Cut(gv, "/"); ok {
				v := map[string]string{"groupVersion": gv, "version": version}
				groups = append(groups, map[string]any{"name": group, "versions": []any{v}, "preferredVersion": v})
			}
		}
		reply(http.StatusOK, map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": groups})
	case p == "/openapi/v3":
		paths := map[string]any{}
		for gv := range gvs {
			paths[apiPrefix(gv)[1:]] = map[string]string{"serverRelativeURL": "/openapi/v3" + apiPrefix(gv) + "?hash=1"}
		}
		reply(http.StatusOK, map[string]any{"paths": paths})
	case strings.HasPrefix(p, "/openapi/v3/"):
		gv := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(p, "/openapi/v3/"), "api/"), "apis/")
		if _, ok := gvs[gv]; !ok {
			status(http.StatusNotFound, metav1.StatusReasonNotFound, "no OpenAPI document for "+gv)
			return
		}
		reply(http.StatusOK, openAPIDocument(gvs[gv]))
	default:
		gv, rest, ok := splitAPIPath(p)
		served, isServed := gvs[gv]
		if !ok || !isServed {
			status(http.StatusNotFound, metav1.StatusReasonNotFound, "not served by fakeAPIServer")
			return
		}
		if len(rest) == 0 {
			reply(http.StatusOK, resourceList(gv, served))
			return
		}
		s.serveObject(r.Method, r.URL.Query().Get("watch") == "true", served, rest, body, reply, status)
	}
}

// serveObject answers a request for a resource or one object of it. rest is
// the path below the group version: [namespaces <ns>] <resource> [<name>].
func (s *fakeAPIServer) serveObject(method string, watch bool, served []apiResource, rest []string, body []byte,
	reply func(int, any), status func(int, metav1.StatusReason, string),
) {
	if len(rest) >= 3 && rest[0] == "namespaces" {
		rest = rest[2:]
	}
	i := slices.IndexFunc(served, func(r apiResource) bool { return r.name == rest[0] })
	if i < 0 {
		status(http.StatusNotFound, metav1.StatusReasonNotFound, "resource "+rest[0]+" not served")
		return
	}
	res := served[i]
	created, established := s.crdState()
	if len(rest) == 1 {
		switch {
		case method == http.MethodGet && res.name == s.forbidListWatch:
			status(http.StatusForbidden, metav1.StatusReasonForbidden, res.name+" is forbidden: the requester may not list or watch "+res.name)
		case method == http.MethodGet && watch:
			status(http.StatusMethodNotAllowed, metav1.StatusReasonMethodNotAllowed, "fakeAPIServer serves no watches")
		case method == http.MethodGet:
			items := []any{}
			if res == crdAPIResource && created {
				items = append(items, widgetsCRDObject(established))
			}
			reply(http.StatusOK, map[string]any{"kind": res.kind + "List", "apiVersion": res.groupVersion,
				"metadata": map[string]string{"resourceVersion": "1"}, "items": items})
		case method == http.MethodPost:
			if res == crdAPIResource {
				s.mu.Lock()
				if s.crdCreated.IsZero() {
					s.crdCreated = time.Now()
				}
				s.mu.Unlock()
			}
			reply(http.StatusCreated, echoObject(body, res))
		default:
			status(http.StatusMethodNotAllowed, metav1.StatusReasonMethodNotAllowed, method+" not served on "+res.name)
		}
		return
	}
	name := rest[1]
	switch {
	case method == http.MethodGet && res == crdAPIResource && name == widgetsCRDName && created:
		reply(http.StatusOK, widgetsCRDObject(established))
	case method == http.MethodPut:
		reply(http.StatusOK, echoObject(body, res))
	default:
		status(http.StatusNotFound, metav1.StatusReasonNotFound, res.name+" "+name+" not found")
	}
}

// splitAPIPath splits an API path into its group version and the path
// segments below it.
func splitAPIPath(p string) (gv string, rest []string, ok bool) {
	seg := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case len(seg) >= 2 && seg[0] == "api":
		return seg[1], seg[2:], true
	case len(seg) >= 3 && seg[0] == "apis":
		return seg[1] + "/" + seg[2], seg[3:], true
	}
	return "", nil, false
}

// apiPrefix is the path under which group version gv is served.
func apiPrefix(gv string) string {
	if strings.Contains(gv, "/") {
		return "/apis/" + gv
	}
	return "/api/" + gv
}

func resourceList(gv string, served []apiResource) map[string]any {
	resources := []any{}
	for _, r := range served {
		resources = append(resources, map[string]any{
			"name": r.name, "singularName": strings.ToLower(r.kind), "namespaced": r.namespaced, "kind": r.kind,
			"verbs": []string{"create", "delete", "get", "list", "patch", "update", "watch"},
		})
	}
	return map[string]any{"kind": "APIResourceList", "groupVersion": gv, "resources": resources}
}

// openAPIDocument is an OpenAPI v3 document in which each served kind's PATCH
// operation takes the fieldValidation parameter, so Helm's client validates
// the objects it builds server-side rather than fetching full schemas.
func openAPIDocument(served []apiResource) map[string]any {
	paths := map[string]any{}
	for _, r := range served {
		group, version, ok := strings.Cut(r.groupVersion, "/")
		if !ok {
			group, version = "", r.groupVersion
		}
		path := apiPrefix(r.groupVersion) + "/" + r.name + "/{name}"
		if r.namespaced {
			path = apiPrefix(r.groupVersion) + "/namespaces/{namespace}/" + r.name + "/{name}"
		}
		paths[path] = map[string]any{"patch": map[string]any{
			"x-kubernetes-group-version-kind": map[string]string{"group": group, "version": version, "kind": r.kind},
			"parameters":                      []any{map[string]any{"name": "fieldValidation", "in": "query", "schema": map[string]string{"type": "string"}}},
			"responses":                       map[string]any{},
		}}
	}
	return map[string]any{"openapi": "3.0.0", "info": map[string]string{"title": "fakeAPIServer", "version": "v1"}, "paths": paths}
}

// echoObject is the object a create or update was sent, as the apiserver
// returns it.
func echoObject(body []byte, res apiResource) map[string]any {
	obj := map[string]any{}
	_ = json.Unmarshal(body, &obj)
	obj["apiVersion"], obj["kind"] = res.groupVersion, res.kind
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["resourceVersion"] = "1"
	obj["metadata"] = meta
	return obj
}

// widgetsCRDObject is the widgets CRD as the apiserver reports it.
func widgetsCRDObject(established bool) map[string]any {
	condition := "False"
	if established {
		condition = "True"
	}
	return map[string]any{
		"apiVersion": crdAPIResource.groupVersion, "kind": crdAPIResource.kind,
		"metadata": map[string]any{"name": widgetsCRDName, "resourceVersion": "1"},
		"spec": map[string]any{
			"group": "example.com", "scope": "Namespaced",
			"names": map[string]string{"plural": "widgets", "singular": "widget", "kind": "Widget", "listKind": "WidgetList"},
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}}}},
		},
		"status": map[string]any{
			"acceptedNames": map[string]string{"plural": "widgets", "kind": "Widget"},
			"conditions": []any{
				map[string]string{"type": "NamesAccepted", "status": "True"},
				map[string]string{"type": "Established", "status": condition},
			},
		},
	}
}

// waitChart builds spec.chartContent for a chart named app from files, whose
// paths are relative to the chart directory.
func waitChart(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files["Chart.yaml"] = "apiVersion: v2\nname: app\nversion: 1.0.0\n"
	for _, path := range slices.Sorted(maps.Keys(files)) {
		data := files[path]
		if err := tw.WriteHeader(&tar.Header{Name: "app/" + path, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// installAgainst runs one reconcile of an adopted HelmChart named app in
// namespace ns whose chart holds files, with the real newActionConfig and a
// kubeconfig that points at api, and returns the HelmChart's status phase and
// message. kube-system is the only privileged namespace, so a HelmChart there
// installs as the operator; elsewhere requester must be set, and the HelmChart
// carries their authentic stamp, so the install runs impersonated as them.
func installAgainst(t *testing.T, api *fakeAPIServer, ns string, files map[string]string, requester *requesterInfo) (phase, message string) {
	t.Helper()
	kubeconfig := api.start(t)
	signer := testSigner()
	hc := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "stratum.ciq.com/v1",
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name": "app", "namespace": ns, "uid": "uid-app",
			"finalizers": []interface{}{helmFinalizer},
		},
		"spec": map[string]interface{}{"chartContent": waitChart(t, files), "timeout": "30s"},
	}}
	if requester != nil {
		groups, err := json.Marshal(requester.Groups)
		if err != nil {
			t.Fatal(err)
		}
		hc.SetAnnotations(map[string]string{
			annRequester:       requester.Username,
			annRequesterUID:    requester.UID,
			annRequesterGroups: string(groups),
			annRequesterSig:    signer.sign(requesterBinding(hc), *requester),
		})
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{helmChartGVR: "HelmChartList", secretGVR: "SecretList"},
		hc.DeepCopy(),
	)

	reconcileHelmChart(context.Background(), dyn, kubeconfig, hc, "", nil,
		helmPolicy{privilegedNamespaces: []string{"kube-system"}, signer: signer})

	got, err := dyn.Resource(helmChartGVR).Namespace(ns).Get(context.Background(), "app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get helmchart: %v", err)
	}
	phase, _, _ = unstructured.NestedString(got.Object, "status", "phase")
	message, _, _ = unstructured.NestedString(got.Object, "status", "message")
	return phase, message
}

const (
	appConfigMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: app
data:
  hello: world
`
	hookConfigMap = `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-hook
  annotations:
    helm.sh/hook: pre-install
data:
  ran: "true"
`
	widgetsCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
  names: {plural: widgets, singular: widget, kind: Widget, listKind: WidgetList}
  scope: Namespaced
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema: {type: object, x-kubernetes-preserve-unknown-fields: true}
`
	widget = `apiVersion: example.com/v1
kind: Widget
metadata:
  name: app
`
)

// A tenant's install of a chart with a ConfigMap pre-install hook succeeds
// when the requester may create ConfigMaps but not list or watch them. Helm v3
// watched only Job and Pod hooks, through a watch on the hook's name, so it
// made no list or watch here; Helm v4's own waiter lists and watches every
// hook's kind across the namespace, and the install failed with "failed
// pre-install: ... forbidden". Helm v4, not the operator, builds the clients
// for discovery, the objects and release storage, so every request it makes
// must still carry the requester's identity.
func TestTenantInstallWithHookNeedsNoListOrWatch(t *testing.T) {
	api := &fakeAPIServer{forbidListWatch: "configmaps"}
	requester := requesterInfo{Username: "alice", UID: "alice-uid", Groups: []string{"team-a", "system:authenticated"}}

	phase, message := installAgainst(t, api, "team-a", map[string]string{
		"templates/hook.yaml": hookConfigMap,
		"templates/app.yaml":  appConfigMap,
	}, &requester)

	if phase != hcPhaseDeployed {
		t.Fatalf("helmchart phase = %q (message %q), want %q", phase, message, hcPhaseDeployed)
	}
	if n := api.count(http.MethodPost, "/api/v1/namespaces/team-a/configmaps"); n != 2 {
		t.Errorf("the install created %d ConfigMaps, want 2: the hook and the chart's ConfigMap", n)
	}
	if n := api.count(http.MethodGet, "/api/v1/namespaces/team-a/configmaps"); n != 0 {
		t.Errorf("the install listed or watched ConfigMaps %d times, want none", n)
	}
	reqs := api.all()
	if !slices.ContainsFunc(reqs, func(r apiRequest) bool { return r.path == "/api" || r.path == "/apis" }) {
		t.Errorf("no discovery request reached the apiserver, so none was checked for the requester's identity; requests: %v", reqs)
	}
	for _, r := range reqs {
		if r.user != requester.Username || r.uid != requester.UID || !slices.Equal(r.groups, requester.Groups) {
			t.Errorf("%s ran as user %q uid %q groups %q, want the requester %q uid %q groups %q",
				r, r.user, r.uid, r.groups, requester.Username, requester.UID, requester.Groups)
		}
	}
}

// TestHookDeletedBeforeCreationIsWaitedFor: before running a hook, Helm deletes
// any earlier copy of it by its delete policy (before-hook-creation unless the
// chart says otherwise) and, as Helm v3 did, waits until that copy is gone by
// polling it by name. Without the wait, a hook whose earlier copy is still
// terminating fails to create with AlreadyExists.
func TestHookDeletedBeforeCreationIsWaitedFor(t *testing.T) {
	api := &fakeAPIServer{}

	phase, message := installAgainst(t, api, "kube-system", map[string]string{
		"templates/hook.yaml": hookConfigMap,
	}, nil)

	if phase != hcPhaseDeployed {
		t.Fatalf("helmchart phase = %q (message %q), want %q", phase, message, hcPhaseDeployed)
	}
	const hook = "/api/v1/namespaces/kube-system/configmaps/app-hook"
	reqs := api.all()
	next := func(from int, method, path string) int {
		if from < 0 {
			return -1
		}
		i := slices.IndexFunc(reqs[from:], func(r apiRequest) bool { return r.method == method && r.path == path })
		if i < 0 {
			return -1
		}
		return from + i
	}
	del := next(0, http.MethodDelete, hook)
	get := next(del+1, http.MethodGet, hook)
	post := next(get+1, http.MethodPost, "/api/v1/namespaces/kube-system/configmaps")
	if del < 0 || get < 0 || post < 0 {
		t.Fatalf("want the hook's DELETE, then a GET of it by name, then its POST; got DELETE at %d, GET at %d, POST at %d in %v", del, get, post, reqs)
	}
}

// An install of a chart whose crds/ holds a CRD waits until the apiserver
// reports the CRD Established, and serves its API, before it renders and
// applies the chart's templates, as Helm v3 did. Without that wait, a custom
// resource of the new kind fails to map ("no matches for kind"), and one
// gated on .Capabilities.APIVersions.Has is left out of a release recorded
// Deployed.
func TestInstallWaitsForChartCRDs(t *testing.T) {
	for _, tc := range []struct{ name, template string }{
		{"custom resource", widget},
		{"capability-gated custom resource", "{{ if .Capabilities.APIVersions.Has \"example.com/v1\" }}\n" + widget + "{{ end }}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPIServer{crdDelay: 300 * time.Millisecond}

			phase, message := installAgainst(t, api, "kube-system", map[string]string{
				"crds/widgets.yaml":     widgetsCRD,
				"templates/widget.yaml": tc.template,
			}, nil)

			if phase != hcPhaseDeployed {
				t.Fatalf("helmchart phase = %q (message %q), want %q", phase, message, hcPhaseDeployed)
			}
			if path := fmt.Sprintf("%s/namespaces/kube-system/%s", apiPrefix(widgetAPIResource.groupVersion), widgetAPIResource.name); api.count(http.MethodPost, path) != 1 {
				t.Errorf("the install did not create the Widget once; requests: %v", api.all())
			}
		})
	}
}

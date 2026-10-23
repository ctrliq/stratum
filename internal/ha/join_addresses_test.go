package ha

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/version"
)

func joinAddressServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/nodes" || r.Header.Get("Authorization") != "Bearer join-admin" {
			t.Errorf("unexpected node-address request: %s %s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	oldKube, oldAdmin := version.KubeconfigDir, version.AdminConf
	version.KubeconfigDir = t.TempDir()
	version.AdminConf = filepath.Join(version.KubeconfigDir, "admin.conf")
	t.Cleanup(func() { version.KubeconfigDir, version.AdminConf = oldKube, oldAdmin })
	// The normal kubeconfigs already point at the proposed VIP. It is not
	// serving here, so only a read through the original join server works.
	conf := clientcmdapi.Config{
		CurrentContext: "join",
		Clusters: map[string]*clientcmdapi.Cluster{"cluster": {
			Server: "https://127.0.0.1:1", CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}),
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"admin": {Token: "join-admin"}},
		Contexts:  map[string]*clientcmdapi.Context{"join": {Cluster: "cluster", AuthInfo: "admin"}},
	}
	if err := clientcmd.WriteToFile(conf, version.SuperAdminConf()); err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestJoinControllerAddressesIncludeBothFamilies(t *testing.T) {
	nodes := corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}, Items: []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "cp1", Labels: map[string]string{k8sutil.ControlPlaneLabel: "true"}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.11"}, {Type: corev1.NodeInternalIP, Address: "fd00::11"},
			{Type: corev1.NodeExternalIP, Address: "198.51.100.11"}, {Type: corev1.NodeHostName, Address: "cp1"},
		}}},
		// The label may not have landed yet; the etcd primary identifies cp2.
		{ObjectMeta: metav1.ObjectMeta{Name: "cp2"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.12"}, {Type: corev1.NodeInternalIP, Address: "fd00::12"},
		}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.20"}, {Type: corev1.NodeInternalIP, Address: "fd00::20"},
		}}},
	}}
	srv := joinAddressServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(nodes); err != nil {
			t.Error(err)
		}
	})
	fx := productionJoinEffects()
	ips, err := fx.controllerNodeIPs(t.Context(), srv.URL, []string{"10.0.0.11", "10.0.0.12"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"10.0.0.11", "fd00::11", "198.51.100.11", "10.0.0.12", "fd00::12"} {
		if !slices.Contains(ips, want) {
			t.Errorf("controller addresses %v omit %s", ips, want)
		}
	}
	for _, other := range []string{"10.0.0.20", "fd00::20", "cp1"} {
		if slices.Contains(ips, other) {
			t.Errorf("controller addresses %v include worker address or hostname %s", ips, other)
		}
	}
}

func TestJoinControllerAddressesRefuseIncompleteInventory(t *testing.T) {
	// The budget is the wait, not the answer: a list that stays incomplete to
	// the end still refuses, with the message that names the re-run.
	old := controllerAddressPollBudget
	controllerAddressPollBudget = 2 * time.Second
	t.Cleanup(func() { controllerAddressPollBudget = old })
	srv := joinAddressServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}})
	})
	ips, err := controllerNodeIPsForJoin(t.Context(), srv.URL, []string{"10.0.0.11"})
	if err == nil || len(ips) != 0 {
		t.Fatalf("missing controller node: ips=%v, err=%v; want refusal rather than treating its secondary addresses as free", ips, err)
	}
	if !strings.Contains(err.Error(), "no registered node advertises controller address 10.0.0.11") {
		t.Errorf("refusal does not name the unadvertised controller address: %v", err)
	}
}

// TestJoinControllerAddressesWaitsForALaggingRegistration is the regression
// for the CI inttest failures: a joining third controller starts the moment
// the second's etcd membership lands, and its kubelet registers the node
// object seconds later, so the first successful list is regularly incomplete.
// The read must keep polling until the inventory is complete — one successful
// list before the registration must not refuse a join the cluster is ready
// for.
func TestJoinControllerAddressesWaitsForALaggingRegistration(t *testing.T) {
	old := controllerAddressPollBudget
	controllerAddressPollBudget = 30 * time.Second
	t.Cleanup(func() { controllerAddressPollBudget = old })
	var calls atomic.Int32
	nodes := corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}, Items: []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "cp1", Labels: map[string]string{k8sutil.ControlPlaneLabel: "true"}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.11"},
		}}},
	}}
	srv := joinAddressServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			// The inventory the join sees first: cp2's node object has not
			// been registered yet.
			_ = json.NewEncoder(w).Encode(nodes)
			return
		}
		registered := nodes.DeepCopy()
		registered.Items = append(registered.Items, corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp2"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "10.0.0.12"},
		}}})
		_ = json.NewEncoder(w).Encode(registered)
	})
	ips, err := controllerNodeIPsForJoin(t.Context(), srv.URL, []string{"10.0.0.11", "10.0.0.12"})
	if err != nil {
		t.Fatalf("a lagging registration refused the join: %v", err)
	}
	if !slices.Contains(ips, "10.0.0.12") {
		t.Errorf("controller addresses %v omit cp2's address once registered", ips)
	}
	if calls.Load() != 2 {
		t.Errorf("node reads=%d; want one incomplete inventory and then the complete one", calls.Load())
	}
}

func TestJoinControllerAddressesPreserveAPIErrors(t *testing.T) {
	srv := joinAddressServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Code: 403, Reason: metav1.StatusReasonForbidden})
	})
	if ips, err := controllerNodeIPsForJoin(t.Context(), srv.URL, nil); !apierrors.IsForbidden(err) || len(ips) != 0 {
		t.Fatalf("forbidden node read: ips=%v, err=%v; want the API error", ips, err)
	}
}

// TestJoinControllerAddressesPreservesAFatalAPIErrorAfterAList pins the
// classification: a successful-but-incomplete list followed by a fatal API
// error reports the API error, not the registration refusal — the poll ended
// on the error, so "wait for the node registration" would be advice the
// error refutes.
func TestJoinControllerAddressesPreservesAFatalAPIErrorAfterAList(t *testing.T) {
	var calls atomic.Int32
	srv := joinAddressServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Code: 403, Reason: metav1.StatusReasonForbidden})
	})
	ips, err := controllerNodeIPsForJoin(t.Context(), srv.URL, []string{"10.0.0.11"})
	if !apierrors.IsForbidden(err) || len(ips) != 0 {
		t.Fatalf("incomplete list then forbidden: ips=%v, err=%v; want the API error, not the registration refusal", ips, err)
	}
}

func TestJoinControllerAddressesStopWithTheJoinContext(t *testing.T) {
	started := make(chan struct{})
	srv := joinAddressServer(t, func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := controllerNodeIPsForJoin(ctx, srv.URL, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("node-address request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled join address read = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("node-address request outlived the join context")
	}
}

func TestJoinControllerAddressesWaitForVIPFailover(t *testing.T) {
	var calls atomic.Int32
	srv := joinAddressServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Code: 503, Reason: metav1.StatusReasonServiceUnavailable})
			return
		}
		_ = json.NewEncoder(w).Encode(corev1.NodeList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}})
	})
	if _, err := controllerNodeIPsForJoin(t.Context(), srv.URL, nil); err != nil {
		t.Fatalf("temporary API outage failed the join: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("node reads=%d; want two unavailable responses and success", calls.Load())
	}
}

// A context deadline can pass before the runtime schedules its cancellation
// timer. The real client limiter then refuses a reservation while Err is nil.
type deferredAddressDeadline struct {
	context.Context
	deadline time.Time
}

func (c deferredAddressDeadline) Deadline() (time.Time, bool) { return c.deadline, true }

func TestJoinControllerAddressDiagnosisAtAnUndeliveredDeadline(t *testing.T) {
	live := t.Context()
	expired := deferredAddressDeadline{Context: live, deadline: time.Now().Add(-time.Second)}
	limiterErr := flowcontrol.NewTokenBucketRateLimiter(5, 10).Wait(expired)
	if limiterErr == nil || expired.Err() != nil {
		t.Fatalf("expected limiter refusal before context timer delivery: limiter=%v, context=%v", limiterErr, expired.Err())
	}
	missing := "10.0.0.11"
	t.Run("expired inventory budget retains the missing controller diagnosis", func(t *testing.T) {
		err := controllerAddressPollError(live, expired, missing, limiterErr)
		if !strings.Contains(err.Error(), "no registered node advertises controller address "+missing) {
			t.Fatalf("lost registration diagnosis at deadline: %v", err)
		}
	})
	t.Run("expired caller retains its deadline error", func(t *testing.T) {
		err := controllerAddressPollError(expired, expired, missing, limiterErr)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller deadline = %v; want DeadlineExceeded", err)
		}
	})
	t.Run("API errors while budget is live remain API errors", func(t *testing.T) {
		forbidden := apierrors.NewForbidden(corev1.Resource("nodes"), "", errors.New("denied"))
		err := controllerAddressPollError(live, live, missing, forbidden)
		if !apierrors.IsForbidden(err) {
			t.Fatalf("API refusal = %v; want Forbidden", err)
		}
	})
	t.Run("an expired budget without an inventory preserves the read error", func(t *testing.T) {
		if err := controllerAddressPollError(live, expired, "", limiterErr); !errors.Is(err, limiterErr) {
			t.Fatalf("read error = %v; want original failure", err)
		}
	})
}

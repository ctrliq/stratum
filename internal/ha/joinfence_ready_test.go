package ha

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// API servers see admission policy updates independently. A successful write
// to storage must not allow the Role before even the slowest server enforces it.
func TestCertsReaderWaitsForAdmissionOnEveryAPIServer(t *testing.T) {
	var mu sync.Mutex
	probes := [2]map[string]int{{}, {}}
	var targets []corev1.EndpointSubset
	roleWritten := false
	handler := func(index int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/namespaces/default/endpoints/kubernetes":
				_ = json.NewEncoder(w).Encode(corev1.Endpoints{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Endpoints"}, Subsets: targets})
			case "/apis/coordination.k8s.io/v1/namespaces/kube-system/leases":
				groups := r.Header.Values("Impersonate-Group")
				group := ""
				for _, candidate := range groups {
					if candidate != "system:masters" {
						group = candidate
					}
				}
				probes[index][group]++
				if r.URL.Query().Get("dryRun") != "All" || r.Header.Get("Impersonate-User") == "" || len(r.Header.Values("Impersonate-Group")) != 2 {
					t.Errorf("probe lacks dry-run or impersonation: %s headers=%v", r.URL, r.Header)
				}
				var lease coordinationv1.Lease
				if err := json.NewDecoder(r.Body).Decode(&lease); err != nil {
					t.Error(err)
				}
				if len(lease.Finalizers) == 0 {
					t.Error("probe does not exercise the latest policy's metadata restriction")
				}
				if probes[index][group] < index+2 {
					_ = json.NewEncoder(w).Encode(lease)
					return
				}
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Code: 422, Reason: metav1.StatusReasonInvalid, Message: fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: a control-plane join token may not set finalizers on a maintenance lease", k8sutil.JoinFencePolicy, k8sutil.JoinFencePolicy)})
			case "/apis/rbac.authorization.k8s.io/v1/namespaces/kube-system/roles":
				roleWritten = true
				for _, group := range []string{k8sutil.ControlPlaneBootstrapGroup, k8sutil.BootstrapGroup} {
					if probes[0][group] < 2 || probes[1][group] < 3 {
						t.Errorf("lease rights enabled before enforcement for %s on both servers: probes=%v", group, probes)
					}
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				_ = json.NewEncoder(w).Encode(body)
			default:
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				_ = json.NewEncoder(w).Encode(body)
			}
		}
	}
	var first *httptest.Server
	for i := 0; i < 2; i++ {
		srv := httptest.NewTLSServer(handler(i))
		t.Cleanup(srv.Close)
		if i == 0 {
			first = srv
		}
		u, _ := url.Parse(srv.URL)
		host, port, _ := net.SplitHostPort(u.Host)
		p, _ := strconv.Atoi(port)
		targets = append(targets, corev1.EndpointSubset{Addresses: []corev1.EndpointAddress{{IP: host}}, Ports: []corev1.EndpointPort{{Name: "https", Port: int32(p), Protocol: corev1.ProtocolTCP}}})
	}
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: first.URL, TLSClientConfig: rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: first.Certificate().Raw})}})
	if err != nil {
		t.Fatal(err)
	}
	if err := GrantCertsReader(t.Context(), cs); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !roleWritten {
		t.Fatal("no Role was granted after enforcement")
	}
}

func TestCertsReaderRefusesUnverifiedAdmissionBeforeGrant(t *testing.T) {
	cs := uploadFixture(t)
	refusal := fmt.Errorf("one API server still admits unrestricted leases")
	awaitJoinFenceEnforcement = func(context.Context, kubernetes.Interface) error { return refusal }
	if err := GrantCertsReader(t.Context(), cs); !errors.Is(err, refusal) {
		t.Fatalf("grant=%v; want admission refusal", err)
	}
	for _, action := range cs.Actions() {
		if action.GetResource().Group == "rbac.authorization.k8s.io" {
			t.Fatalf("RBAC changed before admission enforcement: %v", action)
		}
	}
}

func TestCertsReaderDoesNotMistakeOtherProbeResultsForEnforcement(t *testing.T) {
	for _, message := range []string{"", "RBAC forbids this user", "ValidatingAdmissionPolicy 'other-policy' denied request: a control-plane join token may not set finalizers on a maintenance lease", "ValidatingAdmissionPolicy '" + k8sutil.JoinFencePolicy + "' denied request: may only create maintenance leases"} {
		t.Run(message, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var srv *httptest.Server
			var calls int
			srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/namespaces/default/endpoints/kubernetes":
					u, _ := url.Parse(srv.URL)
					host, port, _ := net.SplitHostPort(u.Host)
					p, _ := strconv.Atoi(port)
					_ = json.NewEncoder(w).Encode(corev1.Endpoints{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Endpoints"}, Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: host}}, Ports: []corev1.EndpointPort{{Name: "https", Port: int32(p), Protocol: corev1.ProtocolTCP}}}}})
				case "/apis/coordination.k8s.io/v1/namespaces/kube-system/leases":
					calls++
					if calls == 2 {
						cancel()
					}
					if message != "" {
						w.WriteHeader(http.StatusForbidden)
						_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Code: 403, Reason: metav1.StatusReasonForbidden, Message: message})
						return
					}
					var body coordinationv1.Lease
					_ = json.NewDecoder(r.Body).Decode(&body)
					_ = json.NewEncoder(w).Encode(body)
				default:
					if strings.HasPrefix(r.URL.Path, "/apis/rbac.authorization.k8s.io/") {
						t.Error("lease permissions granted without proof of the current admission policy")
					}
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					_ = json.NewEncoder(w).Encode(body)
				}
			}))
			t.Cleanup(srv.Close)
			cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL, TLSClientConfig: rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})}})
			if err != nil {
				t.Fatal(err)
			}
			if err := GrantCertsReader(ctx, cs); !errors.Is(err, context.Canceled) {
				t.Fatalf("grant=%v; want refusal after canceled probes", err)
			}
		})
	}
}

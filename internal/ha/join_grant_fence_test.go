package ha

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// Trust can change while a join prepares its local files, before it takes the
// maintenance fence. Admission must recheck the exact grant under that fence.
func TestJoinRevalidatesDownloadedGrantUnderFence(t *testing.T) {
	for _, tc := range []struct {
		name, entry string
		change      string
		wantErr     bool
	}{
		{"retired grant", "certs-abc123", "remove", true},
		{"replaced grant", "certs-abc123", "replace", true},
		{"another pending grant is appended", "certs-abc123", "append", false},
		{"legacy publication is replaced", "certs", "replace", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joinFixture(t)
			uploadFixture(t)
			files, err := collectBundle(false)
			if err != nil {
				t.Fatal(err)
			}
			key := mustKey(t)
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system",
					Annotations: map[string]string{etcdEndpointsAnnotation: "10.0.0.11"}},
				Data: map[string][]byte{tc.entry: encryptFlat(t, key, files)},
			}
			var published atomic.Pointer[corev1.Secret]
			published.Store(sec)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/namespaces/kube-system/secrets/stratum-certs" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(published.Load())
			}))
			t.Cleanup(srv.Close)
			kc := writeBootstrapKubeconfig(t, srv.URL, "abc123.0123456789abcdef")
			r := &recordedJoin{listMembers: listedMembers()}
			fx := fakeJoinEffects(r)
			fx.downloadGrant = func(string, string, func(bundleProfile) error) (bundleGrant, error) {
				return DownloadCerts(kc, key, nil)
			}
			released := false
			fx.takeMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
				changed := sec.DeepCopy()
				switch tc.change {
				case "remove":
					delete(changed.Data, tc.entry)
				case "replace":
					changed.Data[tc.entry] = encryptFlat(t, key, files)
				case "append":
					changed.Data["certs-def456"] = []byte("unrelated grant")
					changed.Data["certs"] = []byte("new legacy publication")
					changed.Annotations[bundleGenerationAnnotation] = "99"
				}
				published.Store(changed)
				return &k8sutil.MaintenanceLease{Release: func() { released = true }}, nil
			}
			_, err = prepareControlPlaneJoin(t.Context(), freshJoinOpts(), t.TempDir(), fx)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "grant changed") {
					t.Errorf("join with retired preparation = %v, want grant changed refusal", err)
				}
				if r.addCalled || r.removeCalled || r.saved != nil {
					t.Errorf("stale preparation touched membership: add=%v remove=%v saved=%v", r.addCalled, r.removeCalled, r.saved != nil)
				}
			} else if err != nil || !r.addCalled || r.saved == nil {
				t.Errorf("unchanged grant after unrelated mint: err=%v add=%v saved=%v", err, r.addCalled, r.saved != nil)
			}
			if !released {
				t.Error("join left its maintenance fence held")
			}
		})
	}
}

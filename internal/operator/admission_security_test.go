package operator

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// Exercise wire-format AdmissionReviews and the JSONPatch the apiserver applies.
// CREATE has no object UID; persistence adds it after the mutating webhook.
func admitObject(t *testing.T, obj, old *unstructured.Unstructured, user string, signer *requesterSigner) *unstructured.Unstructured {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	req := &admissionv1.AdmissionRequest{UID: "request", Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}, UserInfo: authenticationv1.UserInfo{Username: user, UID: user + "-uid", Groups: []string{"system:authenticated"}}}
	if old != nil {
		req.Operation = admissionv1.Update
		req.OldObject.Raw, _ = json.Marshal(old)
	}
	body, _ := json.Marshal(admissionv1.AdmissionReview{Request: req})
	rr := httptest.NewRecorder()
	admissionHandler(signer)(rr, httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body)))
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(rr.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if review.Response == nil || !review.Response.Allowed {
		t.Fatalf("admission refused: %s", rr.Body.String())
	}
	patch, err := jsonpatch.DecodePatch(review.Response.Patch)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := patch.Apply(raw)
	if err != nil {
		t.Fatal(err)
	}
	out := &unstructured.Unstructured{}
	if err := json.Unmarshal(patched, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAdmissionBindsDesiredStateAcrossCreateAndUpdate(t *testing.T) {
	s := testSigner()
	obj := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion, "kind": "HelmChart", "metadata": map[string]interface{}{"name": "app", "namespace": "tenant"}, "spec": map[string]interface{}{"chart": "app", "valuesContent": "replicas: 1"}}}
	created := admitObject(t, obj, nil, "alice", s)
	created.SetUID("server-assigned")
	if info, ok, reason := verifiedRequester(created, s); !ok || info.Username != "alice" {
		t.Fatalf("CREATE stamp rejected after persistence: %v %s", info, reason)
	}
	changed := created.DeepCopy()
	changed.Object["spec"].(map[string]interface{})["valuesContent"] = "replicas: 2"
	if _, ok, _ := verifiedRequester(changed, s); ok {
		t.Fatal("old stamp authorized new desired state")
	}
	updated := admitObject(t, changed, created, "bob", s)
	if info, ok, _ := verifiedRequester(updated, s); !ok || info.Username != "bob" {
		t.Fatal("spec update retained old authority")
	}
	metadata := updated.DeepCopy()
	metadata.SetFinalizers([]string{helmFinalizer})
	preserved := admitObject(t, metadata, updated, "operator-admin", s)
	if info, ok, _ := verifiedRequester(preserved, s); !ok || info.Username != "bob" {
		t.Fatal("finalizer update replaced requester")
	}
	// Recreating a resource must replace a replayed signature, even with same spec.
	replay := admitObject(t, created, nil, "mallory", s)
	if info, ok, _ := verifiedRequester(replay, s); !ok || info.Username != "mallory" {
		t.Fatal("CREATE accepted replayed owner")
	}
}

func TestOverlayRequiresCanonicalFullIdentity(t *testing.T) {
	s := testSigner()
	owner := requesterInfo{Username: "alice", UID: "one", Groups: []string{"a", "b"}}
	hc := signedHC("tenant", "app", "hc", owner, s)
	for _, tc := range []struct {
		name   string
		info   requesterInfo
		refuse bool
	}{
		{"same groups reordered", requesterInfo{Username: "alice", UID: "one", Groups: []string{"b", "a", "a"}}, false},
		{"changed groups", requesterInfo{Username: "alice", UID: "one", Groups: []string{"a"}}, true},
		{"recreated user", requesterInfo{Username: "alice", UID: "two", Groups: []string{"a", "b"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hcc := signedHC("tenant", "app", "hcc", tc.info, s)
			if refuse, _ := hccInstallAllowed(hc, "replicas: 2", hcc, owner, nil, s); refuse != tc.refuse {
				t.Fatalf("refuse=%v", refuse)
			}
		})
	}
}

// TestAdmissionRestampsKeepBothStamps: during the signature migration, admission
// writes BOTH signatures and the key identifier on CREATE, a spec-change re-stamp
// signs with the CURRENT key after a rotation while keeping both signatures, and a
// metadata-only update preserves the v2 stamp annotations instead of dropping them.
func TestAdmissionRestampsKeepBothStamps(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s0 := keyringFromSecret(t, cs, "k0", map[string][]byte{"k0": testRotationKey(0)})
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion, "kind": "HelmChart",
		"metadata": map[string]any{"name": "app", "namespace": "tenant"},
		"spec":     map[string]any{"chart": "app", "valuesContent": "replicas: 1"},
	}}

	created := admitObject(t, obj, nil, "alice", s0)
	ann := created.GetAnnotations()
	if ann[annRequesterSig] == "" || ann[annRequesterSigV2] == "" || ann[annRequesterKeyID] != "k0" {
		t.Fatalf("CREATE during the migration must stamp legacy + v2 signatures and the key id; annotations = %v", ann)
	}
	created.SetUID("server-assigned")
	if info, ok, reason := verifiedRequester(created, s0); !ok || info.Username != "alice" {
		t.Fatalf("migration CREATE stamp rejected after persistence: %v %s", info, reason)
	}

	// Rotate: k1 becomes the current key, k0 stays retained.
	s1 := keyringFromSecret(t, cs, "k1", map[string][]byte{"k0": testRotationKey(0), "k1": testRotationKey(1)})

	changed := created.DeepCopy()
	changed.Object["spec"].(map[string]any)["valuesContent"] = "replicas: 2"
	updated := admitObject(t, changed, created, "bob", s1)
	uann := updated.GetAnnotations()
	if uann[annRequesterSig] == "" || uann[annRequesterSigV2] == "" {
		t.Fatalf("re-stamp must keep both signatures; annotations = %v", uann)
	}
	if got := uann[annRequesterKeyID]; got != "k1" {
		t.Fatalf("re-stamp must use the CURRENT key id k1, stamped %q", got)
	}
	if info, ok, reason := verifiedRequester(updated, s1); !ok || info.Username != "bob" {
		t.Fatalf("re-stamped object must verify under the rotated signer: %v %s", info, reason)
	}
	// The pre-rotation stamp still verifies: its key is retained.
	if _, ok, reason := verifiedRequester(created.DeepCopy(), s1); !ok {
		t.Fatalf("the pre-rotation stamp must still verify after rotation (retained key): %s", reason)
	}

	metadata := updated.DeepCopy()
	metadata.SetFinalizers([]string{helmFinalizer})
	preserved := admitObject(t, metadata, updated, "operator-admin", s1)
	pann := preserved.GetAnnotations()
	if pann[annRequesterSigV2] == "" || pann[annRequesterKeyID] != "k1" {
		t.Fatalf("metadata-only update must preserve the v2 stamp annotations; annotations = %v", pann)
	}
	if info, ok, _ := verifiedRequester(preserved, s1); !ok || info.Username != "bob" {
		t.Fatalf("finalizer update replaced requester: %v", info)
	}
}

// TestAdmissionStampSurvivesSignerReload: a signer reloaded from an unchanged
// Secret — what a process restart does — must verify the v2 stamps admission wrote
// before the restart, and stamp subsequent writes with the SAME key identifier.
// A key id that silently changes identity would turn every pre-restart v2 stamp
// into a wrong-key refusal.
func TestAdmissionStampSurvivesSignerReload(t *testing.T) {
	cs := fake.NewSimpleClientset()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: requesterKeySecret, Namespace: webhookNamespace},
		Type:       corev1.SecretTypeOpaque,
		// A Secret from before the keyed layout: only the legacy key field.
		Data: map[string][]byte{requesterKeyField: testRotationKey(0), requesterBindingVersionField: []byte(stampBindingV2)},
	}
	if _, err := cs.CoreV1().Secrets(webhookNamespace).Create(context.Background(), sec, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	sA, err := ensureRequesterSigner(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion, "kind": "HelmChart",
		"metadata": map[string]any{"name": "app", "namespace": "tenant"},
		"spec":     map[string]any{"chart": "app", "valuesContent": "replicas: 1"},
	}}
	created := admitObject(t, obj, nil, "alice", sA)
	keyID := created.GetAnnotations()[annRequesterKeyID]
	if keyID == "" {
		t.Fatalf("admission must stamp a key identifier; annotations = %v", created.GetAnnotations())
	}

	sB, err := ensureRequesterSigner(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, reason := verifiedRequester(created, sB); !ok {
		t.Fatalf("a stamp written before the reload must verify after it: %s", reason)
	}
	changed := created.DeepCopy()
	changed.Object["spec"].(map[string]any)["valuesContent"] = "replicas: 2"
	restamped := admitObject(t, changed, created, "bob", sB)
	if got := restamped.GetAnnotations()[annRequesterKeyID]; got != keyID {
		t.Fatalf("reloaded signer must stamp the same key id %q, stamped %q", keyID, got)
	}
}

func TestWebhookRequiresAuthenticatedApiserver(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "front-proxy-ca"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, root, root, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	issue := func(cn string, usage x509.ExtKeyUsage) tls.Certificate {
		t.Helper()
		leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{"webhook.test"}, ExtKeyUsage: []x509.ExtKeyUsage{usage}, NotBefore: root.NotBefore, NotAfter: root.NotAfter}
		b, err := x509.CreateCertificate(rand.Reader, leaf, root, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{b}, PrivateKey: key}
	}
	serving := issue("webhook.test", x509.ExtKeyUsageServerAuth)
	config := webhookClientTLS(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &serving, nil }, func() ([]byte, error) { return caPEM, nil })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveWebhook(ctx, listener, config, admissionHandler(testSigner())) }()
	defer func() { cancel(); <-done }()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	forgedPub, forgedKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forgedTemplate := &x509.Certificate{SerialNumber: big.NewInt(99), Subject: pkix.Name{CommonName: "front-proxy-client"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, NotBefore: root.NotBefore, NotAfter: root.NotAfter}
	forgedDER, err := x509.CreateCertificate(rand.Reader, forgedTemplate, forgedTemplate, forgedPub, forgedKey)
	if err != nil {
		t.Fatal(err)
	}
	forged := tls.Certificate{Certificate: [][]byte{forgedDER}, PrivateKey: forgedKey}

	for _, tc := range []struct {
		name    string
		cert    *tls.Certificate
		allowed bool
	}{
		{name: "no certificate"},
		{name: "untrusted certificate with apiserver name", cert: &forged},
		{name: "wrong identity", cert: func() *tls.Certificate { c := issue("tenant", x509.ExtKeyUsageClientAuth); return &c }()},
		{name: "apiserver", cert: func() *tls.Certificate { c := issue("front-proxy-client", x509.ExtKeyUsageClientAuth); return &c }(), allowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &tls.Config{RootCAs: roots, ServerName: "webhook.test", MinVersion: tls.VersionTLS12}
			if tc.cert != nil {
				cfg.Certificates = []tls.Certificate{*tc.cert}
			}
			tr := &http.Transport{TLSClientConfig: cfg}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
			req := admissionRequestFor(t, admissionv1.Create, "", authenticationv1.UserInfo{Username: "alice"}, nil)
			body, _ := json.Marshal(req)
			resp, err := client.Post("https://"+listener.Addr().String()+webhookPath, "application/json", bytes.NewReader(body))
			if err != nil {
				if tc.allowed {
					t.Fatal(err)
				}
				return
			}
			defer resp.Body.Close()
			if !tc.allowed {
				t.Fatal("unauthenticated caller reached signing handler")
			}
			var review admissionv1.AdmissionReview
			if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
				t.Fatal(err)
			}
			if review.Response == nil || len(review.Response.Patch) == 0 {
				t.Fatal("authenticated admission not stamped")
			}
		})
	}
}

func TestWebhookServerFailureDeregistersEndpoint(t *testing.T) {
	cs := fake.NewSimpleClientset()
	registered := make(chan struct{})
	cs.PrependReactor("create", "endpointslices", func(action ktesting.Action) (bool, runtime.Object, error) { close(registered); return false, nil, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	maintainWebhook(ctx, cs, "cp1", "192.0.2.1", &certCache{}, func(ctx context.Context) error {
		select {
		case <-registered:
			return fmt.Errorf("listener failed")
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil)
	if ctx.Err() != nil {
		t.Fatal("delivery loop did not stop on server failure")
	}
	slices, err := cs.DiscoveryV1().EndpointSlices(webhookNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(slices.Items) != 0 {
		t.Fatal("dead server still advertised")
	}
}

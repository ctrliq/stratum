package nodebackup

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newIdentity(t *testing.T) Identity {
	t.Helper()
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func write(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

// A key survives its text form, and a mistyped one is refused before it can
// seal a bundle nobody opens.
func TestKeysRoundTripAndRefuseATypo(t *testing.T) {
	id := newIdentity(t)
	s, err := id.String()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseIdentity(s + "\n")
	if err != nil {
		t.Fatalf("ParseIdentity: %v", err)
	}
	if back.Recipient().String() != id.Recipient().String() {
		t.Error("an identity read back has another recipient")
	}
	r := id.Recipient().String()
	if _, err := ParseRecipient(r); err != nil {
		t.Fatalf("ParseRecipient: %v", err)
	}
	typo := []byte(r)
	last := len(typo) - 10
	if typo[last] == 'a' {
		typo[last] = 'b'
	} else {
		typo[last] = 'a'
	}
	if _, err := ParseRecipient(string(typo)); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("a mistyped recipient: error = %v, want a checksum failure", err)
	}
	if _, err := ParseRecipient(s); err == nil {
		t.Error("an identity was accepted as a recipient")
	}
	if _, err := ParseIdentity(r); err == nil {
		t.Error("a recipient was accepted as an identity")
	}
	if _, err := ParseIdentity("stratum-backup-identity-notakey"); err == nil || strings.Contains(err.Error(), "notakey") {
		t.Errorf("a bad identity: error = %v, want one that does not repeat the input", err)
	}
	if _, err := ParseRecipients([]string{r, r}); err == nil {
		t.Error("the same recipient listed twice was accepted as two")
	}
}

// What one recipient's identity seals, each recipient's identity opens, and
// nobody else's.
func TestSealOpensForEachRecipientAndNoOther(t *testing.T) {
	a, b, stranger := newIdentity(t), newIdentity(t), newIdentity(t)
	plain := []byte("the cluster's root of trust")
	sealed, err := Seal([]Recipient{a.Recipient(), b.Recipient()}, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("the sealed bundle holds its plaintext")
	}
	for name, id := range map[string]Identity{"first": a, "second": b} {
		got, err := Open(id, sealed)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("the %s recipient: Open = %q, %v", name, got, err)
		}
	}
	if _, err := Open(stranger, sealed); !errors.Is(err, ErrNotForIdentity) {
		t.Errorf("a stranger's identity: error = %v, want ErrNotForIdentity", err)
	}
	if _, err := Seal(nil, plain); err == nil {
		t.Error("sealed a bundle to nobody: there is no unencrypted form")
	}
}

// A bundle that was altered anywhere, cut short, or written in a format this
// binary does not know fails to open, and never returns part of its contents.
func TestOpenFailsClosed(t *testing.T) {
	id := newIdentity(t)
	sealed, err := Seal([]Recipient{id.Recipient()}, bytes.Repeat([]byte("secret "), 100))
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{len(magic), len(magic) + 1, len(magic) + 8, len(sealed) / 2, len(sealed) - 1} {
		bad := bytes.Clone(sealed)
		bad[at] ^= 0x01
		if got, err := Open(id, bad); err == nil {
			t.Errorf("a bundle with byte %d flipped opened, returning %d bytes", at, len(got))
		}
	}
	for _, n := range []int{0, 5, len(magic) + 2, len(sealed) / 2, len(sealed) - 1} {
		if _, err := Open(id, sealed[:n]); err == nil {
			t.Errorf("a bundle cut to %d bytes opened", n)
		}
	}
	other := bytes.Clone(sealed)
	other[len(magic)] = formatVersion + 1
	if _, err := Open(id, other); err == nil || !strings.Contains(err.Error(), "format") {
		t.Errorf("a bundle of another format: error = %v, want one that says so", err)
	}
	// A second recipient spliced into the header would let its holder claim
	// the bundle was theirs to open; the header is authenticated with the body.
	spliced := bytes.Clone(sealed)
	spliced[len(magic)+2] = 2
	if _, err := Open(id, spliced); err == nil {
		t.Error("a bundle whose recipient count was changed opened")
	}
}

type node struct {
	src Sources
	dst Destination
}

// fixtureNode lays out a control plane's state under a temp root and returns
// where it is, with the destination a restore to the same paths may write.
func fixtureNode(t *testing.T) node {
	t.Helper()
	root := t.TempDir()
	pki := filepath.Join(root, "var/lib/stratum/pki")
	conf := filepath.Join(root, "etc/stratum")
	state := filepath.Join(root, "var/lib/stratum/config/node.yaml")
	manifests := filepath.Join(root, "var/lib/stratum/manifests")
	ca, caKey := testCA(t)
	write(t, filepath.Join(pki, "ca.crt"), ca, 0o644)
	write(t, filepath.Join(pki, "ca.key"), caKey, 0o600)
	write(t, filepath.Join(pki, "encryption-config.yaml"), "at-rest key", 0o600)
	write(t, filepath.Join(pki, "kubelet/kubelet-client-2026.pem"), "kubelet cert", 0o600)
	if err := os.Symlink(filepath.Join(pki, "kubelet/kubelet-client-2026.pem"), filepath.Join(pki, "kubelet/kubelet-client-current.pem")); err != nil {
		t.Fatal(err)
	}
	write(t, state, "role: controller\nhostname: cp-1\n", 0o600)
	write(t, filepath.Join(conf, "config.yaml"), "spec: {}\n", 0o644)
	write(t, filepath.Join(conf, "stratum.env"), "STRATUM_S3_SECRET_KEY=x\n", 0o600)
	write(t, filepath.Join(conf, "images.d/gpu.tar"), strings.Repeat("image bytes ", 1000), 0o644)
	write(t, filepath.Join(manifests, "app.yaml"), "kind: ConfigMap\n", 0o644)
	return node{
		src: Sources{PKIDir: pki, StatePath: state, ConfDir: conf, ManifestsDir: manifests,
			Skip: []string{filepath.Join(conf, "images.d")}},
		dst: Destination{PKIDir: pki, StatePath: state, ConfDir: conf, ManifestsDir: manifests},
	}
}

var testMeta = Meta{Hostname: "cp-1", NodeIP: "10.0.0.1", StratumVersion: "v1", KubernetesVersion: "v1.37.2",
	CreatedAt: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC), Snapshot: "etcd-snapshot-cp-1-20261125-120000.db"}

// A bundle taken from a node and restored to a wiped one puts back every file
// with its contents and mode, the kubelet's link beside its target, and
// nothing that is not node state.
func TestCollectThenExtractRestoresTheNode(t *testing.T) {
	n := fixtureNode(t)
	plain, m, err := Collect(n.src, testMeta)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if m.ClusterCASHA256 == "" || m.Hostname != "cp-1" || m.Snapshot != testMeta.Snapshot {
		t.Errorf("manifest = %+v, want the CA fingerprint and the node's identity", m)
	}
	for _, f := range m.Files {
		if strings.Contains(f.Path, "images.d") {
			t.Errorf("the bundle holds %s; image archives are not node state", f.Path)
		}
	}
	want := map[string]string{}
	modes := map[string]os.FileMode{}
	for _, f := range m.Files {
		if f.Dir || f.Link != "" {
			continue
		}
		b, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		want[f.Path], modes[f.Path] = string(b), os.FileMode(f.Mode)
	}
	if len(want) != 8 {
		t.Fatalf("the bundle holds %d files, want the 8 of node state", len(want))
	}
	for _, dir := range []string{n.src.PKIDir, n.src.ConfDir, n.src.ManifestsDir, filepath.Dir(n.src.StatePath)} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Inspect(plain, n.dst)
	if err != nil || got.ClusterCASHA256 != m.ClusterCASHA256 {
		t.Fatalf("Inspect = %+v, %v", got, err)
	}
	res, err := Extract(plain, n.dst)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.Files != 9 {
		t.Errorf("restored %d files and links, want 9", res.Files)
	}
	for path, data := range want {
		b, err := os.ReadFile(path)
		if err != nil || string(b) != data {
			t.Errorf("%s = %q, %v; want %q", path, b, err, data)
		}
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != modes[path] {
			t.Errorf("%s has mode %v, want %v", path, fi.Mode().Perm(), modes[path])
		}
	}
	link := filepath.Join(n.src.PKIDir, "kubelet/kubelet-client-current.pem")
	if target, err := os.Readlink(link); err != nil || target != "kubelet-client-2026.pem" {
		t.Errorf("the kubelet link points to %q (%v), want its target beside it", target, err)
	}
	if b, err := os.ReadFile(link); err != nil || string(b) != "kubelet cert" {
		t.Errorf("reading through the kubelet link = %q, %v", b, err)
	}
	if fi, err := os.Stat(n.src.PKIDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the PKI directory came back with mode %v (%v), want 0700", fi.Mode().Perm(), err)
	}
}

// What a bundle cannot hold whole it refuses to take, naming it, rather than
// leaving a gap that is found at a restore.
func TestCollectRefusesWhatItCannotHoldWhole(t *testing.T) {
	t.Run("a file over the cap", func(t *testing.T) {
		n := fixtureNode(t)
		old := maxFileBytes
		maxFileBytes = 1024
		t.Cleanup(func() { maxFileBytes = old })
		big := filepath.Join(n.src.ConfDir, "big.bin")
		write(t, big, strings.Repeat("x", 2000), 0o600)
		if _, _, err := Collect(n.src, testMeta); err == nil || !strings.Contains(err.Error(), big) {
			t.Errorf("error = %v, want one naming %s", err, big)
		}
	})
	t.Run("a tree over the cap", func(t *testing.T) {
		n := fixtureNode(t)
		old := maxBundleBytes
		maxBundleBytes = 512
		t.Cleanup(func() { maxBundleBytes = old })
		if _, _, err := Collect(n.src, testMeta); err == nil || !strings.Contains(err.Error(), "MiB") {
			t.Errorf("error = %v, want the size cap", err)
		}
	})
	t.Run("a link out of its directory", func(t *testing.T) {
		n := fixtureNode(t)
		link := filepath.Join(n.src.ConfDir, "shadow")
		if err := os.Symlink("/etc/shadow", link); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Collect(n.src, testMeta); err == nil || !strings.Contains(err.Error(), link) {
			t.Errorf("error = %v, want one naming %s", err, link)
		}
	})
	t.Run("no PKI", func(t *testing.T) {
		n := fixtureNode(t)
		if err := os.RemoveAll(n.src.PKIDir); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Collect(n.src, testMeta); err == nil {
			t.Error("collected a bundle from a node with no PKI")
		}
	})
	t.Run("no manifests directory is not an error", func(t *testing.T) {
		n := fixtureNode(t)
		if err := os.RemoveAll(n.src.ManifestsDir); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Collect(n.src, testMeta); err != nil {
			t.Errorf("Collect without the optional manifests directory: %v", err)
		}
	})
}

// A bundle proves who can read it, not who wrote it, so a restore writes only
// to the node-state paths whatever the bundle names, and a bundle it refuses
// changes nothing.
func TestExtractWritesOnlyNodeState(t *testing.T) {
	n := fixtureNode(t)
	outside := filepath.Join(t.TempDir(), "cron.d", "job")
	abcd := func(path string) entry {
		return entry{File: File{Path: path, Mode: 0o644, Size: 4,
			SHA256: "88d4266fd4e6338d13b845fcf289579d209c897823b9217da3e161936f031589"}, data: []byte("abcd")}
	}
	// Each forged bundle is otherwise whole, the cluster CA and its key
	// included, so the refusal is the path's and not a missing CA's.
	forge := func(t *testing.T, e entry) []byte {
		t.Helper()
		return rebuildBundle(t, n, func(entries []entry) []entry { return append(entries, e) })
	}
	wiped := func(t *testing.T) {
		t.Helper()
		if err := os.RemoveAll(n.src.PKIDir); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]struct {
		e     entry
		want  string
		plain []byte
	}{
		"a file outside node state":                {e: abcd(outside), want: "not node state"},
		"a sibling of the configuration directory": {e: abcd(n.src.ConfDir + "-evil/job"), want: "not node state"},
		"a link to another directory":              {e: entry{File: File{Path: filepath.Join(n.src.PKIDir, "x"), Mode: 0o777, Link: "../../etc/shadow"}}, want: "link"},
	}
	for name, c := range cases {
		c.plain = forge(t, c.e)
		cases[name] = c
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			plain := c.plain
			wiped(t)
			if _, err := Extract(plain, n.dst); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Extract = %v, want a refusal saying %q", err, c.want)
			}
			if _, err := os.Stat(filepath.Join(n.src.PKIDir, "ca.key")); !os.IsNotExist(err) {
				t.Errorf("a refused bundle still wrote the CA key (stat: %v)", err)
			}
			if _, err := os.Lstat(outside); !os.IsNotExist(err) {
				t.Errorf("a refused bundle wrote outside node state (stat: %v)", err)
			}
		})
	}
	t.Run("contents that do not match the manifest", func(t *testing.T) {
		ok, err := readFile(filepath.Join(n.src.ConfDir, "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		tampered := ok
		tampered.data = []byte("spec: {evil}\n")[:len(ok.data)]
		plain, err := writeArchive(Manifest{Format: formatVersion, Files: []File{ok.File}}, []entry{tampered})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Extract(plain, n.dst); err == nil || !strings.Contains(err.Error(), "manifest") {
			t.Errorf("error = %v, want a mismatch with the manifest", err)
		}
	})
	t.Run("a file the manifest lists and the bundle lacks", func(t *testing.T) {
		ok, err := readFile(filepath.Join(n.src.ConfDir, "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		gone := File{Path: filepath.Join(n.src.PKIDir, "ca.key"), Mode: 0o600, Size: 1, SHA256: "00"}
		plain, err := writeArchive(Manifest{Format: formatVersion, Files: []File{ok.File, gone}}, []entry{ok})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Extract(plain, n.dst); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Errorf("error = %v, want the missing file named", err)
		}
	})
}

// The cluster config a node keeps outside its configuration directory is in
// the bundle, and goes back to that path only when the operator names it: the
// path comes from the bundle, and a bundle may not choose where it writes.
func TestExtractWritesTheOutsideConfigOnlyWhereAllowed(t *testing.T) {
	n := fixtureNode(t)
	extra := filepath.Join(t.TempDir(), "site", "cluster.yaml")
	write(t, extra, "spec: {site: true}\n", 0o640)
	n.src.ExtraConfig = extra
	n.src.ConfigPath = extra
	plain, m, err := Collect(n.src, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	if m.ExtraConfig != extra {
		t.Fatalf("manifest names the outside config as %q, want %s", m.ExtraConfig, extra)
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	res, err := Extract(plain, n.dst)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.ExtraConfigSkipped != extra {
		t.Errorf("Extract reported %q skipped, want %s", res.ExtraConfigSkipped, extra)
	}
	if _, err := os.Stat(extra); !os.IsNotExist(err) {
		t.Errorf("the outside config was written without being allowed (stat: %v)", err)
	}
	n.dst.AllowExtra = extra
	if res, err = Extract(plain, n.dst); err != nil || res.ExtraConfigSkipped != "" {
		t.Fatalf("Extract with the path allowed = %+v, %v", res, err)
	}
	if b, err := os.ReadFile(extra); err != nil || string(b) != "spec: {site: true}\n" {
		t.Errorf("the outside config = %q, %v", b, err)
	}
}

// testCA is a self-signed CA certificate and its key, PEM encoded: a restore
// checks that a bundle's CA key is its CA certificate's.
func testCA(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "stratum-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}

// The fingerprint a restore shows and pins is the carried certificate's own.
// What a bundle states of itself is written by whoever sealed it, so a bundle
// that states one CA and carries another is refused, as is one whose CA key
// is not its CA certificate's.
func TestInspectTakesTheCAFromTheCertificateNotTheManifest(t *testing.T) {
	n := fixtureNode(t)
	plain, m, err := Collect(n.src, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Inspect(plain, n.dst)
	if err != nil || got.ClusterCASHA256 != m.ClusterCASHA256 || len(got.ClusterCASHA256) != 64 {
		t.Fatalf("Inspect = %q, %v; want the certificate's SHA-256", got.ClusterCASHA256, err)
	}
	rebuild := func(t *testing.T, change func(m *Manifest, entries []entry)) []byte {
		t.Helper()
		var entries []entry
		for _, f := range m.Files {
			e := entry{File: f}
			if !f.Dir && f.Link == "" {
				b, err := os.ReadFile(f.Path)
				if err != nil {
					t.Fatal(err)
				}
				e.data = b
			}
			entries = append(entries, e)
		}
		forged := m
		forged.Files = nil
		change(&forged, entries)
		for _, e := range entries {
			forged.Files = append(forged.Files, e.File)
		}
		out, err := writeArchive(forged, entries)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	t.Run("a manifest that claims another cluster's CA", func(t *testing.T) {
		forged := rebuild(t, func(m *Manifest, _ []entry) { m.ClusterCASHA256 = strings.Repeat("ab", 32) })
		if _, err := Inspect(forged, n.dst); err == nil || !strings.Contains(err.Error(), "states cluster CA") {
			t.Errorf("error = %v, want a refusal of the stated CA", err)
		}
		if _, err := Extract(forged, n.dst); err == nil {
			t.Error("Extract restored a bundle that lies about its CA")
		}
	})
	t.Run("a CA key that is not the certificate's", func(t *testing.T) {
		_, otherKey := testCA(t)
		forged := rebuild(t, func(_ *Manifest, entries []entry) {
			for i := range entries {
				if entries[i].Path == filepath.Join(n.src.PKIDir, "ca.key") {
					sum := sha256.Sum256([]byte(otherKey))
					entries[i].data, entries[i].Size, entries[i].SHA256 = []byte(otherKey), int64(len(otherKey)), hex.EncodeToString(sum[:])
				}
			}
		})
		if _, err := Inspect(forged, n.dst); err == nil || !strings.Contains(err.Error(), "not the key of the cluster CA certificate") {
			t.Errorf("error = %v, want a refusal of the mismatched key", err)
		}
	})
}

// A link beside its target is node state wherever it sits, in the
// configuration directory as in the PKI directory: what a save takes, a
// restore accepts.
func TestALinkInTheConfigDirectoryRoundTrips(t *testing.T) {
	n := fixtureNode(t)
	link := filepath.Join(n.src.ConfDir, "current.yaml")
	if err := os.Symlink("config.yaml", link); err != nil {
		t.Fatal(err)
	}
	plain, _, err := Collect(n.src, testMeta)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := os.RemoveAll(n.src.ConfDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(n.src.PKIDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(plain, n.dst); err != nil {
		t.Fatalf("Extract refused a bundle Collect took: %v", err)
	}
	if target, err := os.Readlink(link); err != nil || target != "config.yaml" {
		t.Errorf("the link came back as %q (%v)", target, err)
	}
}

// A save refuses node state it could seal and a restore would turn away:
// found at the save, the node is still there to fix; found at the restore, it
// is gone.
func TestCollectRefusesABundleARestoreWouldRefuse(t *testing.T) {
	n := fixtureNode(t)
	for i := range 400 {
		write(t, filepath.Join(n.src.ManifestsDir, fmt.Sprintf("m-%04d.yaml", i)), "x", 0o600)
	}
	old := maxBundleBytes
	maxBundleBytes = 16 << 10
	t.Cleanup(func() { maxBundleBytes = old })
	if _, _, err := Collect(n.src, testMeta); err == nil || !strings.Contains(err.Error(), "a restore would refuse") {
		t.Errorf("Collect = %v, want a refusal: the manifest of that many files is over what a restore reads", err)
	}
}

// rebuildBundle re-archives the node at n, after change has altered the
// entries: a bundle only someone other than Collect would write.
func rebuildBundle(t *testing.T, n node, change func(entries []entry) []entry) []byte {
	t.Helper()
	return rebuildBundleManifest(t, n, nil, change)
}

// rebuildBundleManifest is rebuildBundle that also lets the test rewrite the
// manifest's own fields before the archive is written.
func rebuildBundleManifest(t *testing.T, n node, manifest func(*Manifest), change func(entries []entry) []entry) []byte {
	t.Helper()
	_, m, err := Collect(n.src, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	var entries []entry
	for _, f := range m.Files {
		e := entry{File: f}
		if !f.Dir && f.Link == "" {
			b, err := os.ReadFile(f.Path)
			if err != nil {
				t.Fatal(err)
			}
			e.data = b
		}
		entries = append(entries, e)
	}
	if change != nil {
		entries = change(entries)
	}
	if manifest != nil {
		manifest(&m)
	}
	m.Files = nil
	for _, e := range entries {
		m.Files = append(m.Files, e.File)
	}
	out, err := writeArchive(m, entries)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A bundle without the cluster CA certificate and its key restores a node
// that mints a new CA or refuses to start, so neither side accepts one.
func TestABundleMustHoldTheClusterCAAndItsKey(t *testing.T) {
	for _, gone := range []string{"ca.crt", "ca.key"} {
		n := fixtureNode(t)
		if err := os.Remove(filepath.Join(n.src.PKIDir, gone)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Collect(n.src, testMeta); err == nil || !strings.Contains(err.Error(), "cluster CA") {
			t.Errorf("without %s: Collect = %v, want a refusal naming the cluster CA", gone, err)
		}
	}
}

// A file is restored whole or not at all: an entry longer than its manifest
// states, or than a bundle may hold, is refused, never cut to the part that
// was hashed.
func TestExtractRefusesAnEntryOfAnotherSize(t *testing.T) {
	n := fixtureNode(t)
	target := filepath.Join(n.src.ConfDir, "config.yaml")
	_, m, err := Collect(n.src, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	var entries []entry
	for i, f := range m.Files {
		e := entry{File: f}
		if !f.Dir && f.Link == "" {
			b, err := os.ReadFile(f.Path)
			if err != nil {
				t.Fatal(err)
			}
			e.data = b
		}
		if f.Path == target {
			// The archive carries more than the manifest states, hashed whole:
			// only the stated size tells the two apart.
			e.data = append(e.data, []byte("trailing bytes the manifest does not count")...)
			sum := sha256.Sum256(e.data)
			e.Size, e.SHA256 = int64(len(e.data)), hex.EncodeToString(sum[:])
			m.Files[i].SHA256 = e.SHA256
		}
		entries = append(entries, e)
	}
	forged, err := writeArchive(m, entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(forged, n.dst); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("error = %v, want a refusal over the entry's size", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a refused bundle wrote %s (stat: %v)", target, err)
	}
}

// An entry beneath a link in the same bundle would be written through the
// link, so a bundle that holds one is refused.
func TestExtractRefusesAnEntryBeneathALink(t *testing.T) {
	n := fixtureNode(t)
	link := filepath.Join(n.src.ConfDir, "sub")
	forged := rebuildBundle(t, n, func(entries []entry) []entry {
		data := []byte("x")
		sum := sha256.Sum256(data)
		return append(entries,
			entry{File: File{Path: link, Mode: 0o777, Link: "images.d"}},
			entry{File: File{Path: filepath.Join(link, "job"), Mode: 0o600, Size: 1, SHA256: hex.EncodeToString(sum[:])}, data: data})
	})
	if _, err := Extract(forged, n.dst); err == nil || !strings.Contains(err.Error(), "beneath the link") {
		t.Errorf("error = %v, want a refusal of the entry beneath the link", err)
	}
}

// A directory already on the machine that links out of node state would
// carry an allowed entry with it. The restore refuses before it writes
// anything, and what the link points to is untouched.
func TestExtractRefusesALinkedDirectoryOnTheMachine(t *testing.T) {
	n := fixtureNode(t)
	write(t, filepath.Join(n.src.ConfDir, "hooks", "job"), "node state", 0o600)
	plain, _, err := Collect(n.src, testMeta)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	write(t, filepath.Join(outside, "job"), "not node state", 0o600)
	for _, dir := range []string{n.src.PKIDir, n.src.ConfDir} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(n.src.ConfDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(n.src.ConfDir, "hooks")); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(plain, n.dst); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("error = %v, want a refusal of the linked directory", err)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "job")); err != nil || string(b) != "not node state" {
		t.Errorf("the restore wrote through the link: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(n.src.PKIDir, "ca.key")); !os.IsNotExist(err) {
		t.Errorf("a refused restore wrote the CA key (stat: %v)", err)
	}
}

// The cluster config a node runs with, kept outside the configuration
// directory, is part of the bundle or there is no bundle: one that names it
// and does not hold it restores a node that cannot start.
func TestCollectRequiresTheOutsideConfigItNames(t *testing.T) {
	n := fixtureNode(t)
	n.src.ExtraConfig = filepath.Join(t.TempDir(), "gone.yaml")
	if _, _, err := Collect(n.src, testMeta); err == nil || !strings.Contains(err.Error(), "gone.yaml") {
		t.Errorf("Collect = %v, want a refusal naming the missing config", err)
	}
}

// `stratum up --config` takes a cluster config of any name. A node that runs
// with one kept outside the configuration directory and not named .yaml takes
// a bundle like any other, and a restore puts the config back at the path it
// was recorded under, owner-only, only when the operator allows that path.
func TestAnOutsideConfigOfAnyNameRoundTrips(t *testing.T) {
	n := fixtureNode(t)
	extra := filepath.Join(t.TempDir(), "srv", "cluster.conf")
	write(t, extra, "spec: {site: true}\n", 0o644)
	n.src.ExtraConfig, n.src.ConfigPath = extra, extra
	plain, m, err := Collect(n.src, testMeta)
	if err != nil {
		t.Fatalf("Collect with the config at %s: %v", extra, err)
	}
	if m.ExtraConfig != extra || m.ConfigPath != extra {
		t.Fatalf("manifest names the config as %q and %q, want %s", m.ExtraConfig, m.ConfigPath, extra)
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	res, err := Extract(plain, n.dst)
	if err != nil || res.ExtraConfigSkipped != extra {
		t.Fatalf("Extract without the path allowed = %+v, %v; want %s skipped", res, err, extra)
	}
	if _, err := os.Lstat(extra); !os.IsNotExist(err) {
		t.Fatalf("the outside config was written without being allowed (stat: %v)", err)
	}
	dst := n.dst
	dst.AllowExtra = extra
	if res, err = Extract(plain, dst); err != nil || res.ExtraConfigSkipped != "" {
		t.Fatalf("Extract with the path allowed = %+v, %v", res, err)
	}
	fi, err := os.Lstat(extra)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the outside config is %v, %v; want a regular file, 0600", fi.Mode(), err)
	}
	if b, err := os.ReadFile(extra); err != nil || string(b) != "spec: {site: true}\n" {
		t.Errorf("the outside config = %q, %v", b, err)
	}
}

// A save checks its bundle the way a restore will, and a save that refuses
// one says so as a save: it has restored nothing because it restores
// nothing. The restore that refuses the same bundle says nothing was
// restored.
func TestASaveRefusalReadsAsASave(t *testing.T) {
	for name, save := range map[string]func(n node) error{
		"a bundle without the CA key": func(n node) error {
			if err := os.Remove(filepath.Join(n.src.PKIDir, "ca.key")); err != nil {
				t.Fatal(err)
			}
			_, _, err := Collect(n.src, testMeta)
			return err
		},
		"a manifest the restore side refuses": func(n node) error {
			meta := testMeta
			meta.Hostname = "cp-1\nok"
			_, _, err := Collect(n.src, meta)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := save(fixtureNode(t))
			if err == nil {
				t.Fatal("Collect took the bundle")
			}
			if strings.Contains(strings.ToLower(err.Error()), "nothing was restored") {
				t.Errorf("Collect = %q; a save restores nothing and must not say so", err)
			}
			if !strings.Contains(err.Error(), "a restore would refuse") {
				t.Errorf("Collect = %q, want the refusal to read as a save's", err)
			}
		})
	}
	n := fixtureNode(t)
	plain := rebuildBundleManifest(t, n, func(m *Manifest) { m.Hostname = "cp-1\nok" }, nil)
	for name, restore := range map[string]func() error{
		"Inspect": func() error { _, err := Inspect(plain, n.dst); return err },
		"Extract": func() error { _, err := Extract(plain, n.dst); return err },
	} {
		err := restore()
		if err == nil || !strings.HasSuffix(err.Error(), "; nothing was restored") ||
			strings.Count(strings.ToLower(err.Error()), "nothing was restored") != 1 {
			t.Errorf("%s = %v, want the refusal to say once that nothing was restored", name, err)
		}
	}
}

// The fields a restore prints come from whoever sealed the bundle. One that
// holds a control character could redraw the CA fingerprint on the terminal,
// and a config path is printed inside a command to run, so neither is read.
func TestReadManifestRefusesTextThatCouldMislead(t *testing.T) {
	n := fixtureNode(t)
	for name, mutate := range map[string]func(*Manifest){
		"an escape in the snapshot name":    func(m *Manifest) { m.Snapshot = "x\x1b[1A\r\x1b[2K  cluster CA SHA-256: 00" },
		"a newline in the hostname":         func(m *Manifest) { m.Hostname = "cp-1\nok" },
		"a relative config path":            func(m *Manifest) { m.ConfigPath = "etc/stratum/config.yaml" },
		"an outside config not the one run": func(m *Manifest) { m.ExtraConfig = "/root/elsewhere.yaml" },
	} {
		t.Run(name, func(t *testing.T) {
			plain := rebuildBundleManifest(t, n, mutate, nil)
			if _, err := Inspect(plain, n.dst); err == nil || !strings.Contains(err.Error(), "manifest") {
				t.Fatalf("Inspect = %v, want the manifest refused", err)
			}
		})
	}
}

// The one path outside node state a restore may write is the bundle's own
// outside config, as a file, at the path the operator named, owner-only.
func TestExtractWritesTheAllowedPathOnlyAsTheBundlesConfigFile(t *testing.T) {
	t.Run("an allowed path the manifest does not name", func(t *testing.T) {
		n := fixtureNode(t)
		target := filepath.Join(t.TempDir(), "elsewhere")
		plain := rebuildBundle(t, n, func(entries []entry) []entry {
			return append(entries, entry{File: File{Path: target, Mode: 0o777, Dir: true}})
		})
		dst := n.dst
		dst.AllowExtra = target
		if _, err := Extract(plain, dst); err == nil {
			t.Fatal("Extract wrote a path the operator allowed but the bundle's manifest does not name as its config")
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Errorf("the refused bundle created %s (stat: %v)", target, err)
		}
	})
	t.Run("the config written owner-only", func(t *testing.T) {
		n := fixtureNode(t)
		extra := filepath.Join(t.TempDir(), "site", "cluster.yaml")
		write(t, extra, "spec: {}\n", 0o666)
		if err := os.Chmod(extra, 0o666); err != nil {
			t.Fatal(err)
		}
		n.src.ExtraConfig, n.src.ConfigPath = extra, extra
		plain, _, err := Collect(n.src, testMeta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(extra); err != nil {
			t.Fatal(err)
		}
		dst := n.dst
		dst.AllowExtra = extra
		if _, err := Extract(plain, dst); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(extra)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("the outside config is %v, %v; want 0600", fi.Mode().Perm(), err)
		}
	})
}

// A directory entry whose own path is a link on this machine is refused:
// making it and setting its mode would follow the link out of node state.
func TestExtractRefusesADirectoryEntryThatIsALinkOnTheMachine(t *testing.T) {
	n := fixtureNode(t)
	hooks := filepath.Join(n.src.ConfDir, "hooks")
	plain := rebuildBundle(t, n, func(entries []entry) []entry {
		return append(entries, entry{File: File{Path: hooks, Mode: 0o777, Dir: true}})
	})
	elsewhere := t.TempDir()
	if err := os.Chmod(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, hooks); err != nil {
		t.Fatal(err)
	}
	if _, err := Extract(plain, n.dst); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Extract = %v, want a refusal naming the link", err)
	}
	if fi, err := os.Stat(elsewhere); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the directory the link names is now %v (%v); want it untouched at 0700", fi.Mode().Perm(), err)
	}
}

// A config naming more recipients than a bundle can be sealed to is refused
// when it is read, not at every snapshot after.
func TestParseRecipientsRefusesMoreThanABundleTakes(t *testing.T) {
	var list []string
	for range maxRecipients + 1 {
		id, err := GenerateIdentity()
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, id.Recipient().String())
	}
	if _, err := ParseRecipients(list[:maxRecipients]); err != nil {
		t.Fatalf("ParseRecipients(%d): %v", maxRecipients, err)
	}
	if _, err := ParseRecipients(list); err == nil {
		t.Fatalf("ParseRecipients(%d) accepted more recipients than Seal takes", len(list))
	}
}

// node.yaml and the outside config sit in no tree, so the directories above
// them are checked too: a link there would carry the write out of node state.
func TestExtractRefusesALinkAboveAFileOutsideTheTrees(t *testing.T) {
	t.Run("node.yaml", func(t *testing.T) {
		n := fixtureNode(t)
		plain, _, err := Collect(n.src, testMeta)
		if err != nil {
			t.Fatal(err)
		}
		configDir := filepath.Dir(n.src.StatePath)
		if err := os.RemoveAll(configDir); err != nil {
			t.Fatal(err)
		}
		elsewhere := t.TempDir()
		if err := os.Symlink(elsewhere, configDir); err != nil {
			t.Fatal(err)
		}
		if _, err := Extract(plain, n.dst); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("Extract = %v, want a refusal naming the link", err)
		}
		if _, err := os.Lstat(filepath.Join(elsewhere, filepath.Base(n.src.StatePath))); !os.IsNotExist(err) {
			t.Errorf("node.yaml was written through the link (stat: %v)", err)
		}
	})
	t.Run("the outside config", func(t *testing.T) {
		n := fixtureNode(t)
		site := filepath.Join(t.TempDir(), "site")
		extra := filepath.Join(site, "cluster.yaml")
		write(t, extra, "spec: {}\n", 0o600)
		n.src.ExtraConfig, n.src.ConfigPath = extra, extra
		plain, _, err := Collect(n.src, testMeta)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(site); err != nil {
			t.Fatal(err)
		}
		elsewhere := t.TempDir()
		if err := os.Symlink(elsewhere, site); err != nil {
			t.Fatal(err)
		}
		dst := n.dst
		dst.AllowExtra = extra
		if _, err := Extract(plain, dst); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("Extract = %v, want a refusal naming the link", err)
		}
		if _, err := os.Lstat(filepath.Join(elsewhere, "cluster.yaml")); !os.IsNotExist(err) {
			t.Errorf("the outside config was written through the link (stat: %v)", err)
		}
	})
}

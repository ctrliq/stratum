package nodebackup

import (
	"bytes"
	"encoding/pem"
	"errors"
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
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("the cluster CA")})
	write(t, filepath.Join(pki, "ca.crt"), string(ca), 0o644)
	write(t, filepath.Join(pki, "ca.key"), "CA KEY", 0o600)
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
	got, err := ReadManifest(plain)
	if err != nil || got.ClusterCASHA256 != m.ClusterCASHA256 {
		t.Fatalf("ReadManifest = %+v, %v", got, err)
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
		maxFileBytes = 128
		t.Cleanup(func() { maxFileBytes = old })
		big := filepath.Join(n.src.ConfDir, "big.bin")
		write(t, big, strings.Repeat("x", 200), 0o600)
		if _, _, err := Collect(n.src, testMeta); err == nil || !strings.Contains(err.Error(), big) {
			t.Errorf("error = %v, want one naming %s", err, big)
		}
	})
	t.Run("a tree over the cap", func(t *testing.T) {
		n := fixtureNode(t)
		old := maxBundleBytes
		maxBundleBytes = 64
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
	ok, err := readFile(filepath.Join(n.src.PKIDir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	forge := func(t *testing.T, e entry) []byte {
		t.Helper()
		entries := []entry{ok, e}
		m := Manifest{Format: formatVersion, Meta: testMeta, Files: []File{ok.File, e.File}}
		plain, err := writeArchive(m, entries)
		if err != nil {
			t.Fatal(err)
		}
		return plain
	}
	wiped := func(t *testing.T) {
		t.Helper()
		if err := os.RemoveAll(n.src.PKIDir); err != nil {
			t.Fatal(err)
		}
	}
	for name, e := range map[string]entry{
		"a file outside node state": {File: File{Path: outside, Mode: 0o644, Size: 4,
			SHA256: "88d4266fd4e6338d13b845fcf289579d209c897823b9217da3e161936f031589"}, data: []byte("abcd")},
		"a link out of the PKI directory": {File: File{Path: filepath.Join(n.src.ConfDir, "x"), Mode: 0o777, Link: "y"}},
		"a link to another directory":     {File: File{Path: filepath.Join(n.src.PKIDir, "x"), Mode: 0o777, Link: "../../etc/shadow"}},
	} {
		t.Run(name, func(t *testing.T) {
			plain := forge(t, e)
			wiped(t)
			if _, err := Extract(plain, n.dst); err == nil {
				t.Fatal("Extract accepted it")
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

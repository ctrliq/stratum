package host

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// Every dir that holds etcd data, private keys, credentialed kubeconfigs or
// audit logs is in the private (0700) list, and none of them is public.
func TestDirsKeepThePrivateDirsPrivate(t *testing.T) {
	pub, priv := publicDirs(), privateDirs()
	for _, d := range []string{version.EtcdDataDir, version.PKIDir, version.KubeconfigDir, version.AuditLogDir} {
		if !slices.Contains(priv, d) {
			t.Errorf("%s is not in the private (0700) list", d)
		}
		if slices.Contains(pub, d) {
			t.Errorf("%s is in the public (0755) list; it must be 0700", d)
		}
	}
}

// privateMode is d's permission bits, failing the test if d cannot be read.
func privateMode(t *testing.T, d string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(d)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// looseDir creates d at exactly 0755, whatever the umask, so a test that
// expects it tightened is not satisfied by the umask alone.
func looseDir(t *testing.T, d string) {
	t.Helper()
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateDirTightensAnExistingDir(t *testing.T) {
	d := filepath.Join(t.TempDir(), "data")
	looseDir(t, d)
	if err := ensurePrivateDir(d); err != nil {
		t.Fatal(err)
	}
	if m := privateMode(t, d); m != 0o700 {
		t.Errorf("existing 0755 dir was not tightened: got %o, want 700", m)
	}
}

// A dir that does not exist yet is created private.
func TestPrivateDirCreatesANewDirPrivate(t *testing.T) {
	d := filepath.Join(t.TempDir(), "new")
	if err := ensurePrivateDir(d); err != nil {
		t.Fatal(err)
	}
	if m := privateMode(t, d); m != 0o700 {
		t.Errorf("new dir is %o, want 700", m)
	}
}

// A restore kept before the data dir was private left a 0755 copy of the
// whole cluster state beside it; every start tightens those copies too.
func TestDirsTightenEtcdRestoreCopies(t *testing.T) {
	// A path with glob metacharacters: the copies are matched by prefix.
	root := filepath.Join(t.TempDir(), "a[b*c?")
	data := filepath.Join(root, "etcd")
	pre := data + ".pre-restore-20261016-120000"
	failed := data + ".failed-restore-20261016-120000"
	other := filepath.Join(root, "etcd-unrelated")
	// Names that share a prefix but are not restore copies: no "-" after the
	// kind, or nothing after it.
	nearPre := data + ".pre-restorefoo"
	nearFailed := data + ".failed-restore"
	for _, d := range []string{pre, failed, other, nearPre, nearFailed} {
		looseDir(t, d)
	}
	target := filepath.Join(root, "target")
	looseDir(t, target)
	if err := os.Symlink(target, data+".pre-restore-link"); err != nil {
		t.Fatal(err)
	}

	if err := tightenEtcdRestoreCopies(data); err != nil {
		t.Fatal(err)
	}
	for d, want := range map[string]os.FileMode{pre: 0o700, failed: 0o700, other: 0o755, nearPre: 0o755, nearFailed: 0o755, target: 0o755} {
		if m := privateMode(t, d); m != want {
			t.Errorf("%s is %o, want %o", d, m, want)
		}
	}
}

// A data dir whose parent does not exist yet has no copies to tighten.
func TestTightenEtcdRestoreCopiesWithoutAParent(t *testing.T) {
	if err := tightenEtcdRestoreCopies(filepath.Join(t.TempDir(), "missing", "etcd")); err != nil {
		t.Errorf("tightenEtcdRestoreCopies with no parent = %v, want nil", err)
	}
}

// A parent that cannot be listed warns and does not fail the start: the live
// data dir is already private, and the copies cannot be found to tighten.
func TestTightenEtcdRestoreCopiesWarnsWhenItCannotList(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "parent")
	if err := os.WriteFile(parent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	out := quiet(t, func() { err = tightenEtcdRestoreCopies(filepath.Join(parent, "etcd")) })
	if err != nil {
		t.Errorf("tightenEtcdRestoreCopies with an unlistable parent = %v, want nil", err)
	}
	if !strings.Contains(out, "WARN: could not list "+parent) {
		t.Errorf("no warning naming %s; printed:\n%s", parent, out)
	}
}

// Dirs makes every private dir, the etcd data dir's restore copies included,
// 0700 on every start, whatever an older release left them at.
func TestDirsMakesTheEtcdDataDirAndItsCopiesPrivate(t *testing.T) {
	root := t.TempDir()
	oldData, oldConf, oldRun := version.DataDir, version.ConfDir, version.RunDir
	oldAudit, oldKubelet, oldCNI := version.AuditLogDir, KubeletDir, CNIDir
	t.Cleanup(func() {
		version.SetRoots(oldData, oldConf, oldRun)
		version.AuditLogDir, KubeletDir, CNIDir = oldAudit, oldKubelet, oldCNI
	})
	version.SetRoots(filepath.Join(root, "data"), filepath.Join(root, "conf"), filepath.Join(root, "run"))
	version.AuditLogDir = filepath.Join(root, "audit")
	KubeletDir, CNIDir = filepath.Join(root, "kubelet"), filepath.Join(root, "cni")

	pre := version.EtcdDataDir + ".pre-restore-20261016-120000"
	failed := version.EtcdDataDir + ".failed-restore-20261016-120000"
	private := append(privateDirs(), pre, failed)
	for _, d := range private {
		looseDir(t, d)
	}
	if err := Dirs(); err != nil {
		t.Fatal(err)
	}
	for _, d := range private {
		if m := privateMode(t, d); m != 0o700 {
			t.Errorf("%s is %o after Dirs, want 700", d, m)
		}
	}
}

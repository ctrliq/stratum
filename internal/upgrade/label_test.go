package upgrade

import (
	"io"

	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/staticpod"
)

// recordLabels stands in for the SELinux labeller and returns the destinations
// it labelled, checking that each was labelled through its temp file before
// the rename.
func recordLabels(t *testing.T) *[]string {
	t.Helper()
	var dsts []string
	old := labelLike
	t.Cleanup(func() { labelLike = old })
	labelLike = func(_ io.Writer, path, dst string) error {
		if filepath.Dir(path) != filepath.Dir(dst) || !strings.HasPrefix(filepath.Base(path), "."+filepath.Base(dst)+".tmp-") {
			t.Errorf("labelled %s for %s, want its temp file", path, dst)
		}
		dsts = append(dsts, dst)
		return nil
	}
	return &dsts
}

// An upgrade labels the backup and the new binary: the new stratum must run
// in the runtime's domain after a reboot, and the backup is what a rollback
// copies back.
func TestBackupAndInstallLabelsBackupAndBinary(t *testing.T) {
	dir := t.TempDir()
	dst, src := filepath.Join(dir, "stratum"), filepath.Join(dir, "next")
	writeFile(t, dst, "old", 0o755)
	writeFile(t, src, "new", 0o755)
	labelled := recordLabels(t)

	bak, err := backupAndInstall(io.Discard, dst, src, "20261011-120000")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{bak, dst}; strings.Join(*labelled, " ") != strings.Join(want, " ") {
		t.Errorf("labelled %q, want %q", *labelled, want)
	}
}

// A rollback restores each binary through the same write, so the restored
// binary is labelled too.
func TestRestoreLabelsTheRestoredBinary(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "kubelet")
	bak := dst + ".bak-20261011-120000"
	writeFile(t, dst, "new", 0o755)
	writeFile(t, bak, "old", 0o755)
	labelled := recordLabels(t)

	if err := restoreAll(io.Discard, []backupItem{{name: "kubelet", bak: bak, dst: dst}}); err != nil {
		t.Fatal(err)
	}
	if len(*labelled) != 1 || (*labelled)[0] != dst {
		t.Errorf("labelled %q, want [%s]", *labelled, dst)
	}
}

// A binary that cannot be labelled is not installed: the live one stays, so an
// enforcing node is never left with a binary that runs unconfined.
func TestInstallStopsOnALabelFailure(t *testing.T) {
	dir := t.TempDir()
	dst, src := filepath.Join(dir, "stratum"), filepath.Join(dir, "next")
	writeFile(t, dst, "old", 0o755)
	writeFile(t, src, "new", 0o755)
	old := labelLike
	t.Cleanup(func() { labelLike = old })
	boom := errors.New("matchpathcon is not installed")
	labelLike = func(io.Writer, string, string) error { return boom }

	if err := atomicInstall(io.Discard, src, dst); !errors.Is(err, boom) {
		t.Fatalf("atomicInstall = %v, want the label error", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" {
		t.Errorf("%s = %q, want the old binary kept", dst, b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// A rollback to manifests that predate spc_t turns containerd's SELinux
// labelling off, so the restored control plane is not confined out of its own
// data; manifests that run as spc_t leave it alone.
func TestUnconfineForOldManifests(t *testing.T) {
	old := disableSELinux
	t.Cleanup(func() { disableSELinux = old })
	calls := 0
	disableSELinux = func() (bool, error) { calls++; return true, nil }

	for _, tc := range []struct {
		name     string
		manifest string
		want     int
	}{
		{"manifests without spc_t", "apiVersion: v1\nkind: Pod\nspec:\n  hostNetwork: true\n", 1},
		{"manifests with spc_t", "apiVersion: v1\nkind: Pod\nspec:\n  securityContext:\n    seLinuxOptions:\n      type: spc_t\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls = 0
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "etcd.yaml"), tc.manifest, 0o600)
			if err := unconfineForOldManifests(io.Discard, dir); err != nil {
				t.Fatal(err)
			}
			if calls != tc.want {
				t.Errorf("disableSELinux called %d times, want %d", calls, tc.want)
			}
		})
	}
	calls = 0
	if err := unconfineForOldManifests(io.Discard, ""); err != nil || calls != 0 {
		t.Errorf("no manifest backup: err %v, %d calls; want neither", err, calls)
	}
}

// A rollback to manifests that predate running as spc_t turns containerd's
// SELinux labelling off first. If that fails, the old manifests stay where
// they are: restored under labelling, the control plane would run as
// container_t and lose its certificates and etcd's data. The rollback reports
// itself incomplete instead.
func TestRollbackKeepsTheManifestsWhenConfinementStaysOn(t *testing.T) {
	newEtcdDowngradeFixture(t, ha.EtcdStorage{StorageVersion: "3.6.0", Members: 1})
	resumeFixture(t)
	writeFile(t, filepath.Join(staticpod.Dir(), "..", "node.yaml"), "role: controller\n", 0o600)
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.9"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(manifestBakDir(resumeTS), "etcd.yaml"), etcdManifest(oldEtcdImage), 0o600)
	live := filepath.Join(staticpod.Dir(), "etcd.yaml")
	writeFile(t, live, etcdManifest(newEtcdImage), 0o600)
	swap(t, &disableSELinux, func() (bool, error) { return false, errors.New("rename containerd.toml: read-only file system") })

	err := RollbackNode(io.Discard, RollbackOptions{})
	if err == nil || !strings.Contains(err.Error(), "INCOMPLETE") || !strings.Contains(err.Error(), manifestsItem) {
		t.Fatalf("err = %v, want an incomplete rollback naming the manifests", err)
	}
	if b, _ := os.ReadFile(live); !strings.Contains(string(b), newEtcdImage) {
		t.Errorf("live manifest %q, want it left on the new etcd", b)
	}
}

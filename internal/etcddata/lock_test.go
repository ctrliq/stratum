package etcddata

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func TestProductionLockSurvivesUninstall(t *testing.T) {
	path := lockPath(version.EtcdDataDir)
	if path == version.DataDir || strings.HasPrefix(path, version.DataDir+string(filepath.Separator)) {
		t.Fatalf("lock %s would be removed with uninstall target %s", path, version.DataDir)
	}
}

func TestLockExcludesOtherDataWriter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "etcd")
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if otherRelease, err := Lock(dir); err == nil {
		otherRelease()
		t.Fatal("a concurrent writer acquired the same data lock")
	}
	release()
	release, err = Lock(dir)
	if err != nil {
		t.Fatalf("lock did not release: %v", err)
	}
	release()
}

// TestRefuseStartInProgress: a data-replacing operation must
// not run between a start's preparation and its supervisor becoming visible.
func TestRefuseStartInProgress(t *testing.T) {
	held := func(v bool) func() (bool, error) { return func() (bool, error) { return v, nil } }
	up := func(v bool) func() bool { return func() bool { return v } }
	for _, tc := range []struct {
		name      string
		sup, lock bool
		refuse    bool
	}{
		{"nothing running: go ahead", false, false, false},
		{"a start between preparation and supervisor: refuse", false, true, true},
		{"a running node (lock held for the whole run, supervisor visible): go ahead", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := RefuseStartInProgress("etcd restore", "replacing its etcd data", up(tc.sup), held(tc.lock))
			if (err != nil) != tc.refuse {
				t.Fatalf("err = %v, want refusal: %v", err, tc.refuse)
			}
			if tc.refuse && !strings.Contains(err.Error(), "etcd restore: a stratum node is starting") {
				t.Errorf("refusal does not say what and why: %v", err)
			}
		})
	}
	if err := RefuseStartInProgress("x", "y", up(false), func() (bool, error) { return false, errors.New("boom") }); err == nil {
		t.Error("an unreadable start lock was treated as no start in progress")
	}
}

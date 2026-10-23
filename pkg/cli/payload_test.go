package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/version"
)

// useStageDataDir points version.DataDir and version.EtcdDataDir into a fresh
// temp dir and returns that dir. The node data dir itself is not created, the
// way uninstall leaves it, and the data lock lands beside it in the temp dir.
// The temp dir is resolved through symlinks (macOS's /var is a link to
// /private/var) so that a relative --root, made absolute against the working
// directory, spells the same path as version.DataDir.
func useStageDataDir(t *testing.T) string {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldData, oldEtcd := version.DataDir, version.EtcdDataDir
	t.Cleanup(func() { version.DataDir, version.EtcdDataDir = oldData, oldEtcd })
	version.DataDir = filepath.Join(tmp, "data")
	version.EtcdDataDir = filepath.Join(version.DataDir, "etcd")
	return tmp
}

// holdDataLock takes the etcd data lock, as uninstall does before it removes
// the data dir, and keeps it until the test ends.
func holdDataLock(t *testing.T) {
	t.Helper()
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatalf("hold the etcd data lock: %v", err)
	}
	t.Cleanup(release)
}

// tryDataLock takes the etcd data lock and releases it at once, returning the
// lock's error when it is held. flock locks belong to the open file, so the
// attempt also conflicts with a lock held elsewhere in this process.
func tryDataLock() error {
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		return err
	}
	release()
	return nil
}

// stubStagePayload replaces the payload extraction for the test and returns
// the roots it was asked to stage into. Each call runs during, when set, at
// the point the real extraction writes its files, then writes one staged
// file under the root and reports it, or returns fail instead when fail is
// set. The test binary carries no payload, so without the stub a stage that
// ignored the lock would still leave nothing on disk to catch it by.
func stubStagePayload(t *testing.T, during func(), fail error) *[]string {
	t.Helper()
	old := stagePayload
	t.Cleanup(func() { stagePayload = old })
	var roots []string
	stagePayload = func(root string) ([]string, error) {
		roots = append(roots, root)
		if during != nil {
			during()
		}
		if fail != nil {
			return nil, fail
		}
		rel := filepath.Join(assets.BinDir, "stub")
		if err := os.MkdirAll(filepath.Join(root, assets.BinDir), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte("stub"), 0o755); err != nil {
			return nil, err
		}
		return []string{rel}, nil
	}
	return &roots
}

// runStage runs `stratum stage --root root` and returns its error. The staged
// paths the command prints are captured and dropped.
func runStage(t *testing.T, root string) error {
	t.Helper()
	var err error
	captureStdout(t, func() {
		cmd := newStageCommand()
		cmd.SetArgs([]string{"--root", root})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		err = cmd.Execute()
	})
	return err
}

// TestStageIntoTheDataDirFailsWhileTheDataLockIsHeld: uninstall holds the etcd
// data lock while it deletes the node data dir, as `stratum up` and the HA join
// do while they stage into it. A `stratum stage` into that dir must take the
// same lock and, finding it held, fail with the lock's error before it
// extracts anything; otherwise it recreates part of the dir uninstall is
// removing. The flag is compared once made absolute and cleaned, so every
// spelling of the data dir counts.
func TestStageIntoTheDataDirFailsWhileTheDataLockIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fromTemp runs the command from the temp dir holding the data dir,
		// so a relative root resolves against it.
		fromTemp bool
		root     func(dataDir string) string
	}{
		{"as configured", false, func(d string) string { return d }},
		{"trailing slash", false, func(d string) string { return d + string(filepath.Separator) }},
		{"dot segments", false, func(d string) string { return d + "/./etcd/.." }},
		{"relative", true, func(d string) string { return filepath.Base(d) }},
		{"missing descendant", false, func(d string) string { return filepath.Join(d, "staging", "release") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := useStageDataDir(t)
			holdDataLock(t)
			lockErr := tryDataLock()
			if lockErr == nil {
				t.Fatal("a second etcddata.Lock took the lock the test holds: this fixture cannot show a held lock")
			}
			roots := stubStagePayload(t, nil, nil)
			if tc.fromTemp {
				t.Chdir(tmp)
			}
			root := tc.root(version.DataDir)

			err := runStage(t, root)
			if err == nil {
				t.Fatalf("stage --root %s succeeded while the data lock was held", root)
			}
			if !errors.Is(err, syscall.EWOULDBLOCK) || !strings.Contains(err.Error(), lockErr.Error()) {
				t.Fatalf("stage --root %s = %v, want the data lock's error %q", root, err, lockErr)
			}
			if len(*roots) != 0 {
				t.Errorf("stage extracted the payload into %v although the data lock was held", *roots)
			}
			if _, err := os.Lstat(version.DataDir); !errors.Is(err, fs.ErrNotExist) {
				var created []string
				_ = filepath.WalkDir(version.DataDir, func(p string, _ fs.DirEntry, _ error) error {
					created = append(created, p)
					return nil
				})
				t.Errorf("stage created %v although the data lock was held: it must fail before it writes anything", created)
			}
		})
	}
}

func TestStageThroughASymlinkIntoTheDataDirTakesTheDataLock(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "lock free"
		if held {
			name = "lock held"
		}
		t.Run(name, func(t *testing.T) {
			tmp := useStageDataDir(t)
			if err := os.MkdirAll(version.DataDir, 0o755); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(tmp, "alias")
			if err := os.Symlink("data", alias); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(alias, "staging", "release")
			if held {
				holdDataLock(t)
			}
			var lockErrDuringStage error
			roots := stubStagePayload(t, func() { lockErrDuringStage = tryDataLock() }, nil)
			err := runStage(t, root)
			if held {
				if !errors.Is(err, syscall.EWOULDBLOCK) || len(*roots) != 0 {
					t.Fatalf("stage through alias: err=%v, extractions=%v; want lock refusal before extraction", err, *roots)
				}
				if _, err := os.Stat(filepath.Join(version.DataDir, "staging")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("refused stage created its target: %v", err)
				}
			} else {
				if err != nil || len(*roots) != 1 || !errors.Is(lockErrDuringStage, syscall.EWOULDBLOCK) {
					t.Fatalf("stage through alias: err=%v, extractions=%v, lock during extraction=%v; want extraction under the lock", err, *roots, lockErrDuringStage)
				}
				if err := tryDataLock(); err != nil {
					t.Fatalf("stage did not release the data lock: %v", err)
				}
			}
		})
	}
}

func TestStagePayloadDirectoryAliasesTakeTheDataLock(t *testing.T) {
	for _, dir := range []string{assets.BinDir, assets.ImagesDir} {
		for _, held := range []bool{false, true} {
			name := dir + "/lock free"
			if held {
				name = dir + "/lock held"
			}
			t.Run(name, func(t *testing.T) {
				tmp := useStageDataDir(t)
				root := filepath.Join(tmp, "scratch")
				for _, path := range []string{root, version.DataDir} {
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(version.DataDir, filepath.Join(root, dir)); err != nil {
					t.Fatal(err)
				}
				if held {
					holdDataLock(t)
				}
				var lockErrDuringStage error
				roots := stubStagePayload(t, func() { lockErrDuringStage = tryDataLock() }, nil)
				err := runStage(t, root)
				if held {
					if !errors.Is(err, syscall.EWOULDBLOCK) || len(*roots) != 0 {
						t.Fatalf("stage with %s alias: err=%v, extractions=%v; want lock refusal before extraction", dir, err, *roots)
					}
				} else {
					if err != nil || len(*roots) != 1 || !errors.Is(lockErrDuringStage, syscall.EWOULDBLOCK) {
						t.Fatalf("stage with %s alias: err=%v, extractions=%v, lock during extraction=%v; want extraction under the lock", dir, err, *roots, lockErrDuringStage)
					}
					if err := tryDataLock(); err != nil {
						t.Fatalf("stage did not release the data lock: %v", err)
					}
				}
			})
		}
	}
}

func TestStageRefusesUnresolvableAliasesBeforeExtraction(t *testing.T) {
	for _, target := range []string{"data", "alias"} {
		t.Run(target, func(t *testing.T) {
			tmp := useStageDataDir(t)
			alias := filepath.Join(tmp, "alias")
			// data is absent, as after uninstall; alias creates a link loop.
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			holdDataLock(t)
			roots := stubStagePayload(t, nil, nil)
			if err := runStage(t, filepath.Join(alias, "staging")); err == nil || !strings.Contains(err.Error(), "resolve staging path") {
				t.Fatalf("stage through unresolved alias = %v; want refusal before extraction", err)
			}
			if len(*roots) != 0 {
				t.Fatalf("stage extracted through an unresolved alias: %v", *roots)
			}
			if _, err := os.Lstat(version.DataDir); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("refused stage recreated the data directory: %v", err)
			}
		})
	}
}

func TestStageResolvesTheDataDirectoryParentBeforeComparing(t *testing.T) {
	tmp := useStageDataDir(t)
	realParent := filepath.Join(tmp, "real")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(tmp, "alias")
	if err := os.Symlink(realParent, alias); err != nil {
		t.Fatal(err)
	}
	version.DataDir = filepath.Join(alias, "data")
	version.EtcdDataDir = filepath.Join(version.DataDir, "etcd")
	holdDataLock(t)
	roots := stubStagePayload(t, nil, nil)
	if err := runStage(t, filepath.Join(realParent, "data", "staging")); !errors.Is(err, syscall.EWOULDBLOCK) || len(*roots) != 0 {
		t.Fatalf("stage through the data directory's real parent: err=%v, extractions=%v; want lock refusal", err, *roots)
	}
}

func TestStageIntoAnUnrelatedSymlinkIgnoresTheDataLock(t *testing.T) {
	tmp := useStageDataDir(t)
	other := version.DataDir + "-ci"
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(tmp, "alias")
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	holdDataLock(t)
	roots := stubStagePayload(t, nil, nil)
	if err := runStage(t, filepath.Join(alias, "release")); err != nil || len(*roots) != 1 {
		t.Fatalf("stage through an unrelated alias: err=%v, extractions=%v; want staging without the node lock", err, *roots)
	}
	if _, err := os.Stat(filepath.Join(other, "release", assets.BinDir, "stub")); err != nil {
		t.Fatalf("payload was not staged through the unrelated alias: %v", err)
	}
}

// TestStageIntoAnotherRootIgnoresTheDataLock: only the node data dir is
// guarded. CI stages into temp dirs as an unprivileged user, who cannot create
// the lock beside /var/lib/stratum, so any other root, including one that only
// shares the data dir's path as a string prefix, stages exactly as before
// whether or not the lock is held.
func TestStageIntoAnotherRootIgnoresTheDataLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		root func(tmp string) string
	}{
		{"unrelated dir", func(tmp string) string { return filepath.Join(tmp, "scratch") }},
		{"shares the data dir's prefix", func(string) string { return version.DataDir + "-ci" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := useStageDataDir(t)
			holdDataLock(t)
			roots := stubStagePayload(t, nil, nil)
			root := tc.root(tmp)

			if err := runStage(t, root); err != nil {
				t.Fatalf("stage --root %s = %v while the data lock was held; only the node data dir %s takes the lock",
					root, err, version.DataDir)
			}
			if len(*roots) != 1 || filepath.Clean((*roots)[0]) != root {
				t.Errorf("stage extracted into %v, want exactly [%s]", *roots, root)
			}
		})
	}
}

// TestStageIntoTheDataDirHoldsTheDataLockAcrossStaging: with nobody else
// holding it, a stage into the node data dir holds the etcd data lock for the
// whole extraction, so an uninstall that starts meanwhile is refused instead of
// deleting files mid-write. It releases the lock afterwards whether staging
// succeeded or failed, so the `stratum up` or uninstall that follows is not
// locked out.
func TestStageIntoTheDataDirHoldsTheDataLockAcrossStaging(t *testing.T) {
	errStage := errors.New("stage stub: no space left on device")
	for _, tc := range []struct {
		name string
		fail error
	}{
		{"staging succeeds", nil},
		{"staging fails", errStage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useStageDataDir(t)
			var lockErrDuringStage error
			roots := stubStagePayload(t, func() { lockErrDuringStage = tryDataLock() }, tc.fail)

			err := runStage(t, version.DataDir)
			if errors.Is(err, syscall.EWOULDBLOCK) {
				t.Fatalf("stage --root %s failed on the data lock although nobody held it: %v", version.DataDir, err)
			}
			if tc.fail == nil && err != nil {
				t.Fatalf("stage --root %s = %v, want success", version.DataDir, err)
			}
			if tc.fail != nil && !errors.Is(err, tc.fail) {
				t.Fatalf("stage --root %s = %v, want the staging error %q", version.DataDir, err, tc.fail)
			}
			if len(*roots) != 1 {
				t.Fatalf("stage extracted %d times (%v), want once into %s", len(*roots), *roots, version.DataDir)
			}
			if !errors.Is(lockErrDuringStage, syscall.EWOULDBLOCK) {
				t.Errorf("the data lock was free while stage extracted into %s (lock attempt: %v): an uninstall could delete the dir mid-write",
					version.DataDir, lockErrDuringStage)
			}
			if err := tryDataLock(); err != nil {
				t.Errorf("the data lock is still held after stage returned: %v", err)
			}
		})
	}
}

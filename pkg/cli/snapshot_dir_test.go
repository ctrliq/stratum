package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

// spec.snapshots.dir applies to the scheduled loop; list and prune must
// read the same resolved directory rather than a fixed /var/backups/etcd.
// It also pins the snapshot config to a local-only one, so no test reads
// the host's snapshot.conf or reaches its S3 target. The directory is part
// of the resolved snapshot settings, so the config stub carries it too.
func useSnapshotDir(t *testing.T) string {
	t.Helper()
	dir := realTempDir(t)
	oldDir, oldCfg := snapshotDir, loadSnapshotConfig
	t.Cleanup(func() { snapshotDir, loadSnapshotConfig = oldDir, oldCfg })
	snapshotDir = func() string { return dir }
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return etcdsnap.Config{Retain: 5, Dir: dir}, nil }
	return dir
}

// realTempDir is t.TempDir with symlinks resolved: restore refuses a symlink at
// any component, and macOS keeps TMPDIR under the /var symlink.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// captureStdout runs fn with the process's stdout swapped for a pipe and
// returns what fn printed. The CLI commands under test print with fmt.Printf
// to the process's own stdout, not to the cobra writer, so a pipe swap is the
// only way to see their output; the reader goroutine keeps a full pipe from
// deadlocking the writer. Copied from internal/uninstall/containers_test.go,
// which needed it for the same reason.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		old := os.Stdout
		os.Stdout = w
		defer func() { os.Stdout = old; _ = w.Close() }()
		fn()
	}()
	return <-done
}

func TestSnapshotSaveWritesToTheConfiguredDir(t *testing.T) {
	dir := useSnapshotDir(t)
	oldSave := saveSnapshot
	t.Cleanup(func() { saveSnapshot = oldSave })
	var got etcdsnap.SaveOptions
	saveSnapshot = func(_ context.Context, _ io.Writer, o etcdsnap.SaveOptions) (string, error) {
		got = o
		return filepath.Join(o.Dir, "x.db"), nil
	}

	cmd := newSnapshotSaveCommand()
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.Dir != dir {
		t.Errorf("save used dir %q, want the configured %q", got.Dir, dir)
	}
}

func TestSnapshotListReadsTheConfiguredDir(t *testing.T) {
	dir := useSnapshotDir(t)
	name := etcdsnap.ScheduledName("cp1", "20250101-000000")
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := newSnapshotListCommand()
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), name) {
		t.Errorf("list did not show %s from the configured dir:\n%s", name, out.String())
	}
}

func TestSnapshotPruneActsOnTheConfiguredDir(t *testing.T) {
	dir := useSnapshotDir(t)
	h, err := host.Hostname()
	if err != nil {
		t.Skip(err)
	}
	oldest := etcdsnap.ScheduledName(h, "20250101-000000")
	newest := etcdsnap.ScheduledName(h, "20250102-000000")
	for _, n := range []string{oldest, newest} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := newSnapshotPruneCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"--retain", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, oldest)); !os.IsNotExist(err) {
		t.Errorf("prune left the oldest snapshot in the configured dir (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, newest)); err != nil {
		t.Errorf("prune removed the newest snapshot: %v", err)
	}
}

// A prune of the local directory that fails is a warning, not a command
// failure: a snapshot already succeeded, and the scheduled loop keeps the
// node. PruneScheduled returns its failures instead of printing them, and
// the command reports each one as a WARN line with exit status 0 — a printed
// warning stays the only report a failed local prune gives. useSnapshotDir
// also stubs loadSnapshotConfig, so the prune is local-only and no S3 target
// is pruned.
func TestSnapshotPrune_ReportsALocalFailureAsAWarning(t *testing.T) {
	useSnapshotDir(t)
	// A regular file where a directory is expected: ReadDir fails without the
	// prune deleting anything.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshotDir = func() string { return file }
	// The directory the prune acts on comes from the resolved snapshot
	// settings, so the broken dir reaches the command through
	// loadSnapshotConfig as well.
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return etcdsnap.Config{Retain: 5, Dir: file}, nil }

	var err error
	out := captureStdout(t, func() {
		cmd := newSnapshotPruneCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--retain", "1"})
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatalf("prune returned %v; a failed local prune must stay a warning, exit status 0", err)
	}
	warn, pruned := strings.Index(out, "WARN: local snapshot prune"), strings.Index(out, "pruned to newest 1 snapshot(s)")
	if warn < 0 {
		t.Errorf("stdout does not report the failed local prune as a WARN line:\n%s", out)
	}
	if pruned < 0 {
		t.Errorf("stdout does not carry the \"pruned to newest 1 snapshot(s)\" line:\n%s", out)
	}
	if warn >= 0 && pruned >= 0 && warn > pruned {
		t.Errorf("the WARN came after the summary line; the failure is reported where it happened:\n%s", out)
	}
}

// The resolved snapshot settings carry the directory
// (cluster.LoadSnapshotConfig returns it), and the CLI takes the directory from
// the settings it already loads instead of a second resolver read. Each command
// must follow the resolved dir even while the separate snapshotDir seam points
// somewhere else: a save, list or prune that read their own directory would
// disagree with the scheduled loop that owns the place.
func TestSnapshotCommands_UseTheResolvedDir(t *testing.T) {
	resolved := realTempDir(t)
	unread := filepath.Join(resolved, "unread")

	oldDir, oldCfg, oldSave := snapshotDir, loadSnapshotConfig, saveSnapshot
	t.Cleanup(func() { snapshotDir, loadSnapshotConfig, saveSnapshot = oldDir, oldCfg, oldSave })
	snapshotDir = func() string { return unread }
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return etcdsnap.Config{Retain: 5, Dir: resolved}, nil }

	t.Run("save", func(t *testing.T) {
		var got etcdsnap.SaveOptions
		saveSnapshot = func(_ context.Context, _ io.Writer, o etcdsnap.SaveOptions) (string, error) {
			got = o
			return filepath.Join(o.Dir, "x.db"), nil
		}
		cmd := newSnapshotSaveCommand()
		cmd.SetArgs(nil)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if got.Dir != resolved {
			t.Errorf("save used dir %q, want the resolved %q", got.Dir, resolved)
		}
	})

	t.Run("list", func(t *testing.T) {
		name := etcdsnap.ScheduledName("cp1", "20250101-000000")
		if err := os.WriteFile(filepath.Join(resolved, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		cmd := newSnapshotListCommand()
		cmd.SetOut(&out)
		cmd.SetArgs(nil)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), name) {
			t.Errorf("list did not show %s from the resolved dir:\n%s", name, out.String())
		}
	})

	t.Run("prune", func(t *testing.T) {
		h, err := host.Hostname()
		if err != nil {
			t.Skip(err)
		}
		oldest := etcdsnap.ScheduledName(h, "20250101-000000")
		newest := etcdsnap.ScheduledName(h, "20250102-000000")
		for _, n := range []string{oldest, newest} {
			if err := os.WriteFile(filepath.Join(resolved, n), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := newSnapshotPruneCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--retain", "1"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(resolved, oldest)); !os.IsNotExist(err) {
			t.Errorf("prune left the oldest snapshot in the resolved dir (stat err %v)", err)
		}
		if _, err := os.Stat(filepath.Join(resolved, newest)); err != nil {
			t.Errorf("prune removed the newest snapshot: %v", err)
		}
	})
}

// The resolved snapshot settings carry the directory the cluster config asks
// for, so every caller that reads the settings reads the same directory the
// scheduled loop writes. The controller's recorded config path decides,
// exactly as it already decides the retention.
func TestLoadSnapshotConfigReturnsTheResolvedDir(t *testing.T) {
	oldConf := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = oldConf })

	cfgPath := filepath.Join(t.TempDir(), "stratum.yaml")
	if err := os.WriteFile(cfgPath, []byte("spec:\n  snapshots:\n    retain: 3\n    dir: /srv/etcd-snaps\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := nodestate.SaveState(&nodestate.NodeState{ConfigPath: cfgPath}); err != nil {
		t.Fatal(err)
	}

	c, err := cluster.LoadSnapshotConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Dir != "/srv/etcd-snaps" {
		t.Errorf("resolved Dir = %q, want /srv/etcd-snaps from spec.snapshots.dir", c.Dir)
	}
}

// snapshot list prints bare names; restore must accept them.
func TestResolveSnapshotArg(t *testing.T) {
	dir := useSnapshotDir(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ arg, want string }{
		{"etcd-snapshot-cp1-20261001-214336.db", filepath.Join(dir, "etcd-snapshot-cp1-20261001-214336.db")},
		{"/var/backups/etcd/x.db", "/var/backups/etcd/x.db"},
		{"./x.db", wd + "/./x.db"},
		{"sub/x.db", wd + "/sub/x.db"},
		// a leading "-" must not reach etcdutl as a flag
		{"-x/f.db", wd + "/-x/f.db"},
		// never cleaned: ".." after a symlink names another file than Clean's
		{"/backups/link/../x.db", "/backups/link/../x.db"},
		{"link/../x.db", wd + "/link/../x.db"},
		{"..", ".."},
		{"s3://bucket/x.db", "s3://bucket/x.db"},
	} {
		got, err := resolveSnapshotArg(tc.arg)
		if err != nil || got != tc.want {
			t.Errorf("resolveSnapshotArg(%q) = %q, %v; want %q", tc.arg, got, err, tc.want)
		}
	}
}

// An s3:// restore downloads into spec.snapshots.dir, not /var/backups/etcd.
func TestSnapshotRestoreS3DownloadsToTheConfiguredDir(t *testing.T) {
	dir := filepath.Join(useSnapshotDir(t), "custom")
	snapshotDir = func() string { return dir }
	loadSnapshotConfig = func() (etcdsnap.Config, error) {
		return etcdsnap.Config{S3: objstore.Target{Bucket: "b"}}, nil
	}
	oldDL, oldRestore := s3Download, restoreLocalSeed
	t.Cleanup(func() { s3Download, restoreLocalSeed = oldDL, oldRestore })
	var downloaded, restored string
	s3Download = func(_ *objstore.Target, _ context.Context, root *os.Root, name string) error {
		downloaded = filepath.Join(root.Name(), name)
		return nil
	}
	restoreLocalSeed = func(_ io.Writer, p string) error { restored = p; return nil }

	cmd := newSnapshotRestoreCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"s3://b/x.db", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "x.db")
	if downloaded != want || restored != want {
		t.Errorf("downloaded to %q, restored %q; want both %q", downloaded, restored, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("configured dir was not created: %v", err)
	}
}

// restoreS3Into runs `snapshot restore s3://b/x.db` with the snapshot dir set
// to dir, and reports the error and whether anything was downloaded.
func restoreS3Into(t *testing.T, dir string) (downloaded bool, err error) {
	t.Helper()
	useSnapshotDir(t)
	snapshotDir = func() string { return dir }
	loadSnapshotConfig = func() (etcdsnap.Config, error) {
		return etcdsnap.Config{S3: objstore.Target{Bucket: "b"}}, nil
	}
	oldDL, oldRestore := s3Download, restoreLocalSeed
	t.Cleanup(func() { s3Download, restoreLocalSeed = oldDL, oldRestore })
	s3Download = func(*objstore.Target, context.Context, *os.Root, string) error { downloaded = true; return nil }
	restoreLocalSeed = func(io.Writer, string) error { return nil }

	cmd := newSnapshotRestoreCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"s3://b/x.db", "--yes"})
	err = cmd.Execute()
	return downloaded, err
}

// An existing dir is checked, not chmodded: chmod follows a symlink and, as
// root, would change whatever a misconfigured spec.snapshots.dir names. One
// other users can write to is refused before the download.
func TestSnapshotRestoreS3ChecksAnExistingDir(t *testing.T) {
	root := realTempDir(t)
	mk := func(name string, mode os.FileMode) string {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	real := mk("real", 0o700)
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, dir string
		wantErr   string
	}{
		{"symlink", link, "symlink"},
		{"symlink, trailing slash", link + "/", "symlink"},
		{"not a directory", file, "not a directory"},
		{"group-writable", mk("gw", 0o770), "writable by group or others"},
		{"world-writable", mk("ww", 0o777), "writable by group or others"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			downloaded, err := restoreS3Into(t, tc.dir)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if downloaded {
				t.Error("downloaded into a refused dir")
			}
		})
	}

	// A symlink above the last component must not pass the check, or MkdirAll
	// and the download would follow it.
	t.Run("symlinked parent", func(t *testing.T) {
		dir := filepath.Join(link, "etcd")
		if err := os.Mkdir(filepath.Join(real, "etcd"), 0o700); err != nil {
			t.Fatal(err)
		}
		downloaded, err := restoreS3Into(t, dir)
		if err == nil || !strings.Contains(err.Error(), "symlink") || downloaded {
			t.Fatalf("err = %v, downloaded = %v; want a symlink refusal", err, downloaded)
		}
	})

	t.Run("symlinked middle component, dir not yet created", func(t *testing.T) {
		dir := filepath.Join(link, "a", "etcd")
		downloaded, err := restoreS3Into(t, dir)
		if err == nil || !strings.Contains(err.Error(), "symlink") || downloaded {
			t.Fatalf("err = %v, downloaded = %v; want a symlink refusal", err, downloaded)
		}
		if _, err := os.Lstat(filepath.Join(real, "a")); err == nil {
			t.Error("created a directory through the symlink")
		}
	})

	t.Run("plain nested dir is created and accepted", func(t *testing.T) {
		dir := filepath.Join(root, "x", "y", "etcd")
		if downloaded, err := restoreS3Into(t, dir); err != nil || !downloaded {
			t.Fatalf("err = %v, downloaded = %v", err, downloaded)
		}
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("dir = %v, %v; want 0700", fi, err)
		}
	})

	t.Run("owned by another user", func(t *testing.T) {
		dir := mk("other", 0o755)
		old := geteuid
		t.Cleanup(func() { geteuid = old })
		geteuid = func() uint32 { return old() + 1 }
		downloaded, err := restoreS3Into(t, dir)
		if err == nil || !strings.Contains(err.Error(), "owned by uid") || downloaded {
			t.Fatalf("err = %v, downloaded = %v; want an ownership refusal", err, downloaded)
		}
	})

	t.Run("0755 is accepted and left as it is", func(t *testing.T) {
		dir := mk("rx", 0o755)
		if downloaded, err := restoreS3Into(t, dir); err != nil || !downloaded {
			t.Fatalf("err = %v, downloaded = %v", err, downloaded)
		}
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("dir = %v, %v; want it left 0755", fi, err)
		}
	})
}

// Restore only has the configured target's credentials, so a URL naming
// another bucket or prefix is refused before the dir is touched or anything is
// downloaded, instead of fetching the configured target's object of that name.
func TestSnapshotRestoreS3RefusesAnotherBucketOrPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, src, prefix string
		wantErr           []string // empty: the download proceeds
		wantKey           string
	}{
		{"matching bucket, no prefix", "s3://b/x.db", "", nil, "x.db"},
		{"matching bucket and prefix", "s3://b/snaps/x.db", "snaps/", nil, "snaps/x.db"},
		{"bare key under a prefix", "s3://b/x.db", "snaps", nil, "snaps/x.db"},
		{"other bucket", "s3://other/x.db", "", []string{`"other"`, `"b"`, "configured"}, ""},
		{"other bucket, same prefix", "s3://other/snaps/x.db", "snaps", []string{`"other"`, `"b"`}, ""},
		{"prefix on an unprefixed target", "s3://b/some/prefix/x.db", "", []string{"outside", `"x.db"`}, ""},
		{"other prefix", "s3://b/other/x.db", "snaps", []string{"outside", `"snaps/x.db"`}, ""},
		{"deeper prefix", "s3://b/snaps/old/x.db", "snaps", []string{"outside", `"snaps/x.db"`}, ""},
		{"dot segment", "s3://b/other/../x.db", "snaps", []string{"outside", `"snaps/x.db"`}, ""},
		{"dot segment inside the prefix", "s3://b/snaps/./x.db", "snaps", []string{"outside", `"snaps/x.db"`}, ""},
		{"repeated separator", "s3://b/snaps//x.db", "snaps", []string{"outside", `"snaps/x.db"`}, ""},
		{"dot segment, no prefix", "s3://b/a/../x.db", "", []string{"outside", `"x.db"`}, ""},
		{"prefix with one trailing slash", "s3://b/snaps/x.db", "snaps/", nil, "snaps/x.db"},
		{"prefix with a leading slash", "s3://b/snaps/x.db", "/snaps", []string{"outside", `"/snaps/x.db"`}, ""},
		{"prefix with two trailing slashes", "s3://b/snaps/x.db", "snaps//", []string{"outside", `"snaps//x.db"`}, ""},
		{"leading-slash prefix, literal URL", "s3://b//snaps/x.db", "/snaps", nil, "/snaps/x.db"},
		{"two-trailing-slash prefix, literal URL", "s3://b/snaps//x.db", "snaps//", nil, "snaps//x.db"},
		{"dot name", "s3://b/snaps/.", "snaps", []string{"no snapshot"}, ""},
		{"dot-dot name", "s3://b/..", "", []string{"no snapshot"}, ""},
		{"no key", "s3://b", "", []string{"no snapshot"}, ""},
		{"trailing slash", "s3://b/snaps/", "snaps", []string{"no snapshot"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(useSnapshotDir(t), "snaps")
			snapshotDir = func() string { return dir }
			loadSnapshotConfig = func() (etcdsnap.Config, error) {
				return etcdsnap.Config{S3: objstore.Target{Bucket: "b", Prefix: tc.prefix}}, nil
			}
			oldDL, oldRestore := s3Download, restoreLocalSeed
			t.Cleanup(func() { s3Download, restoreLocalSeed = oldDL, oldRestore })
			var gotKey string
			s3Download = func(tg *objstore.Target, _ context.Context, _ *os.Root, name string) error {
				gotKey = tg.Key(name)
				return nil
			}
			restored := false
			restoreLocalSeed = func(io.Writer, string) error { restored = true; return nil }

			cmd := newSnapshotRestoreCommand()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetArgs([]string{tc.src, "--yes"})
			err := cmd.Execute()
			if len(tc.wantErr) == 0 {
				if err != nil || gotKey != tc.wantKey || !restored {
					t.Fatalf("err=%v, downloaded %q, restored=%v; want %q downloaded and restored", err, gotKey, restored, tc.wantKey)
				}
				return
			}
			if err == nil {
				t.Fatal("restore succeeded; want a refusal")
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
			if gotKey != "" || restored {
				t.Errorf("downloaded %q, restored=%v; want neither", gotKey, restored)
			}
			if _, err := os.Stat(dir); err == nil {
				t.Error("snapshot dir was created before the refusal")
			}
		})
	}
}

// The dir that is checked is the dir that is written. After openSnapshotDir
// returns, swapping the dir's path for a symlink to elsewhere redirects
// nothing: the handle still names the checked directory.
func TestOpenSnapshotDirHandleSurvivesASwap(t *testing.T) {
	base := realTempDir(t)
	dir := filepath.Join(base, "snaps")
	victim := filepath.Join(base, "victim")
	for _, d := range []string{dir, victim} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root, err := openSnapshotDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	moved := filepath.Join(base, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, dir); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("x.db", []byte("snap"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, "x.db")); err != nil {
		t.Errorf("not written to the checked dir: %v", err)
	}
	if es, _ := os.ReadDir(victim); len(es) != 0 {
		t.Errorf("victim holds %d entries; the write followed the swapped symlink", len(es))
	}
}

// A restore removes the stale temp file a killed download left, and
// nothing else in the snapshot dir.
func TestSnapshotRestoreS3RemovesStaleDownloadTemps(t *testing.T) {
	dir := filepath.Join(realTempDir(t), "snaps")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for _, name := range []string{".x.db.123456", "other.db", ".x.db.bak"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := restoreS3Into(t, dir); err != nil {
		t.Fatal(err)
	}
	var got []string
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		got = append(got, e.Name())
	}
	if want := []string{".x.db.bak", "other.db"}; !slices.Equal(got, want) {
		t.Errorf("dir = %v, want %v", got, want)
	}
}

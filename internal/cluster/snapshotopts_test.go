package cluster

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

func TestScheduledSaveOptions(t *testing.T) {
	cfg := &ClusterConfig{}
	cfg.Spec.Snapshot.Retain = 2
	cfg.Spec.Snapshot.Dir = "/snaps"
	cfg.Spec.Snapshot.S3 = &S3{Bucket: "b", Prefix: "p", Region: "r", Endpoint: "e", PathStyle: true}

	// File absent: LoadConfigOr hands back the base it was given.
	absent := func(base etcdsnap.Config) (etcdsnap.Config, error) { return base, nil }
	// File present: its values replace the cluster config's.
	present := func(etcdsnap.Config) (etcdsnap.Config, error) {
		return etcdsnap.Config{Retain: 9, S3: objstore.Target{Bucket: "file"}}, nil
	}
	// File present but unreadable: LoadConfigOr returns an error, so pruning to
	// the base retention or uploading to the base S3 target could act on
	// settings the operator never configured.
	readErr := errors.New("permission denied")
	broken := func(etcdsnap.Config) (etcdsnap.Config, error) { return etcdsnap.Config{}, readErr }

	var buf strings.Builder
	o := scheduledSaveOptions(cfg, absent, &buf)
	if o.Retain != 2 || o.Dir != "/snaps" || o.S3 == nil || *o.S3 != (objstore.Target{Bucket: "b", Prefix: "p", Region: "r", Endpoint: "e", PathStyle: true}) {
		t.Errorf("absent file: config.yaml values not applied: %+v", o)
	}
	o = scheduledSaveOptions(cfg, present, &buf)
	if o.Retain != 9 || o.S3 == nil || o.S3.Bucket != "file" || o.Dir != "/snaps" {
		t.Errorf("present file should win: %+v", o)
	}
	if buf.Len() != 0 {
		t.Errorf("readable and missing files must not warn: %q", buf.String())
	}
	buf.Reset()
	o = scheduledSaveOptions(cfg, broken, &buf)
	// The snapshot is still taken (Dir stands) but nothing may be pruned
	// (Retain 0 keeps all) and nothing may be uploaded (no S3 target).
	if o.Retain != 0 || o.S3 != nil || o.Dir != "/snaps" {
		t.Errorf("unreadable file: %+v, want keep-all, no S3, unchanged dir", o)
	}
	if !strings.Contains(buf.String(), "WARN") || !strings.Contains(buf.String(), readErr.Error()) || !strings.Contains(buf.String(), "pruning skipped") {
		t.Errorf("unreadable file: warning does not name the read error and the skipped pruning: %q", buf.String())
	}
}

func TestLoadConfigS3PathStyle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	y := "spec:\n  snapshots:\n    s3:\n      bucket: b\n      endpoint: http://minio.lan:9000\n      pathStyle: true\n"
	if err := os.WriteFile(path, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if s3 := cfg.Spec.Snapshot.S3; s3 == nil || !s3.PathStyle {
		t.Errorf("spec.snapshots.s3.pathStyle not parsed: %+v", s3)
	}
}

// A manual `snapshot save`, list, prune, restore s3:// and the MCP snapshot
// tools all resolve through LoadSnapshotConfig, so a config.yaml-only node
// agrees with its scheduled loop instead of reverting to retain 5, no S3.
func TestLoadSnapshotConfigUsesClusterConfigWhenFileAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	y := "spec:\n  snapshots:\n    retain: 10\n    s3:\n      bucket: b\n      pathStyle: true\n"
	if err := os.WriteFile(path, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadSnapshotConfig(path, false, absentConf, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Retain != 10 {
		t.Errorf("Retain = %d, want 10 (a manual save would prune the loop's snapshots to the default)", c.Retain)
	}
	if got := c.S3Target(); got == nil || got.Bucket != "b" || !got.PathStyle {
		t.Errorf("S3 target = %+v, want bucket b with path style", got)
	}
}

func TestLoadSnapshotConfigBadClusterConfigFallsBackToDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("spec: [not a mapping\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadSnapshotConfig(path, false, absentConf, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := etcdsnap.DefaultConfig(); c.Retain != want.Retain || c.S3Target() != nil {
		t.Errorf("got %+v, want defaults", c)
	}
}

// absentConf stands in for etcdsnap.LoadConfigOr with no snapshot.conf: the
// base comes back untouched and the file did not supply it.
func absentConf(base etcdsnap.Config) (etcdsnap.Config, bool, error) { return base, false, nil }

// presentConf stands in for LoadConfigOr with a snapshot.conf on disk.
func presentConf(etcdsnap.Config) (etcdsnap.Config, bool, error) {
	return etcdsnap.Config{Retain: 9}, true, nil
}

func TestLoadSnapshotConfigWarnsOnlyWhenBrokenClusterConfigForcesDefaults(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("spec: [not a mapping\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good.yaml")
	if err := os.WriteFile(good, []byte("spec:\n  snapshots:\n    retain: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path string
		load func(etcdsnap.Config) (etcdsnap.Config, bool, error)
		warn bool
	}{
		{"unreadable config, defaults apply", broken, absentConf, true},
		{"unreadable config, snapshot.conf supplies the settings", broken, presentConf, false},
		{"absent config.yaml", filepath.Join(dir, "missing.yaml"), absentConf, false},
		{"good config", good, absentConf, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			if _, err := loadSnapshotConfig(tc.path, false, tc.load, &buf); err != nil {
				t.Fatal(err)
			}
			if got := buf.Len() > 0; got != tc.warn {
				t.Errorf("warned = %v (%q), want %v", got, buf.String(), tc.warn)
			}
			if tc.warn && !strings.Contains(buf.String(), "broken.yaml") {
				t.Errorf("warning does not name the config path: %q", buf.String())
			}
		})
	}
}

// `stratum up --config /srv/stratum.yaml` is what the scheduled loop reads, so
// the CLI and MCP must read it too, not /etc/stratum/config.yaml.
func TestSnapshotConfigFollowsTheControllersNonDefaultConfigPath(t *testing.T) {
	redirectNodeStateAndLock(t)
	dir := t.TempDir()
	custom := filepath.Join(dir, "stratum.yaml")
	if err := os.WriteFile(custom, []byte("spec:\n  snapshots:\n    retain: 7\n    s3:\n      bucket: custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// What RunController records from the config `stratum up --config` loaded.
	cfg, err := LoadConfig(custom)
	if err != nil {
		t.Fatal(err)
	}
	st := &NodeState{}
	var saved *NodeState
	if err := recordConfigPath(cfg, st, func(s *NodeState) error { saved = s; return nil }); err != nil {
		t.Fatal(err)
	}
	if saved == nil || saved.ConfigPath != custom {
		t.Fatalf("config path not recorded: %+v", saved)
	}

	path, recorded := activeConfigPath(func() (*NodeState, error) { return saved, nil }, io.Discard)
	if path != custom || !recorded {
		t.Fatalf("activeConfigPath = %q, %v, want %q, true", path, recorded, custom)
	}
	c, err := loadSnapshotConfig(path, recorded, absentConf, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.S3Target(); c.Retain != 7 || got == nil || got.Bucket != "custom" {
		t.Errorf("resolved %+v, want retain 7 and bucket custom from the custom path", c)
	}
}

// A node with no state yet, or state with no recorded path, reads the default
// path quietly. Any other state error (unreadable, corrupt) also falls back to
// the default path, but warns: on a node started with a custom --config that
// would otherwise silently apply the default path's retention.
func TestActiveConfigPathFallsBackToDefault(t *testing.T) {
	wrappedNotExist := fmt.Errorf("read node state: %w", fs.ErrNotExist)
	for _, tc := range []struct {
		name string
		load func() (*NodeState, error)
		warn bool
	}{
		{"no node state", func() (*NodeState, error) { return nil, os.ErrNotExist }, false},
		{"no node state, wrapped", func() (*NodeState, error) { return nil, wrappedNotExist }, false},
		{"state without a config path", func() (*NodeState, error) { return &NodeState{}, nil }, false},
		{"unreadable node state", func() (*NodeState, error) { return nil, fs.ErrPermission }, true},
		{"corrupt node state", func() (*NodeState, error) {
			return nil, errors.New("node.yaml: yaml: line 3: did not find expected key")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			got, recorded := activeConfigPath(tc.load, &buf)
			if got != DefaultConfigPath || recorded {
				t.Errorf("activeConfigPath = %q, %v, want %q, false", got, recorded, DefaultConfigPath)
			}
			if warned := buf.Len() > 0; warned != tc.warn {
				t.Errorf("warned = %v (%q), want %v", warned, buf.String(), tc.warn)
			}
			if tc.warn && !strings.Contains(buf.String(), DefaultConfigPath) {
				t.Errorf("warning does not name the path it falls back to: %q", buf.String())
			}
		})
	}
}

func TestRecordConfigPath(t *testing.T) {
	redirectNodeStateAndLock(t)
	boom := errors.New("disk full")
	for _, tc := range []struct {
		name      string
		cfgPath   string
		have      string
		wantSaves int
		wantPath  string // ConfigPath after the call
		saveErr   error
	}{
		{"new path is saved", "/srv/a.yaml", "", 1, "/srv/a.yaml", nil},
		{"changed path is saved", "/srv/a.yaml", "/srv/b.yaml", 1, "/srv/a.yaml", nil},
		{"same path is not rewritten", "/srv/a.yaml", "/srv/a.yaml", 0, "/srv/a.yaml", nil},
		{"config not loaded from a file", "", "/srv/b.yaml", 0, "/srv/b.yaml", nil},
		{"default path is recorded as empty: nothing to save", DefaultConfigPath, "", 0, "", nil},
		{"default path clears a stale custom path", DefaultConfigPath, "/srv/b.yaml", 1, "", nil},
		{"save error is returned", "/srv/a.yaml", "", 1, "/srv/a.yaml", boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saves := 0
			st := &NodeState{ConfigPath: tc.have}
			err := recordConfigPath(&ClusterConfig{path: tc.cfgPath}, st,
				func(*NodeState) error { saves++; return tc.saveErr })
			if saves != tc.wantSaves || !errors.Is(err, tc.saveErr) || st.ConfigPath != tc.wantPath {
				t.Errorf("saves = %d (want %d), err = %v (want %v), ConfigPath = %q (want %q)",
					saves, tc.wantSaves, err, tc.saveErr, st.ConfigPath, tc.wantPath)
			}
		})
	}
}

// Deleting the config the controller recorded must not silently select the
// defaults (retain 5, no S3) while the running controller keeps its loaded
// settings: that is warned about, like an unreadable config.yaml. Quiet when
// snapshot.conf supplies the settings, and for the ordinary no-config node.
func TestLoadSnapshotConfigRecordedConfigDeleted(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "stratum.yaml")
	if err := os.WriteFile(custom, []byte("spec:\n  snapshots:\n    retain: 7\n    s3:\n      bucket: custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recordedPath := func() (string, bool) {
		return activeConfigPath(func() (*NodeState, error) { return &NodeState{ConfigPath: custom}, nil }, io.Discard)
	}

	// Present: applies, no warning.
	var buf strings.Builder
	path, recorded := recordedPath()
	if c, err := loadSnapshotConfig(path, recorded, absentConf, &buf); err != nil || c.Retain != 7 || buf.Len() != 0 {
		t.Fatalf("present: retain %d, err %v, warning %q", c.Retain, err, buf.String())
	}

	if err := os.Remove(custom); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		path       string
		mustExist  bool
		load       func(etcdsnap.Config) (etcdsnap.Config, bool, error)
		wantWarn   bool
		wantRetain int
	}{
		{"recorded path deleted, no snapshot.conf: warns and uses defaults", custom, true, absentConf, true, 5},
		{"recorded path deleted, snapshot.conf supplies the settings: quiet", custom, true, presentConf, false, 9},
		{"no recorded path and no default config: quiet", filepath.Join(dir, "none.yaml"), false, absentConf, false, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			c, err := loadSnapshotConfig(tc.path, tc.mustExist, tc.load, &buf)
			if err != nil {
				t.Fatal(err)
			}
			if c.Retain != tc.wantRetain || c.S3Target() != nil {
				t.Errorf("resolved %+v, want retain %d and no S3 (not the default path, not a stale value)", c, tc.wantRetain)
			}
			if got := buf.Len() > 0; got != tc.wantWarn {
				t.Errorf("warned = %v (%q), want %v", got, buf.String(), tc.wantWarn)
			}
			if tc.wantWarn && !strings.Contains(buf.String(), custom) {
				t.Errorf("warning does not name the missing path %s: %q", custom, buf.String())
			}
		})
	}
}

// The real LoadState reports a missing node.yaml as fs.ErrNotExist, so the
// quiet case is what an uninitialised node actually hits, not just the stub.
func TestActiveConfigPathWithRealNodeState(t *testing.T) {
	old := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = old })

	var buf strings.Builder
	if got, recorded := activeConfigPath(LoadState, &buf); got != DefaultConfigPath || recorded || buf.Len() != 0 {
		t.Fatalf("missing node.yaml: %q, %v, warning %q; want the default path, quietly", got, recorded, buf.String())
	}

	if err := os.WriteFile(StatePath(), []byte("role: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if got, recorded := activeConfigPath(LoadState, &buf); got != DefaultConfigPath || recorded || buf.Len() == 0 {
		t.Fatalf("corrupt node.yaml: %q, %v, warning %q; want the default path with a warning", got, recorded, buf.String())
	}
}

// The CLI's list/prune and the scheduled loop must name the same directory.
func TestSnapshotDirIsSharedWithTheScheduledLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("spec:\n  snapshots:\n    dir: /srv/etcd-snaps\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := snapshotDirFrom(path); got != "/srv/etcd-snaps" {
		t.Errorf("snapshotDirFrom = %q, want /srv/etcd-snaps", got)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := scheduledSaveOptions(cfg, func(b etcdsnap.Config) (etcdsnap.Config, error) { return b, nil }, io.Discard).Dir; got != snapshotDirFrom(path) {
		t.Errorf("loop dir %q != list/prune dir %q", got, snapshotDirFrom(path))
	}

	// No dir set, no file, or an unreadable file: the default.
	for name, p := range map[string]string{"unset": writeCfg(t, "spec:\n  snapshots:\n    retain: 3\n"), "missing": filepath.Join(t.TempDir(), "none.yaml"), "broken": writeCfg(t, "spec: [not a mapping\n")} {
		if got := snapshotDirFrom(p); got != "/var/backups/etcd" {
			t.Errorf("%s: dir = %q, want %q", name, got, "/var/backups/etcd")
		}
	}
}

// A relative spec.snapshots.dir names a different place per process, and "-x"
// would reach etcdutl as a flag, so it is ignored.
func TestSnapshotDirIgnoresARelativeDir(t *testing.T) {
	for _, d := range []string{"-backups", "backups", "./backups", "../backups"} {
		p := writeCfg(t, "spec:\n  snapshots:\n    dir: "+d+"\n")
		if got := snapshotDirFrom(p); got != "/var/backups/etcd" {
			t.Errorf("dir %q: snapshotDirFrom = %q, want %q", d, got, "/var/backups/etcd")
		}
		cfg, err := LoadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.SnapshotDir(); got != "/var/backups/etcd" {
			t.Errorf("dir %q: SnapshotDir = %q, want %q (the loop must agree with list/prune)", d, got, "/var/backups/etcd")
		}
	}
}

// ".." is resolved after symlinks by the kernel (ReadDir) and before them by
// filepath.Clean (Join), so such a dir could name two places. The
// other unclean forms mean the same to both, so they are cleaned, not dropped:
// an existing node must keep finding its snapshots.
func TestSnapshotDirHandlesAnUncleanDir(t *testing.T) {
	for d, want := range map[string]string{
		"/srv/snaps/../x":  "/var/backups/etcd",
		"/srv/snaps/..":    "/var/backups/etcd",
		"/srv/snaps/":      "/srv/snaps",
		"//srv//snaps":     "/srv/snaps",
		"/srv/./snaps":     "/srv/snaps",
		"/srv/..snaps/x..": "/srv/..snaps/x..", // not a ".." element
	} {
		p := writeCfg(t, "spec:\n  snapshots:\n    dir: "+d+"\n")
		cfg, err := LoadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.SnapshotDir(); got != want {
			t.Errorf("dir %q: SnapshotDir = %q, want %q", d, got, want)
		}
		if got := snapshotDirFrom(p); got != want {
			t.Errorf("dir %q: snapshotDirFrom = %q, want %q", d, got, want)
		}
	}
}

func writeCfg(t *testing.T, y string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// State mutations re-read node.yaml while holding the real data lock.
func redirectNodeStateAndLock(t *testing.T) {
	t.Helper()
	data, conf, run := version.DataDir, version.ConfDir, version.RunDir
	t.Cleanup(func() { version.SetRoots(data, conf, run) })
	root := t.TempDir()
	version.SetRoots(filepath.Join(root, "data"), filepath.Join(root, "conf"), filepath.Join(root, "run"))
}

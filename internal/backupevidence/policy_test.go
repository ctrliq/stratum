package backupevidence

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

// The real seam reads a fixed host path a test must not touch, so every test pins it.
func noSnapshotConf(t *testing.T) {
	t.Helper()
	old := loadSnapshotConf
	t.Cleanup(func() { loadSnapshotConf = old })
	loadSnapshotConf = func(_ context.Context, base etcdsnap.Config) (etcdsnap.Config, bool, error) { return base, false, nil }
}

func snapshotConfSupplies(t *testing.T, c etcdsnap.Config) {
	t.Helper()
	old := loadSnapshotConf
	t.Cleanup(func() { loadSnapshotConf = old })
	loadSnapshotConf = func(context.Context, etcdsnap.Config) (etcdsnap.Config, bool, error) { return c, true, nil }
}

func unreadableSnapshotConf(t *testing.T) {
	t.Helper()
	old := loadSnapshotConf
	t.Cleanup(func() { loadSnapshotConf = old })
	loadSnapshotConf = func(context.Context, etcdsnap.Config) (etcdsnap.Config, bool, error) {
		return etcdsnap.DefaultConfig(), false, errors.New("read /etc/stratum/snapshot.conf: is a directory")
	}
}

func recordedConfig(t *testing.T, y string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stratum.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	old := loadNodeState
	t.Cleanup(func() { loadNodeState = old })
	loadNodeState = func(context.Context) (*nodestate.NodeState, error) {
		return &nodestate.NodeState{ConfigPath: p}, nil
	}
	return p
}

func TestPolicyHonorsTheControllersCustomConfigAndSnapshotDir(t *testing.T) {
	oldConf := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = oldConf })

	custom := filepath.Join(t.TempDir(), "stratum.yaml")
	y := "spec:\n  snapshots:\n    retain: 3\n    intervalHours: 24\n    dir: /srv/custom-snaps\n"
	if err := os.WriteFile(custom, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := nodestate.SaveState(&nodestate.NodeState{ConfigPath: custom}); err != nil {
		t.Fatal(err)
	}
	noSnapshotConf(t)

	p := Policy()
	if p.State != StateConfigured {
		t.Errorf("State = %q, want %q", p.State, StateConfigured)
	}
	if p.Retain != 3 || p.RetainFrom != FromClusterConfig {
		t.Errorf("Retain = %d (%s), want 3 from the cluster config", p.Retain, p.RetainFrom)
	}
	if p.Interval != 24*time.Hour || p.IntervalFrom != FromClusterConfig {
		t.Errorf("Interval = %v (%s), want 24h from the cluster config", p.Interval, p.IntervalFrom)
	}
	if p.Dir != "/srv/custom-snaps" || p.DirFrom != FromClusterConfig {
		t.Errorf("Dir = %q (%s), want /srv/custom-snaps from spec.snapshots.dir", p.Dir, p.DirFrom)
	}
	if p.Copy != CopyLocalOnly || p.CopyFrom != FromDefault {
		t.Errorf("Copy = %q (%s), want local-only (no s3 configured)", p.Copy, p.CopyFrom)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("a healthy resolution warned: %q", p.Warnings)
	}
}

func TestPolicyIsLocalOnlyWithoutARemoteTarget(t *testing.T) {
	recordedConfig(t, "spec:\n  snapshots:\n    retain: 3\n")
	noSnapshotConf(t)

	p := Policy()
	if p.Copy != CopyLocalOnly || p.CopyFrom != FromDefault {
		t.Errorf("Copy = %q (%s), want local-only with default provenance (no target is configured)", p.Copy, p.CopyFrom)
	}
	if p.State != StateConfigured {
		t.Errorf("State = %q, want %q", p.State, StateConfigured)
	}
}

func TestPolicyRequiresRemoteCopiesFromTheEffectiveTarget(t *testing.T) {
	t.Run("cluster config target", func(t *testing.T) {
		recordedConfig(t, "spec:\n  snapshots:\n    s3:\n      bucket: offload\n")
		noSnapshotConf(t)
		p := Policy()
		if p.Copy != CopyRemoteRequired || p.CopyFrom != FromClusterConfig {
			t.Errorf("Copy = %q (%s), want remote-required from the cluster config", p.Copy, p.CopyFrom)
		}
	})
	t.Run("snapshot.conf adds a target and its retention", func(t *testing.T) {
		recordedConfig(t, "spec:\n  snapshots:\n    retain: 3\n")
		snapshotConfSupplies(t, etcdsnap.Config{Retain: 9, S3: objstore.Target{Bucket: "offload"}})
		p := Policy()
		if p.Copy != CopyRemoteRequired || p.CopyFrom != FromSnapshotConf {
			t.Errorf("Copy = %q (%s), want remote-required from snapshot.conf", p.Copy, p.CopyFrom)
		}
		if p.Retain != 9 || p.RetainFrom != FromSnapshotConf {
			t.Errorf("Retain = %d (%s), want 9 from snapshot.conf, not the cluster config's 3", p.Retain, p.RetainFrom)
		}
	})
	t.Run("a local-only snapshot.conf removes the cluster config's target", func(t *testing.T) {
		recordedConfig(t, "spec:\n  snapshots:\n    s3:\n      bucket: offload\n")
		snapshotConfSupplies(t, etcdsnap.Config{Retain: 5})
		p := Policy()
		if p.Copy != CopyLocalOnly || p.CopyFrom != FromSnapshotConf {
			t.Errorf("Copy = %q (%s), want local-only from snapshot.conf", p.Copy, p.CopyFrom)
		}
	})
}

func TestPolicyIsDisabledWhenSnapshotsAreDisabled(t *testing.T) {
	recordedConfig(t, "spec:\n  snapshots:\n    disabled: true\n    dir: /srv/snaps\n")
	noSnapshotConf(t)

	p := Policy()
	if p.State != StateDisabled {
		t.Errorf("State = %q, want %q", p.State, StateDisabled)
	}
	if p.Dir != "/srv/snaps" || p.DirFrom != FromClusterConfig {
		t.Errorf("Dir = %q (%s), want the configured dir reported alongside the disabled state", p.Dir, p.DirFrom)
	}
	if p.Interval != version.SnapshotIntervalHours*time.Hour || p.IntervalFrom != FromDefault {
		t.Errorf("Interval = %v (%s), want the built-in default interval", p.Interval, p.IntervalFrom)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("a disabled-but-readable config warned: %q", p.Warnings)
	}
}

func TestPolicyUnconfiguredRunsOnTheBuiltInDefaults(t *testing.T) {
	recordedConfig(t, "spec: {}\n")
	noSnapshotConf(t)

	p := Policy()
	if p.State != StateUnconfigured {
		t.Errorf("State = %q, want %q", p.State, StateUnconfigured)
	}
	if p.Interval != version.SnapshotIntervalHours*time.Hour || p.IntervalFrom != FromDefault {
		t.Errorf("Interval = %v (%s), want the default 12h", p.Interval, p.IntervalFrom)
	}
	if p.Retain != version.SnapshotRetainDefault || p.RetainFrom != FromDefault {
		t.Errorf("Retain = %d (%s), want the default %d", p.Retain, p.RetainFrom, version.SnapshotRetainDefault)
	}
	if p.Copy != CopyLocalOnly || p.CopyFrom != FromDefault {
		t.Errorf("Copy = %q (%s), want local-only by default", p.Copy, p.CopyFrom)
	}
	if p.Dir != "/var/backups/etcd" || p.DirFrom != FromDefault {
		t.Errorf("Dir = %q (%s), want the built-in default snapshot directory", p.Dir, p.DirFrom)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("an unconfigured node warned: %q", p.Warnings)
	}
}

func TestPolicyIsUnknownWhenTheClusterConfigCannotBeRead(t *testing.T) {
	t.Run("corrupt config", func(t *testing.T) {
		recordedConfig(t, "spec: [not a mapping\n")
		noSnapshotConf(t)
		p := Policy()
		if p.State != StateUnknown {
			t.Errorf("State = %q, want %q", p.State, StateUnknown)
		}
		if p.Interval != 0 || p.IntervalFrom != FromUnknown {
			t.Errorf("Interval = %v (%s), want unknown, not the default 12h", p.Interval, p.IntervalFrom)
		}
		if p.Retain != 0 || p.RetainFrom != FromUnknown {
			t.Errorf("Retain = %d (%s), want unknown, not the default %d", p.Retain, p.RetainFrom, version.SnapshotRetainDefault)
		}
		if p.Dir != "" || p.DirFrom != FromUnknown {
			t.Errorf("Dir = %q (%s), want unknown, not the default directory", p.Dir, p.DirFrom)
		}
		if p.Copy != "" || p.CopyFrom != FromUnknown {
			t.Errorf("Copy = %q (%s), want unknown, not local-only", p.Copy, p.CopyFrom)
		}
		if w := strings.Join(p.Warnings, "\n"); !strings.Contains(w, "cluster config") {
			t.Errorf("warnings do not name the unreadable cluster config: %q", w)
		}
	})
	t.Run("recorded config deleted", func(t *testing.T) {
		path := recordedConfig(t, "spec:\n  snapshots:\n    retain: 3\n")
		noSnapshotConf(t)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		p := Policy()
		if p.State != StateUnknown {
			t.Errorf("State = %q, want %q: a deleted recorded config is not the ordinary no-config case", p.State, StateUnknown)
		}
		if p.Retain != 0 || p.RetainFrom != FromUnknown {
			t.Errorf("Retain = %d (%s), want unknown, not the deleted config's 3 or the default", p.Retain, p.RetainFrom)
		}
	})
	t.Run("snapshot.conf still supplies retention and the copy target", func(t *testing.T) {
		recordedConfig(t, "spec: [not a mapping\n")
		snapshotConfSupplies(t, etcdsnap.Config{Retain: 9, S3: objstore.Target{Bucket: "offload"}})
		p := Policy()
		if p.State != StateUnknown {
			t.Errorf("State = %q, want %q: the schedule is still unknown", p.State, StateUnknown)
		}
		if p.Retain != 9 || p.RetainFrom != FromSnapshotConf {
			t.Errorf("Retain = %d (%s), want 9 from snapshot.conf", p.Retain, p.RetainFrom)
		}
		if p.Copy != CopyRemoteRequired || p.CopyFrom != FromSnapshotConf {
			t.Errorf("Copy = %q (%s), want remote-required from snapshot.conf", p.Copy, p.CopyFrom)
		}
		if p.IntervalFrom != FromUnknown {
			t.Errorf("IntervalFrom = %q, want unknown: no interval comes from the conf", p.IntervalFrom)
		}
	})
	t.Run("unreadable node state", func(t *testing.T) {
		old := loadNodeState
		t.Cleanup(func() { loadNodeState = old })
		loadNodeState = func(context.Context) (*nodestate.NodeState, error) { return nil, fs.ErrPermission }
		noSnapshotConf(t)
		p := Policy()
		if p.State != StateUnknown {
			t.Errorf("State = %q, want %q: the config the controller was started with cannot be determined", p.State, StateUnknown)
		}
		if w := strings.Join(p.Warnings, "\n"); !strings.Contains(w, "node state") {
			t.Errorf("warnings do not name the unreadable node state: %q", w)
		}
	})
}

func TestPolicyUnreadableConfIsAWarnAndNeverInventsThresholds(t *testing.T) {
	recordedConfig(t, "spec:\n  snapshots:\n    retain: 3\n    intervalHours: 24\n    dir: /srv/snaps\n    s3:\n      bucket: offload\n")
	unreadableSnapshotConf(t)

	p := Policy()
	if p.State != StateUnreadable {
		t.Errorf("State = %q, want %q", p.State, StateUnreadable)
	}
	if p.Retain != 0 || p.RetainFrom != FromUnknown {
		t.Errorf("Retain = %d (%s), want unknown, not 3, not the default %d", p.Retain, p.RetainFrom, version.SnapshotRetainDefault)
	}
	if p.Copy != "" || p.CopyFrom != FromUnknown {
		t.Errorf("Copy = %q (%s), want unknown: the loop uploads nothing, but the conf may carry a target", p.Copy, p.CopyFrom)
	}
	if p.Interval != 24*time.Hour || p.IntervalFrom != FromClusterConfig {
		t.Errorf("Interval = %v (%s), want 24h: the schedule never comes from the conf", p.Interval, p.IntervalFrom)
	}
	if p.Dir != "/srv/snaps" || p.DirFrom != FromClusterConfig {
		t.Errorf("Dir = %q (%s), want the configured dir: the directory never comes from the conf", p.Dir, p.DirFrom)
	}
	w := strings.Join(p.Warnings, "\n")
	if !strings.Contains(w, "WARN") || !strings.Contains(w, "snapshot config") || !strings.Contains(w, "keeps every snapshot") {
		t.Errorf("warnings do not carry the WARN about the unreadable conf and what the loop does anyway: %q", w)
	}
}

func TestUnreadableNodeStateDoesNotInventFallbackPolicy(t *testing.T) {
	oldNode, oldConfig := loadNodeState, loadClusterConfig
	t.Cleanup(func() { loadNodeState, loadClusterConfig = oldNode, oldConfig })
	loadNodeState = func(context.Context) (*nodestate.NodeState, error) { return nil, fs.ErrPermission }
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	loadClusterConfig = func(context.Context, string) (*cluster.ClusterConfig, error) { return cluster.LoadConfig(missing) }
	noSnapshotConf(t)
	p := Policy()
	if p.IntervalFrom != FromUnknown || p.RetainFrom != FromUnknown || p.CopyFrom != FromUnknown || p.DirFrom != FromUnknown {
		t.Fatalf("unknown original config path resolved invented fallback policy: %+v", p)
	}
}

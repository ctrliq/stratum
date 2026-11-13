package backupevidence

import (
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

// noSnapshotConf pins the conf seam to the no-file case: the cluster-config
// base comes back untouched and the file supplied nothing. Every test pins
// it, because the real seam reads the fixed /etc/stratum path, which a test
// must not touch.
func noSnapshotConf(t *testing.T) {
	t.Helper()
	old := loadSnapshotConf
	t.Cleanup(func() { loadSnapshotConf = old })
	loadSnapshotConf = func(base etcdsnap.Config) (etcdsnap.Config, bool, error) { return base, false, nil }
}

// snapshotConfSupplies pins the conf seam to a file that exists and wins
// outright, the way etcdsnap.LoadConfigOr resolves one.
func snapshotConfSupplies(t *testing.T, c etcdsnap.Config) {
	t.Helper()
	old := loadSnapshotConf
	t.Cleanup(func() { loadSnapshotConf = old })
	loadSnapshotConf = func(etcdsnap.Config) (etcdsnap.Config, bool, error) { return c, true, nil }
}

// unreadableSnapshotConf pins the conf seam to an existing file whose read
// fails: the built-in defaults come back beside the error, exactly what
// etcdsnap.LoadConfigOr returns for one.
func unreadableSnapshotConf(t *testing.T) {
	t.Helper()
	old := loadSnapshotConf
	t.Cleanup(func() { loadSnapshotConf = old })
	loadSnapshotConf = func(etcdsnap.Config) (etcdsnap.Config, bool, error) {
		return etcdsnap.DefaultConfig(), false, errors.New("read /etc/stratum/snapshot.conf: is a directory")
	}
}

// recordedConfig writes a cluster config and pins the node-state seam to a
// controller started with it — what `stratum up --config` records via the
// node state. It returns the config's path.
func recordedConfig(t *testing.T, y string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stratum.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	old := loadNodeState
	t.Cleanup(func() { loadNodeState = old })
	loadNodeState = func() (*nodestate.NodeState, error) {
		return &nodestate.NodeState{ConfigPath: p}, nil
	}
	return p
}

// The controller's recorded config file (`stratum up --config`) decides the
// policy, and spec.snapshots.dir decides the directory — the same directory
// the scheduled loop writes to. This test runs the real node-state read and
// the real cluster config parse, so the path resolution is exercised end to
// end, not through a stub.
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

// A node whose config sets retention but no S3 target keeps its snapshots
// local-only.
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

// The copy requirement comes from the effective S3 target: a bucket in the
// cluster config makes every scheduled snapshot require a remote copy, and
// an existing snapshot.conf wins outright in both directions — it can add a
// target the cluster config lacks and remove one it has.
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

// spec.snapshots.disabled means the controller never starts the snapshot
// loop. The rest of the policy still reports what is configured beside it.
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

// Nothing configured anywhere: the loop still runs, on the built-in
// defaults, and every value says where it came from.
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

// A cluster config that cannot be read — corrupt, or one the controller
// recorded and someone deleted — leaves the schedule unknown: the
// controller keeps running with the config it was started with, and no
// default threshold is invented for what cannot be read. A readable
// snapshot.conf still supplies retention and the copy target, because it
// wins outright over the cluster config.
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
		loadNodeState = func() (*nodestate.NodeState, error) { return nil, fs.ErrPermission }
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

// An unreadable snapshot.conf is a WARN, not an invented policy: the loop
// still takes snapshots but prunes nothing and uploads nothing, so no
// retention threshold and no copy requirement are reported — not the
// built-in defaults, not the cluster config's values. What only the cluster
// config carries (schedule, directory, disabled state) stays resolved,
// because the directory never comes from the conf.
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
	loadNodeState = func() (*nodestate.NodeState, error) { return nil, fs.ErrPermission }
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	loadClusterConfig = func(string) (*cluster.ClusterConfig, error) { return cluster.LoadConfig(missing) }
	noSnapshotConf(t)
	p := Policy()
	if p.IntervalFrom != FromUnknown || p.RetainFrom != FromUnknown || p.CopyFrom != FromUnknown || p.DirFrom != FromUnknown {
		t.Fatalf("unknown original config path resolved invented fallback policy: %+v", p)
	}
}

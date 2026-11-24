// Package backupevidence exposes the engine's resolved backup evidence to external consumers.
package backupevidence

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// Provenance names the source a resolved policy value came from.
type Provenance string

const (
	FromClusterConfig Provenance = "cluster-config"
	FromSnapshotConf  Provenance = "snapshot-conf"
	FromDefault       Provenance = "default"
	FromUnknown       Provenance = "unknown"
)

// State summarizes what a consumer can rely on in the resolved policy.
type State string

const (
	StateConfigured   State = "configured"
	StateDisabled     State = "disabled"
	StateUnconfigured State = "unconfigured"
	StateUnknown      State = "unknown"
	StateUnreadable   State = "unreadable"
)

type CopyRequirement string

const (
	CopyLocalOnly      CopyRequirement = "local-only"
	CopyRemoteRequired CopyRequirement = "remote-required"
)

// EffectivePolicy is a re-resolution of the config the controller read at start.
type EffectivePolicy struct {
	State        State
	Interval     time.Duration
	IntervalFrom Provenance
	// Zero with unknown provenance means unknown, not keep-all.
	Retain     int
	RetainFrom Provenance
	// Empty with unknown provenance means undetermined, not no target.
	Copy     CopyRequirement
	CopyFrom Provenance
	Dir      string
	DirFrom  Provenance
	// Conditions only; no raw errors.
	Warnings []string
}

// Must match the resolver's own default, which actually resolves the directory.
const defaultSnapshotDir = "/var/backups/etcd"

var (
	loadNodeState     = nodestate.LoadStateContext
	loadClusterConfig = cluster.LoadConfigContext
	loadSnapshotConf  = func(ctx context.Context, base etcdsnap.Config) (etcdsnap.Config, bool, error) {
		c, err := etcdsnap.LoadConfigOrContext(ctx, base)
		return c, c.FromFile(), err
	}
)

// Policy resolves the effective backup policy the way the scheduled snapshot loop does each tick.
func Policy() EffectivePolicy { return policy(context.Background()) }

func policy(ctx context.Context) EffectivePolicy {
	var p EffectivePolicy
	var warns []string

	path, recorded, stateKnown := activePath(ctx, &warns)
	cc, ccErr := loadActiveClusterConfig(ctx, path, recorded)
	if ccErr != nil {
		warns = append(warns, fmt.Sprintf("WARN: cannot read the cluster config the controller was started with (%s); the schedule, the snapshot directory and the disabled state are unknown and no defaults are invented for them", path))
	}

	base := etcdsnap.DefaultConfig()
	if ccErr == nil && stateKnown {
		base = cc.SnapshotBase()
	}
	conf, fromFile, confErr := loadSnapshotConf(ctx, base)
	if confErr != nil {
		warns = append(warns, "WARN: cannot read the snapshot config; the scheduled loop still takes snapshots but keeps every snapshot and uploads nothing, and the retention and copy policy are unknown")
	}

	// snapshot.conf carries no interval and no directory, and cannot disable the loop.
	if ccErr == nil && stateKnown {
		p.Interval = time.Duration(cc.Spec.Snapshot.IntervalHours) * time.Hour
		// An explicitly configured default is indistinguishable from an unset field.
		if cc.Spec.Snapshot.IntervalHours == version.SnapshotIntervalHours {
			p.IntervalFrom = FromDefault
		} else {
			p.IntervalFrom = FromClusterConfig
		}
		p.Dir = cc.SnapshotDir()
		if p.Dir == defaultSnapshotDir {
			p.DirFrom = FromDefault
		} else {
			p.DirFrom = FromClusterConfig
		}
	} else {
		p.IntervalFrom, p.DirFrom = FromUnknown, FromUnknown
	}

	switch {
	case confErr != nil:
		p.RetainFrom, p.CopyFrom = FromUnknown, FromUnknown
	case fromFile:
		p.Retain, p.RetainFrom = conf.Retain, FromSnapshotConf
		if conf.S3.Bucket != "" {
			p.Copy, p.CopyFrom = CopyRemoteRequired, FromSnapshotConf
		} else {
			p.Copy, p.CopyFrom = CopyLocalOnly, FromSnapshotConf
		}
	case ccErr != nil || !stateKnown:
		p.RetainFrom, p.CopyFrom = FromUnknown, FromUnknown
	default:
		p.Retain, p.RetainFrom = base.Retain, FromClusterConfig
		if p.Retain == version.SnapshotRetainDefault {
			p.RetainFrom = FromDefault
		}
		if base.S3.Bucket != "" {
			p.Copy, p.CopyFrom = CopyRemoteRequired, FromClusterConfig
		} else if cc.Spec.Snapshot.S3 != nil {
			// An empty bucket in an s3 section is still the operator's config, not the default.
			p.Copy, p.CopyFrom = CopyLocalOnly, FromClusterConfig
		} else {
			p.Copy, p.CopyFrom = CopyLocalOnly, FromDefault
		}
	}

	switch {
	case !stateKnown || ccErr != nil:
		p.State = StateUnknown
	case cc.Spec.Snapshot.Disabled:
		p.State = StateDisabled
	case confErr != nil:
		p.State = StateUnreadable
	case p.IntervalFrom == FromDefault && p.RetainFrom == FromDefault && p.CopyFrom == FromDefault && p.DirFrom == FromDefault:
		p.State = StateUnconfigured
	default:
		p.State = StateConfigured
	}
	p.Warnings = warns
	return p
}

func activePath(ctx context.Context, warns *[]string) (path string, recorded, known bool) {
	st, err := loadNodeState(ctx)
	switch {
	case err == nil && st.ConfigPath != "":
		return st.ConfigPath, true, true
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		*warns = append(*warns, "WARN: cannot read the node state; the cluster config the controller was started with is unknown, so the policy may not be what the controller runs")
		return cluster.DefaultConfigPath, false, false
	default:
		return cluster.DefaultConfigPath, false, true
	}
}

// A recorded config deleted since start is unreadable, not the no-config case.
func loadActiveClusterConfig(ctx context.Context, path string, recorded bool) (*cluster.ClusterConfig, error) {
	cc, err := loadClusterConfig(ctx, path)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err == nil && recorded {
		if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: the cluster config this controller was started with no longer exists", path)
		}
	}
	return cc, err
}

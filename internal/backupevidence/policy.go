// Package backupevidence exposes the engine's own view of what it does for
// backups — the effective policy, the local artifacts, the remote copies —
// so an external consumer reads engine-resolved evidence instead of
// re-deriving it from config files or importing engine internals.
package backupevidence

import (
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
	// FromClusterConfig: spec.snapshots in the cluster config file the
	// controller was started with.
	FromClusterConfig Provenance = "cluster-config"
	// FromSnapshotConf: the operator-managed snapshot.conf, which wins
	// outright over the cluster config when it exists.
	FromSnapshotConf Provenance = "snapshot-conf"
	// FromDefault: the built-in default the engine applies when nothing
	// configures the value.
	FromDefault Provenance = "default"
	// FromUnknown: the value cannot be determined; the value beside it
	// carries no information, and no default is invented for it.
	FromUnknown Provenance = "unknown"
)

// State summarizes what a consumer can rely on in the resolved policy.
type State string

const (
	// StateConfigured: the scheduled snapshot loop runs with at least one
	// operator-configured value, from the cluster config or snapshot.conf.
	StateConfigured State = "configured"
	// StateDisabled: spec.snapshots.disabled — the controller never starts
	// the snapshot loop; the other fields still report the configured policy.
	StateDisabled State = "disabled"
	// StateUnconfigured: nothing configures snapshots anywhere; the loop
	// runs on the built-in defaults, reported with default provenance.
	StateUnconfigured State = "unconfigured"
	// StateUnknown: the cluster config the controller was started with
	// cannot be read, so the schedule — and every value it alone carries —
	// is unknown.
	StateUnknown State = "unknown"
	// StateUnreadable: snapshot.conf exists but cannot be read; the loop
	// takes snapshots but keeps every one and uploads nothing, and no
	// threshold is invented for what the file would have said.
	StateUnreadable State = "unreadable"
)

// CopyRequirement is what the scheduled loop does with every snapshot beyond
// the local file it writes.
type CopyRequirement string

const (
	// CopyLocalOnly: no S3 target is in force; snapshots stay in Dir.
	CopyLocalOnly CopyRequirement = "local-only"
	// CopyRemoteRequired: an S3 target is in force; every scheduled
	// snapshot is also uploaded to it.
	CopyRemoteRequired CopyRequirement = "remote-required"
)

// EffectivePolicy is the backup policy the scheduled snapshot loop enforces,
// every resolved value paired with its provenance. It is a re-resolution of
// the config the controller read at start — the same resolution
// cluster.LoadSnapshotConfig performs for the CLI and the MCP server — so a
// config changed since the controller started shows up here even while the
// running loop still holds its own loaded copy.
type EffectivePolicy struct {
	// State says what a consumer can rely on in the fields below.
	State State
	// Interval is how often the scheduled loop takes a snapshot. Zero with
	// unknown provenance when the cluster config cannot be read.
	Interval     time.Duration
	IntervalFrom Provenance
	// Retain is how many of this node's snapshots pruning keeps. Zero with
	// unknown provenance means unknown, not keep-all.
	Retain     int
	RetainFrom Provenance
	// Copy is the copy requirement the effective S3 target imposes. Empty
	// with unknown provenance when the target cannot be determined.
	Copy     CopyRequirement
	CopyFrom Provenance
	// Dir is the local snapshot directory: spec.snapshots.dir, else the
	// built-in default. Empty with unknown provenance when the cluster
	// config cannot be read.
	Dir     string
	DirFrom Provenance
	// Warnings carry the engine's WARN conditions that shaped this policy:
	// an unreadable snapshot.conf, a cluster config that cannot be read.
	// They describe the condition; they carry no raw errors.
	Warnings []string
}

// defaultSnapshotDir is the built-in snapshot directory, used only to tell a
// config-supplied directory from the default in the provenance. It must stay
// identical to the resolver's own default (internal/cluster's and
// internal/etcdsnap's defaultSnapshotDir), which is what actually resolves
// the directory.
const defaultSnapshotDir = "/var/backups/etcd"

// The reads the policy resolves through, each behind a seam so a test can
// point the resolution at its own files instead of the host's /etc and node
// state (the same seam convention as internal/cluster's loop seams and
// pkg/cli's snapshot commands).
var (
	// loadNodeState reads the node state that records the cluster config
	// file the controller was started with.
	loadNodeState = nodestate.LoadState
	// loadClusterConfig reads the cluster config at a path; a missing file
	// yields the pure defaults (cluster.LoadConfig).
	loadClusterConfig = cluster.LoadConfig
	// loadSnapshotConf reads the operator-managed snapshot.conf over the
	// cluster-config base (etcdsnap.LoadConfigOr): an existing file wins
	// outright, environment credentials included in the values it returns.
	// fromFile reports whether the file supplied the result; the error is
	// its read failure.
	loadSnapshotConf = func(base etcdsnap.Config) (etcdsnap.Config, bool, error) {
		c, err := etcdsnap.LoadConfigOr(base)
		return c, c.FromFile(), err
	}
)

// Policy resolves the effective backup policy — schedule, retention, copy
// requirements and the snapshot directory — exactly as the scheduled
// snapshot loop resolves its options each tick: the cluster config the
// controller was started with, snapshot.conf winning outright over it when
// it exists, environment credentials included in the target the loop would
// use, the directory from spec.snapshots.dir else the built-in default. A
// value that cannot be determined is reported unknown with no invented
// threshold: the loop itself prunes nothing and uploads nothing when its
// snapshot.conf cannot be read, and the controller keeps running with the
// config it was started with when that config can no longer be read.
func Policy() EffectivePolicy {
	var p EffectivePolicy
	var warns []string

	// The cluster config file the running controller reads: the one
	// `stratum up --config` recorded in the node state, else the default
	// path — the same first step cluster.LoadSnapshotConfig takes. A node
	// state that cannot be read falls back to the default path with a
	// warning, exactly as the engine resolver does, but the policy marks
	// itself unknown: the controller may have been started with any config.
	path, recorded, stateKnown := activePath(&warns)
	cc, ccErr := loadClusterConfig(path)
	// A recorded config that has since been deleted is an unreadable one,
	// not the ordinary no-config case (the resolver's mustExist rule).
	if ccErr == nil && recorded {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			ccErr = fmt.Errorf("%s: the cluster config this controller was started with no longer exists", path)
		}
	}
	if ccErr != nil {
		warns = append(warns, fmt.Sprintf("WARN: cannot read the cluster config the controller was started with (%s); the schedule, the snapshot directory and the disabled state are unknown and no defaults are invented for them", path))
	}

	// The snapshot settings base, and snapshot.conf over it. An unreadable
	// conf is not fatal to the loop — it still takes the snapshot — so it
	// is not fatal here either, but the retention and copy policy it would
	// have supplied are unknown: the loop's own resolution keeps every
	// snapshot and uploads nothing rather than act on settings it could not
	// read.
	base := etcdsnap.DefaultConfig()
	if ccErr == nil {
		base = cc.SnapshotBase()
	}
	conf, fromFile, confErr := loadSnapshotConf(base)
	if confErr != nil {
		warns = append(warns, "WARN: cannot read the snapshot config; the scheduled loop still takes snapshots but keeps every snapshot and uploads nothing, and the retention and copy policy are unknown")
	}

	// The schedule and the disabled state come only from the cluster
	// config: snapshot.conf carries no interval and cannot disable the
	// loop. So does the directory, which no conf ever supplies.
	if ccErr == nil {
		p.Interval = time.Duration(cc.Spec.Snapshot.IntervalHours) * time.Hour
		// A value equal to the built-in default is reported with default
		// provenance: after the engine's own defaults pass an explicitly
		// configured default is indistinguishable from an unset field, and
		// the policy in force is identical either way.
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

	// Retention and the copy requirement come from snapshot.conf when it
	// exists and is readable, else from the cluster config.
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
	case ccErr != nil:
		p.RetainFrom, p.CopyFrom = FromUnknown, FromUnknown
	default:
		p.Retain, p.RetainFrom = base.Retain, FromClusterConfig
		if p.Retain == version.SnapshotRetainDefault {
			p.RetainFrom = FromDefault
		}
		if base.S3.Bucket != "" {
			p.Copy, p.CopyFrom = CopyRemoteRequired, FromClusterConfig
		} else if cc.Spec.Snapshot.S3 != nil {
			// An s3 section without a usable bucket contributes no target,
			// but it is still the operator's config saying so.
			p.Copy, p.CopyFrom = CopyLocalOnly, FromClusterConfig
		} else {
			p.Copy, p.CopyFrom = CopyLocalOnly, FromDefault
		}
	}

	// The state leads with what dominates the loop's behaviour: whether it
	// runs at all (disabled), whether its per-tick resolution failed
	// (unreadable conf), and whether the config it runs can be determined
	// at all (unknown).
	switch {
	case !stateKnown || ccErr != nil:
		// What the controller actually runs cannot be determined, so not
		// even a disabled flag can be claimed for it.
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

// activePath resolves the cluster config file the running controller reads —
// the same resolution cluster.LoadSnapshotConfig opens with: the path the
// node state recorded for `stratum up --config`, else the built-in default
// path. A node state that does not exist yet is the ordinary uninitialised
// case; any other read failure warns and falls back to the default path, and
// known comes back false so the policy can mark itself unknown.
func activePath(warns *[]string) (path string, recorded, known bool) {
	st, err := loadNodeState()
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

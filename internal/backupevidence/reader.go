package backupevidence

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/objstore"
)

// Typed, never raw error text: a raw error can carry a credential or a path.
type AttemptOutcome string

const (
	AttemptSucceeded AttemptOutcome = "succeeded"
	AttemptFailed    AttemptOutcome = "failed"
)

type AttemptEvidence struct {
	AttemptedAt time.Time
	Outcome     AttemptOutcome
}

// Maintenance is the scheduled snapshot loop's durable maintenance state as safe diagnostics.
type Maintenance struct {
	Save   AttemptEvidence
	Upload AttemptEvidence
	// Independent of Save and Upload: a saved backup with failed pruning still works.
	Prune AttemptEvidence
	// Separate from the latest attempts so a failure streak does not erase them.
	LastSaveSuccess   time.Time
	LastUploadSuccess time.Time
	Warnings          []string
	// False means unknown, never that no attempt was made.
	AttemptsKnown bool
}

const (
	DefaultEvidenceMaxEntries = DefaultLocalMaxEntries
	DefaultEvidenceMaxBytes   = DefaultLocalMaxBytes
	DefaultEvidenceMaxTime    = 20 * time.Second
)

type EvidenceOptions struct {
	Node       string
	MaxEntries int
	MaxBytes   int64
	MaxTime    time.Duration
}

type EvidenceResult struct {
	Policy      EffectivePolicy
	Node        string
	NodeUID     string
	Local       LocalResult
	Remote      RemoteResult
	Backups     Census
	Maintenance Maintenance
	Complete    bool
	// The policy's, the census's and the maintenance's, in that order.
	Warnings []string
}

var (
	readerPolicy = func(ctx context.Context) (p EffectivePolicy, known bool) {
		if err := ctx.Err(); err != nil {
			return unknownPolicy(), false
		}
		p = policy(ctx)
		if ctx.Err() != nil {
			return unknownPolicy(), false
		}
		return p, true
	}
	readerAttempts = func(ctx context.Context) (*Attempts, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return loadAttempts(ctx)
	}
	// The resolved target carries credentials and must never reach the result.
	readerTarget = func(ctx context.Context) (*objstore.Target, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target, err := effectiveTarget(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return target, err
	}
)

const (
	warningPolicyCancelled  = "WARN: the evidence read was cancelled before the policy was resolved; the effective policy is unknown"
	warningRecordUnreadable = "WARN: the snapshot attempt record cannot be read; the maintenance diagnostics are unknown"
	warningRecordCancelled  = "WARN: the evidence read was cancelled before the attempt record was read; the maintenance diagnostics are unknown"
	warningLocalCancelled   = "WARN: the evidence read was cancelled before the local catalog was walked; the local catalog is unknown and not a complete total"
	warningDirUnknown       = "WARN: the snapshot directory is unknown because the cluster config cannot be read; the local catalog is unknown and not a complete total"
	warningTargetCancelled  = "WARN: the evidence read was cancelled before the remote target was resolved; the remote catalog is unknown and not a complete total"
	warningTargetUnknown    = "WARN: the remote target could not be determined because the snapshot config or the recorded cluster config cannot be read; the remote catalog is unknown and not a complete total"
	warningRemotePageSpent  = "WARN: the overall page budget of %d entries was reached by the local walk alone; the remote catalog is unknown and not a complete total"
	warningRemoteBytesSpent = "WARN: the overall byte budget of %d bytes was reached by the local walk alone; the remote catalog is unknown and not a complete total"
	warningRemotePageTrim   = "WARN: the overall page budget of %d entries was reached across the local and remote reads; the remote catalog is not a complete total"
	// Same condition as the walk's own warning, in the local stage's warnings.
	warningNodeUnknown = "WARN: this node's name could not be determined; the local catalog's location is not node-scoped"
)

// Recorded lines are matched on their phrase; the rest can carry raw errors.
const (
	warningTickConfUnreadable = "WARN: the recorded tick ran with a snapshot config it could not read; it kept every snapshot and uploaded nothing"
	warningTickLocalPrune     = "WARN: the recorded tick's local snapshot prune failed; local retention may not have held"
	warningTickRemotePrune    = "WARN: the recorded tick's remote snapshot prune failed; remote retention may not have held"
	warningTickUnstateable    = "WARN: the recorded tick carried a warning this reader cannot state safely"
)

var recordedWarningConditions = []struct{ phrase, safe string }{
	{"cannot read snapshot config", warningTickConfUnreadable},
	{"local snapshot prune", warningTickLocalPrune},
	{"S3 snapshot prune", warningTickRemotePrune},
}

// An unmatched line becomes the unstateable warning; its text is never echoed.
func safeRecordedWarnings(recorded []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, line := range recorded {
		safe := warningTickUnstateable
		for _, c := range recordedWarningConditions {
			if strings.Contains(line, c.phrase) {
				safe = c.safe
				break
			}
		}
		if !seen[safe] {
			seen[safe] = true
			out = append(out, safe)
		}
	}
	return out
}

func unknownPolicy() EffectivePolicy {
	return EffectivePolicy{
		State:        StateUnknown,
		IntervalFrom: FromUnknown,
		RetainFrom:   FromUnknown,
		CopyFrom:     FromUnknown,
		DirFrom:      FromUnknown,
	}
}

// Internal marker; the remote stage states it as a safe warning, never this text.
var errTargetUndetermined = errors.New("the remote target could not be determined")

// An unreadable conf or config means an undetermined target, never no target.
func effectiveTarget(ctx context.Context) (*objstore.Target, error) {
	var sink []string
	path, recorded, stateKnown := activePath(ctx, &sink)
	cc, ccErr := loadActiveClusterConfig(ctx, path, recorded)
	base := etcdsnap.DefaultConfig()
	if ccErr == nil {
		base = cc.SnapshotBase()
	}
	conf, fromFile, confErr := loadSnapshotConf(ctx, base)
	if confErr != nil {
		return nil, errTargetUndetermined
	}
	if (ccErr != nil || !stateKnown) && !fromFile {
		return nil, errTargetUndetermined
	}
	return conf.S3Target(), nil
}

func maintenanceFrom(a *Attempts) Maintenance {
	return Maintenance{
		Save:              attemptEvidence(a.Save),
		Upload:            attemptEvidence(a.Upload),
		Prune:             attemptEvidence(a.Prune),
		LastSaveSuccess:   a.LastSaveSuccess,
		LastUploadSuccess: a.LastUploadSuccess,
		Warnings:          safeRecordedWarnings(a.Warnings),
		AttemptsKnown:     true,
	}
}

func attemptEvidence(a Attempt) AttemptEvidence {
	if a.IsZero() {
		return AttemptEvidence{}
	}
	e := AttemptEvidence{AttemptedAt: a.AttemptedAt, Outcome: AttemptSucceeded}
	if a.Result != "" {
		e.Outcome = AttemptFailed
	}
	return e
}

func artifactBytes(artifacts []Artifact) int64 {
	var n int64
	for _, a := range artifacts {
		n += a.Size
	}
	return n
}

// MaxTime bounds every read but cannot interrupt a local filesystem syscall stalled in the kernel.
func Evidence(ctx context.Context, o EvidenceOptions) EvidenceResult {
	var res EvidenceResult

	maxEntries, maxBytes, maxTime := o.MaxEntries, o.MaxBytes, o.MaxTime
	if maxEntries <= 0 {
		maxEntries = DefaultEvidenceMaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = DefaultEvidenceMaxBytes
	}
	if maxTime <= 0 {
		maxTime = DefaultEvidenceMaxTime
	}
	ctx, cancel := context.WithTimeout(ctx, maxTime)
	defer cancel()

	// The policy resolves first: its directory scopes the local catalog.
	policy, policyKnown := readerPolicy(ctx)
	if !policyKnown {
		policy.Warnings = append(policy.Warnings, warningPolicyCancelled)
	}
	res.Policy = policy

	var m Maintenance
	a, aerr := readerAttempts(ctx)
	switch {
	case aerr == nil:
		m = maintenanceFrom(a)
	case errors.Is(aerr, fs.ErrNotExist):
		m.AttemptsKnown = true
	case isCancellation(aerr):
		m.Warnings = []string{warningRecordCancelled}
	default:
		m.Warnings = []string{warningRecordUnreadable}
	}
	res.Maintenance = m

	var local LocalResult
	walked := false
	if policy.DirFrom != FromUnknown {
		walked = true
		local = Local(ctx, LocalOptions{
			Dir: policy.Dir, Node: o.Node,
			MaxEntries: maxEntries, MaxBytes: maxBytes,
		})
	} else if policyKnown {
		local = LocalResult{Node: o.Node, Complete: false, Warnings: []string{warningDirUnknown}}
	} else {
		local = LocalResult{Node: o.Node, Complete: false, Warnings: []string{warningLocalCancelled}}
	}
	if !walked && o.Node == "" {
		if hn, err := localHostname(); err == nil {
			local.Node = hn
		} else {
			local.Warnings = append(local.Warnings, warningNodeUnknown)
		}
	}
	res.Local = local

	entriesLeft := maxEntries - len(local.Artifacts)
	bytesLeft := maxBytes - artifactBytes(local.Artifacts)
	tgt, terr := readerTarget(ctx)
	var remote RemoteResult
	switch {
	case terr == nil && !tgt.Enabled():
		remote.Complete = true
	case entriesLeft <= 0:
		remote = RemoteResult{Complete: false, Warnings: []string{fmt.Sprintf(warningRemotePageSpent, maxEntries)}}
	case bytesLeft <= 0:
		remote = RemoteResult{Complete: false, Warnings: []string{fmt.Sprintf(warningRemoteBytesSpent, maxBytes)}}
	case isCancellation(terr):
		remote = RemoteResult{Complete: false, Warnings: []string{warningTargetCancelled}}
	case terr != nil:
		remote = RemoteResult{Complete: false, Warnings: []string{warningTargetUnknown}}
	default:
		remote = Remote(ctx, RemoteOptions{
			Target: tgt,
			// One page per ListKeysPerPage remaining entries; one per entry over-fetches.
			MaxPages: (entriesLeft + objstore.ListKeysPerPage - 1) / objstore.ListKeysPerPage,
			MaxBytes: bytesLeft, MaxTime: maxTime,
		})
		if len(remote.Copies) > entriesLeft {
			// Old-copy flags follow the kept copies; a dropped copy is flagged nowhere.
			keep := slices.Clone(remote.Copies[:entriesLeft])
			kept := make(map[string]bool, len(keep))
			for _, a := range keep {
				kept[a.ID] = true
			}
			remote.Old = slices.DeleteFunc(remote.Old, func(a Artifact) bool { return !kept[a.ID] })
			remote.Copies = keep
			remote.Complete = false
			remote.Warnings = append(remote.Warnings, fmt.Sprintf(warningRemotePageTrim, maxEntries))
		}
	}
	res.NodeUID = identifyLocations(ctx, local.Node, policy.Dir, tgt, &local, &remote)
	res.Local = local
	res.Remote = remote

	res.Backups = Union(local, remote)
	res.Node = res.Backups.Node
	res.Complete = res.Backups.Complete && m.AttemptsKnown && res.NodeUID != ""
	res.Warnings = append(res.Warnings, policy.Warnings...)
	res.Warnings = append(res.Warnings, res.Backups.Warnings...)
	res.Warnings = append(res.Warnings, m.Warnings...)
	return res
}

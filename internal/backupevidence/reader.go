// The assembled read-only Evidence() API: one bounded call composing the
// effective policy (Policy below), the durable attempt state (LoadAttempts)
// and the local and remote catalogs (Local, Remote) into the engine's own
// view of what it does for backups. Everything an external consumer reads
// comes from here — no consumer re-derives the engine's config resolution —
// and every diagnostic is a typed safe reason: no credential, no executable
// path and no raw storage error reaches the result.
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

// AttemptOutcome is a recorded attempt's typed safe result: what the
// attempt did, never the error text it failed with — a raw error can carry
// a credential, an executable path or a storage address, and the evidence
// names conditions only.
type AttemptOutcome string

const (
	// AttemptSucceeded: the attempt's record carries no failure.
	AttemptSucceeded AttemptOutcome = "succeeded"
	// AttemptFailed: the attempt's record carries a failure; the
	// maintenance warnings state its condition where they can.
	AttemptFailed AttemptOutcome = "failed"
)

// AttemptEvidence is one recorded attempt as safe diagnostics: when it ran
// and the typed outcome. A zero AttemptedAt means the loop has not run the
// attempt yet, which is exact — the record's first attempt is its first
// tick — not an unknown.
type AttemptEvidence struct {
	// AttemptedAt is when the loop started the attempt.
	AttemptedAt time.Time
	// Outcome is the typed safe result; empty when no attempt is recorded.
	Outcome AttemptOutcome
}

// Maintenance is the scheduled snapshot loop's durable maintenance state
// as safe diagnostics: the latest save, upload and prune attempts, when
// the save and the upload last succeeded, and the scheduling and retention
// warnings the loop's last tick ran with. Each warning is a typed safe
// reason stating the recorded condition; the recorded line's own text —
// which can carry the raw error the tick printed — is never echoed.
type Maintenance struct {
	// Save, Upload and Prune are the latest recorded attempts, each the
	// attempt's timestamp with a typed outcome. Prune is independent of
	// Save and Upload: a save that succeeded while pruning failed is a
	// working backup with broken retention.
	Save   AttemptEvidence
	Upload AttemptEvidence
	Prune  AttemptEvidence
	// LastSaveSuccess and LastUploadSuccess are when the save and the
	// upload last succeeded, kept separate from the latest attempts so a
	// streak of failures does not erase when backups last worked.
	LastSaveSuccess   time.Time
	LastUploadSuccess time.Time
	// Warnings state the record's scheduling and retention warnings as
	// typed safe reasons.
	Warnings []string
	// AttemptsKnown says the record was read. False means the diagnostics
	// are unknown — a record that exists but cannot be read, or a read the
	// request's bounds cut short — never that no attempt was made.
	AttemptsKnown bool
}

// The built-in bounds of one evidence read: a caller that passes no cap
// gets these, never an unbounded read.
const (
	// DefaultEvidenceMaxEntries is the overall page budget: how many
	// backups — local artifacts and remote copies together — one read may
	// describe.
	DefaultEvidenceMaxEntries = DefaultLocalMaxEntries
	// DefaultEvidenceMaxBytes is the overall byte budget: how many
	// artifact bytes the local walk and the remote listing together may
	// account for.
	DefaultEvidenceMaxBytes = DefaultLocalMaxBytes
	// DefaultEvidenceMaxTime is the overall deadline: how long every read
	// the call makes may take together, however patient the caller's own
	// context is.
	DefaultEvidenceMaxTime = 20 * time.Second
)

// EvidenceOptions bounds and scopes one evidence read. The bounds are one
// overall budget the reads share: MaxTime is a deadline over every read
// the call makes, MaxEntries counts every backup the call describes — each
// local artifact and each remote copy — and MaxBytes counts the artifact
// bytes both walks account for. Zero or negative caps mean the built-in
// defaults, never unbounded reads.
type EvidenceOptions struct {
	// Node scopes the local half's location; empty means this host's short
	// name, the way the loop's own snapshot naming resolves it.
	Node string
	// MaxEntries is the overall page budget.
	MaxEntries int
	// MaxBytes is the overall byte budget.
	MaxBytes int64
	// MaxTime is the overall deadline.
	MaxTime time.Duration
}

// EvidenceResult is one bounded read of the engine's backup evidence: the
// effective policy with every value's provenance, the local artifacts, the
// remote copies, the deduplicated census that counts each backup once
// across both locations, and the maintenance diagnostics. Warnings
// aggregates every stage's safe diagnostics; Complete says every stage ran
// whole within the bounds — false means some stage's evidence is a floor
// or unknown, never that it is zero.
type EvidenceResult struct {
	// Policy is the effective backup policy the scheduled loop enforces.
	Policy EffectivePolicy
	// Node scopes the local half: the node whose store the local catalog
	// read.
	Node string
	// Local is the bounded local catalog, and Remote the bounded remote
	// catalog, each exact only when its own Complete says so.
	Local  LocalResult
	Remote RemoteResult
	// Backups is the deduplicated census: every stable backup ID once,
	// with its local artifact and its remote copy, exact only when both
	// catalogs were complete.
	Backups Census
	// Maintenance is the loop's durable maintenance state as safe
	// diagnostics.
	Maintenance Maintenance
	// Complete says the whole read ran whole within its bounds.
	Complete bool
	// Warnings aggregate the policy's, the catalogs', the maintenance and
	// the read's own safe diagnostics.
	Warnings []string
}

// The reads the assembled evidence resolves through, each behind a seam so
// a test can drive its own files, record and store (the same seam
// convention the policy, the catalogs and the loop's own config reads
// follow). Each seam honours the request context: a context that already
// ended launches no read, so the overall deadline bounds every stage.
var (
	// evidenceNow is the clock the retention window draws; the reads
	// themselves use the times the artifacts and objects carry.
	evidenceNow = time.Now
	// readerPolicy resolves the effective policy under the request
	// context; known is false when the context ended before the
	// resolution, in which case the policy is the same unknown shape
	// Policy reports for a config it cannot read.
	readerPolicy = func(ctx context.Context) (p EffectivePolicy, known bool) {
		if err := ctx.Err(); err != nil {
			return unknownPolicy(), false
		}
		return Policy(), true
	}
	// readerAttempts reads the durable attempt record under the request
	// context.
	readerAttempts = func(ctx context.Context) (*Attempts, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return LoadAttempts()
	}
	// readerTarget resolves the effective S3 target — the one the
	// scheduled loop uploads to, credentials included for the listing
	// that needs them and never reaching the result — under the request
	// context. A non-cancellation error says the target could not be
	// determined, never the read's own failure: the remote stage states
	// it as a safe warning and reports the catalog unknown.
	readerTarget = func(ctx context.Context) (*objstore.Target, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return effectiveTarget()
	}
)

// The read's own safe diagnostics. Each names the condition and what is
// unknown about it; none carries a path, an address or a raw error.
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
	// warningNodeUnknown restates the local walk's own warning, so the
	// read says the same thing wherever the node name fails to resolve.
	warningNodeUnknown = "WARN: this node's name could not be determined; the local catalog's location is not node-scoped"
)

// The conditions the loop's recorded warning lines are stated as. The
// phrases are the stable ones the loop's own WARN lines carry; the recorded
// line is matched on its phrase, never echoed, because the loop prints its
// raw error after the phrase — a storage address, a credential, an
// executable path — and that text never reaches the evidence.
const (
	warningTickConfUnreadable = "WARN: the recorded tick ran with a snapshot config it could not read; it kept every snapshot and uploaded nothing"
	warningTickLocalPrune     = "WARN: the recorded tick's local snapshot prune failed; local retention may not have held"
	warningTickRemotePrune    = "WARN: the recorded tick's remote snapshot prune failed; remote retention may not have held"
	warningTickUnstateable    = "WARN: the recorded tick carried a warning this reader cannot state safely"
)

// recordedWarningConditions pairs each stable phrase with the typed safe
// reason the condition is stated as.
var recordedWarningConditions = []struct{ phrase, safe string }{
	{"cannot read snapshot config", warningTickConfUnreadable},
	{"local snapshot prune", warningTickLocalPrune},
	{"S3 snapshot prune", warningTickRemotePrune},
}

// safeRecordedWarnings states the record's warnings as typed safe reasons,
// one per distinct condition in the recorded order. A line no condition
// matches is stated as a warning this reader cannot state safely — the
// condition is kept, its text is not.
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

// unknownPolicy is the policy shape a resolution that never ran reports:
// the same unknown Policy reports for a config it cannot read — every value
// unknown, no provenance or threshold invented.
func unknownPolicy() EffectivePolicy {
	return EffectivePolicy{
		State:        StateUnknown,
		IntervalFrom: FromUnknown,
		RetainFrom:   FromUnknown,
		CopyFrom:     FromUnknown,
		DirFrom:      FromUnknown,
	}
}

// errTargetUndetermined is the internal marker for an S3 target the reader
// could not determine: an existing snapshot.conf that cannot be read, or a
// cluster config that cannot be read when no readable conf supplies a
// target. It never reaches the result; the remote stage states the condition
// as a safe warning and reports the catalog unknown.
var errTargetUndetermined = errors.New("the remote target could not be determined")

// effectiveTarget resolves the S3 target the scheduled loop uploads to —
// the same conf-over-cluster-config resolution the effective policy derives
// its copy requirement from: the conf seam's values whenever they are
// readable (an existing conf wins outright; a missing one returns the
// cluster-config base with the environment's credentials overlaid), and no
// target at all when nothing configures one, because the loop uploads
// nothing it could not read the target of. A conf that exists but cannot be
// read is an undetermined target, never "no target": the store may still
// hold the copies that conf uploaded while it was readable. So is a cluster
// config that cannot be read when no readable conf supplies a target, and
// so are the two ways the controller's own config is unknowable: a recorded
// config that has since been deleted (the resolver's mustExist rule), and a
// node state that cannot be read — the loop may still be uploading to the
// target that config carried while it was readable, and the store may hold
// those copies. Only a cluster config that was read — or a conf that
// replaced its say — settles the no-target case as a real zero. The
// returned target carries the credentials the listing needs; neither it
// nor the error reaches the result.
func effectiveTarget() (*objstore.Target, error) {
	var sink []string
	path, recorded, stateKnown := activePath(&sink)
	cc, ccErr := loadActiveClusterConfig(path, recorded)
	base := etcdsnap.DefaultConfig()
	if ccErr == nil {
		base = cc.SnapshotBase()
	}
	conf, fromFile, confErr := loadSnapshotConf(base)
	if confErr != nil {
		return nil, errTargetUndetermined
	}
	if (ccErr != nil || !stateKnown) && !fromFile {
		return nil, errTargetUndetermined
	}
	return conf.S3Target(), nil
}

// maintenanceFrom shapes the durable record as safe diagnostics. The
// attempts' error text never crosses — the outcomes are typed — and the
// warnings are stated as typed safe reasons.
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

// attemptEvidence shapes one recorded attempt: a zero record is no attempt,
// and a recorded one is its timestamp with the typed outcome — succeeded
// when the record carries no failure, failed when it does.
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

// retentionCutoff draws the retention window's start from the effective
// policy: Retain snapshots held at Interval cadence — the steady-schedule
// age of the Retain-th newest snapshot — so a remote copy created before
// it predates the retention window or survived a failed prune. A retention
// or a schedule the policy does not know judges nothing: no window is
// invented for values that cannot be read.
func retentionCutoff(p EffectivePolicy) time.Time {
	if p.Retain <= 0 || p.Interval <= 0 {
		return time.Time{}
	}
	// The multiplication saturates rather than wraps: a window past every
	// real clock is no window at all.
	if time.Duration(p.Retain) > (1<<62)/p.Interval {
		return time.Time{}
	}
	return evidenceNow().Add(-time.Duration(p.Retain) * p.Interval)
}

// artifactBytes totals the artifact bytes the local walk accounted for —
// the part of the overall byte budget the local half spent.
func artifactBytes(artifacts []Artifact) int64 {
	var n int64
	for _, a := range artifacts {
		n += a.Size
	}
	return n
}

// Evidence is the engine's read-only view of what it does for backups, in
// one bounded call: the effective policy the scheduled loop enforces with
// every value's provenance, the local artifacts in the snapshot directory,
// the remote copies on the S3 target, the deduplicated census counting
// each backup once across both locations, and the loop's maintenance
// diagnostics — typed attempt outcomes, last-success stamps, scheduling and
// retention warnings.
//
// The bounds are one overall budget the reads share. The time budget is a
// deadline over every read the call makes: the request context propagates
// into the local walk and the S3 listing, so a hung or denied store ends
// when the bound ends it, and a stage whose context already ended launches
// no read. The page budget counts every backup the call describes — each
// local artifact and each remote copy — and the byte budget the artifact
// bytes both walks account for: the local walk draws from the budget first
// and the remote listing gets what remains, so the combined reads never
// exceed the caps. A stage the budget stops is unknown, never zero: a
// partial catalog is a floor and the census is never an exact total.
//
// Every diagnostic is a typed safe reason. The S3 target the listing
// resolves carries its credentials internally and never appears in the
// result; a failed read names its condition — refused, cancelled, a cap —
// and what is unknown about it; the attempt record's raw error text never
// crosses. The config reads resolve through the engine's own config seams,
// the cheap local file reads the engine itself performs for every tick.
func Evidence(ctx context.Context, o EvidenceOptions) EvidenceResult {
	var res EvidenceResult
	var warns []string

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
	// The overall deadline over every read the call makes; the shorter of
	// it and whatever deadline the caller's own context already carries.
	ctx, cancel := context.WithTimeout(ctx, maxTime)
	defer cancel()

	// The policy, resolved first: its directory scopes the local catalog
	// and its retention draws the remote listing's window.
	policy, policyKnown := readerPolicy(ctx)
	if !policyKnown {
		warns = append(warns, warningPolicyCancelled)
	}
	res.Policy = policy

	// The maintenance record: a missing record is a node whose loop has
	// not run yet — known, with no attempts — while a record that exists
	// but cannot be read leaves the diagnostics unknown, never a silently
	// fresh one.
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

	// The local catalog, in the directory the effective policy resolved. A
	// directory the policy cannot determine is not walked: no default
	// directory is invented to walk, and the catalog says it is unknown.
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
	// A walk the read never made leaves the node name to resolve here, the
	// same way the walk resolves it; a walk that ran already did. The
	// result is carried only after, so a name resolved here reaches the
	// local catalog the document reads.
	if !walked && o.Node == "" {
		if hn, err := localHostname(); err == nil {
			local.Node = hn
		} else {
			warns = append(warns, warningNodeUnknown)
		}
	}
	res.Local = local

	// The remote catalog, under the overall budget after the local walk:
	// the page and byte budgets are shared, so the local half's artifacts
	// and accounted bytes leave what remains to the listing.
	entriesLeft := maxEntries - len(local.Artifacts)
	bytesLeft := maxBytes - artifactBytes(local.Artifacts)
	tgt, terr := readerTarget(ctx)
	var remote RemoteResult
	switch {
	case entriesLeft <= 0:
		remote = RemoteResult{Complete: false, Warnings: []string{fmt.Sprintf(warningRemotePageSpent, maxEntries)}}
	case bytesLeft <= 0:
		remote = RemoteResult{Complete: false, Warnings: []string{fmt.Sprintf(warningRemoteBytesSpent, maxBytes)}}
	case isCancellation(terr):
		remote = RemoteResult{Complete: false, Warnings: []string{warningTargetCancelled}}
	case terr != nil:
		// A target that could not be determined is not a nil one: the
		// store may hold the copies an unreadable config's target
		// already uploaded, so the catalog is unknown — never a
		// complete zero.
		remote = RemoteResult{Complete: false, Warnings: []string{warningTargetUnknown}}
	default:
		remote = Remote(ctx, RemoteOptions{
			Target: tgt,
			// The listing's own bounds are the shares of the overall
			// budget that remain, never the full budget again.
			MaxPages: entriesLeft, MaxBytes: bytesLeft, MaxTime: maxTime,
			RetentionCutoff: retentionCutoff(policy),
		})
		if len(remote.Copies) > entriesLeft {
			// A listing page describes more copies than the budget left:
			// the first entriesLeft by ID stand, the rest are dropped,
			// and the old-copy flags follow the copies that survived — a
			// dropped copy is flagged nowhere, least of all on an entry
			// whose remote half no longer exists.
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
	res.Remote = remote

	// The census: each stable backup ID once, exact only when both halves
	// are; the read is whole when the census is and the record was read.
	res.Backups = Union(local, remote)
	res.Node = res.Backups.Node
	res.Complete = res.Backups.Complete && m.AttemptsKnown
	res.Warnings = append(res.Warnings, policy.Warnings...)
	res.Warnings = append(res.Warnings, res.Backups.Warnings...)
	res.Warnings = append(res.Warnings, m.Warnings...)
	res.Warnings = append(res.Warnings, warns...)
	return res
}

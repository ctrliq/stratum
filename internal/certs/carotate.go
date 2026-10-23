// CA rotation (`stratum certs rotate-ca`) — the on-demand, cluster-wide rotation of
// the trust anchors the leaf rotation deliberately leaves alone: the root CA, the
// front-proxy CA, and the ServiceAccount signing keypair. It runs in three phases
// that must each be applied to EVERY node before the next (RotateCACluster, or a
// careful operator, enforces the ordering):
//
//	trust-new  every node trusts old‖new (bundles); signer still old
//	sign-new   control planes promote the new signer + reissue all leaves
//	finalize   every node drops the old CA; only new remains
//
// This file is the single-node executor for one phase; the cross-node orchestrator
// lives in cacluster.go.
package certs

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/token"
	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/client-go/tools/clientcmd"
)

// Phase names for CA rotation.
const (
	PhaseTrustNew = "trust-new"
	PhaseSignNew  = "sign-new"
	PhaseFinalize = "finalize"
)

// CARotateOptions configures a CA rotation.
type CARotateOptions struct {
	Force       bool   // rotate even though an old CA or SA key is gone (compromise recovery); ca.crt is still required
	Phase       string // trust-new | sign-new | finalize
	Cluster     bool   // orchestrate across all nodes (see RotateCACluster)
	FetchShared bool   // (follower CP, trust-new) pull the initiator's staged material first, so every CP promotes the SAME new CA
	Quiet       bool

	// RestartWorkloads (--cluster) has Stratum restart the user Deployments,
	// DaemonSets and StatefulSets that use in-cluster API credentials, between
	// trust-new and sign-new. SkipWorkloadCheck turns off the checks on those
	// workloads (before trust-new, and before sign-new on a node).
	RestartWorkloads  bool
	SkipWorkloadCheck bool

	// SkipTokenCheck finalizes without checking that no mounted ServiceAccount
	// token is still signed by the old key (see satoken.go). The operator's
	// --skip-token-check, for compromise recovery, needs Force (CheckWorkloadFlags);
	// the orchestrator sets it directly, having checked every node itself.
	SkipTokenCheck bool

	// RunID carries the cluster rotation identity to each phase, including
	// remote node Jobs. Manual phases leave it empty and consult the shared
	// record only when finalized material needs to be scoped to an active run.
	RunID string

	// resumeStaging is set only by the --cluster orchestrator, for the
	// initiator's trust-new of a run that resumes an unfinished shared record
	// with no phase completed: that record is what proves a staging on this
	// node is the run's own trust-new, so one whose widened bundles were never
	// written is resumed (RotateCAMaterial reuses what is staged) instead of
	// refused as a rotation someone else left behind.
	resumeStaging bool
}

// CheckWorkloadFlags refuses the workload flag combinations that make no sense.
// One check, for the CLI and the MCP tool both.
func (o CARotateOptions) CheckWorkloadFlags() error {
	if o.RestartWorkloads && o.SkipWorkloadCheck {
		return errors.New("--restart-workloads and --skip-workload-check contradict each other: pass one")
	}
	if o.RestartWorkloads && !o.Cluster {
		return errors.New("--restart-workloads goes with --cluster")
	}
	if o.SkipTokenCheck && !o.Force {
		return errors.New("--skip-token-check is for compromise recovery and goes with --force")
	}
	return nil
}

// CARotatePhases are the CA-rotation phases, in the only order they may run.
// Both front ends name the phases from this list, so a phase that is added,
// dropped or renamed here changes what every front end accepts and prints.
var CARotatePhases = []string{PhaseTrustNew, PhaseSignNew, PhaseFinalize}

// CARotateFlags is the raw flag set `stratum certs rotate-ca` was given: what
// the CLI's cobra flags and the MCP tool's input struct both carry.
type CARotateFlags struct {
	Cluster     bool
	Phase       string
	Force       bool
	Rollback    bool
	FetchShared bool
	// Check, with Rollback, reports whether this node (with Cluster, every
	// node) can roll back, and changes nothing.
	Check bool

	RestartWorkloads  bool
	SkipWorkloadCheck bool
	SkipTokenCheck    bool
}

// Validate refuses contradictory or incomplete argument combinations rather
// than silently picking one, on a command that restarts the control plane on
// every node.
//
// It lives here, not in pkg/cli, because both front ends must refuse the same
// requests with the same words: the MCP tool returns this error verbatim, so
// it spells the CLI's flag names (--cluster, --phase) and an AI client can map
// its own arguments onto what the refusal asks for.
func (f CARotateFlags) Validate() error {
	const cmd = "stratum certs rotate-ca"
	switch {
	case f.Cluster && f.Phase != "":
		return fmt.Errorf("%s: use either --cluster (all phases) or --phase (one node), not both", cmd)
	case f.Check && !f.Rollback:
		return fmt.Errorf("%s: --check goes with --rollback: it reports whether this node (with --cluster, every node) can roll back, and changes nothing", cmd)
	case f.Rollback && f.Cluster && !f.Check:
		// A rollback is deliberately not orchestrated: a node that cannot roll
		// back must be found before any node does, so the cluster-wide form
		// is the read-only check, and the rollback itself stays per node.
		return fmt.Errorf("%s: a rollback runs per node. Run `%s --rollback --cluster --check` first, which confirms every node can roll back, "+
			"then `%s --rollback --yes` on each control plane and then on each worker", cmd, cmd, cmd)
	case f.Phase == "" && !f.Cluster && !f.Rollback:
		return fmt.Errorf("%s: phase is required: pass --cluster (drive all phases) or --phase=<%s> (single node)",
			cmd, strings.Join(CARotatePhases, "|"))
	case f.Phase != "" && !slices.Contains(CARotatePhases, f.Phase):
		return fmt.Errorf("%s: unknown phase %q: want one of %s", cmd, f.Phase, strings.Join(CARotatePhases, ", "))
	case f.FetchShared && f.Phase != PhaseTrustNew:
		// The flag exists for a follower control plane running trust-new by
		// hand; the cluster orchestrator computes the per-node fetch itself,
		// and sign-new, finalize and rollback never read it, so anywhere else
		// it would be a silent no-op on a command that restarts the control
		// plane.
		return fmt.Errorf("%s: --fetch-shared only applies to --phase=%s", cmd, PhaseTrustNew)
	}
	return nil
}

// Options turns validated flags into the options the entry points take, so a
// front end cannot assemble CARotateOptions by hand and drift from what the
// validator just accepted.
func (f CARotateFlags) Options() CARotateOptions {
	return CARotateOptions{
		Phase: f.Phase, Cluster: f.Cluster, Force: f.Force, FetchShared: f.FetchShared,
		RestartWorkloads: f.RestartWorkloads, SkipWorkloadCheck: f.SkipWorkloadCheck,
		SkipTokenCheck: f.SkipTokenCheck,
	}
}

// The restart warnings are printed by the CLI and returned in the MCP result
// before a rotating (not rollback) rotate-ca run. Workloads using in-cluster
// API config cache the CA they loaded at start, so from sign-new they fail with
// x509 errors until restarted: the restart belongs after trust-new publishes
// the new CA and before sign-new. --cluster does it itself for the add-ons
// Stratum ships, and for the user's Deployments, DaemonSets and StatefulSets
// with --restart-workloads; without it --cluster refuses to start while there
// are any, unless --skip-workload-check leaves them to the operator (one
// restart, once the run completes). A --phase run rolls nothing, so
// --phase=sign-new refuses until every workload has restarted, Stratum's own
// included. A pod restarted before sign-new gets an old-key token, which its
// client re-reads when the kubelet refreshes it, so nothing needs restarting
// after sign-new for those. The residual is a client that caches its token in
// memory, which still holds an old-key token after a restart before sign-new
// and which the finalize gate cannot see (it scans the tokens mounted as
// files): it needs a restart after sign-new has completed on every control
// plane and before finalize. --cluster runs finalize itself, so only --phase
// runs leave that window, and a --cluster operator restarts it after the run.
// The gate is cluster-wide under --cluster, but a --phase=finalize run checks
// only its own control plane, and a worker's finalize not at all. The other
// residual is the tokens of legacy kubernetes.io/service-account-token
// Secrets, which never rotate and which a restart never fixes. The per-node
// Jobs of a --cluster run execute --phase, so their logs carry the --phase
// text.
const (
	CARotateClusterWarning = "WARNING: workloads that use in-cluster API config (operators, cert-manager, ingress controllers, Cilium)\n" +
		"         keep the CA they started with, so from sign-new they fail with x509 errors until restarted.\n" +
		"         Stratum restarts its own add-ons before sign-new. Your Deployments, DaemonSets and StatefulSets\n" +
		"         are restarted before sign-new with --restart-workloads; with --skip-workload-check they are\n" +
		"         not, and you restart them once, when this command completes.\n" +
		"         A client that re-reads its service-account token needs no restart after sign-new, if it was\n" +
		"         restarted before it.\n" +
		"         Finalize refuses while any node still mounts a token signed with the old key. That scan reads\n" +
		"         mounted token files, so it cannot see a client that caches its token in memory: that client\n" +
		"         holds an old-key token even after a restart before sign-new, and is rejected with 401 once\n" +
		"         finalize drops the key. Restart it after sign-new has completed on every control plane and\n" +
		"         before finalize. This command runs finalize itself, so use --phase runs for that window;\n" +
		"         otherwise restart it when this command completes.\n" +
		"         Tokens from kubernetes.io/service-account-token Secrets do not rotate: recreate those\n" +
		"         Secrets (and any kubeconfig built from them) after finalize; a restart does not help.\n\n"
	CARotatePhaseWarning = "WARNING: workloads that use in-cluster API config (operators, cert-manager, ingress controllers, Cilium)\n" +
		"         keep the CA they started with, so from sign-new they fail with x509 errors until restarted.\n" +
		"         Restart all of them, Stratum's own add-ons included, after trust-new has completed on every\n" +
		"         node and before sign-new; sign-new refuses until they have, unless you pass --skip-workload-check.\n" +
		"         A client that re-reads its service-account token needs no restart after sign-new, if it was\n" +
		"         restarted before it.\n" +
		"         A client that caches its service-account token in memory holds an old-key token even after\n" +
		"         that restart, and is rejected with 401 once finalize drops the key. Restart it after sign-new\n" +
		"         has completed on every control plane and before finalize.\n" +
		"         --phase=finalize on a control plane refuses while that node mounts a token signed with the\n" +
		"         old key. It checks that control plane only, and finalize on a worker does not check: scan\n" +
		"         every node before the first control plane finalizes. The scan reads mounted token files, so\n" +
		"         it does not see a client that caches its token in memory.\n" +
		"         Tokens from kubernetes.io/service-account-token Secrets do not rotate: recreate those\n" +
		"         Secrets (and any kubeconfig built from them) after finalize; a restart does not help.\n\n"
)

func castep(w io.Writer, o CARotateOptions, msg string) {
	if !o.Quiet {
		k8sutil.Step(w, msg)
	}
}

// isWorker: a control-plane node renders the apiserver static pod; a worker
// does not.
func isWorker() bool { return !staticpod.Exists("kube-apiserver") }

// RotateCA runs one CA-rotation phase on THIS node. Control-plane nodes rotate the
// CA/SA material and reissue leaves; workers refresh their trust bundle (trust-new,
// finalize) and renew the kubelet client cert under the new CA (sign-new).
//
// The node maintenance lock comes first, before the phase is even decided —
// ahead of the staged-rotation guard, the first pre-change refusal on the
// control-plane path — so an invocation beside a live upgrade or other
// maintenance operation refuses with the operation-naming lock message and
// leaves a staged rotation untouched. The defer releases the lock on every
// exit, including a phase that fails after the lock.
func RotateCA(w io.Writer, o CARotateOptions) error {
	release, err := acquireNodeLock("certs rotate-ca", false)
	if err != nil {
		return err
	}
	defer release()
	if err := RequireNodeProfile("rotate-ca"); err != nil {
		return err
	}

	worker := isWorker()
	switch o.Phase {
	case PhaseTrustNew:
		if worker {
			return workerRefreshTrust(w, o, trustWiden)
		}
		return cpTrustNew(w, o)
	case PhaseSignNew:
		if worker {
			return workerReissueClientCert(w, o)
		}
		return cpSignNew(w, o)
	case PhaseFinalize:
		if worker {
			return workerRefreshTrust(w, o, trustNarrow)
		}
		return cpFinalize(w, o)
	default:
		return fmt.Errorf("certs: unknown rotate-ca phase %q (want %s|%s|%s)",
			o.Phase, PhaseTrustNew, PhaseSignNew, PhaseFinalize)
	}
}

// ── control-plane phases ─────────────────────────────────────────────────────

// caRotationGuard refuses to start a fresh CA rotation on a node already
// mid-rotation, which would stage a second, DIVERGENT root CA and split the
// trust domain. Pure, so the decision and its operator-facing messages
// are testable without a live PKI. The follower CP path (fetchShared) is exempt:
// it legitimately arrives with the initiator's ca-new already staged, so that it
// promotes the SAME new CA cluster-wide. Shared by the single-node phase path
// (cpTrustNew) and the cluster orchestrator (RotateCACluster) so the guard can
// never drift between the two.
func caRotationGuard(fetchShared, staged bool, bundleCACount int, node string) error {
	if fetchShared {
		return nil
	}
	if staged {
		return fmt.Errorf("a CA rotation is already staged on %s — finish it or run `stratum certs rotate-ca --rollback --yes` before starting a new one (see `stratum certs check`)", node)
	}
	if bundleCACount > 1 {
		return fmt.Errorf("%s is mid CA rotation (trust bundle still carries %d CAs) — run `stratum certs rotate-ca --rollback --yes` before starting a new rotation", node, bundleCACount)
	}
	return nil
}

// checkCARotationState validates the on-disk evidence before applying the
// shared guard. Force permits a missing old bundle for compromise recovery,
// but must not hide an unreadable or corrupt bundle or a known staged rotation.
// The caPhaseApplied consult in each phase entry sits ahead of this guard, so a
// re-run of a phase the node is past never reaches the refusals below.
func checkCARotationState(o CARotateOptions, node string) error {
	if o.FetchShared {
		return nil // follower downloads the initiator's material below
	}
	rot := pki.ReadCARotation()
	if rot.Staged {
		if o.Force && pki.ForcedRotation() {
			return nil
		}
		if o.Phase == PhaseTrustNew && pki.ForcedRotation() {
			return stateRefusal{forcedStagedRefusal(node)}
		}
		// A re-run of trust-new itself is the idempotent resume of a halted run:
		// RotateCAMaterial reuses the staged material and rewrites the same
		// bundles, so no second, divergent root can appear. The widened bundle is
		// what says the staging is one this command wrote — or, for a staging
		// whose bundle writes failed, the unfinished shared record the
		// orchestrator resumes (resumeStaging); a staging with neither refuses
		// as before, as does a re-run of any other phase (and an entry check
		// that names no phase). A forced rotation resumes only with --force:
		// RotateCAMaterial records --force on the rotation and REMOVES the
		// marker without it, so a re-run without the flag would silently
		// un-force a rotation no node may roll back.
		if o.Phase == PhaseTrustNew && rot.Err == nil && (rot.BundleCAs > 1 || o.resumeStaging) && (o.Force || !pki.ForcedRotation()) {
			return nil
		}
		return stateRefusal{caRotationGuard(false, true, 0, node)}
	}
	if rot.Err != nil {
		if o.Force && errors.Is(rot.Err, os.ErrNotExist) {
			return nil
		}
		return stateRefusal{fmt.Errorf("cannot verify CA rotation state on %s: %w", node, rot.Err)}
	}
	if err := caRotationGuard(false, false, rot.BundleCAs, node); err != nil {
		return stateRefusal{err}
	}
	return nil
}

// stateRefusal is a node's refusal of a phase its on-disk rotation state rules
// out. It names its own way out; the --cluster halt passes it through rather
// than adding the re-run guidance, because a re-run would be refused the same
// way.
type stateRefusal struct{ error }

func (r stateRefusal) Unwrap() error { return r.error }

// phaseRunID is the rotation identity the applied-phase decision scopes to:
// the orchestrator names the rotation this run continues; a run that arrives
// without one scopes by the staged key the cluster holds, if any.
func (o CARotateOptions) phaseRunID() (string, error) {
	if o.RunID != "" {
		return o.RunID, nil
	}
	// Promotion evidence is scoped by scratch files which finalize removes;
	// only a finalized node needs a shared identity to interpret its marker.
	if !pki.CAFinalized() {
		return "", nil
	}
	return stagedRotationRunID()
}

// caPhaseApplied reports whether one CA-rotation phase's material work is
// already present on this node, so a resumed rotation and a re-run
// `rotate-ca --cluster` SKIP finished work instead of tripping a guard written
// for a deliberate re-invocation. It is the root-CA rotation's answer to
// pki.EtcdPhaseApplied, and every phase entry consults it ahead of the guards
// it supersedes, the way a node decides applied before it fetches.
//
// A phase counts as applied only when the node is STRICTLY past it, and only
// when the work is RECORDED — the files are not the process:
//
//   - trust-new is applied once the staged root was promoted or the rotation
//     ended in finalize. The promotion backup pki.ReadCARotation reports is
//     the evidence only while the rotation is still live on this node: the
//     carotate-promoted marker is there, or trust is still old‖new. A backup
//     on a node back at one root, or beside a staged root, is a rollback whose
//     scratch removal failed, and counting it would make a new rotation skip
//     trust-new here — or skip the resume of its trust-new — and never trust
//     its root. A node that never promoted is not applied whatever its
//     bundle reads — a missing or corrupt bundle is checkCARotationState's to
//     judge (--force recovery, a follower's fetch) — and a node that is
//     merely AT trust-new — material staged, bundles written — is not applied
//     either: re-running RotateCAMaterial reuses the staged material and
//     rewrites the same bundles, which is the idempotent re-run the etcd
//     reader also re-runs rather than skips. A promoted node whose bundle
//     cannot be read and that has no marker is an error: it cannot be told
//     from a partial rollback, and a wrong "not applied" would stage a second,
//     divergent root.
//   - sign-new is applied once its work is recorded: the carotate-promoted
//     marker (pki.CAPromoted) or the carotate-finalized marker
//     (pki.CAFinalized). A node whose promotion stopped before the marker —
//     RotateCAMaterial+PromoteCA with the leaves never reissued, or a sign-new
//     whose restart failed — is NOT applied: its sign-new must resume
//     (pki.SignNewResumable, pki.FinishPromotion), and skipping it would strand
//     leaves the old CA signed and leave finalize refusing.
//   - finalize is never applied, like pki.EtcdPhaseApplied's drop-root: a
//     re-run re-verifies and re-carries the restart a node whose finalize
//     restart failed still owes.
//
// The orchestrator scopes that decision to the rotation it continues through
// phaseAppliedForRun; this unscoped form answers as it always has and is what
// TestSignNewIsSkippedWhenAlreadyApplied pins.
func caPhaseApplied(phase string) (bool, error) {
	switch phase {
	case PhaseTrustNew:
		if pki.CAFinalized() {
			return true, nil
		}
		rot := pki.ReadCARotation()
		// A staged root is never past trust-new: PromoteCA renames the staged
		// pair away, and a crash between its two renames leaves at most one of
		// them, which is not a staged pair. So a backup beside a staged root is
		// an earlier rollback's leftover, not this rotation's promotion — and
		// with the new rotation's bundle widened, the checks below would read it
		// as one.
		if rot.Staged {
			return false, nil
		}
		if !rot.Promoted {
			return false, nil
		}
		// FinalizeCA collapses the bundle before it writes its marker, and
		// the carotate-promoted marker it removes last covers that window.
		if pki.CAPromoted() {
			return true, nil
		}
		if rot.Err != nil {
			return false, rot.Err
		}
		return rot.BundleCAs > 1, nil
	case PhaseSignNew:
		return pki.CAPromoted() || pki.CAFinalized(), nil
	case PhaseFinalize:
		return false, nil
	default:
		return false, fmt.Errorf("certs: unknown rotate-ca phase %q (want %s|%s|%s)", phase, PhaseTrustNew, PhaseSignNew, PhaseFinalize)
	}
}

// phaseAppliedForRun is caPhaseApplied scoped to the rotation runID names: a
// trust-new or sign-new counts as applied only when the work on disk belongs
// to THAT rotation. The promotion proofs — the .pre-carotate backup and the
// carotate-promoted marker — are inherently scoped (finalize removes them with
// the rest of the rotation scratch), but the carotate-finalized marker is not:
// pki.FinalizeCA writes it and the scratch removal leaves it, so it survives a
// COMPLETED rotation, and read unscoped it would make the next rotation skip
// trust-new and sign-new on every control plane and report success without
// changing anything. The finalized leg therefore counts only when the
// finished-run marker this node's finalize wrote names runID. An empty runID —
// a manual phase without an active shared run — cannot treat a previous
// rotation as progress for the new one.
func phaseAppliedForRun(phase, runID string) (bool, error) {
	applied, err := caPhaseApplied(phase)
	if err != nil || !applied {
		return applied, err
	}
	if pki.CAFinalized() {
		return finalizedForRun(runID), nil
	}
	return true, nil
}

// finalizedForRun reports whether this node's finalized state belongs to the
// rotation runID names: the carotate-finalized marker is present and the
// finished-run marker records runID. An empty runID starts a new rotation.
func finalizedForRun(runID string) bool {
	if !pki.CAFinalized() {
		return false
	}
	if runID == "" {
		return false
	}
	return readCarotateFinishedRun() == runID
}

func cpTrustNew(w io.Writer, o CARotateOptions) error {
	// The applied consult first, before every guard: a node past trust-new skips
	// the phase instead of reaching the mid-rotation refusals, which would send a
	// resumed rotation back to --rollback as if a fresh rotation had been asked
	// for. Re-running trust-new past a promoted signer is also the one re-run
	// that would mint a second, divergent root (nothing is staged any more), so
	// the skip is what makes the re-run safe, not the guard. The decision is
	// scoped to the rotation the run continues: the orchestrator names it, and a
	// run that arrives without one (a bare --phase, a node Job) scopes by the
	// staged key the cluster holds — the key a live rotate-ca run keeps staged
	// names the rotation a finalized node must not re-rotate under.
	runID, err := o.phaseRunID()
	if err != nil {
		return fmt.Errorf("cannot determine active CA rotation: %w", err)
	}
	if applied, err := phaseAppliedForRun(PhaseTrustNew, runID); err != nil {
		return err
	} else if applied {
		castep(w, o, "trust-new is already applied on this node — skipping (the rotation resumes at sign-new)")
		return nil
	}
	// Refuse a direct trust-new that would stage a SECOND, divergent CA on a node
	// already mid-rotation: once sign-new promoted the previous ca-new
	// away, RotateCAMaterial below regenerates a fresh one, splitting the trust
	// domain — the one case the applied consult above does not cover is the
	// re-run it skips. The cluster orchestrator gates this up front
	// (RotateCACluster); the bare phase path did not. The follower path is
	// exempt — it legitimately arrives with the initiator's ca-new already
	// staged.
	node, err := host.Hostname()
	if err != nil {
		return err
	}
	if err := checkCARotationState(o, node); err != nil {
		return err
	}
	hostname, _, _, vip, err := hostNodeIP()
	if err != nil {
		return err
	}
	if o.FetchShared {
		// Before the fetch, which stages a root: a promoted node is refused with
		// nothing written, and an idle one drops the backups of an earlier rotation.
		if err := pki.PrepareStaging(o.Force); err != nil {
			return err
		}
		// Follower CP: pull the initiator's staged material so RotateCAMaterial
		// below reuses it (same new CA cluster-wide) instead of generating its own.
		castep(w, o, "Fetch staged CA material from the initiator")
		if err := fetchStagedMaterial(); err != nil {
			return fmt.Errorf("fetch staged CA material: %w", err)
		}
		missing, err := pki.StagedSignersMissing()
		if err != nil {
			return fmt.Errorf("check the fetched CA material: %w", err)
		}
		if len(missing) > 0 {
			return fmt.Errorf("certs: %s refused on this follower control plane: the initiator's staged CA material arrived without %s, "+
				"and minting it here would give this node a different new CA from the rest of the cluster, so none was minted; "+
				"re-run `stratum certs rotate-ca --cluster` from the initiator, or `stratum certs rotate-ca --rollback --yes` to abandon the rotation",
				PhaseTrustNew, strings.Join(missing, ", "))
		}
	}
	castep(w, o, "Stage new CA/SA + trust bundles (old‖new)")
	// Idempotent: generates fresh material only if not already staged (so a
	// follower CP that received ca-new.* from the initiator reuses the SAME CA);
	// always (re)writes the old‖new bundles.
	if err := pki.RotateCAMaterial(o.Force); err != nil {
		return err
	}
	if err := regenKubeconfigs(hostname, vip); err != nil {
		return err
	}
	castep(w, o, "Restart control plane (now trusts old‖new)")
	return trustNewRestartThen(w, o, func() error { return restartControlPlane(w) }, repinBestEffort)
}

// trustNewRestartThen restarts the control plane and then repins the cluster-info
// CA whether or not the restart succeeded, returning the restart's error. The
// staged material and old‖new bundles are already written by then, so a slow
// etcd gate must not also skip the repin.
func trustNewRestartThen(w io.Writer, o CARotateOptions, restart func() error, repin func(io.Writer, CARotateOptions)) error {
	err := restartThen(PhaseTrustNew, restart)
	repin(w, o)
	return err
}

func cpSignNew(w io.Writer, o CARotateOptions) error {
	// The applied consult first, before every refusal: a node whose sign-new is
	// recorded skips the phase instead of tripping the requires-staged-material
	// refusal below, which would leave a re-run `rotate-ca --cluster` with no
	// forward path. Scoped like cpTrustNew's, for the same reason.
	runID, err := o.phaseRunID()
	if err != nil {
		return fmt.Errorf("cannot determine active CA rotation: %w", err)
	}
	if applied, err := phaseAppliedForRun(PhaseSignNew, runID); err != nil {
		return err
	} else if applied {
		castep(w, o, "sign-new is already applied on this node — skipping (the rotation resumes at finalize)")
		return nil
	}
	resume := !pki.CARotationStaged() && pki.SignNewResumable()
	if !pki.CARotationStaged() && !resume {
		return fmt.Errorf("certs: %s requires staged material — run --phase=%s first, "+
			"or `stratum certs rotate-ca --rollback --yes` if a prior rotation was interrupted (see `stratum certs check`)",
			PhaseSignNew, PhaseTrustNew)
	}
	// Before anything is promoted: past this point the apiserver serves the new CA.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := signNewClientGate(ctx, o); err != nil {
		return fmt.Errorf("sign-new refused: %w", err)
	}
	hostname, nodeIP, apiExtra, vip, err := hostNodeIP()
	if err != nil {
		return fmt.Errorf("sign-new refused: %w", err)
	}
	if resume {
		castep(w, o, "Finish a sign-new that promoted the new CA and stopped")
		if err := pki.FinishPromotion(); err != nil {
			return err
		}
	} else {
		castep(w, o, "Promote the new CA/SA to the active signer")
		if err := pki.PromoteCA(); err != nil {
			return err
		}
		if err := pki.PromoteSA(); err != nil {
			return err
		}
	}
	recordCarotateRun(w)
	castep(w, o, "Reissue all leaves under the new CA")
	if err := pki.RotateLeaves(hostname, nodeIP, apiExtra); err != nil {
		return err
	}
	if err := regenKubeconfigs(hostname, vip); err != nil {
		return err
	}
	castep(w, o, "Restart control plane (serve new-signed certs)")
	return signNewRestartThen(func() error { return restartControlPlane(w) })
}

// signNewRestartThen restarts the control plane and records the promotion only
// if the restart succeeded: until then a static pod may still serve a leaf the
// old CA signed, and finalize would drop that CA. After a failed restart the
// node is promoted and unmarked, which finalize refuses; running sign-new
// again finishes it (pki.SignNewResumable), and a rollback is the other way
// out unless the rotation was forced.
func signNewRestartThen(restart func() error) error {
	if err := restartThen(PhaseSignNew, restart); err != nil {
		return err
	}
	return pki.MarkCAPromoted()
}

// checkCPFinalize refuses a finalize on a node whose sign-new never promoted the
// new signer: FinalizeCA would collapse trust to the OLD CA and delete the staged
// new one, abandoning the rotation with every step exiting 0. Leftover *-new
// files refuse too, complete or not: a node that crashed mid-staging looks idle
// to CARotationStaged, and finalize would delete what was staged. A node that
// already finalized (pki.CAFinalized) passes, so a finalize whose restart failed
// can be re-run; a node that never rotated has no such record and does not.
func checkCPFinalize() error {
	if left := pki.CAStagedFiles(); len(left) > 0 {
		return fmt.Errorf("certs: %s refused: new CA material is still staged (%s), so this node's sign-new did not complete — "+
			"run --phase=%s first, or `stratum certs rotate-ca --rollback --yes` to abandon the rotation (see `stratum certs check`)",
			PhaseFinalize, strings.Join(left, ", "), PhaseSignNew)
	}
	// Not ca.pre-carotate.crt: one can outlive a rollback. A node promoted by a release that wrote no marker is refused; it
	// rolls back and rotates again.
	if pki.CAPromoted() {
		return nil
	}
	if n, err := pki.CABundleCertCount("ca"); err == nil && n == 1 && pki.CAFinalized() {
		return nil
	}
	return fmt.Errorf("certs: %s refused: this node's sign-new never promoted a new CA, so finalize would collapse trust to the current CA and discard the rotation — "+
		"run --phase=%s first, or `stratum certs rotate-ca --rollback --yes` to abandon it (see `stratum certs check`)",
		PhaseFinalize, PhaseSignNew)
}

func cpFinalize(w io.Writer, o CARotateOptions) error {
	runID := o.RunID
	if runID == "" {
		runID = readCarotateRun()
	}
	if runID == "" {
		runID = readCarotateFinishedRun()
	}
	if err := checkCPFinalize(); err != nil {
		return err
	}
	// Before anything is dropped: past this point the apiserver rejects the old key.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := localTokenGate(ctx, w, o); err != nil {
		return fmt.Errorf("finalize refused: %w", err)
	}
	if err := cpRenewServingCert(w, o); err != nil {
		return fmt.Errorf("finalize refused: %w", err)
	}
	hostname, _, _, vip, err := hostNodeIP()
	if err != nil {
		return err
	}
	// The first check ran before the token gate and the serving-cert renewal,
	// which can take minutes; re-check so FinalizeCA never collapses trust on
	// state that changed since (a rollback, a restaged rotation).
	if err := checkCPFinalize(); err != nil {
		return err
	}
	castep(w, o, "Collapse trust to the new CA only")
	if err := pki.FinalizeCA(); err != nil {
		return err
	}
	// Record which rotation this finalize finished, so the applied-phase
	// decision can scope the finalized marker to it: the marker survives the
	// rotation, and the next one must not read it as its own progress.
	markCarotateFinishedRun(w, runID)
	if err := regenKubeconfigs(hostname, vip); err != nil {
		return err
	}
	castep(w, o, "Restart control plane (drop old trust)")
	return restartThenCleanup(w, o, PhaseFinalize, func() error { return restartControlPlane(w) }, removeLeftoverCarotateKey, repinBestEffort)
}

// cpRenewServingCert renews a control plane's bootstrap-issued kubelet serving
// cert (a profile that declares bootstrap serving certs) before finalize narrows the bundle the apiservers
// verify it against. It runs here rather than at sign-new because only once
// every control plane has promoted does every controller-manager sign with the
// new CA. A kubelet loads its serving cert only as it starts, so whenever the
// running kubelet does not already serve a new-CA cert it is restarted here,
// before trust narrows: after a renewal, and also on a retry whose renewal
// already happened but whose restart failed. A failed restart refuses finalize.
func cpRenewServingCert(w io.Writer, o CARotateOptions) error {
	bootstraps, err := kubeletBootstrapsServing()
	if err != nil || !bootstraps {
		return err
	}
	args, err := kubeletArgs()
	if err != nil {
		return err
	}
	bundlePath := filepath.Join(version.PKIDir, "ca-bundle.crt")
	pemBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		return err
	}
	anchor, err := parseTrustBundle("on-disk trust bundle "+bundlePath, pemBytes)
	if err != nil {
		return err
	}
	renewed, err := renewServingCert(w, o, PhaseFinalize, filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig"),
		filepath.Join(pki.KubeletCertDir(args), pki.KubeletServingCurrent), anchor)
	if err != nil {
		return err
	}
	newCA, err := newestCA(anchor)
	if err != nil {
		return err
	}
	if !renewed {
		if localKubeletServes(newCA) == nil {
			return nil
		}
		castep(w, o, "Restart kubelet (it still serves a cert from before the renewal)")
	} else {
		castep(w, o, "Restart kubelet (serve the renewed cert before trust narrows)")
	}
	if err := restartKubelet(); err != nil {
		return fmt.Errorf("restart the kubelet onto its renewed serving cert: %w", err)
	}
	return waitLocalKubeletServes(newCA)
}

// kubeletServesWait bounds how long finalize waits for a restarted kubelet to
// serve a cert newCA signed; kubeletServesPoll paces the probes.
var (
	kubeletServesWait = 60 * time.Second
	kubeletServesPoll = 2 * time.Second
)

// waitLocalKubeletServes probes the restarted kubelet until it serves a cert
// newCA signed. A restart only proves a new kubelet process exists, and the
// cluster preflight skips the control planes of such a profile, so this is the one check that
// their kubelet serves the new-CA cert before trust narrows.
func waitLocalKubeletServes(newCA *x509.Certificate) error {
	deadline := time.Now().Add(kubeletServesWait)
	for {
		err := localKubeletServes(newCA)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the restarted kubelet does not serve a cert the new CA (%s) signed after %s (%w), so finalize would break kubectl logs/exec to this node — "+
				"check its serving cert names this node's IP and that the kubelet is reachable there, then retry finalize",
				describe([]*x509.Certificate{newCA}), kubeletServesWait, err)
		}
		time.Sleep(kubeletServesPoll)
	}
}

// localKubeletServes probes this node's own kubelet at its node IP and
// returns nil only when it serves a cert newCA signed. A var so a test can
// stand in for the running kubelet.
var localKubeletServes = func(newCA *x509.Certificate) error {
	_, ips, _, _, err := hostNodeIP()
	if err != nil {
		return err
	}
	ip, _, _ := strings.Cut(ips, ",")
	res := pki.ProbeKubelets(context.Background(), []pki.KubeletTarget{{IP: ip}}, []*x509.Certificate{newCA}, probeKubelet)
	return res[0]
}

// restartThenCleanup restarts the control plane and then runs the leftover-key
// cleanup and the cluster-info repin whether or not the restart succeeded,
// returning the restart's error. The phase's files are already written by then, so
// a slow etcd gate must not also skip removing the leftover key or republishing the
// pinned CA. The cleanup goes first: it is bounded by its own
// deadline, while the repin makes API calls with no deadline of their own, and a
// stalled one must not keep the key that decrypts the CA private keys from being
// removed. Finalize and rollback both end a rotation, so both use it.
func restartThenCleanup(w io.Writer, o CARotateOptions, phase string, restart func() error, cleanup func(io.Writer), repin func(io.Writer, CARotateOptions)) error {
	err := restartThen(phase, restart)
	cleanup(w)
	repin(w, o)
	return err
}

// restartThen runs a phase's control-plane restart and, when it fails, says the
// phase's files are already written on this node, so the failure is not a
// failure to rotate. It names no recovery command: whether finishing,
// resuming or rolling back is safe depends on the other nodes, which a node-local
// phase cannot see; RotateCACluster adds that guidance (haltedAt).
func restartThen(phase string, restart func() error) error {
	err := restart()
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w\n  %s: files are written on this node but the restart did not finish", err, phase)
}

// ── worker phases ────────────────────────────────────────────────────────────

// workerRefreshTrust (trust-new, finalize, and RollbackCA) pulls the authoritative CA trust
// bundle from cluster-info — which the control planes republished earlier in this
// same phase (the orchestrator runs CPs before workers) — and applies it to BOTH
// sides of the worker's trust:
//   - ca-bundle.crt and the kubelet's clientCAFile (kubelet-client-ca-bundle.crt):
//     what the kubelet uses to verify INCOMING clients (e.g. apiserver → kubelet
//     for logs/exec);
//   - the CA embedded in kubelet.kubeconfig + bootstrap-kubeconfig: how the kubelet
//     verifies the apiserver's OUTBOUND serving cert. This is the one that matters
//     most — miss it and the kubelet can't talk to a new-CA apiserver → NotReady.
//
// Then restarts kubelet so it adopts the refreshed trust.
func workerRefreshTrust(w io.Writer, o CARotateOptions, intent trustIntent) (err error) {
	p := workerTrustPaths()
	castep(w, o, "Refresh CA trust bundle from cluster-info")
	server, serr := refreshServer(p)
	if serr != nil {
		return serr
	}
	// Everything that can fail happens before the first mutation, so a refused
	// refresh leaves all five files byte-identical — stated once here rather than
	// in each predicate, so it covers the transport and structural refusals too.
	u, perr := planTrustRefresh(server, intent, p)
	if perr != nil {
		return fmt.Errorf("worker %s: %w — the on-disk bundle and kubeconfigs are unchanged", intent, perr)
	}
	// A cleanup failure leaves credential-bearing temp files behind, so it joins
	// the returned error rather than disappearing. After a successful commit there
	// is nothing staged and this is a no-op.
	defer func() {
		if cerr := u.abort(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("worker %s: staged trust files remain on disk and must be removed by hand: %w", intent, cerr))
		}
	}()
	castep(w, o, "Apply refreshed trust: "+u.summary())
	// Narrowing drops the old CA from the apiserver's client-ca too, so a kubelet
	// whose client cert it still signed would be locked out.
	if intent == trustNarrow {
		if err := checkClientCertSurvives(u.dropped, kubeletClientCertPath()); err != nil {
			return fmt.Errorf("worker %s refused: %w — the on-disk bundle and kubeconfigs are unchanged", intent, err)
		}
		if err := checkServingCertSurvives(u.dropped, kubeletServingCertPath()); err != nil {
			return fmt.Errorf("worker %s refused: %w — the on-disk bundle and kubeconfigs are unchanged", intent, err)
		}
	}
	if err := u.commit(); err != nil {
		return fmt.Errorf("worker %s: %w", intent, err)
	}
	castep(w, o, "Restart kubelet (adopt refreshed trust)")
	return comps.Restart("kubelet")
}

// workerReissueClientCert (sign-new) gives the kubelet a client cert signed by
// the NEW CA before finalize drops the old one from the apiservers' client-ca.
// It renews through a selfnodeclient CSR authenticated by the current cert, so
// it needs no bootstrap token, and swaps the new cert in only once it is issued:
// a failed renewal leaves the working cert in place. The serving cert follows
// the same way, since the apiservers verify it against the bundle finalize
// narrows.
func workerReissueClientCert(w io.Writer, o CARotateOptions) error {
	p := workerTrustPaths()
	if err := checkWorkerTrustWidened(p); err != nil {
		return err
	}
	if err := checkWorkerReissue(); err != nil {
		return fmt.Errorf("cannot safely renew kubelet client certificate: %w", err)
	}
	pemBytes, source, err := readTrustAnchor(p)
	if err != nil {
		return err
	}
	anchor, err := parseTrustBundle("on-disk trust bundle "+source, pemBytes)
	if err != nil {
		return err
	}
	castep(w, o, "Renew kubelet client cert under the new CA")
	ctx, cancel := context.WithTimeout(context.Background(), kubeletRenewTimeout)
	defer cancel()
	certPath := kubeletClientCertPath()
	renewed, err := renewKubeletClientCert(ctx, certPath, anchor, kubeletRenewOps{
		request: requestKubeletClientCert(p.kubeletKubeconfig),
		store:   storeKubeletClientCert(certPath),
	})
	if errors.Is(err, errNoKubeletClientCert) {
		return fmt.Errorf("worker %s: %w, and %s renews with it, so it cannot repair this worker — %s", PhaseSignNew, err, PhaseSignNew, rejoinWorker)
	}
	if err != nil {
		return fmt.Errorf("worker %s: %w — the kubelet keeps its current client cert; fix the cause and run --phase=%s on this worker again",
			PhaseSignNew, err, PhaseSignNew)
	}
	if !renewed {
		castep(w, o, "The kubelet client cert is already signed by the new CA; nothing to renew")
	}
	_, serr := renewServingCert(w, o, PhaseSignNew, p.kubeletKubeconfig, kubeletServingCertPath(), anchor)
	if serr != nil {
		serr = fmt.Errorf("worker %s: %w", PhaseSignNew, serr)
	}
	castep(w, o, "Restart kubelet (adopt the renewed certs)")
	return errors.Join(serr, comps.Restart("kubelet"))
}

// checkWorkerTrustWidened refuses sign-new on a worker whose trust-new did not
// finish: sign-new renews the kubelet client cert through a CSR to the
// apiserver and then restarts the kubelet, so a kubelet that cannot verify a
// new-CA apiserver on any of its trust inputs would be stranded. A
// trust refresh commits its five files one by one and may stop partway, so the
// bundle holding two CAs is not enough; the kubelet kubeconfig must hold those
// same CAs, and so must the kubelet's clientCAFile and the bootstrap-kubeconfig
// when the worker has them. Counting is not enough for those: one a stopped
// finalize left on {previous, current} still holds two CAs after the next
// trust-new widens the bundle to {current, next}. An absent bootstrap-kubeconfig
// is skipped, as trust-new skips it; an absent clientCAFile means a kubelet
// still launched on ca-bundle.crt, and a split one is not checked.
func checkWorkerTrustWidened(p trustPaths) error {
	pemBytes, source, err := readTrustAnchor(p)
	if err != nil {
		return err
	}
	anchor, err := checkTwoCAs("on-disk trust bundle "+source, pemBytes)
	if err != nil {
		return err
	}
	split, err := pki.KubeletClientSplitPEM(p.kubeletClientCA, pemBytes, source)
	if err != nil {
		return fmt.Errorf("certs: %s refused: tell whether %s follows the cluster root: %w", PhaseSignNew, p.kubeletClientCA, err)
	}
	if fileExists(p.kubeletClientCA) && !split {
		b, err := os.ReadFile(p.kubeletClientCA)
		if err != nil {
			return fmt.Errorf("certs: %s refused: read %s: %w", PhaseSignNew, p.kubeletClientCA, err)
		}
		if err := checkSameCAs("kubelet client-CA bundle "+p.kubeletClientCA, b, source, anchor); err != nil {
			return err
		}
	}
	for _, kc := range []string{p.kubeletKubeconfig, p.bootstrapKubeconfig} {
		if kc == p.bootstrapKubeconfig && !fileExists(kc) {
			continue
		}
		cfg, err := clientcmd.LoadFromFile(kc)
		if err != nil {
			return fmt.Errorf("certs: %s refused: read %s: %w", PhaseSignNew, kc, err)
		}
		if len(cfg.Clusters) == 0 {
			return fmt.Errorf("certs: %s refused: %s has no cluster entry", PhaseSignNew, kc)
		}
		for name, c := range cfg.Clusters {
			if err := checkSameCAs(fmt.Sprintf("CA of cluster %q in %s", name, kc), c.CertificateAuthorityData, source, anchor); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkTwoCAs(what string, pemBytes []byte) ([]*x509.Certificate, error) {
	cas, err := parseTrustBundle(what, pemBytes)
	if err != nil {
		return nil, fmt.Errorf("certs: %s refused: %w", PhaseSignNew, err)
	}
	cas = dedupe(cas)
	if n := len(cas); n < 2 {
		return nil, fmt.Errorf("certs: %s refused: the %s holds %d CA, so this worker's trust-new has not finished — %s",
			PhaseSignNew, what, n, trustNewRecovery)
	}
	return cas, nil
}

func checkSameCAs(what string, pemBytes []byte, anchorSource string, anchor []*x509.Certificate) error {
	cas, err := checkTwoCAs(what, pemBytes)
	if err != nil {
		return err
	}
	want := fingerprints(anchor)
	if len(cas) != len(want) || len(notIn(cas, want)) > 0 {
		return fmt.Errorf("certs: %s refused: the %s holds [%s] where %s holds [%s], so this worker's trust-new has not finished — %s",
			PhaseSignNew, what, describe(cas), anchorSource, describe(anchor), trustNewRecovery)
	}
	return nil
}

var trustNewRecovery = fmt.Sprintf("run --phase=%s on it again, "+
	"or `stratum certs rotate-ca --rollback --yes` if a prior rotation was interrupted (see `stratum certs check`)", PhaseTrustNew)

// checkClientCertSurvives refuses when the kubelet client cert at path was
// signed by one of the CAs a finalize is about to drop, meaning this worker's
// sign-new never completed, or when there is no client cert, which sign-new
// cannot repair. With nothing to drop, finalize
// cannot break the kubelet's client auth, so there is nothing to guard: a
// finalized worker on a pre-upgrade launch has no rotating cert and must still
// be able to re-run it.
func checkClientCertSurvives(dropped []*x509.Certificate, path string) error {
	if len(dropped) == 0 {
		return nil
	}
	leaf, err := readKubeletClientLeaf(path)
	if errors.Is(err, errNoKubeletClientCert) {
		return fmt.Errorf("%w, so the kubelet cannot authenticate and %s cannot renew it — %s", err, PhaseSignNew, rejoinWorker)
	}
	if err != nil {
		return fmt.Errorf("%w — run --phase=%s first, or `stratum certs rotate-ca --rollback --yes`", err, PhaseSignNew)
	}
	for _, ca := range dropped {
		if leaf.CheckSignatureFrom(ca) == nil {
			return fmt.Errorf("the kubelet client cert %s is still signed by a CA this finalize drops (%s), so the kubelet would be locked out — "+
				"run --phase=%s first, or `stratum certs rotate-ca --rollback --yes`", path, describe([]*x509.Certificate{ca}), PhaseSignNew)
		}
	}
	return nil
}

const kubeletClientCurrentPEM = "kubelet-client-current.pem"

// Inspect the running process, not an argv audit file or newly rendered flags:
// the current supervisor may still be using a pre-upgrade launch configuration.
func checkWorkerReissue() error {
	args, err := runningKubeletArgs()
	if err != nil {
		return err
	}
	return checkWorkerReissueConfig(args, workerTrustPaths(), kubeletClientCertPath())
}

// kubeletArgs is runningKubeletArgs, a var so a test can drive the finalize
// renewal without a supervisor.
var kubeletArgs = runningKubeletArgs

func runningKubeletArgs() ([]string, error) {
	procs, err := supervisor.Dial(version.ControlSocket, "status", "")
	if err != nil {
		return nil, fmt.Errorf("query kubelet supervisor: %w", err)
	}
	for _, proc := range procs {
		if proc.Name != "kubelet" || !proc.Running || proc.Pid <= 0 {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", proc.Pid))
		if err != nil {
			return nil, fmt.Errorf("read running kubelet arguments: %w", err)
		}
		return strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00"), nil
	}
	return nil, fmt.Errorf("no running supervised kubelet; start Stratum before retrying")
}

func checkWorkerReissueConfig(args []string, paths trustPaths, certPath string) error {
	// Accept both flag spellings, but reject duplicates rather than guessing
	// which value the running kubelet selected.
	for flag, want := range map[string]string{
		"--cert-dir":   filepath.Dir(certPath),
		"--kubeconfig": paths.kubeletKubeconfig,
	} {
		var values []string
		for i, arg := range args {
			if value, ok := strings.CutPrefix(arg, flag+"="); ok {
				values = append(values, value)
			} else if arg == flag && i+1 < len(args) {
				values = append(values, args[i+1])
			}
		}
		if len(values) != 1 || values[0] != want {
			return fmt.Errorf("running kubelet must use %s=%s; restart Stratum with the current worker configuration before retrying sign-new", flag, want)
		}
	}
	config, err := clientcmd.LoadFromFile(paths.kubeletKubeconfig)
	if err != nil {
		return fmt.Errorf("read kubelet kubeconfig: %w", err)
	}
	context := config.Contexts[config.CurrentContext]
	if context == nil {
		return fmt.Errorf("kubelet kubeconfig has no active context")
	}
	auth := config.AuthInfos[context.AuthInfo]
	resolve := func(path string) string {
		if path != "" && !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(paths.kubeletKubeconfig), path)
		}
		return filepath.Clean(path)
	}
	if auth == nil || len(auth.ClientCertificateData) != 0 || len(auth.ClientKeyData) != 0 ||
		resolve(auth.ClientCertificate) != certPath || resolve(auth.ClientKey) != certPath ||
		auth.Token != "" || auth.TokenFile != "" || auth.Exec != nil || auth.AuthProvider != nil || auth.Username != "" || auth.Password != "" {
		return fmt.Errorf("kubelet kubeconfig must reference %s for both client certificate and key; refusing to renew a credential the running kubelet does not use", certPath)
	}
	return nil
}

func kubeletClientCertPath() string {
	// Match the --cert-dir configured by cluster.kubeletProcess for TLS bootstrap.
	return filepath.Join(version.PKIDir, "kubelet", kubeletClientCurrentPEM)
}

// ── shared helpers ───────────────────────────────────────────────────────────

func regenKubeconfigs(hostname, vip string) error {
	// Preserve the current endpoint (LB :7445 vs :6443) like leaf rotation does.
	server, err := kubeconfig.CurrentServer(version.AdminConf)
	if err != nil {
		server = fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)
	}
	return writeKubeconfigs(hostname, server, vip)
}

// repinBestEffort republishes cluster-info with the current trust bundle. It is
// cluster-wide and idempotent; a transient failure (e.g. apiserver mid-restart on
// another CP) is non-fatal here because the orchestrator also repins. Bounded by
// cleanupContext, like the other best-effort calls a phase ends with, so an
// unanswered request cannot stop the phase from returning.
func repinBestEffort(w io.Writer, o CARotateOptions) {
	err := func() error {
		cs, err := adminClientset()
		if err != nil {
			return err
		}
		ctx, cancel := cleanupContext()
		defer cancel()
		return token.RepinCAWith(ctx, cs)
	}()
	if err != nil && !o.Quiet {
		fmt.Fprintf(w, "  WARN: republish cluster-info (RepinCA): %v (will be retried)\n", err)
	}
}

// removeLeftoverCarotateKey reaps the stratum-carotate Secret an earlier failed
// `rotate-ca --cluster` stranded. Both finalize and rollback end in the idle
// state, where the Secret has no reader left, yet it still holds the key to
// ca.key/sa.key/front-proxy-ca.key. Best-effort like repinBestEffort:
// the cleanup retries and prints the manual command on failure, and an absent
// Secret is a no-op. Bounded by cleanupContext so an unanswered request cannot
// stop finalize or rollback from returning.
func removeLeftoverCarotateKey(w io.Writer) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		fmt.Fprintf(w, "  WARN: cannot check for a leftover %s/%s key Secret: %v\n"+keyHoldsCAKeys+
			"        If present and no rotation or etcd-CA migration is active, remove it by hand:\n\n          kubectl -n %s delete secret %s\n",
			caNamespace, caRotateKeySecret, err, caNamespace, caNamespace, caRotateKeySecret)
		return
	}
	ctx, cancel := cleanupContext()
	defer cancel()
	reapLeftoverCarotateKey(w, ctx, cs)
}

// keyHoldsCAKeys is the line every message about a stratum-carotate Secret that is
// still in the cluster carries: it holds the key to the CA private keys, which is
// why leaving it is never silent. It takes the namespace as its one argument.
const keyHoldsCAKeys = "        It holds the AES key that decrypts the shared cluster CA material (ca.key, sa.key,\n" +
	"        front-proxy-ca.key); until it is gone, anyone with `get secrets` in %s can decrypt them.\n"

// carotateRunMarker is the node-local record of WHICH rotate-ca run's key this
// node promoted its signer under. It lives in PKIDir but outside carotateScratch,
// so finalize and rollback do not remove it: a retry after a failed finalize,
// restart or delete must still be able to prove the key is its own. It is
// removed only once that key is known gone.
func carotateRunMarker() string { return filepath.Join(version.PKIDir, "carotate-run-id") }

func readCarotateRun() string {
	b, err := os.ReadFile(carotateRunMarker())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func clearCarotateRun(w io.Writer) {
	if err := os.Remove(carotateRunMarker()); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(w, "  WARN: could not remove %s: %v\n", carotateRunMarker(), err)
	}
}

// carotateFinishedRunMarker is the node-local record of WHICH rotate-ca run's
// rotation this node's finalize finished. It is what scopes the applied-phase
// decision's finalized leg to the rotation it finished: the pki
// carotate-finalized marker survives a completed rotation (nothing in the
// scratch removal takes it), so read unscoped it makes the NEXT rotation skip
// trust-new and sign-new on this node and report success without rotating. The
// marker is written by cpFinalize, cleared by the --cluster orchestrator when
// the run completes, and left alone by finalize and rollback otherwise: a
// retried finalize must still find the evidence it wrote.
func carotateFinishedRunMarker() string {
	return filepath.Join(version.PKIDir, "carotate-finished-run")
}

func readCarotateFinishedRun() string {
	b, err := os.ReadFile(carotateFinishedRunMarker())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// markCarotateFinishedRun records the finished rotation's identity: the run
// the phase options name, or — a finalize that arrives without the
// orchestrator's identity, a node Job — the run this node recorded at its
// sign-new, which is the same rotation. Best-effort, like recordCarotateRun:
// the node IS finalized on disk, and failing the phase now would misreport a
// finished finalize.
func markCarotateFinishedRun(w io.Writer, runID string) {
	if runID == "" {
		runID = readCarotateRun()
	}
	if runID == "" {
		return
	}
	if err := atomicfile.WriteFile(carotateFinishedRunMarker(), []byte(runID+"\n"), 0o600); err != nil {
		fmt.Fprintf(w, "  WARN: could not record the finished CA rotation's identity on this node: %v "+
			"(a later rotate-ca --cluster may re-run phases this rotation already applied)\n", err)
	}
}

func clearCarotateFinishedRun(w io.Writer) {
	if err := os.Remove(carotateFinishedRunMarker()); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(w, "  WARN: could not remove %s: %v\n", carotateFinishedRunMarker(), err)
	}
}

// recordCarotateRun runs at sign-new, once the signer is promoted. Under the
// phase-major order RotateCACluster enforces, every control plane has fetched the
// run's key before any node reaches sign-new, so from here that key has no reader
// left. It records the run id of the key staged right now; no readable, annotated
// key (a manual phase run, an unreachable apiserver) clears any older marker and
// leaves this node with no authority to delete anything. Never fails sign-new.
func recordCarotateRun(w io.Writer) {
	cs, err := adminClientset()
	if err != nil {
		clearCarotateRun(w)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	recordCarotateRunFrom(w, ctx, cs)
}

func recordCarotateRunFrom(w io.Writer, ctx context.Context, cs kubernetes.Interface) {
	sec, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, caRotateKeySecret, metav1.GetOptions{})
	run := ""
	// Only a key this operation staged is recorded: the owner type is what
	// keeps a migrate-etcd-ca key's run annotation (a digest of its certificate key) from
	// being read as the run this node promoted under, which would hand this
	// node's recovery authority over another operation's live key.
	if err == nil && stagedByRotation(sec) {
		run = sec.Annotations[carotateRunAnnotation]
	}
	if run == "" {
		clearCarotateRun(w)
		return
	}
	if err := os.WriteFile(carotateRunMarker(), []byte(run+"\n"), 0o600); err != nil {
		fmt.Fprintf(w, "  WARN: could not record the rotate-ca run on this node: %v (a leftover key will not be removed automatically)\n", err)
	}
}

// reapLeftoverCarotateKey is removeLeftoverCarotateKey with the client injected.
// A rotate-ca finalize or rollback can run on this node while another run is
// live, and that run's followers still read the Secret: migrate-etcd-ca's in
// trust-both, a LATER rotate-ca --cluster's in trust-new (fetchStagedMaterial).
// So recovery removes a key only when it can prove that exact key is spent:
//
//  1. read the key and keep that snapshot — absent means it is already gone, so
//     the node's marker is spent too;
//  2. the snapshot must carry the run id this node recorded when it promoted
//     its signer (recordCarotateRun). No marker, no run annotation of this
//     run's id (a migrate-etcd-ca key names its run by a digest of its certificate key), or
//     a different run's id means leave it, and warn;
//  3. read the migration lock — held, or unreadable, means leave the key;
//  4. delete exactly the snapshot (UID + resourceVersion preconditions on every
//     retry), so a key replaced after step 1 makes the delete a Conflict, which
//     is left alone.
//
// The marker is cleared only when the key is known gone — absent at step 1, or
// deleted — so a retried finalize or rollback after any failure still has it.
// The cost of skipping a true leftover is one manual command; the cost of
// deleting a live key is a failed rotation.
func reapLeftoverCarotateKey(w io.Writer, ctx context.Context, cs kubernetes.Interface) {
	_ = waitAPIServer(ctx, cs) // gate before reading, never between the checks and the delete
	run := readCarotateRun()
	snap, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, caRotateKeySecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		clearCarotateRun(w)
		return
	case err != nil:
		fmt.Fprintf(w, "  WARN: cannot read %s/%s to check for a leftover key (%v).\n"+keyHoldsCAKeys+
			"        If it exists and no rotation or etcd-CA migration is active, remove it by hand:\n\n          kubectl -n %s delete secret %s\n",
			caNamespace, caRotateKeySecret, err, caNamespace, caNamespace, caRotateKeySecret)
		return
	}
	if run == "" || snap.Annotations[carotateRunAnnotation] != run {
		if run != "" {
			clearCarotateRun(w) // the key this node promoted under is gone; what is there now is someone else's
		}
		fmt.Fprintf(w, "  WARN: leaving %s/%s in place: it is not the key of the rotate-ca run this node recorded, so another\n"+
			"        rotation or migration may still be fetching it; this recovery does not hold that run's cluster lease.\n"+keyHoldsCAKeys+
			"        If none is running, remove it by hand:\n\n          kubectl -n %s delete secret %s\n",
			caNamespace, caRotateKeySecret, caNamespace, caNamespace, caRotateKeySecret)
		return
	}
	lock, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, etcdMigrationLockSecret, metav1.GetOptions{})
	switch {
	case err == nil:
		fmt.Fprintf(w, "  WARN: leaving %s/%s in place: an etcd-CA migration holds %s (holder=%s since=%s) and its followers still read it.\n"+keyHoldsCAKeys+
			"        If that run was killed, remove the lock, then the key:\n\n          kubectl -n %s delete secret %s %s\n",
			caNamespace, caRotateKeySecret, etcdMigrationLockSecret, lock.Data["holder"], lock.Data["since"],
			caNamespace, caNamespace, etcdMigrationLockSecret, caRotateKeySecret)
		return
	case !apierrors.IsNotFound(err):
		fmt.Fprintf(w, "  WARN: leaving %s/%s in place: cannot confirm no etcd-CA migration is active (%v).\n"+keyHoldsCAKeys+
			"        If none is, remove it by hand:\n\n          kubectl -n %s delete secret %s\n",
			caNamespace, caRotateKeySecret, err, caNamespace, caNamespace, caRotateKeySecret)
		return
	}
	if cleanupCarotateKeySnapshot(ctx, w, cs, snap) {
		clearCarotateRun(w)
	}
}

// signerPromoted reports whether this node reached sign-new: pki.ReadCARotation
// sees the promotion backup pki.PromoteCA writes, which finalize or a completed
// rollback removes.
func signerPromoted() bool {
	return pki.ReadCARotation().Promoted
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// RollbackCA reverts a partial rotation on THIS node. It restores the pre-rotation
// CA/SA + bundles (pki.RollbackCA); if the signer had already been promoted
// (sign-new reached, detected via the .pre-carotate backup), it also reissues the
// leaves under the restored old CA so the node is internally consistent, then
// restarts. Workers hold no CA — they just refresh trust from cluster-info.
func RollbackCA(w io.Writer, o CARotateOptions) error {
	// The node maintenance lock first, before the worker/CP split: a rollback
	// re-writes CA material and restarts the control plane, the same damage a
	// forward rotation does, so it fences like one. The forward entry
	// (RotateCA) and the upgrade's rollback already take it; this is the one
	// certs entry that would otherwise run beside a live upgrade unfenced. No
	// caller holds the lock on this path — it is a top-level CLI entry — so
	// there is no held-lock capability here.
	release, err := acquireNodeLock("certs rotate-ca --rollback", false)
	if err != nil {
		return err
	}
	defer release()
	if err := RequireNodeProfile("rotate-ca --rollback"); err != nil {
		return err
	}

	if isWorker() {
		if err := workerRollbackCheck(); err != nil {
			return err
		}
		// trustEither, not a Phase lookup: `--rollback --phase=finalize` is accepted
		// by the CLI, and a rollback legitimately re-widens as well as narrows.
		return workerRefreshTrust(w, o, trustEither)
	}
	if err := rollbackCPMaterial(w, o); err != nil {
		return err
	}
	castep(w, o, "Restart control plane")
	// A rollback is the way out the halted-run advice points to, and a halted
	// --cluster run can leave stratum-carotate behind with the key to the CA
	// private keys; the old CA is live again, so reap it here too.
	return restartThenCleanup(w, o, "rollback", func() error { return restartControlPlane(w) }, removeLeftoverCarotateKey, repinBestEffort)
}

// rollbackCPMaterial is RollbackCA's on-disk half for a controller, apart from
// the restarts so it can be tested. The kubeconfigs are rewritten on every
// rollback, not only after a promotion: trust-new already widened them to
// old‖new, and pki.RollbackCA collapsing the bundle does not reach the copies
// they embed, so they would keep trusting the CA the rollback discarded —
// kube-vip's too, now that trust-new rewrites it.
func rollbackCPMaterial(w io.Writer, o CARotateOptions) error {
	promoted := signerPromoted()
	hostname, nodeIP, apiExtra, vip, err := hostNodeIP()
	if err != nil {
		return err
	}
	castep(w, o, "Restore pre-rotation CA + trust bundles")
	if err := pki.RollbackCA(); err != nil {
		return err
	}
	// The rollback unwinds the rotation this node's finalize recorded, so the
	// finished-run record goes with it.
	clearCarotateFinishedRun(w)
	if promoted {
		castep(w, o, "Reissue leaves under the restored CA")
		if err := pki.RotateLeaves(hostname, nodeIP, apiExtra); err != nil {
			return err
		}
	}
	if err := regenKubeconfigs(hostname, vip); err != nil {
		return err
	}
	// Last: the backups are what tells a retry that the leaves still need
	// reissuing, so they go only once the leaves and kubeconfigs are repaired.
	return pki.RemoveRotationScratch()
}

// RotationPhase describes this node's CA-rotation state for `stratum certs check`.
func RotationPhase() string {
	rot := pki.ReadCARotation()
	if rot.Staged {
		return "trust-new (new CA staged, signer not yet promoted — run sign-new)"
	}
	if rot.Err == nil && rot.BundleCAs > 1 {
		return "sign-new (new signer active, old CA still trusted — run finalize)"
	}
	return "idle"
}

// RotationDetail is what the halt messages send an operator to `certs check`
// for: whether the rotation was forced, which forbids a rollback, whether a
// staged pair signs in place of a lost key, for each of the root CA, the
// service-account key and the front-proxy CA, and whether this node can roll
// back. Empty when no rotation is open.
func RotationDetail() []string {
	if RotationPhase() == "idle" && !pki.ForcedRotation() {
		return nil
	}
	forced := "forced: no"
	if pki.ForcedRotation() {
		forced = "forced: yes (rollback refused; finish it forward)"
	}
	ca, sa, fp := pki.Signer("ca"), pki.Signer("sa"), pki.Signer("front-proxy-ca")
	signer := fmt.Sprintf("signer: %s/%s/%s (live)", ca, sa, fp)
	if ca != "ca" || sa != "sa" || fp != "front-proxy-ca" {
		signer = fmt.Sprintf("signer: %s/%s/%s (staged, in place of a lost key)", ca, sa, fp)
	}
	lines := []string{forced, signer}
	switch bad := pki.RollbackUnsafePairs(); {
	case pki.ForcedRotation():
	case isWorker():
		if err := workerRollbackCheck(); err != nil {
			lines = append(lines, "rollback: refused on this worker: "+err.Error())
			break
		}
		lines = append(lines, "rollback: possible (a worker refreshes its trust from the control planes)")
	case len(bad) > 0:
		lines = append(lines, fmt.Sprintf("rollback: refused on this node (the restored %s would not pair a certificate with its key: an old key is missing); "+
			"do not roll back any node, finish the rotation forward after `stratum certs rotate-ca --rollback --cluster --check` confirms every node trusts the new CA", strings.Join(bad, ", ")))
	default:
		if err := cpRollbackCheck(); err != nil {
			lines = append(lines, "rollback: refused on this node: "+err.Error())
			break
		}
		lines = append(lines, "rollback: possible on this node; run `stratum certs rotate-ca --rollback --cluster --check` before rolling back any node")
	}
	return lines
}

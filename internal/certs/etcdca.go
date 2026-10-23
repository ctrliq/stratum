// The per-node half of `stratum certs migrate-etcd-ca`: one phase, on
// this node, followed by the restart the phase requires.
//
// internal/pki owns the file surgery and every safety guard; this file owns the
// two things a primitive deliberately does not — deciding whether the phase is
// already applied, and restarting the component whose in-memory state the write
// does not reach. pki.MigrateEtcdTrustBoth's doc comment states the second
// obligation outright: "Nothing in-tree owns that restart yet ... So the restart
// is the caller's responsibility." This is that caller.
//
// The cluster driver (etcdcacluster.go) invokes this same code path on every
// control plane, through a Job, so a `--cluster` run and three hand-run
// `--phase` invocations do exactly the same thing on each node.
package certs

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/staticpod"
)

// EtcdCAOptions configures one per-node invocation.
type EtcdCAOptions struct {
	Phase       string // trust-both | issue-new | drop-root | rollback
	Resume      bool   // --cluster: proceed on a cluster whose nodes are at different phases
	Force       bool   // --phase=rollback: un-migrate a node that already completed drop-root
	FetchShared bool   // follower CP: pull the initiator's etcd CA first — set by --cluster
	Quiet       bool
}

// etcdMigrationRestart names the components each phase must bounce, and why.
//
// This is deliberately NOT restartControlPlane(), which bounces all five static
// pods plus kubelet and the operator. That set is right for a ROOT rotation,
// where every leaf and every kubeconfig moves. Here the only material that
// changes is etcd's trust bundle and the two etcd leaves, so controller-manager,
// scheduler, kubelet and the operator hold nothing that moved — and kube-vip
// holds the VIP, which a needless bounce would migrate. Three phases times N
// control planes made that the difference between restarting kubelet once and
// restarting it nine times, on a change that touches no kubelet material.
//
// Why each entry is present:
//
//	trust-both  etcd only. Its CA pool is built once at listener construction and
//	            never reloads, so the widened bundle is inert until it restarts.
//	            The apiserver's --etcd-cafile points at the same file, but a
//	            WIDER anchor set invalidates nothing it currently accepts.
//	issue-new   etcd then the apiserver, in that order. etcd picks up its new
//	            etcd-CA-signed server/peer leaf; the apiserver then picks up its
//	            new etcd-CA-signed client leaf. Reversing it would hand the new
//	            client cert to an etcd still serving under the old one — which
//	            trust-both makes survivable, but only by accident.
//	drop-root   etcd only. The narrowed bundle is what etcd must reload; the
//	            apiserver's copy still contains the etcd CA, so it keeps
//	            verifying etcd's server certificate without a bounce.
//	rollback    etcd then the apiserver: both leaves were re-derived under the
//	            cluster root, same ordering argument as issue-new.
var etcdMigrationRestart = map[string][]string{
	pki.EtcdPhaseTrustBoth: {"etcd"},
	pki.EtcdPhaseIssueNew:  {"etcd", "kube-apiserver"},
	pki.EtcdPhaseDropRoot:  {"etcd"},
	pki.EtcdPhaseRollback:  {"etcd", "kube-apiserver"},
	// re-add-root: etcd only. Trust widens, no leaf moves, so the apiserver's
	// client certificate is still one this etcd accepts and its own copy of the
	// bundle still verifies etcd's server certificate.
	EtcdPhaseReAddRoot: {"etcd"},
}

// EtcdPhaseReAddRoot is the first pass of a forced un-migration: widen trust
// cluster-wide before any leaf moves. It is a certs-level phase rather than a
// pki one because it exists purely to give the ORCHESTRATOR an ordering it can
// make safe; a single node has no use for it.
const EtcdPhaseReAddRoot = "re-add-root"

// EtcdCAPhases are the forward phases, in the only order they may be run.
var EtcdCAPhases = []string{pki.EtcdPhaseTrustBoth, pki.EtcdPhaseIssueNew, pki.EtcdPhaseDropRoot}

// EtcdCAAllPhases is every phase a single-node migrate-etcd-ca accepts: the
// forward phases plus rollback, the way back. The cluster orchestrator runs
// only EtcdCAPhases, in EtcdCAPhases' order; rollback is never a scheduled
// step, and re-add-root exists only inside a forced --cluster un-migration.
var EtcdCAAllPhases = []string{
	pki.EtcdPhaseTrustBoth, pki.EtcdPhaseIssueNew, pki.EtcdPhaseDropRoot, pki.EtcdPhaseRollback,
}

// EtcdCAFlags is the raw flag set `stratum certs migrate-etcd-ca` was given.
type EtcdCAFlags struct {
	Cluster     bool
	Phase       string
	Resume      bool
	Rollback    bool
	Force       bool
	FetchShared bool
}

// Validate rejects contradictory, incomplete or misspelled arguments rather
// than silently picking one, on a command that restarts etcd on every control
// plane. Both front ends run it before anything touches the host: the CLI's
// RunE and the MCP tool's handler return its error verbatim, so a refusal
// spells the CLI's flag names (--cluster, --phase) and no front end can
// rebuild the rules, or their wording, by hand.
//
// It lives here, not in pkg/cli, because the cluster driver builds the per-node
// Job argv for THIS command (etcdNodeArgv) and the Job runs it through the same
// CLI — so the rules and the argv have to agree, and a rule that lives only in
// pkg/cli cannot be checked against the argv that must satisfy it. An earlier
// version had them disagree: the argv carried `--phase rollback --force` while
// the CLI accepted --force only alongside the separate --rollback bool, so every
// follower Job failed flag validation. The initiator does not go through cobra
// (the driver runs it in-process), so it succeeded — splitting the cluster's
// trust rather than failing cleanly.
func (f EtcdCAFlags) Validate() error {
	const cmd = "stratum certs migrate-etcd-ca"
	// --rollback is the operator's spelling of the rollback phase;
	// --phase=rollback is the orchestrator's. Either means the same thing.
	rollingBack := f.Rollback || f.Phase == pki.EtcdPhaseRollback || f.Phase == EtcdPhaseReAddRoot
	switch {
	case f.Phase == EtcdPhaseReAddRoot && !f.Force:
		// re-add-root has no replay guard of its own — the orchestrator needs it
		// to run on nodes that HAVE completed drop-root, which is exactly what
		// every other writer of etcd trust refuses. That makes a bare
		// single-node invocation a way to re-open the etcd trust hole (the cluster
		// root accepted as an etcd client CA) on one node with no warning, so it
		// carries the same named opt-in as the rollback it is the first half of. Every orchestrator path that emits it is a forced
		// un-migration and already passes --force.
		return fmt.Errorf("%s: --phase=%s re-adds the cluster root to this node's etcd trust, which RE-OPENS the etcd trust hole on it. "+
			"It is the first pass of a forced un-migration, not a standalone operation: run `%s --cluster --rollback --force --yes`, "+
			"or pass --force if you are driving the passes by hand on every control plane",
			cmd, EtcdPhaseReAddRoot, pki.EtcdMigrateCommand)
	case f.Cluster && f.Phase != "":
		return fmt.Errorf("%s: use either --cluster (all phases) or --phase (one node), not both", cmd)
	case f.Rollback && f.Phase != "" && f.Phase != pki.EtcdPhaseRollback:
		return fmt.Errorf("%s: use either --rollback or --phase=<%s|%s|%s>, not both — they select opposite directions",
			cmd, pki.EtcdPhaseTrustBoth, pki.EtcdPhaseIssueNew, pki.EtcdPhaseDropRoot)
	case f.Force && !rollingBack:
		return fmt.Errorf("%s: --force only applies to a rollback (it un-migrates a cluster that already completed %s)", cmd, pki.EtcdPhaseDropRoot)
	case f.Resume && !f.Cluster:
		return fmt.Errorf("%s: --resume only applies to --cluster", cmd)
	case f.FetchShared && f.Phase != pki.EtcdPhaseTrustBoth:
		return fmt.Errorf("%s: --fetch-shared only applies to --phase=%s", cmd, pki.EtcdPhaseTrustBoth)
	case f.Phase == "" && !f.Cluster && !f.Rollback:
		// --cluster runs every phase itself; a bare --rollback lands on the
		// rollback phase in Options. Anything else has no phase to run.
		return fmt.Errorf("%s: phase is required: pass --cluster (drive all phases) or --phase=<%s> (single node)",
			cmd, strings.Join(EtcdCAAllPhases, "|"))
	case f.Phase != "" && f.Phase != EtcdPhaseReAddRoot && !slices.Contains(EtcdCAAllPhases, f.Phase):
		// re-add-root is exempt: the first case above already refused it without
		// --force, and with --force it is the orchestrator's own pass-one argv.
		return fmt.Errorf("%s: unknown phase %q: want one of %s", cmd, f.Phase, strings.Join(EtcdCAAllPhases, ", "))
	}
	return nil
}

// Options turns validated flags into the options the two entry points take.
func (f EtcdCAFlags) Options() EtcdCAOptions {
	o := EtcdCAOptions{Phase: f.Phase, Resume: f.Resume, Force: f.Force, FetchShared: f.FetchShared}
	if f.Rollback {
		o.Phase = pki.EtcdPhaseRollback
	}
	return o
}

// nodeIdent is who this node is, for the leaf SANs a reissue has to reproduce.
type nodeIdent struct {
	Hostname string
	NodeIP   string
	APISANs  *pki.APISANs
}

// MigrateEtcdCA runs one phase on this node.
//
// The node maintenance lock comes before anything is decided about the phase —
// before the node identity, the applied-phase check, the shared-CA fetch or
// the first write — so an invocation beside a live upgrade or other maintenance
// operation refuses with the operation-naming lock message, and the defer
// releases the lock on every exit. The lockable entry is the exported one: the
// cluster driver's in-process initiator and every node Job (which run the same
// CLI command) reach it, while the lock-free migrateEtcdCAWith stays the seam
// the cluster tests drive.
func MigrateEtcdCA(w io.Writer, o EtcdCAOptions) error {
	release, err := acquireNodeLock("certs migrate-etcd-ca", false)
	if err != nil {
		return err
	}
	defer release()
	if err := RequireNodeProfile("migrate-etcd-ca"); err != nil {
		return err
	}

	hostname, nodeIP, apiExtra, _, err := hostNodeIP()
	if err != nil {
		return err
	}
	return migrateEtcdCAWith(w, o, nodeIdent{hostname, nodeIP, apiExtra},
		etcdNodeOps{
			fetchShared: fetchStagedMaterial,
			podExists:   staticpod.Exists,
			restartPod:  staticpod.Restart,
			waitPod:     waitBounced,
		})
}

// etcdNodeOps are the effectful operations a phase performs on a node: pulling
// the cluster's shared etcd CA, and bouncing a static pod.
type etcdNodeOps struct {
	fetchShared func() error
	podExists   func(string) bool
	restartPod  func(string) error
	waitPod     func(string) error
}

// Pass the node identity and the effectful operations explicitly, for the same
// reason restartControlPlaneGuarded does: so phase sequencing, distribution and
// restart failures can be exercised without a live control plane or a timeout's
// worth of waiting. The cluster tests drive THIS function, not a
// re-implementation of it, so the ordering they assert is the ordering that
// ships.
func migrateEtcdCAWith(w io.Writer, o EtcdCAOptions, id nodeIdent, ops etcdNodeOps) error {
	if _, ok := etcdMigrationRestart[o.Phase]; !ok {
		return fmt.Errorf("certs: unknown etcd-CA migration phase %q (want %s, %s, %s or %s)",
			o.Phase, pki.EtcdPhaseTrustBoth, pki.EtcdPhaseIssueNew, pki.EtcdPhaseDropRoot, pki.EtcdPhaseRollback)
	}
	if !ops.podExists("etcd") {
		return fmt.Errorf("certs: %s is a control-plane operation and this node runs no etcd "+
			"(no %s) — workers hold no etcd leaf and never migrate", o.Phase, staticpod.Path("etcd"))
	}
	// Decide whether this node is already past the phase BEFORE fetching
	// anything. The fetch writes the cluster's CURRENT etcd trust bundle over
	// this node's, so running it for a phase that is about to be skipped can
	// narrow the bundle on disk while nothing restarts etcd to adopt it — on a
	// resumed run whose initiator already completed drop-root, that is a
	// drop-root file write on a follower that never passed the cluster-wide
	// gate. Nothing breaks immediately; it breaks at that node's next restart
	// for any reason, far from the cause.
	//
	// Skipping also means no restart is owed: a phase that wrote nothing has
	// nothing for the running process to adopt, and bouncing etcd anyway would
	// be gratuitous churn on the most fragile component — three phases across N
	// control planes on every resumed run.
	//
	// Rollback and re-add-root are exempt: neither is skippable (EtcdPhaseApplied
	// has no case for either), and neither fetches. Both are idempotent, so a
	// resumed un-migration re-running them rewrites the same bundle.
	if o.Phase != pki.EtcdPhaseRollback && o.Phase != EtcdPhaseReAddRoot {
		applied, err := pki.EtcdPhaseApplied(o.Phase)
		if err != nil {
			return fmt.Errorf("certs: refusing %s — cannot determine this node's etcd trust state: %w", o.Phase, err)
		}
		if applied {
			// Strictly past this phase. Skipping is what makes a resumed
			// --cluster run possible at all: its loop restarts at trust-both,
			// and replaying that against a node that has dropped the root would
			// (rightly) trip the replay guard, leaving no way forward.
			etcdstep(w, o, fmt.Sprintf("Skip %s — already applied on this node", o.Phase))
			return nil
		}
	}

	if o.FetchShared {
		// Follower CP: pull the initiator's etcd CA (and the cluster's current
		// etcd trust bundle) so every control plane shares ONE anchor.
		// MigrateEtcdTrustBoth refuses to mint one here precisely so that a
		// failure to distribute surfaces as a distribution error rather than as
		// a quorum loss two phases later.
		etcdstep(w, o, "Fetch the cluster's etcd CA from the initiator")
		if err := ops.fetchShared(); err != nil {
			return fmt.Errorf("fetch shared etcd CA material: %w", err)
		}
	}

	if err := applyEtcdPhase(w, o, id); err != nil {
		return err
	}

	for _, name := range etcdMigrationRestart[o.Phase] {
		if !ops.podExists(name) {
			continue
		}
		etcdstep(w, o, fmt.Sprintf("Restart %s (phase %s)", name, o.Phase))
		if err := ops.restartPod(name); err != nil {
			return fmt.Errorf("certs: %s wrote its material but %s did not restart: %w. "+
				"etcd builds its CA pools once at startup, so the phase has NOT taken effect on the running process",
				o.Phase, name, err)
		}
		if err := ops.waitPod(name); err != nil {
			return fmt.Errorf("certs: %s restarted %s but it did not come back: %w", o.Phase, name, err)
		}
	}
	return nil
}

// applyEtcdPhase runs the pki primitive for one phase. The caller has already
// established that the phase is owed, so every path here writes.
func applyEtcdPhase(w io.Writer, o EtcdCAOptions, id nodeIdent) error {
	hostname, nodeIP, apiExtra := id.Hostname, id.NodeIP, id.APISANs
	switch o.Phase {
	case EtcdPhaseReAddRoot:
		etcdstep(w, o, "Re-add the cluster root to etcd's trust (no leaf moves)")
		return pki.MigrateEtcdReAddRoot()
	case pki.EtcdPhaseRollback:
		// Never skippable, and never silently forced. Plain rollback undoes a
		// FAILED issue-new and refuses on a node past drop-root; --force is the
		// named opt-in that un-migrates one, and it re-opens the etcd trust hole on this node
		// until the cluster is re-migrated.
		etcdstep(w, o, "Re-derive this node's etcd leaves under the cluster root")
		if o.Force {
			return pki.MigrateEtcdRollbackFromMigrated(hostname, nodeIP, apiExtra)
		}
		return pki.MigrateEtcdRollback(hostname, nodeIP, apiExtra)
	}

	switch o.Phase {
	case pki.EtcdPhaseTrustBoth:
		etcdstep(w, o, "Widen etcd's trust to etcd-ca ‖ cluster root")
		// allowMint only on the node that starts the migration. A follower that
		// mints its own anchor looks healthy right through this phase and breaks
		// the cluster at issue-new.
		return pki.MigrateEtcdTrustBoth(!o.FetchShared)
	case pki.EtcdPhaseIssueNew:
		etcdstep(w, o, "Reissue this node's etcd leaves under etcd-ca")
		return pki.MigrateEtcdIssueNew(hostname, nodeIP, apiExtra)
	case pki.EtcdPhaseDropRoot:
		etcdstep(w, o, "Collapse etcd's trust to etcd-ca alone")
		return pki.MigrateEtcdDropRoot()
	}
	return fmt.Errorf("certs: unreachable phase %q", o.Phase)
}

func etcdstep(w io.Writer, o EtcdCAOptions, msg string) {
	if !o.Quiet {
		k8sutil.Step(w, msg)
	}
}

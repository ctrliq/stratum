// The per-node half of migrate-etcd-ca: one phase, then the restart the phase requires.
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

// Only etcd material moves; etcd restarts before the apiserver so its client leaf lands second.
var etcdMigrationRestart = map[string][]string{
	pki.EtcdPhaseTrustBoth: {"etcd"},
	pki.EtcdPhaseIssueNew:  {"etcd", "kube-apiserver"},
	pki.EtcdPhaseDropRoot:  {"etcd"},
	pki.EtcdPhaseRollback:  {"etcd", "kube-apiserver"},
	// re-add-root: trust widens, no leaf moves, so the apiserver's copy still verifies etcd.
	EtcdPhaseReAddRoot: {"etcd"},
}

// EtcdPhaseReAddRoot is the widening first pass of a forced un-migration.
const EtcdPhaseReAddRoot = "re-add-root"

// EtcdCAPhases are the forward phases, in the only order they may be run.
var EtcdCAPhases = []string{pki.EtcdPhaseTrustBoth, pki.EtcdPhaseIssueNew, pki.EtcdPhaseDropRoot}

// EtcdCAAllPhases is the forward phases plus rollback, what a single node accepts.
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

// Runs before anything touches the host; the driver's Job argv must satisfy the same rules.
func (f EtcdCAFlags) Validate() error {
	const cmd = "stratum certs migrate-etcd-ca"
	rollingBack := f.Rollback || f.Phase == pki.EtcdPhaseRollback || f.Phase == EtcdPhaseReAddRoot
	switch {
	case f.Phase == EtcdPhaseReAddRoot && !f.Force:
		// Re-opens the etcd trust hole on one node, so it needs the named opt-in --force gives.
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
		return fmt.Errorf("%s: phase is required: pass --cluster (drive all phases) or --phase=<%s> (single node)",
			cmd, strings.Join(EtcdCAAllPhases, "|"))
	case f.Phase != "" && f.Phase != EtcdPhaseReAddRoot && !slices.Contains(EtcdCAAllPhases, f.Phase):
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

// MigrateEtcdCA runs one phase on this node behind the node maintenance lock.
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

// etcdNodeOps are the effectful operations a phase performs on a node.
type etcdNodeOps struct {
	fetchShared func() error
	podExists   func(string) bool
	restartPod  func(string) error
	waitPod     func(string) error
}

// Passes the node identity and operations explicitly, as restartControlPlaneGuarded does.
func migrateEtcdCAWith(w io.Writer, o EtcdCAOptions, id nodeIdent, ops etcdNodeOps) error {
	if _, ok := etcdMigrationRestart[o.Phase]; !ok {
		return fmt.Errorf("certs: unknown etcd-CA migration phase %q (want %s, %s, %s or %s)",
			o.Phase, pki.EtcdPhaseTrustBoth, pki.EtcdPhaseIssueNew, pki.EtcdPhaseDropRoot, pki.EtcdPhaseRollback)
	}
	if !ops.podExists("etcd") {
		return fmt.Errorf("certs: %s is a control-plane operation and this node runs no etcd "+
			"(no %s) — workers hold no etcd leaf and never migrate", o.Phase, staticpod.Path("etcd"))
	}
	// The fetch overwrites this node's bundle before anything restarts etcd, so decide the skip first.
	if o.Phase != pki.EtcdPhaseRollback && o.Phase != EtcdPhaseReAddRoot {
		applied, err := pki.EtcdPhaseApplied(o.Phase)
		if err != nil {
			return fmt.Errorf("certs: refusing %s — cannot determine this node's etcd trust state: %w", o.Phase, err)
		}
		if applied {
			// A resumed run restarts its loop at trust-both, which a past-phase node refuses.
			etcdstep(w, o, fmt.Sprintf("Skip %s — already applied on this node", o.Phase))
			return nil
		}
	}

	if o.FetchShared {
		// MigrateEtcdTrustBoth refuses to mint here, so a failure surfaces as a distribution error.
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

// applyEtcdPhase runs the pki primitive for one phase already judged owed.
func applyEtcdPhase(w io.Writer, o EtcdCAOptions, id nodeIdent) error {
	hostname, nodeIP, apiExtra := id.Hostname, id.NodeIP, id.APISANs
	switch o.Phase {
	case EtcdPhaseReAddRoot:
		etcdstep(w, o, "Re-add the cluster root to etcd's trust (no leaf moves)")
		return pki.MigrateEtcdReAddRoot()
	case pki.EtcdPhaseRollback:
		// Plain rollback refuses past drop-root; --force is the named opt-in there.
		etcdstep(w, o, "Re-derive this node's etcd leaves under the cluster root")
		if o.Force {
			return pki.MigrateEtcdRollbackFromMigrated(hostname, nodeIP, apiExtra)
		}
		return pki.MigrateEtcdRollback(hostname, nodeIP, apiExtra)
	}

	switch o.Phase {
	case pki.EtcdPhaseTrustBoth:
		etcdstep(w, o, "Widen etcd's trust to etcd-ca ‖ cluster root")
		// allowMint only on the initiator: a follower that mints breaks the cluster at issue-new.
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

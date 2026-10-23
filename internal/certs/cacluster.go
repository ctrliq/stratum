// Cross-node orchestration for `stratum certs rotate-ca --cluster`. It drives the
// three CA-rotation phases across every node PHASE-MAJOR — every node completes
// phase N before any node starts phase N+1 — which is what keeps etcd peers and
// kubelet clients from ever facing a cert signed by a CA a peer doesn't yet trust.
//
// Reuses the same mechanism as `stratum upgrade --cluster`: a privileged, node-pinned
// Job that nsenters PID 1 and runs the on-host `stratum` binary. It does NOT drain or
// cordon (CA rotation is not workload-disruptive). New CA material is generated once
// on the initiator and distributed to the other control planes via the existing
// encrypted stratum-certs Secret (keyed by a transient stratum-carotate Secret).
package certs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	coordinationv1client "k8s.io/client-go/kubernetes/typed/coordination/v1"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

const (
	caNamespace         = "kube-system"
	caRotationStateName = "stratum-ca-rotation" // non-secret identity retained until every node finalizes
	caRotateKeySecret   = "stratum-carotate"    // transient: holds the stratum-certs key for follower CPs
	// carotateRunAnnotation names the run of the operation that staged the key
	// Secret: the rotate-ca --cluster run id, or, for a migrate-etcd-ca key,
	// the unique certificate key that run minted, which names it. A node
	// records it at sign-new, which is what lets finalize/rollback recovery
	// tell a leftover of THAT run from another run's live key.
	carotateRunAnnotation = "stratum.ciq.com/carotate-run"
	// carotateOperationAnnotation names the operation that staged the key
	// Secret. With the run annotation it is the staged key's owner: a put
	// refuses a Secret the other operation's annotations name instead of
	// overwriting it, and every delete is a precondition delete of the object
	// this run read and vetted.
	carotateOperationAnnotation = "stratum.ciq.com/carotate-operation"
	carotateOpRotateCA          = "rotate-ca"
	carotateOpMigrateEtcdCA     = "migrate-etcd-ca"
	carotateOpMigrateKubeletCA  = "migrate-kubelet-client-ca"
	// maintenanceLeaseName is the shared cluster-wide maintenance Lease this
	// package's --cluster orchestrators hold for their whole run, so a
	// rotate-ca --cluster, a migrate-etcd-ca --cluster and a
	// `stratum upgrade --cluster` exclude one another whatever node each runs
	// on. The upgrade takes its own released lease
	// (kube-system/stratum-cluster-upgrade) as well; the name here is a
	// cluster-wide contract pinned as a literal by the fence tests, so it is
	// spelled out here and in internal/upgrade/orchestrate.go rather than
	// behind one package's constant.
	maintenanceLeaseName = "stratum-cluster-maintenance"
)

// maintenanceHolder is this run's identity on the maintenance Lease. The
// nanoseconds make two runs from one host distinct holders, so a second run
// never mistakes the first run's still-live lease for its own. Call it once
// per run: the release must name the holder the lease was taken with.
func maintenanceHolder() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "unknown-host"
	}
	return fmt.Sprintf("%s/%d/%d", h, os.Getpid(), time.Now().UnixNano())
}

// leaseRider is the client the maintenance lease's API calls ride. A CA
// rotation rebuilds its admin client twice mid-run — the admin certificate is
// reissued after trust-new and again after sign-new, and the client the run
// began with stops authenticating once backends promote and finalize drops
// the old CA — so a lease taken on the first client would fail every renewal
// from then on and expire while the run is still restarting nodes. The swap
// below re-points the rider at the CURRENT client at each rebuild, so the
// background renewal, the confirms and the release all keep authenticating
// for the run's whole life. A leaf rotation on a control plane without a VIP
// swaps it once, to another control plane's apiserver, before it restarts its
// own (rideLeaseOffLocalAPIServer). Every lease call in internal/maintlease
// goes through CoordinationV1; the embedded interface answers for the rest of
// kubernetes.Interface and is never consulted (and never non-nil at those).
type leaseRider struct {
	kubernetes.Interface
	mu  sync.Mutex
	cur kubernetes.Interface
}

func (r *leaseRider) swap(cs kubernetes.Interface) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur = cs
}

func (r *leaseRider) CoordinationV1() coordinationv1client.CoordinationV1Interface {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur.CoordinationV1()
}

// acquireMaintenanceLease takes the shared maintenance Lease for this
// package's --cluster orchestrators, or refuses naming the run that holds it.
// It returns the rider (for the client swaps a mid-run client rebuild
// requires), the Guard, and the release func. The lease is renewed in the
// background — riding the rider — for the run's whole life, and self-expires
// if this process dies. The Guard's Confirm at a phase boundary renews and so
// proves the lease is still ours before the run changes more of the cluster;
// its lost state is what a run stops on. The release func stops the renewal
// and deletes the lease only while it is still ours, so it never removes a
// successor's; it runs on a detached context, so an operator's Ctrl-C (which
// cancels the run's context) still releases the lease.
func acquireMaintenanceLease(ctx context.Context, cs kubernetes.Interface, holder string) (*leaseRider, *maintlease.Guard, func(), error) {
	rider := &leaseRider{cur: cs}
	guard, release, err := maintlease.AcquireCompatible(ctx, rider, caNamespace, maintenanceLeaseName, holder)
	return rider, guard, release, err
}

// confirmMaintenanceLease requires a successful renewal before further work.
// An API restart may recover within two minutes; a missing, forbidden or taken
// lease stops immediately. No mutation is authorized by a failed confirmation.
func confirmMaintenanceLease(ctx context.Context, guard *maintlease.Guard, what string) error {
	if err := guard.ConfirmWithin(ctx, 2*time.Minute); err != nil {
		return fmt.Errorf("the maintenance lease could not be confirmed; stopping before %s: %w", what, err)
	}
	return nil
}

type cnode struct {
	Name string
	IsCP bool
}

// fetchStagedMaterial (follower CP, trust-new) pulls the initiator's staged CA
// material from the stratum-certs Secret, keyed by the transient stratum-carotate
// Secret, using this node's admin creds.
var fetchStagedMaterial = func() error {
	cs, err := adminClientset()
	if err != nil {
		return err
	}
	sec, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read %s key secret: %w", caRotateKeySecret, err)
	}
	key := string(sec.Data["certificate-key"])
	if key == "" {
		return fmt.Errorf("%s missing certificate-key", caRotateKeySecret)
	}
	return ha.DownloadCertsAdmin(key)
}

// caDriver is the seam between the --cluster phase engine and the cluster. The
// production implementation answers Survey from the node roster (classifyForCA),
// runs phases through RotateCA in-process on the initiator and node Jobs on every
// other node, publishes the staged material and its key Secret (ha.NewCertKey,
// ha.ReUploadCerts, stageCarotateKey), rebuilds the admin client from the
// reissued admin.kubeconfig, rolls the in-cluster API clients, and runs the
// workload preflight and the finalize preflights. Tests supply one backed by one
// REAL PKI directory per node, so the per-node state machine and the
// caPhaseApplied skip decision are the production ones — the same reason
// etcdDriver exists for the etcd-CA migration.
type caDriver interface {
	// Survey lists every node with its control-plane role, initiator first.
	Survey() ([]cnode, error)
	// RunPhase applies one phase on one node. fetchShared marks a follower
	// control plane in trust-new; force carries the operator's --force.
	RunPhase(n cnode, phase string, fetchShared, force bool) error
	// InitiatorStaged reports whether the initiator still holds the staged
	// root its trust-new publishes: a promoted initiator's live signers are
	// the rotation's new ones, and publishing them as staged material would
	// hand them to the followers as their old ones.
	InitiatorStaged() bool
	// PublishStagedKey publishes the initiator's staged material and stages the
	// transient key Secret the followers fetch it with, returning the run id
	// that names this run's key.
	PublishStagedKey() (string, error)
	// CleanupStagedKey removes the key Secret of the named run id, and only it.
	CleanupStagedKey(runID string)
	// RebuildClient rebuilds the admin client from the reissued admin.kubeconfig.
	RebuildClient() error
	// RestartClients rolls the in-cluster API clients before sign-new.
	RestartClients() error
	// WorkloadsSettled is the trust-new preflight: the workloads that hold
	// in-cluster credentials were restarted, or the operator skipped them.
	WorkloadsSettled() error
	// ClusterHealthy is the finalize preflight's cluster health arm.
	ClusterHealthy(nodes []cnode) error
	// TokensChecked is the finalize preflight's mounted-token arm.
	TokensChecked(nodes []cnode, me string) error
	// Interrupted reports the operator asked the run to stop.
	Interrupted() error
	// ResolveRotation adopts the active shared run and its phase checkpoint,
	// or records a fresh identity after the previous run completed. Node-local
	// finished markers cannot make a completed rotation look active again.
	ResolveRotation() (runID string, resumed bool, err error)
	// ClearFinishedRotation marks this run complete in the shared record after
	// every node finalizes. A failed write leaves the same run resumable.
	ClearFinishedRotation() error
	// CompletedPhase is the last phase every node completed for this run.
	CompletedPhase() string
	// RecordPhase persists a global checkpoint before the next phase starts.
	RecordPhase(phase string) error
	// FinalizeStarted reports whether a run of this rotation has dispatched
	// finalize: from then on some node may have dropped its old CA, though
	// the checkpoint still names sign-new until every node finalized.
	FinalizeStarted() bool
	// RecordFinalizeStarted persists that before the first finalize dispatch.
	RecordFinalizeStarted() error
}

// caPhases is the phase-major order every --cluster run walks.
var caPhases = []string{PhaseTrustNew, PhaseSignNew, PhaseFinalize}

// caCluster is the phase engine. It holds no client and does no I/O of its own —
// the same contract as etcdMigration — so a test can halt it mid-rotation and
// re-run it against a fake driver backed by real per-node PKI directories.
type caCluster struct {
	drv caDriver
	o   CARotateOptions
	log func(string)
	// confirmLease, when set, renews the shared maintenance Lease before each
	// dispatch and reports a takeover: the fence the run must keep checking
	// mid-run. Nil in tests that drive the phases directly.
	confirmLease func(string) error
}

func (c *caCluster) step(format string, a ...any) {
	if c.log != nil {
		c.log(fmt.Sprintf(format, a...))
	}
}

func (c *caCluster) confirm(what string) error {
	if c.confirmLease == nil {
		return nil
	}
	return c.confirmLease(what)
}

// haltIfInterruptedBefore stops the run before the named step once the operator
// asked it to stop, reporting where the cluster stands. haltedIn is the phase
// whose guidance the halt carries: before the initiator starts a phase, the
// last phase every node completed ("" if nothing has been staged yet); once
// the initiator has run it, that phase itself — its node work is under way, and
// after the initiator's finalize its old CA is gone, so the previous phase's
// rollback advice would split the cluster. The driver's Interrupted is the
// boundary check here; interruptedBefore is the same check for a caller holding
// the run's context, and interruptedHalt is the shared text.
func (c *caCluster) haltIfInterruptedBefore(what, haltedIn string) error {
	if err := c.drv.Interrupted(); err != nil {
		return interruptedHalt(what, haltedIn, err)
	}
	return nil
}

// run drives the whole rotation. It surveys the roster first, then walks the
// three phases PHASE-MAJOR, initiator first: publishing the staged key after the
// initiator's trust-new and before the followers fetch it, rebuilding the admin
// client and rolling the in-cluster clients between trust-new and sign-new,
// running the finalize preflights before finalize, checking Interrupted between
// nodes, and cleaning up this run's key on every exit.
//
// A phase a node has already applied is dispatched again and returns nil
// without applying — the node path decides the skip (caPhaseApplied at the
// phase entry, exactly as the etcd node path decides before it fetches) — which
// is what makes a second run after a halt a RESUME instead of a refusal, and
// why no state check here refuses a cluster that is mid-rotation.
func (c *caCluster) run() error {
	nodes, err := c.drv.Survey()
	if err != nil {
		return err
	}
	if len(nodes) == 0 || !nodes[0].IsCP {
		return fmt.Errorf("certs: run `rotate-ca --cluster` from a control-plane node (the survey named no labeled control plane first)")
	}
	// Stable, quorum-safe order for the "others": control planes serially first,
	// then workers. Within a phase the initiator always goes first.
	otherCPs, workers := splitCP(nodes[1:])
	me := nodes[0].Name
	c.step("CA rotation across %d node(s): initiator=%s, other CPs=%d, workers=%d",
		len(nodes), me, len(otherCPs), len(workers))

	// A halt in trust-new cannot be repaired by a flag, so refuse now, before
	// anything has changed, a cluster whose workloads would need a restart nobody
	// asked for.
	if err := c.drv.WorkloadsSettled(); err != nil {
		return err
	}

	// This run's key Secret is removed on every exit, an interrupt included —
	// except a halt inside the trust-new dispatch below: there a follower may
	// not have finished the phase that reads the staged key, so the halt
	// leaves the key for it and says so (keepStagedKey). The cleanup is named
	// by the rotation identity resolved below, and fires only once this run
	// actually staged a key or resumed a globally completed trust-new: before
	// that checkpoint, a halt before publication leaves the follower key alone.
	runID, resumed, err := c.drv.ResolveRotation()
	if err != nil {
		return err
	}
	if resumed {
		c.step("Resuming CA rotation run %s: the phases each node has already applied for that run are skipped", runID)
	}
	resumedAt := c.drv.CompletedPhase()
	stagedKey := phaseIndex(resumedAt) >= phaseIndex(PhaseTrustNew)
	keepStagedKey := false
	defer func() {
		if !stagedKey || keepStagedKey {
			return
		}
		c.drv.CleanupStagedKey(runID)
	}()

	// The record says every node finalized for this run, and only the write
	// that marks it complete is missing. Nothing is left to run: record the
	// completion and refuse, because whoever runs this command now either
	// wants that bookkeeping or a NEW rotation, and only a second run can give
	// them the new one — reporting success here would tell them the cluster
	// rotated when no root was staged.
	if resumedAt == PhaseFinalize {
		if err := c.confirm("recording rotation completion"); err != nil {
			return err
		}
		if err := c.drv.ClearFinishedRotation(); err != nil {
			return completionHalt(err)
		}
		return fmt.Errorf("certs: CA rotation run %s had already finished on every node — only its completion record was missing, and it is now recorded complete. "+
			"This run rotated nothing: run `stratum certs rotate-ca --cluster --yes` again to start a new rotation", runID)
	}

	// doneThrough is the last phase every node completed: it is the state an
	// interrupt check reports.
	doneThrough := ""
	for i, phase := range caPhases {
		if phaseIndex(phase) <= phaseIndex(c.drv.CompletedPhase()) {
			doneThrough = phase
			c.step("Skipping globally completed phase %s", phase)
			continue
		}
		// The lease first, before the preflight and the phase's node work: a
		// run that has lost the maintenance lease must dispatch nothing more.
		if err := c.confirm(fmt.Sprintf("phase %d/3 (%s)", i+1, phase)); err != nil {
			return err
		}
		switch phase {
		case PhaseTrustNew:
			c.step("Phase 1/3: trust-new (every node trusts old‖new)")
		case PhaseSignNew:
			c.step("Phase 2/3: sign-new (promote new signer, reissue leaves)")
		case PhaseFinalize:
			// Hard gate: only drop the old CA once the whole cluster is provably
			// healthy on the new signer (every prior node succeeded and every
			// node returned Ready). The token gate restarts kubelets and can wait
			// up to an hour, so the health check runs again after it. A run that
			// resumes a finalize an earlier run dispatched stops with finalize's
			// guidance: some node may have finalized, and rolling back every
			// node would leave it alone on the new root.
			c.step("Phase 3/3: finalize preflight")
			if err := finalizeChecks(c.o.SkipTokenCheck,
				func() error { return c.drv.ClusterHealthy(nodes) },
				func() error {
					c.step("Phase 3/3: finalize preflight (no node mounts a token signed by the old service-account key)")
					return c.drv.TokensChecked(nodes, me)
				}); err != nil {
				if c.drv.FinalizeStarted() {
					return haltedStep(PhaseFinalize, "finalize refused", err)
				}
				return haltedStep(PhaseSignNew, "finalize refused (cluster left in the safe old‖new state)", err)
			}
			c.step("Phase 3/3: finalize (drop the old CA)")
		}

		// ── the initiator: every phase starts here ───────────────────────────
		haltedIn := doneThrough
		if phase == PhaseFinalize && c.drv.FinalizeStarted() {
			haltedIn = PhaseFinalize
		}
		if err := c.haltIfInterruptedBefore(phase, haltedIn); err != nil {
			return err
		}
		if err := c.confirm("node " + me); err != nil {
			return err
		}
		// Recorded before the first finalize dispatch, so every later halt of
		// this rotation — this run's or a resume's, from any control plane —
		// knows a node may have dropped its old CA. Nothing has finalized if
		// the write fails.
		if phase == PhaseFinalize && !c.drv.FinalizeStarted() {
			if err := c.drv.RecordFinalizeStarted(); err != nil {
				return haltedStep(PhaseSignNew, "record that finalize started", err)
			}
		}
		if err := c.drv.RunPhase(nodes[0], phase, false, c.o.Force && phase == PhaseTrustNew); err != nil {
			return haltedAt(phase, fmt.Errorf("initiator %s: %w", phase, err))
		}
		if phase != PhaseTrustNew {
			for _, n := range otherCPs {
				if err := c.haltIfInterruptedBefore(phase+" on "+n.Name, phase); err != nil {
					return err
				}
				if err := c.confirm("node " + n.Name); err != nil {
					return err
				}
				if err := c.drv.RunPhase(n, phase, false, false); err != nil {
					return haltedAt(phase, fmt.Errorf("control-plane %s %s: %w (halting; bundles still trust old‖new)", n.Name, phase, err))
				}
			}
			for _, n := range workers {
				if err := c.haltIfInterruptedBefore(phase+" on "+n.Name, phase); err != nil {
					return err
				}
				if err := c.confirm("node " + n.Name); err != nil {
					return err
				}
				if err := c.drv.RunPhase(n, phase, false, false); err != nil {
					return haltedAt(phase, fmt.Errorf("worker %s %s: %w (halting)", n.Name, phase, err))
				}
			}
			if err := c.drv.RecordPhase(phase); err != nil {
				return haltedStep(phase, "record completed phase", err)
			}
			doneThrough = phase
			continue
		}

		// ── trust-new: publish, then hand the phase out ──────────────────────
		// The initiator has generated the new material locally; publish it for
		// the other CPs via the encrypted stratum-certs Secret + a transient key
		// Secret before any follower fetches it. Only staged material is
		// published: an initiator that skipped its trust-new because it is past
		// it — a resume whose shared record has trust-new unfinished, after a
		// by-hand sign-new here — holds the new signers live and nothing staged.
		if !c.drv.InitiatorStaged() {
			return promotedInitiatorRefusal(me)
		}
		if err := c.haltIfInterruptedBefore("publishing the staged material", PhaseTrustNew); err != nil {
			return err
		}
		if err := c.confirm("publishing staged material"); err != nil {
			return err
		}
		id, err := c.drv.PublishStagedKey()
		if err != nil {
			// publishStagedKey already carries the trust-new halt guidance.
			return err
		}
		// The cleanup names the run the key was actually staged under, which
		// is the resolved identity: a resumed run stages its key in the kept
		// one's place under the adopted run.
		runID = id
		stagedKey = true
		if err := trustNewOthers(otherCPs, workers, c.o.Force, c.confirm, func(n cnode, fetchShared, force bool) error {
			// trustNewOthers adds the trust-new halt guidance to whatever stops
			// a node, this boundary included.
			if err := c.haltIfInterruptedBefore(PhaseTrustNew+" on "+n.Name, ""); err != nil {
				return err
			}
			return c.drv.RunPhase(n, phase, fetchShared, force)
		}); err != nil {
			// A follower has not finished trust-new, the phase that reads the
			// staged key: removing this run's key now would strand it
			// mid-fetch. Keep the key and say so; the run that resumes the
			// rotation stages its own key in this one's place and removes it
			// when the rotation completes.
			keepStagedKey = true
			return fmt.Errorf("%w\n  the staged key Secret %s/%s is left in place: a follower has not finished trust-new, the phase that reads it, and still needs it to fetch the staged material", err, caNamespace, caRotateKeySecret)
		}

		// The initiator rewrote its admin.kubeconfig to embed old‖new during
		// trust-new. Rebuild our API client from it so we keep trusting
		// apiservers as they switch to NEW-CA serving certs in phase 2 — the
		// client created at startup trusts only the old CA and would fail TLS
		// through the LB once any backend is promoted — and roll the in-cluster
		// API clients: every node trusts old‖new, so kube-root-ca.crt can carry
		// both and an in-cluster client can start with the new CA before
		// anything serves it.
		if err := runAfterTrustNew(afterTrustNewOps{
			rebuildClient:  c.drv.RebuildClient,
			restartClients: c.drv.RestartClients,
		}); err != nil {
			return err
		}
		if err := c.drv.RecordPhase(phase); err != nil {
			return haltedStep(phase, "record completed phase", err)
		}
		doneThrough = phase
	}
	// The shared-cert Secret (stratum-certs) is intentionally NOT re-published here:
	// re-uploading under a throwaway key would only leave it undecryptable. The next
	// `token create --control-plane` an operator runs to mint an HA join re-keys and
	// re-uploads the current (new) material — that is the sole, working re-key path.
	// Retire the identity across the cluster only after every node finalized.
	// A failed completion write leaves the record at finalize, which the next
	// run records complete without replaying a phase (above).
	if err := c.confirm("recording rotation completion"); err != nil {
		return err
	}
	if err := c.drv.ClearFinishedRotation(); err != nil {
		return completionHalt(err)
	}
	if resumedAt == PhaseSignNew {
		// This run only re-ran finalize: the root it finished was staged by the
		// run it resumed, which may have finished by hand already.
		c.step("CA rotation run %s complete: this run resumed it at finalize and staged no new root, so the cluster is on the root that run staged. "+
			"Run `stratum certs rotate-ca --cluster --yes` again to rotate", runID)
		return nil
	}
	c.step("CA rotation complete — cluster now on the new CA")
	return nil
}

// recordCompleteCmd marks the shared rotation record complete by hand, for a
// rotation every node finished without a --cluster run recording it.
const recordCompleteCmd = `kubectl -n kube-system patch configmap stratum-ca-rotation --type merge -p '{"data":{"complete":"true"}}'`

// completionHalt reports a failed completion write: every node finalized, and
// only the record that says so is missing. A re-run records it and stops.
func completionHalt(err error) error {
	return fmt.Errorf("record rotation completion: %w\n  every node finalized: the rotation is done, but its shared record could not be marked complete. "+
		"Run `stratum certs rotate-ca --cluster --yes` again to record it — that run records it and stops, rotating nothing — or record it by hand: `%s`. "+
		"Do NOT roll back any node: every one has finalized, and its old CA is gone", err, recordCompleteCmd)
}

// prodCADriver is the production caDriver. It runs the initiator's phases
// in-process through RotateCA — the node maintenance lock inside it fences this
// host — and every other node's phase through a privileged nsenter Job.
type prodCADriver struct {
	ctx     context.Context
	w       io.Writer
	build   clientBuilder
	o       CARotateOptions
	me      string
	cs      kubernetes.Interface
	rider   *leaseRider
	confirm func(string) error
	nodes   []cnode // the survey, cached: the entry classifies once, the engine re-reads it
	// rotationRun is the rotation identity ResolveRotation resolved for this
	// run: it rides the initiator's in-process phase options and names the
	// run the key Secret is staged under.
	rotationRun    string
	completedPhase string
	// resumedRecord is set when ResolveRotation adopted an unfinished shared
	// record: this run continues a rotation an earlier run recorded.
	resumedRecord bool
	// finalizeStarted mirrors the record's finalizeStartedKey.
	finalizeStarted bool
}

// Survey lists every node with its control-plane role, initiator first, and
// refuses a run from a node no roster labels a control plane: the phases must be
// driven from one.
func (d *prodCADriver) Survey() ([]cnode, error) {
	if d.nodes != nil {
		return d.nodes, nil
	}
	nodes, err := classifyForCA(d.ctx, d.cs)
	if err != nil {
		return nil, err
	}
	meCP := false
	others := make([]cnode, 0, len(nodes))
	for _, n := range nodes {
		if n.Name == d.me {
			meCP = n.IsCP
			continue
		}
		others = append(others, n)
	}
	if !meCP {
		return nil, fmt.Errorf("certs: run `rotate-ca --cluster` from a control-plane node (this node %q is not a labeled CP)", d.me)
	}
	cps, workers := splitCP(others)
	d.nodes = append(append([]cnode{{Name: d.me, IsCP: true}}, cps...), workers...)
	return d.nodes, nil
}

// RunPhase applies one phase on one node. After the initiator's sign-new the
// driver rebuilds its own client: that phase reissued this node's admin
// certificate under the NEW CA, and a client built on the old one stops
// authenticating once finalize drops the old CA from the apiservers' client-ca.
func (d *prodCADriver) RunPhase(n cnode, phase string, fetchShared, force bool) error {
	if n.Name != d.me {
		return runCAJob(d.w, d.ctx, d.cs, n, phase, fetchShared, force, d.rotationRun)
	}
	o := d.initiatorOptions(phase, force)
	switch phase {
	case PhaseTrustNew, PhaseFinalize:
		return RotateCA(d.w, o)
	case PhaseSignNew:
		if err := RotateCA(d.w, o); err != nil {
			return err
		}
		return haltedStep(PhaseSignNew, "rebuild admin client (new-CA cert) after sign-new", d.rebuild())
	}
	return fmt.Errorf("certs: unknown rotate-ca phase %q (want %s|%s|%s)", phase, PhaseTrustNew, PhaseSignNew, PhaseFinalize)
}

// initiatorOptions are the options the initiator's in-process phase runs
// with: the run's identity, the operator's --force on trust-new, and the
// checks the orchestrator makes itself skipped on the node. A trust-new that
// resumes an unfinished record with no phase completed carries resumeStaging:
// that record is the proof the staging on this node is the run's own.
func (d *prodCADriver) initiatorOptions(phase string, force bool) CARotateOptions {
	o := CARotateOptions{Phase: phase, Quiet: true, RunID: d.rotationRun}
	switch phase {
	case PhaseTrustNew:
		o.Force = force
		o.resumeStaging = d.resumedRecord && d.completedPhase == ""
	case PhaseSignNew:
		o.SkipWorkloadCheck = true
	case PhaseFinalize:
		o.SkipTokenCheck = true
	}
	return o
}

// ResolveRotation reads the authoritative shared checkpoint while the caller
// holds the maintenance lease. A completed record supersedes stale local markers
// and key-cleanup failures; an active record survives every halt and initiator.
func (d *prodCADriver) ResolveRotation() (string, bool, error) {
	// Refuse before creating a checkpoint: a rejected attempt must not block
	// the foreign operation's recovery with an unfinished root-rotation record.
	if err := checkStagedKeyOwner(d.ctx, d.cs, carotateOpRotateCA); err != nil {
		return "", false, err
	}
	cm, err := d.cs.CoreV1().ConfigMaps(caNamespace).Get(d.ctx, caRotationStateName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", false, fmt.Errorf("read CA rotation record: %w", err)
	}
	exists := err == nil
	if exists && cm.Data["complete"] != "true" {
		id := cm.Data["run"]
		if id == "" {
			return "", false, fmt.Errorf("CA rotation record %s has no run identity", caRotationStateName)
		}
		d.rotationRun = id
		d.completedPhase = cm.Data["phase"]
		d.finalizeStarted = cm.Data[finalizeStartedKey] == "true"
		rot := pki.ReadCARotation()
		// An empty checkpoint phase can still mean trust-new ran on only
		// some nodes. An untouched initiator must not generate another root
		// under that run, even if no whole-cluster phase was completed.
		if rot.Err == nil && !rot.Staged && !rot.Promoted && !pki.CAPromoted() && !finalizedForRun(id) {
			return "", false, fmt.Errorf("this node has no material for the recorded CA rotation; resume on a control plane that retains this run's staged or promoted material. If the rotation was rolled back, after rollback succeeds on every control plane and worker, clear the rotation record: kubectl -n %s delete configmap %s", caNamespace, caRotationStateName)
		}
		if phaseIndex(d.completedPhase) < -1 {
			return "", false, fmt.Errorf("invalid CA rotation phase %q", d.completedPhase)
		}
		d.resumedRecord = true
		return id, true, nil
	}
	id := ""
	// Only an absent shared record can predate this protocol. A completed
	// record supersedes any leftover key or local marker from its rotation.
	if !exists {
		sec, err := d.cs.CoreV1().Secrets(caNamespace).Get(d.ctx, caRotateKeySecret, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return "", false, err
		}
		if err == nil && stagedByRotation(sec) {
			id = sec.Annotations[carotateRunAnnotation]
		}
	}
	resumed := id != ""
	if id == "" {
		id, err = newCarotateRunID()
		if err != nil {
			return "", false, err
		}
	}
	if !exists {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: caRotationStateName, Namespace: caNamespace}}
	}
	d.completedPhase = ""
	cm.Data = map[string]string{"run": id}
	if exists {
		_, err = d.cs.CoreV1().ConfigMaps(caNamespace).Update(d.ctx, cm, metav1.UpdateOptions{})
	} else {
		_, err = d.cs.CoreV1().ConfigMaps(caNamespace).Create(d.ctx, cm, metav1.CreateOptions{})
	}
	if err != nil {
		return "", false, fmt.Errorf("record CA rotation identity: %w", err)
	}
	d.rotationRun = id
	return id, resumed, nil
}

func phaseIndex(phase string) int {
	if phase == "" {
		return -1
	}
	for i, p := range caPhases {
		if p == phase {
			return i
		}
	}
	return -2
}

func (d *prodCADriver) CompletedPhase() string { return d.completedPhase }

// finalizeStartedKey is the shared record's mark that a run of the rotation
// has dispatched finalize.
const finalizeStartedKey = "finalize-started"

func (d *prodCADriver) FinalizeStarted() bool { return d.finalizeStarted }

func (d *prodCADriver) RecordFinalizeStarted() error {
	cm, err := d.cs.CoreV1().ConfigMaps(caNamespace).Get(d.ctx, caRotationStateName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cm.Data["run"] != d.rotationRun || cm.Data["complete"] == "true" {
		return fmt.Errorf("CA rotation identity changed before finalize")
	}
	cm.Data[finalizeStartedKey] = "true"
	if _, err := d.cs.CoreV1().ConfigMaps(caNamespace).Update(d.ctx, cm, metav1.UpdateOptions{}); err != nil {
		return err
	}
	d.finalizeStarted = true
	return nil
}

func (d *prodCADriver) RecordPhase(phase string) error {
	cm, err := d.cs.CoreV1().ConfigMaps(caNamespace).Get(d.ctx, caRotationStateName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cm.Data["run"] != d.rotationRun || cm.Data["complete"] == "true" {
		return fmt.Errorf("CA rotation identity changed before phase checkpoint")
	}
	// The old public key is removed by the first finalize. Retain only its
	// non-secret identifier so a retry can scan every node again.
	if phase == PhaseSignNew && cm.Data["old-sa-kid"] == "" {
		kid, err := oldSAKeyID()
		if err != nil && !d.o.SkipTokenCheck {
			return err
		}
		cm.Data["old-sa-kid"] = kid
	}
	cm.Data["phase"] = phase
	if _, err := d.cs.CoreV1().ConfigMaps(caNamespace).Update(d.ctx, cm, metav1.UpdateOptions{}); err != nil {
		return err
	}
	d.completedPhase = phase
	return nil
}

// ClearFinishedRotation retires the shared identity only after every node has
// finalized. A failed retirement leaves a resumable run; it never starts a new
// rotation merely because some nodes already cleared their local markers.
func (d *prodCADriver) ClearFinishedRotation() error {
	cm, err := d.cs.CoreV1().ConfigMaps(caNamespace).Get(d.ctx, caRotationStateName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cm.Data["run"] != d.rotationRun {
		return fmt.Errorf("CA rotation identity changed before completion")
	}
	cm.Data["complete"] = "true"
	_, err = d.cs.CoreV1().ConfigMaps(caNamespace).Update(d.ctx, cm, metav1.UpdateOptions{})
	if err == nil {
		clearCarotateFinishedRun(d.w)
	}
	return err
}

// InitiatorStaged reads this node's own PKI: the initiator's phases run
// in-process.
func (d *prodCADriver) InitiatorStaged() bool { return pki.CARotationStaged() }

func (d *prodCADriver) PublishStagedKey() (string, error) {
	return publishStagedKey(d.ctx, stagedKeyOps{
		checkOwner: func(ctx context.Context) error { return checkStagedKeyOwner(ctx, d.cs, carotateOpRotateCA) },
		newKey:     ha.NewCertKey,
		upload:     uploadStagedMaterial,
		newRunID:   d.resolvedRunID,
		stage:      func(ctx context.Context, key, run string) error { return stageCarotateKey(d.w, ctx, d.cs, key, run) },
	})
}

// resolvedRunID is the run id the key Secret is staged under: the rotation
// identity ResolveRotation resolved, which a resumed run adopted from the
// rotation it continues so every phase decision and the staged key's run
// annotation name the same rotation.
func (d *prodCADriver) resolvedRunID() (string, error) {
	if d.rotationRun == "" {
		return newCarotateRunID()
	}
	return d.rotationRun, nil
}

// CleanupStagedKey removes the key Secret of the named run id, and only it. It
// runs on a detached context, so an operator's Ctrl-C (which cancels the run's
// context) still removes the key.
func (d *prodCADriver) CleanupStagedKey(runID string) {
	cctx, cancel := cleanupContext()
	defer cancel()
	cleanupOwnCarotateKey(cctx, d.w, d.cs, runID, "")
}

// RebuildClient rebuilds the admin client from the reissued admin.kubeconfig.
// It builds into a temporary and swaps only on success: a failed AdminClientset
// returns a nil client, and assigning that straight to cs would leave the
// deferred carotate-key cleanup with a nil client (nil-deref panic that masks
// this error and skips cleanup). On failure cs keeps its last valid value. The
// maintenance lease's renewals ride the new client from here: the
// start-of-run client stops authenticating once backends promote (see
// leaseRider).
func (d *prodCADriver) RebuildClient() error { return d.rebuild() }

func (d *prodCADriver) rebuild() error {
	ncs, err := d.build()
	if err != nil {
		return err
	}
	d.cs = ncs
	if d.rider != nil {
		d.rider.swap(ncs)
	}
	return nil
}

// RestartClients rolls the in-cluster API clients before sign-new switches the
// serving certificates, re-confirming the maintenance lease first.
func (d *prodCADriver) RestartClients() error {
	k8sutil.Step(d.w, "Restarting in-cluster API clients (before sign-new switches the serving certificates)")
	if err := d.confirm("restarting API clients"); err != nil {
		return err
	}
	return restartClients(d.ctx, d.w, d.cs, d.o, clientRollerWithFence(d.ctx, clientRoller(), d.confirm))
}

// WorkloadsSettled is the trust-new preflight: a halt in trust-new is recovered
// by a re-run of this command, so a cluster that would need a restart nobody
// asked for is refused while nothing has changed.
func (d *prodCADriver) WorkloadsSettled() error {
	return preflightWorkloads(d.ctx, d.w, d.cs, d.o)
}

// ClusterHealthy is the finalize preflight's cluster health arm: every node
// Ready, no pending CSR, the local apiserver serving, every kubelet serving a
// cert the new CA signed.
func (d *prodCADriver) ClusterHealthy(nodes []cnode) error {
	return FinalizePreflight(d.ctx, d.w, d.cs, nodes)
}

// TokensChecked is the finalize preflight's mounted-token arm.
func (d *prodCADriver) TokensChecked(nodes []cnode, me string) error {
	cm, err := d.cs.CoreV1().ConfigMaps(caNamespace).Get(d.ctx, caRotationStateName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cm.Data["run"] != d.rotationRun || cm.Data["old-sa-kid"] == "" {
		return fmt.Errorf("CA rotation has no recorded old service-account key identity")
	}
	return finalizeTokenGateForKey(d.ctx, d.w, d.cs, nodes, me, d.confirm, cm.Data["old-sa-kid"])
}

// Interrupted reports the operator asked the run to stop.
func (d *prodCADriver) Interrupted() error { return d.ctx.Err() }

// RotateCACluster orchestrates a full CA rotation across the cluster from a control
// plane. It runs trust-new → sign-new → finalize, each phase completed on the
// initiator, then every other control plane (serial, quorum-safe), then every
// worker — before advancing. Halts on the first node failure, leaving the cluster
// in the stable, still-working state reached so far.
func RotateCACluster(o CARotateOptions) error {
	// Cancel on interrupt rather than dying on it. This run stages a Secret holding
	// the key to every cluster private key; on a plain Background context a signal
	// kills the process before the deferred cleanup runs and leaves that key in the
	// cluster. Cancelling lets the running step return so the defer fires, and the
	// cleanup itself runs on a detached context. A second signal kills the process
	// (see interrupt.Context).
	ctx, stop := interrupt.Context()
	defer stop()
	return RotateCAClusterContext(ctx, os.Stdout, o)
}

// RotateCAClusterContext runs the cluster rotation with the caller's context.
// The CLI supplies its parent-derived signal context; this entry point does
// not install another signal handler or detach from the caller's cancellation.
//
// buildClient is the client builder the run acquires every API client through
// (see clientBuilder): the client at entry, and the two mid-run rebuilds after
// trust-new and after sign-new, where the admin certificate has been reissued
// under the new CA. Callers that pass none get the package's admin.conf
// builder; the front ends inject their own, so the credential decision stays
// with the caller and a rebuild cannot quietly reload admin.conf.
func RotateCAClusterContext(ctx context.Context, w io.Writer, o CARotateOptions, buildClient ...clientBuilder) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("certs: CA rotation not started: %w", err)
	}
	// Before anything is read, built or mutated: a --cluster run given a phase
	// must be refused as a contradiction, not run all three phases (or fail
	// later on a missing kubeconfig, which would read like a node fault). The
	// front ends' validators already refuse this; the orchestrator repeats the
	// refusal so no caller can bypass it by building options directly.
	if o.Phase != "" {
		return fmt.Errorf("certs: a cluster CA rotation runs every phase itself; use either --cluster or --phase=%s (one node), not both", o.Phase)
	}
	if err := RequireNodeProfile("rotate-ca --cluster"); err != nil {
		return err
	}
	build := resolveClientBuilder(buildClient)
	cs, err := build()
	if err != nil {
		return err
	}
	me, err := host.Hostname()
	if err != nil {
		return err
	}
	d := &prodCADriver{ctx: ctx, w: w, build: build, o: o, me: me, cs: cs}
	if _, err := d.Survey(); err != nil {
		return err
	}
	// A halted rotation is not refused: a re-run of this command IS the resume —
	// the engine below re-dispatches every phase and each node skips the ones it
	// has already applied. What still refuses before anything is taken or
	// changed is a state no run could resume (checkClusterRotationState).
	if pki.CARotationStaged() {
		resume, err := recordAtTrustNew(ctx, cs)
		if err != nil {
			return err
		}
		o.resumeStaging = resume
	}
	if err := checkClusterRotationState(o, me); err != nil {
		return err
	}
	if o.Force && pki.CARotationStaged() && pki.ForcedRotation() {
		k8sutil.Step(w, "Resuming the forced rotation staged on this control plane: trust-new runs again on every node with the material already staged")
	}
	if !pki.RootCertPresent() {
		return fmt.Errorf("certs: refusing to rotate (nothing has been changed): %s is missing, or is not a certificate in ca-bundle.crt: %w",
			filepath.Join(version.PKIDir, "ca.crt"), pki.ErrNoRootCert)
	}
	if !pki.OldPublicHalvesPresent() {
		return fmt.Errorf("certs: refusing to rotate (nothing has been changed): front-proxy-ca.crt is missing or not in its bundle, or sa.pub and sa.key are both missing, in %s: %w",
			version.PKIDir, pki.ErrNoOldPublicHalf)
	}

	// One maintenance operation at a time, cluster-wide: take the shared
	// maintenance Lease before the run's first act on the cluster — the
	// workload preflight is a read; staging the key Secret and every phase are
	// not — and hold it through the final restart and readiness wait, so an
	// upgrade or an etcd-CA migration cannot start beside this rotation. The
	// release runs on every exit, an interrupt included: the run's context
	// cancels, the step returns, the defers fire.
	//
	// This run rebuilds cs twice as the admin cert is reissued — and after
	// finalize drops the old CA, a client built on the first one no longer
	// authenticates. The lease therefore rides the rider returned here, swapped
	// at each rebuild: the background renewal keeps succeeding with the CURRENT
	// client for the run's whole life, instead of failing silently and letting
	// the lease expire mid-rotation. The guard's Confirm below is the prompt
	// check at each phase boundary; its lost state is what stops the run.
	holder := maintenanceHolder()
	rider, maintGuard, releaseMaintenance, err := acquireMaintenanceLease(ctx, cs, holder)
	if err != nil {
		return err
	}
	defer releaseMaintenance()
	// confirmLease renews the shared maintenance lease NOW, against the
	// current admin client, and stops the run the moment another run holds it.
	// The background renewal keeps the lease live between these checks (the
	// rider above); this is the prompt refusal at each phase boundary and
	// after each client rebuild, the moments the run is about to change more
	// of the cluster.
	confirmLease := func(what string) error {
		return confirmMaintenanceLease(ctx, maintGuard, what)
	}
	d.rider = rider
	d.confirm = confirmLease
	return (&caCluster{drv: d, o: o, log: func(msg string) { k8sutil.Step(w, msg) }, confirmLease: confirmLease}).run()
}

// checkClusterRotationState refuses a --cluster run whose node state no run
// could resume. A halted rotation is not one of them: a re-run of this command
// IS the resume, so a rotation this command staged and widened — or one whose
// signer a node already promoted — walks forward again, each node skipping the
// phases it has applied. What still refuses, before the lease is taken or
// anything changed: an unreadable trust bundle (no node path could classify
// itself either), and a staging that never finished trust-new's writes with no
// unfinished record at trust-new vouching for it — such staged files name no
// rotation this command left behind, so a re-run cannot tell which root it
// would publish; finish or roll that back first. A staged rotation started with --force resumes only with --force, for
// the same reason the node-side guard pins: re-running without the flag would
// silently un-force a rotation no node may roll back.
func checkClusterRotationState(o CARotateOptions, node string) error {
	rot := pki.ReadCARotation()
	if rot.Staged {
		if o.Force && pki.ForcedRotation() {
			return nil
		}
		// A forced staged rotation resumes only with --force: RotateCAMaterial
		// removes the forced marker without it, and a forced rotation is the one
		// kind no node may roll back.
		if pki.ForcedRotation() {
			return forcedStagedRefusal(node)
		}
		// A staging whose bundle writes failed is this command's when the
		// unfinished record the run resumes has no phase completed
		// (resumeStaging): the initiator's trust-new stopped between them.
		if rot.Err == nil && (rot.BundleCAs > 1 || o.resumeStaging) {
			return nil
		}
		return caRotationGuard(false, true, 0, node)
	}
	if rot.Err != nil {
		if o.Force && errors.Is(rot.Err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot verify CA rotation state on %s: %w", node, rot.Err)
	}
	return nil
}

// recordAtTrustNew reports whether the shared rotation record is unfinished
// with no phase completed: the run this command resumes stopped in its
// trust-new, so a staging it finds on this control plane is that run's own.
// It is read before the maintenance lease is taken, for the entry check
// alone; the in-process trust-new decides again from the record
// ResolveRotation adopts under the lease.
func recordAtTrustNew(ctx context.Context, cs kubernetes.Interface) (bool, error) {
	cm, err := cs.CoreV1().ConfigMaps(caNamespace).Get(ctx, caRotationStateName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read CA rotation record: %w", err)
	}
	return cm.Data["complete"] != "true" && cm.Data["run"] != "" && cm.Data["phase"] == "", nil
}

// finalizeChecks is the finalize preflight: the cluster health check, then the
// token gate unless it is skipped, then the health check again. The token gate
// restarts kubelets and can wait up to an hour, so what the first check saw (every
// node Ready, no CSR pending) is not known to hold when it returns.
func finalizeChecks(skipTokens bool, health, tokens func() error) error {
	if err := health(); err != nil {
		return err
	}
	if skipTokens {
		return nil
	}
	if err := tokens(); err != nil {
		return err
	}
	return health()
}

// haltedAt appends what to do next to the error that halted a --cluster run in
// phase. The one command that works in every halted state is the re-run of
// `rotate-ca --cluster` from this control plane: a re-run is the resume — each
// node skips the phases it has already applied (caPhaseApplied) and finishes
// where it stopped. It deliberately names no single-node phase command: the
// phases must finish on every node, in order, and a lone `--phase=sign-new`
// after a failed trust-new promotes a CA the other nodes never trusted, while a
// lone `--phase=finalize` after a failed sign-new drops the old CA before
// FinalizePreflight has seen the followers. `--rollback` is the one command
// that is safe on any node while the old CA is still held, except in a rotation
// started with --force: there a node that lost a key has no old signer to
// return to, and rolling back the others splits the cluster, so the text says
// how to finish it instead. A node's refusal of its own rotation state
// (stateRefusal) is returned as it is: it names its way out, and a re-run
// would be refused the same way.
func haltedAt(phase string, err error) error {
	if errors.As(err, new(stateRefusal)) {
		return err
	}
	const bin = "stratum certs rotate-ca"
	resume := fmt.Sprintf("Re-run `%s --cluster --yes` from this control plane, with the --restart-workloads or --skip-workload-check the first run had: it skips the phases every node has already applied and finishes the rotation.", bin)
	next := fmt.Sprintf("the run halted in %s. %s Run `stratum certs check` on every node to see where each one stands, and do not run a single --phase on one node: "+
		"the phases must finish on every node in order. To abandon the rotation instead, first run `%s --rollback --cluster --check` from a control plane, "+
		"which confirms every node can roll back (a node that lost an old key cannot, and rolling back the others would split the cluster), "+
		"then run `%s --rollback --yes` on every node; only after every rollback succeeds, clear the shared rotation record: `kubectl -n kube-system delete configmap stratum-ca-rotation`", phase, resume, bin, bin)
	if pki.ForcedRotation() && phase != PhaseFinalize {
		next = forcedHaltGuidance(phase)
	}
	if phase == PhaseFinalize {
		// Past the point of no return on any node that finalized: its old CA is gone.
		next = fmt.Sprintf("the run halted in finalize. %s Run `stratum certs check` on every node. Do NOT roll back a node that already finalized (its old CA is gone). "+
			"FinalizePreflight already passed, so finish by running `%s --phase=%s --yes` on each node that has not finalized, this one included if its restart failed, "+
			"and once every node has, record the rotation complete: `%s`", resume, bin, PhaseFinalize, recordCompleteCmd)
	}
	return fmt.Errorf("%w\n  %s", err, next)
}

// forcedHaltGuidance is haltedAt's text for a rotation started with --force,
// which no node may roll back. Before sign-new the run resumes with --cluster
// --force; after it each node finishes sign-new, a control plane that promoted
// and stopped included.
func forcedHaltGuidance(phase string) string {
	const bin = "stratum certs rotate-ca"
	const dont = "This rotation was started with --force, so do not roll back any node: a node that lost a key has no old signer to return to, " +
		"and rolling back the others splits the cluster. "
	if phase == PhaseTrustNew {
		return fmt.Sprintf("the run halted in trust-new. %sFix the cause above, then run `%s --cluster --force --yes` again from this control plane, "+
			"with the --restart-workloads or --skip-workload-check the first run had: it resumes the staged rotation from trust-new", dont, bin)
	}
	return fmt.Sprintf("the run halted in %s. %sRun `stratum certs check` on every node, fix the cause above, then run `%s --phase=%s --yes` "+
		"on each control plane and then each worker where sign-new has not completed (a control plane whose sign-new promoted and stopped finishes where it stopped), "+
		"then finish with \"Finishing a rotation by hand\" in the certificate-rotation documentation", phase, dont, bin, PhaseSignNew)
}

// haltedStep reports a failure that stops a --cluster run after phase has
// completed everywhere with that phase's halt guidance, so no exit once the
// rotation has begun leaves the operator without `certs check` and rollback
// advice. A nil err stays nil.
func haltedStep(phase, what string, err error) error {
	if err == nil {
		return nil
	}
	return haltedAt(phase, fmt.Errorf("%s: %w", what, err))
}

// forcedStagedRefusal refuses to re-run a rotation staged with --force without
// the flag, naming the one command that resumes it: without --force,
// RotateCAMaterial would remove the forced marker and un-force a rotation no
// node may roll back, and --rollback refuses every forced rotation.
func forcedStagedRefusal(node string) error {
	return fmt.Errorf("the CA rotation staged on %s was started with --force, so it resumes only with --force and cannot be rolled back: "+
		"run `stratum certs rotate-ca --cluster --force --yes` again from the control plane that started it, with the --restart-workloads or --skip-workload-check the first run had", node)
}

// unfinishedRotationExits names the ways out of an unfinished shared rotation
// record, for the refusals it causes elsewhere: a halted rotate-ca --cluster
// run does not finish on its own, so waiting never clears the record. The run
// resumes from a control plane; a rotation abandoned instead leaves the record
// behind once every node's rollback succeeded, and only then may it go.
const unfinishedRotationExits = "A halted run does not finish on its own: resume it with `stratum certs rotate-ca --cluster --yes` " +
	"from a control plane, with the --force, --restart-workloads or --skip-workload-check the first run had (`stratum certs check` " +
	"on each node shows where it stands). Only if the rotation was abandoned and `stratum certs rotate-ca --rollback --yes` " +
	"succeeded on every node, clear its record: `kubectl -n kube-system delete configmap stratum-ca-rotation`"

// promotedInitiatorRefusal stops a resume whose initiator has promoted its
// signer while the shared record still has trust-new unfinished: no staged
// material is left on it to publish, and what it holds live is the rotation's
// new signer. The way out is a run from a control plane still at trust-new,
// whose staged material is the same rotation's — never one that is idle,
// which would stage a root of its own — or, once every control plane has
// promoted, the by-hand finish.
func promotedInitiatorRefusal(node string) error {
	const bin = "stratum certs rotate-ca"
	return fmt.Errorf("certs: refusing to publish staged CA material from %s: its signer is already promoted (sign-new ran on it), "+
		"but the shared rotation record does not show trust-new completed on every node. What it would publish as staged is the rotation's new signer, "+
		"and a control plane that has not promoted would take it as its old one and drop the old root from its trust. Nothing was published. "+
		"Run `stratum certs check` on every node, then run `%s --cluster --yes` again from a control plane that check shows at trust-new "+
		"(staged, and has not promoted), with the --force, --restart-workloads or --skip-workload-check the first run had: it publishes the staged material from there "+
		"and skips the phases each node has already applied. If every control plane has promoted, finish by hand instead: run `%s --phase=%s --yes` "+
		"on each control plane and then each worker where it has not completed (a worker that has not finished trust-new runs `%s --phase=%s --yes` first), "+
		"then finish with \"Finishing a rotation by hand\" in the certificate-rotation documentation", node, bin, bin, PhaseSignNew, bin, PhaseTrustNew)
}

// trustNewOthers runs trust-new on the other control planes, which fetch the
// staged material and take the run's --force, and then on the workers.
func trustNewOthers(otherCPs, workers []cnode, force bool, confirmLease func(string) error, run func(n cnode, fetchShared, force bool) error) error {
	for _, n := range otherCPs {
		if err := confirmLease("node " + n.Name); err != nil {
			return err
		}
		if err := run(n, true, force); err != nil {
			return haltedAt(PhaseTrustNew, fmt.Errorf("control-plane %s trust-new: %w (halting; cluster still works on old‖new)", n.Name, err))
		}
	}
	for _, n := range workers {
		if err := confirmLease("node " + n.Name); err != nil {
			return err
		}
		if err := run(n, false, false); err != nil {
			return haltedAt(PhaseTrustNew, fmt.Errorf("worker %s trust-new: %w (halting)", n.Name, err))
		}
	}
	return nil
}

// afterTrustNewOps are the steps between trust-new and sign-new, injected so the
// halt guidance each failure carries can be tested without a cluster.
type afterTrustNewOps struct {
	rebuildClient  func() error
	restartClients func() error
}

// runAfterTrustNew rebuilds the admin client and then restarts the in-cluster
// clients. Both failures leave the cluster after trust-new with nothing promoted,
// so both give the resume path (haltedBeforeSignNew).
func runAfterTrustNew(ops afterTrustNewOps) error {
	if err := ops.rebuildClient(); err != nil {
		return haltedBeforeSignNew("rebuild admin client (old‖new trust)", err)
	}
	return haltedBeforeSignNew("restart in-cluster API clients", ops.restartClients())
}

// haltedBeforeSignNew reports a failure after trust-new has completed on every
// node and before anything is promoted. Unlike the other trust-new halts, this one
// can be resumed: each node already trusts old‖new, so the operator fixes the cause
// and re-runs `rotate-ca --cluster` from this control plane, which skips the
// phases every node has applied; running sign-new per node, control planes first,
// is the by-hand alternative. A bare --phase=sign-new
// re-runs the client gate, which the orchestrator skips; unlike the orchestrator it
// also neither waits for Ready nor runs FinalizePreflight, hence the checks below.
// Finalize is not spelled out here: by hand it needs the all-node token scan and
// the wait the orchestrator does, so the text points at the documented procedure.
// Workers go after control planes in every phase: a worker's finalize and rollback
// read the cluster-info bundle the control planes republish.
func haltedBeforeSignNew(what string, err error) error {
	if err == nil {
		return nil
	}
	const bin = "stratum certs rotate-ca"
	resume := fmt.Sprintf("Re-run `%s --cluster --yes` from this control plane, with the --restart-workloads or --skip-workload-check the first run had: it skips the phases every node has already applied and finishes the rotation. ", bin)
	abandon := fmt.Sprintf("To abandon the rotation instead, first run `%s --rollback --cluster --check` from a control plane, which confirms every node can roll back, "+
		"then run `%s --rollback --yes` on each control plane and then on each worker; only after every rollback succeeds, clear the shared rotation record: `kubectl -n kube-system delete configmap stratum-ca-rotation`", bin, bin)
	if pki.ForcedRotation() {
		resume = fmt.Sprintf("Re-run `%s --cluster --force --yes` from this control plane, with the --restart-workloads or --skip-workload-check the first run had: "+
			"it skips the phases every node has already applied and finishes the rotation. ", bin)
		abandon = fmt.Sprintf("This rotation was started with --force, so do not roll back any node; running `%s --cluster --force --yes` again from this control plane, "+
			"with the --restart-workloads or --skip-workload-check the first run had, also resumes it", bin)
	}
	return fmt.Errorf("%s: %w\n  the run halted after trust-new: every node trusts old‖new and nothing has been promoted. "+
		"%sTo finish by hand instead, run `%s --phase=%s --yes` on each control plane and then on each worker "+
		"(add --skip-workload-check only for workloads you will not restart, such as Jobs). "+
		"Confirm every node is Ready, `kubectl get csr` shows none pending and the local apiserver /healthz responds, "+
		"then finish with \"Finishing a rotation by hand\" in the certificate-rotation documentation, not with a bare --phase=%s: "+
		"that procedure checks every node's mounted service-account tokens before the first finalize, which one node's finalize does not. %s",
		what, err, resume, bin, PhaseSignNew, PhaseFinalize, abandon)
}

// checkStagedKeyOwner runs under the maintenance lease, before replacing the
// bundle a retained fetch key decrypts. The key's put also checks ownership,
// but checking only there would refuse after the foreign payload was lost.
func checkStagedKeyOwner(ctx context.Context, cs kubernetes.Interface, operation string) error {
	sec, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, caRotateKeySecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read staged-key ownership before publishing shared material: %w", err)
	}
	if foreign, conflict := stagedKeyForeign(stagedKeyOwner{operation: operation}, sec); conflict {
		return fmt.Errorf("refusing to publish %s shared material: %s/%s holds a key the %s operation staged, and its followers may still be fetching the current bundle. Finish or resume that operation first; if it was abandoned, remove its staged key before retrying",
			operation, caNamespace, caRotateKeySecret, foreign)
	}
	return nil
}

// uploadStagedMaterial is shared by both publishers so a regression test can
// observe bundle replacement without a live control plane.
var uploadStagedMaterial = ha.ReUploadCerts

// stagedKeyOps are the steps that publish the initiator's staged material and the
// key Secret the followers fetch it with, injected so the order and the halt
// guidance can be tested without a cluster. newRunID resolves the run id the
// key is staged under — the rotation identity, adopted when the run resumes
// one and minted when it starts fresh.
type stagedKeyOps struct {
	checkOwner func(context.Context) error
	newKey     func() (string, error)
	upload     func(key string) error
	newRunID   func() (string, error)
	stage      func(ctx context.Context, key, runID string) error
}

// publishStagedKey runs those steps after the initiator's trust-new. Every failure
// leaves the cluster halted in trust-new, so every one carries that guidance.
func publishStagedKey(ctx context.Context, ops stagedKeyOps) (runID string, err error) {
	if ops.checkOwner != nil {
		if err := ops.checkOwner(ctx); err != nil {
			return "", haltedStep(PhaseTrustNew, "check staged-key ownership", err)
		}
	}
	key, err := ops.newKey()
	if err != nil {
		return "", haltedStep(PhaseTrustNew, "generate the staged-material key", err)
	}
	if err := ops.upload(key); err != nil {
		return "", haltedStep(PhaseTrustNew, "publish staged material", err)
	}
	runID, err = ops.newRunID()
	if err != nil {
		return "", haltedStep(PhaseTrustNew, "generate the rotate-ca run id", err)
	}
	if err := ops.stage(ctx, key, runID); err != nil {
		return "", haltedStep(PhaseTrustNew, "stage carotate key", err)
	}
	return runID, nil
}

// interruptedBefore refuses to start the next step of a --cluster run once the run
// has been interrupted, reporting where the cluster stands. The local phases do
// not observe ctx, so without this a Ctrl-C that lands between steps would still
// promote the new signer or drop the old CA before anything noticed. doneThrough
// is the last phase every node completed ("" if nothing has been staged yet).
// The engine reaches the same boundary through caDriver.Interrupted —
// interruptedHalt below is the one boundary text both forms share.
func interruptedBefore(ctx context.Context, what, doneThrough string) error {
	if ctx.Err() == nil {
		return nil
	}
	return interruptedHalt(what, doneThrough, ctx.Err())
}

// interruptedHalt shapes a boundary halt: what the run was about to start, the
// state the cluster is left in (haltedAt's guidance when that state is a
// completed phase; bare when nothing has been staged yet), and the
// cancellation itself.
func interruptedHalt(what, doneThrough string, cause error) error {
	err := fmt.Errorf("interrupted before %s: %w", what, cause)
	if doneThrough == "" {
		return err
	}
	return haltedAt(doneThrough, err)
}

// runInitiatorPhase runs one phase on this node unless the run was interrupted,
// and reports a failure with the --cluster halt guidance for that phase. The
// --cluster engine drives the same boundary through the driver seam (its
// Interrupted check between dispatches, its initiator wrap on a phase failure);
// this ctx-holding form is the boundary contract the interrupt pins drive.
func runInitiatorPhase(ctx context.Context, phase, doneThrough string, run func() error) error {
	if err := interruptedBefore(ctx, phase, doneThrough); err != nil {
		return err
	}
	if err := run(); err != nil {
		return haltedAt(phase, fmt.Errorf("initiator %s: %w", phase, err))
	}
	return nil
}

// FinalizePreflight refuses to drop the old CA unless the cluster is provably
// healthy on the new signer: every node Ready, no pending CSRs (a worker awaiting
// its new-CA client cert), every CP's apiserver serving, and every kubelet
// serving a cert the new CA signed (checkKubeletServingTrusted). Because the
// orchestrator halts on the first node failure, reaching here already implies
// every node completed sign-new; this is the explicit safety re-check. The
// mounted-token arm of the preflight (finalizeTokenGateForKey) runs after it.
func FinalizePreflight(ctx context.Context, w io.Writer, cs kubernetes.Interface, nodes []cnode) error {
	nl, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range nl.Items {
		if !nodejob.Ready(&nl.Items[i]) {
			return fmt.Errorf("node %s is not Ready", nl.Items[i].Name)
		}
	}
	if err := k8sutil.WaitHealthz(6); err != nil {
		return fmt.Errorf("local apiserver /healthz: %w", err)
	}
	// A pending CSR means a kubelet is still waiting on its new-CA client cert;
	// dropping the old CA now could lock it out.
	if err := checkNoPendingCSR(ctx, cs); err != nil {
		return err
	}
	return finalizeKubeletCheck(ctx, w, nl.Items)
}

// kubeletServingRoots is pki.KubeletServingRoots and
// controlPlanesBootstrapServing reports whether this node's profile has every
// kubelet, control planes included, bootstrap its serving cert; both are vars
// so a test can stand in for the node. A profile this binary does not register
// refuses rather than read as "static leaf".
var (
	kubeletServingRoots           = pki.KubeletServingRoots
	controlPlanesBootstrapServing = func() (bool, error) {
		st, err := nodestate.LoadState()
		if err != nil {
			return false, fmt.Errorf("read the node state for the cluster profile: %w", err)
		}
		prof, err := profile.ForNode(st.ProfileName())
		if err != nil {
			return false, fmt.Errorf("resolve the cluster profile: %w", err)
		}
		return prof.KubeletServerTLSBootstrap, nil
	}
)

// finalizeKubeletCheck is FinalizePreflight's kubelet arm: every kubelet must
// already serve a cert the newest CA in the bundle signed. The profile comes
// from the initiating control plane's node state, which every node of the
// cluster shares.
func finalizeKubeletCheck(ctx context.Context, w io.Writer, nodes []corev1.Node) error {
	roots, err := kubeletServingRoots()
	if err != nil {
		return err
	}
	newCA, err := newestCA(roots)
	if err != nil {
		return err
	}
	cpBootstrap, err := controlPlanesBootstrapServing()
	if err != nil {
		return err
	}
	return checkKubeletServingTrusted(ctx, w, nodes, cpBootstrap, newCA, probeKubelet)
}

// checkNoPendingCSR is the CSR arm of FinalizePreflight, split out so it is
// testable without a live apiserver. It FAILS CLOSED: a still-pending CSR means a
// node is awaiting its new-CA client cert and dropping the old CA would lock it
// out — and, critically, if the List itself fails the gate cannot prove the
// cluster is safe, so it refuses rather than falling through to "safe to
// finalize". Every other check in FinalizePreflight already returns on error;
// a swallowed list error here would wave the finalize through, so a transient
// apiserver blip mid-rotation could collapse the trust bundle while a worker's
// CSR was still pending.
func checkNoPendingCSR(ctx context.Context, cs kubernetes.Interface) error {
	csrs, err := cs.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("cannot verify no CSR is pending before dropping the old CA: %w", err)
	}
	for i := range csrs.Items {
		c := &csrs.Items[i]
		if len(c.Status.Certificate) > 0 || csrTerminal(c) {
			continue // issued, or denied/failed (not our concern)
		}
		return fmt.Errorf("CSR %s is still pending (a node awaiting its new-CA cert) — retry finalize shortly", c.Name)
	}
	return nil
}

func csrTerminal(c *certificatesv1.CertificateSigningRequest) bool {
	for _, cond := range c.Status.Conditions {
		if cond.Type == certificatesv1.CertificateDenied || cond.Type == certificatesv1.CertificateFailed {
			return true
		}
	}
	return false
}

func classifyForCA(ctx context.Context, cs kubernetes.Interface) ([]cnode, error) {
	nl, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]cnode, 0, len(nl.Items))
	anyCP := false
	for i := range nl.Items {
		cp := nl.Items[i].Labels[k8sutil.ControlPlaneLabel] == "true"
		anyCP = anyCP || cp
		out = append(out, cnode{Name: nl.Items[i].Name, IsCP: cp})
	}
	if !anyCP {
		return nil, fmt.Errorf("no node carries the %s label — label the control planes first (stratum kubectl label node <cp> %s=true)",
			k8sutil.ControlPlaneLabel, k8sutil.ControlPlaneLabel)
	}
	return out, nil
}

func splitCP(nodes []cnode) (cps, workers []cnode) {
	for _, n := range nodes {
		if n.IsCP {
			cps = append(cps, n)
		} else {
			workers = append(workers, n)
		}
	}
	return cps, workers
}

// A remote phase Job runs a whole control-plane restart sequence, so its limits
// must outlast the worst case of that sequence rather than a flat ten minutes, or a
// follower's Job is killed and reported failed while its gates are still
// legitimately waiting. Worst case: etcd's 5m serve budget,
// 90s each for the apiserver, controller-manager and scheduler, up to 2m of
// /healthz polling, a 30s down-window wait per pod, and up to 90s each to restart
// the kubelet and the operator, about 16.5 minutes; 20 minutes leaves margin for
// the PKI work and the Job starting. Both limits move
// together: nodejob polls once every 5s, so 240 polls is the same 20 minutes.
const (
	caJobDeadline     = 20 * time.Minute
	caJobPollAttempts = 240
)

// runCAJob runs one CA-rotation phase on a remote node via a privileged nsenter
// Job, then gates on the Job result AND the node returning Ready (so a worker that
// failed to obtain a new-CA client cert is caught before we advance).
func runCAJob(w io.Writer, ctx context.Context, cs kubernetes.Interface, n cnode, phase string, fetchShared, force bool, runID ...string) error {
	role := "worker"
	if n.IsCP {
		role = "control-plane"
	}
	fmt.Fprintf(w, "  %s (%s): %s\n", n.Name, role, phase)
	err := nodejob.Run(ctx, cs, nodejob.Spec{
		Output:         w,
		Namespace:      caNamespace,
		Name:           caJobName(n.Name, phase),
		Node:           n.Name,
		LabelKey:       "stratum.ciq.com/carotate",
		Container:      "carotate",
		Argv:           caNodeArgv(phase, fetchShared, force, runID...),
		ActiveDeadline: caJobDeadline,
		PollAttempts:   caJobPollAttempts,
		Kind:           "carotate",
	})
	if err != nil {
		return err
	}
	return nodejob.WaitReady(ctx, cs, n.Name, 60)
}

// caNodeArgv builds the exact argv the per-node CA-rotation Job runs.
//
// It is deliberately shell-free, for the same reason nodeUpgradeArgv is
// (internal/upgrade/orchestrate.go): nsenter execs the stratum binary
// directly, so every value below is a discrete argv element that no interpreter
// ever re-parses. One string handed to `/bin/bash -c` would make phase
// executable text on EVERY node in the cluster, as root, in the host namespaces.
//
// Nothing here is attacker-influenced today — phase is one of the three Phase*
// constants and version.StratumInstallPath is fixed at build time — so this closes a
// shape, not a live injection. That is exactly why it is worth closing: the
// guarantee should not depend on who happens to call it.
//
// Keep it that way: this payload has no pipes, redirections, globs or variable
// expansions, so it has never needed a shell. If a future change genuinely needs
// shell features, pass the values in as environment variables and read them
// quoted, rather than re-introducing interpolation here.
func caNodeArgv(phase string, fetchShared, force bool, runID ...string) []string {
	argv := []string{
		"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--",
		version.StratumInstallPath, "certs", "rotate-ca",
		"--phase", phase,
		"--yes",
	}
	if len(runID) > 0 && runID[0] != "" {
		argv = append(argv, "--rotation-run", runID[0])
	}
	if fetchShared {
		argv = append(argv, "--fetch-shared")
	}
	// A follower control plane may have lost the same old keys the initiator
	// went ahead without.
	if force && phase == PhaseTrustNew {
		argv = append(argv, "--force")
	}
	// The orchestrator has rolled the clients, or been told not to; a node's
	// sign-new does not check them again.
	if phase == PhaseSignNew {
		argv = append(argv, "--skip-workload-check")
	}
	// Likewise the tokens: a node's finalize is not gated again, where a refusal
	// would halt the rotation with the old key already dropped on earlier nodes.
	// Not --skip-token-check, which is the operator's and needs --force.
	if phase == PhaseFinalize {
		argv = append(argv, "--tokens-checked")
	}
	return argv
}

func caJobName(node, phase string) string {
	return nodejob.TruncName(fmt.Sprintf("stratum-carotate-%s-%s", phase, node))
}

// CARotateKeyStaged reports whether the transient stratum-carotate Secret is
// present in the cluster. `certs rotate-ca --cluster` and the etcd-CA migration
// create it to hand follower control planes the key that decrypts stratum-certs;
// while it exists, re-encrypting stratum-certs under a new key would strand those
// followers. Unlike RotationPhase() (this node's local PKI files, which read
// "idle" on a follower the initiator has not reached yet), this is a cluster-wide
// signal visible from any control plane — what `certs check` reads to flag a key
// that outlived its run. The re-key guard reads the Secret and the shared
// rotation record itself (GuardRekey).
func CARotateKeyStaged() (bool, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return false, err
	}
	_, err = cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// stagedRotationRunID scopes a manual phase by the active shared run, falling
// back to a staged rotation key only when the cluster predates shared records.
// Completed records suppress stale keys. Node Jobs carry their identity directly.
// Tests without an apiserver replace the reader.
var stagedRotationRunID = func() (string, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cm, err := cs.CoreV1().ConfigMaps(caNamespace).Get(ctx, caRotationStateName, metav1.GetOptions{})
	if err == nil {
		if cm.Data["complete"] == "true" {
			return "", nil
		}
		if cm.Data["run"] == "" {
			return "", fmt.Errorf("active CA rotation record has no run identity")
		}
		return cm.Data["run"], nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	sec, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, caRotateKeySecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !stagedByRotation(sec) {
		return "", nil
	}
	return sec.Annotations[carotateRunAnnotation], nil
}

// stageCarotateKey writes the key Secret. A failed write may still have landed
// (the Create reached the apiserver and its response was lost), and the caller's
// deferred cleanup is registered only after this succeeds, so a failure removes
// the Secret here rather than leave a key nothing will come back for. It removes
// only a Secret carrying this attempt's run id: the Secret is shared with the
// etcd-CA migration, so a put that failed because another operation holds it must
// leave theirs alone.
func stageCarotateKey(w io.Writer, ctx context.Context, cs kubernetes.Interface, keyHex, runID string) error {
	err := putStagedKey(ctx, cs, keyHex, rotationStagedKeyOwner(runID))
	if err != nil {
		removeOwnCarotateKey(w, cs, runID)
	}
	return err
}

// stagedKeyOwner names the operation and the run that own a staged key Secret:
// the two annotations every staged key carries, which the put, the cleanups
// and the reapers read before they touch the Secret.
type stagedKeyOwner struct {
	operation string
	run       string
}

// rotationStagedKeyOwner is the owner a rotate-ca --cluster run stages its key
// under: the operation and the run id newCarotateRunID minted for it.
func rotationStagedKeyOwner(runID string) stagedKeyOwner {
	return stagedKeyOwner{operation: carotateOpRotateCA, run: runID}
}

// migrationStagedKeyOwner is the owner a migrate-etcd-ca run stages its key
// under. The migration mints no run id: its unique certificate key names the
// run, and the migration's own cleanup matches the Secret by that key in its
// data. The run annotation carries only a one-way digest of the key, never the
// key itself: `kubectl describe` and every metadata-only reader print
// annotations in full, and this key decrypts every CA private key in
// stratum-certs.
func migrationStagedKeyOwner(certKey string) stagedKeyOwner {
	sum := sha256.Sum256([]byte(certKey))
	return stagedKeyOwner{operation: carotateOpMigrateEtcdCA, run: hex.EncodeToString(sum[:16])}
}

// annotations returns the Secret annotations that record this owner.
func (o stagedKeyOwner) annotations() map[string]string {
	return map[string]string{
		carotateOperationAnnotation: o.operation,
		carotateRunAnnotation:       o.run,
	}
}

// stagedKeyForeign reports whether sec records another operation as its owner,
// and which one. Before operation annotations, only rotations recorded a run,
// so a legacy key with a run still belongs to rotate-ca. An entirely
// unannotated key has no recorded owner and remains replaceable. A Secret this
// operation's own annotations name is not foreign either: another run of the
// same operation is excluded
// while this run holds the shared maintenance lease, so what is there is that
// run's leftover (or this run's own lost-response retry), which the put
// replaces.
func stagedKeyForeign(owner stagedKeyOwner, sec *corev1.Secret) (string, bool) {
	op := sec.Annotations[carotateOperationAnnotation]
	if op == "" && stagedByRotation(sec) {
		op = carotateOpRotateCA
	}
	if op == "" || op == owner.operation {
		return "", false
	}
	return op, true
}

// stagedByRotation reports whether a staged key Secret carries a rotate-ca
// run's ownership: the operation annotation names rotate-ca, or — a Secret
// staged before the owner type landed — only the run annotation is present,
// and rotation keys are the only ones that ever carried it.
func stagedByRotation(sec *corev1.Secret) bool {
	switch op := sec.Annotations[carotateOperationAnnotation]; op {
	case carotateOpRotateCA:
		return true
	case "":
		return sec.Annotations[carotateRunAnnotation] != ""
	default:
		return false
	}
}

// putStagedKey stages keyHex under owner: the owner-aware put both
// orchestrators stage their fetch key through. It creates the key Secret
// carrying owner's operation and run annotations; when the Secret is already
// there, a key the OTHER operation staged is refused, never overwritten — its
// followers may still be fetching the shared material with it, and that run's
// own cleanup removes it — while a key of this operation (a dead run's
// leftover, or this run's own lost-response retry) is replaced by this run's.
// The read before any write of ours is what makes the refusal hold, and it
// holds for the read-create race too: a Create that comes back AlreadyExists
// lost a race with a stager that arrived between the read and the create, and
// the loop's next read vets the key that appeared before any update of ours
// touches it.
func putStagedKey(ctx context.Context, cs kubernetes.Interface, keyHex string, owner stagedKeyOwner) error {
	for {
		cur, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, caRotateKeySecret, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// Absent: a bare Create is atomic, so the only way it can come
			// back AlreadyExists is a stager that arrived between the read
			// and the create. The loop then re-reads once — the maintenance
			// lease excludes a second live operation, so a second look is the
			// last one needed — and every call here rides the run's context,
			// which bounds a pathological stager that keeps appearing and
			// vanishing underneath this one.
			_, err = cs.CoreV1().Secrets(caNamespace).Create(ctx, stagedKeySecret(keyHex, owner), metav1.CreateOptions{})
			if err == nil || !apierrors.IsAlreadyExists(err) {
				return err
			}
		case err != nil:
			return err
		default:
			if foreign, conflict := stagedKeyForeign(owner, cur); conflict {
				return fmt.Errorf("refusing to stage the %s fetch key: %s/%s already holds a key the %s operation staged, and its followers may still be fetching the shared material with it. That run's own cleanup removes the key when it finishes or resumes; if no %s run is live, remove it by hand: kubectl -n %s delete secret %s",
					owner.operation, caNamespace, caRotateKeySecret, foreign, foreign, caNamespace, caRotateKeySecret)
			}
			// Tie the update to the exact object whose ownership was vetted:
			// Secrets otherwise allow updates with no resourceVersion, which
			// could overwrite a successor that replaced the key after our GET.
			next := stagedKeySecret(keyHex, owner)
			next.ResourceVersion = cur.ResourceVersion
			next.UID = cur.UID
			_, err = cs.CoreV1().Secrets(caNamespace).Update(ctx, next, metav1.UpdateOptions{})
			return err
		}
	}
}

// stagedKeySecret is the key Secret as owner stages it.
func stagedKeySecret(keyHex string, owner stagedKeyOwner) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        caRotateKeySecret,
			Namespace:   caNamespace,
			Annotations: owner.annotations(),
		},
		Data: map[string][]byte{"certificate-key": []byte(keyHex)},
	}
}

// removeOwnCarotateKey removes the key Secret if, and only if, it carries runID: a
// Secret this attempt stored whose put then failed. It runs detached from the
// caller's context, under its own short deadline, and deletes the exact object it
// verified (UID and resourceVersion), as removeOwnLock does for the lock.
// Another operation's Secret (another run id, or none) is left alone. If the
// Secret cannot be read, say so: it may be ours.
func removeOwnCarotateKey(w io.Writer, cs kubernetes.Interface, runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sec, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, caRotateKeySecret, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return
	case err != nil:
		fmt.Fprintf(w, "  WARN: could not check whether a failed attempt to stage %s/%s left it behind (%v).\n"+
			"        It holds the AES key that decrypts the shared cluster CA material (ca.key, sa.key,\n"+
			"        front-proxy-ca.key). If no rotation or etcd-CA migration is running, remove it by hand:\n\n          kubectl -n %s delete secret %s\n",
			caNamespace, caRotateKeySecret, err, caNamespace, caRotateKeySecret)
		return
	case runID == "" || sec.Annotations[carotateRunAnnotation] != runID:
		return
	}
	if err := delCarotateKey(ctx, cs, sec); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		fmt.Fprintf(w, "  WARN: a failed attempt to stage %s/%s left it behind and it could not be removed (%v).\n"+
			"        It holds the AES key that decrypts the shared cluster CA material; remove it by hand:\n\n          kubectl -n %s delete secret %s\n",
			caNamespace, caRotateKeySecret, err, caNamespace, caRotateKeySecret)
	}
}

// delCarotateKey deletes the key Secret. With snap non-nil it deletes only that
// exact object: the UID and resourceVersion preconditions make the apiserver
// answer Conflict if the Secret was replaced or updated since snap was read.
func delCarotateKey(ctx context.Context, cs kubernetes.Interface, snap *corev1.Secret) error {
	var opts metav1.DeleteOptions
	if snap != nil {
		opts.Preconditions = &metav1.Preconditions{UID: &snap.UID, ResourceVersion: &snap.ResourceVersion}
	}
	return cs.CoreV1().Secrets(caNamespace).Delete(ctx, caRotateKeySecret, opts)
}

// carotateCleanupAttempts / carotateCleanupDelay bound the delete retry below.
// Vars, not consts, so a test can zero the delay. ~5 tries over ~20s rides out a
// transient apiserver blip during the sign-new restart storm (Phase 2/3 of the
// cluster rotation) — the moment this defer runs — without hanging the finished
// command.
var (
	carotateCleanupAttempts = 5
	carotateCleanupDelay    = 5 * time.Second
	// carotateGateBudget caps the apiserver health gate below, so a gate that
	// never opens cannot use up the context the delete needs.
	carotateGateBudget = 60 * time.Second
	// waitAPIServer gates the delete on the apiserver answering /healthz, not on
	// the retries below: a failed run can reach cleanup while the apiserver is
	// still restarting (etcd gate timed out), and five retries in ~20s all land
	// inside that window. A var so tests can stand in for it.
	waitAPIServer = waitAPIServerHealthy
)

// cleanupCarotateKey removes the transient stratum-carotate Secret, which holds
// the AES key that decrypts the shared cert bundle (ca.key, sa.key,
// front-proxy-ca.key). Discarding a failed delete would leave that key readable by
// anyone with `get` on secrets in kube-system — silently, since the command has
// already printed "CA rotation complete". It runs in a defer after the
// rotation result is decided, so it cannot return an error; instead it retries
// briefly and, on persistent failure, prints a loud warning naming the Secret and
// the manual cleanup command.
func cleanupCarotateKey(ctx context.Context, cs kubernetes.Interface) {
	_ = cleanupCarotateKeySnapshot(ctx, io.Discard, cs, nil)
}

// cleanupOwnCarotateKey removes only this run's fetch key. A lease may expire
// while the apiserver is unavailable; a successor's key must survive both the
// health gate and retries of this predecessor's deferred cleanup. Rotations
// identify their key by run id; migrations use their unique certificate key.
func cleanupOwnCarotateKey(ctx context.Context, w io.Writer, cs kubernetes.Interface, runID, keyHex string) {
	if cs == nil {
		cleanupCarotateKey(ctx, cs)
		return
	}
	ctx, cancel := boundedCleanup(ctx)
	defer cancel()
	_ = waitAPIServer(ctx, cs)
	var snap *corev1.Secret
	err := retryDelete(ctx, carotateCleanupAttempts, carotateCleanupDelay, func() error {
		var err error
		snap, err = cs.CoreV1().Secrets(caNamespace).Get(ctx, caRotateKeySecret, metav1.GetOptions{})
		return err
	})
	if err != nil {
		fmt.Fprintf(w, "  WARN: could not check ownership of the transient key Secret %s/%s (%v); if no rotation or etcd-CA migration is running, remove it by hand: kubectl -n %s delete secret %s\n",
			caNamespace, caRotateKeySecret, err, caNamespace, caRotateKeySecret)
		return
	}
	if snap == nil || (runID != "" && snap.Annotations[carotateRunAnnotation] != runID) ||
		(runID == "" && (keyHex == "" || string(snap.Data["certificate-key"]) != keyHex)) {
		return
	}
	_ = cleanupCarotateKeySnapshot(ctx, w, cs, snap)
}

// cleanupCarotateKeySnapshot is cleanupCarotateKey for a caller that does not
// own the Secret: it removes only snap, the object that caller already read and
// vetted, on every retry. A Conflict means the Secret was replaced since (a new
// run's key) and is left alone. snap != nil also skips the health gate: the
// caller gated before reading, and no wait may sit between its check and the delete.
// It reports whether the Secret is known gone (deleted, or already absent), which
// a caller holding state about that key needs before it drops that state.
func cleanupCarotateKeySnapshot(ctx context.Context, w io.Writer, cs kubernetes.Interface, snap *corev1.Secret) bool {
	// A defer must never panic. The callers now guarantee a non-nil cs, but guard
	// anyway rather than nil-deref inside delCarotateKey — and still tell the
	// operator the key Secret needs removing.
	if cs == nil {
		fmt.Fprintf(w, "  WARN: no API client available to remove the transient CA-rotation key Secret %s/%s.\n"+
			"        Remove it by hand:\n\n          kubectl -n %s delete secret %s\n",
			caNamespace, caRotateKeySecret, caNamespace, caRotateKeySecret)
		return false
	}
	// Every caller's context is bounded here, not by each caller: a caller passing
	// context.Background() would otherwise let a stalled API call hang the command
	// it is cleaning up after.
	ctx, cancel := boundedCleanup(ctx)
	defer cancel()
	// Not fatal: if the gate never opens the retries still run, and the warning
	// below names the manual command.
	if snap == nil {
		_ = waitAPIServer(ctx, cs)
	}
	replaced, tried := false, 0
	err := retryDelete(ctx, carotateCleanupAttempts, carotateCleanupDelay, func() error {
		tried++
		err := delCarotateKey(ctx, cs, snap)
		if apierrors.IsConflict(err) {
			replaced = true
			return nil
		}
		return err
	})
	if replaced {
		fmt.Fprintf(w, "  WARN: leaving %s/%s in place: it was replaced while recovery was running, so it now belongs to a newer run.\n",
			caNamespace, caRotateKeySecret)
		return false
	}
	if err != nil {
		// retryDelete stops early once ctx is done, so report what was really tried.
		fmt.Fprintf(w, "  WARN: could not remove the transient CA-rotation key Secret %s/%s after %d of up to %d attempts (%v).\n"+
			"        It holds the AES key that decrypts the shared cluster CA material (ca.key, sa.key,\n"+
			"        front-proxy-ca.key); until it is gone, anyone with `get secrets` in %s can decrypt them.\n"+
			"        Remove it by hand:\n\n          kubectl -n %s delete secret %s\n",
			caNamespace, caRotateKeySecret, tried, carotateCleanupAttempts, err, caNamespace, caNamespace, caRotateKeySecret)
		return false
	}
	return true
}

// boundedCleanup caps ctx at cleanupBudget, keeping a shorter deadline ctx already has.
func boundedCleanup(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, cleanupBudget)
}

func newCarotateRunID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate rotate-ca run id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// waitAPIServerHealthy polls /healthz until it answers, for at most
// carotateGateBudget or half of ctx's remaining time, whichever is less, so the
// delete that follows keeps usable time on the same context. Each probe is
// bounded too: a hung request must not outlast the gate. An Unauthorized or
// Forbidden answer still means the apiserver is up; the delete reports its own
// auth failure.
func waitAPIServerHealthy(ctx context.Context, cs kubernetes.Interface) error {
	budget := carotateGateBudget
	if dl, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(dl)/2)
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := cs.Discovery().RESTClient().Get().AbsPath("/healthz").DoRaw(pctx)
		pcancel()
		if err == nil || apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("apiserver /healthz did not become ok: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}

// retryDelete calls del up to attempts times, waiting delay between tries, and
// treats an already-absent object as success. It stops as soon as ctx is done,
// returning the last error, so an expired cleanup budget is not extended by the
// remaining waits. Pure (del is injected) so the retry policy is testable
// without a live apiserver.
func retryDelete(ctx context.Context, attempts int, delay time.Duration, del func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = del(); err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(delay):
			}
		}
	}
	return err
}

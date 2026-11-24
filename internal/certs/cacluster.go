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
	caNamespace                 = "kube-system"
	caRotationStateName         = "stratum-ca-rotation"
	caRotateKeySecret           = "stratum-carotate"
	carotateRunAnnotation       = "stratum.ciq.com/carotate-run"
	carotateOperationAnnotation = "stratum.ciq.com/carotate-operation"
	carotateOpRotateCA          = "rotate-ca"
	carotateOpMigrateEtcdCA     = "migrate-etcd-ca"
	carotateOpMigrateKubeletCA  = "migrate-kubelet-client-ca"
	// a cluster-wide contract, also pinned as a literal in internal/upgrade/orchestrate.go
	maintenanceLeaseName = "stratum-cluster-maintenance"
)

// nanoseconds make two runs from one host distinct holders; call once per run.
func maintenanceHolder() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "unknown-host"
	}
	return fmt.Sprintf("%s/%d/%d", h, os.Getpid(), time.Now().UnixNano())
}

// rides the current client; maintlease only calls CoordinationV1, never the nil embedded Interface
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

func acquireMaintenanceLease(ctx context.Context, cs kubernetes.Interface, holder string) (*leaseRider, *maintlease.Guard, func(), error) {
	rider := &leaseRider{cur: cs}
	guard, release, err := maintlease.AcquireCompatible(ctx, rider, caNamespace, maintenanceLeaseName, holder)
	return rider, guard, release, err
}

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

type caDriver interface {
	Survey() ([]cnode, error)
	RunPhase(n cnode, phase string, fetchShared, force bool) error
	// a promoted initiator has nothing staged; publishing its live signers would mis-feed followers
	InitiatorStaged() bool
	PublishStagedKey() (string, error)
	CleanupStagedKey(runID string)
	RebuildClient() error
	RestartClients() error
	WorkloadsSettled() error
	ClusterHealthy(nodes []cnode) error
	TokensChecked(nodes []cnode, me string) error
	Interrupted() error
	ResolveRotation() (runID string, resumed bool, err error)
	ClearFinishedRotation() error
	CompletedPhase() string
	RecordPhase(phase string) error
	// true once some node may already have dropped its old CA
	FinalizeStarted() bool
	RecordFinalizeStarted() error
}

var caPhases = []string{PhaseTrustNew, PhaseSignNew, PhaseFinalize}

type caCluster struct {
	drv          caDriver
	o            CARotateOptions
	log          func(string)
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

// haltedIn is the phase the halt guidance names: once the initiator has run it, that phase itself.
func (c *caCluster) haltIfInterruptedBefore(what, haltedIn string) error {
	if err := c.drv.Interrupted(); err != nil {
		return interruptedHalt(what, haltedIn, err)
	}
	return nil
}

func (c *caCluster) run() error {
	nodes, err := c.drv.Survey()
	if err != nil {
		return err
	}
	if len(nodes) == 0 || !nodes[0].IsCP {
		return fmt.Errorf("certs: run `rotate-ca --cluster` from a control-plane node (the survey named no labeled control plane first)")
	}
	// quorum-safe order: control planes serially, then workers.
	otherCPs, workers := splitCP(nodes[1:])
	me := nodes[0].Name
	c.step("CA rotation across %d node(s): initiator=%s, other CPs=%d, workers=%d",
		len(nodes), me, len(otherCPs), len(workers))

	// a halt in trust-new cannot be repaired by a flag; refuse before anything changes.
	if err := c.drv.WorkloadsSettled(); err != nil {
		return err
	}

	// a halt inside the trust-new dispatch leaves the key for a follower mid-fetch (keepStagedKey).
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

	// every node finalized; only the completion write is missing, so record it and refuse.
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

	doneThrough := ""
	for i, phase := range caPhases {
		if phaseIndex(phase) <= phaseIndex(c.drv.CompletedPhase()) {
			doneThrough = phase
			c.step("Skipping globally completed phase %s", phase)
			continue
		}
		// a run that lost the lease must dispatch nothing more.
		if err := c.confirm(fmt.Sprintf("phase %d/3 (%s)", i+1, phase)); err != nil {
			return err
		}
		switch phase {
		case PhaseTrustNew:
			c.step("Phase 1/3: trust-new (every node trusts old‖new)")
		case PhaseSignNew:
			c.step("Phase 2/3: sign-new (promote new signer, reissue leaves)")
		case PhaseFinalize:
			// the token gate can wait up to an hour, so the health check runs again after it.
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
		// recorded before the first finalize dispatch: later halts must know a node may have finalized.
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
		runID = id
		stagedKey = true
		if err := trustNewOthers(otherCPs, workers, c.o.Force, c.confirm, func(n cnode, fetchShared, force bool) error {
			if err := c.haltIfInterruptedBefore(PhaseTrustNew+" on "+n.Name, ""); err != nil {
				return err
			}
			return c.drv.RunPhase(n, phase, fetchShared, force)
		}); err != nil {
			// a follower may still be mid-fetch of the staged material; removing the key would strand it.
			keepStagedKey = true
			return fmt.Errorf("%w\n  the staged key Secret %s/%s is left in place: a follower has not finished trust-new, the phase that reads it, and still needs it to fetch the staged material", err, caNamespace, caRotateKeySecret)
		}

		// the startup client trusts only the old CA and would fail TLS once any backend promotes.
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
	// re-uploading stratum-certs under a throwaway key would leave it undecryptable.
	if err := c.confirm("recording rotation completion"); err != nil {
		return err
	}
	if err := c.drv.ClearFinishedRotation(); err != nil {
		return completionHalt(err)
	}
	if resumedAt == PhaseSignNew {
		// this run staged no root: it resumed at finalize and finished the root the earlier run staged
		c.step("CA rotation run %s complete: this run resumed it at finalize and staged no new root, so the cluster is on the root that run staged. "+
			"Run `stratum certs rotate-ca --cluster --yes` again to rotate", runID)
		return nil
	}
	c.step("CA rotation complete — cluster now on the new CA")
	return nil
}

const recordCompleteCmd = `kubectl -n kube-system patch configmap stratum-ca-rotation --type merge -p '{"data":{"complete":"true"}}'`

func completionHalt(err error) error {
	return fmt.Errorf("record rotation completion: %w\n  every node finalized: the rotation is done, but its shared record could not be marked complete. "+
		"Run `stratum certs rotate-ca --cluster --yes` again to record it — that run records it and stops, rotating nothing — or record it by hand: `%s`. "+
		"Do NOT roll back any node: every one has finalized, and its old CA is gone", err, recordCompleteCmd)
}

type prodCADriver struct {
	ctx             context.Context
	w               io.Writer
	build           clientBuilder
	o               CARotateOptions
	me              string
	cs              kubernetes.Interface
	rider           *leaseRider
	confirm         func(string) error
	nodes           []cnode
	rotationRun     string
	completedPhase  string
	resumedRecord   bool
	finalizeStarted bool
}

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

// after sign-new the admin cert is new-CA; the old client stops authenticating at finalize.
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

// resumeStaging: the unfinished record vouches that this node's staging is the run's own.
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

func (d *prodCADriver) ResolveRotation() (string, bool, error) {
	// a rejected attempt must not leave an unfinished record blocking the foreign operation's recovery.
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
		// an untouched initiator must not generate another root under an already-staged run.
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
	// a completed record supersedes any leftover key or local marker from its rotation.
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
	// the old key is gone after finalize; keep its identifier so a retry can rescan.
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

func (d *prodCADriver) resolvedRunID() (string, error) {
	if d.rotationRun == "" {
		return newCarotateRunID()
	}
	return d.rotationRun, nil
}

// detached context: an operator's Ctrl-C still removes the key.
func (d *prodCADriver) CleanupStagedKey(runID string) {
	cctx, cancel := cleanupContext()
	defer cancel()
	cleanupOwnCarotateKey(cctx, d.w, d.cs, runID, "")
}

// build to a temporary; on failure cs keeps its last valid client for the deferred key cleanup.
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

func (d *prodCADriver) RestartClients() error {
	k8sutil.Step(d.w, "Restarting in-cluster API clients (before sign-new switches the serving certificates)")
	if err := d.confirm("restarting API clients"); err != nil {
		return err
	}
	return restartClients(d.ctx, d.w, d.cs, d.o, clientRollerWithFence(d.ctx, clientRoller(), d.confirm))
}

func (d *prodCADriver) WorkloadsSettled() error {
	return preflightWorkloads(d.ctx, d.w, d.cs, d.o)
}

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

func (d *prodCADriver) Interrupted() error { return d.ctx.Err() }

func RotateCACluster(o CARotateOptions) error {
	// on a plain Background a signal kills the process before the deferred key cleanup runs.
	ctx, stop := interrupt.Context()
	defer stop()
	return RotateCAClusterContext(ctx, os.Stdout, o)
}

// buildClient: callers inject it so a mid-run rebuild cannot quietly reload admin.conf.
func RotateCAClusterContext(ctx context.Context, w io.Writer, o CARotateOptions, buildClient ...clientBuilder) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("certs: CA rotation not started: %w", err)
	}
	// refuse before anything is read or mutated, so it cannot read like a node fault.
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
	// a re-run IS the resume; only a state no run could resume is refused.
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

	// held before the first mutating act (earlier checks only read) to exclude upgrades and migrations.
	holder := maintenanceHolder()
	rider, maintGuard, releaseMaintenance, err := acquireMaintenanceLease(ctx, cs, holder)
	if err != nil {
		return err
	}
	defer releaseMaintenance()
	confirmLease := func(what string) error {
		return confirmMaintenanceLease(ctx, maintGuard, what)
	}
	d.rider = rider
	d.confirm = confirmLease
	return (&caCluster{drv: d, o: o, log: func(msg string) { k8sutil.Step(w, msg) }, confirmLease: confirmLease}).run()
}

func checkClusterRotationState(o CARotateOptions, node string) error {
	rot := pki.ReadCARotation()
	if rot.Staged {
		if o.Force && pki.ForcedRotation() {
			return nil
		}
		// without --force, RotateCAMaterial removes the forced marker and un-forces a no-rollback rotation.
		if pki.ForcedRotation() {
			return forcedStagedRefusal(node)
		}
		// resumeStaging vouches the staging is this run's: trust-new stopped between the bundle writes.
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

// read before the lease, for the entry check alone; ResolveRotation decides again under it.
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

// a stateRefusal names its own way out and passes through unchanged.
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
		// past the point of no return on any node that finalized: its old CA is gone.
		next = fmt.Sprintf("the run halted in finalize. %s Run `stratum certs check` on every node. Do NOT roll back a node that already finalized (its old CA is gone). "+
			"FinalizePreflight already passed, so finish by running `%s --phase=%s --yes` on each node that has not finalized, this one included if its restart failed, "+
			"and once every node has, record the rotation complete: `%s`", resume, bin, PhaseFinalize, recordCompleteCmd)
	}
	return fmt.Errorf("%w\n  %s", err, next)
}

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

func haltedStep(phase, what string, err error) error {
	if err == nil {
		return nil
	}
	return haltedAt(phase, fmt.Errorf("%s: %w", what, err))
}

func forcedStagedRefusal(node string) error {
	return fmt.Errorf("the CA rotation staged on %s was started with --force, so it resumes only with --force and cannot be rolled back: "+
		"run `stratum certs rotate-ca --cluster --force --yes` again from the control plane that started it, with the --restart-workloads or --skip-workload-check the first run had", node)
}

const unfinishedRotationExits = "A halted run does not finish on its own: resume it with `stratum certs rotate-ca --cluster --yes` " +
	"from a control plane, with the --force, --restart-workloads or --skip-workload-check the first run had (`stratum certs check` " +
	"on each node shows where it stands). Only if the rotation was abandoned and `stratum certs rotate-ca --rollback --yes` " +
	"succeeded on every node, clear its record: `kubectl -n kube-system delete configmap stratum-ca-rotation`"

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

// injected so the halt guidance can be tested without a cluster.
type afterTrustNewOps struct {
	rebuildClient  func() error
	restartClients func() error
}

// both failures leave nothing promoted, so both resume via haltedBeforeSignNew.
func runAfterTrustNew(ops afterTrustNewOps) error {
	if err := ops.rebuildClient(); err != nil {
		return haltedBeforeSignNew("rebuild admin client (old‖new trust)", err)
	}
	return haltedBeforeSignNew("restart in-cluster API clients", ops.restartClients())
}

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

// checking only in the put would refuse after the foreign payload was lost.
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

var uploadStagedMaterial = ha.ReUploadCerts

type stagedKeyOps struct {
	checkOwner func(context.Context) error
	newKey     func() (string, error)
	upload     func(key string) error
	newRunID   func() (string, error)
	stage      func(ctx context.Context, key, runID string) error
}

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

// the local phases do not observe ctx, so a Ctrl-C between steps must be caught here.
func interruptedBefore(ctx context.Context, what, doneThrough string) error {
	if ctx.Err() == nil {
		return nil
	}
	return interruptedHalt(what, doneThrough, ctx.Err())
}

func interruptedHalt(what, doneThrough string, cause error) error {
	err := fmt.Errorf("interrupted before %s: %w", what, cause)
	if doneThrough == "" {
		return err
	}
	return haltedAt(doneThrough, err)
}

func runInitiatorPhase(ctx context.Context, phase, doneThrough string, run func() error) error {
	if err := interruptedBefore(ctx, phase, doneThrough); err != nil {
		return err
	}
	if err := run(); err != nil {
		return haltedAt(phase, fmt.Errorf("initiator %s: %w", phase, err))
	}
	return nil
}

// re-check: halting on first node failure already implies every node finished sign-new.
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
	// a pending CSR means a kubelet awaits its new-CA cert; dropping the old CA would lock it out.
	if err := checkNoPendingCSR(ctx, cs); err != nil {
		return err
	}
	return finalizeKubeletCheck(ctx, w, nl.Items)
}

// vars so a test can stand in for the node.
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

// fails closed: if the List fails the gate cannot prove the cluster is safe, so it refuses.
func checkNoPendingCSR(ctx context.Context, cs kubernetes.Interface) error {
	csrs, err := cs.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("cannot verify no CSR is pending before dropping the old CA: %w", err)
	}
	for i := range csrs.Items {
		c := &csrs.Items[i]
		if len(c.Status.Certificate) > 0 || csrTerminal(c) {
			continue
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

// must outlast the worst-case restart sequence (~16.5m); 240 polls at nodejob's 5s = the same 20m
const (
	caJobDeadline     = 20 * time.Minute
	caJobPollAttempts = 240
)

// the Ready gate catches a worker that failed to obtain its new-CA client cert.
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

// deliberately shell-free: nsenter execs stratum directly, so every value is one argv element.
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
	// a follower control plane may have lost the same old keys the initiator went ahead without.
	if force && phase == PhaseTrustNew {
		argv = append(argv, "--force")
	}
	// the orchestrator rolled the clients; a node's sign-new does not check them again.
	if phase == PhaseSignNew {
		argv = append(argv, "--skip-workload-check")
	}
	// not --skip-token-check, which is the operator's flag and needs --force.
	if phase == PhaseFinalize {
		argv = append(argv, "--tokens-checked")
	}
	return argv
}

func caJobName(node, phase string) string {
	return nodejob.TruncName(fmt.Sprintf("stratum-carotate-%s-%s", phase, node))
}

// while it exists, re-keying stratum-certs would strand the followers still fetching with it.
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

// var so tests without an apiserver can replace the reader.
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

// a failed Create may have landed; remove this attempt's key rather than orphan it.
func stageCarotateKey(w io.Writer, ctx context.Context, cs kubernetes.Interface, keyHex, runID string) error {
	err := putStagedKey(ctx, cs, keyHex, rotationStagedKeyOwner(runID))
	if err != nil {
		removeOwnCarotateKey(w, cs, runID)
	}
	return err
}

type stagedKeyOwner struct {
	operation string
	run       string
}

func rotationStagedKeyOwner(runID string) stagedKeyOwner {
	return stagedKeyOwner{operation: carotateOpRotateCA, run: runID}
}

// the annotation is a digest: kubectl describe prints annotations, and the key decrypts every CA
func migrationStagedKeyOwner(certKey string) stagedKeyOwner {
	sum := sha256.Sum256([]byte(certKey))
	return stagedKeyOwner{operation: carotateOpMigrateEtcdCA, run: hex.EncodeToString(sum[:16])}
}

func (o stagedKeyOwner) annotations() map[string]string {
	return map[string]string{
		carotateOperationAnnotation: o.operation,
		carotateRunAnnotation:       o.run,
	}
}

// a legacy run-only key is rotate-ca's; a same-operation leftover is replaceable under the lease.
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

// a pre-owner-type Secret staged before operation annotations carries only the run annotation.
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

// the read before every write makes the foreign-key refusal hold, across the read-create race too.
func putStagedKey(ctx context.Context, cs kubernetes.Interface, keyHex string, owner stagedKeyOwner) error {
	for {
		cur, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, caRotateKeySecret, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// a Create that returns AlreadyExists lost a race with a stager between the read and create.
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
			// Secrets allow updates with no resourceVersion, which could overwrite a successor's key.
			next := stagedKeySecret(keyHex, owner)
			next.ResourceVersion = cur.ResourceVersion
			next.UID = cur.UID
			_, err = cs.CoreV1().Secrets(caNamespace).Update(ctx, next, metav1.UpdateOptions{})
			return err
		}
	}
}

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

// deletes only the exact Secret this attempt verified; anything else is left alone.
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

// with snap non-nil the UID and resourceVersion preconditions answer Conflict on a replaced Secret.
func delCarotateKey(ctx context.Context, cs kubernetes.Interface, snap *corev1.Secret) error {
	var opts metav1.DeleteOptions
	if snap != nil {
		opts.Preconditions = &metav1.Preconditions{UID: &snap.UID, ResourceVersion: &snap.ResourceVersion}
	}
	return cs.CoreV1().Secrets(caNamespace).Delete(ctx, caRotateKeySecret, opts)
}

// vars so a test can zero the delay; ~20s rides out a blip in the sign-new restart storm.
var (
	carotateCleanupAttempts = 5
	carotateCleanupDelay    = 5 * time.Second
	// a gate that never opens cannot use up the context the delete needs.
	carotateGateBudget = 60 * time.Second
	// var so tests can stand in for the gate.
	waitAPIServer = waitAPIServerHealthy
)

// a failed delete left in place hands that AES key to anyone with `get` on secrets.
func cleanupCarotateKey(ctx context.Context, cs kubernetes.Interface) {
	_ = cleanupCarotateKeySnapshot(ctx, io.Discard, cs, nil)
}

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

// removes only the vetted snap (Conflict = a newer run's key); the caller gated before reading it
func cleanupCarotateKeySnapshot(ctx context.Context, w io.Writer, cs kubernetes.Interface, snap *corev1.Secret) bool {
	// a defer must never panic; guard anyway and still name the manual command.
	if cs == nil {
		fmt.Fprintf(w, "  WARN: no API client available to remove the transient CA-rotation key Secret %s/%s.\n"+
			"        Remove it by hand:\n\n          kubectl -n %s delete secret %s\n",
			caNamespace, caRotateKeySecret, caNamespace, caRotateKeySecret)
		return false
	}
	ctx, cancel := boundedCleanup(ctx)
	defer cancel()
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

// stops as soon as ctx is done, so an expired cleanup budget is not extended by the waits.
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

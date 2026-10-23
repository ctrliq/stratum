package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/token"
	"github.com/ctrliq/stratum/internal/version"
)

// kubeletClusterOps are the cluster effects of a --cluster run, so a test can
// drive the run without one.
type kubeletClusterOps struct {
	local       func(w io.Writer, o KubeletCAOptions) error
	remote      func(n cnode, phase string, fetchShared, force bool) error
	publishKey  func() (cleanup func(), err error)
	unpublish   func() error
	checkClient func(leaf string, trusted bool) error
	confirm     func(what string) error
}

// MigrateKubeletClientCACluster runs every phase on every node from this
// control plane: to KubeletClientCAName, or with Rollback back to the root.
func MigrateKubeletClientCACluster(w io.Writer, o KubeletCAOptions) error {
	ctx, stop := interrupt.Context()
	defer stop()
	return MigrateKubeletClientCAClusterContext(ctx, w, o)
}

// MigrateKubeletClientCAClusterContext is MigrateKubeletClientCACluster with
// the caller's context. Its progress goes through w, and like the other
// cluster orchestrators it acquires its API client through an injectable
// builder: the front ends hand it the node credential's builder,
// the default stays the package's admin seam.
func MigrateKubeletClientCAClusterContext(ctx context.Context, w io.Writer, o KubeletCAOptions, buildClient ...clientBuilder) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("certs: kubelet client-CA migration not started: %w", err)
	}
	if err := RequireNodeProfile("migrate-kubelet-client-ca --cluster"); err != nil {
		return err
	}
	cs, err := resolveClientBuilder(buildClient)()
	if err != nil {
		return err
	}
	me, err := host.Hostname()
	if err != nil {
		return err
	}
	nodes, err := classifyForCA(ctx, cs)
	if err != nil {
		return err
	}
	var others []cnode
	meCP := false
	for _, n := range nodes {
		if n.Name == me {
			meCP = n.IsCP
			continue
		}
		others = append(others, n)
	}
	if !meCP {
		return fmt.Errorf("certs: run `%s --cluster` from a control-plane node (this node %q is not a labeled control plane)", pki.KubeletMigrateCommand, me)
	}
	if pki.CARotationStaged() {
		return fmt.Errorf("certs: refusing to migrate the kubelet client CA (nothing has been changed): a CA rotation is staged on this node; finish it or roll it back first")
	}
	_, guard, release, err := acquireMaintenanceLease(ctx, cs, maintenanceHolder())
	if err != nil {
		return err
	}
	defer release()
	// Another initiator may already be finalized locally while its root rotation
	// still needs followers. Read its shared checkpoint under the same lease.
	active, err := unfinishedCARotation(ctx, cs)
	if err != nil {
		return fmt.Errorf("read CA rotation record before kubelet client-CA migration: %w", err)
	}
	if active {
		return fmt.Errorf("certs: refusing to migrate the kubelet client CA: a cluster CA rotation is unfinished (its record kube-system/stratum-ca-rotation is not complete). %s", unfinishedRotationExits)
	}
	if err := checkStagedKeyOwner(ctx, cs, carotateOpMigrateKubeletCA); err != nil {
		return err
	}
	otherCPs, workers := splitCP(others)
	k8sutil.Step(w, fmt.Sprintf("Kubelet client-CA %s across %d node(s): initiator=%s, other control planes=%d, workers=%d",
		map[bool]string{false: "migration", true: "rollback"}[o.Rollback], len(nodes), me, len(otherCPs), len(workers)))
	return runKubeletCACluster(w, o, otherCPs, workers, kubeletClusterOps{
		local: func(w io.Writer, lo KubeletCAOptions) error {
			if err := interruptedBefore(ctx, lo.Phase, ""); err != nil {
				return err
			}
			return MigrateKubeletClientCA(w, lo)
		},
		remote: func(n cnode, phase string, fetchShared, force bool) error {
			return runKubeletCAJob(ctx, w, cs, n, phase, fetchShared, force)
		},
		publishKey: func() (func(), error) {
			return publishKubeletCAKey(ctx, w, cs)
		},
		unpublish:   token.UnpublishKubeletClientCA,
		checkClient: func(leaf string, trusted bool) error { return checkKubeletsClientAuth(ctx, w, cs, leaf, trusted) },
		confirm:     func(what string) error { return confirmMaintenanceLease(ctx, guard, what) },
	})
}

func publishKubeletCAKey(ctx context.Context, w io.Writer, cs kubernetes.Interface) (func(), error) {
	runID, err := publishStagedKey(ctx, stagedKeyOps{
		checkOwner: func(ctx context.Context) error { return checkStagedKeyOwner(ctx, cs, carotateOpMigrateKubeletCA) },
		newKey:     ha.NewCertKey,
		upload:     uploadStagedMaterial,
		newRunID:   newCarotateRunID,
		stage: func(ctx context.Context, key, run string) error {
			owner := stagedKeyOwner{operation: carotateOpMigrateKubeletCA, run: run}
			err := putStagedKey(ctx, cs, key, owner)
			if err != nil {
				removeOwnCarotateKey(w, cs, run)
			}
			return err
		},
	})
	if err != nil {
		return nil, err
	}
	return func() {
		cctx, cancel := cleanupContext()
		defer cancel()
		cleanupOwnCarotateKey(cctx, w, cs, runID, "")
	}, nil
}

func runKubeletCACluster(w io.Writer, o KubeletCAOptions, otherCPs, workers []cnode, ops kubeletClusterOps) error {
	phases := KubeletCAPhases
	if o.Rollback {
		phases = KubeletCARollbackPhases
	}
	for i, phase := range phases {
		if err := ops.confirm(fmt.Sprintf("phase %d/%d (%s)", i+1, len(phases), phase)); err != nil {
			return err
		}
		k8sutil.Step(w, fmt.Sprintf("Phase %d/%d: %s", i+1, len(phases), phase))
		if err := runKubeletCAPhase(w, o, phase, otherCPs, workers, ops); err != nil {
			return kubeletHalted(phase, o.Rollback, err)
		}
	}
	if o.Rollback {
		k8sutil.Step(w, "Kubelet client-CA rollback complete: every kubelet trusts the cluster root for clients again")
	} else {
		k8sutil.Step(w, "Kubelet client-CA migration complete: no kubelet trusts a root-signed client certificate")
	}
	return nil
}

// runKubeletCAPhase runs phase on this node, then each other control plane,
// then each worker, and checks every kubelet's answer where the phase calls
// for it.
func runKubeletCAPhase(w io.Writer, o KubeletCAOptions, phase string, otherCPs, workers []cnode, ops kubeletClusterOps) error {
	if err := ops.local(w, KubeletCAOptions{Phase: phase, Force: o.Force, Quiet: true}); err != nil {
		return fmt.Errorf("initiator: %w", err)
	}
	fetchShared := phase == pki.KubeletPhaseTrustBoth && len(otherCPs) > 0
	if fetchShared {
		cleanup, err := ops.publishKey()
		if err != nil {
			return err
		}
		defer cleanup()
	}
	for _, n := range otherCPs {
		if err := ops.confirm("node " + n.Name); err != nil {
			return err
		}
		if err := ops.remote(n, phase, fetchShared, o.Force); err != nil {
			return fmt.Errorf("control plane %s: %w", n.Name, err)
		}
	}
	switch phase {
	case pki.KubeletPhaseIssueNew, pki.KubeletPhaseReissueRoot:
		return ops.checkClient("apiserver-kubelet-client", true)
	case pki.KubeletPhaseRemoveCA:
		if err := ops.unpublish(); err != nil {
			return fmt.Errorf("remove the published kubelet client-CA bundle: %w", err)
		}
	}
	for _, n := range workers {
		if err := ops.confirm("node " + n.Name); err != nil {
			return err
		}
		if err := ops.remote(n, phase, false, o.Force); err != nil {
			return fmt.Errorf("worker %s: %w", n.Name, err)
		}
	}
	switch phase {
	case pki.KubeletPhaseDropRoot:
		if err := ops.checkClient("apiserver-kubelet-client", true); err != nil {
			return err
		}
		return ops.checkClient("admin", false)
	case pki.KubeletPhaseRemoveCA:
		return ops.checkClient("apiserver-kubelet-client", true)
	}
	return nil
}

// kubeletHalted adds what to do next to the error that stopped a --cluster run.
func kubeletHalted(phase string, rollback bool, err error) error {
	const bin = pki.KubeletMigrateCommand
	if rollback {
		return fmt.Errorf("%w\n  the rollback halted in %s. Fix the cause above and run `%s --cluster --rollback --yes` again (with the --force the first run had): it skips what each node has already done",
			err, phase, bin)
	}
	return fmt.Errorf("%w\n  the migration halted in %s. Fix the cause above and run `%s --cluster --yes` again: it skips what each node has already done. "+
		"To abandon it, run `%s --cluster --rollback --yes` (add --force once any node has completed %s)", err, phase, bin, bin, pki.KubeletPhaseDropRoot)
}

// checkKubeletsClientAuth checks that every node's kubelet trusts this node's
// {leaf} client certificate, or with trusted false that it refuses it.
func checkKubeletsClientAuth(ctx context.Context, w io.Writer, cs kubernetes.Interface, leaf string, trusted bool) error {
	client, err := pki.LeafKeyPair(leaf)
	if err != nil {
		return fmt.Errorf("load %s: %w", leaf, err)
	}
	roots, err := kubeletServingRoots()
	if err != nil {
		return err
	}
	nl, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range nl.Items {
		n := &nl.Items[i]
		if err := checkKubeletClientAuth(ctx, n.Name, nodeInternalIP(n), leaf, roots, client, trusted, probeKubeletClientAuth); err != nil {
			return err
		}
	}
	verb := "accepts"
	if !trusted {
		verb = "refuses"
	}
	k8sutil.Step(w, fmt.Sprintf("Every kubelet %s %s", verb, leaf))
	return nil
}

// probeKubeletClientAuth is pki.ProbeKubeletClientAuth, a var so a test can
// stand in for the kubelets.
var probeKubeletClientAuth = pki.ProbeKubeletClientAuth

func checkKubeletClientAuth(ctx context.Context, node, ip, leaf string, roots []*x509.Certificate, client tls.Certificate, trusted bool,
	probe func(context.Context, string, []*x509.Certificate, tls.Certificate) (int, error)) error {
	if ip == "" {
		return fmt.Errorf("node %s reports no InternalIP, so its kubelet's client trust cannot be checked", node)
	}
	pctx, cancel := context.WithTimeout(ctx, kubeletAuthProbeTimeout)
	defer cancel()
	code, err := probe(pctx, net.JoinHostPort(ip, strconv.Itoa(version.KubeletPort)), roots, client)
	switch {
	case err != nil:
		return fmt.Errorf("check node %s's kubelet with %s: %w", node, leaf, err)
	case trusted && code == 401:
		return fmt.Errorf("node %s's kubelet refuses %s (401 Unauthorized), so its API servers cannot reach it", node, leaf)
	case !trusted && code != 401:
		return fmt.Errorf("node %s's kubelet still accepts %s, a client certificate the cluster root signed (HTTP %d)", node, leaf, code)
	}
	return nil
}

const (
	kubeletCAJobDeadline     = caJobDeadline
	kubeletCAJobPollAttempts = caJobPollAttempts
)

func runKubeletCAJob(ctx context.Context, w io.Writer, cs kubernetes.Interface, n cnode, phase string, fetchShared, force bool) error {
	err := nodejob.Run(ctx, cs, nodejob.Spec{
		Output:         w,
		Namespace:      caNamespace,
		Name:           kubeletCAJobName(n.Name, phase),
		Node:           n.Name,
		LabelKey:       "stratum.ciq.com/kubelet-client-ca",
		Container:      "kubelet-client-ca",
		Argv:           kubeletCANodeArgv(phase, fetchShared, force),
		ActiveDeadline: kubeletCAJobDeadline,
		PollAttempts:   kubeletCAJobPollAttempts,
		Kind:           "kubelet-client-ca",
	})
	if err != nil {
		return err
	}
	return nodejob.WaitReady(ctx, cs, n.Name, 60)
}

// kubeletCANodeArgv is the argv a node's Job runs; shell-free like caNodeArgv.
func kubeletCANodeArgv(phase string, fetchShared, force bool) []string {
	argv := []string{
		"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--",
		version.StratumInstallPath, "certs", "migrate-kubelet-client-ca",
		"--phase", phase,
		"--yes",
	}
	if fetchShared {
		argv = append(argv, "--fetch-shared")
	}
	if force && phase == pki.KubeletPhaseReAddRoot {
		argv = append(argv, "--force")
	}
	return argv
}

func kubeletCAJobName(node, phase string) string {
	return nodejob.TruncName(fmt.Sprintf("stratum-kubeletca-%s-%s", phase, node))
}

const kubeletAuthProbeTimeout = 10 * time.Second

package certs

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// ExitRollbackUnsafe is the exit status of `rotate-ca --rollback --check` on a
// node that cannot roll back, so the cluster-wide check tells a refusal from a
// Job that failed to run.
const ExitRollbackUnsafe = 11

const rollbackCheck = "rollback-check"

// rollbackStatePrefix starts the line the node check prints first, which the
// cluster-wide check reads from each node's Job log: whether the rotation was
// forced, whether the node trusts the new CA, and, on a control plane, the
// signers a rollback would leave live.
const rollbackStatePrefix = "rollback-state:"

// rollbackState is one node's rollbackStatePrefix line.
type rollbackState struct {
	forced, trustsNew bool
	target            string
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func (s rollbackState) String() string {
	line := fmt.Sprintf("%s forced=%s trusts-new=%s", rollbackStatePrefix, yesNo(s.forced), yesNo(s.trustsNew))
	if s.target != "" {
		line += " target=" + s.target
	}
	return line
}

// parseRollbackState reads the rollbackStatePrefix line from a node check's
// log; ok is false when the log has none.
func parseRollbackState(log string) (s rollbackState, ok bool) {
	for _, l := range strings.Split(log, "\n") {
		f := strings.Fields(strings.TrimSpace(l))
		if len(f) == 0 || f[0] != rollbackStatePrefix {
			continue
		}
		for _, kv := range f[1:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "forced":
				s.forced = v == "yes"
			case "trusts-new":
				s.trustsNew = v == "yes"
			case "target":
				s.target = v
			}
		}
		return s, true
	}
	return rollbackState{}, false
}

// trustsNewCA reports whether this node's trust carries old‖new, which
// trust-new writes and finalize collapses: on a control plane its root bundle,
// and on a worker every file worker sign-new requires widened, since a
// refresh that stopped partway leaves the kubeconfigs on the old CA alone.
func trustsNewCA(worker bool) bool {
	if worker {
		return checkWorkerTrustWidened(workerTrustPaths()) == nil
	}
	n, err := pki.CABundleCertCount("ca")
	return err == nil && n > 1
}

// workerRollbackCheck refuses a worker rollback that would not complete or
// would strand the kubelet. The rollback is a trust refresh, so the local
// inputs that refresh reads, its kubeconfigs included, must be usable. And once
// sign-new renewed the kubelet client certificate under the new CA, the refresh
// does not undo that, so once the control planes trust the old CA alone the
// kubelet could not authenticate.
func workerRollbackCheck() error {
	p := workerTrustPaths()
	_, err := refreshServer(p)
	var pemBytes []byte
	var source string
	var anchor []*x509.Certificate
	if err == nil {
		pemBytes, source, anchor, err = readRefreshAnchor(p)
	}
	if err == nil {
		_, err = checkKubeletRefreshInputs(p, pemBytes, source)
	}
	for _, kc := range refreshKubeconfigs(p) {
		if err != nil {
			break
		}
		_, err = refreshKubeconfig(kc, pemBytes)
	}
	if err != nil {
		return fmt.Errorf("certs: refusing to roll back this worker (nothing has been changed): its trust refresh would refuse: %w: %w. "+
			"Repair it and run the check again, or %s", err, pki.ErrRollbackUnsafe, rejoinWorker)
	}
	if len(anchor) < 2 {
		return nil
	}
	newCA, err := newestCA(anchor)
	if err != nil {
		return fmt.Errorf("certs: refusing to roll back this worker (nothing has been changed): %w, so it cannot tell whether its kubelet client certificate would still be trusted after the rollback: %w. "+
			"Finish the rotation forward: %s", err, pki.ErrRollbackUnsafe, forwardFromSignNew)
	}
	leaf, err := readKubeletClientLeaf(kubeletClientCertPath())
	if err != nil || leaf.CheckSignatureFrom(newCA) != nil {
		return nil
	}
	return fmt.Errorf("certs: refusing to roll back this worker (nothing has been changed): %s renewed its kubelet client certificate under the new CA, "+
		"and a rollback restores only its trust, so the kubelet could not authenticate to control planes that trust the old CA alone: %w. "+
		"Finish the rotation forward: %s. If the control planes have already rolled back, %s",
		PhaseSignNew, pki.ErrRollbackUnsafe, forwardFromSignNew, rejoinWorker)
}

// cpRollbackCheck is a control plane's rollback check: pki.RollbackCheck, and
// the certificate inputs rollbackCPMaterial reads before it restores anything.
func cpRollbackCheck() error {
	if err := pki.RollbackCheck(); err != nil {
		return err
	}
	if _, _, _, _, err := hostNodeIP(); err != nil {
		return fmt.Errorf("certs: refusing to roll back (nothing has been changed): this node's certificate inputs cannot be read, so the rollback could not reissue its leaves: %w: %w. "+
			"Repair them, then run the check again", err, pki.ErrRollbackUnsafe)
	}
	return nil
}

// CheckRollback reports whether THIS node can roll back, and changes nothing.
// A control plane answers with cpRollbackCheck, the whole of RollbackCA's
// refusal logic; a worker holds no CA and refreshes its trust from the control
// planes, so it can unless workerRollbackCheck refuses. It first prints the
// node's rollbackState.
func CheckRollback(w io.Writer) error {
	if isWorker() {
		fmt.Fprintln(w, rollbackState{trustsNew: trustsNewCA(true)})
		if err := workerRollbackCheck(); err != nil {
			return err
		}
		fmt.Fprintln(w, "rollback: possible (a worker refreshes its trust from the control planes)")
		return nil
	}
	fmt.Fprintln(w, rollbackState{forced: pki.ForcedRotation(), trustsNew: trustsNewCA(false), target: pki.RollbackTarget()})
	if err := cpRollbackCheck(); err != nil {
		return err
	}
	fmt.Fprintln(w, "rollback: possible on this node")
	return nil
}

// rollbackCheckNodeArgv is the argv of a node's check Job; shell-free, as
// caNodeArgv is.
func rollbackCheckNodeArgv() []string {
	return []string{
		"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--",
		version.StratumInstallPath, "certs", "rotate-ca",
		"--rollback", "--check",
	}
}

// runRollbackCheckJob runs the node check on n and returns its exit status and
// log. A seam, so the cluster check's verdict can be tested without a cluster.
var runRollbackCheckJob = func(ctx context.Context, w io.Writer, cs kubernetes.Interface, n cnode) (int, string, error) {
	spec := nodejob.Spec{
		Output:         w,
		Namespace:      caNamespace,
		Name:           caJobName(n.Name, rollbackCheck),
		Node:           n.Name,
		LabelKey:       "stratum.ciq.com/carotate",
		Container:      "carotate",
		Argv:           rollbackCheckNodeArgv(),
		ActiveDeadline: scanJobDeadline,
		PollAttempts:   scanJobPollAttempts,
		Kind:           "rollback check",
	}
	return runExitCodeLogs(ctx, cs, spec)
}

// runExitCodeLogs is nodejob.RunExitCodeLogs; a var so a test can answer for
// a node's Job.
var runExitCodeLogs = nodejob.RunExitCodeLogs

// CheckRollbackCluster asks every node, control planes first, whether it can
// roll back, and refuses if one cannot. It takes no lease and changes nothing
// on any node: the point is to find a node that lost an old key before any
// node rolls back, because the healthy nodes would return to the old root
// while that one can only go forward, and the cluster would be split across
// two roots. Run it from a control plane, before `--rollback --yes` on any
// node.
func CheckRollbackCluster(ctx context.Context, w io.Writer, buildClient ...clientBuilder) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("certs: rollback check not started: %w", err)
	}
	build := resolveClientBuilder(buildClient)
	cs, err := build()
	if err != nil {
		return err
	}
	nodes, err := classifyForCA(ctx, cs)
	if err != nil {
		return err
	}
	cps, workers := splitCP(nodes)
	var refused, refusedCPs, forced, untrustingCPs, untrustingWorkers []string
	targets := map[string][]string{}
	for _, n := range append(cps, workers...) {
		role := "worker"
		if n.IsCP {
			role = "control-plane"
		}
		code, log, err := runRollbackCheckJob(ctx, w, cs, n)
		if err != nil {
			return fmt.Errorf("certs: the rollback check did not run on %s: %w; no node was checked past it, and nothing was changed", n.Name, err)
		}
		if code != 0 && code != ExitRollbackUnsafe {
			return fmt.Errorf("certs: the rollback check on %s exited %d: %s", n.Name, code, strings.TrimSpace(log))
		}
		st, ok := parseRollbackState(log)
		if !ok || (n.IsCP && st.target == "") {
			return fmt.Errorf("certs: the rollback check on %s did not report what a rollback would restore there (is it running an older stratum?); nothing was changed", n.Name)
		}
		if code == 0 {
			fmt.Fprintf(w, "  %s (%s): can roll back\n", n.Name, role)
		} else {
			fmt.Fprintf(w, "  %s (%s): cannot roll back: %s\n", n.Name, role, lastLine(log))
			refused = append(refused, n.Name)
			if n.IsCP {
				refusedCPs = append(refusedCPs, n.Name)
			}
		}
		if st.forced {
			forced = append(forced, n.Name)
		}
		switch {
		case st.trustsNew:
		case n.IsCP:
			untrustingCPs = append(untrustingCPs, n.Name)
		default:
			untrustingWorkers = append(untrustingWorkers, n.Name)
		}
		// Every control plane's readable target counts, a refusing one's too:
		// a control plane that finalized and one that lost an old key are
		// each half of the one case no advice below can mend, and only
		// together do they show that the cluster can no longer return to
		// one root. A target with an unreadable half says nothing about
		// where that node would land, so it joins no group.
		if n.IsCP && !strings.Contains(st.target, "unreadable") {
			targets[st.target] = append(targets[st.target], n.Name)
		}
	}
	const bin = "stratum certs rotate-ca"
	switch {
	case len(forced) > 0:
		return fmt.Errorf("certs: do not roll back any node: the rotation was started with --force (%w). "+
			"If `%s --cluster` halted before sign-new, run `%s --cluster --force --yes` again from the control plane that started it, "+
			"with the --restart-workloads or --skip-workload-check the first run had: it resumes the staged rotation. Otherwise %s",
			pki.ErrRollbackUnsafe, bin, bin, forwardFromSignNew)
	case len(targets) > 1:
		var groups []string
		for t, names := range targets {
			groups = append(groups, fmt.Sprintf("%s to %s", strings.Join(names, ", "), t))
		}
		slices.Sort(groups)
		return fmt.Errorf("certs: do not roll back any node: the control planes would not roll back to the same signers (%s): %w. "+
			"A control plane that finalized keeps only the new CA, so rolling back the others would leave the cluster split across two roots. "+
			"Finish the rotation forward instead: %s",
			strings.Join(groups, "; "), pki.ErrRollbackUnsafe, forwardFromSignNew)
	case len(refusedCPs) > 0 && len(untrustingCPs) > 0:
		return fmt.Errorf("certs: do not roll back any node: %s cannot roll back (%w). Rolling back the others would leave the cluster split across two roots. "+
			"Do not run sign-new either: %s do not trust the new CA, so a node signing with it would not be trusted there, "+
			"and no command gives a control plane the staged CA once the run that staged it has ended. "+
			"Restore the missing key on %s from a backup, so that it can roll back, and run this check again: when it passes, roll back every node",
			strings.Join(refused, ", "), pki.ErrRollbackUnsafe, strings.Join(untrustingCPs, ", "), strings.Join(refusedCPs, ", "))
	case len(refused) > 0 && len(untrustingCPs) > 0:
		return fmt.Errorf("certs: the workers %s cannot roll back (%w). "+
			"Do not run sign-new: %s do not trust the new CA, and no command gives a control plane the staged CA once the run that staged it has ended. "+
			"Roll back every other node instead (`%s --rollback --yes` on each control plane, then each worker), "+
			"then, on each of %s, %s",
			strings.Join(refused, ", "), pki.ErrRollbackUnsafe, strings.Join(untrustingCPs, ", "), bin, strings.Join(refused, ", "), rejoinWorker)
	case len(refused) > 0 && len(untrustingWorkers) > 0:
		return fmt.Errorf("certs: do not roll back any node: %s cannot roll back (%w). Rolling back the others would leave the cluster split across two roots. "+
			"Do not run sign-new yet either: the workers %s do not trust the new CA, so a node signing with it would not be trusted there. "+
			"First run `%s --phase=trust-new --yes` on each of them, which refreshes its trust from the control planes, "+
			"and run this check again to confirm every node trusts the new CA. Then %s",
			strings.Join(refused, ", "), pki.ErrRollbackUnsafe, strings.Join(untrustingWorkers, ", "), bin, forwardFromSignNew)
	case len(refused) > 0:
		return fmt.Errorf("certs: do not roll back any node: %s cannot roll back (%w). Rolling back the others would leave the cluster split across two roots. "+
			"Finish the rotation forward instead: %s",
			strings.Join(refused, ", "), pki.ErrRollbackUnsafe, forwardFromSignNew)
	}
	fmt.Fprintln(w, "every node can roll back; run `stratum certs rotate-ca --rollback --yes` on each control plane and then on each worker")
	return nil
}

// forwardFromSignNew is the forward path once every node trusts the new CA.
const forwardFromSignNew = "run `stratum certs rotate-ca --phase=sign-new --yes` on each control plane and then each worker where it has not completed, " +
	"then follow \"Finishing a rotation by hand\" in the certificate-rotation documentation"

// lastLine is the last non-empty line of a Job log: the refusal, which the
// node prints last.
func lastLine(log string) string {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return "(no output)"
}

// IsRollbackUnsafe reports whether err is a refusal to roll back, from this
// node's check or from the cluster-wide one, so a front end can exit with
// ExitRollbackUnsafe.
func IsRollbackUnsafe(err error) bool { return errors.Is(err, pki.ErrRollbackUnsafe) }

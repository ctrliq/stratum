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

const (
	PhaseTrustNew = "trust-new"
	PhaseSignNew  = "sign-new"
	PhaseFinalize = "finalize"
)

type CARotateOptions struct {
	// rotate even though an old CA or SA key is gone (compromise recovery); ca.crt is still required
	Force   bool
	Phase   string
	Cluster bool
	// follower CP, trust-new: pull the initiator's staged material so every CP promotes one new CA
	FetchShared bool
	Quiet       bool

	RestartWorkloads  bool
	SkipWorkloadCheck bool

	SkipTokenCheck bool

	RunID string

	resumeStaging bool
}

// CheckWorkloadFlags is one check, for the CLI and the MCP tool both.
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

// CARotatePhases is the list both front ends name the phases from, in the only order they may run.
var CARotatePhases = []string{PhaseTrustNew, PhaseSignNew, PhaseFinalize}

// CARotateFlags is the raw flag set both the CLI and the MCP tool carry.
type CARotateFlags struct {
	Cluster     bool
	Phase       string
	Force       bool
	Rollback    bool
	FetchShared bool
	// with Rollback: report whether this node (with Cluster, every node) can roll back
	Check bool

	RestartWorkloads  bool
	SkipWorkloadCheck bool
	SkipTokenCheck    bool
}

// Validate lives here so both front ends refuse the same requests with the same words.
func (f CARotateFlags) Validate() error {
	const cmd = "stratum certs rotate-ca"
	switch {
	case f.Cluster && f.Phase != "":
		return fmt.Errorf("%s: use either --cluster (all phases) or --phase (one node), not both", cmd)
	case f.Check && !f.Rollback:
		return fmt.Errorf("%s: --check goes with --rollback: it reports whether this node (with --cluster, every node) can roll back, and changes nothing", cmd)
	case f.Rollback && f.Cluster && !f.Check:
		// A rollback is deliberately per node: the cluster-wide form is the read-only check.
		return fmt.Errorf("%s: a rollback runs per node. Run `%s --rollback --cluster --check` first, which confirms every node can roll back, "+
			"then `%s --rollback --yes` on each control plane and then on each worker", cmd, cmd, cmd)
	case f.Phase == "" && !f.Cluster && !f.Rollback:
		return fmt.Errorf("%s: phase is required: pass --cluster (drive all phases) or --phase=<%s> (single node)",
			cmd, strings.Join(CARotatePhases, "|"))
	case f.Phase != "" && !slices.Contains(CARotatePhases, f.Phase):
		return fmt.Errorf("%s: unknown phase %q: want one of %s", cmd, f.Phase, strings.Join(CARotatePhases, ", "))
	case f.FetchShared && f.Phase != PhaseTrustNew:
		return fmt.Errorf("%s: --fetch-shared only applies to --phase=%s", cmd, PhaseTrustNew)
	}
	return nil
}

// Options keeps a front end from assembling CARotateOptions by hand, drifting from the validator.
func (f CARotateFlags) Options() CARotateOptions {
	return CARotateOptions{
		Phase: f.Phase, Cluster: f.Cluster, Force: f.Force, FetchShared: f.FetchShared,
		RestartWorkloads: f.RestartWorkloads, SkipWorkloadCheck: f.SkipWorkloadCheck,
		SkipTokenCheck: f.SkipTokenCheck,
	}
}

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

func isWorker() bool { return !staticpod.Exists("kube-apiserver") }

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

func checkCARotationState(o CARotateOptions, node string) error {
	if o.FetchShared {
		return nil
	}
	rot := pki.ReadCARotation()
	if rot.Staged {
		if o.Force && pki.ForcedRotation() {
			return nil
		}
		if o.Phase == PhaseTrustNew && pki.ForcedRotation() {
			return stateRefusal{forcedStagedRefusal(node)}
		}
		// A widened bundle (or resumeStaging) proves the staging is this run's own; forced needs --force.
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

type stateRefusal struct{ error }

func (r stateRefusal) Unwrap() error { return r.error }

func (o CARotateOptions) phaseRunID() (string, error) {
	if o.RunID != "" {
		return o.RunID, nil
	}
	if !pki.CAFinalized() {
		return "", nil
	}
	return stagedRotationRunID()
}

func caPhaseApplied(phase string) (bool, error) {
	switch phase {
	case PhaseTrustNew:
		if pki.CAFinalized() {
			return true, nil
		}
		rot := pki.ReadCARotation()
		if rot.Staged {
			return false, nil
		}
		if !rot.Promoted {
			return false, nil
		}
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
	// The applied consult comes first, so a resumed rotation is not sent back to --rollback.
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
		// Before the fetch, which stages a root: a promoted node is refused with nothing written.
		if err := pki.PrepareStaging(o.Force); err != nil {
			return err
		}
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
	// Idempotent: a follower CP that received the initiator's material reuses the SAME CA.
	if err := pki.RotateCAMaterial(o.Force); err != nil {
		return err
	}
	if err := regenKubeconfigs(hostname, vip); err != nil {
		return err
	}
	castep(w, o, "Restart control plane (now trusts old‖new)")
	return trustNewRestartThen(w, o, func() error { return restartControlPlane(w) }, repinBestEffort)
}

func trustNewRestartThen(w io.Writer, o CARotateOptions, restart func() error, repin func(io.Writer, CARotateOptions)) error {
	err := restartThen(PhaseTrustNew, restart)
	repin(w, o)
	return err
}

func cpSignNew(w io.Writer, o CARotateOptions) error {
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

func signNewRestartThen(restart func() error) error {
	if err := restartThen(PhaseSignNew, restart); err != nil {
		return err
	}
	return pki.MarkCAPromoted()
}

func checkCPFinalize() error {
	if left := pki.CAStagedFiles(); len(left) > 0 {
		return fmt.Errorf("certs: %s refused: new CA material is still staged (%s), so this node's sign-new did not complete — "+
			"run --phase=%s first, or `stratum certs rotate-ca --rollback --yes` to abandon the rotation (see `stratum certs check`)",
			PhaseFinalize, strings.Join(left, ", "), PhaseSignNew)
	}
	// Not ca.pre-carotate.crt: one can outlive a rollback.
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
	// Re-check after the gate and the renewal, which can take minutes; state may have changed since.
	if err := checkCPFinalize(); err != nil {
		return err
	}
	castep(w, o, "Collapse trust to the new CA only")
	if err := pki.FinalizeCA(); err != nil {
		return err
	}
	// The marker survives the rotation; the next one must not read it as its own progress.
	markCarotateFinishedRun(w, runID)
	if err := regenKubeconfigs(hostname, vip); err != nil {
		return err
	}
	castep(w, o, "Restart control plane (drop old trust)")
	return restartThenCleanup(w, o, PhaseFinalize, func() error { return restartControlPlane(w) }, removeLeftoverCarotateKey, repinBestEffort)
}

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

var (
	kubeletServesWait = 60 * time.Second
	kubeletServesPoll = 2 * time.Second
)

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

// localKubeletServes is a var so a test can stand in for the running kubelet.
var localKubeletServes = func(newCA *x509.Certificate) error {
	_, ips, _, _, err := hostNodeIP()
	if err != nil {
		return err
	}
	ip, _, _ := strings.Cut(ips, ",")
	res := pki.ProbeKubelets(context.Background(), []pki.KubeletTarget{{IP: ip}}, []*x509.Certificate{newCA}, probeKubelet)
	return res[0]
}

func restartThenCleanup(w io.Writer, o CARotateOptions, phase string, restart func() error, cleanup func(io.Writer), repin func(io.Writer, CARotateOptions)) error {
	err := restartThen(phase, restart)
	cleanup(w)
	repin(w, o)
	return err
}

func restartThen(phase string, restart func() error) error {
	err := restart()
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w\n  %s: files are written on this node but the restart did not finish", err, phase)
}

func workerRefreshTrust(w io.Writer, o CARotateOptions, intent trustIntent) (err error) {
	p := workerTrustPaths()
	castep(w, o, "Refresh CA trust bundle from cluster-info")
	server, serr := refreshServer(p)
	if serr != nil {
		return serr
	}
	// Every failing check runs before the first mutation, so a refused refresh changes no file.
	u, perr := planTrustRefresh(server, intent, p)
	if perr != nil {
		return fmt.Errorf("worker %s: %w — the on-disk bundle and kubeconfigs are unchanged", intent, perr)
	}
	// A cleanup failure leaves credential-bearing temp files behind, so it joins the returned error.
	defer func() {
		if cerr := u.abort(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("worker %s: staged trust files remain on disk and must be removed by hand: %w", intent, cerr))
		}
	}()
	castep(w, o, "Apply refreshed trust: "+u.summary())
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

func checkWorkerReissue() error {
	args, err := runningKubeletArgs()
	if err != nil {
		return err
	}
	return checkWorkerReissueConfig(args, workerTrustPaths(), kubeletClientCertPath())
}

// kubeletArgs is a var so a test can drive the finalize renewal without a supervisor.
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
	// Both flag spellings are accepted; duplicates are refused rather than guessed.
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

func regenKubeconfigs(hostname, vip string) error {
	// Preserve the current endpoint (LB :7445 vs :6443) like leaf rotation does.
	server, err := kubeconfig.CurrentServer(version.AdminConf)
	if err != nil {
		server = fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)
	}
	return writeKubeconfigs(hostname, server, vip)
}

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

const keyHoldsCAKeys = "        It holds the AES key that decrypts the shared cluster CA material (ca.key, sa.key,\n" +
	"        front-proxy-ca.key); until it is gone, anyone with `get secrets` in %s can decrypt them.\n"

// carotateRunMarker lives outside carotateScratch so a retried finalize can still prove its key.
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

func signerPromoted() bool {
	return pki.ReadCARotation().Promoted
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func RollbackCA(w io.Writer, o CARotateOptions) error {
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
		// trustEither, not a Phase lookup: the CLI accepts --rollback --phase=finalize.
		return workerRefreshTrust(w, o, trustEither)
	}
	if err := rollbackCPMaterial(w, o); err != nil {
		return err
	}
	castep(w, o, "Restart control plane")
	// A halted --cluster run can leave stratum-carotate, which holds the CA key-encryption key.
	return restartThenCleanup(w, o, "rollback", func() error { return restartControlPlane(w) }, removeLeftoverCarotateKey, repinBestEffort)
}

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
	// The rollback unwinds the rotation this node's finalize recorded.
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
	// The backups are what tells a retry that the leaves still need reissuing, so they go last.
	return pki.RemoveRotationScratch()
}

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

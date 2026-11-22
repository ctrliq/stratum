// Package k8sutil holds small host-side helpers shared across the stratum subcommands.
package k8sutil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// Labels/annotations the upgrade machinery reads and writes:
const (
	ControlPlaneLabel = "stratum.ciq.com/control-plane"
	VersionAnnotation = "stratum.ciq.com/k8s-version"
	// The stratum build the node runs, shown for it in cluster views.
	StratumVersionAnnotation = "stratum.ciq.com/version"
	// Still set once the kubelet reports it: the node was interrupted mid-upgrade.
	UpgradeInProgressAnnotation = "stratum.ciq.com/upgrade-in-progress"
	// "<kind>@<holder>": kind is the target version, "preexisting", or "released".
	UpgradeCordonAnnotation = "stratum.ciq.com/upgrade-cordoned"
)

// Labels <prefix><handler>="true" per declared handler, for RuntimeClass selection.
const RuntimeHandlerLabelPrefix = "runtime.stratum.ciq.com/"

// Only the lease holder reconciles; the dashboard's SPA reads the same Lease.
const OperatorLeaseName = "stratum-operator"

// API group and version of the operator's HelmChart CRs; the dashboard's SPA shares them.
const (
	HelmChartGroup   = "stratum.ciq.com"
	HelmChartVersion = "v1"
)

// Set by uninstall to the stop time, so the operator can remove the Node once the worker is gone.
const UninstalledAnnotation = "stratum.ciq.com/uninstalled"

// The admission policy letting only a node write UninstalledAnnotation on itself.
const NodeMarkPolicy = "stratum-node-uninstall-mark"

// The admission policy holding a control-plane join token to its own maintenance fence.
const JoinFencePolicy = "stratum-join-maintenance-fence"

// Nanosecond RFC 3339, the lease renewal's precision.
func UninstallMark(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// The auth group token.Create puts every joiner in; the group the CSR bindings name.
const BootstrapGroup = "system:bootstrappers:stratum:nodes"

// Kept present with no subjects; see DisableNodeClientAutoApprove for why it is never deleted.
const NodeClientAutoApproveBinding = "stratum-csr-autoapprove"

// The only group ha.UploadCerts grants read on the shared-certs Secret.
const ControlPlaneBootstrapGroup = "system:bootstrappers:stratum:control-planes"

// Writes the progress header through w, never implicitly to the MCP stdio JSON-RPC stream.
func Step(w io.Writer, msg string) { fmt.Fprintf(w, "\n\033[1;32m==>\033[0m %s\n", msg) }

// Taken at the command seams only; internal/ha helpers run mid-fence and must not refuse it.
const (
	// An older binary follows only the literal, repeating in internal/certs, internal/upgrade and ha.
	maintenanceLeaseName      = "stratum-cluster-maintenance"
	maintenanceLeaseNamespace = "kube-system"
)

// host/pid/nanoseconds, so two runs on one host are distinct holders.
func MaintenanceHolder() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "unknown-host"
	}
	return fmt.Sprintf("%s/%d/%d", h, os.Getpid(), time.Now().UnixNano())
}

// Confirm before each mutation: a pause or API outage can let a successor take the leases.
type MaintenanceLease struct {
	Guard   *maintlease.Guard
	Release func()
}

func (l *MaintenanceLease) Confirm(ctx context.Context) error {
	if l == nil {
		return nil
	}
	return l.Guard.Confirm(ctx)
}

// Each attempt renews as the same holder, so success proves nobody held the leases meanwhile.
func (l *MaintenanceLease) ConfirmWithin(ctx context.Context, budget time.Duration) error {
	if l == nil {
		return nil
	}
	return l.Guard.ConfirmWithin(ctx, budget)
}

// Takes the fence, refusing rather than waiting; release must run on every exit.
func MaintenanceFence(cs kubernetes.Interface) (*MaintenanceLease, error) {
	return MaintenanceFenceAs(cs, MaintenanceHolder())
}

// Takes the fence under the holder given; a join's token takes it as JoinMaintenanceHolder.
func MaintenanceFenceAs(cs kubernetes.Interface, holder string) (*MaintenanceLease, error) {
	guard, release, err := maintlease.AcquireCompatible(context.Background(), cs,
		maintenanceLeaseNamespace, maintenanceLeaseName, holder)
	if err != nil {
		return nil, err
	}
	return &MaintenanceLease{Guard: guard, Release: release}, nil
}

// The holder prefix JoinFencePolicy admits a join token's lease writes under.
func JoinMaintenanceHolder(tokenID string) string {
	return "system:bootstrap:" + tokenID + "/" + MaintenanceHolder()
}

// Matches the admission dispatcher's wording; the policy sets no reason, so the status is Invalid.
func JoinFenceDenied(err error) bool {
	var st apierrors.APIStatus
	return errors.As(err, &st) && strings.Contains(st.Status().Message, "ValidatingAdmissionPolicy '"+JoinFencePolicy+"'")
}

func AdminMaintenanceFence() (*MaintenanceLease, error) {
	cs, err := AdminClientset()
	if err != nil {
		return nil, err
	}
	return MaintenanceFence(cs)
}

// super-admin.conf, or admin.conf where super-admin.conf is not yet written.
func InternalAdminConf() string {
	if _, err := os.Stat(version.SuperAdminConf()); err == nil {
		return version.SuperAdminConf()
	}
	return version.AdminConf
}

func Kubectl(args ...string) *exec.Cmd { return KubectlContext(context.Background(), args...) }

func KubectlContext(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, filepath.Join(version.BinDir, "kubectl"), args...)
	c.Env = append(os.Environ(), "KUBECONFIG="+InternalAdminConf())
	return c
}

func AdminRESTConfig() (*rest.Config, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", InternalAdminConf())
	if err != nil {
		return nil, fmt.Errorf("load admin kubeconfig: %w", err)
	}
	return cfg, nil
}

func AdminClientset() (*kubernetes.Clientset, error) {
	cfg, err := AdminRESTConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// The v3 mTLS env `stratum etcdctl` and the etcdsnap snapshot save path share.
func EtcdctlEnv() []string {
	return []string{
		// No ETCDCTL_API=3: the bundled v3-only etcdctl errors on it on stderr.
		fmt.Sprintf("ETCDCTL_ENDPOINTS=https://127.0.0.1:%d", version.EtcdClientPort),
		"ETCDCTL_CACERT=" + etcdCACertPath(),
		"ETCDCTL_CERT=" + version.PKIDir + "/apiserver-etcd-client.crt",
		"ETCDCTL_KEY=" + version.PKIDir + "/apiserver-etcd-client.key",
	}
}

// Prefers pki.EtcdTrustFile's etcd CA bundle; ca.crt remains a blind fallback for a PKI-less node.
func etcdCACertPath() string {
	if p, err := pki.EtcdTrustFile(); err == nil {
		return p
	}
	return filepath.Join(version.PKIDir, "ca.crt")
}

// Polls until ok or attempts × HealthzPollInterval; callers size deadlines from that window.
func WaitHealthz(attempts int) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(attempts)*HealthzPollInterval)
	defer cancel()
	return WaitHealthzContext(ctx)
}

func WaitHealthzContext(ctx context.Context) error {
	return waitHealthz(ctx, func(ctx context.Context) ([]byte, error) {
		return KubectlContext(ctx, "get", "--raw", "/healthz").Output()
	})
}

// A refused credential ends the wait at once; waiting cannot grant it the rights.
func WaitHealthzClient(ctx context.Context, cs kubernetes.Interface) error {
	return waitHealthz(ctx, func(ctx context.Context) ([]byte, error) {
		return cs.Discovery().RESTClient().Get().AbsPath("/healthz").DoRaw(ctx)
	})
}

// One hung probe must cost one attempt, not the whole wait window.
const healthzAttemptTimeout = 10 * time.Second

const HealthzPollInterval = 2 * time.Second

// One probe under its own deadline, so a hang costs one attempt inside the window.
func probeAttempt(ctx context.Context, probe func(context.Context) ([]byte, error)) ([]byte, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, healthzAttemptTimeout)
	defer cancel()
	// A deadline-killed kubectl exits without wrapping DeadlineExceeded, so test the ctxs.
	out, err := probe(attemptCtx)
	if err != nil && ctx.Err() == nil && attemptCtx.Err() != nil {
		return out, fmt.Errorf("no answer within %s: %w", healthzAttemptTimeout, err)
	}
	return out, err
}

func waitHealthz(ctx context.Context, probe func(context.Context) ([]byte, error)) error {
	var last error
	for {
		if ctx.Err() != nil {
			if last != nil {
				return fmt.Errorf("apiserver /healthz did not become ok (last check: %v): %w", last, ctx.Err())
			}
			return fmt.Errorf("apiserver /healthz did not become ok: %w", ctx.Err())
		}
		out, err := probeAttempt(ctx, probe)
		if strings.TrimSpace(string(out)) == "ok" {
			return nil
		}
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return fmt.Errorf("apiserver /healthz refused the credential in use, which needs get on the "+
				"nonResourceURL /healthz: %w", err)
		}
		if ctx.Err() == nil {
			if err != nil {
				last = err
			} else {
				last = fmt.Errorf("answered %q", strings.TrimSpace(string(out)))
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(HealthzPollInterval):
		}
	}
}

// Ends with ctx at a poll boundary; no sleep follows the last attempt.
func WaitNodeRegistered(ctx context.Context, hostname string, attempts int) error {
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("node %s did not register: %w", hostname, err)
		}
		if KubectlContext(ctx, "get", "node", hostname).Run() == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("node %s did not register: %w", hostname, err)
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("node %s did not register: %w", hostname, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("node %s did not register", hostname)
}

// Best-effort, needs admin.conf (control-plane); ctx frees the run group from a hung apiserver.
func LabelControlPlaneContext(ctx context.Context, node string) error {
	return KubectlContext(ctx, "label", "node", node, ControlPlaneLabel+"=true", "--overwrite").Run()
}

// Unbounded; new callers want LabelControlPlaneContext.
func LabelControlPlane(node string) error {
	return LabelControlPlaneContext(context.Background(), node)
}

// Appended to the kubelet's version by the build (KUBE_GIT_VERSION in build/components/Makefile).
const skeVersionSuffix = "-stratum"

// Trims the build suffix; anything else after "-" is an upstream prerelease and stays.
func ParseVersion(s string) (string, error) {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "v") && strings.Count(f, ".") >= 1 {
			return strings.TrimSuffix(f, skeVersionSuffix), nil
		}
	}
	return "", fmt.Errorf("could not parse a version from %q", strings.TrimSpace(s))
}

// Kubelet-only on purpose: the control plane is static pods, no apiserver binary on disk.
func KubeletVersion() (string, error) { return KubeletVersionContext(context.Background()) }

// Stdout goes to an unlinked file: a pipe keeps Wait copying until every child holding it exits.
func probeOutput(ctx context.Context, path string, args ...string) ([]byte, error) {
	f, err := os.CreateTemp("", "stratum-probe-")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_ = os.Remove(f.Name())
	c := exec.CommandContext(ctx, path, args...)
	c.Stdout = f
	if err := c.Run(); err != nil {
		return nil, err
	}
	// Read by position: a child that outlived the probe shares f's offset.
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.NewSectionReader(f, 0, fi.Size()))
}

func KubeletVersionContext(ctx context.Context) (string, error) {
	out, err := probeOutput(ctx, filepath.Join(version.BinDir, "kubelet"), "--version")
	if err != nil {
		return "", fmt.Errorf("kubelet --version: %w", err)
	}
	return ParseVersion(string(out))
}

// Vars so tests can point them at stand-in binaries.
var (
	stratumPath = version.StratumInstallPath
	selfExe     = os.Executable
)

// Asked, not read from this build, so an override or another binary answers truthfully.
func BinaryVersion(path string) (string, error) {
	return BinaryVersionContext(context.Background(), path)
}

func BinaryVersionContext(ctx context.Context, path string) (string, error) {
	out, err := probeOutput(ctx, path, "version")
	if err != nil {
		return "", fmt.Errorf("%s version: %w", path, err)
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", fmt.Errorf("%s version: no output", path)
	}
	return f[0], nil
}

// The running build is stale after an upgrade; only a first `up` uses this process's version.
func installedStratumVersion() (string, error) {
	return installedStratumVersionContext(context.Background())
}

func installedStratumVersionContext(ctx context.Context) (string, error) {
	if _, err := os.Stat(stratumPath); errors.Is(err, fs.ErrNotExist) {
		exe, err := selfExe()
		if err != nil {
			return "", fmt.Errorf("find the running stratum: %w", err)
		}
		return BinaryVersionContext(ctx, exe)
	}
	return BinaryVersionContext(ctx, stratumPath)
}

// Reads the installed binaries, not this build, so a supervisor stamps what is now on disk.
func StampNode(node string) error { return StampNodeContext(context.Background(), node) }

func StampNodeContext(ctx context.Context, node string) error {
	return stamp(ctx, node, func(k8s, stratum string) error {
		c := KubectlContext(ctx, stampArgs(node, k8s, stratum)...)
		var stderr bytes.Buffer
		c.Stderr = &stderr
		if err := c.Run(); err != nil {
			if msg := capStderr(stderr.String()); msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
			return err
		}
		return nil
	})
}

const maxStderr = 200

func capStderr(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxStderr {
		return string(r[:maxStderr]) + "..."
	}
	return s
}

// A worker has no admin.conf; the Node authorizer allows this patch via the kubelet credential.
func StampOwnNode(node string) error {
	cs, err := kubeletClientset()
	if err != nil {
		return fmt.Errorf("stamp %s: %w", node, err)
	}
	return stamp(context.Background(), node, func(k8s, stratum string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := cs.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, stampPatch(k8s, stratum), metav1.PatchOptions{})
		return err
	})
}

func OwnNodeUID(ctx context.Context, node string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cs, err := kubeletClientsetContext(ctx)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	n, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(n.UID), nil
}

func OwnNodeTaints(ctx context.Context, node string) ([]corev1.Taint, error) {
	cs, err := kubeletClientset()
	if err != nil {
		return nil, fmt.Errorf("read %s's taints: %w", node, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	n, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read %s's taints: %w", node, err)
	}
	return n.Spec.Taints, nil
}

// One deadline covers every attempt, sleep and request.
func WaitOwnNodeRegistered(node string, timeout time.Duration) error {
	err := pollUntil(timeout, 3*time.Second, func(ctx context.Context) error {
		cs, err := kubeletClientset()
		if err != nil {
			return err
		}
		_, err = cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("node %s did not register within %s: %w", node, timeout, err)
	}
	return nil
}

func pollUntil(timeout, interval time.Duration, check func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		err := check(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(interval):
		}
	}
}

func kubeletClientset() (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig"))
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 15 * time.Second
	return kubernetes.NewForConfig(cfg)
}

func stamp(ctx context.Context, node string, apply func(k8s, stratum string) error) error {
	k8s, err := KubeletVersionContext(ctx)
	if err != nil {
		return fmt.Errorf("stamp %s: %w", node, err)
	}
	stratum, verr := installedStratumVersionContext(ctx)
	if err := apply(k8s, stratum); err != nil {
		return err
	}
	if verr != nil {
		return fmt.Errorf("stamped %s on %s but not %s: %w", VersionAnnotation, node, StratumVersionAnnotation, verr)
	}
	return nil
}

func stampPatch(k8s, stratum string) []byte {
	ann := map[string]string{VersionAnnotation: k8s}
	if stratum != "" {
		ann[StratumVersionAnnotation] = stratum
	}
	b, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	return b
}

func stampArgs(node, k8s, stratum string) []string {
	args := []string{"annotate", "node", node, VersionAnnotation + "=" + k8s}
	if stratum != "" {
		args = append(args, StratumVersionAnnotation+"="+stratum)
	}
	return append(args, "--overwrite")
}

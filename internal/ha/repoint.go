package ha

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// NodeRepoint moves an existing node's clients onto the cluster VIP
// (kube-vip's one address serves in-cluster and external clients): it rewrites
// this node's kubeconfigs to https://<vip>:6443 and restarts the local
// consumers. Used when a cluster that started as a single controller (clients
// on 127.0.0.1) grows into HA. There is no per-worker proxy; kube-vip's one
// address takes its place. It changes nothing unless the VIP already serves
// this cluster's apiserver with a certificate that covers it. A consumer that
// fails to restart comes back as a warning beside a nil error — the
// kubeconfigs were rewritten, so the repoint landed — never as a failure.
func NodeRepoint(w io.Writer, vip string) ([]string, error) {
	return nodeRepoint(w, vip, RepointKubeconfigs)
}

func nodeRepoint(w io.Writer, vip string, repoint func(string) error) ([]string, error) {
	if vip == "" {
		return nil, fmt.Errorf("ha-repoint: --vip is required")
	}
	// Before the kubeconfigs and the node state are rewritten: the static-pod
	// restarts below render from the node's profile and would stop on one
	// this binary does not register only after the rewrite.
	if err := nodestate.RequireNodeProfile("ha-repoint"); err != nil {
		return nil, err
	}
	server := "https://" + net.JoinHostPort(vip, strconv.Itoa(version.APIServerPort))
	k8sutil.Step(w, "Check that the VIP serves this cluster's apiserver")
	if err := VIPServes(vip); err != nil {
		return nil, vipCheckFailed(vip, err)
	}
	k8sutil.Step(w, "Repoint this node's kubeconfigs to "+server)
	if err := repoint(server); err != nil {
		return nil, err
	}
	// Restart whichever consumers exist on this node (cm/scheduler are present only
	// on control planes). Best-effort, but REPORT failures: a kubelet that
	// does not come back after its kubeconfig was rewritten leaves the node
	// NotReady, and a silent success here made a broken repoint indistinguishable
	// from a working one. Each failure is printed and returned as a warning
	// beside the nil error: the same sentence once for the operator, once as
	// data the caller can report without matching stdout text.
	var warnings []string
	k8sutil.Step(w, "Restart local API consumers onto the VIP")
	if err := comps.Restart("kubelet"); err != nil {
		w2 := fmt.Sprintf("WARN: restart kubelet: %v (this node may go NotReady; check the kubelet log)", err)
		fmt.Fprintf(w, "  %s\n", w2)
		warnings = append(warnings, w2)
	}
	for _, name := range []string{"kube-controller-manager", "kube-scheduler"} {
		if staticpod.Exists(name) {
			if err := staticpod.Restart(name); err != nil {
				w2 := fmt.Sprintf("WARN: restart %s: %v", name, err)
				fmt.Fprintf(w, "  %s\n", w2)
				warnings = append(warnings, w2)
			}
		}
	}
	fmt.Fprintf(w, "ha-repoint: this node's clients now target %s\n", server)
	return warnings, nil
}

// RepointCilium points every Cilium agent + operator at the VIP instead of a
// single control-plane IP, so the CNI survives any single control-plane
// failure. Central, run ONCE after the VIP exists. (Cilium's kube-proxy-free
// agents dial the apiserver directly and cannot use the in-cluster Service
// before they are up, so the address must be highly available itself.) It
// writes through this node's admin.conf. A rollout that does not finish is a
// warning beside a nil error: the env change is already applied.
func RepointCilium(w io.Writer, vip string) ([]string, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return nil, fmt.Errorf("repoint cilium: %w", err)
	}
	return RepointCiliumWith(context.Background(), w, cs, vip)
}

// RepointCiliumWith is RepointCilium through cs, so a caller with its own
// credential is bounded by that credential's RBAC. A rollout that does not
// finish in time is returned as a warning beside the nil error — the change
// is applied and live, so the repoint is not a failure — naming what each
// unfinished rollout last reported. Like NodeRepoint, it changes nothing
// unless the VIP serves this cluster's apiserver, and its progress goes
// through the writer it is handed first (after its context).
func RepointCiliumWith(ctx context.Context, w io.Writer, cs kubernetes.Interface, vip string) ([]string, error) {
	k8sutil.Step(w, "Check that the VIP serves this cluster's apiserver")
	if err := VIPServes(vip); err != nil {
		return nil, vipCheckFailed(vip, err)
	}
	k8sutil.Step(w, fmt.Sprintf("Repoint Cilium (agents + operator) at the VIP %s", vip))
	if err := setCiliumAPIServer(ctx, cs, vip); err != nil {
		return nil, err
	}
	var warnings []string
	for _, r := range []struct {
		target  string
		timeout time.Duration
		status  func(context.Context) (bool, string, error)
	}{
		{"ds/cilium", ciliumRolloutTimeout, func(ctx context.Context) (bool, string, error) {
			return daemonSetRolledOut(ctx, cs, "cilium")
		}},
		{"deploy/cilium-operator", ciliumOperatorRolloutTimeout, func(ctx context.Context) (bool, string, error) {
			return deploymentRolledOut(ctx, cs, "cilium-operator")
		}},
	} {
		if err := waitRolledOut(ctx, r.timeout, r.status); err != nil {
			fmt.Fprintf(w, "  WARN: cilium %s did not roll out within %s: %v\n"+
				"        agents may be dialing a VIP a node cannot reach; check `stratum kubectl -n kube-system rollout status %s`\n",
				r.target, r.timeout, err, r.target)
			warnings = append(warnings, fmt.Sprintf("WARN: cilium %s did not roll out within %s: %v", r.target, r.timeout, err))
		}
	}
	if len(warnings) > 0 {
		return warnings, nil
	}
	fmt.Fprintf(w, "ha-repoint --cilium: Cilium now targets the VIP %s\n", vip)
	return nil, nil
}

// How long each Cilium rollout may take.
const (
	ciliumRolloutTimeout         = 300 * time.Second
	ciliumOperatorRolloutTimeout = 180 * time.Second
)

// rolloutPoll is how often a rollout is checked, a var for tests.
var rolloutPoll = 2 * time.Second

// errProgressDeadline is a Deployment's own verdict that its rollout has
// stalled.
var errProgressDeadline = errors.New("exceeded its progress deadline")

// waitRolledOut polls status until it reports done or timeout passes. An
// error waiting cannot clear (refused, unauthorized, not found, or a stalled
// Deployment) ends the wait at once. A timeout names the last progress seen.
func waitRolledOut(ctx context.Context, timeout time.Duration, status func(context.Context) (bool, string, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last string
	var lastErr error
	for {
		done, progress, err := status(ctx)
		switch {
		case err == nil && done:
			return nil
		case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err), apierrors.IsNotFound(err), errors.Is(err, errProgressDeadline):
			return err
		case err == nil:
			last, lastErr = progress, nil
		case ctx.Err() == nil:
			lastErr = err
		}
		select {
		case <-ctx.Done():
			switch {
			case lastErr != nil:
				return fmt.Errorf("still rolling out (last check: %v): %w", lastErr, ctx.Err())
			case last != "":
				return fmt.Errorf("still rolling out (%s): %w", last, ctx.Err())
			}
			return fmt.Errorf("still rolling out: %w", ctx.Err())
		case <-time.After(rolloutPoll):
		}
	}
}

// daemonSetRolledOut reports what `kubectl rollout status` waits for: the
// controller has seen the latest spec and every scheduled pod is updated and
// available. The string is the progress so far.
func daemonSetRolledOut(ctx context.Context, cs kubernetes.Interface, name string) (bool, string, error) {
	ds, err := cs.AppsV1().DaemonSets("kube-system").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, "", err
	}
	st := ds.Status
	progress := fmt.Sprintf("generation %d observed %d, %d of %d updated, %d available",
		ds.Generation, st.ObservedGeneration, st.UpdatedNumberScheduled, st.DesiredNumberScheduled, st.NumberAvailable)
	return st.ObservedGeneration >= ds.Generation &&
		st.UpdatedNumberScheduled == st.DesiredNumberScheduled &&
		st.NumberAvailable == st.DesiredNumberScheduled, progress, nil
}

// deploymentRolledOut is daemonSetRolledOut for a Deployment, which also
// fails at once when the Deployment reports its progress deadline exceeded.
func deploymentRolledOut(ctx context.Context, cs kubernetes.Interface, name string) (bool, string, error) {
	d, err := cs.AppsV1().Deployments("kube-system").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, "", err
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	st := d.Status
	progress := fmt.Sprintf("generation %d observed %d, %d of %d updated, %d available",
		d.Generation, st.ObservedGeneration, st.UpdatedReplicas, want, st.AvailableReplicas)
	for _, c := range st.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return false, progress, fmt.Errorf("deployment %q %w (%s)", name, errProgressDeadline, progress)
		}
	}
	return st.ObservedGeneration >= d.Generation &&
		st.UpdatedReplicas == want && st.Replicas == want && st.AvailableReplicas == want, progress, nil
}

// RepointKubeconfigs rewrites every kubeconfig on this node to the given
// server URL, preserving embedded credentials, and records that endpoint in
// the node state's Server — the record every kubeconfig writer reads, so the
// next `stratum up` writes kubeconfigs from the repointed endpoint instead of
// the stale one and the repoint survives the restart. That includes
// bootstrap.conf: it is never deleted, workers hand it to the kubelet on every
// start, and a kubelet that has to re-bootstrap would otherwise dial the old
// server. A node with no state file has nothing to record (workers keep their
// join server); a state that cannot be read is an error, never a silent skip.
func RepointKubeconfigs(server string) error {
	// The record and the rewrites land under one data-lock acquisition, so
	// two concurrent repoints cannot interleave into files that target one
	// endpoint under a state that records another — A writes endpoint X, B
	// writes and records Y, then A records X, and the next start's recorded
	// server discards B's deliberate repoint. A node with no state file has
	// nothing to record (workers keep their join server): its files rewrite
	// without the lock, which guards the state the worker does not have.
	if _, err := os.Stat(nodestate.StatePath()); errors.Is(err, fs.ErrNotExist) {
		return repointKubeconfigFiles(server)
	}
	if err := nodestate.Mutate(func(st *nodestate.NodeState) error {
		if err := repointKubeconfigFiles(server); err != nil {
			return err
		}
		// Mutate supplies an empty state when no file exists; a repoint must
		// not turn a worker without a record into a new controller record.
		if _, err := os.Stat(nodestate.StatePath()); errors.Is(err, fs.ErrNotExist) {
			return nodestate.ErrUnchanged
		} else if err != nil {
			return err
		}
		if st.Server == server {
			return nodestate.ErrUnchanged
		}
		st.Server = server
		return nil
	}); err != nil {
		return fmt.Errorf("repoint: record %s in the node state %s: %w", server, nodestate.StatePath(), err)
	}
	return nil
}

func repointKubeconfigFiles(server string) error {
	for _, f := range repointKubeconfigs() {
		if _, err := os.Stat(f); err != nil {
			continue // workers have only a subset
		}
		if err := kubeconfig.SetServer(f, server); err != nil {
			return fmt.Errorf("repoint %s: %w", f, err)
		}
	}
	return nil
}

// setCiliumAPIServer sets KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT
// on every container and init container of the Cilium agent DaemonSet and
// operator Deployment, through the API rather than a command line.
func setCiliumAPIServer(ctx context.Context, cs kubernetes.Interface, vip string) error {
	ds := cs.AppsV1().DaemonSets("kube-system")
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o, err := ds.Get(ctx, "cilium", metav1.GetOptions{})
		if err != nil {
			return err
		}
		setAPIServerEnv(&o.Spec.Template.Spec, vip)
		_, err = ds.Update(ctx, o, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("repoint cilium (ds/cilium): %w", err)
	}
	deploy := cs.AppsV1().Deployments("kube-system")
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		o, err := deploy.Get(ctx, "cilium-operator", metav1.GetOptions{})
		if err != nil {
			return err
		}
		setAPIServerEnv(&o.Spec.Template.Spec, vip)
		_, err = deploy.Update(ctx, o, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("repoint cilium (deploy/cilium-operator): %w", err)
	}
	return nil
}

// setAPIServerEnv sets the apiserver address variables on every container of
// spec, replacing any value already there, as `kubectl set env` did.
func setAPIServerEnv(spec *corev1.PodSpec, vip string) {
	vars := []corev1.EnvVar{
		{Name: "KUBERNETES_SERVICE_HOST", Value: vip},
		{Name: "KUBERNETES_SERVICE_PORT", Value: strconv.Itoa(version.APIServerPort)},
	}
	set := func(cs []corev1.Container) {
		for i := range cs {
			for _, v := range vars {
				found := false
				for j := range cs[i].Env {
					if cs[i].Env[j].Name == v.Name {
						cs[i].Env[j] = v
						found = true
					}
				}
				if !found {
					cs[i].Env = append(cs[i].Env, v)
				}
			}
		}
	}
	set(spec.InitContainers)
	set(spec.Containers)
}

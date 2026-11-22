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

func NodeRepoint(w io.Writer, vip string) ([]string, error) {
	return nodeRepoint(w, vip, RepointKubeconfigs)
}

func nodeRepoint(w io.Writer, vip string, repoint func(string) error) ([]string, error) {
	if vip == "" {
		return nil, fmt.Errorf("ha-repoint: --vip is required")
	}
	// the pod restarts below render from the profile; the check must precede the rewrite
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
	// a failed restart must reach the caller: the kubeconfigs were already rewritten
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

// Agents dial the apiserver directly; run once, after the VIP exists; slow rollouts warn, not fail
func RepointCilium(w io.Writer, vip string) ([]string, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return nil, fmt.Errorf("repoint cilium: %w", err)
	}
	return RepointCiliumWith(context.Background(), w, cs, vip)
}

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

const (
	ciliumRolloutTimeout         = 300 * time.Second
	ciliumOperatorRolloutTimeout = 180 * time.Second
)

// a var so tests can shorten the poll
var rolloutPoll = 2 * time.Second

var errProgressDeadline = errors.New("exceeded its progress deadline")

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

// records the server every kubeconfig writer reads; bootstrap.conf is included for the kubelet
func RepointKubeconfigs(server string) error {
	// the rewrites and the record must land under one data-lock acquisition, or repoints interleave
	if _, err := os.Stat(nodestate.StatePath()); errors.Is(err, fs.ErrNotExist) {
		return repointKubeconfigFiles(server)
	}
	if err := nodestate.Mutate(func(st *nodestate.NodeState) error {
		if err := repointKubeconfigFiles(server); err != nil {
			return err
		}
		// Mutate invents an empty state when no file exists; a repoint must not create a controller record
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

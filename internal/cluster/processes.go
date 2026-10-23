package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/k8sutil"
	stratumpki "github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Processes renders the supervised host-process roster for this node. Since
// the control plane runs as kubelet-managed static pods
// (internal/staticpod), so the supervisor's roster is down to what has to be a
// host process: containerd, kubelet, and the operator on controllers.
// Ordering/readiness is expressed with WaitFor gates rather than
// After=/Requires=.
//
// operator is the cluster's spec.operator switch, the only cluster setting the
// roster reads. It is a bool rather than the whole ClusterConfig because a nil
// spec.operator means on: a caller that passed an empty config would have run
// the operator on a cluster that turned it off. A worker never runs the
// operator, whatever the switch.
func Processes(st *NodeState, operator bool) []supervisor.Process {
	bin := func(b string) string { return filepath.Join(version.BinDir, b) }
	pki := func(f string) string { return filepath.Join(version.PKIDir, f) }
	kc := func(f string) string { return filepath.Join(version.KubeconfigDir, f) }

	procs := []supervisor.Process{
		{Name: "containerd", Path: bin("containerd"), Args: []string{
			"--config", version.ContainerdConfig,
		},
			// The runc shim resolves "runc" from PATH; the payload's runc
			// lives in BinDir, not on the host PATH.
			Env:       []string{"PATH=" + version.BinDir + ":" + os.Getenv("PATH")},
			Socket:    strings.TrimPrefix(version.ContainerdSock, "unix://"),
			ExecLabel: selinux.SystemExecLabel()},
		kubeletProcess(st, pki, kc),
	}

	if st.Role == "controller" && operator {
		// The HelmChart + manifest-deployer controller runs as a
		// supervised host process — no operator image to pull, which keeps
		// the airgap story one-artifact.
		procs = append(procs, supervisor.Process{Name: "operator", Path: selfExe(),
			WaitFor: func(ctx context.Context) error { return waitAPIReady(ctx, 5*time.Minute) },
			Args: []string{
				"operator",
				"--kubeconfig", version.SuperAdminConf(),
				"--manifests-dir", version.ManifestsDir,
				"--helm-cache", version.HelmCacheDir,
				"--leader-elect",
				// Serves the requester-stamping admission webhook and registers this
				// node's endpoint so the apiserver can reach it.
				// Disabling all instances requires the explicit cluster-wide
				// webhook cleanup in docs/content/cluster-admin/hardening.md;
				// ordinary shutdown must not disable admission enforcement.
				"--node-ip", st.NodeIP,
				"--cluster-domain", st.Domain(),
			}})
		if st.VIP.Address != "" {
			op := &procs[len(procs)-1]
			op.Args = append(op.Args, "--apiserver-vip", st.VIP.Address)
		}
	}
	return procs
}

func kubeletProcess(st *NodeState, pki, kc func(string) string) supervisor.Process {
	args := []string{
		"--config=" + filepath.Join(version.K8sConfDir, "kubelet.conf"),
		"--node-ip=" + st.KubeletNodeIP,
		"--hostname-override=" + st.Hostname,
		// Kubelet state on the stratum data dir — a real filesystem wherever the
		// supervisor runs (see the containerd root comment in runtime.go).
		"--root-dir=" + filepath.Join(version.DataDir, "kubelet"),
	}
	// Workers need the same bootstrap and cert-directory flags after their
	// first bootstrap creates kubelet.kubeconfig, including after a host restart.
	if _, err := os.Stat(kc("kubelet.kubeconfig")); st.Role != "worker" && err == nil {
		args = append(args, "--kubeconfig="+kc("kubelet.kubeconfig"))
	} else {
		args = append(args,
			"--kubeconfig="+kc("kubelet.kubeconfig"), // written by TLS bootstrap
			"--bootstrap-kubeconfig="+version.BootstrapKubeconfig,
			"--cert-dir="+pki("kubelet"),
		)
	}
	return supervisor.Process{
		Name: "kubelet", Path: filepath.Join(version.BinDir, "kubelet"),
		// Bootstrap order: containerd up, payload images imported,
		// THEN kubelet — so the control-plane static pods it launches find
		// their images locally rather than crash-looping on pulls that can
		// never succeed on an airgapped node.
		WaitFor: func(ctx context.Context) error {
			if err := kubeletRuntimeReady(ctx); err != nil {
				return err
			}
			healKubeletServing(args)
			return nil
		},
		Args: args,
	}
}

// kubeletRuntimeReady is what the kubelet waits for before it starts:
// containerd up and the payload images imported. A var so a test can run the
// kubelet's WaitFor without them.
var kubeletRuntimeReady = func(ctx context.Context) error {
	if err := waitContainerd(ctx, 2*time.Minute); err != nil {
		return err
	}
	return importPayloadImages(ctx)
}

// healKubeletServing moves aside a bootstrap-issued serving cert that no CA in
// this node's ca-bundle.crt signed, so the kubelet requests a new one as it
// starts (pki.HealKubeletServingCert). It never fails the start.
func healKubeletServing(args []string) {
	roots, err := stratumpki.KubeletServingRoots()
	if err != nil {
		return
	}
	aside, err := stratumpki.HealKubeletServingCert(stratumpki.KubeletCertDir(args), roots)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "stratum: WARN: %v\n", err)
	case aside != "":
		fmt.Fprintf(os.Stderr, "stratum: the kubelet serving cert was not signed by the cluster CA; moved it to %s so the kubelet requests a new one\n", aside)
	}
}

// waitAPIReady gates apiserver clients on real readiness — the bootstrap RBAC
// configmap, not just /healthz (an apiserver answers /healthz before its
// bootstrap controllers have run, and a controller-manager started in that
// window exits immediately).
func waitAPIReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cs, err := k8sutil.AdminClientset()
		if err == nil {
			_, err = cs.CoreV1().ConfigMaps("kube-system").Get(ctx, "extension-apiserver-authentication", metav1.GetOptions{})
		}
		if err == nil {
			return nil
		}
		last = err
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("apiserver not ready after %v: %w", timeout, last)
}

func waitContainerd(ctx context.Context, timeout time.Duration) error {
	sock := strings.TrimPrefix(version.ContainerdSock, "unix://")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := os.Stat(sock); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("containerd socket %s not up after %v", sock, timeout)
}

func selfExe() string {
	exe, err := os.Executable()
	if err != nil {
		return version.StratumInstallPath
	}
	return exe
}

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

// The host processes a node's supervisor runs; the control plane is static pods.
func Processes(st *NodeState, operator bool) []supervisor.Process {
	bin := func(b string) string { return filepath.Join(version.BinDir, b) }
	pki := func(f string) string { return filepath.Join(version.PKIDir, f) }
	kc := func(f string) string { return filepath.Join(version.KubeconfigDir, f) }

	procs := []supervisor.Process{
		{Name: "containerd", Path: bin("containerd"), Args: []string{
			"--config", version.ContainerdConfig,
		},
			// The runc shim resolves runc from PATH; the payload's runc is only in BinDir.
			Env:       []string{"PATH=" + version.BinDir + ":" + os.Getenv("PATH")},
			Socket:    strings.TrimPrefix(version.ContainerdSock, "unix://"),
			ExecLabel: selinux.SystemExecLabel()},
		kubeletProcess(st, pki, kc),
	}

	if st.Role == "controller" && operator {
		// A host process: no operator image to pull, keeping the airgap one-artifact.
		procs = append(procs, supervisor.Process{Name: "operator", Path: selfExe(),
			WaitFor: func(ctx context.Context) error { return waitAPIReady(ctx, 5*time.Minute) },
			Args: []string{
				"operator",
				"--kubeconfig", version.SuperAdminConf(),
				"--manifests-dir", version.ManifestsDir,
				"--helm-cache", version.HelmCacheDir,
				"--leader-elect",
				// The apiserver reaches its admission webhook only through this endpoint.
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
		// Kubelet state needs a real filesystem wherever the supervisor runs.
		"--root-dir=" + filepath.Join(version.DataDir, "kubelet"),
	}
	// A worker keeps the bootstrap flags even once kubelet.kubeconfig exists.
	if _, err := os.Stat(kc("kubelet.kubeconfig")); st.Role != "worker" && err == nil {
		args = append(args, "--kubeconfig="+kc("kubelet.kubeconfig"))
	} else {
		args = append(args,
			"--kubeconfig="+kc("kubelet.kubeconfig"),
			"--bootstrap-kubeconfig="+version.BootstrapKubeconfig,
			"--cert-dir="+pki("kubelet"),
		)
	}
	return supervisor.Process{
		Name: "kubelet", Path: filepath.Join(version.BinDir, "kubelet"),
		// Kubelet starts after containerd and the payload images, or airgapped pods crash-loop.
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

// A var so a test can run the kubelet's WaitFor without them.
var kubeletRuntimeReady = func(ctx context.Context) error {
	if err := waitContainerd(ctx, 2*time.Minute); err != nil {
		return err
	}
	return importPayloadImages(ctx)
}

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

// Gates on the bootstrap RBAC configmap; /healthz answers before bootstrap completes.
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

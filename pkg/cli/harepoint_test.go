package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/version"
)

// TestHARepoint_WarnsWhenTheKubeletDoesNotRestart pins `stratum ha-repoint`'s
// best-effort contract: the kubeconfigs are rewritten first, and when a local
// consumer then fails to restart, the command still succeeds (the repoint
// landed) but says so on stdout — a kubelet that does not come back leaves the
// node NotReady, and a silent success would make a broken repoint
// indistinguishable from a working one. The kubeconfig paths are redirected
// with no files present (as on a node before the repoint, every kubeconfig
// absent so RepointKubeconfigs skips them all), and the supervisor's control
// socket points at an absent path so the kubelet restart fails.
func TestHARepoint_WarnsWhenTheKubeletDoesNotRestart(t *testing.T) {
	root := t.TempDir()
	oldDir, oldAdmin, oldBoot, oldConf, oldSock := version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig, version.K8sConfDir, version.ControlSocket
	t.Cleanup(func() {
		version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig, version.K8sConfDir, version.ControlSocket = oldDir, oldAdmin, oldBoot, oldConf, oldSock
	})
	version.KubeconfigDir = filepath.Join(root, "kubeconfig")
	version.AdminConf = filepath.Join(version.KubeconfigDir, "admin.conf")
	version.BootstrapKubeconfig = filepath.Join(version.KubeconfigDir, "bootstrap.conf")
	// No static-pod manifests under K8sConfDir, so the controller-manager and
	// scheduler restarts are skipped: only the kubelet warning can fire.
	version.K8sConfDir = filepath.Join(root, "config")
	version.ControlSocket = filepath.Join(root, "absent.sock")
	// The VIP serves the cluster: ha-repoint checks that before it rewrites
	// anything, and this test is about what happens after.
	oldServes := ha.VIPServes
	ha.VIPServes = func(string) error { return nil }
	t.Cleanup(func() { ha.VIPServes = oldServes })

	var err error
	out := captureStdout(t, func() {
		cmd := newHARepointCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--vip", "10.0.0.5"})
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatalf("ha-repoint returned %v; a failed best-effort restart must not fail the command", err)
	}
	if !strings.Contains(out, "WARN: restart kubelet") {
		t.Errorf("stdout does not warn that the kubelet did not restart:\n%s", out)
	}
	if !strings.Contains(out, "ha-repoint: this node's clients now target https://10.0.0.5:6443") {
		t.Errorf("stdout does not say the node's clients now target the VIP:\n%s", out)
	}
}

// TestHARepoint_KubeletWarningTravelsAsData pins the caller side of the repoint
// outcome contract: the warning the command prints (the pin above holds the
// printed text) must also come back as data beside a nil error, so a caller —
// the ha_repoint MCP tool especially — can report it without matching stdout
// text. Same environment as the printed-text pin: no kubeconfig files present,
// the supervisor's control socket absent so the kubelet restart fails, a VIP
// that serves, and no static-pod manifests so the kubelet is the only
// consumer that can warn.
func TestHARepoint_KubeletWarningTravelsAsData(t *testing.T) {
	root := t.TempDir()
	oldDir, oldConf, oldSock := version.KubeconfigDir, version.K8sConfDir, version.ControlSocket
	t.Cleanup(func() { version.KubeconfigDir, version.K8sConfDir, version.ControlSocket = oldDir, oldConf, oldSock })
	version.KubeconfigDir = filepath.Join(root, "kubeconfig") // no kubeconfigs: RepointKubeconfigs skips them all
	version.K8sConfDir = filepath.Join(root, "config")        // no manifests: controller-manager and scheduler are skipped
	version.ControlSocket = filepath.Join(root, "absent.sock")
	oldServes := ha.VIPServes
	ha.VIPServes = func(string) error { return nil }
	t.Cleanup(func() { ha.VIPServes = oldServes })

	var buf bytes.Buffer
	warnings, err := ha.NodeRepoint(&buf, "10.0.0.5")
	if err != nil {
		t.Errorf("NodeRepoint = %v; a failed best-effort restart must not fail the repoint", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "restart kubelet") {
		t.Errorf("warnings = %q, want one warning naming the kubelet restart", warnings)
	}
	// The warning stays printed exactly as today, through the writer the CLI
	// hands the operation; the returned list is data about the same event,
	// not a replacement for the operator-facing text.
	if !strings.Contains(buf.String(), "WARN: restart kubelet") {
		t.Errorf("the writer does not warn that the kubelet did not restart:\n%s", buf.String())
	}
}

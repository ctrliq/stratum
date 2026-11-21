package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/version"
)

// Pins the best-effort contract: the repoint lands, and a failed kubelet restart warns.
func TestHARepoint_WarnsWhenTheKubeletDoesNotRestart(t *testing.T) {
	root := t.TempDir()
	oldDir, oldAdmin, oldBoot, oldConf, oldSock := version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig, version.K8sConfDir, version.ControlSocket
	t.Cleanup(func() {
		version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig, version.K8sConfDir, version.ControlSocket = oldDir, oldAdmin, oldBoot, oldConf, oldSock
	})
	version.KubeconfigDir = filepath.Join(root, "kubeconfig")
	version.AdminConf = filepath.Join(version.KubeconfigDir, "admin.conf")
	version.BootstrapKubeconfig = filepath.Join(version.KubeconfigDir, "bootstrap.conf")
	// No static-pod manifests, so only the kubelet restart can warn.
	version.K8sConfDir = filepath.Join(root, "config")
	version.ControlSocket = filepath.Join(root, "absent.sock")
	// The VIP must serve before anything is rewritten; this test is about after.
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

// The warning also comes back as data beside a nil error, for callers that cannot match stdout.
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
	// The returned list is data about the same event, not a replacement for the text.
	if !strings.Contains(buf.String(), "WARN: restart kubelet") {
		t.Errorf("the writer does not warn that the kubelet did not restart:\n%s", buf.String())
	}
}

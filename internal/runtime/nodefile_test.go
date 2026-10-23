package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// TestKubeletConfigIsAlways0600: writeKubeletConfig leaves kubelet.conf at
// 0600 on a first write, and brings a file an older release wrote 0644 back
// to 0600 on the next start — CIS 4.1.1. The shared atomic writer replaces
// the file whole, so the mode arrives with the replacement inode and no
// separate chmod is needed.
func TestKubeletConfigIsAlways0600(t *testing.T) {
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })
	version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()
	st := &nodestate.NodeState{Role: "worker", Hostname: "w1"}
	p := filepath.Join(version.K8sConfDir, "kubelet.conf")

	if err := writeKubeletConfig(st); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil {
		t.Fatal(err)
	} else if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("fresh kubelet.conf mode = %o, want 600 (CIS 4.1.1)", got)
	}

	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeKubeletConfig(st); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("kubelet.conf written 0644 by an older release: mode = %o, want 600 (CIS 4.1.1)", got)
	}
}

// TestDisableSELinuxRewritesTheConfigAtomically: turning enable_selinux off
// goes through the shared atomic writer, so the config is replaced whole —
// no fixed .tmp name beside it that two writers could tear, no truncated
// config where a working one was.
func TestDisableSELinuxRewritesTheConfigAtomically(t *testing.T) {
	oldConfig := version.ContainerdConfig
	t.Cleanup(func() { version.ContainerdConfig = oldConfig })
	version.ContainerdConfig = filepath.Join(t.TempDir(), "config", "containerd.toml")
	if err := os.MkdirAll(filepath.Dir(version.ContainerdConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	on := strings.Replace(containerdDefault(t), "enable_selinux = false", "enable_selinux = true", 1)
	if !strings.Contains(on, "enable_selinux = true") {
		t.Fatalf("fixture carries no enable_selinux line to turn off")
	}
	if err := os.WriteFile(version.ContainerdConfig, []byte(on), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := DisableSELinux()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Errorf("DisableSELinux reported no change to a config with enable_selinux = true")
	}
	b, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "enable_selinux = true") {
		t.Errorf("enable_selinux is still on after DisableSELinux:\n%s", b)
	}
	entries, err := os.ReadDir(filepath.Dir(version.ContainerdConfig))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("stray hidden temporary file beside the config: %s", e.Name())
		}
	}
}

// TestEnsureConfigWritesConfigAndRecordAtomically: the rendered containerd
// config and the confinement record land through the shared atomic writer —
// each file whole, nothing but the two files in the config directory, and the
// record readable by ConfinedSince.
func TestEnsureConfigWritesConfigAndRecordAtomically(t *testing.T) {
	dir := t.TempDir()
	oldBin, oldConfig, oldSocket, oldLabelled := version.BinDir, version.ContainerdConfig, version.ControlSocket, selinuxLabelled
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	version.ControlSocket = filepath.Join(dir, "supervisor.sock")
	selinuxLabelled = func() bool { return true }
	t.Cleanup(func() {
		version.BinDir, version.ContainerdConfig, version.ControlSocket, selinuxLabelled = oldBin, oldConfig, oldSocket, oldLabelled
	})

	fixture, err := filepath.Abs(filepath.Join("testdata", "containerd-2.3-default.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "containerd"), []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureConfig(); err != nil {
		t.Fatal(err)
	}
	conf, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "enable_selinux = true") {
		t.Errorf("a labelled node's config does not confine containers:\n%s", conf)
	}
	if _, ok := ConfinedSince(); !ok {
		t.Errorf("a labelled node's start wrote no confinement record containerd can read")
	}
	entries, err := os.ReadDir(filepath.Dir(version.ContainerdConfig))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := []string{"containerd.toml", "selinux-confined-since"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("config directory holds %v, want exactly %v (a stray temporary file would be residue)", got, want)
	}
}

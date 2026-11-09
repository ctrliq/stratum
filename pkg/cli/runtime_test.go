package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/version"
)

// runtimeWorkerHost redirects a worker's node state, containerd config and
// runtime-handler drop-in to a temp tree, with the supervisor socket absent so
// containerd reports not running and `apply` never restarts anything. It
// returns an executable standing in for a runtime binary.
func runtimeWorkerHost(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(root, "data")},
		{&version.K8sConfDir, filepath.Join(root, "config")},
		{&version.EtcdDataDir, filepath.Join(root, "etcd")},
		{&version.ContainerdConfig, filepath.Join(root, "containerd.toml")},
		{&version.ContainerdRuntimeConfD, filepath.Join(root, "containerd.d", "stratum-runtime.toml")},
		{&version.ContainerdConfD, filepath.Join(root, "conf.d", "stratum-registry.toml")},
		{&version.ControlSocket, filepath.Join(root, "absent.sock")},
		{&version.RuntimeHandlersLockPath, filepath.Join(root, "runtime-handlers.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	imports := "imports = ['/etc/containerd/conf.d/*.toml', '" + filepath.Join(root, "containerd.d", "*.toml") + "']\n"
	if err := os.WriteFile(version.ContainerdConfig, []byte("version = 3\n"+imports), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cluster.SaveState(&cluster.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "runsc")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func runRuntime(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newRuntimeCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// TestRuntimeApplyThenCheck walks a worker through declaring a handler,
// checking it and clearing it. containerd is not running, so apply must say
// the handlers load at its next start, never that it restarted.
func TestRuntimeApplyThenCheck(t *testing.T) {
	bin := runtimeWorkerHost(t)

	out, err := runRuntime(t, "apply", "--runtime-handler", "gvisor="+bin)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, "gvisor="+bin) || !strings.Contains(out, "loads the runtime handlers when it next starts") || strings.Contains(out, "containerd restarted") {
		t.Errorf("apply printed:\n%s\nwant the handler and the not-running line, and no restart line", out)
	}
	if out, err = runRuntime(t, "check"); err != nil {
		t.Fatalf("check after apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, "OK    runtime handler gvisor runs "+bin) {
		t.Errorf("check printed:\n%s\nwant the handler reported ok", out)
	}

	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	out, err = runRuntime(t, "check")
	if !errors.Is(err, ErrCheckFailed) || !strings.Contains(out, "ERROR runtime handler gvisor") {
		t.Errorf("check with the binary gone = %v\n%s\nwant ErrCheckFailed and the handler's error", err, out)
	}

	if out, err = runRuntime(t, "apply", "--clear"); err != nil {
		t.Fatalf("apply --clear: %v\n%s", err, out)
	}
	if _, err := os.Stat(version.ContainerdRuntimeConfD); !os.IsNotExist(err) {
		t.Errorf("apply --clear left the drop-in in place (stat: %v)", err)
	}
	if out, err = runRuntime(t, "check"); err != nil {
		t.Errorf("check after --clear: %v\n%s", err, out)
	}
}

func TestRuntimeApplyRefusesBadFlags(t *testing.T) {
	bin := runtimeWorkerHost(t)
	for name, args := range map[string][]string{
		"not name=path":         {"apply", "--runtime-handler", "gvisor"},
		"clear and a handler":   {"apply", "--clear", "--runtime-handler", "gvisor=" + bin},
		"a positional argument": {"apply", "gvisor"},
	} {
		if _, err := runRuntime(t, args...); err == nil {
			t.Errorf("%s: apply %v succeeded", name, args)
		}
	}
	if _, err := os.Stat(version.ContainerdRuntimeConfD); !os.IsNotExist(err) {
		t.Errorf("a refused apply wrote the drop-in (stat: %v)", err)
	}
}

func TestRuntimeRestartRefusesWithoutASupervisor(t *testing.T) {
	runtimeWorkerHost(t)
	if _, err := runRuntime(t, "restart"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("restart with no supervisor = %v, want the not-running refusal", err)
	}
}

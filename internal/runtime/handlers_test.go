package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// redirectRuntimeConfD points the runtime-handler drop-in into a temp dir.
func redirectRuntimeConfD(t *testing.T) string {
	t.Helper()
	old := version.ContainerdRuntimeConfD
	version.ContainerdRuntimeConfD = filepath.Join(t.TempDir(), "conf.d", "stratum-runtime.toml")
	t.Cleanup(func() { version.ContainerdRuntimeConfD = old })
	return version.ContainerdRuntimeConfD
}

// fakeBinary writes an executable file standing in for a handler's runtime.
func fakeBinary(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRenderHandlersGolden(t *testing.T) {
	handlers := map[string]string{
		"nvidia": "/usr/bin/nvidia-container-runtime",
		"gvisor": "/usr/local/bin/runsc",
	}
	for _, tc := range []struct {
		cgroup  string
		systemd string
	}{
		{"systemd", "true"},
		{"cgroupfs", "false"},
	} {
		t.Run(tc.cgroup, func(t *testing.T) {
			want := handlersMarker + `version = 4

[plugins.'io.containerd.cri.v1.runtime']
  enable_cdi = true
  cdi_spec_dirs = ['/etc/cdi', '/var/run/cdi']

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gvisor]
  runtime_type = 'io.containerd.runc.v2'
  sandboxer = 'podsandbox'

  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gvisor.options]
    BinaryName = '/usr/local/bin/runsc'
    SystemdCgroup = ` + tc.systemd + `

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.nvidia]
  runtime_type = 'io.containerd.runc.v2'
  sandboxer = 'podsandbox'

  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.nvidia.options]
    BinaryName = '/usr/bin/nvidia-container-runtime'
    SystemdCgroup = ` + tc.systemd + `
`
			got := renderHandlers(handlers, tc.cgroup)
			if got != want {
				t.Errorf("render:\n%s\nwant:\n%s", got, want)
			}
			if err := verifyHandlers(got, handlers, tc.cgroup); err != nil {
				t.Errorf("the render does not read back as containerd reads it: %v", err)
			}
		})
	}
}

// verifyHandlers is what stops a render that drifts off containerd's table
// path or cgroup driver from reaching disk. Each mutation of a correct render
// must fail it.
func TestVerifyHandlersCatchesADriftedRender(t *testing.T) {
	handlers := map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime"}
	good := renderHandlers(handlers, "systemd")
	for name, mutate := range map[string]func(string) string{
		"table path": func(s string) string { return strings.ReplaceAll(s, ".containerd.runtimes.", ".runtimes.") },
		"plugin id": func(s string) string {
			return strings.ReplaceAll(s, "io.containerd.cri.v1.runtime", "io.containerd.grpc.v1.cri")
		},
		"cgroup driver":  func(s string) string { return strings.ReplaceAll(s, "SystemdCgroup = true", "SystemdCgroup = false") },
		"shim":           func(s string) string { return strings.ReplaceAll(s, "io.containerd.runc.v2", "io.containerd.runc.v1") },
		"binary":         func(s string) string { return strings.ReplaceAll(s, "nvidia-container-runtime", "runc") },
		"cdi off":        func(s string) string { return strings.ReplaceAll(s, "enable_cdi = true", "enable_cdi = false") },
		"no version":     func(s string) string { return strings.Replace(s, "version = 4\n", "", 1) },
		"sandboxer":      func(s string) string { return strings.ReplaceAll(s, "sandboxer = 'podsandbox'", "sandboxer = ''") },
		"unparseable":    func(s string) string { return s + "\n[broken\n" },
		"options nested": func(s string) string { return strings.ReplaceAll(s, ".nvidia.options]", ".nvidia.options.extra]") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyHandlers(mutate(good), handlers, "systemd"); err == nil {
				t.Errorf("a render with a drifted %s passed verification", name)
			}
		})
	}
}

func TestValidateHandlerRefusals(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"runc", "/usr/bin/runc"},
		{"Nvidia", "/usr/bin/nvidia-container-runtime"},
		{"nvidia.gpu", "/usr/bin/nvidia-container-runtime"},
		{"", "/usr/bin/nvidia-container-runtime"},
		{strings.Repeat("a", 64), "/usr/bin/nvidia-container-runtime"},
		{"nvidia", "usr/bin/nvidia-container-runtime"},
		{"nvidia", "/usr/bin/../bin/nvidia-container-runtime"},
		{"nvidia", "/usr/bin/nvidia-container-runtime/"},
		{"nvidia", "/usr/bin/nvidia container runtime"},
		{"nvidia", "/usr/bin/nvidia'runtime"},
		{"nvidia", "/usr/bin/$(id)"},
		{"nvidia", "/usr/bin/nvidia;rm"},
	} {
		if err := ValidateHandler(tc.name, tc.path); err == nil {
			t.Errorf("ValidateHandler(%q, %q) accepted it", tc.name, tc.path)
		}
	}
	if err := ValidateHandler("nvidia", "/usr/bin/nvidia-container-runtime"); err != nil {
		t.Errorf("a well-formed handler was refused: %v", err)
	}
}

func TestCheckHandlerBinary(t *testing.T) {
	dir := t.TempDir()
	notExec := filepath.Join(dir, "plain")
	if err := os.WriteFile(notExec, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "absent"), notExec, dir} {
		if err := CheckHandlerBinary(p); err == nil {
			t.Errorf("CheckHandlerBinary(%s) accepted it", p)
		}
	}
	if err := CheckHandlerBinary(fakeBinary(t, "runtime")); err != nil {
		t.Errorf("an executable file was refused: %v", err)
	}
}

func TestWriteHandlersIsIdempotentAndPrunesOnlyItsOwnFile(t *testing.T) {
	path := redirectRuntimeConfD(t)
	handlers := map[string]string{"fake": fakeBinary(t, "fake-runtime")}
	var out bytes.Buffer

	changed, err := writeHandlers(&out, handlers)
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v, want a written drop-in", changed, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), handlersMarker) {
		t.Errorf("drop-in does not open with the generated marker:\n%s", b)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("drop-in mode: %v %v, want 0644", fi.Mode().Perm(), err)
	}

	changed, err = writeHandlers(&out, handlers)
	if err != nil || changed {
		t.Errorf("unchanged handlers rewrote the drop-in: changed=%v err=%v; a needless change restarts containerd", changed, err)
	}
	if out.Len() != 0 {
		t.Errorf("an installed handler warned: %s", out.String())
	}

	changed, err = writeHandlers(&out, nil)
	if err != nil || !changed {
		t.Fatalf("removing the last handler: changed=%v err=%v, want the drop-in removed", changed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the drop-in survived removing the last handler: %v", err)
	}
	changed, err = writeHandlers(&out, nil)
	if err != nil || changed {
		t.Errorf("no handlers and no drop-in reported a change: changed=%v err=%v", changed, err)
	}

	const hand = "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.mine]\n"
	if err := os.WriteFile(path, []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err = writeHandlers(&out, nil)
	if err != nil || changed {
		t.Errorf("pruned a file stratum did not write: changed=%v err=%v", changed, err)
	}
	if b, _ := os.ReadFile(path); string(b) != hand {
		t.Errorf("an operator's file at the drop-in path was modified:\n%s", b)
	}
}

// A restart must not take the node's other pods down for a handler whose
// binary is missing: the drop-in is written and the gap is reported.
func TestWriteHandlersWarnsOnAMissingBinary(t *testing.T) {
	path := redirectRuntimeConfD(t)
	var out bytes.Buffer
	changed, err := writeHandlers(&out, map[string]string{"nvidia": "/nonexistent/nvidia-container-runtime"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want the drop-in written", changed, err)
	}
	if !strings.Contains(out.String(), "WARN: runtime handler nvidia") {
		t.Errorf("no warning for a missing binary: %q", out.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("drop-in not written: %v", err)
	}
}

func TestWriteHandlersRefusesAnInvalidHandler(t *testing.T) {
	path := redirectRuntimeConfD(t)
	if _, err := writeHandlers(&bytes.Buffer{}, map[string]string{"runc": fakeBinary(t, "runc")}); err == nil {
		t.Error("redeclaring runc was written")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a refused handler left a drop-in behind: %v", err)
	}
}

func TestTheCapturedDefaultImportsConfD(t *testing.T) {
	if !importsConfD(containerdDefault(t)) {
		t.Fatalf("the captured containerd default no longer imports %s; both stratum drop-ins depend on it", confdImportGlob)
	}
}

// A containerd default without the conf.d import would silently drop the
// registry mirrors and runtime handlers stratum writes there, so the render
// refuses and writes nothing.
func TestEnsureConfigRefusesADefaultWithoutTheConfDImport(t *testing.T) {
	dir := t.TempDir()
	oldBin, oldConfig, oldSocket := version.BinDir, version.ContainerdConfig, version.ControlSocket
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	version.ControlSocket = filepath.Join(dir, "supervisor.sock")
	t.Cleanup(func() { version.BinDir, version.ContainerdConfig, version.ControlSocket = oldBin, oldConfig, oldSocket })

	noImport := strings.Replace(containerdDefault(t), "imports = ['"+confdImportGlob+"']", "imports = []", 1)
	if importsConfD(noImport) {
		t.Fatal("the fixture edit did not remove the import")
	}
	fixture := filepath.Join(dir, "default.toml")
	if err := os.WriteFile(fixture, []byte(noImport), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "containerd"), []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := ensureConfig()
	if err == nil || !strings.Contains(err.Error(), "registry mirrors and runtime handlers") {
		t.Fatalf("err = %v, want a refusal naming both drop-ins", err)
	}
	if _, err := os.Stat(version.ContainerdConfig); !os.IsNotExist(err) {
		t.Errorf("a config without the conf.d import was written: %v", err)
	}
}

// A config stratum wrote before the import was asserted is regenerated, not
// accepted, when it lacks the import.
func TestEnsureConfigRegeneratesAnExistingConfigWithoutTheConfDImport(t *testing.T) {
	dir := t.TempDir()
	oldBin, oldConfig, oldSocket, oldLabelled := version.BinDir, version.ContainerdConfig, version.ControlSocket, selinuxLabelled
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	version.ControlSocket = filepath.Join(dir, "supervisor.sock")
	selinuxLabelled = func() bool { return false }
	t.Cleanup(func() {
		version.BinDir, version.ContainerdConfig, version.ControlSocket, selinuxLabelled = oldBin, oldConfig, oldSocket, oldLabelled
	})

	cgroup := "SystemdCgroup = false"
	if CgroupDriver() == "systemd" {
		cgroup = "SystemdCgroup = true"
	}
	current := rewriteContainerdConfig(containerdDefault(t), cgroup, false)
	stale := strings.Replace(current, "imports = ['"+confdImportGlob+"']", "imports = []", 1)
	if err := os.MkdirAll(filepath.Dir(version.ContainerdConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(version.ContainerdConfig, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs(filepath.Join("testdata", "containerd-2.3-default.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "containerd"), []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	changed, err := ensureConfig()
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want the stale config regenerated", changed, err)
	}
	b, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !importsConfD(string(b)) {
		t.Errorf("the regenerated config still does not import %s", confdImportGlob)
	}
}

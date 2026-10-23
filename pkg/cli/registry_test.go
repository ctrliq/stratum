package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ctrliq/stratum/internal/registry"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// redirectRegistry points the registry paths at a temp tree — the redirect of
// internal/registry's own tests, inlined because it lives in another package —
// and points the supervisor's control socket at an absent path so containerd
// reports not running and `apply` never restarts anything.
func redirectRegistry(t *testing.T) (certsDir, confd, regYaml string) {
	t.Helper()
	root := t.TempDir()
	certsDir = filepath.Join(root, "certs.d")
	confd = filepath.Join(root, "conf.d", "stratum-registry.toml")
	regYaml = filepath.Join(root, "registries.yaml")
	oldCerts, oldConfd, oldReg, oldSock := version.CertsDir, version.ContainerdConfD, version.RegistriesConfig, version.ControlSocket
	t.Cleanup(func() {
		version.CertsDir, version.ContainerdConfD, version.RegistriesConfig, version.ControlSocket = oldCerts, oldConfd, oldReg, oldSock
	})
	version.CertsDir = certsDir
	version.ContainerdConfD = confd
	version.RegistriesConfig = regYaml
	version.ControlSocket = filepath.Join(root, "absent.sock")
	return certsDir, confd, regYaml
}

// TestRegistryApply_PrintsMirrorsAndWarnings pins what `stratum registry apply`
// reports through stdout: the per-registry warnings (a mirror told to skip TLS
// verification is an operator-visible risk) and the list of registries it
// applied mirrors for. The restart line it deliberately asserts nothing about:
// the restart line follows runtime.ApplyRegistry's Restarted result, never
// res.Changed alone (Changed says the config changed, not that a running
// containerd was restarted), so
// TestRegistryApply_ReportsNoRestartWhenContainerdIsStopped pins that side.
func TestRegistryApply_PrintsMirrorsAndWarnings(t *testing.T) {
	_, _, regYaml := redirectRegistry(t)
	yaml := `mirrors:
  docker.io:
    endpoint: ["https://mirror.internal:5000"]
configs:
  "mirror.internal:5000":
    tls: {insecure_skip_verify: true}
`
	if err := os.WriteFile(regYaml, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		cmd := newRegistryApplyCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs(nil)
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "WARN: insecure_skip_verify is set for https://mirror.internal:5000") {
		t.Errorf("stdout does not carry the skip-verify warning:\n%s", out)
	}
	if !strings.Contains(out, "applied registry configuration for: docker.io") {
		t.Errorf("stdout does not list the registry mirrors were applied for:\n%s", out)
	}
}

// fakeSupervisor serves the supervisor control protocol on a temp unix socket
// and points version.ControlSocket at it, so the runtime package sees
// containerd running: status reports pid 100 until a restart op is served,
// then pid 101 — the new pid comps.Restart's replacement check waits for.
// Returns a func counting the restart ops served. Call it AFTER
// redirectRegistry: it replaces the absent socket path that helper points at
// containerd-stopped, and its cleanup restores that path before
// redirectRegistry's cleanup restores the real one.
func fakeSupervisor(t *testing.T) (restarts func() int) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	old := version.ControlSocket
	version.ControlSocket = socket
	t.Cleanup(func() { version.ControlSocket = old; ln.Close() })

	var mu sync.Mutex
	served := 0
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req struct {
				Op   string `json:"op"`
				Name string `json:"name"`
			}
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				conn.Close()
				continue
			}
			mu.Lock()
			out := struct {
				OK    bool                    `json:"ok"`
				Error string                  `json:"error,omitempty"`
				Procs []supervisor.ProcStatus `json:"procs,omitempty"`
			}{OK: true}
			switch req.Op {
			case "status":
				pid := 100
				if served > 0 {
					pid = 101
				}
				out.Procs = []supervisor.ProcStatus{{Name: "containerd", Pid: pid, Running: true}}
			case "restart":
				served++
			}
			mu.Unlock()
			_ = json.NewEncoder(conn).Encode(out)
			conn.Close()
		}
	}()
	return func() int { mu.Lock(); defer mu.Unlock(); return served }
}

// writeOneMirror writes a one-mirror registries.yaml at the redirected path,
// so a first apply creates the conf.d drop-in and reports Changed.
func writeOneMirror(t *testing.T, regYaml string) {
	t.Helper()
	const oneMirror = "mirrors:\n  docker.io:\n    endpoint: [\"https://mirror.internal:5000\"]\n"
	if err := os.WriteFile(regYaml, []byte(oneMirror), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runRegistryApply runs `stratum registry apply` and returns its stdout. The
// command prints with fmt.Printf to the process's own stdout, so the pipe
// swap is the only way to see its output.
func runRegistryApply(t *testing.T) string {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		cmd := newRegistryApplyCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs(nil)
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRegistryApply_ReportsNoRestartWhenContainerdIsStopped pins that on a
// first apply the drop-in changes, but with containerd not running nothing
// was restarted, so the restart line must be absent. The CLI prints it from
// runtime.ApplyRegistry's Restarted result, not from Changed alone, which
// would report a restart that did not happen.
func TestRegistryApply_ReportsNoRestartWhenContainerdIsStopped(t *testing.T) {
	_, _, regYaml := redirectRegistry(t)
	writeOneMirror(t, regYaml)

	out := runRegistryApply(t)
	if strings.Contains(out, "containerd restarted") {
		t.Errorf("containerd was not running, yet the apply reported a restart:\n%s", out)
	}
	if !strings.Contains(out, "applied registry configuration for: docker.io") {
		t.Errorf("stdout does not list the registry mirrors were applied for:\n%s", out)
	}
}

// TestRegistryApply_PrintsTheRestartLineOnlyWhenContainerdWasRestarted pins
// the other side of that contract: with containerd active the first apply restarts
// it and prints the restart line; an identical re-apply changes nothing,
// restarts nothing and prints none — exactly what the returned Restarted
// says, never what Changed alone says.
func TestRegistryApply_PrintsTheRestartLineOnlyWhenContainerdWasRestarted(t *testing.T) {
	_, _, regYaml := redirectRegistry(t)
	restarts := fakeSupervisor(t)
	writeOneMirror(t, regYaml)

	first := runRegistryApply(t)
	if !strings.Contains(first, "containerd restarted") {
		t.Errorf("the first apply restarted containerd, but stdout lacks the restart line:\n%s", first)
	}

	second := runRegistryApply(t)
	if strings.Contains(second, "containerd restarted") {
		t.Errorf("the identical re-apply restarted nothing, yet stdout claims a restart:\n%s", second)
	}
	if !strings.Contains(second, "applied registry configuration for: docker.io") {
		t.Errorf("stdout does not list the registry mirrors were applied for:\n%s", second)
	}
	if n := restarts(); n != 1 {
		t.Errorf("the supervisor served %d restart ops, want the first apply's 1 only", n)
	}
}

// TestRegistryApplyConfigsOnly pins that a configs-only registries.yaml applies
// (the registry is listed, no "nothing configured" line) and that the success
// line says "configuration": a configs-only host is not a mirror.
func TestRegistryApplyConfigsOnly(t *testing.T) {
	_, _, regYaml := redirectRegistry(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"registry.corp:5000\":\n    auth: {username: u, password: p}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runRegistryApply(t)
	if !strings.Contains(out, "applied registry configuration for: registry.corp:5000") {
		t.Errorf("configs-only apply should list the registry under the configuration line:\n%s", out)
	}
	if strings.Contains(out, registry.NothingConfigured) {
		t.Errorf("a configs-only file IS configured, yet apply printed the nothing-configured line:\n%s", out)
	}
}

// TestRegistryApplyEmptyFilePrintsNothingConfigured pins the empty-file wording:
// the shared NothingConfigured sentence with the file's path, not the old
// "registry mirrors cleared" claim (an existing file is not "no <path>").
func TestRegistryApplyEmptyFilePrintsNothingConfigured(t *testing.T) {
	_, _, regYaml := redirectRegistry(t)
	if err := os.WriteFile(regYaml, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runRegistryApply(t)
	if !strings.Contains(out, regYaml+": "+registry.NothingConfigured) {
		t.Errorf("empty file should print the path plus the shared nothing-configured sentence:\n%s", out)
	}
	if strings.Contains(out, "registry mirrors cleared") {
		t.Errorf("the file exists, so 'no <path> — registry mirrors cleared' is wrong:\n%s", out)
	}
}

// TestRegistryCheckConfigsOnly pins that `registry check` lists a configs-only
// registry as direct (no endpoints) with its auth kind, no secrets.
func TestRegistryCheckConfigsOnly(t *testing.T) {
	_, _, regYaml := redirectRegistry(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"registry.corp:5000\":\n    auth: {username: u, password: p}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	out := captureStdout(t, func() {
		cmd := newRegistryCheckCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs(nil)
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "registry.corp:5000 (direct)") {
		t.Errorf("configs-only registry should be listed as direct:\n%s", out)
	}
	if !strings.Contains(out, "auth=basic(user u)") {
		t.Errorf("check should report the registry's own auth kind:\n%s", out)
	}
	if strings.Contains(out, "p\n") || strings.Contains(out, "password") {
		t.Errorf("check must not print the secret:\n%s", out)
	}
}

// TestRegistryCheckShowsClientCertAndKey pins that the TLS row of `registry
// check` reports client cert/key paths, not just CA and skip_verify: a
// configs-only host with cert_file+key_file is configured with a client
// certificate, and the preview must show it.
func TestRegistryCheckShowsClientCertAndKey(t *testing.T) {
	_, _, regYaml := redirectRegistry(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"registry.corp:5000\":\n    tls: {cert_file: /etc/stratum/client.crt, key_file: /etc/stratum/client.key}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	out := captureStdout(t, func() {
		cmd := newRegistryCheckCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs(nil)
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "registry.corp:5000 (direct)") {
		t.Errorf("cert+key configs-only registry should be listed as direct:\n%s", out)
	}
	if !strings.Contains(out, "/etc/stratum/client.crt") || !strings.Contains(out, "/etc/stratum/client.key") {
		t.Errorf("check must show the client cert and key paths:\n%s", out)
	}
}

// TestRegistryCheckWildcardCredentialsWarn pins that `registry check` warns that
// configs."*" credentials and client certificate are not applied, and shows
// only what is: the catch-all CA, with no auth row.
func TestRegistryCheckWildcardCredentialsWarn(t *testing.T) {
	_, _, regYaml := redirectRegistry(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"*\":\n    auth: {username: u, password: p}\n    tls: {ca_file: /etc/pki/ca.crt, cert_file: /c.crt, key_file: /c.key}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	out := captureStdout(t, func() {
		cmd := newRegistryCheckCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs(nil)
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "WARN: "+registry.WildcardCredentialsIgnored) {
		t.Errorf("check must warn that the catch-all credentials are not applied:\n%s", out)
	}
	if !strings.Contains(out, `*: tls ca="/etc/pki/ca.crt" cert="" key="" skip_verify=false`) {
		t.Errorf("check must show only the catch-all CA as applied:\n%s", out)
	}
	if strings.Contains(out, "auth=") {
		t.Errorf("check must not list catch-all credentials it will not apply:\n%s", out)
	}
}

// TestRegistryCheckAuthlessConfigsIsNothing pins that `registry check` treats a
// configs entry with neither auth nor TLS as nothing configured, using the
// shared sentence — the same predicate as `registry apply`.
func TestRegistryCheckAuthlessConfigsIsNothing(t *testing.T) {
	_, _, regYaml := redirectRegistry(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"registry.corp:5000\": {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	out := captureStdout(t, func() {
		cmd := newRegistryCheckCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs(nil)
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, regYaml+": "+registry.NothingConfigured) {
		t.Errorf("a configs entry with neither auth nor TLS is nothing configured:\n%s", out)
	}
}

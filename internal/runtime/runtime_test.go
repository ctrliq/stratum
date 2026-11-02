package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// containerd renders this setting differently across versions and the node has
// to end up pointed at the payload's pause image either way. Every spelling is
// verbatim from `containerd config default`.
func TestSandboxImageRewrite(t *testing.T) {
	const want = "ghcr.io/ctrliq/stratum/pause:3.10.2"
	old := SandboxImage
	SandboxImage = want
	t.Cleanup(func() { SandboxImage = old })

	for name, tc := range map[string]struct{ cfg, key string }{
		"containerd 2.3": {"    [plugins.'io.containerd.cri.v1.images'.pinned_images]\n" +
			"      sandbox = 'registry.k8s.io/pause:3.10.2'\n", "      sandbox"},
		"containerd 2.0-2.2": {"[plugins.'io.containerd.cri.v1.images']\n" +
			"  snapshotter = 'overlayfs'\n" +
			"  sandbox_image = 'registry.k8s.io/pause:3.10'\n", "  sandbox_image"},
		"containerd 1.x": {`[plugins."io.containerd.grpc.v1.cri"]` + "\n" +
			`  sandbox_image = "registry.k8s.io/pause:3.9"` + "\n", "  sandbox_image"},
		"already ours": {"  sandbox_image = '" + want + "'\n", "  sandbox_image"},
	} {
		got := sandboxImageLine.ReplaceAllString(tc.cfg, "${1}'"+SandboxImage+"'")
		// The key and its indentation both survive: indentation is part of the
		// TOML block structure, and losing it moves the key out of its table
		// where containerd silently ignores it.
		if !strings.Contains(got, "\n"+tc.key+" = '"+want+"'") && !strings.HasPrefix(got, tc.key+" = '"+want+"'") {
			t.Errorf("%s: %s not rewritten in place:\n%s", name, strings.TrimSpace(tc.key), got)
		}
		if strings.Contains(got, "registry.k8s.io") {
			t.Errorf("%s: registry.k8s.io survived the rewrite:\n%s", name, got)
		}
	}

	// The sandboxer is a different key that shares the prefix. Rewriting it
	// would hand containerd an image reference as a sandboxer name.
	const sandboxer = "          sandboxer = 'podsandbox'\n"
	if got := sandboxImageLine.ReplaceAllString(sandboxer, "${1}'"+SandboxImage+"'"); got != sandboxer {
		t.Errorf("sandboxer was rewritten:\n%s", got)
	}
}

// containerdDefault is `containerd config default` from the containerd the
// payload ships (2.3.6+stratum, config version 4), captured verbatim. The
// earlier sandbox-image regex slipped past the hand-written spellings above: they
// tested the regex against what the author expected containerd to print, not
// what it printed. Regenerate it on a containerd bump:
//
//	docker run --rm --platform linux/arm64 -v "$PWD/staging/linux-arm64/bin:/b:ro" \
//	  rockylinux/rockylinux:10 /b/containerd config default \
//	  > internal/runtime/testdata/containerd-2.3-default.toml
func containerdDefault(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "containerd-2.3-default.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The effective setting, read the way containerd reads it: parse the TOML and
// follow the table path, so a rewrite that lands on the wrong key or drops the
// line out of its table fails here rather than on an airgapped node.
func pinnedSandbox(t *testing.T, cfg string) string {
	t.Helper()
	var c struct {
		Plugins map[string]struct {
			PinnedImages struct {
				Sandbox string `toml:"sandbox"`
			} `toml:"pinned_images"`
		} `toml:"plugins"`
	}
	if _, err := toml.Decode(cfg, &c); err != nil {
		t.Fatalf("rendered config does not parse: %v", err)
	}
	return c.Plugins["io.containerd.cri.v1.images"].PinnedImages.Sandbox
}

func TestRewriteRealContainerdDefault(t *testing.T) {
	const want = "ghcr.io/ctrliq/stratum/pause:3.10.2"
	old := SandboxImage
	SandboxImage = want
	t.Cleanup(func() { SandboxImage = old })

	def := containerdDefault(t)
	if got := pinnedSandbox(t, def); !strings.HasPrefix(got, "registry.k8s.io/") {
		t.Fatalf("fixture no longer carries the upstream default this test exists for: %q", got)
	}
	got := rewriteContainerdConfig(def, "SystemdCgroup = true", false)
	if s := pinnedSandbox(t, got); s != want {
		t.Errorf("pinned_images.sandbox = %q, want the payload's %q", s, want)
	}
	if strings.Contains(got, "registry.k8s.io") {
		t.Errorf("registry.k8s.io survived the rewrite")
	}
	if !strings.Contains(got, "sandboxer = 'podsandbox'") {
		t.Errorf("the runtime's sandboxer was rewritten")
	}
	if !isStratumConfig([]byte(got)) {
		t.Errorf("rewritten config is not recognised by isStratumConfig")
	}
}

// ensureConfig end to end against the real default: it writes a config pinned
// to the payload's pause image, and a second start leaves it alone. Before the
// fix the written file never contained SandboxImage, so every start re-ran
// `containerd config default` and rewrote the same wrong config.
func TestEnsureConfigPinsThePayloadPauseImage(t *testing.T) {
	const want = "ghcr.io/ctrliq/stratum/pause:3.10.2"
	dir := t.TempDir()
	oldBin, oldConfig, oldSocket, oldImage := version.BinDir, version.ContainerdConfig, version.ControlSocket, SandboxImage
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	version.ControlSocket = filepath.Join(dir, "supervisor.sock")
	SandboxImage = want
	t.Cleanup(func() {
		version.BinDir, version.ContainerdConfig, version.ControlSocket, SandboxImage = oldBin, oldConfig, oldSocket, oldImage
	})

	fixture, err := filepath.Abs(filepath.Join("testdata", "containerd-2.3-default.toml"))
	if err != nil {
		t.Fatal(err)
	}
	containerd := filepath.Join(dir, "containerd")
	if err := os.WriteFile(containerd, []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfig(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if s := pinnedSandbox(t, string(b)); s != want {
		t.Errorf("written pinned_images.sandbox = %q, want %q", s, want)
	}

	// A second start must be satisfied by what the first wrote.
	if err := os.WriteFile(containerd, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfig(); err != nil {
		t.Errorf("second start re-rendered a config the first already wrote: %v", err)
	}
}

// A node upgraded from the broken build already has a config with the right
// cgroup driver but the upstream pause image. Its next start must replace that
// config, not accept the cgroup setting as proof that it is current.
func TestEnsureConfigHealsAnExistingUpstreamSandbox(t *testing.T) {
	const want = "ghcr.io/ctrliq/stratum/pause:3.10.2"
	dir := t.TempDir()
	oldBin, oldConfig, oldSocket, oldImage := version.BinDir, version.ContainerdConfig, version.ControlSocket, SandboxImage
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	version.ControlSocket = filepath.Join(dir, "supervisor.sock")
	SandboxImage = want
	t.Cleanup(func() {
		version.BinDir, version.ContainerdConfig, version.ControlSocket, SandboxImage = oldBin, oldConfig, oldSocket, oldImage
	})

	defaultConfig := containerdDefault(t)
	cgroup := "SystemdCgroup = false"
	if CgroupDriver() == "systemd" {
		cgroup = "SystemdCgroup = true"
	}
	stale := strings.ReplaceAll(defaultConfig, "SystemdCgroup = false", cgroup)
	if got := pinnedSandbox(t, stale); !strings.HasPrefix(got, "registry.k8s.io/") {
		t.Fatalf("stale config has no upstream sandbox image: %q", got)
	}
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

	if _, err := ensureConfig(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if image := pinnedSandbox(t, string(got)); image != want {
		t.Errorf("healed pinned_images.sandbox = %q, want %q", image, want)
	}
}

// A default with no sandbox setting stratum knows means containerd renamed the
// key again. Writing that config would leave containerd pulling pause from the
// internet instead of the payload, so ensureConfig refuses and writes nothing.
func TestEnsureConfigRefusesAnUnrecognisedSandboxSetting(t *testing.T) {
	dir := t.TempDir()
	oldBin, oldConfig := version.BinDir, version.ContainerdConfig
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	t.Cleanup(func() { version.BinDir, version.ContainerdConfig = oldBin, oldConfig })

	renamed := strings.Replace(containerdDefault(t), "      sandbox = ", "      sandbox_ref = ", 1)
	src := filepath.Join(dir, "default.toml")
	if err := os.WriteFile(src, []byte(renamed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "containerd"), []byte("#!/bin/sh\ncat '"+src+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := ensureConfig()
	if err == nil || !strings.Contains(err.Error(), "sandbox image") {
		t.Fatalf("ensureConfig = %v, want a refusal naming the sandbox image", err)
	}
	if _, err := os.Stat(version.ContainerdConfig); !os.IsNotExist(err) {
		t.Errorf("a config was written despite the refusal: stat err = %v", err)
	}
}

// isStratumConfig must recognise a config stratum wrote (it points containerd's
// root/state at the stratum data dirs) and must NOT match a host's own config —
// that distinction is the whole safety of removing the legacy file.
func TestIsStratumConfig(t *testing.T) {
	if !isStratumConfig([]byte(stratumConfigBody())) {
		t.Error("a stratum-written config was not recognised as ours")
	}
	host := "version = 2\nroot = '/var/lib/containerd'\nstate = '/run/containerd'\n"
	if isStratumConfig([]byte(host)) {
		t.Error("a host's own containerd config was misidentified as stratum's — removing it would destroy host config")
	}
	// A host config that merely MENTIONS a stratum path (a comment, an unrelated
	// option) must not be mistaken for ours — both assignments are required.
	mentionsOnly := "version = 2\n# migrated away from " + filepath.Join(version.DataDir, "containerd") + "\n" +
		"root = '/var/lib/containerd'\nstate = '/run/containerd'\n"
	if isStratumConfig([]byte(mentionsOnly)) {
		t.Error("a host config that only mentions a stratum path in passing was misidentified as stratum's")
	}
	// The root assignment alone is not enough either — the state assignment must
	// also match, so a partial coincidence cannot trigger a deletion.
	rootOnly := "version = 2\nroot = '" + filepath.Join(version.DataDir, "containerd") + "'\nstate = '/run/containerd'\n"
	if isStratumConfig([]byte(rootOnly)) {
		t.Error("a config with only the root assignment matching was misidentified as stratum's")
	}
	// Both stratum paths present, but only inside COMMENTS — the live top-level
	// root/state are host-owned. A substring match would delete this host config;
	// the TOML parse must not (regression for the legacy-config heal).
	inComments := "version = 2\n" +
		"# migrated from " + rootAssign() + "\n" +
		"# and " + stateAssign() + "\n" +
		"root = '/var/lib/containerd'\nstate = '/run/containerd'\n"
	if isStratumConfig([]byte(inComments)) {
		t.Error("a host config with the stratum paths only in comments was misidentified as stratum's — TOML parse must read live keys, not text")
	}
	// Unparseable input must fail closed (never deleted).
	if isStratumConfig([]byte("this is : not = valid toml [[[")) {
		t.Error("unparseable input was treated as a stratum config")
	}
}

// stratumConfigBody renders the two assignments via the same rootAssign/
// stateAssign helpers the production matcher and producer use, so the fixture
// cannot drift from them independently.
func stratumConfigBody() string {
	return "version = 2\n" + rootAssign() + "\n" + stateAssign() + "\n"
}

// removeLegacyHostConfig deletes /etc/containerd/config.toml only when stratum
// wrote it: heals a host an older build broke without ever touching a
// host's genuine config, and is a no-op when the file is absent.
func TestRemoveLegacyHostConfig(t *testing.T) {
	t.Run("removes a stratum-written legacy file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(p, []byte(stratumConfigBody()), 0o644); err != nil {
			t.Fatal(err)
		}
		removed, err := removeLegacyHostConfig(p)
		if err != nil {
			t.Fatalf("removeLegacyHostConfig: %v", err)
		}
		if !removed {
			t.Error("removed = false for a stratum-written config; caller would not log the host-file deletion")
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stratum's legacy config survived: stat err = %v", err)
		}
	})

	t.Run("leaves a host's own config alone", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "config.toml")
		body := []byte("version = 2\nroot = '/var/lib/containerd'\n")
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		removed, err := removeLegacyHostConfig(p)
		if err != nil {
			t.Fatalf("removeLegacyHostConfig: %v", err)
		}
		if removed {
			t.Error("removed = true for a host's own config — data loss")
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("host config was removed or unreadable: %v", err)
		}
		if string(got) != string(body) {
			t.Errorf("host config was modified: %q", got)
		}
	})

	t.Run("no-op when absent", func(t *testing.T) {
		removed, err := removeLegacyHostConfig(filepath.Join(t.TempDir(), "nope.toml"))
		if err != nil {
			t.Errorf("expected nil for a missing file, got %v", err)
		}
		if removed {
			t.Error("removed = true for a missing file")
		}
	})

	t.Run("never removes stratum's own current config path", func(t *testing.T) {
		// If the legacy path and the current stratum path were ever the same
		// (a redirected test env), the reap must not delete the live config.
		old := version.ContainerdConfig
		p := filepath.Join(t.TempDir(), "config.toml")
		version.ContainerdConfig = p
		t.Cleanup(func() { version.ContainerdConfig = old })
		if err := os.WriteFile(p, []byte(stratumConfigBody()), 0o644); err != nil {
			t.Fatal(err)
		}
		removed, err := removeLegacyHostConfig(p)
		if err != nil {
			t.Fatalf("removeLegacyHostConfig: %v", err)
		}
		if removed {
			t.Error("removed = true for stratum's own current config path")
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the current stratum config was removed: %v", err)
		}
	})
}

// captureProcessStdout runs fn with the process's stdout captured, and returns
// whatever fn printed there. The heal's outcome must reach the writer it is
// given and never this stream: on the MCP stdio transport os.Stdout is the
// JSON-RPC stream, so a line printed there corrupts the response mid-message.
func captureProcessStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = old
	w.Close()
	return <-done
}

// TestRemoveLegacyHostConfigLogsToTheWriterItIsGiven pins where the heal's
// outcome goes. The exported entry is the function the uninstall run wires as
// its removeLegacyHostConfig effect, so the writer it is handed is the run's
// progress writer — the CLI's os.Stdout, the MCP handler's response buffer —
// and both of the heal's outcomes (the removal report and the failure WARN)
// must land in that writer, never on the process's stdout.
func TestRemoveLegacyHostConfigLogsToTheWriterItIsGiven(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(legacy, []byte(stratumConfigBody()), 0o644); err != nil {
		t.Fatal(err)
	}
	old := legacyHostConfig
	legacyHostConfig = legacy
	t.Cleanup(func() { legacyHostConfig = old })

	t.Run("a removal is reported through the writer", func(t *testing.T) {
		var buf bytes.Buffer
		stdout := captureProcessStdout(t, func() { RemoveLegacyHostConfig(&buf) })
		want := fmt.Sprintf("  removed %s — a previous stratum wrote it over the host's config; host containerd/Docker can start again\n", legacy)
		if buf.String() != want {
			t.Errorf("the heal wrote %q to its writer, want %q", buf.String(), want)
		}
		if stdout != "" {
			t.Errorf("the heal printed %q onto the process's stdout; on the MCP stdio transport that stream is the JSON-RPC response", stdout)
		}
	})

	t.Run("a failure is warned through the writer", func(t *testing.T) {
		// A directory at the legacy path cannot be read as a config, so the
		// reap fails before it deletes anything — the WARN outcome, with no
		// dependency on host state or permissions.
		dirPath := filepath.Join(t.TempDir(), "config.toml")
		if err := os.Mkdir(dirPath, 0o755); err != nil {
			t.Fatal(err)
		}
		legacyHostConfig = dirPath
		t.Cleanup(func() { legacyHostConfig = legacy })
		var buf bytes.Buffer
		stdout := captureProcessStdout(t, func() { RemoveLegacyHostConfig(&buf) })
		if !strings.Contains(buf.String(), fmt.Sprintf("  WARN: could not remove legacy containerd config %s:", dirPath)) {
			t.Errorf("the heal wrote %q to its writer; want the could-not-remove WARN for %s", buf.String(), dirPath)
		}
		if stdout != "" {
			t.Errorf("the heal printed %q onto the process's stdout; on the MCP stdio transport that stream is the JSON-RPC response", stdout)
		}
	})
}

// TestRewriteRoundTrip closes the producer↔matcher gap: it feeds captured
// `containerd config default` output — in BOTH the double-quoted (1.x) and
// single-quoted (2.x) styles — through the real rewriteContainerdConfig and
// asserts isStratumConfig accepts the result. If a future rendering change
// (spacing, quoting, a retargeted state dir) made the producer and matcher
// disagree, the legacy-config heal would silently stop reaping stratum's own legacy
// file; this test fails loudly instead of the fixture-mirrors-matcher tautology
// passing green.
func TestRewriteRoundTrip(t *testing.T) {
	samples := map[string]string{
		"containerd 1.x (double-quoted)": `version = 2
root = "/var/lib/containerd"
state = "/run/containerd"
[plugins."io.containerd.grpc.v1.cri"]
  sandbox_image = "registry.k8s.io/pause:3.9"
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
    SystemdCgroup = false
`,
		"containerd 2.x (single-quoted)": `version = 3
root = '/var/lib/containerd'
state = '/run/containerd'
[plugins.'io.containerd.cri.v1.images']
  sandbox_image = 'registry.k8s.io/pause:3.10'
`,
	}
	for name, sample := range samples {
		got := rewriteContainerdConfig(sample, "SystemdCgroup = false", false)
		if !isStratumConfig([]byte(got)) {
			t.Errorf("%s: rewriteContainerdConfig output is not recognised by isStratumConfig — producer/matcher drift:\n%s", name, got)
		}
		if strings.Contains(got, "root = '/var/lib/containerd'") || strings.Contains(got, `root = "/var/lib/containerd"`) {
			t.Errorf("%s: default root survived the rewrite:\n%s", name, got)
		}
	}
}

// LegacyHostConfig backs uninstall's dry-run disclosure: it must report present
// only when a stratum-written file is actually at the legacy path.
func TestLegacyHostConfigDetection(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	old := legacyHostConfig
	legacyHostConfig = p
	t.Cleanup(func() { legacyHostConfig = old })

	if _, present := LegacyHostConfig(); present {
		t.Error("reported present with no file on disk")
	}
	if err := os.WriteFile(p, []byte("root = '/var/lib/containerd'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, present := LegacyHostConfig(); present {
		t.Error("reported present for a host's own config — uninstall would wrongly warn it will delete it")
	}
	if err := os.WriteFile(p, []byte(stratumConfigBody()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, present := LegacyHostConfig(); !present {
		t.Error("did not report present for a stratum-written legacy config")
	}
}

// The legacy cleanup heals the host's containerd/Docker independently of
// rendering stratum's new config. In particular, a broken payload binary must
// not leave the version-4 file an older stratum wrote at the host path.
func TestEnsureConfigHealsLegacyConfigWhenRenderingFails(t *testing.T) {
	dir := t.TempDir()
	oldBin, oldConfig, oldLegacy := version.BinDir, version.ContainerdConfig, legacyHostConfig
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	legacyHostConfig = filepath.Join(dir, "legacy-config.toml")
	t.Cleanup(func() {
		version.BinDir, version.ContainerdConfig, legacyHostConfig = oldBin, oldConfig, oldLegacy
	})

	containerd := filepath.Join(dir, "containerd")
	if err := os.WriteFile(containerd, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyHostConfig, []byte(stratumConfigBody()), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnsureConfig(); err == nil {
		t.Fatal("EnsureConfig succeeded despite a failing containerd config default")
	}
	if _, err := os.Stat(legacyHostConfig); !os.IsNotExist(err) {
		t.Fatalf("legacy stratum config survived a failed render: stat err = %v", err)
	}
}

// WriteConfig is the upgrade's writer and must never reap the legacy file
// itself: a supervisor that predates reload still starts containerd with
// --config at that path, and deleting it there left containerd unable to start
// again. The upgrade heals only after containerd's reload lands.
func TestWriteConfigKeepsTheLegacyConfig(t *testing.T) {
	dir := t.TempDir()
	oldBin, oldConfig, oldLegacy := version.BinDir, version.ContainerdConfig, legacyHostConfig
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	legacyHostConfig = filepath.Join(dir, "legacy-config.toml")
	t.Cleanup(func() {
		version.BinDir, version.ContainerdConfig, legacyHostConfig = oldBin, oldConfig, oldLegacy
	})

	if err := os.WriteFile(filepath.Join(dir, "containerd"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyHostConfig, []byte(stratumConfigBody()), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _ = WriteConfig()
	if _, err := os.Stat(legacyHostConfig); err != nil {
		t.Fatalf("WriteConfig removed the legacy config a running supervisor may still pass: %v", err)
	}
}

// TestWriteKubeletConfigServingCert pins the three serving-cert branches, and in
// particular the worker one. A token-joined worker holds no local PKI,
// so it must bootstrap its serving cert via CSR (serverTLSBootstrap: true) — and
// must NOT be pointed at a kubelet.crt/kubelet.key it does not have (which
// would name nonexistent files and stop kubelet starting). Bootstrapping is what
// lets the operator's approver issue a CA-signed serving cert carrying the node's
// IP SAN; a self-signed fallback carries only a DNS SAN, so an IP-scraping,
// TLS-verifying client (metrics-server) could not reach the worker.
func TestWriteKubeletConfigServingCert(t *testing.T) {
	profiletest.Register(t)
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })

	render := func(t *testing.T, st *nodestate.NodeState, pkiHasKubeletLeaf bool) string {
		version.PKIDir = joinedPKIDir(t)
		version.K8sConfDir = t.TempDir()
		if pkiHasKubeletLeaf {
			for _, f := range []string{"kubelet.crt", "kubelet.key"} {
				if err := os.WriteFile(filepath.Join(version.PKIDir, f), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := writeKubeletConfig(st); err != nil {
			t.Fatalf("writeKubeletConfig: %v", err)
		}
		b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "kubelet.conf"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// CIS 4.1.1 wants kubelet.conf 0600, and a node that wrote it
	// 0644 before keeps that mode unless it is changed, since WriteFile sets
	// the mode only on a file it creates.
	t.Run("kubelet.conf is 0600, even over an existing 0644 one", func(t *testing.T) {
		render(t, &nodestate.NodeState{Role: "worker", Hostname: "w1"}, false)
		p := filepath.Join(version.K8sConfDir, "kubelet.conf")
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeKubeletConfig(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("kubelet.conf is %o, want 600", fi.Mode().Perm())
		}
	})

	// A worker with no recorded range uses the address it learned
	// from the kube-dns Service; a recorded range still wins.
	t.Run("cluster DNS: range, then the learned address, then the default", func(t *testing.T) {
		for _, tc := range []struct {
			st   *nodestate.NodeState
			want string
		}{
			{&nodestate.NodeState{Role: "worker", Hostname: "w1", ClusterDNSIP: "10.43.0.10"}, "clusterDNS: [10.43.0.10]"},
			{&nodestate.NodeState{Role: "worker", Hostname: "w1", ServiceCIDR: "10.44.0.0/16", ClusterDNSIP: "10.43.0.10"}, "clusterDNS: [10.44.0.10]"},
			{&nodestate.NodeState{Role: "worker", Hostname: "w1"}, "clusterDNS: [" + netdefault.DNSClusterIP + "]"},
		} {
			if got := render(t, tc.st, false); !strings.Contains(got, tc.want) {
				t.Errorf("%+v: kubelet.conf lacks %q:\n%s", tc.st, tc.want, got)
			}
		}
	})

	// A worker renews its own client certificate; a controller's is
	// a Stratum PKI leaf, renewed by the certificate loop, and is left alone.
	t.Run("only a worker rotates its client certificate", func(t *testing.T) {
		for _, tc := range []struct {
			st   *nodestate.NodeState
			leaf bool
			want bool
		}{
			{&nodestate.NodeState{Role: "worker", Hostname: "w1"}, false, true},
			{&nodestate.NodeState{Role: "worker", Hostname: "w1", CIS: true}, false, true},
			{&nodestate.NodeState{Role: "controller", Hostname: "cp1"}, true, false},
			{&nodestate.NodeState{Role: "controller", Hostname: "cp1", CIS: true}, true, false},
		} {
			got := strings.Contains(render(t, tc.st, tc.leaf), "rotateCertificates: true")
			if got != tc.want {
				t.Errorf("%+v: rotateCertificates set = %v, want %v", tc.st, got, tc.want)
			}
		}
	})

	t.Run("token-joined worker bootstraps a serving cert via CSR", func(t *testing.T) {
		got := render(t, &nodestate.NodeState{Role: "worker", Hostname: "w1"}, false)
		if !strings.Contains(got, "serverTLSBootstrap: true") {
			t.Errorf("a non-CIS worker must set serverTLSBootstrap: true so the operator issues a CA-signed serving cert with the node IP SAN; got:\n%s", got)
		}
		if strings.Contains(got, "tlsCertFile") {
			t.Errorf("a worker holds no local PKI — pinning tlsCertFile/tlsPrivateKeyFile would name files that do not exist and stop kubelet starting; got:\n%s", got)
		}
		// Off outside CIS: RuntimeDefault breaks some workloads that never
		// asked for it.
		if strings.Contains(got, "seccompDefault") {
			t.Errorf("a non-CIS node must not set seccompDefault; got:\n%s", got)
		}
	})

	t.Run("control-plane node with local PKI pins the static serving cert", func(t *testing.T) {
		got := render(t, &nodestate.NodeState{Role: "controller", Hostname: "cp1"}, true)
		for _, want := range []string{
			"serverTLSBootstrap: false",
			"tlsCertFile: " + version.PKIDir + "/kubelet.crt",
			"tlsPrivateKeyFile: " + version.PKIDir + "/kubelet.key",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("control-plane kubelet config missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("CIS node bootstraps via CSR and hardens kernel defaults", func(t *testing.T) {
		// PKI leaf present, but the CIS branch takes precedence over pinning it.
		got := render(t, &nodestate.NodeState{Role: "controller", Hostname: "cp1", CIS: true}, true)
		if !strings.Contains(got, "serverTLSBootstrap: true") || !strings.Contains(got, "protectKernelDefaults: true") || !strings.Contains(got, "seccompDefault: true") {
			t.Errorf("a CIS node must bootstrap its serving cert via CSR and set protectKernelDefaults and seccompDefault; got:\n%s", got)
		}
		if strings.Contains(got, "tlsCertFile") {
			t.Errorf("CIS bootstraps via CSR and must not pin a static serving cert, even when a PKI leaf exists; got:\n%s", got)
		}
	})
}

// The effective enable_selinux, read the way containerd reads it.
func enableSELinux(t *testing.T, cfg string) bool {
	t.Helper()
	var c struct {
		Plugins map[string]struct {
			EnableSELinux *bool `toml:"enable_selinux"`
		} `toml:"plugins"`
	}
	if err := toml.Unmarshal([]byte(cfg), &c); err != nil {
		t.Fatal(err)
	}
	v := c.Plugins["io.containerd.cri.v1.runtime"].EnableSELinux
	if v == nil {
		t.Fatal("no enable_selinux in the CRI runtime plugin")
	}
	return *v
}

// A labelled node confines its containers; any other node does not.
func TestRewriteSetsEnableSELinux(t *testing.T) {
	for _, on := range []bool{true, false} {
		got := rewriteContainerdConfig(containerdDefault(t), "SystemdCgroup = true", on)
		if enableSELinux(t, got) != on || strings.Count(got, "enable_selinux") != 1 {
			t.Errorf("confine=%v: enable_selinux = %v in\n%s", on, enableSELinux(t, got), got)
		}
	}
}

// A node that becomes labelled, or stops being labelled, gets its containerd
// config rewritten on the next start; a start that changes nothing keeps it.
func TestEnsureConfigFollowsTheNodesLabelling(t *testing.T) {
	dir := t.TempDir()
	oldBin, oldConfig, oldSocket, oldLabelled := version.BinDir, version.ContainerdConfig, version.ControlSocket, selinuxLabelled
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	version.ControlSocket = filepath.Join(dir, "supervisor.sock")
	t.Cleanup(func() {
		version.BinDir, version.ContainerdConfig, version.ControlSocket, selinuxLabelled = oldBin, oldConfig, oldSocket, oldLabelled
	})
	fixture, err := filepath.Abs(filepath.Join("testdata", "containerd-2.3-default.toml"))
	if err != nil {
		t.Fatal(err)
	}
	containerd := filepath.Join(dir, "containerd")
	render := func() {
		t.Helper()
		if err := os.WriteFile(containerd, []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, on := range []bool{false, true, false} {
		selinuxLabelled = func() bool { return on }
		render()
		if _, err := ensureConfig(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(version.ContainerdConfig)
		if err != nil {
			t.Fatal(err)
		}
		if enableSELinux(t, string(b)) != on {
			t.Errorf("labelled=%v: written enable_selinux = %v", on, !on)
		}
		if err := os.WriteFile(containerd, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := ensureConfig(); err != nil {
			t.Errorf("labelled=%v: second start re-rendered a current config: %v", on, err)
		}
	}
}

// The confinement record follows the config: written when containerd starts
// confining, kept by later starts, backfilled from the config's age on a node
// confined before the record existed, and removed when confinement stops.
func TestEnsureConfigRecordsWhenConfinementBegan(t *testing.T) {
	dir := t.TempDir()
	oldBin, oldConfig, oldSocket, oldLabelled := version.BinDir, version.ContainerdConfig, version.ControlSocket, selinuxLabelled
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	version.ControlSocket = filepath.Join(dir, "supervisor.sock")
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
	start := func(labelled bool) {
		t.Helper()
		selinuxLabelled = func() bool { return labelled }
		if _, err := ensureConfig(); err != nil {
			t.Fatal(err)
		}
	}

	start(false)
	if _, ok := ConfinedSince(); ok {
		t.Fatal("a node that does not confine containers has a confinement record")
	}
	before := time.Now()
	start(true)
	since, ok := ConfinedSince()
	if !ok || since.Before(before.Add(-time.Second)) {
		t.Fatalf("after confinement began: ConfinedSince = %v, %v; want a time from this start", since, ok)
	}
	start(true)
	if again, _ := ConfinedSince(); !again.Equal(since) {
		t.Errorf("a later start moved the record from %v to %v", since, again)
	}

	if err := os.Remove(confinedPath()); err != nil {
		t.Fatal(err)
	}
	written := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(version.ContainerdConfig, written, written); err != nil {
		t.Fatal(err)
	}
	start(true)
	if got, ok := ConfinedSince(); !ok || !got.Equal(written) {
		t.Errorf("a node confined before the record existed: ConfinedSince = %v, %v; want the config's age %v", got, ok, written)
	}

	if changed, err := DisableSELinux(); err != nil || !changed {
		t.Fatalf("DisableSELinux = %v, %v; want true, nil", changed, err)
	}
	if _, ok := ConfinedSince(); ok {
		t.Error("DisableSELinux left the confinement record")
	}
	start(true)
	if got, ok := ConfinedSince(); !ok || got.Before(before) {
		t.Errorf("confinement turned on again: ConfinedSince = %v, %v; want a time from this start", got, ok)
	}
	start(false)
	if _, ok := ConfinedSince(); ok {
		t.Error("a start that stops confinement left the record")
	}
}

// DisableSELinux turns enable_selinux off in a config that has it on, and
// leaves any other config alone.
func TestDisableSELinux(t *testing.T) {
	old := version.ContainerdConfig
	version.ContainerdConfig = filepath.Join(t.TempDir(), "containerd.toml")
	t.Cleanup(func() { version.ContainerdConfig = old })

	if changed, err := DisableSELinux(); err != nil || changed {
		t.Fatalf("no config: DisableSELinux = %v, %v; want false, nil", changed, err)
	}
	on := rewriteContainerdConfig(containerdDefault(t), "SystemdCgroup = true", true)
	if err := os.WriteFile(version.ContainerdConfig, []byte(on), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := DisableSELinux(); err != nil || !changed {
		t.Fatalf("DisableSELinux = %v, %v; want true, nil", changed, err)
	}
	b, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if enableSELinux(t, string(b)) || string(b) != rewriteContainerdConfig(containerdDefault(t), "SystemdCgroup = true", false) {
		t.Error("DisableSELinux changed more than enable_selinux, or did not turn it off")
	}
	if changed, err := DisableSELinux(); err != nil || changed {
		t.Errorf("second call: DisableSELinux = %v, %v; want false, nil", changed, err)
	}
}

// ConfinesContainers reads enable_selinux from the CRI runtime table of the
// config Stratum writes from containerd's own default.
func TestConfinesContainers(t *testing.T) {
	old := version.ContainerdConfig
	version.ContainerdConfig = filepath.Join(t.TempDir(), "containerd.toml")
	t.Cleanup(func() { version.ContainerdConfig = old })
	if on, err := ConfinesContainers(); err != nil || on {
		t.Fatalf("no config: ConfinesContainers = %v, %v; want false, nil", on, err)
	}
	for _, confine := range []bool{true, false} {
		if err := os.WriteFile(version.ContainerdConfig, []byte(rewriteContainerdConfig(containerdDefault(t), "SystemdCgroup = true", confine)), 0o644); err != nil {
			t.Fatal(err)
		}
		if on, err := ConfinesContainers(); err != nil || on != confine {
			t.Errorf("written with confine=%v: ConfinesContainers = %v, %v", confine, on, err)
		}
	}
	if err := os.WriteFile(version.ContainerdConfig, []byte("[plugins\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfinesContainers(); err == nil {
		t.Error("a config that does not parse read as a setting")
	}
}

// A DisableSELinux that finds confinement already off still clears the
// record, so a retry after one stopped between the rename and the clear
// leaves config and record in step.
func TestDisableSELinuxClearsAStaleRecordWhenAlreadyOff(t *testing.T) {
	old := version.ContainerdConfig
	version.ContainerdConfig = filepath.Join(t.TempDir(), "containerd.toml")
	t.Cleanup(func() { version.ContainerdConfig = old })
	off := rewriteContainerdConfig(containerdDefault(t), "SystemdCgroup = true", false)
	if err := os.WriteFile(version.ContainerdConfig, []byte(off), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(confinedPath(), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := DisableSELinux(); err != nil || changed {
		t.Fatalf("DisableSELinux = %v, %v; want false, nil", changed, err)
	}
	if _, ok := ConfinedSince(); ok {
		t.Error("a stale confinement record outlived a config with confinement off")
	}
}

// TestKubeletConfCISLinesAreExactlyTheProfile pins the CIS half of kubelet.conf
// the way the apiserver's argv is pinned: renders are compared with each other,
// byte for byte, not by substring. The renders differ only in the CIS flag —
// and, for the serving-certificate branch, only in whether the PKI leaf exists
// — so a CIS line that moves, changes value or disappears, or a
// serving-certificate branch that changes, fails, while an unrelated template
// change applied to every node kind passes. Both directories are redirected
// once for the whole test because both appear in the rendered lines, and
// between renders only the kubelet leaf changes.
func TestKubeletConfCISLinesAreExactlyTheProfile(t *testing.T) {
	profiletest.Register(t)
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })
	version.PKIDir = joinedPKIDir(t)
	version.K8sConfDir = t.TempDir()

	leaf := func(on bool) {
		t.Helper()
		for _, f := range []string{"kubelet.crt", "kubelet.key"} {
			p := filepath.Join(version.PKIDir, f)
			if on {
				if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
	}
	// render renders kubelet.conf for node states that differ only in the CIS
	// flag (and the leaf the serving-cert branch reads), keeping the role
	// fixed — only a worker carries rotateCertificates.
	render := func(role string, cis, hasLeaf bool) string {
		t.Helper()
		leaf(hasLeaf)
		if err := WriteKubeletConfig(&nodestate.NodeState{
			Role: role, Hostname: "cp1", NodeIP: "192.0.2.10",
			PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12", CIS: cis,
		}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "kubelet.conf"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// The CIS delta is exactly this run of lines, byte for byte: removing it
	// from a CIS render must produce the non-CIS one for the same node, so a
	// line that moves, is re-ordered or re-worded fails as loudly as one that
	// disappears.
	minusCIS := func(s string) string {
		return strings.Replace(s, "protectKernelDefaults: true\nseccompDefault: true\n", "", 1)
	}

	// A controller: four renders over node states that differ only in CIS, with
	// and without the kubelet serving-cert leaf a control plane carries.
	cisLeaf := render("controller", true, true)
	cisNoLeaf := render("controller", true, false)
	plainNoLeaf := render("controller", false, false)
	plainLeaf := render("controller", false, true)

	if cisLeaf != cisNoLeaf {
		t.Errorf("the CIS render must not depend on the serving-cert leaf (CIS bootstraps via CSR):\nwith leaf:\n%s\nwithout:\n%s", cisLeaf, cisNoLeaf)
	}
	if got, want := plainNoLeaf, minusCIS(cisNoLeaf); got != want {
		t.Errorf("outside CIS, a controller's kubelet.conf is not the CIS render minus protectKernelDefaults and seccompDefault:\ngot:\n%s\nwant:\n%s", got, want)
	}
	wantLeaf := strings.Replace(plainNoLeaf, "serverTLSBootstrap: true\n",
		"serverTLSBootstrap: false\ntlsCertFile: "+version.PKIDir+"/kubelet.crt\ntlsPrivateKeyFile: "+version.PKIDir+"/kubelet.key\n", 1)
	if plainLeaf != wantLeaf {
		t.Errorf("outside CIS, the leaf must pin exactly the static serving pair instead of bootstrapping:\ngot:\n%s\nwant:\n%s", plainLeaf, wantLeaf)
	}

	// A worker holds no leaf; only CIS differs. rotateCertificates and the
	// intentional serving-cert bootstrap are shared by both renders, so the
	// delta is the same two lines.
	plainWorker := render("worker", false, false)
	if got, want := plainWorker, minusCIS(render("worker", true, false)); got != want {
		t.Errorf("outside CIS, a worker's kubelet.conf is not the CIS render minus protectKernelDefaults and seccompDefault:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// nodeConfigsFixture redirects every path the node-config writers touch to a
// temp dir and stages a containerd stub that prints the captured real default,
// so PrepareNodeConfigs and WriteNodeConfigs run their full write path —
// containerd config, registry no-op, runtime-handler drop-in, kubelet.conf. It seeds a stratum-written
// file at the legacy host-config path and returns that path, so a test can pin
// whether the entry point reaps it.
func nodeConfigsFixture(t *testing.T) string {
	t.Helper()
	const want = "ghcr.io/ctrliq/stratum/pause:3.10.2"
	dir := t.TempDir()
	oldBin, oldConfig, oldSocket, oldImage := version.BinDir, version.ContainerdConfig, version.ControlSocket, SandboxImage
	oldReg, oldCerts, oldConfD, oldRuntimeConfD := version.RegistriesConfig, version.CertsDir, version.ContainerdConfD, version.ContainerdRuntimeConfD
	oldConf, oldPKI, oldLegacy := version.K8sConfDir, version.PKIDir, legacyHostConfig
	t.Cleanup(func() {
		version.BinDir, version.ContainerdConfig, version.ControlSocket, SandboxImage = oldBin, oldConfig, oldSocket, oldImage
		version.RegistriesConfig, version.CertsDir, version.ContainerdConfD, version.ContainerdRuntimeConfD = oldReg, oldCerts, oldConfD, oldRuntimeConfD
		version.K8sConfDir, version.PKIDir, legacyHostConfig = oldConf, oldPKI, oldLegacy
	})
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	version.ControlSocket = filepath.Join(dir, "supervisor.sock")
	// Absent, so registry.Apply is a deterministic no-op and these tests'
	// verdicts are about the two node configs, not about mirrors.
	version.RegistriesConfig = filepath.Join(dir, "registries.yaml")
	version.CertsDir = filepath.Join(dir, "certs.d")
	version.ContainerdConfD = filepath.Join(dir, "conf.d", "stratum-registry.toml")
	version.ContainerdRuntimeConfD = filepath.Join(dir, "conf.d", "stratum-runtime.toml")
	version.K8sConfDir = filepath.Join(dir, "kubelet")
	version.PKIDir = joinedPKIDir(t)
	SandboxImage = want
	fixture, err := filepath.Abs(filepath.Join("testdata", "containerd-2.3-default.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "containerd"), []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, "legacy-config.toml")
	if err := os.WriteFile(legacy, []byte(stratumConfigBody()), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyHostConfig = legacy
	return legacy
}

// kubeletConfParses asserts the written kubelet.conf is real YAML carrying the
// settings its readers parse it for: kubelet reads the file, and the hardening
// audit unmarshals the same keys, so a written file that does not parse fails
// here rather than on a node.
func kubeletConfParses(t *testing.T) {
	t.Helper()
	p := filepath.Join(version.K8sConfDir, "kubelet.conf")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var conf map[string]interface{}
	if err := yaml.Unmarshal(b, &conf); err != nil {
		t.Fatalf("written kubelet.conf does not parse: %v\n%s", err, b)
	}
	if _, ok := conf["clusterDNS"]; !ok {
		t.Errorf("written kubelet.conf carries no clusterDNS:\n%s", b)
	}
}

// TestPrepareNodeConfigsHealsTheLegacyConfigAndWritesBothConfigs owns the join
// path's entry point (internal/cluster and internal/ha call it): it is
// EnsureConfig plus kubelet.conf, so the legacy-config heal belongs there — a
// joining node with a legacy host config would still crash-loop the distro
// containerd after a reboot. Both configs it exists to write must land: a
// containerd config isStratumConfig recognises with the payload's pause image
// pinned, and a kubelet.conf that parses. The heal's failure path is pinned by
// TestEnsureConfigHealsLegacyConfigWhenRenderingFails; this pins the success
// path the join actually runs.
func TestPrepareNodeConfigsHealsTheLegacyConfigAndWritesBothConfigs(t *testing.T) {
	legacy := nodeConfigsFixture(t)
	if err := PrepareNodeConfigs(&nodestate.NodeState{Role: "worker", Hostname: "w1", ServiceCIDR: "10.96.0.0/12"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("the legacy host config survived PrepareNodeConfigs: stat err = %v", err)
	}
	b, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !isStratumConfig(b) {
		t.Errorf("written containerd config is not recognised by isStratumConfig:\n%s", b)
	}
	if s := pinnedSandbox(t, string(b)); s != SandboxImage {
		t.Errorf("written pinned_images.sandbox = %q, want %q", s, SandboxImage)
	}
	kubeletConfParses(t)
}

// TestWriteNodeConfigsKeepsTheLegacyConfigAndWritesBoth owns the upgrade's
// entry point (internal/upgrade wires it as writeNodeConfigs): on the success
// path — the one an in-place upgrade actually runs — both node configs must
// land while the legacy host config survives untouched, because a supervisor
// predating reload still launches containerd with --config at that path.
// TestWriteConfigKeepsTheLegacyConfig pins the failing-render side only; the
// success side, where both configs land, was unpinned.
func TestWriteNodeConfigsKeepsTheLegacyConfigAndWritesBoth(t *testing.T) {
	legacy := nodeConfigsFixture(t)
	before, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteNodeConfigs(&nodestate.NodeState{Role: "worker", Hostname: "w1", ServiceCIDR: "10.96.0.0/12"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal("the legacy host config was removed while a supervisor may still pass it:", err)
	}
	if string(before) != string(after) {
		t.Errorf("WriteNodeConfigs modified the legacy host config:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	b, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !isStratumConfig(b) {
		t.Errorf("written containerd config is not recognised by isStratumConfig:\n%s", b)
	}
	if s := pinnedSandbox(t, string(b)); s != SandboxImage {
		t.Errorf("written pinned_images.sandbox = %q, want %q", s, SandboxImage)
	}
	kubeletConfParses(t)
}

// ---------------------------------------------------------------------------- ApplyRegistry

// registryFixture redirects the registry paths to a temp tree and writes a
// one-mirror registries.yaml, so ApplyRegistry performs a real first apply:
// a certs.d hosts.toml written and the conf.d drop-in created (Changed).
func registryFixture(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	oldCerts, oldConfd, oldReg := version.CertsDir, version.ContainerdConfD, version.RegistriesConfig
	t.Cleanup(func() {
		version.CertsDir, version.ContainerdConfD, version.RegistriesConfig = oldCerts, oldConfd, oldReg
	})
	version.CertsDir = filepath.Join(root, "certs.d")
	version.ContainerdConfD = filepath.Join(root, "conf.d", "stratum-registry.toml")
	version.RegistriesConfig = filepath.Join(root, "registries.yaml")
	const oneMirror = "mirrors:\n  docker.io:\n    endpoint: [\"https://mirror.internal:5000\"]\n"
	if err := os.WriteFile(version.RegistriesConfig, []byte(oneMirror), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeSupervisor serves the supervisor control protocol on a temp unix socket
// and points version.ControlSocket at it, so comps sees containerd running:
// status reports pid 100 until a restart op is served, then pid 101 — the new
// pid comps.Restart's replacement check waits for. With refuseRestart the
// restart op is refused, for the error path. Returns a func counting the
// restart ops served.
func fakeSupervisor(t *testing.T, refuseRestart bool) (restarts func() int) {
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
				if refuseRestart {
					out.OK, out.Error = false, "restart refused"
				}
			}
			mu.Unlock()
			_ = json.NewEncoder(conn).Encode(out)
			conn.Close()
		}
	}()
	return func() int { mu.Lock(); defer mu.Unlock(); return served }
}

// TestApplyRegistry_ReportsWhetherItRestarted pins ApplyRegistry's contract: it
// returns a runtime-level result — the registry result plus Restarted — and
// Restarted is true only when containerd was actually restarted: the conf.d
// drop-in changed AND containerd was running. Changed alone only means the
// drop-in changed, so a caller that derives the restart report from it reports
// restarts that did not happen.
func TestApplyRegistry_ReportsWhetherItRestarted(t *testing.T) {
	t.Run("drop-in changed and containerd active: restarts and reports Restarted", func(t *testing.T) {
		restarts := fakeSupervisor(t, false)
		registryFixture(t)
		res, err := ApplyRegistry()
		if err != nil {
			t.Fatal(err)
		}
		if !res.Changed {
			t.Fatal("the first apply did not report a changed drop-in — the fixture is wrong")
		}
		if !res.Restarted {
			t.Error("ApplyRegistry restarted containerd but reported Restarted=false")
		}
		if n := restarts(); n != 1 {
			t.Errorf("the supervisor served %d restart ops, want 1", n)
		}
	})

	t.Run("drop-in changed but containerd stopped: no restart, Restarted=false", func(t *testing.T) {
		registryFixture(t)
		old := version.ControlSocket
		version.ControlSocket = filepath.Join(t.TempDir(), "absent.sock")
		t.Cleanup(func() { version.ControlSocket = old })

		res, err := ApplyRegistry()
		if err != nil {
			t.Fatal(err)
		}
		if !res.Changed {
			t.Fatal("the first apply did not report a changed drop-in — the fixture is wrong")
		}
		if res.Restarted {
			t.Error("nothing was running to restart, but the result reported Restarted=true")
		}
	})

	t.Run("re-apply with an unchanged drop-in: no restart, Restarted=false", func(t *testing.T) {
		restarts := fakeSupervisor(t, false)
		registryFixture(t)
		if _, err := ApplyRegistry(); err != nil {
			t.Fatal(err)
		}
		res, err := ApplyRegistry()
		if err != nil {
			t.Fatal(err)
		}
		if res.Changed {
			t.Fatal("the identical re-apply reported a changed drop-in — the fixture is wrong")
		}
		if res.Restarted {
			t.Error("nothing changed on re-apply, but the result reported Restarted=true")
		}
		if n := restarts(); n != 1 {
			t.Errorf("the supervisor served %d restart ops, want the first apply's 1 only", n)
		}
	})

	t.Run("restart refused: the error carries and Restarted stays false", func(t *testing.T) {
		restarts := fakeSupervisor(t, true)
		registryFixture(t)
		res, err := ApplyRegistry()
		if err == nil {
			t.Fatal("a refused restart did not fail ApplyRegistry")
		}
		if res.Restarted {
			t.Error("the restart was refused, but the result reported Restarted=true")
		}
		if n := restarts(); n != 1 {
			t.Errorf("the supervisor served %d restart ops, want 1", n)
		}
	})
}

// TestWaitActiveNamesTheSupervisorLog pins the hint on containerd's start-up
// timeout: containerd runs under the stratum supervisor, which logs it to
// LogDir/containerd.log, and there is no containerd systemd unit for journalctl
// to read. LogDir is moved to a temp dir so the hint must follow the variable,
// not a hard-coded path.
func TestWaitActiveNamesTheSupervisorLog(t *testing.T) {
	oldSock, oldLog := version.ControlSocket, version.LogDir
	version.ControlSocket = filepath.Join(t.TempDir(), "absent.sock")
	version.LogDir = filepath.Join(t.TempDir(), "log")
	t.Cleanup(func() { version.ControlSocket, version.LogDir = oldSock, oldLog })

	err := waitActive(0)
	if err == nil {
		t.Fatal("containerd is not running, but waitActive reported it active")
	}
	if want := filepath.Join(version.LogDir, "containerd.log"); !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name containerd's log %s", err, want)
	}
	if strings.Contains(err.Error(), "journalctl") {
		t.Errorf("error %q points at journalctl, but containerd is not a systemd unit", err)
	}
}

// ------------------------------------------------------------------ config render reports its change

// TestWriteConfig_LeavesTheRestartToTheCaller pins that the config render
// reports whether it changed the file and restarts nothing — the caller owns
// the restart. With a fake supervisor reporting containerd as running, a first
// render reports changed and the supervisor serves no restart request, and an
// identical re-render reports unchanged and still restarts nothing. The render
// must restart nothing itself: the upgrade reaches this render through
// WriteConfig/WriteNodeConfigs and restarts containerd again itself, and
// `stratum up` reaches it through EnsureConfig.
func TestWriteConfig_LeavesTheRestartToTheCaller(t *testing.T) {
	// The fixture order matters: nodeConfigsFixture points ControlSocket at a
	// dead path; fakeSupervisor re-points it at a serving fake.
	nodeConfigsFixture(t)
	restarts := fakeSupervisor(t, false)
	if !Active() {
		t.Fatal("fixture: the fake supervisor does not report containerd running")
	}

	changed, err := WriteConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("the first render wrote a new containerd config but reported changed=false")
	}
	if n := restarts(); n != 0 {
		t.Errorf("rendering the config served %d containerd restart ops; the render must restart nothing and leave the restart to the caller", n)
	}
	b, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !isStratumConfig(b) {
		t.Error("the first render did not write a config isStratumConfig recognises — the fixture is wrong")
	}

	before := b
	if changed, err = WriteConfig(); err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("an identical re-render of a current config reported changed=true")
	}
	if n := restarts(); n != 0 {
		t.Errorf("a re-render served %d containerd restart ops, want 0", n)
	}
	after, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("a re-render that reported unchanged modified the config")
	}
}

// EnsureConfig (`stratum up`'s prepare call, internal/cluster/run.go) and
// WriteNodeConfigs (the in-place upgrade's writer, internal/upgrade/node.go)
// sit on the same ensureConfig path and carry the same contract as WriteConfig:
// they report whether the render changed the containerd config and restart
// nothing. `stratum up` restarts containerd only when the result reports a
// change, and the upgrade folds the change into its own single host reload —
// so neither entry point may restart containerd here.
func TestEnsureConfigAndWriteNodeConfigs_LeaveTheRestartToTheCaller(t *testing.T) {
	for _, e := range []struct {
		name   string
		render func() (bool, error)
	}{
		{"EnsureConfig", func() (bool, error) { return EnsureConfig() }},
		{"WriteNodeConfigs", func() (bool, error) {
			return WriteNodeConfigs(&nodestate.NodeState{Role: "worker", Hostname: "w1", ServiceCIDR: "10.96.0.0/12"})
		}},
	} {
		t.Run(e.name, func(t *testing.T) {
			nodeConfigsFixture(t)
			restarts := fakeSupervisor(t, false)
			if !Active() {
				t.Fatal("fixture: the fake supervisor does not report containerd running")
			}

			changed, err := e.render()
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Errorf("%s wrote a new containerd config but reported changed=false", e.name)
			}
			if n := restarts(); n != 0 {
				t.Errorf("%s served %d containerd restart ops; the render must restart nothing", e.name, n)
			}

			if changed, err = e.render(); err != nil {
				t.Fatal(err)
			}
			if changed {
				t.Errorf("%s re-rendered a current config and reported changed=true", e.name)
			}
			if n := restarts(); n != 0 {
				t.Errorf("%s's re-render served %d containerd restart ops, want 0", e.name, n)
			}
		})
	}
}

// failDirSync points this package's write seams at a directory sync that
// never completes — the window a power loss leaves behind: the real atomic
// writer renames the new file into place and then reports ErrNotDurable. The
// override lasts for one test; the tests that use it are not parallel.
func failDirSync(t *testing.T) {
	t.Helper()
	syncDir = func(string) error { return syscall.EIO }
	t.Cleanup(func() { syncDir = atomicfile.SyncDir })
}

// TestEnsureConfigReportsTheChangeWhenTheSyncFails pins the change flag's
// contract past a post-rename failure: the config the render wrote is already
// in place when the directory sync fails, so reporting "unchanged" would tell
// the caller that folds this into its own host reload that the config did not
// move. The report is changed-with-error, the file on disk is the rendered
// one, and the retry takes the unchanged fast path without writing or syncing.
func TestEnsureConfigReportsTheChangeWhenTheSyncFails(t *testing.T) {
	// The render's confinement choice is the host's to make; the assertion
	// below reads the file, so the choice is pinned unlabelled — on a
	// stratum-labelled host the render would carry enable_selinux = true and
	// the test would fire as a false alarm.
	oldLabelled := selinuxLabelled
	selinuxLabelled = func() bool { return false }
	t.Cleanup(func() { selinuxLabelled = oldLabelled })
	const want = "ghcr.io/ctrliq/stratum/pause:3.10.2"
	dir := t.TempDir()
	oldBin, oldConfig, oldSocket, oldImage := version.BinDir, version.ContainerdConfig, version.ControlSocket, SandboxImage
	version.BinDir = dir
	version.ContainerdConfig = filepath.Join(dir, "config", "containerd.toml")
	version.ControlSocket = filepath.Join(dir, "supervisor.sock")
	SandboxImage = want
	t.Cleanup(func() {
		version.BinDir, version.ContainerdConfig, version.ControlSocket, SandboxImage = oldBin, oldConfig, oldSocket, oldImage
	})
	fixture, err := filepath.Abs(filepath.Join("testdata", "containerd-2.3-default.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "containerd"), []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	failDirSync(t)
	changed, err := ensureConfig()
	if !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("ensureConfig = %v, want the write's ErrNotDurable", err)
	}
	if !changed {
		t.Error("ensureConfig reported no change to a config the rename already replaced")
	}
	b, err := os.ReadFile(version.ContainerdConfig)
	if err != nil || !strings.Contains(string(b), want) || !strings.Contains(string(b), enableSELinuxAssign(false)) {
		t.Errorf("the config on disk after the failed sync = %q, %v; want the rendered one in place", b, err)
	}

	changed, err = ensureConfig()
	if err != nil {
		t.Fatalf("the retry = %v; want the unchanged fast path over the installed config", err)
	}
	if changed {
		t.Error("the retry reported a change to the config it had just written")
	}
	// The sync seam is still failing here: the fast path passes because it
	// never writes, so it never syncs — a retry that rewrote would hit the
	// still-failing sync and fail loudly.
}

// TestDisableSELinuxReportsTheChangeWhenTheSyncFails pins the same contract on
// DisableSELinux: its config write renames enable_selinux = false in place and
// then fails the directory sync, so the report is changed-with-error — the
// rollback caller must not be told the config was left untouched when it was
// replaced.
func TestDisableSELinuxReportsTheChangeWhenTheSyncFails(t *testing.T) {
	oldConfig := version.ContainerdConfig
	t.Cleanup(func() { version.ContainerdConfig = oldConfig })
	version.ContainerdConfig = filepath.Join(t.TempDir(), "config", "containerd.toml")
	if err := os.MkdirAll(filepath.Dir(version.ContainerdConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	on := strings.Replace(containerdDefault(t), "enable_selinux = false", "enable_selinux = true", 1)
	if err := os.WriteFile(version.ContainerdConfig, []byte(on), 0o644); err != nil {
		t.Fatal(err)
	}

	failDirSync(t)
	changed, err := DisableSELinux()
	if !errors.Is(err, atomicfile.ErrNotDurable) {
		t.Fatalf("DisableSELinux = %v, want the write's ErrNotDurable", err)
	}
	if !changed {
		t.Error("DisableSELinux reported no change to a config the rename already replaced")
	}
	b, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "enable_selinux = true") {
		t.Errorf("enable_selinux is still on after the replaced config:\n%s", b)
	}
}

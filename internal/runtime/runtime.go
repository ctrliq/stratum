// Package runtime configures containerd from the staged payload.
package runtime

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/registry"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/version"
)

// Active reports whether containerd is running.
func Active() bool { return comps.IsActive("containerd") }

// ApplyRegistryResult adds Restarted: Changed alone means only the drop-in changed.
type ApplyRegistryResult struct {
	registry.Result
	Restarted bool
}

// ApplyRegistry restarts containerd only for a config_path change; hosts.toml files are read live.
func ApplyRegistry() (ApplyRegistryResult, error) {
	res, err := registry.Apply()
	out := ApplyRegistryResult{Result: res}
	if err != nil {
		return out, err
	}
	if out.Changed && Active() {
		if err := sharedConfDParseError(); err != nil {
			return out, fmt.Errorf("%w; the registry config was written, but containerd was not restarted to load it. Fix or remove that file, then run `stratum runtime restart`", err)
		}
		restarted, err := restartContainerd(" after registry config change")
		out.Restarted = restarted
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

const containerdRestartWait = 10 * time.Second

// restartContainerd: restarted is true even if containerd then dies.
func restartContainerd(cause string) (restarted bool, err error) {
	if err := comps.Restart("containerd"); err != nil {
		return false, fmt.Errorf("restart containerd%s: %w", cause, err)
	}
	return true, waitActive(containerdRestartWait)
}

// waitActive names the supervisor's log file: containerd is a supervisor child, not a systemd unit.
func waitActive(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for !Active() {
		if time.Now().After(deadline) {
			return fmt.Errorf("containerd did not become active within %s (see %s)",
				timeout, filepath.Join(version.LogDir, "containerd.log"))
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func installed() bool {
	// containerd ships in the stratum payload: staged, not dnf-installed.
	_, err := os.Stat(filepath.Join(version.BinDir, "containerd"))
	return err == nil
}

func install() error {
	return fmt.Errorf("containerd missing from %s — the payload was not staged (run `stratum stage`, or this is an EMBEDDED_BINS=none build)", version.BinDir)
}

// SandboxImage is the mirrored pause pin; containerd's default points at registry.k8s.io.
var SandboxImage = "ghcr.io/ctrliq/stratum/pause:unknown"

// sandboxImageLine matches every version's spelling; `sandboxer = 'podsandbox'` must not match.
var sandboxImageLine = regexp.MustCompile(`(?m)^(\s*(?:sandbox_image|sandbox)\s*=\s*)['"][^'"]*['"]`)

// selinuxLabelled is a var so a test can stand in for a labelled node.
var selinuxLabelled = selinux.Labelled

// enableSELinuxLine matches enable_selinux in the CRI runtime plugin's table.
var enableSELinuxLine = regexp.MustCompile(`(?m)^(\s*)enable_selinux\s*=\s*(?:true|false)`)

// DisableSELinux: with labelling on, rollback pods run as container_t, denied PKI and etcd data.
func DisableSELinux() (bool, error) {
	cfg := version.ContainerdConfig
	b, err := os.ReadFile(cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return false, recordConfinement(false, time.Time{})
	}
	if err != nil {
		return false, err
	}
	if !strings.Contains(string(b), enableSELinuxAssign(true)) {
		return false, recordConfinement(false, time.Time{})
	}
	out := enableSELinuxLine.ReplaceAllString(string(b), "${1}"+enableSELinuxAssign(false))
	// A fixed .tmp name is one two writers could tear.
	if err := atomicfile.WriteFileWith(cfg, []byte(out), 0o644, atomicfileSeams()); err != nil {
		// ErrNotDurable is post-rename: the new config is already in place.
		return errors.Is(err, atomicfile.ErrNotDurable), err
	}
	return true, recordConfinement(false, time.Time{})
}

// syncDir is a var so a test can inject a failure into the real directory sync.
var syncDir = atomicfile.SyncDir

func atomicfileSeams() atomicfile.Seams {
	return atomicfile.Seams{SyncDir: syncDir}
}

// ConfinesContainers reads as containerd reads it, so the key in another table does not count.
func ConfinesContainers() (bool, error) {
	b, err := os.ReadFile(version.ContainerdConfig)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var cfg struct {
		Plugins map[string]struct {
			EnableSELinux bool `toml:"enable_selinux"`
		} `toml:"plugins"`
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return false, fmt.Errorf("parse %s: %w", version.ContainerdConfig, err)
	}
	return cfg.Plugins["io.containerd.cri.v1.runtime"].EnableSELinux, nil
}

func confinedPath() string {
	return filepath.Join(filepath.Dir(version.ContainerdConfig), "selinux-confined-since")
}

// ConfinedSincePath is the file recording when containerd began confining containers.
func ConfinedSincePath() string { return confinedPath() }

// ConfinedSince: a sandbox started before then runs without labels until the pod is recreated.
func ConfinedSince() (time.Time, bool) {
	b, err := os.ReadFile(confinedPath())
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return t, err == nil
}

func recordConfinement(confine bool, began time.Time) error {
	if !confine {
		if err := os.Remove(confinedPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if _, ok := ConfinedSince(); ok {
		return nil
	}
	return atomicfile.WriteFile(confinedPath(), []byte(began.UTC().Format(time.RFC3339Nano)+"\n"), 0o644)
}

// enableSELinuxAssign is on when the node is labelled, so containers run as container_t.
func enableSELinuxAssign(on bool) string {
	if on {
		return "enable_selinux = true"
	}
	return "enable_selinux = false"
}

// ensureConfig renders the kubelet config from the same CgroupDriver() so the two cannot disagree.
func ensureConfig() (bool, error) { return ensureConfigWithWriter(os.Stdout) }

func ensureConfigWithWriter(w io.Writer) (bool, error) {
	cfg := version.ContainerdConfig
	want := "SystemdCgroup = false"
	if CgroupDriver() == "systemd" {
		want = "SystemdCgroup = true"
	}
	confine := selinuxLabelled()
	// Both settings: an older config can have the right driver and the wrong sandbox image.
	prev, prevErr := os.ReadFile(cfg)
	var written time.Time
	if fi, err := os.Stat(cfg); err == nil {
		written = fi.ModTime()
	}
	if prevErr == nil &&
		strings.Contains(string(prev), want) && strings.Contains(string(prev), SandboxImage) &&
		strings.Contains(string(prev), enableSELinuxAssign(confine)) && importsDropIns(string(prev)) {
		if err := recordConfinement(confine, written); err != nil {
			return false, err
		}
		return false, nil
	}
	out, err := exec.Command(filepath.Join(version.BinDir, "containerd"), "config", "default").Output()
	if err != nil {
		return false, err
	}
	// Fail closed: no recognizable sandbox setting means containerd renamed the key again.
	if !sandboxImageLine.MatchString(string(out)) {
		return false, fmt.Errorf("containerd config default has no sandbox image setting stratum recognises; refusing to write a config that would pull pause from the internet instead of %s", SandboxImage)
	}
	if imports, err := configImports(string(out)); err != nil {
		return false, fmt.Errorf("containerd config default does not parse: %w", err)
	} else if !slices.Contains(imports, confdImportGlob) {
		return false, fmt.Errorf("containerd config default does not import %s; refusing to write a config that would silently drop stratum's registry mirrors", confdImportGlob)
	}
	if confine && !enableSELinuxLine.MatchString(string(out)) {
		return false, errors.New("containerd config default has no enable_selinux setting stratum recognises; refusing to write a config that would leave containers unconfined on an SELinux host")
	}
	s := rewriteContainerdConfig(string(out), want, confine)
	if !importsDropIns(s) {
		return false, fmt.Errorf("the containerd config stratum rendered does not import %s after %s; refusing to write a config that would silently drop this node's runtime handlers", handlersImportGlob(), confdImportGlob)
	}
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		return false, err
	}
	// A plain write torn by a crash would leave containerd with no config to start from.
	if err := atomicfile.WriteFileWith(cfg, []byte(s), 0o644, atomicfileSeams()); err != nil {
		// ErrNotDurable is post-rename: the new config is already in place.
		return errors.Is(err, atomicfile.ErrNotDurable), err
	}
	began := written
	if prevErr != nil || !strings.Contains(string(prev), enableSELinuxAssign(true)) {
		began = time.Now()
		if err := recordConfinement(false, time.Time{}); err != nil {
			return true, err
		}
	}
	if err := recordConfinement(confine, began); err != nil {
		return true, err
	}
	if confine && prevErr == nil && !strings.Contains(string(prev), enableSELinuxAssign(true)) {
		fmt.Fprintln(w, "  SELinux: containerd now confines containers; pods already running keep their labels until they are recreated")
	}
	// No restart here: containerd reads the new config on the start its caller performs.
	return true, nil
}

// rootAssign / stateAssign are the single source shared by producer and matcher.
func rootAssign() string  { return "root = '" + version.ContainerdRootDir + "'" }
func stateAssign() string { return "state = '" + version.ContainerdStateDir + "'" }

// rewriteContainerdConfig: older containerd builds double-quote, so matching tries both styles.
func rewriteContainerdConfig(defaultTOML, cgroup string, confine bool) string {
	s := strings.ReplaceAll(defaultTOML, "SystemdCgroup = false", cgroup)
	s = enableSELinuxLine.ReplaceAllString(s, "${1}"+enableSELinuxAssign(confine))
	// Without this the node pulls pause from registry.k8s.io on the first pod.
	s = sandboxImageLine.ReplaceAllString(s, "${1}'"+SandboxImage+"'")
	// Under /run/stratum so nothing collides with a distro containerd on the host.
	s = strings.ReplaceAll(s, "/run/containerd/containerd.sock", strings.TrimPrefix(version.ContainerdSock, "unix://"))
	// A real filesystem is required for the overlayfs snapshotter; overlay-on-overlay does not mount.
	for _, q := range []string{`'`, `"`} {
		s = strings.ReplaceAll(s, `root = `+q+`/var/lib/containerd`+q, rootAssign())
		s = strings.ReplaceAll(s, `state = `+q+`/run/containerd`+q, stateAssign())
	}
	return addHandlersImport(s)
}

// legacyHostConfig is a var, not a const, so a test can redirect it.
var legacyHostConfig = "/etc/containerd/config.toml"

// isStratumConfig parses the TOML; a substring match could satisfy a host's own config.
func isStratumConfig(b []byte) bool {
	var cfg struct {
		Root  string `toml:"root"`
		State string `toml:"state"`
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return false
	}
	return cfg.Root == version.ContainerdRootDir && cfg.State == version.ContainerdStateDir
}

// RemoveLegacyHostConfig logs success too: deleting a host file is the heal's biggest mutation.
func RemoveLegacyHostConfig(w io.Writer) {
	removed, err := removeLegacyHostConfig(legacyHostConfig)
	switch {
	case err != nil:
		fmt.Fprintf(w, "  WARN: could not remove legacy containerd config %s: %v\n", legacyHostConfig, err)
	case removed:
		fmt.Fprintf(w, "  removed %s — a previous stratum wrote it over the host's config; host containerd/Docker can start again\n", legacyHostConfig)
	}
}

// LegacyHostConfig lets uninstall's dry-run disclose the deletion instead of springing it at --yes.
func LegacyHostConfig() (path string, present bool) {
	if legacyHostConfig == version.ContainerdConfig {
		return legacyHostConfig, false
	}
	b, err := os.ReadFile(legacyHostConfig)
	if err != nil {
		return legacyHostConfig, false
	}
	return legacyHostConfig, isStratumConfig(b)
}

// removeLegacyHostConfig is pure of logging so tests can drive it quietly.
func removeLegacyHostConfig(path string) (removed bool, err error) {
	// Only in a test that redirected it is the host path stratum's own config path.
	if path == version.ContainerdConfig {
		return false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !isStratumConfig(b) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

// CgroupDriver reports the driver both containerd and the kubelet must use.
func CgroupDriver() string {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return "systemd"
	}
	return "cgroupfs"
}

// EnsureConfig is the prepare-phase entry point; ApplyRegistry is the live path.
func EnsureConfig() (bool, error) {
	// Even a failed render or write must not strand the host on a known-bad legacy file.
	defer RemoveLegacyHostConfig(os.Stdout)
	return WriteConfig()
}

// WriteConfig skips the heal: a predating supervisor keeps the legacy --config argv.
func WriteConfig() (bool, error) { return WriteConfigWithWriter(os.Stdout) }

func WriteConfigWithWriter(w io.Writer) (bool, error) {
	if !installed() {
		return false, install()
	}
	changed, err := ensureConfigWithWriter(w)
	if err != nil {
		return changed, err
	}
	res, err := registry.Apply()
	if err != nil {
		return changed, fmt.Errorf("apply registry mirrors: %w", err)
	}
	for _, warning := range res.Warnings {
		fmt.Fprintf(w, "  WARN: registry: %s\n", warning)
	}
	return changed, nil
}

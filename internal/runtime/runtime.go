// Package runtime configures containerd from the staged payload: config.toml
// with the host-matched cgroup driver, the stratum CRI socket, and registry
// mirrors. There is no package-manager install path: containerd ships in the
// payload.
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

// ApplyRegistryResult is ApplyRegistry's runtime-level outcome: the registry
// package's apply result plus Restarted, true only when this call actually
// restarted containerd. registry.Result's Changed alone means only that the
// conf.d drop-in changed — reporting a restart from it claims one that did not
// happen wherever containerd is not running.
type ApplyRegistryResult struct {
	registry.Result
	Restarted bool // the drop-in changed AND containerd was running and came back
}

// ApplyRegistry re-applies /etc/stratum/registries.yaml to containerd's certs.d and
// restarts containerd when the config_path enablement changed (hosts.toml alone
// are read live) and containerd is running to restart. Backs `stratum registry
// apply` — lets an operator update mirrors without reinstalling. Restarted, not
// Changed, is the restart fact a caller reports from.
func ApplyRegistry() (ApplyRegistryResult, error) {
	res, err := registry.Apply()
	out := ApplyRegistryResult{Result: res}
	if err != nil {
		return out, err
	}
	if out.Changed && Active() {
		if err := comps.Restart("containerd"); err != nil {
			return out, fmt.Errorf("restart containerd after registry config change: %w", err)
		}
		// comps.Restart has already waited for the replacement PID, so the
		// restart happened; the waitActive check below reports it coming back.
		out.Restarted = true
		if err := waitActive(10 * time.Second); err != nil {
			return out, err
		}
	}
	return out, nil
}

// waitActive polls until containerd reports active (everything downstream —
// kubelet, the CRI socket — depends on it, so a silent start failure here used
// to surface much later as an unrelated-looking "node did not register").
func waitActive(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for !Active() {
		if time.Now().After(deadline) {
			return fmt.Errorf("containerd did not become active within %s (journalctl -u containerd)", timeout)
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

// SandboxImage is the pause image containerd starts every pod sandbox from.
// containerd's built-in default points at registry.k8s.io, which an airgapped
// node cannot reach and which nothing at runtime is allowed to reference; this
// is the mirrored pin the payload actually carries (tools/mirror-images.sh).
// Stamped by the Makefile from build/images.txt, so it cannot drift from the
// image set `ctr images import` registers.
var SandboxImage = "ghcr.io/ctrliq/stratum/pause:unknown"

// sandboxImageLine matches containerd's own rendering of the setting across
// versions: 1.x double-quotes `sandbox_image` under the CRI grpc plugin, 2.0-2.2
// single-quote `sandbox_image` under io.containerd.cri.v1.images, and 2.3
// (config version 4) renders it as `sandbox` under that plugin's pinned_images
// table. Missing the 2.3 spelling left every node pulling pause from
// registry.k8s.io, which never starts a pod on an airgapped host.
// `sandboxer = 'podsandbox'` in the runtime table must NOT match: the key has
// to end at the `=`.
var sandboxImageLine = regexp.MustCompile(`(?m)^(\s*(?:sandbox_image|sandbox)\s*=\s*)['"][^'"]*['"]`)

// selinuxLabelled is selinux.Labelled; a var so a test can stand in for a
// labelled node.
var selinuxLabelled = selinux.Labelled

// enableSELinuxLine matches containerd's enable_selinux setting, in the CRI
// runtime plugin's table, keeping its indentation.
var enableSELinuxLine = regexp.MustCompile(`(?m)^(\s*)enable_selinux\s*=\s*(?:true|false)`)

// DisableSELinux turns enable_selinux off in the containerd config Stratum
// wrote, reporting whether it changed the file. containerd reads it on its
// next start. A rollback to a build whose control-plane manifests predate
// spc_t needs it: with it on, those pods run as container_t and are denied the
// node's PKI and etcd data, and the older build never turns it off.
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
	// Through the atomic writer, not a fixed .tmp name beside the config: a
	// fixed name is one two writers could tear, and a plain write a crash
	// could truncate.
	if err := atomicfile.WriteFileWith(cfg, []byte(out), 0o644, atomicfileSeams()); err != nil {
		// ErrNotDurable is a post-rename failure: the new config is already
		// in place, so the change report says so even while the durability
		// error goes out.
		return errors.Is(err, atomicfile.ErrNotDurable), err
	}
	return true, recordConfinement(false, time.Time{})
}

// atomicfileSeams returns this package's write seams, for handing to the
// atomic single-file writer. Production leaves syncDir as the writer's own;
// a test fills it to inject a failure into the real writer — a directory sync
// that never completes, the window a power loss leaves behind.
var syncDir = atomicfile.SyncDir

func atomicfileSeams() atomicfile.Seams {
	return atomicfile.Seams{SyncDir: syncDir}
}

// ConfinesContainers reports whether the containerd config Stratum wrote turns
// on SELinux labelling: enable_selinux in the CRI runtime plugin's table, read
// as containerd reads it, so a comment or the key in another table does not
// count. A missing config confines nothing; one that cannot be read or parsed
// is an error.
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

// confinedPath records when containerd began confining containers, as RFC
// 3339: the time enable_selinux was turned on in its config.
func confinedPath() string {
	return filepath.Join(filepath.Dir(version.ContainerdConfig), "selinux-confined-since")
}

// ConfinedSincePath is the file recording when containerd began confining containers.
func ConfinedSincePath() string { return confinedPath() }

// ConfinedSince reports when containerd began confining containers; false
// when it does not confine them, or the record is missing or unreadable. A
// pod sandbox started before then runs without SELinux labels until the pod
// is recreated.
func ConfinedSince() (time.Time, bool) {
	b, err := os.ReadFile(confinedPath())
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return t, err == nil
}

// recordConfinement keeps the confinement record in step with the config:
// removed when containerd does not confine containers, and otherwise written
// with began when there is none.
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

// enableSELinuxAssign is the enable_selinux line stratum writes. It is on when
// the node is labelled (selinux.Labelled), so containers run as container_t
// with their own categories and pod seLinuxOptions apply; on a node that
// opted out or lacks the policy it is off.
func enableSELinuxAssign(on bool) string {
	if on {
		return "enable_selinux = true"
	}
	return "enable_selinux = false"
}

// ensureConfig writes a default containerd config, with the cgroup driver
// matched to the host: SystemdCgroup=true when systemd is PID 1, cgroupfs
// otherwise (the container-based integration harness has no systemd). The
// kubelet config renders from the same CgroupDriver() so they cannot disagree
// — the driver-mismatch pod churn is the bug this guards.
//
// It reports whether it changed the file and restarts nothing: rendering the
// config is a file write, not a process operation, and the caller owns the
// restart — a render that restarts containerd behind its caller's back cannot
// be composed, and an upgrade that renders and then reloads the host would
// restart containerd twice. changed is disk-truthful: true whenever the file
// was rewritten, beside any error that happens after the write.
func ensureConfig() (bool, error) { return ensureConfigWithWriter(os.Stdout) }

func ensureConfigWithWriter(w io.Writer) (bool, error) {
	cfg := version.ContainerdConfig
	want := "SystemdCgroup = false"
	if CgroupDriver() == "systemd" {
		want = "SystemdCgroup = true"
	}
	confine := selinuxLabelled()
	// Both settings, not just the cgroup driver: an existing config from an
	// older SKE has the right driver and the wrong sandbox image, and an early
	// return on the driver alone would leave the node pulling from
	// registry.k8s.io forever.
	prev, prevErr := os.ReadFile(cfg)
	var written time.Time
	if fi, err := os.Stat(cfg); err == nil {
		written = fi.ModTime()
	}
	if prevErr == nil &&
		strings.Contains(string(prev), want) && strings.Contains(string(prev), SandboxImage) &&
		strings.Contains(string(prev), enableSELinuxAssign(confine)) {
		if err := recordConfinement(confine, written); err != nil {
			return false, err
		}
		return false, nil
	}
	out, err := exec.Command(filepath.Join(version.BinDir, "containerd"), "config", "default").Output()
	if err != nil {
		return false, err
	}
	// Fail closed: a default config with no sandbox setting to rewrite means
	// containerd renamed the key again, and writing it anyway would leave the
	// node pulling pause from registry.k8s.io -- quietly on a connected host,
	// never starting a pod on an airgapped one. That is how a config pulling
	// pause from the internet once shipped.
	if !sandboxImageLine.MatchString(string(out)) {
		return false, fmt.Errorf("containerd config default has no sandbox image setting stratum recognises; refusing to write a config that would pull pause from the internet instead of %s", SandboxImage)
	}
	if confine && !enableSELinuxLine.MatchString(string(out)) {
		return false, errors.New("containerd config default has no enable_selinux setting stratum recognises; refusing to write a config that would leave containers unconfined on an SELinux host")
	}
	s := rewriteContainerdConfig(string(out), want, confine)
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		return false, err
	}
	// The config containerd reads at every start goes through the shared
	// atomic writer: a plain write torn by a crash would leave containerd
	// with no config to start from.
	if err := atomicfile.WriteFileWith(cfg, []byte(s), 0o644, atomicfileSeams()); err != nil {
		// ErrNotDurable is a post-rename failure: the new config is already
		// in place, so the change report says so even while the durability
		// error goes out.
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
	// No restart here: containerd reads the new config on the start its caller
	// performs — the supervisor's first launch after `stratum up` or a join,
	// the reload an in-place upgrade runs itself, the restart
	// runtime.ApplyRegistry makes after a registry change.
	return true, nil
}

// rootAssign / stateAssign render the exact TOML assignments stratum puts in
// its containerd config. They are the SINGLE source shared by the producer
// (rewriteContainerdConfig) and the matcher (isStratumConfig): the legacy-config heal
// deletes a legacy /etc/containerd/config.toml only when it carries both, so if
// these two ever disagreed the heal would silently stop reaping stratum's own
// files. Keeping one definition makes that drift impossible.
func rootAssign() string  { return "root = '" + version.ContainerdRootDir + "'" }
func stateAssign() string { return "state = '" + version.ContainerdStateDir + "'" }

// rewriteContainerdConfig turns `containerd config default` output into
// stratum's config: host-matched cgroup driver, the payload's pause image, the
// stratum CRI socket, and root/state on the stratum layout. Pure (no I/O) so a
// test can feed a captured default through it and assert isStratumConfig accepts
// the result — closing the producer↔matcher gap a hand-built fixture cannot.
// containerd 2.x emits single-quoted TOML; older builds double-quote, so the
// match side tries both quote styles while the written side is always single.
func rewriteContainerdConfig(defaultTOML, cgroup string, confine bool) string {
	s := strings.ReplaceAll(defaultTOML, "SystemdCgroup = false", cgroup)
	s = enableSELinuxLine.ReplaceAllString(s, "${1}"+enableSELinuxAssign(confine))
	// The sandbox image comes from the payload, by the mirrored name the payload
	// registers. Without this the node pulls pause from registry.k8s.io on the
	// first pod — which fails outright on an airgapped cluster and reintroduces
	// an upstream-registry dependency on every other one.
	s = sandboxImageLine.ReplaceAllString(s, "${1}'"+SandboxImage+"'")
	// The CRI socket lives under /run/stratum so nothing collides with a distro
	// containerd already on the host.
	s = strings.ReplaceAll(s, "/run/containerd/containerd.sock", strings.TrimPrefix(version.ContainerdSock, "unix://"))
	// State rides the stratum data dir: one directory to back up and uninstall,
	// and — required, not cosmetic — a real filesystem for the overlayfs
	// snapshotter (overlay-on-overlay does not mount, which is exactly what
	// happens when the default /var/lib/containerd sits on a container's own
	// overlay root in the integration harness).
	for _, q := range []string{`'`, `"`} {
		s = strings.ReplaceAll(s, `root = `+q+`/var/lib/containerd`+q, rootAssign())
		s = strings.ReplaceAll(s, `state = `+q+`/run/containerd`+q, stateAssign())
	}
	return s
}

// legacyHostConfig is where earlier stratum builds wrote their containerd
// config: straight over the host's own /etc/containerd/config.toml. Stratum
// keeps its config at version.ContainerdConfig, so a host that ran an older
// stratum can be left with a version=4 file the distro's own containerd (<=v2.2)
// cannot parse — crash-looping containerd and Docker after a reboot, with
// nothing to undo it. A var, not a const, so the test can redirect it.
var legacyHostConfig = "/etc/containerd/config.toml"

// isStratumConfig reports whether b is a containerd config stratum wrote. It
// PARSES the TOML and checks the effective top-level root and state keys equal
// the stratum data dirs — not a substring match, which a host's own config
// could satisfy with those paths sitting in a comment or a nested table while
// its real root/state are host-owned. Parsing is the actual ownership proof
// that makes deleting the legacy file safe: we only ever remove a file whose
// live root AND state point into the stratum layout. Unparseable input is
// treated as not-ours (fail closed — never delete what we cannot verify).
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

// RemoveLegacyHostConfig deletes /etc/containerd/config.toml when an older
// stratum build wrote it there, healing a host whose distro containerd (and
// Docker) would otherwise fail to start after a reboot. It never
// touches a config it did not write, and is a no-op when the file is absent.
//
// It logs the outcome to w: deleting a file outside stratum's own data dir is
// the most consequential host mutation in the heal, so a success is surfaced (not
// only a failure) — otherwise an operator reading the logs of an ordinary `up`
// or uninstall could not tell a host file had been removed. Owns its logging so
// both callers (up and uninstall) report it identically and neither retypes the
// path; `up` passes os.Stdout, the uninstall run its progress writer.
func RemoveLegacyHostConfig(w io.Writer) {
	removed, err := removeLegacyHostConfig(legacyHostConfig)
	switch {
	case err != nil:
		fmt.Fprintf(w, "  WARN: could not remove legacy containerd config %s: %v\n", legacyHostConfig, err)
	case removed:
		fmt.Fprintf(w, "  removed %s — a previous stratum wrote it over the host's config; host containerd/Docker can start again\n", legacyHostConfig)
	}
}

// LegacyHostConfig returns the legacy host config path and whether a
// stratum-written config is present there right now — i.e. whether a call to
// RemoveLegacyHostConfig would delete it. It lets uninstall's dry-run disclose
// the host-level deletion instead of springing it on the operator at --yes.
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

// removeLegacyHostConfig reaps path when it is a stratum-written config, and
// reports whether it actually deleted anything (so the caller can log the one
// consequential host mutation). Pure of logging so tests can drive it quietly.
func removeLegacyHostConfig(path string) (removed bool, err error) {
	// Stratum writes its own config elsewhere; if that IS the host path
	// (only in a test that redirected it), there is no legacy file to reap.
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
		return false, nil // a host's own config — leave it alone
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

// CgroupDriver reports the driver both containerd and the kubelet must use:
// "systemd" when systemd is PID 1, "cgroupfs" otherwise.
func CgroupDriver() string {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return "systemd"
	}
	return "cgroupfs"
}

// EnsureConfig writes the containerd config and registry mirrors and reports
// whether the render changed containerd's config. It touches no running
// process — the render restarts nothing, and the caller owns the restart:
// `stratum registry apply` restarts through ApplyRegistry, the in-place
// upgrade folds the change into its own single host reload, and a node start
// (`stratum up`, a join) has no containerd running yet, so its supervisor
// brings containerd up on the config just written. This is the prepare-phase
// entry point, before the supervisor owns containerd; ApplyRegistry is the
// live-node path.
func EnsureConfig() (bool, error) {
	// Heal a host an older stratum broke: our config lives under DataDir, so
	// a version=4 file left at /etc/containerd/config.toml would only keep the
	// distro's containerd (and Docker) crash-looping after a reboot.
	// This is independent of installing or rendering the new config: even a
	// missing payload, failed `containerd config default`, failed write, or bad
	// registry configuration must not strand the host on a known-bad legacy
	// file. Best-effort and self-logging; a node that never ran the old build
	// has nothing to remove. The heal reports to os.Stdout: every caller here
	// is a CLI or node-boot path whose output stream is the process's stdout.
	defer RemoveLegacyHostConfig(os.Stdout)
	return WriteConfig()
}

// WriteConfig is EnsureConfig without the legacy-config heal, for a node whose
// running supervisor may still launch containerd with --config at the legacy
// path: a supervisor that predates reload keeps the argv it was started with
// across an in-place upgrade, so deleting that file there left containerd
// unable to start again until the next `stratum up`. The caller
// removes the legacy file itself, once nothing still passes it.
//
// It reports whether the render changed containerd's config and restarts
// nothing, like ensureConfig above. changed stays true beside an error raised
// after the config was written (a failed registry apply), because the file
// change on disk is the fact a restart decision needs.
func WriteConfig() (bool, error) { return WriteConfigWithWriter(os.Stdout) }

// WriteConfigWithWriter writes containerd config and reports progress to w.
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

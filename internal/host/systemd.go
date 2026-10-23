package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/toolenv"
	"github.com/ctrliq/stratum/internal/version"
)

// systemdMarkerPath is systemd's own standard tell (systemd-detect-virt and
// friends use it too). A var, not a const, so tests can point it at a temp
// dir instead of asserting on the test host's real init system.
var systemdMarkerPath = "/run/systemd/system"

// toolEnv is the environment of the tools this package runs, which run as
// root and so get none of the caller's. A var so a test can set nil, which
// exec reads as the caller's, for a stub that takes its script from it.
var toolEnv = toolenv.Env()

// toolPath is where those tools are looked for, not the caller's PATH, which
// can put a shim of any of them first. A var so a test can put a stub there.
var toolPath = toolenv.Path

// hostCmd is toolenv.Cmd with toolEnv.
func hostCmd(name string, args ...string) *exec.Cmd {
	cmd := toolenv.Cmd(toolPath, name, args...)
	cmd.Env = toolEnv
	return cmd
}

// SystemdAvailable reports whether this host's PID 1 is systemd — the only
// init system the reboot-persistence unit supports. Non-systemd hosts are out
// of scope: InstallUnit/DisableUnit/StopUnit are no-ops there rather than errors.
func SystemdAvailable() bool {
	_, err := os.Stat(systemdMarkerPath)
	return err == nil
}

// InstallUnit renders and enables the systemd unit that restarts argv — the
// exact steady-state command line this node was brought up with (never a
// one-shot join/token flag; see runUp's ExecStart builder) — after a reboot
// or crash.
//
// The in-process supervisor (internal/supervisor) still parents containerd,
// kubelet and the operator; kubelet in turn reconciles the control-plane
// static pods. None of that is systemd's job — this unit wraps
// only the single top-level `stratum up` process, the one layer nothing else
// restarts after the node itself reboots. A separate unit per
// containerd/kubelet/operator would supervise those three twice over, once
// from systemd and once from the supervisor already running inside `stratum
// up`.
func InstallUnit(argv []string) error {
	if !SystemdAvailable() {
		return nil
	}
	// A `systemctl mask` is an operator's own deliberate choice to keep this
	// unit from ever starting, persisted as a symlink to /dev/null at
	// version.SystemdUnitPath — checked here, before anything else, and
	// before removeStaleRestartOverride's own reload runs, because writing a
	// fresh unit over that symlink (what the rename in WriteFileAtomically
	// would otherwise do) silently undoes the mask and re-enables the unit.
	// No write, reload, or enable happens on this path;
	// the caller (runUp) turns this error into a warning, same as any other
	// InstallUnit failure.
	if masked, err := unitMasked(); err != nil {
		return fmt.Errorf("determine whether %s is masked: %w", unitName(), err)
	} else if masked {
		return fmt.Errorf("%s is masked, not installing a fresh unit over it (unmask it first if this node should come back after a reboot)", unitName())
	}
	// A leftover Restart=no override (uninstall's SuppressRestart) must never
	// survive into a freshly (re-)installed unit: if a prior uninstall's own
	// AllowRestart failed to clean it up (that failure is
	// a warning, not an abort, so uninstall can finish with the drop-in
	// still there), a same-boot `stratum up` before the next real reboot
	// clears /run on its own would otherwise silently install with
	// Restart=always but run with the old override's Restart=no still
	// in force.
	if err := removeStaleRestartOverride(); err != nil {
		return err
	}
	unit, err := renderUnit(argv)
	if err != nil {
		return err
	}
	cur, err := os.ReadFile(version.SystemdUnitPath)
	// Enablement is checked with systemctl, not inferred from the file
	// matching: a content match only proves an earlier `stratum up` WROTE this
	// file, not that its `systemctl enable` succeeded. Trusting the bytes alone
	// meant a first enable that failed (permissions, a wedged dbus) was never
	// retried — the file already matched, so every later `stratum up` took the
	// early return and the node stayed unarmed for its next reboot.
	contentMatches := err == nil && string(cur) == unit
	enabled, enabledErr := unitEnabled()
	if contentMatches {
		// The file is already exactly what this call would write, so a query
		// failure here is not a reason to rewrite and re-enable it — that
		// would be a reinstall for a question that has nothing to do with the
		// unit's content. Return the error and let the caller (runUp) warn:
		// the next `stratum up` gets a fresh, real answer instead of paying
		// for a redundant reinstall on every failed query.
		if enabledErr != nil {
			return enabledErr
		}
		if enabled {
			return nil
		}
	}
	// Reached with stale content, or content that matches but is not
	// enabled (including enabledErr's own "disabled" answer): the real
	// install+enable path below is what surfaces a genuine enable failure.
	if err := os.MkdirAll(filepath.Dir(version.SystemdUnitPath), 0o755); err != nil {
		return err
	}
	if err := WriteFileAtomically(version.SystemdUnitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	if out, err := hostCmd("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, out)
	}
	if out, err := hostCmd("systemctl", "enable", unitName()).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable %s: %w: %s", unitName(), err, out)
	}
	return nil
}

// unitMasked reports whether the reboot-persistence unit is masked — an
// operator's own `systemctl mask`, which InstallUnit must never write over.
// Checked two ways: Lstat's own read of version.SystemdUnitPath, without
// following the symlink a persistent mask replaces it with (following it
// would just see an empty read from /dev/null and miss the mask entirely),
// and systemctl's own UnitFileState, which also catches a mask this host's
// filesystem cannot show — a runtime-only mask lives under /run, not at
// version.SystemdUnitPath.
func unitMasked() (bool, error) {
	if fi, err := os.Lstat(version.SystemdUnitPath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(version.SystemdUnitPath)
			if err != nil {
				return false, err
			}
			if target == os.DevNull {
				return true, nil
			}
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if !SystemdAvailable() {
		return false, nil
	}
	out, err := hostCmd("systemctl", "show", "-p", "UnitFileState", "--value", unitName()).Output()
	if err != nil {
		return false, fmt.Errorf("systemctl show %s: %w", unitName(), err)
	}
	// masked-runtime counts here too: a /run-only mask still means "do not
	// install over this," even though unitEnabled treats it as an error
	// rather than folding it into "disabled" — the two functions answer
	// different questions (may InstallUnit write here at all, vs. is the
	// persisted unit file enabled) and masked-runtime warrants a human's
	// attention for the second one but not a write-protection exemption from
	// the first.
	switch state := strings.TrimSpace(string(out)); state {
	case "masked", "masked-runtime":
		return true, nil
	default:
		return false, nil
	}
}

// unitEnabled reports whether the unit is currently enabled, reading
// `systemctl show -p UnitFileState --value` the same way unitPresent reads
// LoadState/ActiveState, rather than inferring the answer
// from `is-enabled`'s exit code: that exit code does not distinguish a real
// "not enabled" from systemctl never having run at all, since a shell
// invocation failure (missing binary, a wedged dbus) can surface as the same
// nonzero status as a clean negative answer.
//
// `enabled` is enabled. `disabled`, an empty value (no file to have a
// state), `not-found` (never installed), and `masked` (an operator's own
// `systemctl mask`, persisted to /etc) all count as not boot-enabled — none
// of them stop InstallUnit's caller from retrying enablement, which is the
// whole point of not trusting a file-content match alone; a mask blocks that
// retry's `systemctl enable` on its own; `unitEnabled` does not need to
// duplicate that refusal as an error. `masked-runtime` (a /run-only mask,
// unusual for this unit) and anything else — `static`, `alias`, `generated`,
// `transient`, `bad`, or the query itself failing — is an error: those need
// a human's attention, not a silent "go ahead and try to enable it anyway".
func unitEnabled() (bool, error) {
	if !SystemdAvailable() {
		return false, nil
	}
	out, err := hostCmd("systemctl", "show", "-p", "UnitFileState", "--value", unitName()).Output()
	if err != nil {
		return false, fmt.Errorf("systemctl show %s: %w", unitName(), err)
	}
	switch state := strings.TrimSpace(string(out)); state {
	case "enabled":
		return true, nil
	case "disabled", "", "not-found", "masked":
		return false, nil
	default:
		return false, fmt.Errorf("systemctl show %s: unexpected UnitFileState %q", unitName(), state)
	}
}

// UnitEnabled reports whether the reboot-persistence unit is currently
// enabled to start at boot — the same check InstallUnit uses to decide
// whether an earlier enable actually succeeded. `stratum uninstall` calls
// this before DisableUnit, so a teardown step that fails afterward can
// restore exactly the enablement it found (never force one) — see
// EnableUnit.
func UnitEnabled() (bool, error) { return unitEnabled() }

// DisableUnit disables the reboot-persistence unit from starting at the NEXT
// boot — the first step of `stratum uninstall`, before it touches any
// running process or on-disk state. Boot enablement is not the same thing as
// the ACTIVE unit's restart behavior: disabling alone does nothing to stop
// Restart=always from relaunching the unit right now if it happens to exit
// for some unrelated reason while uninstall is still tearing the node down —
// SuppressRestart is what covers that. What DisableUnit alone protects
// against is a reboot that interrupts uninstall (or a failed rollback):
// without it, that reboot would bring the whole node back up over whatever
// state uninstall had already deleted.
//
// A missing unit (never installed, or a non-systemd host) is not an error:
// there is nothing to disable. A real systemctl failure is returned so
// uninstall does not proceed to stop and delete state while the unit may
// still be armed to restart it.
func DisableUnit() error { return systemctlOnUnit("disable") }

// EnableUnit (re-)enables the reboot-persistence unit for the next boot,
// without touching its ExecStart, restarting it, or requiring
// SystemdAvailable() to have been true when the unit was first written —
// only that it is now. Used by `stratum uninstall`'s rollback to restore
// enablement when DisableUnit succeeded but a later teardown step failed, so
// a failed uninstall never leaves a still-running node stripped of the
// reboot persistence it had before.
//
// A missing unit is not an error, matching DisableUnit/StopUnit: rollback
// runs unconditionally, and a node that was never enabled (or never
// systemd) has nothing to restore.
func EnableUnit() error {
	if !SystemdAvailable() {
		return nil
	}
	present, err := unitPresent()
	if err != nil {
		return fmt.Errorf("determine whether %s is present: %w", unitName(), err)
	}
	if !present {
		return nil
	}
	if out, err := hostCmd("systemctl", "enable", unitName()).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable %s: %w: %s", unitName(), err, out)
	}
	return nil
}

// StopUnit stops the reboot-persistence unit. `stratum uninstall` calls this
// only after the static pods are torn down and every stratum host process is
// confirmed stopped, right before it deletes on-disk state (which includes the
// unit file itself) — stopping the unit any earlier sends the top-level
// `stratum up` process a SIGTERM that cascades into stopping kubelet through
// its own shutdown path, before kubelet has had the chance to reap the static
// pods removal asks it to.
func StopUnit() error { return systemctlOnUnit("stop") }

// systemdDropInPath is a RUNTIME-only override — under /run, never /etc — that
// SuppressRestart/AllowRestart toggle around uninstall's teardown window. A
// var, not a const, so tests can point it at a temp dir.
var systemdDropInPath = "/run/systemd/system/stratum.service.d/uninstall-no-restart.conf"

// SuppressRestart drops in a runtime override that disarms the ACTIVE unit's
// Restart= without touching the persisted unit file under /etc or its boot
// enablement (DisableUnit). `systemctl disable` only stops the unit from
// starting at the NEXT boot — the unit stays active, with Restart=always
// still in force, until something changes ITS live configuration. Without
// this, an unrelated exit of `stratum up` during uninstall's teardown (a
// panic, a signal, anything short of StopUnit) would have systemd relaunch
// it in the middle of removing static pods and stopping host processes,
// racing the very teardown uninstall is mid-way through.
//
// The override lives under /run, so a real reboot clears it on its own with
// no cleanup needed; AllowRestart removes it explicitly for a node that keeps
// running — an aborted uninstall, or once StopUnit is about to take the unit
// down for good and the override no longer means anything.
//
// A missing unit is not an error, matching DisableUnit/StopUnit/EnableUnit:
// there is nothing active to protect from restarting, so writing a drop-in
// for a unit that does not exist would only leave an orphaned directory
// under /run — reachable on a real systemd host where `stratum up` never
// installed a unit (never systemd-managed, or uninstalled already).
func SuppressRestart() error {
	if !SystemdAvailable() {
		return nil
	}
	present, err := unitPresent()
	if err != nil {
		return fmt.Errorf("determine whether %s is present: %w", unitName(), err)
	}
	if !present {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(systemdDropInPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(systemdDropInPath, []byte("[Service]\nRestart=no\n"), 0o644); err != nil {
		return err
	}
	if out, err := hostCmd("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, out)
	}
	return nil
}

// AllowRestart removes the runtime override SuppressRestart wrote, restoring
// Restart=always for the unit. A missing override file is not an error:
// rollback calls this unconditionally, whether or not SuppressRestart ever
// ran. But an absent file must not skip the reload: a prior AllowRestart call
// can have removed the file and then failed the reload that follows (systemctl
// unreachable, a wedged dbus) — the caller's retry finds no file to remove,
// and skipping the reload too would leave systemd's live config still
// carrying the old Restart=no it was never told to forget.
func AllowRestart() error {
	if !SystemdAvailable() {
		return nil
	}
	return removeStaleRestartOverride()
}

// removeStaleRestartOverride removes systemdDropInPath if it is still there,
// then reloads systemd UNCONDITIONALLY, whether or not there was a file to
// remove — matching AllowRestart, and for the same reason: a prior call
// (this one, or AllowRestart) can have removed the file and then failed its
// own reload, and skipping the reload on the retry because there is no
// longer a file to remove would leave systemd's live config still carrying
// the stale Restart=no override it was never told to forget. A reload on
// every `stratum up` costs little. See InstallUnit's call
// site for why a leftover override must never survive into a fresh install.
func removeStaleRestartOverride() error {
	if _, err := os.Stat(systemdDropInPath); err == nil {
		if err := os.Remove(systemdDropInPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if out, err := hostCmd("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, out)
	}
	return nil
}

// RestartUnit restarts the reboot-persistence unit. With KillMode=process
// systemd signals only the supervisor, which stops the components it runs and
// leaves the containers to their shims; systemd then starts ExecStart again.
func RestartUnit() error { return systemctlOnUnit("restart") }

// UnitMainPID is the pid systemd runs the reboot-persistence unit as, 0 when
// systemd is not running it: no systemd, or the unit is absent or inactive.
func UnitMainPID() (int, error) {
	if !SystemdAvailable() {
		return 0, nil
	}
	out, err := hostCmd("systemctl", "show", "-p", "MainPID", "--value", unitName()).Output()
	if err != nil {
		return 0, fmt.Errorf("systemctl show %s: %w", unitName(), err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("systemctl show %s: MainPID %q: %w", unitName(), strings.TrimSpace(string(out)), err)
	}
	return pid, nil
}

// UnitExecPath is the program the reboot-persistence unit's ExecStart runs:
// the binary a restart of the unit starts.
func UnitExecPath() (string, error) {
	out, err := hostCmd("systemctl", "show", "-p", "ExecStart", "--value", unitName()).Output()
	if err != nil {
		return "", fmt.Errorf("systemctl show %s: %w", unitName(), err)
	}
	return execStartPath(string(out))
}

// execStartPath takes the program from systemctl's rendering of an ExecStart,
// "{ path=/usr/local/bin/stratum ; argv[]=... ; ... }".
func execStartPath(show string) (string, error) {
	_, rest, ok := strings.Cut(show, "path=")
	if !ok {
		return "", fmt.Errorf("no program in ExecStart %q", strings.TrimSpace(show))
	}
	path, _, _ := strings.Cut(rest, " ;")
	if path = strings.TrimSpace(path); path == "" {
		return "", fmt.Errorf("no program in ExecStart %q", strings.TrimSpace(show))
	}
	return path, nil
}

// UnitArgv is the command line the reboot-persistence unit's ExecStart runs:
// the argv every restart of the unit starts. It is read from the unit file
// rather than from `systemctl show`, whose argv[] rendering joins the
// arguments with spaces unquoted, so a --config path with a space in it
// could not be told apart from two arguments. A unit that does not exist is
// an fs.ErrNotExist error, and so is a masked one (a symlink to /dev/null, as
// unitMasked reads it): systemd never starts it, and InstallUnit leaves the
// mask in place. A drop-in that sets ExecStart= is an error: systemd would run
// the drop-in's command, and this reads only the unit file stratum writes, so
// it does not guess which one wins. A drop-in that cannot be opened because it
// is gone (a dangling symlink, or one removed meanwhile) is skipped, as
// systemd skips it; only the unit file's own absence is fs.ErrNotExist.
func UnitArgv() ([]string, error) {
	if fi, err := os.Lstat(version.SystemdUnitPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(version.SystemdUnitPath); err == nil && target == os.DevNull {
			return nil, fmt.Errorf("%s is masked: %w", unitName(), fs.ErrNotExist)
		}
	}
	b, err := os.ReadFile(version.SystemdUnitPath)
	if err != nil {
		return nil, err
	}
	for _, dir := range unitDropInDirs() {
		confs, err := filepath.Glob(filepath.Join(dir, "*.conf"))
		if err != nil {
			return nil, err
		}
		for _, conf := range confs {
			d, err := os.ReadFile(conf)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if serviceSetsExecStart(string(d)) {
				return nil, fmt.Errorf("drop-in %s overrides the unit's ExecStart=", conf)
			}
		}
	}
	return execStartArgv(string(b))
}

// unitDropInDirs are the directories systemd reads the unit's drop-ins from:
// the unit's own under /etc, /run and the vendor trees, and the ones every
// service shares (systemd 246 and later). A var so tests read none of the
// host's.
var unitDropInDirs = func() []string {
	name := unitName()
	var dirs []string
	for _, base := range []string{filepath.Dir(version.SystemdUnitPath), filepath.Dir(filepath.Dir(systemdDropInPath)),
		"/usr/local/lib/systemd/system", "/usr/lib/systemd/system"} {
		dirs = append(dirs, filepath.Join(base, name+".d"), filepath.Join(base, "service.d"))
	}
	return dirs
}

// serviceExecStarts is every ExecStart= value in a unit's [Service] section,
// in order. systemd reads ExecStart= nowhere else.
func serviceExecStarts(unit string) []string {
	var vals []string
	inService := false
	for _, l := range strings.Split(unit, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			inService = l == "[Service]"
			continue
		}
		// systemd strips the whitespace around the =.
		if k, v, ok := strings.Cut(l, "="); ok && inService && strings.TrimSpace(k) == "ExecStart" {
			vals = append(vals, strings.TrimSpace(v))
		}
	}
	return vals
}

func serviceSetsExecStart(unit string) bool { return len(serviceExecStarts(unit)) > 0 }

// execStartArgv takes the argv from a unit file's ExecStart=. An empty
// assignment resets the list, as systemd's does; more than one command left
// after the last reset is an error, because systemd refuses to load such a
// unit unless it is Type=oneshot, which stratum's never is.
func execStartArgv(unit string) ([]string, error) {
	var cmds []string
	for _, v := range serviceExecStarts(unit) {
		if strings.TrimSpace(v) == "" {
			cmds = nil
			continue
		}
		cmds = append(cmds, v)
	}
	switch len(cmds) {
	case 0:
		return nil, fmt.Errorf("no ExecStart= command in %s", version.SystemdUnitPath)
	case 1:
	default:
		return nil, fmt.Errorf("%d ExecStart= commands in %s, which systemd refuses for a Type=simple unit", len(cmds), version.SystemdUnitPath)
	}
	argv, err := splitSystemdArgv(cmds[0])
	if err != nil {
		return nil, fmt.Errorf("ExecStart= %q: %w", cmds[0], err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("no ExecStart= command in %s", version.SystemdUnitPath)
	}
	return argv, nil
}

// splitSystemdArgv is quoteArgv's inverse: it splits an ExecStart= command
// line into argv under the same subset of systemd's command-line grammar
// escapeSystemdArg writes — whitespace-separated words, double-quoted spans
// with \" and \\, single-quoted spans, and %% and $$ for a literal % and $.
// Anything outside that subset (another backslash escape, a lone specifier or
// variable, an unterminated quote) is an error rather than a guess: the
// upgrade's missing-config guard refuses on it.
func splitSystemdArgv(line string) ([]string, error) {
	var argv []string
	var word strings.Builder
	inWord := false
	var quote rune
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		// The escapes come first: escapeSystemdArg doubles % and $ before it
		// quotes, so they appear inside a quoted span as well as outside one.
		switch {
		case r == '\\':
			if i+1 >= len(rs) || (rs[i+1] != '\\' && rs[i+1] != '"') {
				return nil, errors.New("unsupported backslash escape")
			}
			i++
			word.WriteRune(rs[i])
		case r == '%' || r == '$':
			if i+1 >= len(rs) || rs[i+1] != r {
				return nil, fmt.Errorf("unescaped %c, a specifier or variable this cannot expand", r)
			}
			i++
			word.WriteRune(r)
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == ' ' || r == '\t':
			if inWord {
				argv = append(argv, word.String())
				word.Reset()
				inWord = false
			}
			continue
		default:
			word.WriteRune(r)
		}
		inWord = true
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inWord {
		argv = append(argv, word.String())
	}
	return argv, nil
}

// systemctlOnUnit runs one systemctl verb against the reboot-persistence
// unit, treating an absent unit (or a non-systemd host) as nothing to do, and
// any other systemctl failure — including one from unitPresent's own query —
// as one uninstall must not paper over.
func systemctlOnUnit(verb string) error {
	if !SystemdAvailable() {
		return nil
	}
	present, err := unitPresent()
	if err != nil {
		return fmt.Errorf("determine whether %s is present: %w", unitName(), err)
	}
	if !present {
		return nil
	}
	if out, err := hostCmd("systemctl", verb, unitName()).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", verb, unitName(), err, out)
	}
	return nil
}

// unitPresent reports whether systemd itself still knows about the
// reboot-persistence unit — loaded, active, or both — straight from
// `systemctl show`, rather than from whether the unit file is still on disk.
// systemd only re-reads unit files on daemon-reload, so a file removed by
// hand (or by a teardown step that got this far before failing) does not by
// itself unload or stop a unit systemd already has in memory; a
// file-existence check alone would then treat a still-loaded, possibly
// ACTIVE unit as absent and skip disabling, stopping, or suppressing its
// restart.
//
// A query failure (systemctl unreachable, a wedged dbus) or output with no
// usable LoadState/ActiveState is returned as an error, not swallowed into
// "absent": every caller fails closed on it — attempting the real systemctl
// action (and surfacing ITS error) is safer than silently treating a unit
// we could not ask about as gone.
func unitPresent() (bool, error) {
	out, err := hostCmd("systemctl", "show", "-p", "LoadState,ActiveState", "--value", unitName()).Output()
	if err != nil {
		return false, fmt.Errorf("systemctl show %s: %w", unitName(), err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) == "" || strings.TrimSpace(lines[1]) == "" {
		return false, fmt.Errorf("systemctl show %s: no usable LoadState/ActiveState in output %q", unitName(), out)
	}
	for _, line := range lines {
		if v := strings.TrimSpace(line); v != "not-found" && v != "inactive" {
			return true, nil
		}
	}
	return false, nil
}

// ReloadSystemd re-reads unit files. `stratum uninstall` calls this once,
// after it has deleted the (now stopped, disabled) unit file on disk, folding
// a failure into its own firstErr: systemd otherwise keeps
// serving the just-deleted unit's last-loaded definition.
func ReloadSystemd() error {
	if !SystemdAvailable() {
		return nil
	}
	if out, err := hostCmd("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, out)
	}
	return nil
}

func unitName() string { return filepath.Base(version.SystemdUnitPath) }

// labelLike gives a file the SELinux label of another, routed to stderr: this
// low-level writer also serves protocol handlers with no operation progress
// stream, so labelling diagnostics must not corrupt protocol stdout. A var so
// a test can see which files are labelled.
var labelLike = func(tmp, dst string) error {
	return selinux.LabelLikeWithWriter(os.Stderr, tmp, dst)
}

// The durable single-file writer's own test seams. The writer lives in
// internal/atomicfile, and the wrappers below hand these to it on every call,
// so the tests that pin this package's write behavior keep working through the
// delegation.
var (
	// syncFile is (*os.File).Sync behind a seam, so a test can see what the
	// file looks like at the moment it is made durable.
	syncFile = func(f *os.File) error { return f.Sync() }
	// removeTempName is os.Remove behind a seam, so a test can fail it.
	removeTempName = os.Remove
	// syncDir fsyncs a directory behind a seam, so a test can see it is
	// called, and with which directory.
	syncDir = atomicfile.SyncDir
)

// atomicfileSeams returns this package's write seams, for handing to the
// atomicfile writer.
func atomicfileSeams() atomicfile.Seams {
	return atomicfile.Seams{
		SyncFile:       syncFile,
		LabelLike:      labelLike,
		RemoveTempName: removeTempName,
		SyncDir:        syncDir,
	}
}

// WriteFileAtomically writes data to path by creating a temp file in the SAME
// directory, fsyncing it, then renaming it into place. It writes the systemd
// unit, the firewalld record and the shared certs a control plane downloads;
// a CIS at-rest key minted on this node, which must never replace a file
// already at path, goes through CreateFileExclusively instead. The writer
// itself — unique hidden same-dir temp name, SELinux label, mode write before
// fsync, file fsync, rename, dir fsync, ErrNotDurable/ErrStrayCopy — lives in
// internal/atomicfile and is shared with every other package that writes a
// start-critical file; this wrapper only routes this package's test seams into
// it, so its behavior is exactly atomicfile.WriteFile's.
func WriteFileAtomically(path string, data []byte, perm os.FileMode) error {
	return atomicfile.WriteFileWith(path, data, perm, atomicfileSeams())
}

// ErrNotDurable is atomicfile.ErrNotDurable: the file was replaced, but the
// replacement could not be made durable. Existing callers match it by this
// name.
var ErrNotDurable = atomicfile.ErrNotDurable

// ErrStrayCopy is atomicfile.ErrStrayCopy: the temporary name could not be
// removed, and still holds the file's contents. Existing callers match it by
// this name.
var ErrStrayCopy = atomicfile.ErrStrayCopy

// CreateFileExclusively is WriteFileAtomically for a file that must never
// replace one already at path. The at-rest key is minted on an observation,
// made earlier, that no key exists; a rename would silently replace a key that
// appeared since -- one an operator put there, whose Secrets would then be
// unreadable. The create-only write itself lives in internal/atomicfile: it
// fails with an error matching fs.ErrExist when a file is already at path, and
// returns the created file's FileInfo once it exists -- even when the directory
// sync then fails -- so a caller that has to take the file back off can remove
// exactly the inode this call created, and nothing that replaced it. If the
// temp name outlives that link, the error matches ErrStrayCopy and names it.
func CreateFileExclusively(path string, data []byte, perm os.FileMode) (os.FileInfo, error) {
	return atomicfile.CreateExclusiveWith(path, data, perm, atomicfileSeams())
}

// SyncDir fsyncs dir, so an entry added to or removed from it survives a power
// loss.
func SyncDir(dir string) error { return syncDir(dir) }

// KillMode=process, deliberately, even though a crash (not a clean
// SIGTERM-and-exit) leaves `stratum up`'s children — containerd, kubelet, the
// operator, each its own process group (internal/supervisor) — running as
// orphans, which makes Restart=always's replacement refuse to start over
// them (see preflightNoRunningNode). `mixed` would close that gap by
// having systemd SIGKILL everything left in the unit's cgroup on every stop
// or restart — but Delegate=yes means that cgroup is the SAME one
// containerd's shims run in, even though they escape into their own session
// precisely so they survive containerd (and this unit) stopping; `mixed`
// would SIGKILL them too on every ordinary `stratum up` restart, not just a
// crash, breaking the documented "containers survive a supervisor stop"
// contract. `process` only signals the main PID, so
// systemd never touches the cgroup at all. Reaping a crash's orphaned
// containerd/kubelet/operator without that side effect needs a design this
// unit deliberately leaves alone — a live supervisor still refuses and asks
// for a human (a deliberate scope call: a reboot leaves no orphans, since
// everything dies with it; a crash without a reboot is a separate follow-up).
func renderUnit(argv []string) (string, error) {
	execStart, err := quoteArgv(argv)
	if err != nil {
		return "", err
	}
	// EnvironmentFile=-<path>, leading dash: this file is entirely optional.
	// Stratum never creates it — an operator who wants
	// environment-only S3 credentials (STRATUM_S3_*/AWS_*, for
	// scheduled etcd snapshot uploads) to survive a systemd-managed restart
	// writes it themselves, root-owned and mode 0600, since this unit's
	// environment is otherwise near-empty (see escapeSystemdArg below). A
	// missing file is silently fine, matching every `stratum up` before this
	// unit existed, which had no such file either.
	envFile := filepath.Join(version.ConfDir, "stratum.env")
	return fmt.Sprintf(`[Unit]
Description=Stratum Kubernetes Engine node agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=-%s
ExecStart=%s
Restart=always
RestartSec=5s
KillMode=process
Delegate=yes

[Install]
WantedBy=multi-user.target
`, envFile, execStart), nil
}

// quoteArgv renders argv for ExecStart=, escaping each argument for systemd's
// command-line grammar (systemd.service(5) "Command Lines"), which is not a
// shell's.
func quoteArgv(argv []string) (string, error) {
	parts := make([]string, len(argv))
	for i, a := range argv {
		esc, err := escapeSystemdArg(a)
		if err != nil {
			return "", err
		}
		parts[i] = esc
	}
	return strings.Join(parts, " "), nil
}

// escapeSystemdArg escapes one ExecStart= argument. Every argv element this
// package is ever handed is a binary path or a plain --flag/value, so the
// space/tab/quote check below is only reached for an operator-supplied
// --config path — but the specifier and variable escaping runs
// unconditionally, because `%`/`$` change meaning even with no space in
// sight, silently rather than with an error:
//
//   - backslash is systemd's own C-style escape lead-in (`\n`, `\t`, ...);
//     escaped first, so none of the escapes this function inserts below are
//     mistaken for user-supplied ones and re-escaped.
//   - `%` unescaped starts a specifier (`%h`, `%t`, ...); an operator's path
//     that happens to contain a literal `%` would have systemd substitute or
//     reject it, not pass it through.
//   - `$` unescaped starts a variable reference (`$FOO`/`${FOO}`); this
//     unit's environment is empty unless an operator's own
//     /etc/stratum/stratum.env (EnvironmentFile= above) sets something, so an
//     unescaped `$` most likely expands to nothing, truncating the argument
//     rather than erroring — the config path silently becomes a different,
//     shorter one. A populated stratum.env only raises the stakes: an
//     operator's own `$FOO` there could substitute unexpected text into a
//     path this function never meant to interpret at all.
//
// `'` triggers quoting too, same as a space, tab or `"`: unquoted, it starts
// a quote span of its own in systemd's command-line grammar. Once inside the
// double quotes this function wraps around, `'` has no special meaning, so
// it needs no escaping of its own the way `"` does.
//
// An empty argument is rendered as `""`: unquoted, it is
// nothing at all in systemd's command-line grammar — whitespace-separated,
// the same as a shell's word splitting — so it would silently vanish from
// argv instead of passing through as one empty element.
//
// A CR or LF returns an error instead of being encoded: a
// `--config` path is never going to legitimately contain one, and systemd's
// command-line grammar has no escape for a literal newline inside a
// double-quoted argument, only a line continuation — passing one through
// would corrupt ExecStart= into extra, unintended directive lines.
func escapeSystemdArg(a string) (string, error) {
	if strings.ContainsAny(a, "\r\n") {
		return "", fmt.Errorf("argument %q contains a CR or LF, which systemd's command-line grammar cannot express", a)
	}
	a = strings.NewReplacer(`\`, `\\`, `%`, `%%`, `$`, `$$`).Replace(a)
	if a == "" || strings.ContainsAny(a, " \t\"'") {
		a = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
	}
	return a, nil
}

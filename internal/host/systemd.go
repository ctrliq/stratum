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

// systemd's own standard marker; systemd-detect-virt uses it too.
var systemdMarkerPath = "/run/systemd/system"

// Root-run tools get an explicit environment, not the caller's.
var toolEnv = toolenv.Env()

// Not the caller's PATH, which can put a shim of any tool first.
var toolPath = toolenv.Path

func hostCmd(name string, args ...string) *exec.Cmd {
	cmd := toolenv.Cmd(toolPath, name, args...)
	cmd.Env = toolEnv
	return cmd
}

// Non-systemd hosts are out of scope: the unit operations below are no-ops there.
func SystemdAvailable() bool {
	_, err := os.Stat(systemdMarkerPath)
	return err == nil
}

// Renders the reboot unit for steady-state argv, never a join flag; supervises only stratum up.
func InstallUnit(argv []string) error {
	if !SystemdAvailable() {
		return nil
	}
	// Writing a fresh unit over a mask symlink silently re-enables the unit.
	if masked, err := unitMasked(); err != nil {
		return fmt.Errorf("determine whether %s is masked: %w", unitName(), err)
	} else if masked {
		return fmt.Errorf("%s is masked, not installing a fresh unit over it (unmask it first if this node should come back after a reboot)", unitName())
	}
	// A leftover Restart=no override must never survive into a freshly installed unit.
	if err := removeStaleRestartOverride(); err != nil {
		return err
	}
	unit, err := renderUnit(argv)
	if err != nil {
		return err
	}
	cur, err := os.ReadFile(version.SystemdUnitPath)
	// A content match only proves an earlier call wrote the file, not that enable succeeded.
	contentMatches := err == nil && string(cur) == unit
	enabled, enabledErr := unitEnabled()
	if contentMatches {
		// A query failure with matching content is returned, not answered with a reinstall.
		if enabledErr != nil {
			return enabledErr
		}
		if enabled {
			return nil
		}
	}
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

// unitMasked is true for an operator's mask; Lstat must not follow the /dev/null symlink.
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
	// A /run-only mask still means do-not-install, though unitEnabled errors on it.
	switch state := strings.TrimSpace(string(out)); state {
	case "masked", "masked-runtime":
		return true, nil
	default:
		return false, nil
	}
}

// unitEnabled reads UnitFileState; `is-enabled`'s exit code cannot tell a clean negative.
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

// Uninstall captures it before DisableUnit so a failed teardown can restore it.
func UnitEnabled() (bool, error) { return unitEnabled() }

// Disables boot start, protecting against a reboot that interrupts uninstall.
func DisableUnit() error { return systemctlOnUnit("disable") }

// For uninstall's rollback: restore boot enablement after a failed teardown.
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

// Called only after the static pods are down; earlier, its SIGTERM races kubelet's reaping.
func StopUnit() error { return systemctlOnUnit("stop") }

// A /run drop-in, so a real reboot clears it without cleanup.
var systemdDropInPath = "/run/systemd/system/stratum.service.d/uninstall-no-restart.conf"

// Disarms the active unit's Restart= for the teardown window; disable alone does not.
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

// An absent file must not skip the reload: a prior call may have failed its own.
func AllowRestart() error {
	if !SystemdAvailable() {
		return nil
	}
	return removeStaleRestartOverride()
}

// Removes the drop-in if present, then reloads systemd even if absent; a failed remove stops first.
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

// With KillMode=process systemd signals only the supervisor, never the containers.
func RestartUnit() error { return systemctlOnUnit("restart") }

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

func UnitExecPath() (string, error) {
	out, err := hostCmd("systemctl", "show", "-p", "ExecStart", "--value", unitName()).Output()
	if err != nil {
		return "", fmt.Errorf("systemctl show %s: %w", unitName(), err)
	}
	return execStartPath(string(out))
}

// Parses systemctl's ExecStart rendering "{ path=/s ; argv[]=... }".
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

// Read from the unit file: systemctl's argv[] rendering joins arguments unquoted.
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

// The directories systemd reads the unit's drop-ins from, shared ones included.
var unitDropInDirs = func() []string {
	name := unitName()
	var dirs []string
	for _, base := range []string{filepath.Dir(version.SystemdUnitPath), filepath.Dir(filepath.Dir(systemdDropInPath)),
		"/usr/local/lib/systemd/system", "/usr/lib/systemd/system"} {
		dirs = append(dirs, filepath.Join(base, name+".d"), filepath.Join(base, "service.d"))
	}
	return dirs
}

// systemd reads ExecStart= nowhere but the [Service] section.
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

// An empty ExecStart= resets the list, as systemd's does.
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

// quoteArgv's inverse under the same grammar subset; anything else is an error.
func splitSystemdArgv(line string) ([]string, error) {
	var argv []string
	var word strings.Builder
	inWord := false
	var quote rune
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		// escapeSystemdArg doubles % and $ before quoting, so they appear inside quotes too.
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

// An absent unit (or non-systemd host) is nothing to do; any other failure is returned.
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

// systemd re-reads unit files only on daemon-reload; file existence is not state.
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

// systemd otherwise keeps serving a deleted unit's last-loaded definition.
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

// Diagnostics go to stderr: this writer serves protocol handlers without a stream.
var labelLike = func(tmp, dst string) error {
	return selinux.LabelLikeWithWriter(os.Stderr, tmp, dst)
}

var (
	syncFile       = func(f *os.File) error { return f.Sync() }
	removeTempName = os.Remove
	syncDir        = atomicfile.SyncDir
)

func atomicfileSeams() atomicfile.Seams {
	return atomicfile.Seams{
		SyncFile:       syncFile,
		LabelLike:      labelLike,
		RemoveTempName: removeTempName,
		SyncDir:        syncDir,
	}
}

// Writes durably, via a temp file in the same directory.
func WriteFileAtomically(path string, data []byte, perm os.FileMode) error {
	return atomicfile.WriteFileWith(path, data, perm, atomicfileSeams())
}

// The file was replaced, but the replacement could not be made durable.
var ErrNotDurable = atomicfile.ErrNotDurable

// The temporary name could not be removed, and still holds the file's contents.
var ErrStrayCopy = atomicfile.ErrStrayCopy

// For the at-rest key: a rename would replace a key an operator put there since.
func CreateFileExclusively(path string, data []byte, perm os.FileMode) (os.FileInfo, error) {
	return atomicfile.CreateExclusiveWith(path, data, perm, atomicfileSeams())
}

// SyncDir fsyncs dir, so an entry added to or removed from it survives a power loss.
func SyncDir(dir string) error { return syncDir(dir) }

// KillMode=process, not mixed: shims share this cgroup (Delegate=yes); mixed would SIGKILL them.
func renderUnit(argv []string) (string, error) {
	execStart, err := quoteArgv(argv)
	if err != nil {
		return "", err
	}
	// The leading dash makes the file optional; stratum never creates it.
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

// Renders argv for ExecStart= under systemd's command-line grammar, not a shell's.
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

// A % starts a specifier and $ a variable reference, silently; ' starts a quote span.
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

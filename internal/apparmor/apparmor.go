// Package apparmor manages the runc profile under kernel.apparmor_restrict_unprivileged_unconfined.
package apparmor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/toolenv"
	"github.com/ctrliq/stratum/internal/version"
)

// Vars so a test can stand in for a host.
var (
	enabledPath    = "/sys/module/apparmor/parameters/enabled"
	securityfsPath = "/sys/kernel/security/apparmor"
	restrictPath   = "/proc/sys/kernel/apparmor_restrict_unprivileged_unconfined"
	hostProfile    = "/etc/apparmor.d/runc"
	hostDisabled   = "/etc/apparmor.d/disable/runc"
	procDir        = "/proc"
)

// parserTimeout keeps a hung parser from holding up a start, the reload loop or an uninstall.
const parserTimeout = time.Minute

const profileName = "runc"

// An AppArmor variable with several values expands to an alternation of them.
const attachVar = "@{stratum_runc_attach}"

// header finds the line that names the host profile's attachment.
var header = regexp.MustCompile(`(?m)^([ \t]*profile[ \t]+runc[ \t]+)("[^"\n]*"|[/@][^ \t\n]*)`)

func runcPath() string { return filepath.Join(version.BinDir, "runc") }

// A var so a test can stand in for the parser.
var parse = func(ctx context.Context, profile []byte, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, parserTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := toolenv.Cmd(toolenv.Path, "apparmor_parser", args...)
	cmd.Stdin = bytes.NewReader(profile)
	cmd.Stderr = &stderr
	err := cmd.Start()
	if err == nil {
		stop := context.AfterFunc(ctx, func() { _ = cmd.Process.Kill() })
		err = cmd.Wait()
		if !stop() && err != nil {
			err = fmt.Errorf("%w: %w", ctx.Err(), err)
		}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return errors.New("apparmor_parser is not installed (it is in the apparmor package)")
	}
	if msg := strings.TrimSpace(stderr.String()); err != nil && msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	if err != nil {
		return fmt.Errorf("apparmor_parser %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func enabled() (bool, error) {
	on, err := readSetting(enabledPath)
	if err != nil || on != "Y" {
		return false, err
	}
	return exists(securityfsPath)
}

func Restricted() (bool, error) {
	if on, err := enabled(); err != nil || !on {
		return false, err
	}
	restrict, err := readSetting(restrictPath)
	if err != nil || restrict == "" {
		return false, err
	}
	return restrict != "0", nil
}

func readSetting(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// A var so a test can answer for the kernel.
var runcLabel = probeRuncLabel

// Without tracing, nothing tells when the host reloads the profile, so nothing reloads it either.
var errCannotProbe = errors.New("the kernel does not allow tracing it, as with kernel.yama.ptrace_scope=3, so Stratum cannot tell when the host replaces the runc profile; restart stratum after reloading the host's AppArmor profiles")

// Not read from the kernel's profile list: apparmor_parser 5 shows <unknown> for attachments.
func Covers() (bool, error) {
	if on, err := enabled(); err != nil || !on {
		return false, err
	}
	label, err := runcLabel()
	if errors.Is(err, fs.ErrPermission) {
		err = fmt.Errorf("%w: %w", err, errCannotProbe)
	}
	if err != nil {
		return false, fmt.Errorf("read the AppArmor label %s runs with: %w", runcPath(), err)
	}
	return label == profileName, nil
}

// Drops the mode the kernel may append to the label.
func readLabel(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "attr", "apparmor", "current"))
	if errors.Is(err, fs.ErrNotExist) {
		b, err = os.ReadFile(filepath.Join(dir, "attr", "current"))
	}
	if err != nil {
		return "", err
	}
	label, _, _ := strings.Cut(strings.TrimSpace(string(b)), " (")
	return label, nil
}

type StackedProcess struct {
	PID   int
	Comm  string
	Label string
}

// A label is set at process start, so pods runc started uncovered keep theirs until recreated.
func Stacked() ([]StackedProcess, error) {
	if on, err := enabled(); err != nil || !on {
		return nil, err
	}
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, fmt.Errorf("list the processes: %w", err)
	}
	var out []StackedProcess
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procDir, e.Name())
		label, err := readLabel(dir)
		if err != nil || !strings.HasSuffix(label, "//&unconfined") {
			continue
		}
		if cg, err := os.ReadFile(filepath.Join(dir, "cgroup")); err != nil || !strings.Contains(string(cg), "kubepods") {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
		out = append(out, StackedProcess{PID: pid, Comm: strings.TrimSpace(string(comm)), Label: label})
	}
	return out, nil
}

// The host's profile widened for Stratum's runc, or a builtin one when the host ships none.
func Profile() (profile []byte, notWidened, err error) {
	own, err := hostOwnProfile()
	if err != nil {
		return nil, nil, err
	}
	if own == nil {
		return builtinProfile(), nil, nil
	}
	p, notWidened := widen(own)
	if notWidened != nil {
		return builtinProfile(), notWidened, nil
	}
	return p, nil, nil
}

var ErrHostProfileNotWidened = errors.New("Stratum's runc cannot be added to it, so Stratum loaded a runc profile for its own runc only, and the host's own runc runs without the host's profile until the host loads it again")

func widen(src []byte) ([]byte, error) {
	m := header.FindAllSubmatchIndex(src, -1)
	if len(m) != 1 {
		return nil, fmt.Errorf("%s has %d profile runc lines naming an attachment, want 1: %w; make it one, or disable it in /etc/apparmor.d/disable/", hostProfile, len(m), ErrHostProfileNotWidened)
	}
	line, prefixEnd, attachStart, attachEnd := m[0][0], m[0][3], m[0][4], m[0][5]
	var b bytes.Buffer
	b.Write(src[:line])
	fmt.Fprintf(&b, "%s = %s %s\n", attachVar, src[attachStart:attachEnd], runcPath())
	b.Write(src[line:prefixEnd])
	b.WriteString(attachVar)
	b.Write(src[attachEnd:])
	return b.Bytes(), nil
}

// Names the path where Ubuntu's uses @{exec_path}, which apparmor_parser only defines from 5.0.
func builtinProfile() []byte {
	return fmt.Appendf(nil, `abi <abi/4.0>,
include <tunables/global>

profile runc %[1]s flags=(unconfined) {
  userns,
  %[1]s mr,

  include if exists <local/runc>
}
`, runcPath())
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("inspect %s: %w", path, err)
}

// With the restriction off, runc's profile change is not stacked; loading would only relabel pods.
func Ensure(w io.Writer) error {
	if on, err := Restricted(); err != nil || !on {
		return err
	}
	warning, err := load(context.Background())
	if warning != nil {
		fmt.Fprintf(w, "  WARN: loaded the runc AppArmor profile for %s, but %v\n", runcPath(), warning)
	}
	return err
}

func Reconcile(ctx context.Context) (loaded bool, err error) {
	if err := Check(); !errors.Is(err, ErrNotCovered) {
		return false, err
	}
	warning, err := load(ctx)
	return true, errors.Join(err, warning)
}

// The parser's exit status does not prove runc took the profile.
func load(ctx context.Context) (warning, err error) {
	p, notWidened, err := Profile()
	if err == nil {
		err = parse(ctx, p, "--replace", "--skip-cache")
	}
	if err == nil {
		var ok bool
		ok, err = Covers()
		if errors.Is(err, errCannotProbe) {
			return errors.Join(notWidened, fmt.Errorf("could not check it: %w", err)), nil
		}
		if err == nil && !ok {
			err = fmt.Errorf("loaded, but %w", ErrNotCovered)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("give %s the AppArmor profile runc: %w%s", runcPath(), err, remedy)
	}
	return notWidened, nil
}

const remedy = " (with kernel.apparmor_restrict_unprivileged_unconfined set, containers cannot signal their own processes without it; to run without that restriction instead, set it to 0 in a file under /etc/sysctl.d)"

var ErrNotCovered = errors.New("it does not run under the runc AppArmor profile")

func Check() error {
	if on, err := Restricted(); err != nil || !on {
		return err
	}
	ok, err := Covers()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s: %w while kernel.apparmor_restrict_unprivileged_unconfined is set, so containers created now are labelled <profile>//&unconfined and cannot signal their own processes", runcPath(), ErrNotCovered)
	}
	_, notWidened, err := Profile()
	return errors.Join(err, notWidened)
}

// An upgrade asks before it changes the node, so an unloadable profile is refused in advance.
func Prerequisite() error {
	if on, err := Restricted(); err != nil || !on {
		return err
	}
	p, _, err := Profile()
	if err == nil {
		err = parse(context.Background(), p, "--skip-kernel-load", "--skip-cache")
	}
	if err != nil {
		return fmt.Errorf("%s cannot be given the AppArmor profile runc: %w%s", runcPath(), err, remedy)
	}
	return nil
}

// Where runc's label cannot be read, the restriction and a loaded profile stand for it.
func toRestore() (bool, error) {
	ok, err := Covers()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case errors.Is(err, errCannotProbe):
		if on, err := Restricted(); err != nil || !on {
			return false, err
		}
		return runcLoaded()
	}
	return ok, err
}

func Widened() bool {
	ok, err := toRestore()
	return ok || err != nil
}

func RestoreWithWriter(w io.Writer) error {
	if ok, err := toRestore(); err != nil || !ok {
		return err
	}
	own, err := hostOwnProfile()
	if err != nil {
		return err
	}
	if own != nil {
		// Not checked afterwards: the host's own profile may itself cover Stratum's runc.
		if err := parse(context.Background(), own, "--replace", "--skip-cache"); err != nil {
			return fmt.Errorf("restore the runc AppArmor profile: %w", err)
		}
		fmt.Fprintln(w, "  AppArmor: restored the host's runc profile")
		return nil
	}
	if err := parse(context.Background(), builtinProfile(), "--remove"); err != nil {
		return fmt.Errorf("remove the runc AppArmor profile: %w", err)
	}
	fmt.Fprintln(w, "  AppArmor: removed the runc profile")
	return nil
}

func runcLoaded() (bool, error) {
	dir := filepath.Join(securityfsPath, "policy", "profiles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("list the loaded AppArmor profiles: %w", err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), profileName+".") {
			continue
		}
		if name, _ := readSetting(filepath.Join(dir, e.Name(), "name")); name == profileName {
			return true, nil
		}
	}
	return false, nil
}

func hostOwnProfile() ([]byte, error) {
	disabled, err := exists(hostDisabled)
	if err != nil || disabled {
		return nil, err
	}
	src, err := os.ReadFile(hostProfile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", hostProfile, err)
	}
	return src, nil
}

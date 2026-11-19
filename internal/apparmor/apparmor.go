// Package apparmor gives Stratum's runc the AppArmor profile the host gives
// its own runc.
//
// With kernel.apparmor_restrict_unprivileged_unconfined set, as Ubuntu sets
// it, a task labelled unconfined that changes profile without
// CAP_MAC_OVERRIDE gets the new profile stacked on unconfined instead of
// switched to. runc runs the container's command after dropping to the
// container's capabilities, so its processes run as
// cri-containerd.apparmor.d//&unconfined: containerd's profile denies signals
// between them, and host profiles, such as the one for /usr/bin/nslookup,
// attach inside the container. The restriction applies only to the unconfined
// profile itself, which is why Ubuntu ships a profile named runc, in
// unconfined mode, for /usr/bin/runc and /usr/sbin/runc. Stratum's runc lives
// in version.BinDir, which that profile does not cover.
//
// The profile keeps the name runc because containerd's default profile
// accepts signals from runc; under any other name runc could not stop a
// container. It is loaded from standard input, not from a file under
// /etc/apparmor.d: the host's AppArmor loads that directory in parallel, so a
// second file defining runc would race the host's at boot, and a file of
// Stratum's own there is skipped when the operator disables the host's.
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

// The host's AppArmor state, as the kernel and the apparmor package lay it
// out. Vars so a test can stand in for a host's.
var (
	enabledPath    = "/sys/module/apparmor/parameters/enabled"
	securityfsPath = "/sys/kernel/security/apparmor"
	restrictPath   = "/proc/sys/kernel/apparmor_restrict_unprivileged_unconfined"
	hostProfile    = "/etc/apparmor.d/runc"
	hostDisabled   = "/etc/apparmor.d/disable/runc"
	procDir        = "/proc"
)

// parserTimeout keeps a parser that hangs from holding up a start, the reload
// loop or an uninstall.
const parserTimeout = time.Minute

const profileName = "runc"

// attachVar holds the widened profile's attachment: the host's own and
// Stratum's runc. A variable with several values expands to an alternation of
// them, so the host's attachment is kept as it is written, whether a path, a
// glob or another variable.
const attachVar = "@{stratum_runc_attach}"

// header finds the line that names the host profile's attachment.
var header = regexp.MustCompile(`(?m)^([ \t]*profile[ \t]+runc[ \t]+)("[^"\n]*"|[/@][^ \t\n]*)`)

func runcPath() string { return filepath.Join(version.BinDir, "runc") }

// parse runs apparmor_parser with args on profile, fed on standard input; a
// var so a test can stand in for the parser.
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

// enabled reports whether AppArmor confines containers on this node: the
// kernel runs it and its securityfs is mounted, which containerd and runc
// check before they apply a profile.
func enabled() (bool, error) {
	on, err := readSetting(enabledPath)
	if err != nil || on != "Y" {
		return false, err
	}
	return exists(securityfsPath)
}

// Restricted reports whether the kernel stacks runc's profile change: AppArmor
// is enabled and kernel.apparmor_restrict_unprivileged_unconfined is set.
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

// readSetting reads a kernel setting, trimmed; one the kernel does not have
// reads as empty.
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

// runcLabel is the AppArmor label Stratum's runc gets when it starts; a var so
// a test can answer for the kernel.
var runcLabel = probeRuncLabel

// errCannotProbe marks a kernel that will not let Stratum start its runc
// traced, as one that forbids ptrace altogether (kernel.yama.ptrace_scope=3)
// does. Nothing then tells Stratum whether runc runs under the profile, so
// nothing loads it again when the host's AppArmor replaces it.
var errCannotProbe = errors.New("the kernel does not allow tracing it, as with kernel.yama.ptrace_scope=3, so Stratum cannot tell when the host replaces the runc profile; restart stratum after reloading the host's AppArmor profiles")

// Covers reports whether Stratum's runc runs under the runc profile. It asks
// the kernel by starting the runc and reading the label it got, not from the
// kernel's list of loaded profiles: apparmor_parser 5 loads a profile's
// attachment without its text, so that list shows <unknown> for what any
// profile attaches to. A kernel without AppArmor has no runc profile.
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

// readLabel reads the AppArmor label of the process whose /proc directory is
// dir, without its mode.
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

// StackedProcess is a process running with a label the kernel stacked on
// unconfined.
type StackedProcess struct {
	PID   int
	Comm  string
	Label string
}

// Stacked lists the container processes, those in the kubelet's kubepods
// cgroups, running with a label the kernel stacked on unconfined, such as
// cri-containerd.apparmor.d//&unconfined. A label is set when a process
// starts, so containers runc started while it did not run under the runc
// profile keep theirs until their pods are recreated. A process that ends
// while it is read is left out.
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

// Profile is the runc profile Ensure loads: the host's file with its
// attachment widened to Stratum's runc, so whatever the distribution changes in
// its profile carries over, and the host's runc keeps it. When the host ships
// none, or the operator has disabled it, it is a profile of the same shape for
// Stratum's runc alone. So it is for a host profile Stratum cannot widen,
// rather than a node that does not start; notWidened, which wraps
// ErrHostProfileNotWidened, then says why.
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

// ErrHostProfileNotWidened marks a host runc profile Stratum's runc could not
// be added to.
var ErrHostProfileNotWidened = errors.New("Stratum's runc cannot be added to it, so Stratum loaded a runc profile for its own runc only, and the host's own runc runs without the host's profile until the host loads it again")

// widen points the host profile's header at attachVar, defined just above it
// as the host's attachment and Stratum's runc.
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

// builtinProfile is Ubuntu's runc profile, attached to Stratum's runc only.
func builtinProfile() []byte {
	return fmt.Appendf(nil, `abi <abi/4.0>,
include <tunables/global>

profile runc %s flags=(unconfined) {
  userns,
  @{exec_path} mr,

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

// Ensure loads the runc profile when the kernel is Restricted and checks that
// Stratum's runc then runs under it. It loads on every start, so the profile
// is current again before containerd starts even when the host's AppArmor
// reloaded its own since. With the restriction off it loads nothing: runc's
// profile change is not stacked then, and loading would only move unconfined
// pods from the label unconfined to runc. What load could not do without
// failing is warned about to w.
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

// Reconcile loads the runc profile again when Check finds the kernel's no
// longer covers Stratum's runc, as after the host's AppArmor reloaded the
// host's own. loaded says it ran the load, err how the check or the load went.
func Reconcile(ctx context.Context) (loaded bool, err error) {
	if err := Check(); !errors.Is(err, ErrNotCovered) {
		return false, err
	}
	warning, err := load(ctx)
	return true, errors.Join(err, warning)
}

// load loads Profile and checks runc took it: the parser's exit status does
// not prove it, since another profile can attach to runc first. warning is
// what it could not do without failing: widen the host's profile, or read
// runc's label. Only the label read may fail that way, so a permission error
// before the load is not taken for it.
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

// remedy ends an error that keeps Stratum's runc from its profile.
const remedy = " (with kernel.apparmor_restrict_unprivileged_unconfined set, containers cannot signal their own processes without it; to run without that restriction instead, set it to 0 in a file under /etc/sysctl.d)"

// ErrNotCovered marks a kernel whose runc profile does not cover Stratum's
// runc.
var ErrNotCovered = errors.New("it does not run under the runc AppArmor profile")

// Check reports what keeps containers from running under their own profile
// alone on a Restricted kernel: ErrNotCovered, with the consequence, when
// Stratum's runc does not run under the runc profile; the error that leaves
// that unknown; or, once it does, ErrHostProfileNotWidened when the profile is
// Stratum's own because the host's could not be widened.
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

// Prerequisite reports, loading nothing, whether Ensure can load the runc
// profile on this node: nil when the kernel is not Restricted, and otherwise
// the error building or compiling the profile gives. A host profile it cannot
// widen is not one: Ensure loads a profile of Stratum's own then. An upgrade
// asks before it changes the node, so a node the new release could not start
// is refused while it still runs the old one.
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

// toRestore reports whether uninstall has a runc profile to put back: one
// Stratum's runc runs under. Where runc's label cannot be read, the
// restriction and a loaded runc profile stand for it, since Stratum loads its
// profile only with the restriction on. A host without Stratum's runc has
// nothing to put back.
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

// Widened reports whether uninstall would restore the host's runc profile, for
// its preview; what it cannot read counts as yes.
func Widened() bool {
	ok, err := toRestore()
	return ok || err != nil
}

// RestoreWithWriter puts the kernel's runc profile back as the host has it:
// the host's own profile when it ships one the operator has not disabled, and
// no runc profile otherwise. It reports what it did to w, and does nothing
// where Stratum's runc does not run under the runc profile.
func RestoreWithWriter(w io.Writer) error {
	if ok, err := toRestore(); err != nil || !ok {
		return err
	}
	own, err := hostOwnProfile()
	if err != nil {
		return err
	}
	if own != nil {
		// Not checked afterwards: the host's own profile may itself cover
		// Stratum's runc, as an operator's workaround does.
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

// runcLoaded reports whether the kernel has a profile named runc loaded.
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

// hostOwnProfile is the host's runc profile as it ships it, or nil when it
// ships none or the operator has disabled it.
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

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
	"sync"
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
)

// parserTimeout bounds one apparmor_parser run, so a parser that hangs cannot
// hold up a start, the reload loop's shutdown or an uninstall; a var so a test
// can shorten it.
var parserTimeout = time.Minute

const profileName = "runc"

// attachVar holds the attachment of the profile Profile renders: the host's
// own attachment and Stratum's runc. A variable with several values expands to
// an alternation of them, so the host's attachment is kept as it is written,
// whether a path, a glob or another variable.
const attachVar = "@{stratum_runc_attach}"

// header finds the line that names the host profile's attachment.
var header = regexp.MustCompile(`(?m)^([ \t]*profile[ \t]+runc[ \t]+)("[^"\n]*"|[/@][^ \t\n]*)`)

// runcPath is Stratum's runc, which the profile attaches to.
func runcPath() string { return filepath.Join(version.BinDir, "runc") }

// parse runs apparmor_parser with args on profile, fed on standard input, and
// kills it when ctx ends or parserTimeout passes. A var so a test can stand in
// for the parser.
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
		if !stop() {
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
// check before they apply a profile. In a container without securityfs, as a
// node under test can be, no profile is applied, so none is stacked.
func enabled() (bool, error) {
	on, err := readSetting(enabledPath)
	if err != nil || on != "Y" {
		return false, err
	}
	return exists(securityfsPath)
}

// Restricted reports whether the kernel stacks runc's profile change: AppArmor
// is enabled and kernel.apparmor_restrict_unprivileged_unconfined is set. A
// kernel without the setting has no such restriction.
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
		err = fmt.Errorf("%w: %w", errCannotProbe, err)
	}
	if err != nil {
		return false, fmt.Errorf("read the AppArmor label %s runs with: %w", runcPath(), err)
	}
	name, _, _ := strings.Cut(label, " (")
	return name == profileName, nil
}

// procDir is where the kernel lists processes; a var so a test can stand in
// for it.
var procDir = "/proc"

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

// Stacked lists the container processes running with a label the kernel
// stacked on unconfined, such as cri-containerd.apparmor.d//&unconfined:
// containers runc started while it did not run under the runc profile. A
// container process is one in the kubelet's kubepods cgroups. A label is set when a
// process starts, so they keep it, and cannot signal their own processes,
// until their pods are recreated; the profile helps only containers started
// after it is loaded. A process that ends while it is read is left out.
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

// errCannotProbe marks a kernel that will not let Stratum start its runc
// traced, as one that forbids ptrace altogether (kernel.yama.ptrace_scope=3)
// does, so whether runc runs under the profile cannot be read.
var errCannotProbe = errors.New("the kernel does not allow tracing it")

// Profile is the runc profile Ensure loads. When the host ships one, it is the
// host's file with its attachment widened to Stratum's runc, so whatever the
// distribution changes in its profile carries over. When the host ships none,
// or the operator has disabled the host's, it is a profile of the same shape
// for Stratum's runc alone, which leaves the host's runc as the operator set
// it.
//
// A host profile Stratum cannot widen gets the same profile of its own, and
// notWidened, which wraps ErrHostProfileNotWidened, says why: the node still
// starts, and the host's own runc runs without the host's profile until the
// host loads it again.
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
// the kernel's runc profile then covers Stratum's runc. It loads on every
// start, so the profile is current again before containerd starts even when
// the host's AppArmor reloaded its own since. With the restriction off it
// loads nothing: runc's profile change is not stacked then, and loading would
// only move unconfined pods from the label unconfined to runc.
//
// It checks by starting runc traced, which a kernel that forbids ptrace
// altogether (kernel.yama.ptrace_scope=3) refuses; the load then stands, with
// a warning to w that it could not be checked. A host profile it cannot widen
// is warned about the same way (Profile).
func Ensure(w io.Writer) error {
	on, err := Restricted()
	if err != nil {
		return fmt.Errorf("read the AppArmor restriction: %w", err)
	}
	if !on {
		return nil
	}
	warning, err := load(context.Background())
	if err != nil {
		return err
	}
	if warning != nil {
		fmt.Fprintf(w, "  WARN: loaded the runc AppArmor profile for %s, but %v\n", runcPath(), warning)
	}
	return nil
}

// Reload is what Reconcile did.
type Reload int

const (
	// NotLoaded: the profile covered runc, or the restriction is off.
	NotLoaded Reload = iota
	// Loaded: the profile no longer covered runc, and it was loaded again.
	Loaded
	// LoadedUnchecked: whether it covered runc could not be read, so it was
	// loaded again anyway.
	LoadedUnchecked
)

// Reconcile loads the runc profile again when Check finds the kernel's runc
// profile no longer covers Stratum's runc, as after the host's AppArmor
// reloaded the host's own. Where runc's label cannot be read, it loads when
// the kernel's runc profile is no longer the one this process loaded.
func Reconcile(ctx context.Context) (Reload, error) {
	err := Check()
	switch {
	case errors.Is(err, ErrNotCovered):
		warning, err := load(ctx)
		return Loaded, errors.Join(err, warning)
	case errors.Is(err, errCannotProbe):
		if !loaded.replaced() {
			return NotLoaded, nil
		}
		_, err := load(ctx)
		return LoadedUnchecked, err
	}
	return NotLoaded, err
}

// load loads Profile and checks runc took it. warning is what it could not do
// as asked without failing: widen the host's profile, or read runc's label.
func load(ctx context.Context) (warning, err error) {
	p, notWidened, err := Profile()
	if err == nil {
		err = parse(ctx, p, "--replace", "--skip-cache")
	}
	if err == nil {
		loaded.record()
		var ok bool
		ok, err = Covers()
		if err == nil && !ok {
			err = fmt.Errorf("loaded, but %w", ErrNotCovered)
		}
	}
	if errors.Is(err, errCannotProbe) {
		return errors.Join(notWidened, fmt.Errorf("could not check it: %w", err)), nil
	}
	if err != nil {
		return nil, fmt.Errorf("give %s the AppArmor profile runc: %w%s", runcPath(), err, remedy)
	}
	return notWidened, nil
}

// loadState is what this process last loaded. Where runc's label cannot be
// read, a look compares the kernel's hash of its runc profile with the one
// recorded here to tell whether the host replaced the profile, rather than
// loading it again every time, which would wake the next look; with no hash
// to read, it loads at most once per blindInterval.
type loadState struct {
	sync.Mutex
	hash      string
	blindLoad time.Time
}

var loaded loadState

// blindInterval is how often a look loads the profile when neither runc's
// label nor the kernel's hash of it can be read; a var so a test can shorten
// it.
var blindInterval = 50 * time.Second

// record notes the hash of the runc profile just loaded.
func (l *loadState) record() {
	h := runcHash()
	l.Lock()
	defer l.Unlock()
	l.hash = h
}

// replaced reports whether the kernel's runc profile is no longer the one this
// process loaded, or, with no hash to compare, whether blindInterval has
// passed since the last load it could not compare.
func (l *loadState) replaced() bool {
	h := runcHash()
	l.Lock()
	defer l.Unlock()
	if h != "" {
		return h != l.hash
	}
	if time.Since(l.blindLoad) < blindInterval {
		return false
	}
	l.blindLoad = time.Now()
	return true
}

// remedy ends an error that keeps Stratum's runc from its profile.
const remedy = " (with kernel.apparmor_restrict_unprivileged_unconfined set, containers cannot signal their own processes without it; to run without that restriction instead, set it to 0 in a file under /etc/sysctl.d)"

// ErrNotCovered marks a kernel whose runc profile does not cover Stratum's
// runc.
var ErrNotCovered = errors.New("it does not run under the runc AppArmor profile")

// Check reports what keeps containers from running under their own profile
// alone: ErrNotCovered, with the consequence, when the kernel is Restricted
// and its runc profile does not cover Stratum's runc, or the error that leaves
// either unknown; and, once it covers it, ErrHostProfileNotWidened when that
// took a profile of Stratum's own. A host without the restriction has nothing
// to report.
func Check() error {
	on, err := Restricted()
	if err != nil {
		return fmt.Errorf("read the AppArmor restriction: %w", err)
	}
	if !on {
		return nil
	}
	ok, err := Covers()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s: %w while kernel.apparmor_restrict_unprivileged_unconfined is set, so containers created now are labelled <profile>//&unconfined and cannot signal their own processes", runcPath(), ErrNotCovered)
	}
	if _, notWidened, err := Profile(); err != nil || notWidened != nil {
		return errors.Join(err, notWidened)
	}
	return nil
}

// Widened reports whether Stratum's runc runs under the runc profile, so
// uninstall can say it will restore the host's. A label it cannot read counts
// as yes, so the preview errs toward saying so, unless the kernel would not
// have made Stratum load one; a host without AppArmor, or without Stratum's
// runc, has nothing to restore.
func Widened() bool {
	ok, err := Covers()
	switch {
	case err == nil:
		return ok
	case errors.Is(err, fs.ErrNotExist):
		return false
	case errors.Is(err, errCannotProbe):
		on, rerr := Restricted()
		return on || rerr != nil
	}
	return true
}

// RestoreWithWriter puts the kernel's runc profile back as the host has it,
// once its runc profile covers Stratum's runc: the host's own profile when it
// ships one the operator has not disabled, and no runc profile otherwise. It
// reports what it did to w.
//
// Where runc's label cannot be read, the restriction stands for it: Stratum
// loaded its profile only with the restriction on. It then restores the same
// way without checking the result, and removes a runc profile only if one is
// loaded.
func RestoreWithWriter(w io.Writer) error {
	ok, err := Covers()
	checked := true
	switch {
	case errors.Is(err, fs.ErrNotExist) || (err == nil && !ok):
		return nil
	case errors.Is(err, errCannotProbe):
		on, rerr := Restricted()
		if rerr != nil || !on {
			return rerr
		}
		if loaded, lerr := runcLoaded(); lerr != nil || !loaded {
			return lerr
		}
		checked = false
	case err != nil:
		return err
	}
	own, err := hostOwnProfile()
	if err != nil {
		return err
	}
	ctx := context.Background()
	// The host's own profile is the host's to decide, including whether it
	// covers Stratum's runc, as an operator's own workaround may; only a
	// profile left behind once the host loads none must not.
	if own != nil {
		if err := parse(ctx, own, "--replace", "--skip-cache"); err != nil {
			return fmt.Errorf("restore the runc AppArmor profile: %w", err)
		}
		fmt.Fprintln(w, "  AppArmor: restored the host's runc profile")
		return nil
	}
	err = parse(ctx, builtinProfile(), "--remove")
	if err == nil && checked {
		if ok, err = Covers(); err == nil && ok {
			err = fmt.Errorf("it still attaches to %s", runcPath())
		}
	}
	if err != nil {
		return fmt.Errorf("remove the runc AppArmor profile: %w", err)
	}
	fmt.Fprintln(w, "  AppArmor: removed the runc profile")
	return nil
}

// Prerequisite reports, loading nothing, whether Ensure can load the runc
// profile on this node: nil when the kernel is not Restricted, and otherwise
// the error building or compiling the profile gives. A host profile it cannot
// widen is not one: Ensure loads a profile of Stratum's own then. An upgrade asks it before
// it changes the node, so a node the new release could not start is refused
// while it still runs the old one.
func Prerequisite() error {
	on, err := Restricted()
	if err != nil {
		return fmt.Errorf("read the AppArmor restriction: %w", err)
	}
	if !on {
		return nil
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

// runcProfileDir is the kernel's directory for its loaded runc profile, or
// empty when it has none loaded.
func runcProfileDir() (string, error) {
	dir := filepath.Join(securityfsPath, "policy", "profiles")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("list the loaded AppArmor profiles: %w", err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), profileName+".") {
			continue
		}
		if name, err := readSetting(filepath.Join(dir, e.Name(), "name")); err == nil && name == profileName {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", nil
}

// runcLoaded reports whether the kernel has a profile named runc loaded.
func runcLoaded() (bool, error) {
	dir, err := runcProfileDir()
	return dir != "", err
}

// runcHash is the kernel's hash of the runc profile it has loaded, or empty
// when it has none or does not keep hashes.
func runcHash() string {
	dir, err := runcProfileDir()
	if err != nil || dir == "" {
		return ""
	}
	h, _ := readSetting(filepath.Join(dir, "raw_sha256"))
	return h
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

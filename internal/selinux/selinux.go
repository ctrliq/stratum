// Package selinux keeps the files Stratum writes labelled the way the host's
// SELinux policy says they should be, and registers the file contexts that make
// the policy treat Stratum's paths the way container-selinux treats the
// standard container runtime's.
//
// Everything here is a no-op on a host where SELinux is disabled.
package selinux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ctrliq/stratum/internal/toolenv"
	"github.com/ctrliq/stratum/internal/version"
)

// Mode is the host's SELinux mode.
type Mode int

const (
	Disabled Mode = iota
	Permissive
	Enforcing
)

func (m Mode) String() string {
	switch m {
	case Permissive:
		return "permissive"
	case Enforcing:
		return "enforcing"
	}
	return "disabled"
}

// selinuxFS is where the kernel mounts selinuxfs; a var so a test can stand in
// for a host in any mode.
var selinuxFS = "/sys/fs/selinux"

// CurrentMode reads the host's mode from selinuxfs. A host without the enforce
// file has SELinux disabled. One whose enforce file exists but cannot be read
// is treated as enforcing, so an unreadable mode never turns the checks off.
func CurrentMode() Mode {
	b, err := os.ReadFile(filepath.Join(selinuxFS, "enforce"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Disabled
	case err != nil:
		return Enforcing
	case strings.TrimSpace(string(b)) == "0":
		return Permissive
	}
	return Enforcing
}

// run executes a policy tool as toolenv has it found and run, whose C locale
// matters as semanage translates the section headings parseListing reads, and
// returns its trimmed standard output. Its standard error goes only into the
// error, so a warning a tool prints never becomes part of a label or a
// listing. A var so a test can stand in for semodule, semanage, matchpathcon,
// chcon and restorecon.
var run = func(name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := toolenv.Cmd(toolenv.Path, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if msg := strings.TrimSpace(stderr.String()); err != nil && msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	return strings.TrimSpace(stdout.String()), err
}

// errNotInstalled marks a policy tool, or the policy module, that is not
// installed.
var errNotInstalled = errors.New("not installed")

// tool runs name and reports a missing binary as errNotInstalled, naming the
// package that provides it.
func tool(pkg, name string, args ...string) (string, error) {
	out, err := run(name, args...)
	if errors.Is(err, exec.ErrNotFound) {
		return "", fmt.Errorf("%s is %w (it is in the %s package)", name, errNotInstalled, pkg)
	}
	if err != nil {
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// stampPath is the file Ensure writes once it has registered Stratum's
// contexts and relabelled everything under them, holding the contexts it
// registered. Its presence is what turns on LabelLike, and a missing or stale
// stamp makes the next Ensure relabel everything again. A start with SELinux
// disabled removes it: files written then have no labels. It lives in the data
// directory, so uninstalling removes it.
func stampPath() string { return filepath.Join(version.DataDir, "selinux-labels") }

// LabelStampPath is the stamp Ensure writes once this node's files are labelled for Stratum's contexts.
func LabelStampPath() string { return stampPath() }

// labelling reports whether this node's files are labelled for Stratum's
// contexts. A stamp that exists but cannot be inspected is an error, not an
// unlabelled node.
func labelling() (bool, error) {
	_, err := os.Stat(stampPath())
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("inspect %s: %w", stampPath(), err)
}

// ForgetLabels stops labelling on a node that opted out, before anything is
// written on this start, so the first start after spec.selinux is set to
// false labels nothing. Ensure does the same later in the start; this runs
// before payload staging, which precedes reading the config.
func ForgetLabels() error {
	if CurrentMode() == Disabled {
		return nil
	}
	return removeStamp()
}

// Labelled reports whether SELinux is enabled and Ensure has labelled this
// node, which is when containers can be confined: containerd's enable_selinux
// gives them types container-selinux defines, and only on a labelled node can
// the runtime and its state carry the labels those types are allowed to use.
// A stamp that cannot be inspected counts as labelled, so the runtime keeps
// confining containers rather than dropping confinement on an I/O error.
func Labelled() bool {
	if CurrentMode() == Disabled {
		return false
	}
	on, err := labelling()
	return on || err != nil
}

// LabelLike gives path the label the policy assigns to dst. A file written to a
// temp name and renamed over dst otherwise keeps the label of the temp name, and
// a binary with the wrong label runs in the wrong domain: the stratum binary
// labelled bin_t runs unconfined, and then runc may not start any container.
// Labelling the temp file before the rename means dst is never visible with the
// wrong label.
//
// It does nothing until Ensure has labelled the node: on a node that opted
// out, that lacks the policy, or whose contexts are not registered yet, there
// is no label to keep, and the next Ensure relabels what was written. With
// SELinux disabled the file gets no label at all, so the stamp goes, and the
// first start with SELinux enabled relabels everything. A
// failure because the policy is gone, as on a labelled node whose operator
// has just opted out and removed the packages before Ensure has run, is a
// warning for the same reason; any other failure fails the write.
func LabelLike(path, dst string) error { return LabelLikeWithWriter(os.Stdout, path, dst) }

// LabelLikeWithWriter labels path for dst and reports warnings to w.
func LabelLikeWithWriter(w io.Writer, path, dst string) error {
	if CurrentMode() == Disabled {
		return removeStamp()
	}
	if on, err := labelling(); err != nil || !on {
		return err
	}
	label, err := tool("libselinux-utils", "matchpathcon", "-n", dst)
	if err != nil {
		return policyGone(w, dst, fmt.Errorf("look up the SELinux label for %s: %w", dst, err))
	}
	if label == "" || label == "<<none>>" {
		return nil
	}
	if _, err := tool("coreutils", "chcon", label, path); err != nil {
		return policyGone(w, dst, fmt.Errorf("label %s for %s: %w", path, dst, err))
	}
	return nil
}

// policyGone turns a failure to label dst into a warning when the policy it
// needs is gone: matchpathcon, or the container module that defines the
// labels Stratum's contexts name, as on a labelled node whose operator has
// removed container-selinux. The stamp goes, so nothing more is labelled
// until Ensure has run, and Ensure then decides whether this node may start
// without the policy. Any other failure is returned.
func policyGone(w io.Writer, dst string, err error) error {
	if !errors.Is(err, errNotInstalled) {
		perr := requirePolicy()
		if !errors.Is(perr, errNotInstalled) {
			return err
		}
		err = errors.Join(err, perr)
	}
	if rmErr := removeStamp(); rmErr != nil {
		return errors.Join(err, rmErr)
	}
	fmt.Fprintf(w, "  WARN: %s is not labelled for SELinux: %v; the next start relabels it\n", dst, err)
	return nil
}

// mountInfo lists this mount namespace's mounts; a var so a test can stand in
// for a host's.
var mountInfo = "/proc/self/mountinfo"

// mountsUnder lists the directories mounted strictly beneath any of roots.
// restorecon -x stops only where the device changes, so a bind mount from the
// same filesystem, as the kubelet makes for subPath volumes, is walked unless
// excluded by name.
func mountsUnder(roots []string) ([]string, error) {
	b, err := os.ReadFile(mountInfo)
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		m := unescapeMount(f[4])
		for _, r := range roots {
			if m != r && strings.HasPrefix(m, strings.TrimSuffix(r, "/")+"/") {
				if fi, err := os.Stat(m); err == nil && fi.IsDir() {
					out = append(out, m)
				}
				break
			}
		}
	}
	return out, nil
}

// unescapeMount undoes mountinfo's octal escapes for space, tab, newline and
// backslash.
func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// Restore resets the label of each path that exists to the policy's; a path
// that cannot be inspected is an error, not a path to skip. With
// recursive set it walks directories in parallel, ignores files that vanish
// during the walk, as containerd's and etcd's do, and never enters a mount
// beneath them, whether of another filesystem or a bind mount: the run and
// data directories hold the mounts of running containers and pod volumes,
// whose labels belong to the runtime and the kubelet.
func Restore(recursive bool, paths ...string) error {
	if CurrentMode() == Disabled {
		return nil
	}
	var present []string
	for _, p := range paths {
		_, err := os.Lstat(p)
		switch {
		case err == nil:
			present = append(present, p)
		case !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("restore SELinux labels: %w", err)
		}
	}
	if len(present) == 0 {
		return nil
	}
	args := present
	if recursive {
		mounts, err := mountsUnder(present)
		if err != nil {
			return fmt.Errorf("restore SELinux labels: %w", err)
		}
		args = []string{"-R", "-x", "-i", "-T", "0"}
		for _, m := range mounts {
			args = append(args, "-e", m)
		}
		args = append(args, present...)
	}
	if _, err := tool("policycoreutils", "restorecon", args...); err != nil {
		return fmt.Errorf("restore SELinux labels: %w", err)
	}
	return nil
}

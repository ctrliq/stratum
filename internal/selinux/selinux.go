// Package selinux registers Stratum's file contexts and labels its files; a no-op when disabled.
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

// a var so a test can stand in for a host in any mode.
var selinuxFS = "/sys/fs/selinux"

// an unreadable enforce file is treated as enforcing, so the checks never turn off.
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

// runs with LC_ALL=C: semanage translates the section headings parseListing reads.
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

// a missing policy tool, or policy module, matches it.
var errNotInstalled = errors.New("not installed")

// a missing binary is errNotInstalled, naming the package that provides it.
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

// its presence turns on LabelLike; a missing or stale stamp forces a full relabel.
func stampPath() string { return filepath.Join(version.DataDir, "selinux-labels") }

// the stamp Ensure writes once this node's files are labelled.
func LabelStampPath() string { return stampPath() }

// a stamp that exists but cannot be inspected is an error, not an unlabelled node.
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

// runs before payload staging, earlier than the same removal Ensure does.
func ForgetLabels() error {
	if CurrentMode() == Disabled {
		return nil
	}
	return removeStamp()
}

// a stamp that cannot be inspected counts as labelled, keeping containers confined.
func Labelled() bool {
	if CurrentMode() == Disabled {
		return false
	}
	on, err := labelling()
	return on || err != nil
}

// gives path the label the policy assigns to dst, before a temp-file rename lands.
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

// turns a labelling failure into a warning when the policy it needs is gone.
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

// a var so a test can stand in for a host's.
var mountInfo = "/proc/self/mountinfo"

// restorecon -x stops only where the device changes, so bind mounts are excluded by name.
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

// undoes mountinfo's octal escapes for space, tab, newline and backslash.
func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// recursive walks never enter a mount beneath the paths, bind mounts included.
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

// Package toolenv is how host tools are found and run when Stratum runs them
// as root: iptables and tc load code from XTABLES_LIBDIR and TC_LIB_DIR, and a
// shim of any tool can sit first on the caller's PATH, so neither the caller's
// environment nor its PATH is used.
package toolenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Path is where those tools are looked for and what they see as PATH.
const Path = "/usr/sbin:/usr/bin:/sbin:/bin"

// Env is the whole environment of a host tool. LC_ALL=C because callers match
// the English text tools print.
func Env() []string { return []string{"PATH=" + Path, "LC_ALL=C"} }

// Cmd is exec.Command with name looked up in path, not on the caller's PATH,
// and Env as the environment. A name with a slash is used as given. The
// command is built from the resolved path: exec looks a bare name up on the
// caller's PATH as soon as it is given one. A name not in path leaves a path
// with a slash, so exec does not look it up elsewhere, and exec.ErrNotFound,
// which is also what the command then fails with.
func Cmd(path, name string, args ...string) *exec.Cmd {
	p, err := Look(path, name)
	cmd := exec.Command(p, args...)
	cmd.Err = err
	cmd.Env = Env()
	return cmd
}

// Look is name's path in the dirs of path, as Cmd uses it.
func Look(path, name string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return filepath.Join(string(filepath.Separator), name), &exec.Error{Name: name, Err: exec.ErrNotFound}
}

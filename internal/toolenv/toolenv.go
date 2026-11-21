// Package toolenv runs host tools with a fixed environment: iptables and tc load code from env.
package toolenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const Path = "/usr/sbin:/usr/bin:/sbin:/bin"

// Env sets LC_ALL=C because callers match the English text tools print.
func Env() []string { return []string{"PATH=" + Path, "LC_ALL=C"} }

// Cmd builds the command from the resolved path: exec looks a bare name up on the caller's PATH.
func Cmd(path, name string, args ...string) *exec.Cmd {
	p, err := Look(path, name)
	cmd := exec.Command(p, args...)
	cmd.Err = err
	cmd.Env = Env()
	return cmd
}

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

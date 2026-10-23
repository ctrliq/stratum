package selinux

import (
	"os"
	"strings"
)

// procCurrent is this process's SELinux context; a var so a test can stand in
// for it.
var procCurrent = "/proc/self/attr/current"

// systemRuntimeContext is the context containerd runs in when systemd starts
// Stratum.
const systemRuntimeContext = "system_u:system_r:container_runtime_t:s0"

// SystemExecLabel is the context to exec containerd in so that a `stratum up`
// started from a login shell runs it as systemd would, or "" to leave it as
// inherited: an unlabelled node (SELinux off, opted out, contexts not
// registered) or a process already running as system_u. It does not ask for
// container_runtime_t in this process: the start that first labels the node
// keeps the domain the shell gave it.
func SystemExecLabel() string {
	if !Labelled() {
		return ""
	}
	b, err := os.ReadFile(procCurrent)
	if err != nil {
		return ""
	}
	f := strings.SplitN(strings.TrimRight(string(b), "\x00\n"), ":", 4)
	if len(f) != 4 || f[0] == "system_u" {
		return ""
	}
	return systemRuntimeContext
}

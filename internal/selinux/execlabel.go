package selinux

import (
	"os"
	"strings"
)

// a var so a test can stand in for it.
var procCurrent = "/proc/self/attr/current"

// the context containerd runs in when systemd starts Stratum.
const systemRuntimeContext = "system_u:system_r:container_runtime_t:s0"

// the context to exec containerd in, or "" on an unlabelled node or system_u process.
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

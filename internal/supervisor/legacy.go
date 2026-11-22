package supervisor

import (
	"bytes"
	"debug/buildinfo"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/ctrliq/stratum/internal/procid"
)

// Older supervisors can unlink the socket; the scan misses any started after it: stop old first.
func refuseLegacySupervisor() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	pid, err := findLegacySupervisor("/proc", os.Getpid(), procid.BuildInfo)
	if err != nil {
		return fmt.Errorf("supervisor: cannot verify legacy supervisor exit: %w", err)
	}
	if pid != 0 {
		return fmt.Errorf("supervisor: another Stratum up process (PID %d) is still alive; wait for it to exit completely before starting this version", pid)
	}
	return nil
}

// self and its forked children read as self until their exec completes, so neither is reported.
func findLegacySupervisor(procRoot string, self int, readInfo func(string) (*buildinfo.BuildInfo, error)) (int, error) {
	var found int
	err := procid.Walk(procRoot, func(p procid.Process) (bool, error) {
		if p.PID == self || !procid.IsUpCommand(p.Argv) || parentPID(procRoot, p.PID) == self {
			return true, nil
		}
		info, err := readInfo(filepath.Join(procRoot, strconv.Itoa(p.PID), "exe"))
		if procid.Exited(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if procid.IsStratumBuild(info) {
			found = p.PID
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return 0, err
	}
	return found, nil
}

// Scanned from the closing ')': the command name may hold spaces and parentheses.
func parentPID(procRoot string, pid int) int {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	if len(f) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return 0
	}
	return ppid
}

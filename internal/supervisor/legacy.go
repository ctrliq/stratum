package supervisor

import (
	"debug/buildinfo"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/ctrliq/stratum/internal/procid"
)

// Older supervisors do not take our locks and can unlink the socket after their
// listener closes. On Linux, refuse handoff until their parent process exits.
// This cannot coordinate an old binary launched AFTER the check: migration must
// stop old versions first, then start the new one. Other platforms are used only
// for development; Stratum nodes run Linux.
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

// findLegacySupervisor walks procRoot through procid and returns the pid of
// the top-level `stratum up` process — identity by subcommand plus build
// information: the candidate must run the up subcommand, and its executable
// must be built from the Stratum module. self is never reported. A candidate
// that exits mid-scan is skipped; a candidate whose binary cannot be inspected
// fails the scan, so a start never proceeds on an unverified handoff.
func findLegacySupervisor(procRoot string, self int, readInfo func(string) (*buildinfo.BuildInfo, error)) (int, error) {
	var found int
	err := procid.Walk(procRoot, func(p procid.Process) (bool, error) {
		if p.PID == self || !procid.IsUpCommand(p.Argv) {
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

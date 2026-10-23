package comps

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ctrliq/stratum/internal/version"
)

// RunningSupervisor is the process serving the control socket: its pid, the
// binary it runs as /proc reports it, and whether that is the stratum
// installed on this node.
type RunningSupervisor struct {
	PID       int
	Exe       string
	Installed bool
}

// Stale reports a supervisor that is not running the installed stratum: an
// upgrade replaced the binary under it, so it runs the previous build while
// the processes it starts run the new one.
func (r RunningSupervisor) Stale() bool { return !r.Installed }

// Supervisor finds the process serving the control socket from the socket's
// peer credentials, which every supervisor build has, and compares the binary
// it runs with version.StratumInstallPath.
func Supervisor() (RunningSupervisor, error) {
	pid, err := socketPeerPID(version.ControlSocket)
	if err != nil {
		return RunningSupervisor{}, fmt.Errorf("find the supervisor's pid: %w", err)
	}
	exe, err := procExe(pid)
	if err != nil {
		return RunningSupervisor{}, fmt.Errorf("read the supervisor's binary: %w", err)
	}
	running, err := os.Stat(procExePath(pid))
	if err != nil {
		return RunningSupervisor{}, fmt.Errorf("stat the supervisor's binary: %w", err)
	}
	installed, err := os.Stat(version.StratumInstallPath)
	if err != nil {
		return RunningSupervisor{}, fmt.Errorf("stat the installed stratum: %w", err)
	}
	same := os.SameFile(running, installed)
	if !same && running.Size() == installed.Size() {
		same, err = sameContent(procExePath(pid), version.StratumInstallPath)
		if err != nil {
			return RunningSupervisor{}, fmt.Errorf("compare the supervisor's binary with the installed stratum: %w", err)
		}
	}
	return RunningSupervisor{PID: pid, Exe: exe, Installed: same}, nil
}

// sameContent reports whether two files hold the same bytes. A rollback
// restores the build a supervisor already runs as a new file, which is the
// same build under another inode.
func sameContent(a, b string) (bool, error) {
	ha, err := fileSHA256(a)
	if err != nil {
		return false, err
	}
	hb, err := fileSHA256(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func fileSHA256(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// socketPeerPID and procExePath are seams over the control socket's peer
// credentials and the /proc path of a process's binary, so Supervisor is
// testable without a running supervisor.
var (
	socketPeerPID = peerPID
	procExePath   = func(pid int) string { return filepath.Join("/proc", strconv.Itoa(pid), "exe") }
)

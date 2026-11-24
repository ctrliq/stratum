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

type RunningSupervisor struct {
	PID       int
	Exe       string
	Installed bool
}

// It runs the previous build while the processes it starts run the new one.
func (r RunningSupervisor) Stale() bool { return !r.Installed }

// Peer credentials work with every supervisor build.
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

// A rollback restores the same build as a new file, under another inode.
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

// Seams so Supervisor is testable without a running supervisor.
var (
	socketPeerPID = peerPID
	procExePath   = func(pid int) string { return filepath.Join("/proc", strconv.Itoa(pid), "exe") }
)

package apparmor

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

// Stops the child at execve, before any of runc's code runs, and reads its label.
func probeRuncLabel() (string, error) {
	// The tracer is the thread that started the child.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cmd := exec.Command(runcPath(), "--version")
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	var ws syscall.WaitStatus
	for {
		_, err := syscall.Wait4(cmd.Process.Pid, &ws, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("wait for it to stop: %w", err)
		}
		break
	}
	if !ws.Stopped() {
		return "", fmt.Errorf("it did not stop after exec (wait status %#x)", uint32(ws))
	}
	return readLabel(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid)))
}

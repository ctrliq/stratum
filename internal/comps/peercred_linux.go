package comps

import (
	"errors"
	"net"
	"syscall"
	"time"
)

// peerPID is the pid of the process listening on the unix socket at path,
// from SO_PEERCRED: the kernel records it at listen time, so it needs nothing
// from the server.
func peerPID(path string) (int, error) {
	c, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return int(cred.Pid), nil
}

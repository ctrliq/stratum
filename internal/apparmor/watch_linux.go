package apparmor

import (
	"context"
	"syscall"
)

// revisionPath is the kernel's AppArmor policy revision; a var so a test can
// stand in for it.
var revisionPath = "/sys/kernel/security/apparmor/revision"

// WatchPolicy sends on the channel it returns each time the kernel's AppArmor
// policy changes, as when any profile is loaded, replaced or removed, after
// the call and until ctx ends. Changes that come faster than they are received
// are sent once. A kernel without the revision file never sends. The kernel
// marks the file readable once the revision moves past the one its reader
// last read.
func WatchPolicy(ctx context.Context) <-chan struct{} {
	fd, err := syscall.Open(revisionPath, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil
	}
	buf := make([]byte, 64)
	// Reading marks the current revision as seen, before the caller can
	// change it.
	_, _ = syscall.Read(fd, buf)
	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err == nil {
		err = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)})
		if err != nil {
			_ = syscall.Close(epfd)
		}
	}
	if err != nil {
		_ = syscall.Close(fd)
		return nil
	}
	changed := make(chan struct{}, 1)
	go func() {
		defer syscall.Close(fd)
		defer syscall.Close(epfd)
		events := make([]syscall.EpollEvent, 1)
		for ctx.Err() == nil {
			n, err := syscall.EpollWait(epfd, events, 1000)
			if err != nil && err != syscall.EINTR {
				return
			}
			if n <= 0 {
				continue
			}
			if _, err := syscall.Read(fd, buf); err == syscall.EAGAIN {
				continue
			}
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}()
	return changed
}

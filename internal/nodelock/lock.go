// Package nodelock excludes concurrent node-maintenance operations on one host.
package nodelock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/ctrliq/stratum/internal/version"
)

// Acquire takes the node maintenance lock for op and refuses rather than waits when held.
func Acquire(op string) (release func(), err error) {
	f, err := os.OpenFile(version.NodeUpgradeLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("stratum %s: node maintenance lock: %w", op, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(version.NodeUpgradeLockPath)
		_ = f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("stratum %s: node maintenance lock: %w", op, err)
		}
		by := "another process"
		holding := "another operation"
		if h, ok := readHolder(holder); ok {
			by = fmt.Sprintf("pid %d", h.pid)
			if h.op != "" {
				holding = h.op
			}
		}
		return nil, fmt.Errorf("stratum %s: %s is already running on this node (%s holds %s); nothing was changed",
			op, holding, by, version.NodeUpgradeLockPath)
	}
	// Best effort: the holder only names a refusal; the lock is held either way.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(op+" "+strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	// A second release must not unlock an fd number another Acquire may have reused.
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}

// holder is what a lock file records; op is empty for a legacy pid-only holder.
type holder struct {
	op  string
	pid int
}

func readHolder(contents []byte) (holder, bool) {
	fields := strings.Fields(string(contents))
	if len(fields) == 0 {
		return holder{}, false
	}
	pid, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || pid <= 0 {
		return holder{}, false
	}
	return holder{op: strings.Join(fields[:len(fields)-1], " "), pid: pid}, true
}

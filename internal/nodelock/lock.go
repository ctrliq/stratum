// Package nodelock excludes concurrent node-maintenance operations — upgrade,
// rollback, certificate rotation, etcd CA migration, snapshot restore, rejoin,
// uninstall — on one host: while one such operation holds the lock, the next
// refuses instead of waiting.
//
// The lock lives at version.NodeUpgradeLockPath (/run/stratum-upgrade.lock),
// the path every released binary already fences at, so a new build and a
// released one refuse each other; moving it needs a compatibility release
// first.
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

// Acquire takes the node maintenance lock for op — an exclusive non-blocking
// flock on version.NodeUpgradeLockPath — and refuses rather than waits when
// another operation holds it. The returned release func unlocks; call it when
// the operation ends.
//
// flock rather than a pid file: the kernel drops it when the holder dies, so a
// SIGKILLed or OOM-killed run never strands the node locked, and /run is tmpfs,
// so a reboot clears the file. The fd is close-on-exec (os.OpenFile's
// default), so a unit the operation restarts cannot inherit the lock and keep
// the node locked after the run exits.
//
// The holder's operation and pid are written into the file, over whatever the
// last holder left, only so a refusal can name them; the lock is held whether
// or not that write succeeds. A file holding only a pid was written by a
// released binary, which recorded no operation; such a holder refuses too,
// naming the pid.
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
	// Best effort: the recorded holder only improves a refusal's message, and
	// the lock is held whether or not that write succeeds.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(op+" "+strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	// once makes release idempotent: a second call is a no-op instead of an
	// LOCK_UN and Close of an already-closed descriptor — and, if that fd
	// number was meanwhile reused by another Acquire in this process, of
	// another holder's lock. Callers release via defer on every exit path; a
	// shared primitive must not turn one stray second call into another
	// operation's unlocked fence.
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}

// holder is what a lock file records about the operation holding it: its
// operation and pid. op is empty when the holder recorded only its pid, the
// way every released binary's lockNode did.
type holder struct {
	op  string
	pid int
}

// readHolder parses a lock file's contents: "op pid", with the pid last so an
// operation name that is more than one word ("upgrade --check") still parses.
// A lone pid is a released binary's file. Anything else — an empty file left
// by a holder killed between flock and write, foreign content — is an unknown
// holder, which a refusal can name only as "another process".
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

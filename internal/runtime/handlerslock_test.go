package runtime

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

func TestLockHandlersWaitsForItsHolder(t *testing.T) {
	old, oldPoll := version.RuntimeHandlersLockPath, handlersLockPoll
	t.Cleanup(func() { version.RuntimeHandlersLockPath, handlersLockPoll = old, oldPoll })
	version.RuntimeHandlersLockPath = filepath.Join(t.TempDir(), "handlers.lock")
	handlersLockPoll = 5 * time.Millisecond

	release, err := LockHandlers(context.Background(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		r, err := LockHandlers(context.Background(), &bytes.Buffer{})
		if err != nil {
			t.Error(err)
		}
		got <- r
	}()
	select {
	case <-got:
		t.Fatal("a second holder took the lock while the first held it")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case r := <-got:
		r()
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter never took the lock after its release")
	}

	release, err = LockHandlers(context.Background(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := LockHandlers(ctx, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("a waiter whose context ended = %v, want the context's error", err)
	}
}

package cluster

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/apparmor"
)

// syncBuffer is a log the loop writes while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The loop reloads on every tick the profile is lost, and logs a failure once
// until it changes or clears, so a host that cannot take the profile does not
// fill the journal.
func TestAppArmorProfileLoopReloadsAndLogsEachChangeOnce(t *testing.T) {
	oldInterval, oldReconcile, oldLog, oldWatch := appArmorProfileInterval, reconcileAppArmor, appArmorLog, watchAppArmorPolicy
	t.Cleanup(func() {
		appArmorProfileInterval, reconcileAppArmor, appArmorLog, watchAppArmorPolicy = oldInterval, oldReconcile, oldLog, oldWatch
	})
	watchAppArmorPolicy = func(context.Context) <-chan struct{} { return nil }

	type result struct {
		loaded apparmor.Reload
		err    error
	}
	results := []result{
		{apparmor.NotLoaded, nil},
		{apparmor.NotLoaded, errors.New("apparmor_parser: syntax error")},
		{apparmor.NotLoaded, errors.New("apparmor_parser: syntax error")},
		{apparmor.NotLoaded, errors.New("apparmor_parser: syntax error")},
		{apparmor.Loaded, nil},
		{apparmor.NotLoaded, errors.New("read the label: permission denied")},
		{apparmor.NotLoaded, nil},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	ticks := 0
	reconcileAppArmor = func(context.Context) (apparmor.Reload, error) {
		mu.Lock()
		defer mu.Unlock()
		if ticks == len(results) {
			cancel()
			return apparmor.NotLoaded, nil
		}
		r := results[ticks]
		ticks++
		return r.loaded, r.err
	}
	log := &syncBuffer{}
	appArmorLog = log
	appArmorProfileInterval = time.Millisecond

	done := make(chan struct{})
	go func() { appArmorProfileLoop(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("appArmorProfileLoop did not stop when its context ended")
	}

	got := log.String()
	if n := strings.Count(got, "syntax error"); n != 1 {
		t.Errorf("logged the same failure %d times, want once:\n%s", n, got)
	}
	for _, want := range []string{"loaded it again", "permission denied", "covers Stratum's runc again"} {
		if !strings.Contains(got, want) {
			t.Errorf("log does not say %q:\n%s", want, got)
		}
	}
}

// A change of the kernel's policy, as a reload of the host's profiles makes,
// is looked at once it settles, without waiting for the interval: containers
// created until the profile is back are stacked for good.
func TestAppArmorProfileLoopLooksWhenThePolicyChanges(t *testing.T) {
	oldInterval, oldReconcile, oldLog, oldWatch, oldSettle := appArmorProfileInterval, reconcileAppArmor, appArmorLog, watchAppArmorPolicy, appArmorSettle
	t.Cleanup(func() {
		appArmorProfileInterval, reconcileAppArmor, appArmorLog, watchAppArmorPolicy, appArmorSettle = oldInterval, oldReconcile, oldLog, oldWatch, oldSettle
	})
	appArmorProfileInterval = time.Hour
	appArmorSettle = time.Millisecond
	appArmorLog = &syncBuffer{}
	changes := make(chan struct{}, 1)
	watchAppArmorPolicy = func(context.Context) <-chan struct{} { return changes }
	looked := make(chan struct{}, 1)
	reconcileAppArmor = func(context.Context) (apparmor.Reload, error) {
		looked <- struct{}{}
		return apparmor.NotLoaded, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { appArmorProfileLoop(ctx); close(done) }()
	changes <- struct{}{}
	select {
	case <-looked:
	case <-time.After(10 * time.Second):
		t.Fatal("appArmorProfileLoop did not look after the policy changed")
	}
	cancel()
	<-done
}

// Every change gets a look, even one that lands just after a look that loaded:
// it may be the host replacing the profile again.
func TestAppArmorProfileLoopLooksAtEveryChange(t *testing.T) {
	oldInterval, oldReconcile, oldLog, oldWatch, oldSettle := appArmorProfileInterval, reconcileAppArmor, appArmorLog, watchAppArmorPolicy, appArmorSettle
	t.Cleanup(func() {
		appArmorProfileInterval, reconcileAppArmor, appArmorLog, watchAppArmorPolicy, appArmorSettle = oldInterval, oldReconcile, oldLog, oldWatch, oldSettle
	})
	appArmorProfileInterval = time.Hour
	appArmorSettle = time.Millisecond
	appArmorLog = &syncBuffer{}
	changes := make(chan struct{}, 1)
	watchAppArmorPolicy = func(context.Context) <-chan struct{} { return changes }
	looked := make(chan struct{}, 2)
	reconcileAppArmor = func(context.Context) (apparmor.Reload, error) {
		looked <- struct{}{}
		return apparmor.Loaded, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { appArmorProfileLoop(ctx); close(done) }()
	for i := range 2 {
		changes <- struct{}{}
		select {
		case <-looked:
		case <-time.After(10 * time.Second):
			t.Fatalf("change %d got no look", i+1)
		}
	}
	cancel()
	<-done
}

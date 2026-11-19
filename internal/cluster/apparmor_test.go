package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/apparmor"
)

// stubAppArmorLoop gives appArmorProfileLoop, until the test ends, a channel
// standing in for the kernel's policy changes, which it returns, an interval
// that never comes, a short settle and no log.
func stubAppArmorLoop(t *testing.T) chan struct{} {
	t.Helper()
	oldInterval, oldReconcile, oldLog, oldWatch, oldSettle := appArmorProfileInterval, reconcileAppArmor, appArmorLog, watchAppArmorPolicy, appArmorSettle
	t.Cleanup(func() {
		appArmorProfileInterval, reconcileAppArmor, appArmorLog, watchAppArmorPolicy, appArmorSettle = oldInterval, oldReconcile, oldLog, oldWatch, oldSettle
	})
	changes := make(chan struct{}, 1)
	watchAppArmorPolicy = func(context.Context) <-chan struct{} { return changes }
	appArmorProfileInterval, appArmorSettle, appArmorLog = time.Hour, time.Millisecond, io.Discard
	return changes
}

// The loop looks on every tick, and logs a failure once until it changes or
// clears, so a host that cannot take the profile does not fill the journal.
func TestAppArmorProfileLoopLogsEachChangeOnce(t *testing.T) {
	stubAppArmorLoop(t)
	var log bytes.Buffer
	appArmorProfileInterval, appArmorLog = time.Millisecond, &log
	type result struct {
		loaded bool
		err    error
	}
	results := []result{
		{false, nil},
		{true, errors.New("apparmor_parser: syntax error")},
		{true, errors.New("apparmor_parser: syntax error")},
		{true, errors.New("apparmor_parser: syntax error")},
		{true, nil},
		{false, errors.New("read the label: permission denied")},
		{false, nil},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reconcileAppArmor = func(context.Context) (bool, error) {
		if len(results) == 0 {
			cancel()
			return false, nil
		}
		r := results[0]
		results = results[1:]
		return r.loaded, r.err
	}
	appArmorProfileLoop(ctx)

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

// The loop looks as soon as it starts, since its watcher counts only later
// changes, and then on every change of the kernel's policy, as a reload of the
// host's profiles makes, once it settles, without waiting for the interval:
// containers created until the profile is back are stacked for good. So is a
// change that lands just after a look that loaded: it may be the host
// replacing the profile again.
func TestAppArmorProfileLoopLooksAtStartAndAtEveryPolicyChange(t *testing.T) {
	changes := stubAppArmorLoop(t)
	looked := make(chan struct{}, 3)
	reconcileAppArmor = func(context.Context) (bool, error) {
		looked <- struct{}{}
		return true, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { appArmorProfileLoop(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case <-looked:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not look when it started")
	}
	for i := range 2 {
		changes <- struct{}{}
		select {
		case <-looked:
		case <-time.After(10 * time.Second):
			t.Fatalf("change %d got no look", i+1)
		}
	}
}

// A load that leaves runc uncovered, as when another profile attaches to it
// first, changes the policy too; the loop must not answer its own change with
// another load, so it waits for the interval.
func TestAppArmorProfileLoopBacksOffALoadThatDoesNotCover(t *testing.T) {
	changes := stubAppArmorLoop(t)
	looks := 0
	reconcileAppArmor = func(context.Context) (bool, error) {
		looks++
		select {
		case changes <- struct{}{}: // the load's own change
		default:
		}
		return true, fmt.Errorf("give it the profile: loaded, but %w", apparmor.ErrNotCovered)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// The look at start loads, and its own change is already pending.
	go func() { appArmorProfileLoop(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if looks != 1 {
		t.Errorf("the loop looked %d times, want once: a load that left runc uncovered must wait for the interval", looks)
	}
}

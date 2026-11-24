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
	go func() { appArmorProfileLoop(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if looks != 1 {
		t.Errorf("the loop looked %d times, want once: a load that left runc uncovered must wait for the interval", looks)
	}
}

package upgrade

import (
	"io"

	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/nodestate"
)

// The per-node Job's deadline holds the upgrade's expected budget plus a window
// for each best-effort step after it, so an upgrade that stays within that
// budget does not reach the deadline during the version stamp or the backstop.
// The budget is an assumption the deadline is sized from, not
// something enforced at runtime: an upgrade that runs past it can still hit the
// deadline in that tail.
func TestJobDeadlineHoldsTheExpectedBudgetAndTheBestEffortTail(t *testing.T) {
	if upgradeWorkBudget != 15*time.Minute {
		t.Errorf("the upgrade's expected budget changed to %s; the Job's deadline is sized from it", upgradeWorkBudget)
	}
	if after := stampWindow + backstopWindow; upgradeWorkBudget+after > upgradeJobDeadline {
		t.Errorf("work %s + stamp %s + backstop %s exceeds the Job deadline %s", upgradeWorkBudget, stampWindow, backstopWindow, upgradeJobDeadline)
	}
	if got := time.Duration(upgradeJobPolls) * 5 * time.Second; got != upgradeJobDeadline {
		t.Errorf("the orchestrator polls for %s (%d x 5s), the Job's deadline is %s", got, upgradeJobPolls, upgradeJobDeadline)
	}
}

// A kubectl the API server never answers cannot hold the Job in front of the
// backstop: the stamp is killed when its window ends and only warns.
func TestStampUpgradedIsBounded(t *testing.T) {
	swap(t, &stampWindow, 50*time.Millisecond)
	swap(t, &stampNode, func(ctx context.Context, _ string) error {
		<-ctx.Done() // never answers
		return ctx.Err()
	})
	start := time.Now()
	out, err := runWriter(t, func(w io.Writer) error {
		stampUpgraded(w, &nodestate.NodeState{Role: "controller", Hostname: "cp-1"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("stampUpgraded held for %s", took)
	}
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "deadline exceeded") {
		t.Errorf("output = %q, want a WARN naming the deadline", out)
	}
}

// And the backstop itself ends within backstopWindow, however the API server
// behaves.
func TestBackstopEndsWithinItsWindow(t *testing.T) {
	swap(t, &backstopWindow, 300*time.Millisecond)
	swap(t, &apiCallTimeout, 50*time.Millisecond)
	swap(t, &apiRetryInterval, 10*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	done, err := runBackstop(cs, "cp1", "v1.37.1")
	if done || err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("done=%v err=%v, want the deadline", done, err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the backstop ran %s, window %s", took, backstopWindow)
	}
}

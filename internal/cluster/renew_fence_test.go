package cluster

// The daily renewal tick's fence. certRenewLoop fires once a day and calls
// certs.Rotate, which restarts control-plane components on this host: while
// another maintenance operation holds the node lock, the tick must skip that
// day's renewal with a log line naming the holder — a skip, not a renewal
// failure — and run again the next day. The tick drives the same
// certs.Rotate the CLI and the MCP tool call, so the refusal's wording is
// theirs; what is pinned here is the loop's skip, which only the tick owns.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/version"
)

// redirectVar points one of the version package's path vars at value for the
// test and restores it afterwards.
func redirectVar(t *testing.T, v *string, value string) {
	t.Helper()
	old := *v
	*v = value
	t.Cleanup(func() { *v = old })
}

// runOneTick advances the fake clock to exactly one renewal tick, waits for
// its work to finish, then cancels and joins the loop. A file captures stderr
// without a pipe buffer that could block the loop under test.
func runOneTick(t *testing.T, interval time.Duration) string {
	t.Helper()
	old := certRenewInterval
	certRenewInterval = interval
	t.Cleanup(func() { certRenewInterval = old })

	capture, err := os.CreateTemp(t.TempDir(), "renewal-stderr")
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = capture
	t.Cleanup(func() { os.Stderr = oldStderr; capture.Close() })
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		startedAt := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			certRenewLoop(ctx, "cp1")
		}()
		synctest.Wait()
		time.Sleep(renewalJitter("cp1", interval))
		synctest.Wait()
		record, err := certs.LoadRenewalRecord()
		if err != nil {
			t.Fatalf("renewal tick did not record its attempt: %v", err)
		}
		if want := startedAt.Add(renewalJitter("cp1", interval)); !record.AttemptedAt.Equal(want) {
			t.Fatalf("renewal attempt at %v, want the first tick at %v", record.AttemptedAt, want)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("certRenewLoop did not return after its context was canceled")
		}
	})

	// The loop has returned, so nothing writes to the capture any more.
	os.Stderr = oldStderr
	if _, err := capture.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(capture)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestCertRenewTickSkipsWhileNodeLockHeld: while an upgrade holds the node,
// the day's renewal is skipped — a log line naming the holder operation, not a
// renewal failure — and the loop lives on for the next day.
func TestCertRenewTickSkipsWhileNodeLockHeld(t *testing.T) {
	t.Setenv("NODE_IP", "192.0.2.10")
	redirectVar(t, &version.PKIDir, t.TempDir()) // nothing to rotate: the only loggable event is the skip
	redirectVar(t, &version.K8sConfDir, t.TempDir())
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	release, err := nodelock.Acquire("upgrade")
	if err != nil {
		t.Fatalf("take the node lock as an upgrade would: %v", err)
	}
	defer release()

	logs := runOneTick(t, 5*time.Millisecond)
	if !strings.Contains(logs, "skip") {
		t.Errorf("the tick did not say it skipped the renewal while the node was held: %q", logs)
	}
	if !strings.Contains(logs, "upgrade") {
		t.Errorf("the skip line does not name the holder operation: %q", logs)
	}
}

// TestCertRenewTickQuietWithoutContention: with the node free, a tick whose
// rotation finds nothing to do produces NO output at all — the skip line exists
// for contention, not as constant noise beside every renewal, and the renewal
// path is silent when it finds nothing to rotate. Asserting emptiness rather
// than the absence of one word is what makes this prove the capability: if the
// tick stopped passing the held-lock capability (or Rotate stopped honoring
// it), the tick would refuse against its own flock and print that refusal
// here — an error that names no "skip" — so logs == "" fails the moment the
// tick's renewal self-refuses.
func TestCertRenewTickQuietWithoutContention(t *testing.T) {
	t.Setenv("NODE_IP", "192.0.2.10")
	redirectVar(t, &version.PKIDir, t.TempDir()) // nothing to rotate
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	// The attempt record writes under the state area; the real /var/lib/stratum
	// is not writable here, so point it at a temp dir — a successful record is
	// silent, exactly what the pin asserts.
	redirectVar(t, &version.K8sConfDir, t.TempDir())

	if logs := runOneTick(t, 5*time.Millisecond); logs != "" {
		t.Errorf("the uncontended tick was not silent (its renewal may be refusing against its own lock): %q", logs)
	}
}

package cluster

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

func redirectVar(t *testing.T, v *string, value string) {
	t.Helper()
	old := *v
	*v = value
	t.Cleanup(func() { *v = old })
}

// runOneTick waits for the tick's work, then cancels; a file captures stderr (a pipe can block).
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

func TestCertRenewTickQuietWithoutContention(t *testing.T) {
	t.Setenv("NODE_IP", "192.0.2.10")
	redirectVar(t, &version.PKIDir, t.TempDir())
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	redirectVar(t, &version.K8sConfDir, t.TempDir()) // renewal records write here, not on the host

	if logs := runOneTick(t, 5*time.Millisecond); logs != "" {
		t.Errorf("the uncontended tick was not silent (its renewal may be refusing against its own lock): %q", logs)
	}
}

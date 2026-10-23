package k8sutil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

// The stamp context must cancel the installed-version probe that is actually
// running. Wait for that probe to start instead of budgeting process startup
// against a short wall-clock deadline, which is unreliable on busy CI runners.
func TestReviewStampContextBoundsVersionProbes(t *testing.T) {
	for _, slow := range []string{"kubelet", "stratum"} {
		t.Run(slow, func(t *testing.T) {
			dir := t.TempDir()
			oldBin, oldPath := version.BinDir, stratumPath
			version.BinDir, stratumPath = dir, filepath.Join(dir, "stratum")
			t.Cleanup(func() { version.BinDir, stratumPath = oldBin, oldPath })
			started := filepath.Join(dir, "probe-started")
			t.Setenv("STRATUM_TEST_PROBE_STARTED", started)
			for name, output := range map[string]string{"kubelet": "Kubernetes v1.37.1", "stratum": "v1.37.1-stratum.0 linux/arm64", "kubectl": ""} {
				body := "printf '%s\\n' '" + output + "'\n"
				if name == slow {
					// exec lets cancellation kill the sleeper itself, leaving
					// no child process behind after the test.
					body = "printf x > \"$STRATUM_TEST_PROBE_STARTED\"\nexec sleep 30\n"
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- StampNodeContext(ctx, "cp1") }()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			timeout := time.NewTimer(5 * time.Second)
			defer timeout.Stop()
		waitForProbe:
			for {
				select {
				case err := <-done:
					t.Fatalf("stamp returned before %s probe started: %v", slow, err)
				case <-timeout.C:
					t.Fatalf("%s probe did not start", slow)
				case <-ticker.C:
					if _, err := os.Stat(started); err == nil {
						break waitForProbe
					} else if !os.IsNotExist(err) {
						t.Fatal(err)
					}
				}
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("stamp succeeded after canceling its %s probe", slow)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s version probe did not stop with the stamp context", slow)
			}
		})
	}
}

// A failing stamp's error carries kubectl's stderr, trimmed and capped.
func TestStampNodeContextIncludesKubectlStderr(t *testing.T) {
	long := strings.Repeat("x", 300)
	for _, tc := range []struct {
		name, stderr, want, notWant string
	}{
		{"cause", "  Error from server (NotFound): nodes \"cp1\" not found  ", `Error from server (NotFound): nodes "cp1" not found`, ""},
		{"capped", long, strings.Repeat("x", maxStderr) + "...", strings.Repeat("x", maxStderr+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			oldBin, oldPath := version.BinDir, stratumPath
			version.BinDir, stratumPath = dir, filepath.Join(dir, "stratum")
			t.Cleanup(func() { version.BinDir, stratumPath = oldBin, oldPath })
			for name, body := range map[string]string{
				"kubelet": "printf 'Kubernetes v1.37.1\\n'",
				"stratum": "printf 'v1.37.1-stratum.0 linux/arm64\\n'",
				"kubectl": "printf '%s\\n' '" + tc.stderr + "' >&2\nexit 1",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			err := StampNodeContext(t.Context(), "cp1")
			if err == nil {
				t.Fatal("stamp succeeded against a failing kubectl")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("err = %q, stderr not capped", err)
			}
			if !strings.Contains(err.Error(), "exit status 1") {
				t.Errorf("err = %q, lost the exit status", err)
			}
		})
	}
}

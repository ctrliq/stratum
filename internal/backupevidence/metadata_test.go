package backupevidence

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

func TestReaderRejectsUnsafeMetadata(t *testing.T) {
	for _, input := range []string{"attempts", "node state", "cluster config"} {
		for _, kind := range []string{"fifo", "oversized"} {
			t.Run(input+"/"+kind, func(t *testing.T) {
				withEvidencePaths(t)
				noSnapshotConf(t)
				if err := os.MkdirAll(version.K8sConfDir, 0o700); err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				writeSnapshot(t, dir, "kept.db", 10, snapTime(1))
				config := filepath.Join(t.TempDir(), "cluster.yaml")
				if err := os.WriteFile(config, []byte("spec:\n  snapshots:\n    dir: "+dir+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := nodestate.SaveState(&nodestate.NodeState{ConfigPath: config}); err != nil {
					t.Fatal(err)
				}
				path := EvidencePath()
				if input == "node state" {
					path = nodestate.StatePath()
				} else if input == "cluster config" {
					path = config
				}
				if kind == "fifo" {
					if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					if err := syscall.Mkfifo(path, 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(path, bytes.Repeat([]byte("# padding\n"), 210000), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan EvidenceResult, 1)
				go func() {
					done <- Evidence(context.Background(), EvidenceOptions{Node: "cp-1", MaxTime: 100 * time.Millisecond})
				}()
				var res EvidenceResult
				select {
				case res = <-done:
				case <-time.After(500 * time.Millisecond):
					t.Error("metadata read blocked past the evidence deadline")
					// Release the old blocking implementation so a failing test
					// leaves no reader behind.
					if kind == "fifo" {
						f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
						if err != nil {
							t.Fatal(err)
						}
						_, _ = f.Write([]byte("{}\n"))
						_ = f.Close()
					}
					res = <-done
				}
				if res.Complete {
					t.Error("unsafe metadata was reported as complete evidence")
				}
				if input == "attempts" {
					if res.Maintenance.AttemptsKnown || !res.Local.Complete || len(res.Local.Artifacts) != 1 {
						t.Errorf("unreadable attempts should preserve usable local evidence: %+v", res)
					}
				} else if res.Policy.State != StateUnknown || res.Remote.Complete {
					t.Errorf("unreadable configuration should leave policy and remote totals unknown: %+v", res)
				}
			})
		}
	}
}

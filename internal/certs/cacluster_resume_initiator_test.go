package certs

import (
	"io"
	"strings"
	"testing"
)

// An untouched replacement initiator has none of the run's original staged
// roots. Manual progress on another node must not make it generate new ones.
func TestClusterResumeRefusesUntouchedInitiatorBeforeFirstCheckpoint(t *testing.T) {
	for _, history := range []string{"first rotation", "after completed rotation"} {
		t.Run(history, func(t *testing.T) {
			f := newFakeCACluster(t, "cp1", "cp2", "cp3")
			if history == "after completed rotation" {
				if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
					t.Fatal(err)
				}
			}
			f.failAt["cp2:"+PhaseTrustNew] = true
			if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err == nil {
				t.Fatal("expected initial trust-new halt")
			}
			f.in("cp1", func() {
				if err := cpSignNew(io.Discard, CARotateOptions{Phase: PhaseSignNew, Quiet: true, SkipWorkloadCheck: true}); err != nil {
					t.Fatal(err)
				}
			})
			delete(f.failAt, "cp2:"+PhaseTrustNew)
			f.order[0], f.order[1] = f.order[1], f.order[0]
			before := len(f.calls)
			err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run()
			if err == nil || !strings.Contains(err.Error(), "no material for the recorded CA rotation") {
				t.Fatalf("resume from untouched cp2 = %v, want refusal before generating another root", err)
			}
			if len(f.calls) != before {
				t.Fatalf("refused resume dispatched phases: %v", f.calls[before:])
			}
		})
	}
}

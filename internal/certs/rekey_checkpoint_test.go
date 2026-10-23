package certs

import (
	"context"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
)

func TestRekeyRefusesARecordedPartialFinalizeWithoutAStagedKey(t *testing.T) {
	f := newFakeCACluster(t, "cp1", "cp2")
	f.failAt["cp2:finalize"] = true
	if err := (&caCluster{drv: f}).run(); err == nil {
		t.Fatal("expected partial-finalize halt")
	}
	stubStagedKey(t, func() (bool, error) { return rekeyMaterialInFlight(context.Background(), f.cs) })
	f.in("cp1", func() {
		if !pki.CAFinalized() || RotationPhase() != "idle" {
			t.Fatal("initiator must already be finalized")
		}
		err := GuardRekey()
		if err == nil || !strings.Contains(err.Error(), "rotation") {
			t.Fatalf("mint allowed during recorded partial finalize: %v", err)
		}
		// Nothing is running, so the refusal must name the ways out: the
		// resume, and the record to clear once a rollback has succeeded.
		for _, want := range []string{"rotate-ca --cluster --yes", "delete configmap stratum-ca-rotation"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q does not name %q", err, want)
			}
		}
		if strings.Contains(err.Error(), "Wait for it to finish") {
			t.Errorf("refusal %q tells the operator to wait for a run that is not running", err)
		}
	})
	delete(f.failAt, "cp2:finalize")
	if err := (&caCluster{drv: f}).run(); err != nil {
		t.Fatal(err)
	}
	f.in("cp1", func() {
		if err := GuardRekey(); err != nil {
			t.Fatalf("completed rotation still blocks mint: %v", err)
		}
	})
}

package cluster

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/version"
)

func TestCISKeyMintStopsAfterLosingMaintenanceLeaseDuringPreparation(t *testing.T) {
	assemblyFixture(t)
	guard := &maintlease.Guard{}
	oldAssembly := assembleNode
	t.Cleanup(func() { assembleNode = oldAssembly })
	assembleNode = func(_ context.Context, a ha.NodeAssembly, _ ha.AssembleRequest) (host.NodeIPChoice, error) {
		// Host preparation can outlast a lease. Its successor must fence the
		// later key write, even though the CLI acquired before preparation.
		guard.Observe(maintlease.ErrTaken, time.Now())
		return host.NodeIPChoice{}, a.WriteProfileConfigs()
	}
	_, err := PrepareControllerWithLease(context.Background(), &ClusterConfig{Spec: Spec{CIS: true}}, "", &k8sutil.MaintenanceLease{Guard: guard}, nil)
	if !errors.Is(err, maintlease.ErrTaken) {
		t.Fatalf("lost lease must refuse CIS key mutation: %v", err)
	}
	if _, err := os.Stat(version.EncryptionConfigPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lost lease wrote the at-rest key: %v", err)
	}
}

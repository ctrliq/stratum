package certs

import (
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

func prep(t *testing.T) {
	t.Helper()
	version.PKIDir = t.TempDir()
	t.Setenv("NODE_IP", "10.0.0.9") // host.NodeIP() returns $NODE_IP if set
	if err := pki.GenerateAll("cp1", "10.0.0.9", nil); err != nil {
		t.Fatalf("GenerateAll: %v", err)
	}
	// The code under test looks this node up by host.NodeName, which refuses
	// a hostname no Node can take: such a dev host skips rather than fails.
	if _, err := host.NodeName(); err != nil {
		t.Skip("no usable hostname: ", err)
	}
}

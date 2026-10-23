package certs

import (
	"os"
	"testing"

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
	if _, err := os.Hostname(); err != nil {
		t.Skip("no hostname")
	}
}

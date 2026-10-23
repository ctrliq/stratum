package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workerConfig returns spec.network.nodeIP as the file gives it, or empty when
// the file gives none. --node-ip outranks it in cluster.PrepareWorker, which
// is handed both and names the one it took.
func TestWorkerConfigReadsTheNodeIP(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("spec:\n  network:\n    nodeIP: 10.50.0.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(t.TempDir(), "absent.yaml")
	for _, tc := range []struct{ name, path, want string }{
		{"config", cfg, "10.50.0.4"},
		{"no config", absent, ""},
		{"empty --config", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := workerConfig(tc.path)
			if err != nil || got.NodeIP != tc.want {
				t.Fatalf("workerConfig = %+v, %v; want node IP %q, nil", got, err, tc.want)
			}
		})
	}
}

// A field a worker cannot honour is refused, so the file never looks applied
// when it was not. runUp reads the file even when --node-ip is given
// (TestRunUpWorkerJoinReadsTheConfig).
func TestWorkerNodeIPRefusesInapplicableFields(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfg, []byte("spec:\n  network:\n    nodeIP: 10.50.0.4\n  vip:\n    address: 10.50.0.100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := workerConfig(cfg); err == nil || !strings.Contains(err.Error(), "spec.vip.address") {
		t.Errorf("workerConfig = %+v, %v; want an error naming spec.vip.address", c, err)
	}
}

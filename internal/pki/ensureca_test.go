package pki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func TestEnsureCARefusesIncompletePairWithoutOverwriting(t *testing.T) {
	for _, keep := range []string{"crt", "key"} {
		t.Run(keep, func(t *testing.T) {
			original := version.PKIDir
			version.PKIDir = t.TempDir()
			t.Cleanup(func() { version.PKIDir = original })
			if _, err := NewCA("ca", "stratum-ca"); err != nil {
				t.Fatal(err)
			}
			keptPath := filepath.Join(version.PKIDir, "ca."+keep)
			before, err := os.ReadFile(keptPath)
			if err != nil {
				t.Fatal(err)
			}
			missing := "key"
			if keep == "key" {
				missing = "crt"
			}
			if err := os.Remove(filepath.Join(version.PKIDir, "ca."+missing)); err != nil {
				t.Fatal(err)
			}
			if _, err := EnsureCA("ca", "stratum-ca"); err == nil || !strings.Contains(err.Error(), "incomplete") {
				t.Fatalf("accepted partial CA pair: %v", err)
			}
			after, err := os.ReadFile(keptPath)
			if err != nil || string(after) != string(before) {
				t.Fatalf("partial CA material changed: %v", err)
			}
		})
	}
}

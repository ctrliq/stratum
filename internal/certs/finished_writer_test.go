package certs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func TestFinishedRotationMarkerWarningsUseOperationWriter(t *testing.T) {
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	// A nonempty directory prevents both writing and removing the marker.
	if err := os.Mkdir(carotateFinishedRunMarker(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(carotateFinishedRunMarker(), "child"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"record", "remove"} {
		t.Run(operation, func(t *testing.T) {
			var out bytes.Buffer
			stdout := captureRealStdout(t, func() {
				if operation == "record" {
					markCarotateFinishedRun(&out, "run")
				} else {
					clearCarotateFinishedRun(&out)
				}
			})
			if stdout != "" {
				t.Errorf("warning escaped operation writer: %q", stdout)
			}
			if !strings.Contains(out.String(), "WARN:") {
				t.Errorf("operation output lacks warning: %q", out.String())
			}
		})
	}
}

func TestFinishedRotationMarkerReplacesPreviousIdentityAtomically(t *testing.T) {
	old := version.PKIDir
	version.PKIDir = t.TempDir()
	t.Cleanup(func() { version.PKIDir = old })
	var out bytes.Buffer
	markCarotateFinishedRun(&out, "first-run")
	reader := filepath.Join(version.PKIDir, "previous-identity")
	if err := os.Link(carotateFinishedRunMarker(), reader); err != nil {
		t.Fatal(err)
	}
	markCarotateFinishedRun(&out, "next-run")
	if b, err := os.ReadFile(reader); err != nil || string(b) != "first-run\n" {
		t.Fatalf("previous identity changed in place: %q, %v", b, err)
	}
	if got := readCarotateFinishedRun(); got != "next-run" {
		t.Fatalf("finished identity = %q, want next-run", got)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected warning: %s", out.String())
	}
}

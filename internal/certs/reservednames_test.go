package certs

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

func TestRotationRunMarkersAreReservedFromProfileSecrets(t *testing.T) {
	for _, m := range []string{carotateRunMarker(), carotateFinishedRunMarker()} {
		if name := filepath.Base(m); !strings.HasPrefix(name, profile.ReservedSecretPrefix) {
			t.Errorf("rotation marker %q does not start with %q, so a profile secret could take its name", name, profile.ReservedSecretPrefix)
		}
	}
}

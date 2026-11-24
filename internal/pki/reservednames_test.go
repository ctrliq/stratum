package pki

import (
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

// profile.Validate refuses a shared secret named like a rotation marker.
func TestRotationMarkersAreReservedFromProfileSecrets(t *testing.T) {
	for _, m := range []string{caPromotedMarker, caFinalizedMarker, caForcedMarker} {
		if !strings.HasPrefix(m, profile.ReservedSecretPrefix) {
			t.Errorf("rotation marker %q does not start with %q, so a profile secret could take its name", m, profile.ReservedSecretPrefix)
		}
	}
}

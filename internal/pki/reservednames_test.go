package pki

import (
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

// Every CA-rotation marker pki keeps in the PKI directory starts with
// profile.ReservedSecretPrefix, which profile.Validate refuses as a shared
// secret's name: a profile secret named like one would be the rotation's
// state, minted, shared and removed by its cleanup.
func TestRotationMarkersAreReservedFromProfileSecrets(t *testing.T) {
	for _, m := range []string{caPromotedMarker, caFinalizedMarker, caForcedMarker} {
		if !strings.HasPrefix(m, profile.ReservedSecretPrefix) {
			t.Errorf("rotation marker %q does not start with %q, so a profile secret could take its name", m, profile.ReservedSecretPrefix)
		}
	}
}

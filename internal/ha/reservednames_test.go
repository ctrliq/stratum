package ha

import (
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

func TestTheEncryptionConfigEntryIsTheNameReservedForTheCISProfile(t *testing.T) {
	if encryptionConfigEntry != profile.CISEncryptionConfigName {
		t.Errorf("encryptionConfigEntry = %q, profile.CISEncryptionConfigName = %q; the upload's proof and the profile rule name different files", encryptionConfigEntry, profile.CISEncryptionConfigName)
	}
}

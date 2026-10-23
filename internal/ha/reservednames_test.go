package ha

import (
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

// The bundle entry the upload proves an at-rest key from is the one name
// profile.Validate reserves for the cis profile: a secret of that name from
// another profile would be minted and then fail every upload with "names no
// secretbox key", so the two spellings must stay one.
func TestTheEncryptionConfigEntryIsTheNameReservedForTheCISProfile(t *testing.T) {
	if encryptionConfigEntry != profile.CISEncryptionConfigName {
		t.Errorf("encryptionConfigEntry = %q, profile.CISEncryptionConfigName = %q; the upload's proof and the profile rule name different files", encryptionConfigEntry, profile.CISEncryptionConfigName)
	}
}

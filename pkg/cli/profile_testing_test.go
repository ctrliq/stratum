package cli

import (
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

func registerCISForTest(t *testing.T) {
	t.Helper()
	registerProfilesForTest(t, profile.Profile{
		Name:                      "cis",
		RequiresOperator:          true,
		RequiresSELinux:           true,
		KubeletServerTLSBootstrap: true,
		DisableNodeShell:          true,
		SharedSecrets: []profile.Secret{{Name: filepath.Base(version.EncryptionConfigPath),
			Mint: func() ([]byte, error) { return []byte("key"), nil }}},
	})
}

func registerProfilesForTest(t *testing.T, ps ...profile.Profile) {
	t.Helper()
	prev := profile.All()
	if err := profile.SetRegistered(ps); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

func isolateProfileRegistryForTest(t *testing.T) {
	t.Helper()
	prev := profile.All()
	t.Cleanup(func() {
		profile.SetRegistrationError(nil)
		_ = profile.SetRegistered(prev)
	})
}

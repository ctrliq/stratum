package cli

import (
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// registerCISForTest registers a cis profile with the properties the engine's
// own registration declares, for tests that select it without going through
// NewRootCommand, and puts back the registry it replaced when the test
// ends.
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

// registerProfilesForTest makes ps the binary's registered profiles for the
// test and puts back the registry it replaced when the test ends.
func registerProfilesForTest(t *testing.T, ps ...profile.Profile) {
	t.Helper()
	prev := profile.All()
	if err := profile.SetRegistered(ps); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

// isolateProfileRegistryForTest puts back the registered profiles and clears
// any registration error when the test ends, for tests that run a root
// command, whose hook registers the profiles it was given and records why
// they could not be registered.
func isolateProfileRegistryForTest(t *testing.T) {
	t.Helper()
	prev := profile.All()
	t.Cleanup(func() {
		profile.SetRegistrationError(nil)
		_ = profile.SetRegistered(prev)
	})
}

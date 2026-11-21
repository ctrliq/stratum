package cluster

import (
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

// Registers the engine's cis profile for tests that skip NewRootCommand.
func registerCISForTest(t *testing.T) {
	t.Helper()
	registerProfilesForTest(t, profile.Profile{
		Name:                      "cis",
		RequiresOperator:          true,
		RequiresSELinux:           true,
		KubeletServerTLSBootstrap: true,
		DisableNodeShell:          true,
	})
}

// Makes ps the binary's registered profiles for the test.
func registerProfilesForTest(t *testing.T, ps ...profile.Profile) {
	t.Helper()
	prev := profile.All()
	if err := profile.SetRegistered(ps); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

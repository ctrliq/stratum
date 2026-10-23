package cluster

import (
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
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

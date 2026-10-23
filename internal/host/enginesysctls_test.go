package host

import (
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
)

func TestTheEnginesSysctlsAreReservedFromProfiles(t *testing.T) {
	for _, line := range strings.Split(strings.TrimSpace(renderSysctls()), "\n") {
		key, _, _ := strings.Cut(line, "=")
		if !profile.EngineOwnsSysctl(key) {
			t.Errorf("99-stratum.conf sets %q, which a profile may still declare and silently lose", key)
		}
	}
	for _, line := range profiletest.Sysctls {
		key, _, _ := strings.Cut(line, "=")
		if profile.EngineOwnsSysctl(key) {
			t.Errorf("the cis profile's sysctl %q is reserved for the engine", key)
		}
	}
}

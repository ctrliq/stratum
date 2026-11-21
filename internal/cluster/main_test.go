package cluster

// Stubs the host seams package-wide: a test that reached the host would depend on its environment.
import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/ctrliq/stratum/internal/apparmor"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
)

func TestMain(m *testing.M) {
	if err := os.Unsetenv("NODE_IP"); err != nil {
		fmt.Fprintf(os.Stderr, "cluster tests: clear $NODE_IP: %v\n", err)
		os.Exit(1)
	}
	hostDetectNodeIPs = func(dualStack bool) (string, error) {
		if dualStack {
			return "192.0.2.1,2001:db8::1", nil
		}
		return "192.0.2.1", nil
	}
	hostDirs = func() error { return nil }
	hostTrustPodCIDRs = func(string) {}
	hostPrereqs = func(host.PrereqOptions) error { return nil }
	watchAppArmorPolicy = func(context.Context) <-chan struct{} { return nil }
	reconcileAppArmor = func(context.Context) (bool, error) { return false, nil }
	checkAppArmor = func() error { return nil }
	stackedAppArmor = func() ([]apparmor.StackedProcess, error) { return nil, nil }
	if err := profile.SetRegistered([]profile.Profile{profiletest.Profile()}); err != nil {
		fmt.Fprintf(os.Stderr, "cluster tests: register the cis profile: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// Compared by identity, not called: the real host.Dirs would create dirs outside any temp dir.
func TestTestMainStubsTheHostSeams(t *testing.T) {
	for _, s := range []struct {
		name       string
		seam, real any
	}{
		{"hostDetectNodeIPs", hostDetectNodeIPs, host.DetectNodeIPs},
		{"hostTrustPodCIDRs", hostTrustPodCIDRs, host.TrustPodCIDRs},
		{"hostPrereqs", hostPrereqs, host.Prereqs},
		{"hostDirs", hostDirs, host.Dirs},
		{"watchAppArmorPolicy", watchAppArmorPolicy, apparmor.WatchPolicy},
		{"reconcileAppArmor", reconcileAppArmor, apparmor.Reconcile},
		{"checkAppArmor", checkAppArmor, apparmor.Check},
		{"stackedAppArmor", stackedAppArmor, apparmor.Stacked},
	} {
		if reflect.ValueOf(s.seam).Pointer() == reflect.ValueOf(s.real).Pointer() {
			t.Errorf("%s is still the real host function; TestMain must stub it for the whole package", s.name)
		}
	}
}

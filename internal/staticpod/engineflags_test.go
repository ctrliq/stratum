package staticpod

import (
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/enginesettings"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
)

func TestTheEngineAPIServerFlagsAreReservedFromProfiles(t *testing.T) {
	st := &nodestate.NodeState{
		Role: "controller", Hostname: "cp1", NodeIP: "192.0.2.10",
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12",
	}
	pki := func(f string) string { return "/pki/" + f }
	for _, arg := range apiserverArgs(st, profile.Profile{}, pki) {
		name, _, _ := strings.Cut(arg, "=")
		if !profile.EngineOwnsAPIServerFlag(name) {
			t.Errorf("the engine renders %q, which a profile may still declare and so replace", name)
		}
	}
	if name, _, _ := strings.Cut(enginesettings.ArgvAnchor, "="); !profile.EngineOwnsAPIServerFlag(name) {
		t.Errorf("the argv anchor %q is not reserved", name)
	}
	for _, f := range profiletest.Profile().APIServerFlags {
		if profile.EngineOwnsAPIServerFlag(f.Name) {
			t.Errorf("the cis profile's flag %q is reserved for the engine", f.Name)
		}
	}
}

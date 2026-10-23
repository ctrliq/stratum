package host

import (
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/selinux"
)

func TestSELinuxOptionsCarryTheNodesProfile(t *testing.T) {
	o := PrereqOptions{Profile: profile.Profile{Name: "edge", RequiresSELinux: true}, SELinuxOptOut: true, Existing: true}
	want := selinux.Options{RequiresSELinux: true, Profile: "edge", OptOut: true, Existing: true}
	if got := selinuxOptions(o); got != want {
		t.Errorf("selinuxOptions = %+v, want %+v", got, want)
	}
	if got := selinuxOptions(PrereqOptions{}); got != (selinux.Options{}) {
		t.Errorf("selinuxOptions without a profile = %+v, want none", got)
	}
}

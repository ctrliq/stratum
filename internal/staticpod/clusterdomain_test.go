package staticpod

import (
	"slices"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
)

// The apiserver's issuer and audience follow spec.network.clusterDomain.
func TestAPIServerArgsFollowTheClusterDomain(t *testing.T) {
	st := testNode(t)
	pki := func(f string) string { return "/pki/" + f }
	for _, tc := range []struct{ domain, want string }{
		{"", netdefault.ClusterDomain},
		{"fuzz.internal", "fuzz.internal"},
	} {
		st.ClusterDomain = tc.domain
		args := apiserverArgs(st, profiletest.For(st.CIS), pki)
		for _, flag := range []string{"--service-account-issuer", "--api-audiences"} {
			if want := flag + "=https://kubernetes.default.svc." + tc.want; !slices.Contains(args, want) {
				t.Errorf("clusterDomain %q: missing %s", tc.domain, want)
			}
		}
	}
}

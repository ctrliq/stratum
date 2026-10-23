package nodestate

import (
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
)

func TestDomain(t *testing.T) {
	if got := (&NodeState{}).Domain(); got != netdefault.ClusterDomain {
		t.Errorf("unset Domain() = %q, want the default %q", got, netdefault.ClusterDomain)
	}
	if got := (&NodeState{ClusterDomain: "fuzz.internal"}).Domain(); got != "fuzz.internal" {
		t.Errorf("Domain() = %q, want fuzz.internal", got)
	}
}

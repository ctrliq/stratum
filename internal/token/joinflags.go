package token

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
)

// Rendered per front end; a field is empty when the cluster sits at the default for it.
type JoinFlags struct {
	PodCIDR       string `json:"podCidr,omitempty" jsonschema:"the cluster's pod range, when it differs from the default; as --pod-cidr it goes on a worker join line only, since a control plane takes it from the cluster it joins"`
	ServiceCIDR   string `json:"serviceCidr,omitempty" jsonschema:"the cluster's service range, when it differs from the default; as --service-cidr it goes on a worker join line only, since a control plane takes it from the cluster it joins"`
	ClusterDomain string `json:"clusterDomain,omitempty" jsonschema:"the cluster's DNS domain, when it differs from the default; as --cluster-domain it goes on a worker join line only, since a control plane takes it from the cluster it joins"`
	Profile       string `json:"profile,omitempty" jsonschema:"the node profile the cluster runs, by name; the joining node needs --profile <name> to match it"`
}

// A lost netconfig recovers from node state; one that exists but cannot parse is an error.
func LoadJoinFlags() (JoinFlags, error) {
	netc, err := netcfg.Load()
	if err != nil {
		return JoinFlags{}, fmt.Errorf("read this cluster's network config, which the join flags describe: %w", err)
	}
	def := netcfg.Default()
	var f JoinFlags
	if netc.PodCIDR != def.PodCIDR {
		f.PodCIDR = netc.PodCIDR
	}
	if netc.ServiceCIDR != def.ServiceCIDR {
		f.ServiceCIDR = netc.ServiceCIDR
	}
	if netc.ClusterDomain != def.ClusterDomain {
		f.ClusterDomain = netc.ClusterDomain
	}
	st, err := nodestate.LoadState()
	switch {
	case err == nil:
		if _, err := profile.Select(st.ProfileName()); err != nil {
			return JoinFlags{}, fmt.Errorf("this node runs profile %q, which this binary does not register, so the join lines it would hand out cannot be followed: %w", st.ProfileName(), err)
		}
		f.Profile = st.ProfileName()
	case errors.Is(err, fs.ErrNotExist):
		// No state yet means no profile, so no --profile to hand a joiner.
	default:
		return JoinFlags{}, fmt.Errorf("read this node's state, which says whether join lines need --profile: %w", err)
	}
	return f, nil
}

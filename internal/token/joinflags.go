// The join-flag derivation: one owner for the flags a joining node needs, so
// every front end that mints a token derives them here instead of rebuilding
// the rules (and drifting from each other, which is how a node joined through
// the MCP server silently ran the default ranges and the non-CIS profile).

package token

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
)

// JoinFlags are the flags a joining node must be given so it matches the
// cluster it joins. The pod and service ranges and the DNS domain cannot be
// discovered by a node after it joins — a worker learns them only from its
// join line — and --profile is the only way a worker learns the profile the
// cluster runs (with it, the kubelet's seccompDefault and the rest of the
// profile).
//
// It is data, not a rendered line: a field is empty when the cluster sits at
// the default for it, so each front end renders --pod-cidr, --service-cidr
// and --cluster-domain only on a non-default cluster and --profile only on a
// cluster that runs one, and no front end re-derives the rule to drift from
// it.
type JoinFlags struct {
	PodCIDR       string `json:"podCidr,omitempty" jsonschema:"the cluster's pod range, when it differs from the default; as --pod-cidr it goes on a worker join line only, since a control plane takes it from the cluster it joins"`
	ServiceCIDR   string `json:"serviceCidr,omitempty" jsonschema:"the cluster's service range, when it differs from the default; as --service-cidr it goes on a worker join line only, since a control plane takes it from the cluster it joins"`
	ClusterDomain string `json:"clusterDomain,omitempty" jsonschema:"the cluster's DNS domain, when it differs from the default; as --cluster-domain it goes on a worker join line only, since a control plane takes it from the cluster it joins"`
	Profile       string `json:"profile,omitempty" jsonschema:"the node profile the cluster runs, by name; the joining node needs --profile <name> to match it"`
}

// LoadJoinFlags derives the join flags from what this control plane persists:
// the network config for the ranges and domain, and the node's own state for
// the profile.
//
// A lost netconfig is not a default cluster. netcfg.Load recovers the ranges
// and domain a lost file describes from node state — the state the control
// plane recorded at init — and falls back to the built-in defaults only for a
// node with neither, so a custom-network control plane prints join lines for
// the network it actually runs.
//
// A netconfig that exists but cannot be parsed is an error, not a default
// cluster: treating it as one printed a worker join line without
// --service-cidr, the join succeeded, and the worker's kubelet pointed pods
// at the default DNS address on a cluster whose DNS is elsewhere, with
// nothing saying why.
//
// The same care applies to node state: a state that exists but cannot be read
// is an error rather than a silent profile-less join line, while a node that
// has no state yet runs no profile, so the join lines carry no --profile. A
// profile this binary does not register is refused: the join line would name
// it while the bundle this node publishes carries none of its shared secrets,
// a grant no joiner could use.
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
		// A node with no state yet runs no profile, so there is no --profile
		// to hand a joiner.
	default:
		return JoinFlags{}, fmt.Errorf("read this node's state, which says whether join lines need --profile: %w", err)
	}
	return f, nil
}

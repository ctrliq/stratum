package netcfg

import (
	"cmp"
	"fmt"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
)

// The per-node pod-CIDR masks the controller-manager carves each node's range
// at. They are fixed, not defaults a cluster can override, so they need no
// resolving.
const (
	NodeCIDRMask   = 24
	NodeCIDRMaskV6 = 64 // per-node IPv6 pod-CIDR mask under dual-stack
)

// Default returns the built-in network: the default pod and service ranges,
// the Service and DNS addresses derived from the service range the way Parse
// derives them for any range, and the default domain. Deriving them, rather
// than copying netdefault's addresses, means the named defaults cannot drift
// from the range they come from.
func Default() Config {
	c, err := Parse(netdefault.PodCIDR, netdefault.ServiceCIDR)
	if err != nil {
		// The defaults are constants, so only a broken build gets here.
		panic(fmt.Sprintf("parse the built-in network defaults: %v", err))
	}
	return c
}

// DomainOr returns domain when it is set, else the default domain. A domain
// that is set is returned as given, valid or not: refusing a bad one is
// ValidateDomain's job, and swapping it for the default would hide it.
func DomainOr(domain string) string {
	return cmp.Or(domain, netdefault.ClusterDomain)
}

// WithDefaults is Parse with an empty pod or service CIDR taken from the
// default. A CIDR that is given is kept verbatim, and one that cannot be used
// is Parse's error, never replaced by the default.
func WithDefaults(podCIDR, serviceCIDR string) (Config, error) {
	return Parse(cmp.Or(podCIDR, netdefault.PodCIDR), cmp.Or(serviceCIDR, netdefault.ServiceCIDR))
}

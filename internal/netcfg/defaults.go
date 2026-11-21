package netcfg

import (
	"cmp"
	"fmt"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
)

// Fixed per-node pod-CIDR masks the controller-manager carves, not cluster-settable.
const (
	NodeCIDRMask   = 24
	NodeCIDRMaskV6 = 64 // per-node IPv6 pod-CIDR mask under dual-stack
)

// Derived from the ranges, so the named defaults cannot drift from them.
func Default() Config {
	c, err := Parse(netdefault.PodCIDR, netdefault.ServiceCIDR)
	if err != nil {
		// The defaults are constants, so only a broken build gets here.
		panic(fmt.Sprintf("parse the built-in network defaults: %v", err))
	}
	return c
}

// Returned as given, valid or not; refusing a bad one is ValidateDomain's job.
func DomainOr(domain string) string {
	return cmp.Or(domain, netdefault.ClusterDomain)
}

// A CIDR that is given is kept verbatim; an unusable one is Parse's error.
func WithDefaults(podCIDR, serviceCIDR string) (Config, error) {
	return Parse(cmp.Or(podCIDR, netdefault.PodCIDR), cmp.Or(serviceCIDR, netdefault.ServiceCIDR))
}

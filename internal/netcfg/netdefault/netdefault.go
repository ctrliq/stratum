// Package netdefault holds the cluster network's built-in defaults: the pod
// and service ranges, the addresses on the default service range, and the
// DNS domain a cluster gets when the user names none.
//
// It imports nothing on purpose. nodestate needs the default domain and
// netcfg imports nodestate, so the defaults can serve both only from a package
// that reaches neither. Every other package's non-test code goes through
// netcfg's resolver (DomainOr, WithDefaults, and Default where the cluster's
// value cannot apply) instead of reading these, so a value the user gives is
// never passed over for a default.
package netdefault

const (
	// PodCIDR is the default pod range.
	PodCIDR = "10.245.0.0/16"
	// ServiceCIDR is the default service range.
	ServiceCIDR = "10.96.0.0/12"
	// APIServerSvcIP is the kubernetes Service's ClusterIP on the default
	// service range: its first address. No production code reads it:
	// netcfg.Default derives the address from ServiceCIDR, so it cannot
	// drift, and this spells the value tests pin it to.
	APIServerSvcIP = "10.96.0.1"
	// DNSClusterIP is the cluster DNS Service's ClusterIP on the default
	// service range: its tenth address. Like APIServerSvcIP, it spells the
	// value tests pin netcfg.Default's derived address to.
	DNSClusterIP = "10.96.0.10"
	// ClusterDomain is the default cluster DNS domain.
	ClusterDomain = "cluster.local"
)

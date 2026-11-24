// Package netdefault holds network defaults and imports nothing: netcfg and nodestate import it.
package netdefault

const (
	PodCIDR     = "10.245.0.0/16"
	ServiceCIDR = "10.96.0.0/12"

	// Not read by production: netcfg.Default derives the address from ServiceCIDR.
	APIServerSvcIP = "10.96.0.1"
	// Like APIServerSvcIP: tests pin netcfg.Default's derived address to this.
	DNSClusterIP = "10.96.0.10"

	ClusterDomain = "cluster.local"
)

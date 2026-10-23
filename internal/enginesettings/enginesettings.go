// Package enginesettings holds the apiserver flags and kubelet lines the
// engine renders on every node whatever its profile: the apiserver verifies
// kubelet serving certificates against the cluster's trust bundle, and the
// kubelet either bootstraps its serving certificate through the CSR flow or
// serves the static leaf pair a control plane holds.
//
// The package is a leaf: the renderers (internal/staticpod, internal/runtime)
// import it, so it imports no stratum package back. PKI files arrive through
// the caller's resolver.
package enginesettings

// ArgvAnchor is the flag the renderer splices the profile's flags and the
// engine's own in front of.
const ArgvAnchor = "--enable-aggregator-routing=true"

// APIServerFlags is the apiserver flags every control plane carries, with
// PKI files resolved through pki. The renderer splices them after the node
// profile's flags and in front of ArgvAnchor.
func APIServerFlags(pki func(file string) string) []string {
	return []string{"--kubelet-certificate-authority=" + pki("ca-bundle.crt")}
}

// ServingLines is the kubelet's serving-certificate lines: the CSR bootstrap
// when bootstrap holds, the static leaf pair otherwise.
func ServingLines(bootstrap bool, pki func(file string) string) string {
	if bootstrap {
		return "serverTLSBootstrap: true\n"
	}
	return "serverTLSBootstrap: false\n" +
		"tlsCertFile: " + pki("kubelet.crt") + "\n" +
		"tlsPrivateKeyFile: " + pki("kubelet.key") + "\n"
}

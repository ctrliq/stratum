// Package enginesettings is a leaf: the renderers import it, so it imports no stratum package back.
package enginesettings

// A live apiserver flag; the renderer splices the profile's and engine's flags in front of it.
const ArgvAnchor = "--enable-aggregator-routing=true"

// The renderer splices these after the node profile's flags and before ArgvAnchor.
func APIServerFlags(pki func(file string) string) []string {
	return []string{"--kubelet-certificate-authority=" + pki("ca-bundle.crt")}
}

// bootstrap selects the CSR bootstrap; otherwise the static leaf pair is served.
func ServingLines(bootstrap bool, pki func(file string) string) string {
	if bootstrap {
		return "serverTLSBootstrap: true\n"
	}
	return "serverTLSBootstrap: false\n" +
		"tlsCertFile: " + pki("kubelet.crt") + "\n" +
		"tlsPrivateKeyFile: " + pki("kubelet.key") + "\n"
}

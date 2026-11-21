package addons

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// An airgapped cluster must reach none of these; k8s.gcr.io is frozen.
var forbiddenRegistries = []string{
	"registry.k8s.io/",
	"k8s.gcr.io/",
	"quay.io/",
	"docker.io/",
	"gcr.io/",
	"index.docker.io/",
}

var imageLine = regexp.MustCompile(`(?m)^\s*-?\s*image:\s*"?([^"\s]+)`)

func manifests() map[string][]byte {
	return map[string][]byte{
		"coredns":    corednsYAML,
		"local-path": localPathYAML,
		"cilium":     ciliumKPRTrueYAML,
		"dashboard":  dashboardYAML,
	}
}

func TestEmbeddedManifestsNameNoUpstreamRegistry(t *testing.T) {
	for name, data := range manifests() {
		for _, m := range imageLine.FindAllStringSubmatch(string(data), -1) {
			for _, bad := range forbiddenRegistries {
				if strings.Contains(m[1], bad) {
					t.Errorf("%s: image %q names %s — write it as %s/<name>:<tag> instead",
						name, m[1], bad, registrySentinel)
				}
			}
		}
	}
}

// make images registers the payload under these names; anything else is an ImagePullBackOff.
func TestRenderedManifestsUseOnlyTheMirror(t *testing.T) {
	// Stamped by the Makefile in a real build; set here so the test asserts the substitution.
	ImageRegistry = "registry.example/ske"
	CorednsVersion = "9.9.9"
	LocalPathVersion = "9.9.9"
	version.NsenterImage = "registry.example/ske/busybox:9.9.9"
	DashImage = "registry.example/ske/stratum-dash:9.9.9"
	t.Cleanup(func() {
		ImageRegistry, CorednsVersion, LocalPathVersion, DashImage = "unknown", "unknown", "unknown", "unknown"
		version.NsenterImage = "ghcr.io/ctrliq/stratum/busybox:unknown"
	})

	rendered := map[string][]byte{
		"coredns":    RenderCoreDNS(),
		"local-path": RenderLocalPath(),
		"cilium":     FillAPIServer(RenderCilium(false, CiliumSettings{}), APIServer{Host: "10.0.0.1"}),
		"dashboard":  RenderDashboard(DashboardSettings{NodePort: DefaultDashboardPort}),
	}
	for name, data := range rendered {
		found := 0
		for _, m := range imageLine.FindAllStringSubmatch(string(data), -1) {
			found++
			if !strings.HasPrefix(m[1], ImageRegistry+"/") {
				t.Errorf("%s: image %q is not from the mirror %q", name, m[1], ImageRegistry)
			}
			if strings.Contains(m[1], "__STRATUM_") {
				t.Errorf("%s: image %q still carries an unsubstituted sentinel", name, m[1])
			}
		}
		if found == 0 {
			t.Errorf("%s: no image references found — the regex or the manifest changed shape", name)
		}
	}
}

// The nsenter image runs on the node during CA rotation and upgrades, on airgapped clusters.
func TestNsenterImageIsMirrored(t *testing.T) {
	for _, bad := range forbiddenRegistries {
		if strings.Contains(version.NsenterImage, bad) {
			t.Errorf("NsenterImage %q names %s; it must be the mirrored ref (see the Makefile ldflags)",
				version.NsenterImage, bad)
		}
	}
}

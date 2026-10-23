package addons

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// The registries a running SKE cluster is not allowed to talk to. registry.k8s.io
// is the one that matters most — an airgapped node cannot reach it, and a cluster
// that silently pulls from it is one upstream outage away from not scheduling
// pods. k8s.gcr.io is worse: it is frozen and must not appear anywhere at all.
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

// The embedded manifests must not name an upstream registry even before
// rendering: a sentinel that was missed is a literal that survives to the
// cluster, and the render path has no way to fix it.
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

// …and every rendered image must come from the mirror. This is the runtime half
// of the airgap claim: `make images` registers the payload under exactly these
// names, so a manifest that asks for anything else is an ImagePullBackOff on a
// cluster with no registry.
func TestRenderedManifestsUseOnlyTheMirror(t *testing.T) {
	// Stamped by the Makefile in a real build; set here so the test asserts the
	// substitution happened rather than asserting the default.
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

// The nsenter image runs on the node during CA rotation and cluster upgrade,
// which happen on airgapped clusters by definition. It has to be a mirrored
// name for the same reason the manifests do.
func TestNsenterImageIsMirrored(t *testing.T) {
	for _, bad := range forbiddenRegistries {
		if strings.Contains(version.NsenterImage, bad) {
			t.Errorf("NsenterImage %q names %s; it must be the mirrored ref (see the Makefile ldflags)",
				version.NsenterImage, bad)
		}
	}
}

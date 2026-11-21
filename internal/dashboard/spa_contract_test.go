package dashboard

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// The contract is with the SPA sources, not the embedded dist, which can be older.
const spaSrc = "../../dash/src"

type spaIdentity struct {
	name     string
	constant string
	value    string
	where    string
}

// "v1" alone is in every /api/v1 path, so group and version are one search.
var spaIdentities = []spaIdentity{
	{name: "control-plane label", constant: "k8sutil.ControlPlaneLabel", value: k8sutil.ControlPlaneLabel, where: "dash/src/live.js"},
	{name: "version annotation", constant: "k8sutil.VersionAnnotation", value: k8sutil.VersionAnnotation, where: "dash/src/live.js"},
	{name: "operator lease name", constant: "k8sutil.OperatorLeaseName", value: k8sutil.OperatorLeaseName, where: "dash/src/live.js"},
	{name: "helmchart group and version", constant: `k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion`, value: k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion, where: "dash/src/resources.jsx"},
}

// The SPA's own tests reuse these literals, so a Go-side rename passes them.
func TestSPAUsesTheEngineIdentityStrings(t *testing.T) {
	srcs := spaSources(t)
	for _, id := range spaIdentities {
		t.Run(id.name, func(t *testing.T) {
			if spaIdentityPresent(srcs, id) {
				return
			}
			t.Errorf("%s (%q) is missing from its consumer %s",
				id.constant, id.value, id.where)
		})
	}
}

func spaIdentityPresent(srcs map[string]string, id spaIdentity) bool {
	return strings.Contains(srcs[filepath.Join("..", "..", id.where)], id.value)
}

// Exercise the guard with sources independent of the real SPA, where all match.
func TestSPAIdentityGuardUsesTheDesignatedConsumer(t *testing.T) {
	for _, id := range spaIdentities {
		t.Run(id.name, func(t *testing.T) {
			consumer := filepath.Join("..", "..", id.where)
			for _, tc := range []struct {
				name    string
				sources map[string]string
				want    bool
			}{
				{"correct consumer", map[string]string{consumer: id.value}, true},
				{"stale unrelated comment", map[string]string{consumer: "obsolete-identity", "../../dash/src/unrelated.js": "// " + id.value}, false},
				{"missing consumer", map[string]string{"../../dash/src/unrelated.js": id.value}, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if got := spaIdentityPresent(tc.sources, id); got != tc.want {
						t.Errorf("identity guard = %t, want %t for %s", got, tc.want, id.where)
					}
				})
			}
		})
	}
}

// The identity strings are literals in the sources, so a text search is the check.
func spaSources(t *testing.T) map[string]string {
	t.Helper()
	srcs := make(map[string]string)
	err := filepath.WalkDir(spaSrc, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := filepath.Ext(path); ext != ".js" && ext != ".jsx" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		srcs[path] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("read %s: %v", spaSrc, err)
	}
	if len(srcs) == 0 {
		// No sources means the walk points wrong, not that every string drifted.
		t.Fatalf("no .js or .jsx files under %s", spaSrc)
	}
	return srcs
}

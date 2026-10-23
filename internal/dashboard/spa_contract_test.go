package dashboard

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// spaSrc is the SPA's source tree, relative to the package directory go test
// runs in. The sources are read as text, not through the embedded dist build:
// the contract is with what the SPA's own code says, and a checked-in dist can
// be older than its sources.
const spaSrc = "../../dash/src"

// spaIdentity is one engine identity string the SPA must keep using.
type spaIdentity struct {
	// name labels the subtest. constant is the Go declaration the string
	// comes from — the source of truth — and is what a failure names.
	name     string
	constant string
	value    string
	// where is the dash/src file the string lives in today, so a failure
	// points at the copy that went stale.
	where string
}

// spaIdentities are the engine facts the SPA hand-copies, all owned by
// internal/k8sutil so a rename there cannot leave the SPA behind: the node
// label and version annotation the upgrade stamps, the Lease the operator
// campaigns for, and the API group/version of its HelmChart CRs.
//
// The group and the version are checked as the single URL segment the SPA
// builds its HelmChart paths from ("stratum.ciq.com/v1"). The version alone
// is too short to search for — "v1" is in every /api/v1 path — so checking
// it apart from the group would pass while the HelmCharts list 404s.
var spaIdentities = []spaIdentity{
	{name: "control-plane label", constant: "k8sutil.ControlPlaneLabel", value: k8sutil.ControlPlaneLabel, where: "dash/src/live.js"},
	{name: "version annotation", constant: "k8sutil.VersionAnnotation", value: k8sutil.VersionAnnotation, where: "dash/src/live.js"},
	{name: "operator lease name", constant: "k8sutil.OperatorLeaseName", value: k8sutil.OperatorLeaseName, where: "dash/src/live.js"},
	{name: "helmchart group and version", constant: `k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion`, value: k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion, where: "dash/src/resources.jsx"},
}

// TestSPAUsesTheEngineIdentityStrings pins the SPA's hand-copies of the
// engine's identity strings. The SPA's own tests in dash/test use the same
// literals as the code they test, so a Go-side rename passes them while the
// dashboard shows no control planes, no stamped versions, no operator status
// and no HelmCharts; this test fails that rename instead.
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

// A copy in an unrelated module must not mask a changed lookup in the
// designated consumer. Exercise the guard with sources independent of the
// real SPA, where every identity currently matches.
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

// spaSources reads every .js and .jsx source in the SPA's tree, keyed by its
// path. The files are not transpiled here — the identity strings are literals
// in the sources, so a plain text search is the whole check.
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
		// No sources at all means the walk points wrong (a renamed tree, a
		// changed working directory), not that every identity string
		// drifted; say so instead of failing all four subtests.
		t.Fatalf("no .js or .jsx files under %s", spaSrc)
	}
	return srcs
}

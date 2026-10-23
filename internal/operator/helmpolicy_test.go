package operator

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestTargetNamespaceAllowed pins the namespace containment rule: a HelmChart may
// target its own namespace (or an unset target, which parseHelmSpec defaults to
// its own), a privileged-namespace chart may target anywhere, and every other
// cross-namespace target is refused.
func TestTargetNamespaceAllowed(t *testing.T) {
	priv := []string{"kube-system"}
	cases := []struct {
		name         string
		crNS, target string
		privileged   []string
		want         bool
	}{
		{"same namespace", "team-a", "team-a", priv, true},
		{"empty target (defaults to own)", "team-a", "", priv, true},
		{"tenant crossing into kube-system", "team-a", "kube-system", priv, false},
		{"tenant crossing into another tenant", "team-a", "team-b", priv, false},
		{"privileged ns may target anywhere", "kube-system", "team-a", priv, true},
		{"privileged ns targeting itself", "kube-system", "kube-system", priv, true},
		{"no privileged namespaces: cross refused", "team-a", "team-b", nil, false},
		{"multiple privileged", "addons", "team-a", []string{"kube-system", "addons"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := targetNamespaceAllowed(c.crNS, c.target, c.privileged); got != c.want {
				t.Errorf("targetNamespaceAllowed(%q,%q,%v) = %v, want %v", c.crNS, c.target, c.privileged, got, c.want)
			}
		})
	}
}

// TestSplitNamespaces: comma-split, trims spaces, drops empties.
func TestSplitNamespaces(t *testing.T) {
	if got := splitNamespaces("kube-system"); !slices.Equal(got, []string{"kube-system"}) {
		t.Errorf("single = %v", got)
	}
	if got := splitNamespaces(" kube-system , addons ,,"); !slices.Equal(got, []string{"kube-system", "addons"}) {
		t.Errorf("multi/trim/empty = %v", got)
	}
	if got := splitNamespaces(""); len(got) != 0 {
		t.Errorf("empty = %v, want none", got)
	}
}

// TestParseHelmSpecInsecureForInlineChart pins that an inline chart
// does no fetch, so insecureSkipTLSVerify is forced off (never logged or
// surfaced in .status as a skipped verification); a repo-based chart keeps it.
func TestParseHelmSpecInsecureForInlineChart(t *testing.T) {
	mk := func(spec map[string]interface{}) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"namespace": "team-a", "name": "x"},
			"spec":     spec,
		}}
	}
	inline := parseHelmSpec(mk(map[string]interface{}{
		"chartContent":          "AAAA",
		"insecureSkipTLSVerify": true,
	}))
	if inline.insecureSkipTLSVerify {
		t.Error("inline chart must force insecureSkipTLSVerify=false (no fetch happens)")
	}
	repo := parseHelmSpec(mk(map[string]interface{}{
		"repo":                  "https://charts.example.com",
		"chart":                 "c",
		"insecureSkipTLSVerify": true,
	}))
	if !repo.insecureSkipTLSVerify {
		t.Error("repo chart must preserve the requested insecureSkipTLSVerify (operator gate applies later)")
	}
}

// TestUninstallTarget pins the rule that the deletion path uninstalls what was
// ADOPTED, not what the spec currently says. The spec stays tenant-writable
// right up to the delete, so the two attacks this closes are: mutate
// releaseName to have the operator uninstall a release the CR never owned, and
// mutate targetNamespace to something refused so the CR's own release is left
// running with nothing tracking it.
func TestUninstallTarget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   map[string]interface{}
		wantNS   string
		wantName string
		wantOK   bool
	}{
		{
			name:     "recorded release in the CR's own namespace",
			status:   map[string]interface{}{"releaseNamespace": "team-a", "releaseName": "app"},
			wantNS:   "team-a",
			wantName: "app",
			wantOK:   true,
		},
		{
			name:   "nothing recorded: adopted but never deployed",
			status: map[string]interface{}{},
			wantOK: false,
		},
		{
			name:   "nil status",
			wantOK: false,
		},
		{
			name:   "half-recorded is not a target",
			status: map[string]interface{}{"releaseNamespace": "team-a"},
			wantOK: false,
		},
		{
			// Older installs remain owned after the namespace policy changes.
			name:     "recorded namespace no longer allowed for installs",
			status:   map[string]interface{}{"releaseNamespace": "kube-system", "releaseName": "coredns"},
			wantNS:   "kube-system",
			wantName: "coredns",
			wantOK:   true,
		},
		{
			name:     "privileged namespace may uninstall elsewhere",
			status:   map[string]interface{}{"releaseNamespace": "team-a", "releaseName": "app"},
			wantNS:   "team-a",
			wantName: "app",
			wantOK:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, name, ok := uninstallTarget(tc.status)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (ns=%q name=%q)", ok, tc.wantOK, ns, name)
			}
			if ok && (ns != tc.wantNS || name != tc.wantName) {
				t.Errorf("target = %s/%s, want %s/%s", ns, name, tc.wantNS, tc.wantName)
			}
		})
	}
}

// The spec is not consulted at all. This is the actual regression guard: a
// reader who "helpfully" reintroduces spec.targetNamespace/spec.releaseName
// into the deletion path re-opens both attacks, and every case above would
// still pass because they only exercise status.
func TestUninstallTargetIgnoresSpec(t *testing.T) {
	// Recorded: the release this CR really owns. The tenant has since repointed
	// the spec at kube-system/coredns, which must have no effect here.
	status := map[string]interface{}{"releaseNamespace": "team-a", "releaseName": "app"}
	ns, name, ok := uninstallTarget(status)
	if !ok {
		t.Fatal("refused to uninstall the release the CR actually owns")
	}
	if ns != "team-a" || name != "app" {
		t.Errorf("uninstall target = %s/%s, want the RECORDED team-a/app — the mutated spec must not reach this decision", ns, name)
	}
}

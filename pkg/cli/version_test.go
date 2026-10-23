package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// TestVersionComponents_ReadsInternalVersion: `version --components` reads
// the five component stamps out of internal/version as its defaults, not
// only through WithComponentVersions from cmd/stratum's main-package vars,
// so a build stamped without that wiring still reports every component.
func TestVersionComponents_ReadsInternalVersion(t *testing.T) {
	old := [5]string{
		version.ContainerdVersion, version.RuncVersion, version.KubeVipVersion,
		version.CiliumVersion, version.CorednsVersion,
	}
	t.Cleanup(func() {
		version.ContainerdVersion, version.RuncVersion, version.KubeVipVersion = old[0], old[1], old[2]
		version.CiliumVersion, version.CorednsVersion = old[3], old[4]
	})
	version.ContainerdVersion = "2.91.3"
	version.RuncVersion = "1.91.4"
	version.KubeVipVersion = "1.91.5"
	version.CiliumVersion = "1.91.6"
	version.CorednsVersion = "1.91.7"

	// No WithComponentVersions: the stamped defaults must reach the report.
	got := runVersionComponents(t, rootConfig{displayVersion: "v1.37.1-stratum.0"})
	for name, want := range map[string]string{
		"containerd": "2.91.3", "runc": "1.91.4", "kube-vip": "1.91.5",
		"cilium": "1.91.6", "coredns": "1.91.7",
	} {
		if got[name] != want {
			t.Errorf("version --components reports %s = %q, want the internal/version stamp %q", name, got[name], want)
		}
	}

	// konnectivity is not shipped, so it is never reported, stamped or not.
	if _, ok := got["konnectivity"]; ok {
		t.Errorf("version --components reports konnectivity = %q, want no row", got["konnectivity"])
	}

	// WithComponentVersions stays an override: a non-empty field replaces the
	// stamped default, an empty field leaves the stamp in place.
	got = runVersionComponents(t, rootConfig{
		displayVersion: "v1.37.1-stratum.0",
		components:     ComponentVersions{Containerd: "9.9.9-override", Konnectivity: "9.9.9-ignored"},
	})
	// The deprecated Konnectivity field is a no-op: its value reaches no row.
	if _, ok := got["konnectivity"]; ok {
		t.Errorf("version --components reports konnectivity = %q for a nonempty ComponentVersions.Konnectivity, want no row", got["konnectivity"])
	}
	if got["containerd"] != "9.9.9-override" {
		t.Errorf("version --components reports containerd = %q, want the WithComponentVersions override", got["containerd"])
	}
	if got["runc"] != "1.91.4" {
		t.Errorf("version --components reports runc = %q, want the stamped default an empty override field leaves", got["runc"])
	}
}

// runVersionComponents runs `version --components` for one rootConfig and
// returns the reported name/version pairs. version.go prints with fmt.Print*
// straight to os.Stdout, so the capture swaps os.Stdout for a pipe the way
// TestVersionBuildPrintsTheBuildID does.
func runVersionComponents(t *testing.T, cfg rootConfig) map[string]string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()
	cmd := newVersionCommand(cfg)
	cmd.SetArgs([]string{"--components"})
	runErr := cmd.Execute()
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatal(runErr)
	}
	components := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.HasPrefix(line, "  ") {
			components[fields[0]] = fields[1]
		}
	}
	return components
}

func TestRuntimeMismatchWarning(t *testing.T) {
	if w := runtimeMismatchWarning("2.4.1+stratum", "v2.4.1+stratum", "/s"); w != "" {
		t.Errorf("same version warned: %q", w)
	}
	w := runtimeMismatchWarning("v2.3.5+stratum", "v2.4.1+stratum", "/s")
	if strings.Contains(w, "older") {
		t.Errorf("warning %q claims the other containerd is older; only the versions differ", w)
	}
	for _, want := range []string{"v2.4.1+stratum is installed", "reports v2.3.5+stratum", "/s", "another containerd"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q lacks %q", w, want)
		}
	}
}

// TestRuntimeMismatchSilentWithoutARuntime: a node with nothing on the CRI
// socket (or no stratum at all) gets no warning on `version --components`.
func TestRuntimeMismatchSilentWithoutARuntime(t *testing.T) {
	dir := t.TempDir()
	if w := runtimeMismatch(filepath.Join(dir, "none.sock"), filepath.Join(dir, "containerd")); w != "" {
		t.Errorf("warned with no runtime: %q", w)
	}
}

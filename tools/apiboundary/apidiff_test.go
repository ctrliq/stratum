package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// releasedBaselineTag names the release whose pkg/cli surface released
// downstream builds were last compiled against. Its exported API is kept in
// testdata/pkgcli.released.api, so the compatibility check below needs no git
// tag and no history: it passes in a shallow clone and in one without tags. Regenerating the documentation golden
// (testdata/pkgcli.api) only records a surface change as deliberate; it cannot
// bless one, because the diff against the released baseline still fails until
// the baseline moves. The baseline file is pinned by releasedBaselineSHA256, so
// it cannot be edited by hand without a visible change to this file either.
//
// Moving the baseline is the release-by-release decision that released
// consumers have moved with the change: point releasedBaselineTag at the
// release downstream builds now pin, regenerate the file, and set
// releasedBaselineSHA256 to the sum the run reports:
//
//	go test ./tools/apiboundary/ -run TestPkgCLIExportedAPICompatibleWithTheReleasedBaseline -update-baseline
const (
	releasedBaselineTag    = "v1.37.1-stratum.0-rc.1"
	releasedBaselineSHA256 = "342eda120cb35bc3d2d87fa188bb3a08a0e6d961a05a2befe06007e8e21fd450"
)

var releasedBaselinePath = filepath.Join("testdata", "pkgcli.released.api")

func releasedBaselineHeader() string {
	return "# Exported API of github.com/ctrliq/stratum/pkg/cli at " + releasedBaselineTag + ", one declaration per line.\n" +
		"# Regenerate with: go test ./tools/apiboundary/ -run TestPkgCLIExportedAPICompatibleWithTheReleasedBaseline -update-baseline\n"
}

var updateBaseline = flag.Bool("update-baseline", false,
	"rewrite testdata/pkgcli.released.api from pkg/cli's exported API at releasedBaselineTag")

// TestPkgCLIExportedAPICompatibleWithTheReleasedBaseline diffs the released
// baseline against HEAD's golden, which TestPkgCLIExportedAPIMatchesTheGolden
// keeps equal to the live package. An identifier the baseline had that is gone
// from HEAD, or whose signature, type or value moved, fails the test: a
// consumer compiled against the released surface stops compiling. Identifiers
// added since the baseline are reported but allowed, because an addition
// cannot break a released consumer.
func TestPkgCLIExportedAPICompatibleWithTheReleasedBaseline(t *testing.T) {
	if *updateBaseline {
		writeReleasedBaseline(t)
		return
	}
	raw, err := os.ReadFile(releasedBaselinePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), releasedBaselineHeader()) {
		t.Fatalf("%s was not generated at %s; regenerate it with -update-baseline", releasedBaselinePath, releasedBaselineTag)
	}
	if sum := fmt.Sprintf("%x", sha256.Sum256(raw)); sum != releasedBaselineSHA256 {
		t.Fatalf("%s has sha256 %s, want releasedBaselineSHA256 %s: the released baseline moves only by regenerating it at a new releasedBaselineTag", releasedBaselinePath, sum, releasedBaselineSHA256)
	}
	baseline := readAPILines(t, releasedBaselinePath)
	head := readAPILines(t, filepath.Join("testdata", "pkgcli.api"))

	broken, added := apiCompatibility(baseline, head)
	if len(added) > 0 {
		t.Logf("added to pkg/cli since %s (compatible; reported for review):\n  %s",
			releasedBaselineTag, strings.Join(added, "\n  "))
	}
	if len(broken) > 0 {
		t.Errorf("pkg/cli's exported API removed or retyped identifiers the released baseline %s had:\n  %s\n"+
			"A consumer compiled against that surface stops compiling.\n"+
			"If the change is the deliberate baseline bump, point releasedBaselineTag in apidiff_test.go at a release whose surface carries it, regenerate %s with -update-baseline and update releasedBaselineSHA256.",
			releasedBaselineTag, strings.Join(broken, "\n  "), releasedBaselinePath)
	}
}

// TestReleasedBaselineCarriesTheAPIsDownstreamBuildsCall pins the entry points
// a downstream build calls today into the baseline, so removing or retyping one
// of them fails the compatibility check instead of passing as an identifier
// added after the release.
func TestReleasedBaselineCarriesTheAPIsDownstreamBuildsCall(t *testing.T) {
	baseline := readAPILines(t, releasedBaselinePath)
	for _, name := range []string{"WithStacks", "WithProfiles", "ExitCode", "StampedVersions"} {
		if !slices.ContainsFunc(baseline, func(l string) bool { return declares(l, name) }) {
			t.Errorf("the released baseline %s does not declare pkg/cli.%s", releasedBaselinePath, name)
		}
	}
}

// TestAPICompatibilityFailsOnARemovedOrRetypedIdentifier proves the diff
// itself: each baseline entry point, removed from HEAD or given another
// signature, is reported as broken, while an identifier added to HEAD is not.
func TestAPICompatibilityFailsOnARemovedOrRetypedIdentifier(t *testing.T) {
	baseline := readAPILines(t, releasedBaselinePath)
	for _, name := range []string{"WithStacks", "WithProfiles", "ExitCode", "StampedVersions"} {
		i := slices.IndexFunc(baseline, func(l string) bool { return declares(l, name) })
		if i < 0 {
			t.Fatalf("the released baseline does not declare pkg/cli.%s", name)
		}
		line := baseline[i]

		removed := slices.Delete(slices.Clone(baseline), i, i+1)
		if broken, _ := apiCompatibility(baseline, removed); !slices.Equal(broken, []string{line}) {
			t.Errorf("removing %s: broken = %q, want [%q]", name, broken, line)
		}

		retyped := slices.Clone(baseline)
		retyped[i] = line + " /* retyped */"
		if broken, _ := apiCompatibility(baseline, retyped); !slices.Equal(broken, []string{line}) {
			t.Errorf("retyping %s: broken = %q, want [%q]", name, broken, line)
		}
	}

	grown := append(slices.Clone(baseline), "func github.com/ctrliq/stratum/pkg/cli.Added()")
	if broken, added := apiCompatibility(baseline, grown); len(broken) != 0 || len(added) != 1 {
		t.Errorf("adding an identifier: broken = %q, added = %q, want no breaks and one addition", broken, added)
	}
}

// apiCompatibility returns the baseline lines HEAD no longer carries, which
// break a released consumer, and the HEAD lines the baseline lacked.
func apiCompatibility(baseline, head []string) (broken, added []string) {
	for _, l := range baseline {
		if !slices.Contains(head, l) {
			broken = append(broken, l)
		}
	}
	for _, l := range head {
		if !slices.Contains(baseline, l) {
			added = append(added, l)
		}
	}
	return broken, added
}

// declares reports whether an API line declares pkg/cli's package-scope name.
func declares(line, name string) bool {
	return strings.Contains(line, "github.com/ctrliq/stratum/pkg/cli."+name+"(") ||
		strings.Contains(line, "github.com/ctrliq/stratum/pkg/cli."+name+" ")
}

func readAPILines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

// writeReleasedBaseline extracts pkg/cli's exported API at releasedBaselineTag
// with the same golden extraction the documentation golden uses. The tag is
// checked out into a temporary git worktree, so regenerating needs the tag in
// the local repository; the check itself never does.
func writeReleasedBaseline(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "baseline")
	add := exec.Command("git", "worktree", "add", "--detach", dir, releasedBaselineTag)
	add.Dir = moduleRoot
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("checking %s out into a worktree (fetch it with: git fetch origin tag %s): %v\n%s",
			releasedBaselineTag, releasedBaselineTag, err, out)
	}
	t.Cleanup(func() {
		remove := exec.Command("git", "worktree", "remove", "--force", dir)
		remove.Dir = moduleRoot
		if out, err := remove.CombinedOutput(); err != nil {
			t.Logf("removing the %s worktree: %v\n%s", releasedBaselineTag, err, out)
		}
	})

	// The released tree's go.mod can predate indirect dependencies of its own
	// dependency graph, so the go command must be allowed to complete go.mod
	// and go.sum.
	t.Setenv("GOFLAGS", "-mod=mod")
	pkg, _, _, err := loadExportedTypes(filepath.Join(dir, "pkg", "cli"))
	if err != nil {
		t.Fatalf("extracting pkg/cli's exported API at %s: %v", releasedBaselineTag, err)
	}
	body := []byte(releasedBaselineHeader() + strings.Join(goldenLines(pkg), "\n") + "\n")
	if err := os.WriteFile(releasedBaselinePath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s; set releasedBaselineSHA256 to %x", releasedBaselinePath, sha256.Sum256(body))
}

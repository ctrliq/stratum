package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// releasedBaselineTag names the release whose pkg/cli surface released
// downstream builds were last compiled against. The
// differential test below holds pkg/cli's exported API to it: an identifier
// the baseline had must keep its name and its exact shape. Regenerating the
// documentation golden (testdata/pkgcli.api) only records a surface change
// as deliberate — it cannot bless one, because the diff against this tag
// still fails until the tag itself is bumped. Bumping this constant is the
// release-by-release decision that released consumers have moved with the
// change.
const releasedBaselineTag = "v1.37.1-stratum.0-rc.6"

// The compatibility check materializes a release tag locally, so CI must
// fetch that history before running the test suite.
func TestCICheckoutIncludesReleasedBaseline(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string `json:"uses"`
				With struct {
					FetchDepth *int `json:"fetch-depth"`
				} `json:"with"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, step := range workflow.Jobs["go"].Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			if step.With.FetchDepth == nil || *step.With.FetchDepth != 0 {
				t.Fatalf("CI must fetch release history for the %s API baseline; checkout defaults omit the tag", releasedBaselineTag)
			}
			return
		}
	}
	t.Fatal("CI go job has no checkout for the released API baseline")
}

// TestPkgCLIExportedAPICompatibleWithTheReleasedBaseline extracts pkg/cli's
// exported API at releasedBaselineTag with the same golden extraction the
// documentation golden uses (goldenLines over the compiled package), and
// diffs it against HEAD's golden — kept equal to the live package by
// TestPkgCLIExportedAPIMatchesTheGolden. An identifier the baseline had that
// is gone from HEAD, or whose signature, type or value moved, fails the
// test: a consumer compiled against the released surface stops compiling.
// Identifiers added since the baseline are reported but allowed, because an
// addition cannot break a released consumer.
func TestPkgCLIExportedAPICompatibleWithTheReleasedBaseline(t *testing.T) {
	baselineDir := materializeReleasedBaseline(t, releasedBaselineTag)
	// The released tree's go.mod predates some indirect dependencies of its
	// own dependency graph, so the go command must be allowed to complete
	// go.mod and go.sum. The modules come from the module cache: once the
	// tag has been built once, the whole extraction runs offline (verified
	// by running the suite under GOPROXY=off).
	t.Setenv("GOFLAGS", "-mod=mod")
	baselinePkg, _, _, err := loadExportedTypes(filepath.Join(baselineDir, "pkg", "cli"))
	if err != nil {
		t.Fatalf("extracting pkg/cli's exported API at %s: %v", releasedBaselineTag, err)
	}
	baseline := goldenLines(baselinePkg)

	raw, err := os.ReadFile(filepath.Join("testdata", "pkgcli.api"))
	if err != nil {
		t.Fatalf("reading the golden: %v (regenerate with -update)", err)
	}
	var head []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			head = append(head, line)
		}
	}

	headLines := make(map[string]bool, len(head))
	for _, l := range head {
		headLines[l] = true
	}
	baselineLines := make(map[string]bool, len(baseline))
	var broken []string
	for _, l := range baseline {
		baselineLines[l] = true
		if !headLines[l] {
			broken = append(broken, l)
		}
	}
	var added []string
	for _, l := range head {
		if !baselineLines[l] {
			added = append(added, l)
		}
	}
	if len(added) > 0 {
		t.Logf("added to pkg/cli since %s (compatible; reported for review):\n  %s",
			releasedBaselineTag, strings.Join(added, "\n  "))
	}
	if len(broken) > 0 {
		t.Errorf("pkg/cli's exported API removed or retyped identifiers the released baseline %s had:\n  %s\n"+
			"A consumer compiled against that surface stops compiling.\n"+
			"If the change is the deliberate baseline bump, point releasedBaselineTag in apidiff_test.go at a tag whose surface carries it.",
			releasedBaselineTag, strings.Join(broken, "\n  "))
	}
}

// materializeReleasedBaseline checks the tag out into a temporary git
// worktree — a local operation on this repository's object database, with no
// clone, no fetch and no network, so the baseline is materialized from what
// the checkout already carries. Removing the worktree restores the
// repository's worktree bookkeeping.
func materializeReleasedBaseline(t *testing.T, tag string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "baseline")
	add := exec.Command("git", "worktree", "add", "--detach", dir, tag)
	add.Dir = root
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("checking %s out into a worktree (the tag must exist in the local repository; fetching it needs a network, the check itself does not): %v\n%s",
			tag, err, out)
	}
	t.Cleanup(func() {
		remove := exec.Command("git", "worktree", "remove", "--force", dir)
		remove.Dir = root
		if out, err := remove.CombinedOutput(); err != nil {
			t.Logf("removing the %s worktree: %v\n%s", tag, err, out)
		}
	})
	return dir
}

package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/nodejob"
)

// answer stands in for the check Job on a node and records what it ran.
func answer(t *testing.T, code int, err error) *[]nodejob.Spec {
	t.Helper()
	var ran []nodejob.Spec
	old := runExitCode
	runExitCode = func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) (int, error) {
		ran = append(ran, s)
		return code, err
	}
	t.Cleanup(func() { runExitCode = old })
	return &ran
}

// The orchestrator's question before a drain: exit 0 is "already on
// this build", ExitUpgradeNeeded is "upgrade it", and anything else is not an
// answer at all -- so the node is neither skipped nor drained on a guess.
func TestNodeRunsThisBuild(t *testing.T) {
	o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-rc3"}
	n := Node{Name: "w1", Version: "v1.37.1"}
	for _, c := range []struct {
		name    string
		code    int
		err     error
		current bool
		fails   bool
		wantMsg string
	}{
		{"on this build", 0, nil, true, false, ""},
		{"on another build", ExitUpgradeNeeded, nil, false, false, ""},
		{"refuses its prerequisites", ExitPrereqMissing, nil, false, true, "the node was not drained"},
		{"the check itself failed", 1, nil, false, true, ""},
		{"no answer", 0, errors.New("build check Job did not complete in time"), false, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ran := answer(t, c.code, c.err)
			current, err := nodeRunsThisBuild(context.Background(), io.Discard, nil, n, o)
			if current != c.current || (err != nil) != c.fails {
				t.Errorf("nodeRunsThisBuild = %v, %v; want %v, error=%v", current, err, c.current, c.fails)
			}
			if c.wantMsg != "" && (err == nil || !strings.Contains(err.Error(), c.wantMsg)) {
				t.Errorf("error %v does not say %q", err, c.wantMsg)
			}
			if len(*ran) != 1 || (*ran)[0].Node != "w1" {
				t.Fatalf("check Jobs = %+v, want one on w1", *ran)
			}
		})
	}
}

// The check runs the staged binary, read-only: --check, the same --from and
// --target as the upgrade would, and nothing that belongs to an upgrade.
func TestNodeCheckArgv(t *testing.T) {
	o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-rc3", SoakSec: 15, SkipSnapshot: true}
	argv := nodeCheckArgv(o, Node{})
	i := slices.Index(argv, o.From)
	if i < 0 || !slices.Equal(argv[i:], []string{o.From, "upgrade", "--from", o.From, "--target", "v1.37.1", "--check"}) {
		t.Errorf("check argv = %q", argv)
	}
	if joined := strings.Join(argv, " "); strings.Contains(joined, "--soak") || strings.Contains(joined, "--skip-snapshot") {
		t.Errorf("check argv carries upgrade-only flags: %q", argv)
	}
}

// The check and the upgrade exec the same staged binary against the same
// target, with the same pin: everything before --check is exactly what the
// upgrade starts with, so the check cannot answer for another binary.
func TestCheckAndUpgradeArgvShareTheirPrefix(t *testing.T) {
	o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-next", SoakSec: 5,
		expectBuild: "0123abcd", expectSHA256: "feedface", arch: "arm64"}
	for _, n := range []Node{{Name: "w1", Arch: "arm64"}, {Name: "w2", Arch: "amd64"}} {
		check, up := nodeCheckArgv(o, n), nodeUpgradeArgv(o, n)
		prefix := check[:len(check)-1]
		if check[len(check)-1] != "--check" || !slices.Equal(up[:len(prefix)], prefix) {
			t.Errorf("%s: check argv %q is not the upgrade argv %q plus --check", n.Name, check, up)
		}
	}
}

// Every node is pinned to the build ID; the exact bytes pin only the nodes of
// the orchestrator's architecture, since another architecture runs another
// binary of the same build.
func TestStagedArgvPinsBuildAndSameArchBytes(t *testing.T) {
	o := ClusterOptions{Target: "v1.37.1", From: "/x", expectBuild: "0123abcd", expectSHA256: "feedface", arch: "arm64"}
	flag := func(argv []string, f string) string {
		if i := slices.Index(argv, f); i >= 0 && i+1 < len(argv) {
			return argv[i+1]
		}
		return ""
	}
	same, other := stagedUpgradeArgv(o, Node{Arch: "arm64"}), stagedUpgradeArgv(o, Node{Arch: "amd64"})
	if flag(same, "--expect-build") != "0123abcd" || flag(other, "--expect-build") != "0123abcd" {
		t.Errorf("--expect-build: same arch %q, other arch %q; want both 0123abcd", same, other)
	}
	if flag(same, "--expect-sha256") != "feedface" {
		t.Errorf("same-arch argv %q lacks --expect-sha256", same)
	}
	if slices.Contains(other, "--expect-sha256") {
		t.Errorf("other-arch argv %q pins bytes that cannot match", other)
	}
}

// A --from that cannot say what it is does not start the rollout: unpinned,
// each node would answer for whatever is staged there.
func TestPinStagedBinaryRefusesAnUnidentifiedBinary(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "stratum-next")
	writeFile(t, from, "build", 0o755)
	old := readStagedBuild
	t.Cleanup(func() { readStagedBuild = old })

	for _, id := range []string{"", "unknown"} {
		readStagedBuild = func(string) string { return id }
		o := ClusterOptions{From: from}
		if err := pinStagedBinary(&o); err == nil || !strings.Contains(err.Error(), "not started") {
			t.Errorf("build %q: pinStagedBinary = %v, want a refusal", id, err)
		}
	}
	readStagedBuild = func(string) string { return "0123abcd" }
	oldVer := readStagedVersion
	t.Cleanup(func() { readStagedVersion = oldVer })
	// A staged binary that cannot report its release would have every node
	// stamped with the wrong one.
	readStagedVersion = func(string) (string, error) { return "", errors.New("exec format error") }
	o := ClusterOptions{From: from}
	if err := pinStagedBinary(&o); err == nil || !strings.Contains(err.Error(), "does not report its version") {
		t.Errorf("pinStagedBinary = %v, want a refusal for a binary with no version", err)
	}
	readStagedVersion = func(string) (string, error) { return "v1.37.1-stratum.0", nil }
	o = ClusterOptions{From: from}
	if err := pinStagedBinary(&o); err != nil || o.expectBuild != "0123abcd" || len(o.expectSHA256) != 64 || o.arch == "" {
		t.Errorf("pinStagedBinary = %v, %+v; want the build, a sha256 and this arch", err, o)
	}
	if o.stratumVersion != "v1.37.1-stratum.0" {
		t.Errorf("stratumVersion = %q, want the staged binary's, not this orchestrator's", o.stratumVersion)
	}
	o = ClusterOptions{From: filepath.Join(dir, "missing")}
	if err := pinStagedBinary(&o); err == nil {
		t.Errorf("pinStagedBinary hashed a missing binary")
	}
}

// The version is read by running the staged binary, so it is read before the
// hash: whatever the run leaves at --from is what gets pinned.
func TestPinStagedBinaryReadsTheVersionBeforeHashing(t *testing.T) {
	from := filepath.Join(t.TempDir(), "stratum-next")
	writeFile(t, from, "before", 0o755)
	oldBuild, oldVer := readStagedBuild, readStagedVersion
	t.Cleanup(func() { readStagedBuild, readStagedVersion = oldBuild, oldVer })
	readStagedBuild = func(string) string { return "0123abcd" }
	readStagedVersion = func(p string) (string, error) {
		writeFile(t, p, "after", 0o755)
		return "v1.37.1-stratum.0", nil
	}

	o := ClusterOptions{From: from}
	if err := pinStagedBinary(&o); err != nil {
		t.Fatalf("pinStagedBinary = %v", err)
	}
	want := sha256.Sum256([]byte("after"))
	if o.expectSHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("expectSHA256 = %s, want the hash of the bytes left after the version read", o.expectSHA256)
	}
}

// The kubelet's own report, "v1.37.1-stratum", is at a bare --target v1.37.1.
// Compared raw it never was, so the plan could not show a finished or an
// interrupted node for what it was.
func TestNodeKubeletVersionDropsTheBuildSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"v1.37.1-stratum": "v1.37.1",
		"v1.37.1":         "v1.37.1",
		"v1.37.1-rc.1":    "v1.37.1-rc.1", // not at target v1.37.1
		"garbage":         "garbage",
	} {
		if got := NodeKubeletVersion(in); got != want {
			t.Errorf("NodeKubeletVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// The check Job can take two minutes; a lease lost while it ran stops the node
// before its cordon, whatever the check answered.
func TestNodeRunsThisBuildRechecksTheLeaseAfterTheJob(t *testing.T) {
	g := &leaseGuard{}
	o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-rc3", lease: g}
	old := runExitCode
	runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) {
		g.mu.Lock()
		g.lost = errors.New("lease lost")
		g.mu.Unlock()
		return ExitUpgradeNeeded, nil
	}
	t.Cleanup(func() { runExitCode = old })

	current, err := nodeRunsThisBuild(context.Background(), io.Discard, nil, Node{Name: "w1", Version: "v1.37.1"}, o)
	if err == nil || !strings.Contains(err.Error(), "lease lost") || current {
		t.Errorf("nodeRunsThisBuild = %v, %v; want the lost lease, before any cordon", current, err)
	}
}

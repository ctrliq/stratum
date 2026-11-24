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

// Anything but exit 0 or ExitUpgradeNeeded is not an answer: no skip, no drain.
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

// The check must not be able to answer for another binary than the upgrade runs.
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

// Another architecture runs another binary of the same build, so bytes pin only this arch.
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

// Unpinned, each node would answer for whatever is staged at --from.
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
	// A staged binary that cannot report its release would have every node stamped with the wrong one.
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

// Reading the version runs the binary, so it can leave other bytes at --from.
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

// The kubelet reports "v1.37.1-stratum", which is at a bare --target v1.37.1.
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

// A lease lost during the two-minute check Job must stop the node before its cordon.
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

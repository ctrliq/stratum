package upgrade

// The cluster-wide prerequisite pass: before the rollout cordons or drains
// anything, every Ready node runs the staged binary's --check, and the run
// refuses, naming every failing node at once, when any would take the upgrade
// without container-selinux. The tests drive UpgradeCluster through the
// package's seams (see duallease_test.go for the fixture); no Job ever runs.

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/nodejob"
)

// fiveNodesFixture is two Ready control planes and three Ready workers, all
// behind the target, the smallest HA cluster that passes preflight.
func fiveNodesFixture() []runtime.Object {
	return []runtime.Object{
		upgradeNodeFixture("cp1", true, "10.0.0.9"),
		upgradeNodeFixture("cp2", true, "10.0.0.8"),
		upgradeNodeFixture("w1", false, "10.0.0.7"),
		upgradeNodeFixture("w2", false, "10.0.0.6"),
		upgradeNodeFixture("w3", false, "10.0.0.5"),
	}
}

// answerLogsWithExitCode answers the prerequisite pass's log-reading Job with
// whatever runExitCode answers at the time, and an empty log, so a test that
// answers through runExitCode answers the pass too.
func answerLogsWithExitCode(t *testing.T) {
	t.Helper()
	old := runExitCodeLogs
	runExitCodeLogs = func(ctx context.Context, cs kubernetes.Interface, s nodejob.Spec) (int, string, error) {
		code, err := runExitCode(ctx, cs, s)
		return code, "", err
	}
	t.Cleanup(func() { runExitCodeLogs = old })
}

// answerPerNode answers the prerequisite check per node: ExitPrereqMissing
// for each node in refusing, 10 for every other node, recording what it ran.
func answerPerNode(t *testing.T, refusing ...string) *[]nodejob.Spec {
	t.Helper()
	answerLogsWithExitCode(t)
	var ran []nodejob.Spec
	old := runExitCode
	runExitCode = func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) (int, error) {
		ran = append(ran, s)
		if slices.Contains(refusing, s.Node) {
			return ExitPrereqMissing, nil
		}
		return ExitUpgradeNeeded, nil
	}
	t.Cleanup(func() { runExitCode = old })
	return &ran
}

// The pass runs before any node is touched: a node whose own upgrade would
// refuse a missing container-selinux stops the rollout with every refusing
// node named, the cluster without a cordon patch or an upgrade Job. The
// refusing nodes span the whole ordered list, first control plane and last
// worker, so a pass that stopped at the first refusal is caught naming only
// one of them.
func TestUpgradeClusterRefusesBeforeAnyNodeOnMissingSELinuxPrereq(t *testing.T) {
	cs := fake.NewSimpleClientset(fiveNodesFixture()...)
	fastOrchestrator(t, nil)
	oldJob := runNodeJob
	runNodeJob = func(context.Context, kubernetes.Interface, nodejob.Spec) error {
		t.Error("an upgrade Job ran although the prerequisite pass refused the run")
		return nil
	}
	t.Cleanup(func() { runNodeJob = oldJob })
	ran := answerPerNode(t, "cp1", "w3")

	err := UpgradeCluster(io.Discard, ClusterOptions{Target: "v1.37.1", From: stagedBinary(t), Client: cs})
	if err == nil {
		t.Fatal("the rollout ran although cp1 and w3 lack container-selinux")
	}
	for _, want := range []string{"cp1", "w3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s: %v", want, err)
		}
	}
	for _, other := range []string{"cp2", "w1", "w2"} {
		if strings.Contains(err.Error(), other) {
			t.Errorf("the refusal names %s, which passed: %v", other, err)
		}
	}
	for _, want := range []string{"preflight", "container-selinux", "policycoreutils", "libselinux-utils", "spec.selinux: false", "cordoned or drained"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is missing %q: %v", want, err)
		}
	}
	for _, s := range *ran {
		if s.Kind != "prerequisite check" {
			t.Errorf("the pass ran %q on %s, not a prerequisite check", s.Kind, s.Node)
		}
	}
	if n := len(*ran); n != 5 {
		t.Errorf("the pass ran %d checks, want one per node", n)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "nodes" {
			t.Errorf("a node was cordoned although the prerequisite pass refused the run: %v", a)
		}
		if a.GetVerb() == "create" && a.GetResource().Resource == "jobs" {
			t.Errorf("a Job was created by the refused run: %v", a)
		}
	}
}

// Nodes that pass (current or needing the upgrade) let the run through; a
// NotReady node cannot run the check, so it is warned about and left to its
// own upgrade's refusal; a check that answers nothing at all stops the run.
func TestPreflightNodePrereqsSkipsNotReadyAndPassesCurrent(t *testing.T) {
	answerLogsWithExitCode(t)
	nodes := []Node{
		{Name: "w1", Ready: true},  // on this build
		{Name: "w2", Ready: true},  // needs the upgrade
		{Name: "w3", Ready: false}, // cannot be asked
	}

	t.Run("current, needing the upgrade and NotReady pass", func(t *testing.T) {
		var ran []string
		old := runExitCode
		runExitCode = func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) (int, error) {
			ran = append(ran, s.Node)
			if s.Node == "w1" {
				return 0, nil
			}
			return ExitUpgradeNeeded, nil
		}
		t.Cleanup(func() { runExitCode = old })

		var buf strings.Builder
		if err := preflightNodePrereqs(context.Background(), &buf, nil, nodes, ClusterOptions{}); err != nil {
			t.Fatalf("preflightNodePrereqs: %v", err)
		}
		if len(ran) != 2 {
			t.Errorf("checks ran on %v, want only the Ready nodes w1 and w2", ran)
		}
		out := buf.String()
		if !strings.Contains(out, "w3") || !strings.Contains(out, "NotReady") {
			t.Errorf("the NotReady node is not warned about: %q", out)
		}
		if strings.Contains(out, "w1") || strings.Contains(out, "w2") {
			t.Errorf("passing nodes are reported: %q", out)
		}
	})

	t.Run("a check that does not answer stops the run", func(t *testing.T) {
		old := runExitCode
		runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) {
			return 1, nil
		}
		t.Cleanup(func() { runExitCode = old })

		// Exit 1 is the build check's refusal (a staged binary that is not
		// the pinned one, another upgrade on the node), not a missing
		// package, so it is worded as the build check words it.
		err := preflightNodePrereqs(context.Background(), io.Discard, nil, nodes[:1], ClusterOptions{})
		want := "check w1's stratum build: the check exited 1 (logs: kubectl -n kube-system logs job/stratum-upgrade-w1); nothing was cordoned or drained"
		if err == nil || err.Error() != want {
			t.Errorf("preflightNodePrereqs = %v; want %q", err, want)
		}
	})
}

// A check that could not ask its question lets the node through with only a
// WARN line in its Job's log. The pass relays that line, naming the node,
// before the rollout drains anything, and leaves out the log's other lines.
func TestPreflightNodePrereqsRelaysTheCheckWarnings(t *testing.T) {
	old := runExitCodeLogs
	runExitCodeLogs = func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) (int, string, error) {
		if s.Node == "w2" {
			return ExitUpgradeNeeded, "stratum upgrade: this node needs the upgrade\n  WARN: could not read this node's SELinux settings: boom; not checking for container-selinux\n", nil
		}
		return ExitUpgradeNeeded, "stratum upgrade: this node needs the upgrade\n", nil
	}
	t.Cleanup(func() { runExitCodeLogs = old })

	var buf strings.Builder
	nodes := []Node{{Name: "w1", Ready: true}, {Name: "w2", Ready: true}}
	if err := preflightNodePrereqs(context.Background(), &buf, nil, nodes, ClusterOptions{}); err != nil {
		t.Fatalf("preflightNodePrereqs: %v", err)
	}
	out := buf.String()
	if want := "  w2: WARN: could not read this node's SELinux settings: boom; not checking for container-selinux\n"; !strings.Contains(out, want) {
		t.Errorf("the check's warning is not relayed with its node; output:\n%s\nwant a line %q", out, want)
	}
	if strings.Contains(out, "w1") || strings.Contains(out, "needs the upgrade") {
		t.Errorf("the pass relayed more than the warnings:\n%s", out)
	}
}

// A check whose exit status is known but whose log cannot be read, as on a
// node whose kubelet serving certificate the apiserver does not trust yet,
// still decides by its exit status: a passing node goes through with a WARN
// naming it, and a refusing one is still refused. Only the relayed warnings
// are lost.
func TestPreflightNodePrereqsDecidesWithoutTheLog(t *testing.T) {
	unread := fmt.Errorf("prerequisite check Job x exited 10, and %w: kubelet: x509: certificate signed by unknown authority", nodejob.ErrLogUnread)
	old := runExitCodeLogs
	runExitCodeLogs = func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) (int, string, error) {
		if s.Node == "w2" {
			return ExitPrereqMissing, "", unread
		}
		return ExitUpgradeNeeded, "", unread
	}
	t.Cleanup(func() { runExitCodeLogs = old })

	var buf strings.Builder
	err := preflightNodePrereqs(context.Background(), &buf, nil, []Node{{Name: "w1", Ready: true}}, ClusterOptions{})
	if err != nil {
		t.Fatalf("preflightNodePrereqs refused a passing node whose log it could not read: %v", err)
	}
	for _, want := range []string{"WARN: w1:", "unknown authority", "warnings its check printed are not shown"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the warning is missing %q:\n%s", want, buf.String())
		}
	}

	err = preflightNodePrereqs(context.Background(), io.Discard, nil, []Node{{Name: "w1", Ready: true}, {Name: "w2", Ready: true}}, ClusterOptions{})
	if err == nil || !strings.Contains(err.Error(), "w2") || strings.Contains(err.Error(), "w1") {
		t.Errorf("preflightNodePrereqs = %v; want a refusal naming only w2", err)
	}
}

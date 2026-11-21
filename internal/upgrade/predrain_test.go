package upgrade

import (
	"io"

	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodejob"
)

var errStopAtJob = errors.New("stopped at the upgrade Job")

func preDrain(t *testing.T, n Node, code int, checkErr error) (out nodeOutcome, cordoned bool, checks, upgrades int, err error) {
	t.Helper()
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.Name}})

	oldCheck, oldRun := runExitCode, runNodeJob
	runExitCode = func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) (int, error) {
		if !strings.Contains(strings.Join(s.Argv, " "), "--check") {
			t.Errorf("the pre-drain Job is not a --check: %q", s.Argv)
		}
		if s.Critical {
			t.Errorf("the pre-drain check is critical: it could evict workloads on a node it then leaves undrained")
		}
		checks++
		return code, checkErr
	}
	runNodeJob = func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) error {
		if strings.Contains(strings.Join(s.Argv, " "), "--check") {
			t.Errorf("the upgrade Job is a --check: %q", s.Argv)
		}
		if !s.Critical {
			t.Errorf("the upgrade Job is not critical: under node pressure the kubelet would refuse it")
		}
		upgrades++
		return errStopAtJob
	}
	t.Cleanup(func() { runExitCode, runNodeJob = oldCheck, oldRun })

	o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-next", DrainGraceSec: 1}
	out, err = upgradeOneNode(io.Discard, context.Background(), cs, n, o)
	got, gerr := cs.CoreV1().Nodes().Get(context.Background(), n.Name, metav1.GetOptions{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	return out, got.Spec.Unschedulable, checks, upgrades, err
}

func TestUpgradeOneNodeDrainsOnlyWhatTheCheckSays(t *testing.T) {
	atTarget := Node{Name: "w1", Version: "v1.37.1", Ready: true}

	t.Run("already on this build: returned before any cordon", func(t *testing.T) {
		out, cordoned, checks, upgrades, err := preDrain(t, atTarget, 0, nil)
		if err != nil || out != nodeCurrent || cordoned || checks != 1 || upgrades != 0 {
			t.Errorf("out=%v err=%v cordoned=%v checks=%d upgrades=%d; want current, not cordoned, one check, no upgrade", out, err, cordoned, checks, upgrades)
		}
	})
	t.Run("another build: cordoned and upgraded", func(t *testing.T) {
		_, cordoned, checks, upgrades, err := preDrain(t, atTarget, ExitUpgradeNeeded, nil)
		if !errors.Is(err, errStopAtJob) || !cordoned || checks != 1 || upgrades != 1 {
			t.Errorf("err=%v cordoned=%v checks=%d upgrades=%d; want it to reach the upgrade Job, cordoned", err, cordoned, checks, upgrades)
		}
	})
	t.Run("the check failed: untouched", func(t *testing.T) {
		_, cordoned, _, upgrades, err := preDrain(t, atTarget, 1, nil)
		if err == nil || errors.Is(err, errStopAtJob) || cordoned || upgrades != 0 {
			t.Errorf("err=%v cordoned=%v upgrades=%d; want an error, not cordoned, no upgrade", err, cordoned, upgrades)
		}
	})
	t.Run("the check gave no answer: untouched", func(t *testing.T) {
		_, cordoned, _, upgrades, err := preDrain(t, atTarget, 0, errors.New("build check Job did not complete in time"))
		if err == nil || cordoned || upgrades != 0 {
			t.Errorf("err=%v cordoned=%v upgrades=%d; want an error, not cordoned, no upgrade", err, cordoned, upgrades)
		}
	})
	t.Run("behind target: checked, then upgraded", func(t *testing.T) {
		_, cordoned, checks, upgrades, err := preDrain(t, Node{Name: "w1", Version: "v1.37.0", Ready: true}, ExitUpgradeNeeded, nil)
		if !errors.Is(err, errStopAtJob) || !cordoned || checks != 1 || upgrades != 1 {
			t.Errorf("err=%v cordoned=%v checks=%d upgrades=%d; want one check, then the upgrade Job", err, cordoned, checks, upgrades)
		}
	})
	t.Run("behind target, another binary staged: untouched", func(t *testing.T) {
		_, cordoned, _, upgrades, err := preDrain(t, Node{Name: "w1", Version: "v1.37.0", Ready: true}, 1, nil)
		if err == nil || cordoned || upgrades != 0 {
			t.Errorf("err=%v cordoned=%v upgrades=%d; want an error, not cordoned, no upgrade", err, cordoned, upgrades)
		}
	})
	t.Run("behind target, NotReady: upgraded without a check", func(t *testing.T) {
		_, cordoned, checks, upgrades, err := preDrain(t, Node{Name: "w1", Version: "v1.37.0", Ready: false}, 0, nil)
		if !errors.Is(err, errStopAtJob) || !cordoned || checks != 0 || upgrades != 1 {
			t.Errorf("err=%v cordoned=%v checks=%d upgrades=%d; want no check and the upgrade Job", err, cordoned, checks, upgrades)
		}
	})
	t.Run("NotReady at target: left alone, reported unchecked", func(t *testing.T) {
		n := Node{Name: "w1", Version: "v1.37.1", Ready: false}
		out, cordoned, checks, upgrades, err := preDrain(t, n, 0, nil)
		if err != nil || out != nodeUnchecked || cordoned || checks != 0 || upgrades != 0 {
			t.Errorf("out=%v err=%v cordoned=%v checks=%d upgrades=%d; want unchecked with no check, no cordon", out, err, cordoned, checks, upgrades)
		}
	})
	t.Run("interrupted at target: checked, then finished", func(t *testing.T) {
		n := Node{Name: "w1", Version: "v1.37.1", InProgress: "v1.37.1", Ready: true}
		_, cordoned, checks, upgrades, err := preDrain(t, n, ExitUpgradeNeeded, nil)
		if !errors.Is(err, errStopAtJob) || !cordoned || checks != 1 || upgrades != 1 {
			t.Errorf("err=%v cordoned=%v checks=%d upgrades=%d; want one check, then the upgrade Job", err, cordoned, checks, upgrades)
		}
	})
	t.Run("in progress to target, API version stale, Job already done: still finished", func(t *testing.T) {
		n := Node{Name: "w1", Version: "v1.37.0", InProgress: "v1.37.1", Ready: true}
		out, cordoned, checks, upgrades, err := preDrain(t, n, 0, nil)
		if out == nodeCurrent || !errors.Is(err, errStopAtJob) || !cordoned || checks != 1 || upgrades != 1 {
			t.Errorf("out=%v err=%v cordoned=%v checks=%d upgrades=%d; want one check, then the upgrade Job", out, err, cordoned, checks, upgrades)
		}
	})
	t.Run("interrupted at target, Job already done: still finished, not skipped", func(t *testing.T) {
		n := Node{Name: "w1", Version: "v1.37.1", InProgress: "v1.37.1", Ready: true}
		out, cordoned, checks, upgrades, err := preDrain(t, n, 0, nil)
		if out == nodeCurrent || !errors.Is(err, errStopAtJob) || !cordoned || checks != 1 || upgrades != 1 {
			t.Errorf("out=%v err=%v cordoned=%v checks=%d upgrades=%d; want one check, then the upgrade Job", out, err, cordoned, checks, upgrades)
		}
	})
}

func TestUpgradeOneNodeRecordsTheUpgradeBeforeItsJob(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "w1"}})
	oldCheck, oldRun := runExitCode, runNodeJob
	runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) { return ExitUpgradeNeeded, nil }
	var recorded string
	runNodeJob = func(ctx context.Context, cs kubernetes.Interface, _ nodejob.Spec) error {
		nd, _ := cs.CoreV1().Nodes().Get(ctx, "w1", metav1.GetOptions{})
		recorded = nd.Annotations[k8sutil.UpgradeInProgressAnnotation]
		return errStopAtJob
	}
	t.Cleanup(func() { runExitCode, runNodeJob = oldCheck, oldRun })

	_, _ = upgradeOneNode(io.Discard, context.Background(), cs, Node{Name: "w1", Version: "v1.37.1", Ready: true}, ClusterOptions{Target: "v1.37.1", From: "/x", DrainGraceSec: 1})
	if recorded != "v1.37.1" {
		t.Errorf("in-progress record when the upgrade Job started = %q, want v1.37.1", recorded)
	}
}

func TestPlanStateMatchesWhatTheRunDoes(t *testing.T) {
	const target = "v1.37.1"
	for _, tt := range []struct {
		n    Node
		want string
	}{
		{Node{Version: "v1.37.0", Ready: true}, "upgrade"},
		{Node{Version: "v1.37.0", Ready: false}, "upgrade"},
		{Node{Version: target, Ready: true}, "check build"},
		{Node{Version: target, Ready: false}, "skip (NotReady"},
		{Node{Version: target, InProgress: target, Ready: false}, "finish"},
	} {
		if got := planState(tt.n, target); !strings.HasPrefix(got, tt.want) {
			t.Errorf("planState(%+v) = %q, want %q...", tt.n, got, tt.want)
		}
	}
}

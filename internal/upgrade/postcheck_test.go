package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodejob"
)

func postCheckCM(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: PostCheckConfigMap, Namespace: cuNamespace},
		Data:       data,
	}
}

func TestParsePostChecks(t *testing.T) {
	good := "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag, run, \"1\"]\n"
	for _, tc := range []struct {
		name    string
		cm      *corev1.ConfigMap
		wantErr string
	}{
		{"valid", postCheckCM(map[string]string{"b": good, "a": good + "timeoutSeconds: 60\n"}), ""},
		{"no selector", postCheckCM(map[string]string{"x": "command: [/bin/true]\n"}), "nodeSelector is required"},
		{"bad selector", postCheckCM(map[string]string{"x": "nodeSelector: 'a=b=c'\ncommand: [/bin/true]\n"}), "nodeSelector"},
		{"no command", postCheckCM(map[string]string{"x": "nodeSelector: a=b\n"}), "command is required"},
		{"relative command", postCheckCM(map[string]string{"x": "nodeSelector: a=b\ncommand: [diag]\n"}), "not an absolute path"},
		{"negative timeout", postCheckCM(map[string]string{"x": good + "timeoutSeconds: -1\n"}), "timeoutSeconds -1"},
		{"timeout over a day", postCheckCM(map[string]string{"x": good + "timeoutSeconds: 86401\n"}), "timeoutSeconds 86401"},
		{"timeout that wraps a duration", postCheckCM(map[string]string{"x": good + "timeoutSeconds: 18446744074\n"}), "timeoutSeconds 18446744074"},
		{"unknown field", postCheckCM(map[string]string{"x": good + "image: busybox\n"}), "unknown field"},
		{"binaryData", &corev1.ConfigMap{BinaryData: map[string][]byte{"x": []byte(good)}}, "binaryData"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePostChecks(tc.cm)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
				t.Fatalf("checks = %+v, want a then b", got)
			}
			if got[0].timeout() != time.Minute || got[1].timeout() != defaultPostCheckTimeout {
				t.Errorf("timeouts = %s, %s; want 1m and the default", got[0].timeout(), got[1].timeout())
			}
			if !slices.Equal(got[1].Command, []string{"/usr/bin/diag", "run", "1"}) {
				t.Errorf("command = %q", got[1].Command)
			}
		})
	}
}

func TestLoadPostChecksAbsentMeansNoneAndUnreadableRefuses(t *testing.T) {
	checks, err := loadPostChecks(context.Background(), fake.NewSimpleClientset())
	if err != nil || checks != nil {
		t.Fatalf("no ConfigMap: checks=%v err=%v, want none", checks, err)
	}
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, PostCheckConfigMap, errors.New("denied"))
	})
	if _, err := loadPostChecks(context.Background(), cs); err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("a ConfigMap that cannot be read: %v, want the refusal", err)
	}
}

func gpuCheck(t *testing.T) postCheck {
	t.Helper()
	got, err := parsePostChecks(postCheckCM(map[string]string{
		"accel-diag": "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag, run, \"1\"]\ntimeoutSeconds: 300\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return got[0]
}

// stubUpgradeJob makes the upgrade Job succeed and, as a controller's Job
// does unless told to keep it, release the upgrade's cordon on its way out.
func stubUpgradeJob(t *testing.T, isCP bool) {
	t.Helper()
	old := runNodeJob
	t.Cleanup(func() { runNodeJob = old })
	runNodeJob = func(ctx context.Context, cs kubernetes.Interface, s nodejob.Spec) error {
		if isCP && !slices.Contains(s.Argv, "--keep-cordon") {
			if _, err := releaseUpgradeCordon(ctx, cs, s.Node, "v1.37.1"); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
}

func stubPostCheck(t *testing.T, fn func(context.Context, kubernetes.Interface, nodejob.Spec) error) {
	t.Helper()
	old := runPostUpgradeCheck
	t.Cleanup(func() { runPostUpgradeCheck = old })
	runPostUpgradeCheck = fn
}

func runPostCheckNode(cs kubernetes.Interface, isCP, keepCPCordon bool, checks []postCheck) error {
	n := Node{Name: "cp1", Version: "v1.36.5", Ready: true, IsCP: isCP}
	o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-next", DrainGraceSec: 1,
		postChecks: checks, keepCPCordon: keepCPCordon}
	_, err := upgradeOneNode(io.Discard, context.Background(), cs, n, o)
	return err
}

// The check runs on a matching node after its upgrade and before the node is
// stamped and uncordoned, as a node Job running the command on the host.
func TestPostCheckRunsOnAMatchingNodeBeforeItIsFinished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
		changeNode(t, cs, func(n *corev1.Node) { n.Labels = map[string]string{"example.com/accel": "true"} })
		stubUpgradeJob(t, false)
		var got []nodejob.Spec
		stubPostCheck(t, func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) error {
			n := getNode(t, cs)
			if !n.Spec.Unschedulable || n.Annotations[k8sutil.VersionAnnotation] != "" ||
				n.Annotations[k8sutil.UpgradeInProgressAnnotation] != "v1.37.1" {
				t.Errorf("the check ran on a node already finished: unschedulable=%v annotations=%v", n.Spec.Unschedulable, n.Annotations)
			}
			got = append(got, s)
			return nil
		})
		if err := runPostCheckNode(cs, false, false, []postCheck{gpuCheck(t)}); err != nil {
			t.Fatalf("upgradeOneNode: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("the check ran %d times, want once", len(got))
		}
		s := got[0]
		wantArgv := []string{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "/usr/bin/diag", "run", "1"}
		if !slices.Equal(s.Argv, wantArgv) || s.Node != "cp1" || s.Name != jobName("cp1") || s.LabelKey != upgradeJobLabel ||
			s.Namespace != cuNamespace || s.ActiveDeadline != 5*time.Minute || s.PollAttempts != 72 {
			t.Errorf("check Job = %+v", s)
		}
		n := getNode(t, cs)
		if n.Spec.Unschedulable || n.Annotations[k8sutil.VersionAnnotation] != "v1.37.1" {
			t.Errorf("after a passed check: unschedulable=%v annotations=%v, want the node finished", n.Spec.Unschedulable, n.Annotations)
		}
	})
}

// A node the selector does not match is finished without the check.
func TestPostCheckSkipsANodeItDoesNotSelect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
		changeNode(t, cs, func(n *corev1.Node) { n.Labels = map[string]string{"example.com/accel": "false"} })
		stubUpgradeJob(t, false)
		stubPostCheck(t, func(context.Context, kubernetes.Interface, nodejob.Spec) error {
			t.Error("the check ran on a node its selector does not match")
			return nil
		})
		if err := runPostCheckNode(cs, false, false, []postCheck{gpuCheck(t)}); err != nil {
			t.Fatalf("upgradeOneNode: %v", err)
		}
		if n := getNode(t, cs); n.Spec.Unschedulable || n.Annotations[k8sutil.VersionAnnotation] != "v1.37.1" {
			t.Errorf("unschedulable=%v annotations=%v, want the node finished", n.Spec.Unschedulable, n.Annotations)
		}
	})
}

// A failed check halts the run with the node unstamped and cordoned. A worker,
// and a control plane of several, whose Job leaves its cordon, are cordoned
// while the check runs; the only control plane, which its own Job uncordoned,
// is cordoned again. A re-run finishes the node, runs the check again, and
// uncordons the node only once it passes.
func TestPostCheckFailureHoldsTheNodeUntilARerunPasses(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		isCP, keep, uncordons bool
	}{
		{"worker", false, false, false},
		{"control plane of several", true, true, false},
		{"the only control plane", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
				changeNode(t, cs, func(n *corev1.Node) { n.Labels = map[string]string{"example.com/accel": "true"} })
				stubUpgradeJob(t, tc.isCP)
				stubPostCheck(t, func(context.Context, kubernetes.Interface, nodejob.Spec) error {
					if got := !getNode(t, cs).Spec.Unschedulable; got != tc.uncordons {
						t.Errorf("schedulable while the check runs = %v, want %v", got, tc.uncordons)
					}
					return fmt.Errorf("post-upgrade check accel-diag Job stratum-upgrade-cp1 failed: %w", nodejob.ErrNotPassed)
				})
				err := runPostCheckNode(cs, tc.isCP, tc.keep, []postCheck{gpuCheck(t)})
				if !errors.Is(err, errPostCheckFailed) {
					t.Fatalf("err = %v, want the failed check", err)
				}
				for _, want := range []string{"accel-diag", "logs job/stratum-upgrade-cp1", "left cordoned", "upgrade --cluster --target v1.37.1"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error missing %q:\n%v", want, err)
					}
				}
				n := getNode(t, cs)
				if !n.Spec.Unschedulable || n.Annotations[k8sutil.VersionAnnotation] != "" ||
					n.Annotations[k8sutil.UpgradeInProgressAnnotation] != "v1.37.1" ||
					n.Annotations[k8sutil.UpgradeCordonAnnotation] != cordonClaim("v1.37.1", "") {
					t.Errorf("unschedulable=%v annotations=%v, want it cordoned under the upgrade's claim and unfinished", n.Spec.Unschedulable, n.Annotations)
				}

				reran := 0
				stubPostCheck(t, func(context.Context, kubernetes.Interface, nodejob.Spec) error {
					reran++
					if got := !getNode(t, cs).Spec.Unschedulable; got != tc.uncordons {
						t.Errorf("re-run: schedulable while the check runs = %v, want %v", got, tc.uncordons)
					}
					return nil
				})
				if err := runPostCheckNode(cs, tc.isCP, tc.keep, []postCheck{gpuCheck(t)}); err != nil {
					t.Fatalf("re-run: %v", err)
				}
				if reran != 1 {
					t.Errorf("re-run ran the check %d times, want once", reran)
				}
				n = getNode(t, cs)
				if n.Spec.Unschedulable || n.Annotations[k8sutil.VersionAnnotation] != "v1.37.1" ||
					n.Annotations[k8sutil.UpgradeInProgressAnnotation] != "" || n.Annotations[k8sutil.UpgradeCordonAnnotation] != "" {
					t.Errorf("after a re-run whose check passed: unschedulable=%v annotations=%v, want the node finished", n.Spec.Unschedulable, n.Annotations)
				}
			})
		})
	}
}

// A check Job that never ran, as one the run could not create or stopped
// waiting on, is not a failed check: the run halts with its ordinary recovery
// steps, and the node is not held as having failed.
func TestPostCheckThatDidNotRunIsNotAFailedCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
		changeNode(t, cs, func(n *corev1.Node) { n.Labels = map[string]string{"example.com/accel": "true"} })
		stubUpgradeJob(t, false)
		stubPostCheck(t, func(context.Context, kubernetes.Interface, nodejob.Spec) error {
			return errors.New("a post-upgrade check accel-diag Job stratum-upgrade-cp1 is already running; refusing to replace it")
		})
		err := runPostCheckNode(cs, false, false, []postCheck{gpuCheck(t)})
		if err == nil || errors.Is(err, errPostCheckFailed) {
			t.Fatalf("err = %v, want an error that is not a failed check", err)
		}
		for _, want := range []string{"accel-diag", "refusing to replace", "To recover"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error missing %q:\n%v", want, err)
			}
		}
	})
}

// The run keeps a control plane's cordon past its Job when a check is declared,
// unless the control plane is the cluster's only one.
func TestKeepsCPCordon(t *testing.T) {
	cp := Node{Name: "cp1", IsCP: true}
	checks := []postCheck{gpuCheck(t)}
	for _, tc := range []struct {
		name   string
		checks []postCheck
		nodes  []Node
		want   bool
	}{
		{"checks on three control planes", checks, []Node{cp, {Name: "cp2", IsCP: true}, {Name: "cp3", IsCP: true}}, true},
		{"checks on one control plane and a worker", checks, []Node{cp, {Name: "w1"}}, false},
		{"no checks", nil, []Node{cp, {Name: "cp2", IsCP: true}}, false},
	} {
		if got := keepsCPCordon(tc.checks, tc.nodes); got != tc.want {
			t.Errorf("%s: keepsCPCordon = %v, want %v", tc.name, got, tc.want)
		}
	}
	o := ClusterOptions{From: "/b", Target: "v1.37.1", keepCPCordon: true}
	if !slices.Contains(nodeUpgradeArgv(o, cp), "--keep-cordon") {
		t.Error("a control plane's upgrade Job is not told to keep its cordon")
	}
	if slices.Contains(nodeUpgradeArgv(o, Node{Name: "w1"}), "--keep-cordon") {
		t.Error("a worker's upgrade Job is told to keep its cordon")
	}
	o.keepCPCordon = false
	if slices.Contains(nodeUpgradeArgv(o, cp), "--keep-cordon") {
		t.Error("the only control plane's upgrade Job is told to keep its cordon")
	}
}

func postCheckNode(t *testing.T) (kubernetes.Interface, Node) {
	t.Helper()
	n := upgradeNodeFixture("w1", false, "10.0.0.20")
	n.Labels["example.com/accel"] = "true"
	return fake.NewSimpleClientset(n), Node{Name: "w1", Version: "v1.37.1", Ready: true}
}

// A run that loses its lease after one check does not start the next: it
// stops with the lease error, not as a failed check.
func TestPostChecksStopOnALostLeaseBeforeTheNextCheck(t *testing.T) {
	checks, err := parsePostChecks(postCheckCM(map[string]string{
		"a-first":  "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag, one]\n",
		"b-second": "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag, two]\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	cs, n := postCheckNode(t)
	g := &leaseGuard{}
	lost := errors.New("lease lost")
	var ran []string
	stubPostCheck(t, func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) error {
		ran = append(ran, s.Kind)
		g.mu.Lock()
		g.lost = lost
		g.mu.Unlock()
		return nil
	})
	o := ClusterOptions{Target: "v1.37.1", postChecks: checks, lease: g}
	err = runPostChecks(context.Background(), io.Discard, cs, n, jobName(n.Name), o)
	if !errors.Is(err, lost) || errors.Is(err, errPostCheckFailed) {
		t.Fatalf("err = %v, want the lost lease and not a failed check", err)
	}
	if !slices.Equal(ran, []string{"post-upgrade check a-first"}) {
		t.Errorf("checks run = %q, want only the first", ran)
	}
}

// A check without timeoutSeconds gets a 900-second deadline.
func TestPostCheckDefaultTimeoutIsFifteenMinutes(t *testing.T) {
	checks, err := parsePostChecks(postCheckCM(map[string]string{
		"accel-diag": "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag]\n",
		"zero":       "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag]\ntimeoutSeconds: 0\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	cs, n := postCheckNode(t)
	var got []nodejob.Spec
	stubPostCheck(t, func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) error {
		got = append(got, s)
		return nil
	})
	if err := runPostChecks(context.Background(), io.Discard, cs, n, jobName(n.Name), ClusterOptions{postChecks: checks}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("checks run = %d, want 2", len(got))
	}
	for _, s := range got {
		if s.ActiveDeadline != 900*time.Second || s.PollAttempts != postCheckPolls(900*time.Second) {
			t.Errorf("%s: deadline=%s polls=%d, want 900s and %d", s.Kind, s.ActiveDeadline, s.PollAttempts, postCheckPolls(900*time.Second))
		}
	}
}

// A check of the shortest timeout is polled past its deadline, not given up on
// before its Job is read.
func TestPostCheckPollsOutlastTheDeadline(t *testing.T) {
	for _, secs := range []int{1, 4, 900, 86400} {
		d := time.Duration(secs) * time.Second
		if polls := postCheckPolls(d); time.Duration(polls)*5*time.Second <= d {
			t.Errorf("timeoutSeconds %d: %d polls of 5s do not outlast the deadline", secs, polls)
		}
	}
}

func TestUpgradeClusterDryRunListsThePostChecks(t *testing.T) {
	objs := append(upgradeNodesFixture(), postCheckCM(map[string]string{
		"accel-diag": "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag, run, \"1\"]\n",
	}))
	cs := fake.NewSimpleClientset(objs...)
	out, err := runWriter(t, func(w io.Writer) error {
		return UpgradeCluster(w, ClusterOptions{Target: "v1.37.1", From: "/staged/stratum", Client: cs, DryRun: true})
	})
	if err != nil {
		t.Fatalf("UpgradeCluster --dry-run: %v", err)
	}
	if !strings.Contains(out, "\n  after each node's upgrade, before its uncordon: check accel-diag on nodes matching example.com/accel=true: /usr/bin/diag run 1\n") {
		t.Errorf("the dry run does not list the check:\n%s", out)
	}
}

// A ConfigMap the run cannot use refuses it before any node is touched.
func TestUpgradeClusterRefusesAMalformedPostCheckBeforeAnyChange(t *testing.T) {
	objs := append(upgradeNodesFixture(), postCheckCM(map[string]string{"accel-diag": "command: [diag]\n"}))
	cs := fake.NewSimpleClientset(objs...)
	err := UpgradeCluster(io.Discard, ClusterOptions{Target: "v1.37.1", From: "/staged/stratum", Client: cs})
	if err == nil || !strings.Contains(err.Error(), PostCheckConfigMap) || !strings.Contains(err.Error(), "accel-diag") {
		t.Fatalf("err = %v, want the ConfigMap's check named", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" && a.GetVerb() != "watch" {
			t.Errorf("the refused run wrote: %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// A full run with a declared check: on a cluster of several control planes,
// each control plane's upgrade Job is told to keep its cordon and stays
// cordoned through its check; with one control plane, or a staged binary
// without the flag, the Job uncordons the node itself and the run warns about
// the latter.
func TestUpgradeClusterKeepsEachControlPlaneCordonedForItsChecks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cps         []string
		binaryKeeps bool
		wantKeep    bool
		wantWarn    bool
	}{
		{"several control planes", []string{"cp1", "cp2", "cp3"}, true, true, false},
		{"one control plane", []string{"cp1"}, true, false, false},
		{"a staged binary without the flag", []string{"cp1", "cp2", "cp3"}, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := []runtime.Object{postCheckCM(map[string]string{
				"accel-diag": "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag, run, \"1\"]\n",
			})}
			for i, name := range append(slices.Clone(tc.cps), "w1") {
				n := upgradeNodeFixture(name, name != "w1", fmt.Sprintf("10.0.0.%d", 10+i))
				n.Labels["example.com/accel"] = "true"
				objs = append(objs, n)
			}
			cs := fake.NewSimpleClientset(objs...)
			fastOrchestrator(t, nil)
			probed := false
			swap(t, &stagedKeepsCordon, func(context.Context, string) (bool, error) {
				probed = true
				return tc.binaryKeeps, nil
			})
			keptBy := map[string]bool{}
			swap(t, &runNodeJob, func(ctx context.Context, cs kubernetes.Interface, s nodejob.Spec) error {
				keep := slices.Contains(s.Argv, "--keep-cordon")
				keptBy[s.Node] = keep
				if s.Node != "w1" && !keep {
					if _, err := releaseUpgradeCordon(ctx, cs, s.Node, "v1.37.1"); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			cordonedForCheck := map[string]bool{}
			stubPostCheck(t, func(ctx context.Context, cs kubernetes.Interface, s nodejob.Spec) error {
				n, err := cs.CoreV1().Nodes().Get(ctx, s.Node, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				cordonedForCheck[s.Node] = n.Spec.Unschedulable
				return nil
			})

			out, err := runWriter(t, func(w io.Writer) error {
				return UpgradeCluster(w, ClusterOptions{Target: "v1.37.1", From: stagedBinary(t), Client: cs, NodeChecks: []string{"accel-diag"}})
			})
			if err != nil {
				t.Fatalf("UpgradeCluster: %v", err)
			}
			if probed != (len(tc.cps) > 1) {
				t.Errorf("the staged binary was asked about --keep-cordon: %v, want %v", probed, len(tc.cps) > 1)
			}
			for _, name := range tc.cps {
				if got, ran := keptBy[name]; !ran || got != tc.wantKeep {
					t.Errorf("%s's upgrade Job: ran=%v --keep-cordon=%v, want --keep-cordon=%v", name, ran, got, tc.wantKeep)
				}
				if got, ran := cordonedForCheck[name]; !ran || got != tc.wantKeep {
					t.Errorf("%s during its check: ran=%v cordoned=%v, want cordoned=%v", name, ran, got, tc.wantKeep)
				}
			}
			if keptBy["w1"] {
				t.Error("the worker's upgrade Job is told to keep its cordon")
			}
			if !cordonedForCheck["w1"] {
				t.Error("the worker was schedulable during its check")
			}
			for _, name := range append(slices.Clone(tc.cps), "w1") {
				if n := getNodeNamed(t, cs, name); n.Spec.Unschedulable || n.Annotations[k8sutil.VersionAnnotation] != "v1.37.1" {
					t.Errorf("%s after the run: unschedulable=%v annotations=%v, want it finished", name, n.Spec.Unschedulable, n.Annotations)
				}
			}
			if got := strings.Contains(out, "cannot leave a control plane cordoned for its checks"); got != tc.wantWarn {
				t.Errorf("warned about the staged binary: %v, want %v:\n%s", got, tc.wantWarn, out)
			}
		})
	}
}

func getNodeNamed(t *testing.T, cs kubernetes.Interface, name string) *corev1.Node {
	t.Helper()
	n, err := cs.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAcceptedPostChecks(t *testing.T) {
	checks, err := parsePostChecks(postCheckCM(map[string]string{
		"accel-diag": "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag]\n",
		"fw-match":   "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/fw]\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		checks []postCheck
		named  []string
		want   string
	}{
		{"no checks, none named", nil, nil, ""},
		{"every check named", checks, []string{"fw-match", "accel-diag"}, ""},
		{"a check not named", checks, []string{"accel-diag"}, "declares the post-upgrade check(s) fw-match, which --node-checks does not name"},
		{"none named", checks, nil, "check(s) accel-diag, fw-match, which --node-checks does not name"},
		{"a name not declared", checks, []string{"accel-diag", "fw-match", "nccl"}, "--node-checks names nccl, which kube-system/stratum-upgrade-node-checks does not declare"},
		{"a name with no ConfigMap", nil, []string{"accel-diag"}, "--node-checks names accel-diag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := acceptedPostChecks(tc.checks, tc.named)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// Whoever can write the ConfigMap chooses a root command on nodes, so a check
// the operator did not name refuses the run before anything changes; the dry
// run still lists it and says the run would refuse.
func TestUpgradeClusterRefusesAnUnnamedPostCheckBeforeAnyChange(t *testing.T) {
	objs := append(upgradeNodesFixture(), postCheckCM(map[string]string{
		"accel-diag": "nodeSelector: example.com/accel=true\ncommand: [/bin/sh, -c, id]\n",
	}))
	cs := fake.NewSimpleClientset(objs...)
	err := UpgradeCluster(io.Discard, ClusterOptions{Target: "v1.37.1", From: "/staged/stratum", Client: cs})
	if err == nil || !strings.Contains(err.Error(), "accel-diag, which --node-checks does not name") || !strings.Contains(err.Error(), "no node was cordoned or upgraded") {
		t.Fatalf("err = %v, want the unnamed check refused before any change", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" && a.GetVerb() != "watch" {
			t.Errorf("the refused run wrote: %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
	out, err := runWriter(t, func(w io.Writer) error {
		return UpgradeCluster(w, ClusterOptions{Target: "v1.37.1", From: "/staged/stratum", Client: cs, DryRun: true})
	})
	if err != nil {
		t.Fatalf("UpgradeCluster --dry-run: %v", err)
	}
	if !strings.Contains(out, "check accel-diag on nodes matching example.com/accel=true: /bin/sh -c id\n") ||
		!strings.Contains(out, "  this run would refuse: kube-system/stratum-upgrade-node-checks declares the post-upgrade check(s) accel-diag") {
		t.Errorf("the dry run does not list the check and the refusal:\n%s", out)
	}
}

// The lease's cached state can lag a takeover, so each check first renews
// both leases: a successor that took the lease after the first check stops
// the run before the second, though the cached state still says held.
func TestPostChecksConfirmTheLeaseBeforeEachCheck(t *testing.T) {
	checks, err := parsePostChecks(postCheckCM(map[string]string{
		"a-first":  "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag, one]\n",
		"b-second": "nodeSelector: example.com/accel=true\ncommand: [/usr/bin/diag, two]\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	cs, n := postCheckNode(t)
	renewals := 0
	g := &leaseGuard{lastRenew: time.Now(), renew: func(context.Context) error {
		renewals++
		if renewals > 1 {
			return fmt.Errorf("%w: held by another run", errLeaseTaken)
		}
		return nil
	}}
	var ran []string
	stubPostCheck(t, func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) error {
		ran = append(ran, s.Kind)
		if err := g.Err(); err != nil {
			t.Errorf("the cached lease state is %v during the first check, want held, so only a renewal can see the takeover", err)
		}
		return nil
	})
	o := ClusterOptions{Target: "v1.37.1", postChecks: checks, lease: g}
	err = runPostChecks(context.Background(), io.Discard, cs, n, jobName(n.Name), o)
	if !errors.Is(err, errLeaseTaken) || errors.Is(err, errPostCheckFailed) {
		t.Fatalf("err = %v, want the takeover and not a failed check", err)
	}
	if !slices.Equal(ran, []string{"post-upgrade check a-first"}) || renewals != 2 {
		t.Errorf("checks run = %q after %d renewals, want only the first after two", ran, renewals)
	}
}

// A failed check prints the end of its log, and a re-run is told to use this
// orchestrator, which runs the checks, not an upgrade binary that may predate
// them; the names the operator gave carry over.
func TestAFailedPostCheckRelaysItsLogAndNamesACheckingRerun(t *testing.T) {
	cs, n := postCheckNode(t)
	stubPostCheck(t, func(context.Context, kubernetes.Interface, nodejob.Spec) error {
		return fmt.Errorf("post-upgrade check accel-diag Job stratum-upgrade-w1 failed: %w", nodejob.ErrNotPassed)
	})
	var lines []string
	for i := 1; i <= 25; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	swap(t, &readPostCheckLog, func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) (string, error) {
		if s.Name != "stratum-upgrade-w1" || s.Container != "check" {
			t.Errorf("log read for Job %s container %s, want stratum-upgrade-w1 check", s.Name, s.Container)
		}
		return strings.Join(lines, "\n") + "\n", nil
	})
	o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-next", self: "/usr/local/bin/stratum",
		postChecks: []postCheck{gpuCheck(t)}, NodeChecks: []string{"accel-diag"}}
	var out strings.Builder
	err := runPostChecks(context.Background(), &out, cs, n, jobName(n.Name), o)
	if !errors.Is(err, errPostCheckFailed) {
		t.Fatalf("err = %v, want the failed check", err)
	}
	if strings.Contains(out.String(), "check accel-diag: line 5\n") || !strings.Contains(out.String(), "  w1 check accel-diag: line 6\n") ||
		!strings.Contains(out.String(), "  w1 check accel-diag: line 25\n") {
		t.Errorf("relayed log, want its last 20 lines:\n%s", out.String())
	}
	held := holdFailedNode(context.Background(), cs, n, jobName(n.Name), o, err)
	want := "re-run /usr/local/bin/stratum upgrade --cluster --target v1.37.1 --from /var/lib/stratum/upgrade/stratum-next --node-checks accel-diag --yes"
	if !strings.Contains(held.Error(), want) {
		t.Errorf("held error:\n%v\nwant %q", held, want)
	}
	if steps := recoverySteps(n, jobName(n.Name), o); !strings.Contains(steps, "2. /usr/local/bin/stratum upgrade --cluster") {
		t.Errorf("recovery steps re-run with the upgrade binary:\n%s", steps)
	}

	swap(t, &readPostCheckLog, func(context.Context, kubernetes.Interface, nodejob.Spec) (string, error) { return "\n", nil })
	out.Reset()
	if err := runPostChecks(context.Background(), &out, cs, n, jobName(n.Name), o); !errors.Is(err, errPostCheckFailed) {
		t.Fatalf("err = %v, want the failed check", err)
	}
	if !strings.Contains(out.String(), "  w1 check accel-diag printed nothing\n") || strings.Contains(out.String(), "check accel-diag: \n") {
		t.Errorf("an empty log is relayed as a blank line:\n%s", out.String())
	}

	swap(t, &readPostCheckLog, func(context.Context, kubernetes.Interface, nodejob.Spec) (string, error) {
		return "", errors.New("the Job has no Pod")
	})
	out.Reset()
	if err := runPostChecks(context.Background(), &out, cs, n, jobName(n.Name), o); !errors.Is(err, errPostCheckFailed) {
		t.Fatalf("err = %v, want the failed check", err)
	}
	if !strings.Contains(out.String(), "WARN: could not read check accel-diag's log on w1: the Job has no Pod") {
		t.Errorf("an unreadable log is not reported:\n%s", out.String())
	}
}

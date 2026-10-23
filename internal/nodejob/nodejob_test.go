package nodejob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/version"
)

func testSpec() Spec {
	return Spec{
		Namespace:      "kube-system",
		Name:           "stratum-upgrade-node1",
		Node:           "node1",
		LabelKey:       "stratum.ciq.com/upgrade",
		Container:      "upgrade",
		Argv:           []string{"nsenter", "-t", "1", "--", "/bin/stratum", "upgrade"},
		ActiveDeadline: 900 * time.Second,
		PollAttempts:   3,
		Kind:           "per-node upgrade",
	}
}

func TestCompletedJobCleanupWarningsStayOffStdout(t *testing.T) {
	for _, exitCode := range []bool{false, true} {
		t.Run(fmt.Sprintf("exit-code=%t", exitCode), func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			cs.PrependReactor("get", "jobs", statusReactor(batchv1.JobStatus{Succeeded: 1}))
			cs.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("cleanup unavailable")
			})
			r, out, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			saved := os.Stdout
			t.Cleanup(func() { os.Stdout = saved; _ = out.Close() })
			os.Stdout = out
			var runErr error
			var progress bytes.Buffer
			spec := testSpec()
			spec.Output = &progress
			if exitCode {
				_, runErr = RunExitCode(context.Background(), cs, spec)
			} else {
				runErr = Run(context.Background(), cs, spec)
			}
			os.Stdout = saved
			_ = out.Close()
			printed, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if runErr != nil {
				t.Fatalf("successful job with failed cleanup: %v", runErr)
			}
			if !strings.Contains(progress.String(), "WARN: could not delete completed Job stratum-upgrade-node1: cleanup unavailable") {
				t.Fatalf("cleanup warning missing from progress: %q", progress.String())
			}
			if len(printed) != 0 {
				t.Fatalf("job cleanup warning corrupted the MCP stdout transport: %q", printed)
			}
		})
	}
}

// TestBuild pins the privileged pod spec.
func TestBuild(t *testing.T) {
	j := Build(testSpec())

	if j.Name != "stratum-upgrade-node1" || j.Namespace != "kube-system" {
		t.Errorf("name/ns = %s/%s", j.Name, j.Namespace)
	}
	if j.Labels["stratum.ciq.com/upgrade"] != "node1" {
		t.Errorf("label = %v", j.Labels)
	}
	if *j.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit = %d, want 0", *j.Spec.BackoffLimit)
	}
	if *j.Spec.ActiveDeadlineSeconds != 900 {
		t.Errorf("ActiveDeadlineSeconds = %d, want 900", *j.Spec.ActiveDeadlineSeconds)
	}
	ps := j.Spec.Template.Spec
	if !ps.HostPID || !ps.HostNetwork {
		t.Error("must be HostPID + HostNetwork")
	}
	if ps.NodeName != "node1" {
		t.Errorf("NodeName = %q", ps.NodeName)
	}
	// Not critical unless asked: a critical pod may evict workloads to be
	// admitted, past every PDB.
	if ps.PriorityClassName != "" {
		t.Errorf("PriorityClassName = %q, want none for a Job that did not ask", ps.PriorityClassName)
	}
	if ps.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q", ps.RestartPolicy)
	}
	if len(ps.Tolerations) != 1 || ps.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("tolerations = %v (want tolerate-all)", ps.Tolerations)
	}
	if ps.TerminationGracePeriodSeconds == nil || *ps.TerminationGracePeriodSeconds != 30 {
		t.Errorf("grace = %v", ps.TerminationGracePeriodSeconds)
	}
	c := ps.Containers[0]
	if c.Name != "upgrade" || c.Image != version.NsenterImage {
		t.Errorf("container = %s / image = %s", c.Name, c.Image)
	}
	if c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("pull policy = %q, want IfNotPresent (airgap)", c.ImagePullPolicy)
	}
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("container must be privileged")
	}
	if strings.Join(c.Command, " ") != "nsenter -t 1 -- /bin/stratum upgrade" {
		t.Errorf("command = %v (must be the shell-free argv verbatim)", c.Command)
	}
}

func TestTruncName(t *testing.T) {
	if got := TruncName("short"); got != "short" {
		t.Errorf("short = %q", got)
	}
	exact := strings.Repeat("x", 63)
	if got := TruncName(exact); got != exact {
		t.Errorf("a 63-character name changed: %q", got)
	}
	seen := map[string]string{}
	for _, name := range []string{
		strings.Repeat("x", 80),
		"stratum-carotate-trust-new-ske-39099666359-0-gcp-rocky-9-amd64-controller-0",
		"stratum-carotate-trust-new-ske-39099666359-0-gcp-rocky-9-amd64-controller-1",
		"stratum-etcdca-trust-both-" + strings.Repeat("a", 20) + "." + strings.Repeat("b", 30),
		"stratum-kubeletca-issue-new-" + strings.Repeat("c", 17) + "--" + strings.Repeat("d", 30),
	} {
		got := TruncName(name)
		if len(got) > 63 {
			t.Errorf("TruncName(%q) is %d characters", name, len(got))
		}
		if errs := validation.IsDNS1123Subdomain(got); len(errs) != 0 {
			t.Errorf("TruncName(%q) = %q is not a valid object name: %v", name, got, errs)
		}
		if prev, ok := seen[got]; ok {
			t.Errorf("TruncName(%q) and TruncName(%q) are both %q", prev, name, got)
		}
		seen[got] = name
		if TruncName(name) != got {
			t.Errorf("TruncName(%q) is not stable", name)
		}
	}
}

// statusReactor makes every Jobs.Get return a job carrying the given status.
func statusReactor(st batchv1.JobStatus) clienttesting.ReactionFunc {
	return func(a clienttesting.Action) (bool, runtime.Object, error) {
		ga := a.(clienttesting.GetAction)
		return true, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: ga.GetNamespace()},
			Status:     st,
		}, nil
	}
}

func TestRun(t *testing.T) {
	restore := pollInterval
	pollInterval = 0
	defer func() { pollInterval = restore }()

	t.Run("succeeds and deletes the job", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "jobs", statusReactor(batchv1.JobStatus{Succeeded: 1}))
		deleted := false
		cs.PrependReactor("delete", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
			deleted = true
			return true, nil, nil
		})
		if err := Run(context.Background(), cs, testSpec()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !deleted {
			t.Error("a succeeded Job should be deleted")
		}
	})

	t.Run("failed job returns an error naming the kind and its log", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "jobs", statusReactor(batchv1.JobStatus{Failed: 1}))
		err := Run(context.Background(), cs, testSpec())
		if err == nil {
			t.Fatal("want an error for a failed Job")
		}
		for _, want := range []string{"per-node upgrade", "failed", "logs: kubectl -n kube-system logs job/stratum-upgrade-node1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q missing %q", err, want)
			}
		}
	})

	t.Run("running job times out", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "jobs", statusReactor(batchv1.JobStatus{})) // never completes
		s := testSpec()
		s.PollAttempts = 2
		err := Run(context.Background(), cs, s)
		if err == nil || !strings.Contains(err.Error(), "did not complete in time") {
			t.Fatalf("want a timeout error, got %v", err)
		}
	})
}

// TestRunRidesOutAnAPIServerRestart: on a one-node cluster the
// upgrade restarts the API server the poll reads from. Reads that fail meanwhile
// are retried, not the end of a Job that is still changing the host.
func TestRunRidesOutAnAPIServerRestart(t *testing.T) {
	restore := pollInterval
	pollInterval = 0
	defer func() { pollInterval = restore }()
	refused := errors.New("dial tcp 127.0.0.1:6443: connect: connection refused")

	downThen := func(n int, st batchv1.JobStatus) *fake.Clientset {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
			if n > 0 {
				n--
				return true, nil, refused
			}
			return statusReactor(st)(a)
		})
		return cs
	}
	s := testSpec()
	s.PollAttempts = 5

	t.Run("Run", func(t *testing.T) {
		if err := Run(context.Background(), downThen(3, batchv1.JobStatus{Succeeded: 1}), s); err != nil {
			t.Fatalf("Run through 3 refused reads: %v", err)
		}
		err := Run(context.Background(), downThen(100, batchv1.JobStatus{Succeeded: 1}), s)
		if !errors.Is(err, refused) || !strings.Contains(err.Error(), "did not complete in time") {
			t.Fatalf("Run with the API never back = %v, want the timeout naming the last read error", err)
		}
	})
	t.Run("RunExitCode", func(t *testing.T) {
		if code, err := RunExitCode(context.Background(), downThen(3, batchv1.JobStatus{Succeeded: 1}), s); err != nil || code != 0 {
			t.Fatalf("RunExitCode through 3 refused reads = %d, %v", code, err)
		}
	})
	t.Run("a deleted Job still ends the poll", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "jobs"}, a.(clienttesting.GetAction).GetName())
		})
		if err := Run(context.Background(), cs, s); !apierrors.IsNotFound(err) {
			t.Fatalf("Run on a vanished Job = %v, want NotFound", err)
		}
	})
}

func TestReady(t *testing.T) {
	ready := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
	}}}
	notReady := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}}}
	if !Ready(ready) {
		t.Error("Ready node reported not-ready")
	}
	if Ready(notReady) || Ready(&corev1.Node{}) {
		t.Error("not-ready / condition-less node reported ready")
	}
}

func TestWaitReady(t *testing.T) {
	restore := pollInterval
	pollInterval = 0
	defer func() { pollInterval = restore }()

	cs := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}},
	})
	if err := WaitReady(context.Background(), cs, "node1", 3); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if err := WaitReady(context.Background(), cs, "absent", 2); err == nil {
		t.Error("want an error for a node that never becomes Ready")
	}
}

// A successful old Job must never satisfy Run: foreground deletion can take
// multiple polls, and only the replacement's status determines the result.
func TestRunReplacesStaleJob(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "replacement succeeds"
		if failed {
			name = "replacement fails"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := testSpec()
				stale := Build(s)
				stale.Status.Succeeded = 1
				cs := fake.NewSimpleClientset(stale)
				resource := batchv1.SchemeGroupVersion.WithResource("jobs")
				creates, deletes, deletionPolls, replacementPolls := 0, 0, 0, 0
				deleting := false
				cs.PrependReactor("create", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
					creates++
					return false, nil, nil // tracker enforces AlreadyExists
				})
				cs.PrependReactor("delete", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
					deletes++
					if deletes == 1 {
						policy := a.(clienttesting.DeleteAction).GetDeleteOptions().PropagationPolicy
						if policy == nil || *policy != metav1.DeletePropagationForeground {
							t.Fatal("stale Job deletion must be foreground")
						}
						deleting = true
						return true, nil, nil // simulate asynchronous deletion
					}
					return false, nil, nil
				})
				cs.PrependReactor("get", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
					if deleting {
						deletionPolls++
						if deletionPolls == 1 {
							return false, nil, nil
						}
						if err := cs.Tracker().Delete(resource, s.Namespace, s.Name); err != nil {
							t.Fatal(err)
						}
						deleting = false
						return false, nil, nil // tracker now returns NotFound
					}
					if creates == 1 {
						// Run inspects the existing Job before replacing it: it
						// must see the stale, finished one here.
						return false, nil, nil
					}
					obj, err := cs.Tracker().Get(resource, s.Namespace, s.Name)
					if err != nil {
						return true, nil, err
					}
					j := obj.(*batchv1.Job).DeepCopy()
					if creates != 2 || j.Status.Succeeded != 0 {
						t.Fatal("polled stale Job instead of a fresh replacement")
					}
					replacementPolls++
					if replacementPolls > 1 {
						if failed {
							j.Status.Failed = 1
						} else {
							j.Status.Succeeded = 1
						}
					}
					return true, j, nil
				})
				err := Run(context.Background(), cs, s)
				if failed {
					if err == nil || !strings.Contains(err.Error(), "failed") {
						t.Fatalf("want replacement failure, got %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				wantDeletes := 2
				if failed {
					wantDeletes = 1
				}
				if creates != 2 || deletes != wantDeletes || deletionPolls != 2 || replacementPolls != 2 {
					t.Fatalf("creates=%d deletes=%d deletion polls=%d replacement polls=%d", creates, deletes, deletionPolls, replacementPolls)
				}
				_, err = cs.Tracker().Get(resource, s.Namespace, s.Name)
				if failed && err != nil {
					t.Fatalf("failed replacement must remain for inspection: %v", err)
				}
				if !failed && err == nil {
					t.Fatal("successful replacement was not cleaned up")
				}
			})
		})
	}
}

func TestPollingCancellation(t *testing.T) {
	for _, operation := range []string{"run", "delete", "ready"} {
		for _, preCanceled := range []bool{false, true} {
			name := operation + "/during wait"
			if preCanceled {
				name = operation + "/before request"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					s := testSpec()
					cs := fake.NewSimpleClientset()
					cs.PrependReactor("get", "jobs", statusReactor(batchv1.JobStatus{}))
					cs.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
						return true, nil, nil // deletion remains pending
					})
					if preCanceled {
						cancel()
					} else {
						go func() { time.Sleep(time.Second); cancel() }()
					}
					start := time.Now()
					var err error
					switch operation {
					case "run":
						err = Run(ctx, cs, s)
					case "delete":
						err = DeleteAndWait(ctx, cs, s.Namespace, s.Name)
					case "ready":
						err = WaitReady(ctx, cs, s.Node, 3)
					}
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("want context.Canceled, got %v", err)
					}
					wantDelay, wantActions := time.Second, 2
					if operation == "ready" {
						wantActions = 1
					}
					if preCanceled {
						wantDelay, wantActions = 0, 0
					}
					if elapsed := time.Since(start); elapsed != wantDelay {
						t.Errorf("returned after %v, want %v", elapsed, wantDelay)
					}
					if actions := cs.Actions(); len(actions) != wantActions {
						t.Errorf("unexpected requests after cancellation: %v", actions)
					}
				})
			})
		}
	}
}

// TestRunNeverReplacesARunningJob: a Job still running, or
// being deleted with its Pod still up, belongs to a run that is alive or
// crashed mid-change on that node. Replacing it would interrupt the change on
// the host, so Run refuses and leaves it exactly as it was.
func TestRunNeverReplacesARunningJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*batchv1.Job)
	}{
		{"running", func(j *batchv1.Job) { j.Status.Active = 1 }},
		{"being deleted with its Pod still up", func(j *batchv1.Job) {
			j.Status.Active = 1
			j.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			j.Finalizers = []string{"foregroundDeletion"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSpec()
			running := Build(s)
			tc.mod(running)
			cs := fake.NewSimpleClientset(running)
			deletes := 0
			cs.PrependReactor("delete", "jobs", func(clienttesting.Action) (bool, runtime.Object, error) {
				deletes++
				return false, nil, nil
			})
			err := Run(context.Background(), cs, s)
			if err == nil || !strings.Contains(err.Error(), "already running") {
				t.Fatalf("Run over a running Job = %v, want a refusal", err)
			}
			if deletes != 0 {
				t.Fatalf("the running Job was deleted (%d delete calls)", deletes)
			}
		})
	}
}

func TestFinished(t *testing.T) {
	j := func(mod func(*batchv1.Job)) *batchv1.Job { x := &batchv1.Job{}; mod(x); return x }
	for _, tc := range []struct {
		name string
		job  *batchv1.Job
		want bool
	}{
		{"succeeded", j(func(x *batchv1.Job) { x.Status.Succeeded = 1 }), true},
		{"failed", j(func(x *batchv1.Job) { x.Status.Failed = 1 }), true},
		{"Complete condition", j(func(x *batchv1.Job) {
			x.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		}), true},
		{"running", j(func(x *batchv1.Job) { x.Status.Active = 1 }), false},
		{"terminating, Pod still up", j(func(x *batchv1.Job) {
			x.Status.Active = 1
			x.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		}), false},
	} {
		if got := Finished(tc.job); got != tc.want {
			t.Errorf("%s: Finished = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestBuildIsValidForALongNodeName is the check the fake clientset cannot make.
//
// A node name is a DNS subdomain and may be 253 characters; a label value may
// be 63. A node name over 63 characters must not go straight into the label:
// a real API server rejects the Job outright — while the fake, which validates
// nothing, accepts it and every test passes.
func TestBuildIsValidForALongNodeName(t *testing.T) {
	const labelKey = "stratum.ciq.com/upgrade"

	for _, node := range []string{
		"cp1",
		strings.Repeat("a", 63),
		strings.Repeat("a", 64),
		strings.Repeat("a", 80),
		strings.Repeat("a", 63) + "." + strings.Repeat("b", 63),
	} {
		t.Run(node[:min(len(node), 24)], func(t *testing.T) {
			if errs := validation.IsDNS1123Subdomain(node); len(errs) != 0 {
				t.Fatalf("invalid fixture: %v", errs)
			}
			j := Build(Spec{
				Namespace: "kube-system", Name: "stratum-upgrade-x", Node: node,
				LabelKey: labelKey, Container: "upgrade", Argv: []string{"true"},
			})
			for k, v := range j.Labels {
				if errs := validation.IsValidLabelValue(v); len(errs) != 0 {
					t.Errorf("label %s=%q is not a valid label value: %v", k, v, errs)
				}
			}
			// The ownership answer must survive the hashing.
			if got := JobNode(j, labelKey); got != node {
				t.Errorf("JobNode = %q, want the full node name", got)
			}
		})
	}

	// Two nodes must not share a label value, or the hash would make them
	// indistinguishable to anything selecting on it.
	t.Run("distinct nodes get distinct label values", func(t *testing.T) {
		a := strings.Repeat("a", 70) + "1"
		b := strings.Repeat("a", 70) + "2"
		if NodeLabelValue(a) == NodeLabelValue(b) {
			t.Error("two node names collided in the label value")
		}
	})

	// A Job written before the annotation existed still answers, because a
	// node whose name did not fit could not have had a Job at all.
	t.Run("a pre-annotation Job still answers", func(t *testing.T) {
		j := Build(Spec{Namespace: "kube-system", Name: "n", Node: "cp1", LabelKey: labelKey})
		j.Annotations = nil
		if got := JobNode(j, labelKey); got != "cp1" {
			t.Errorf("JobNode = %q, want cp1", got)
		}
	})
}

// TestRunRefusesAJobItDoesNotOwn covers the Job that neither sweep can reach.
//
// The upgrade sweeps visit nodes the cluster currently has. A node that has
// been REMOVED leaves its Job behind, and if that Job's name is one a current
// node now wants, Run is the only thing standing between them — so replacing
// by name alone would silently delete a departed node's object, which is the
// cross-owner deletion the naming exists to prevent.
func TestRunRefusesAJobItDoesNotOwn(t *testing.T) {
	const ns, name, labelKey = "kube-system", "stratum-upgrade-shared", "stratum.ciq.com/upgrade"

	spec := func(node string) Spec {
		return Spec{
			Namespace: ns, Name: name, Node: node, LabelKey: labelKey,
			Container: "upgrade", Argv: []string{"true"}, PollAttempts: 1,
		}
	}

	t.Run("a finished Job owned by another node is refused, not replaced", func(t *testing.T) {
		theirs := Build(spec("departed-node"))
		theirs.Status.Succeeded = 1
		cs := fake.NewSimpleClientset(theirs)

		err := Run(t.Context(), cs, spec("cp1"))
		if err == nil {
			t.Fatal("another node's Job was replaced")
		}
		if !strings.Contains(err.Error(), "departed-node") {
			t.Errorf("error %q does not name the owner", err)
		}
		if _, gerr := cs.BatchV1().Jobs(ns).Get(t.Context(), name, metav1.GetOptions{}); gerr != nil {
			t.Errorf("the other node's Job was deleted: %v", gerr)
		}
	})

	// An owner that cannot be established is not this node's.
	t.Run("a Job with no owner is refused", func(t *testing.T) {
		orphan := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		orphan.Status.Succeeded = 1
		cs := fake.NewSimpleClientset(orphan)

		if err := Run(t.Context(), cs, spec("cp1")); err == nil {
			t.Fatal("a Job with no owner was replaced")
		}
	})

	// The ordinary path must still work: this node's own finished Job is
	// exactly what Run is supposed to replace.
	t.Run("this node's own finished Job is still replaced", func(t *testing.T) {
		mine := Build(spec("cp1"))
		mine.Status.Succeeded = 1
		cs := fake.NewSimpleClientset(mine)

		// Run polls for completion; the recreated Job is never finished in
		// a fake, so the ownership gate passing is what this asserts.
		err := Run(t.Context(), cs, spec("cp1"))
		if err != nil && strings.Contains(err.Error(), "belongs to node") {
			t.Errorf("Run refused this node's own Job: %v", err)
		}
	})
}

func TestCriticalJobIsSystemNodeCritical(t *testing.T) {
	// Test the priorityClass helper function directly
	if got := priorityClass(true); got != "system-node-critical" {
		t.Errorf("priorityClass(true) = %q", got)
	}

	// Test that Spec.Critical=true results in the correct PriorityClassName in the pod spec
	spec := Spec{
		Name:      "test-job",
		Node:      "test-node",
		LabelKey:  "test-label",
		Container: "test-container",
		Argv:      []string{"echo", "hello"},
		Critical:  true,
	}

	job := Build(spec)
	if got := job.Spec.Template.Spec.PriorityClassName; got != "system-node-critical" {
		t.Errorf("Build(Spec{Critical: true}).Spec.Template.Spec.PriorityClassName = %q, want %q", got, "system-node-critical")
	}
}

// A read the API server never answers must not stretch the poll past
// attempts * pollInterval: the read is held to the interval and a poll sleeps
// only what is left of it.
func TestWaitReadyStaysWithinItsAttemptsWhenReadsStall(t *testing.T) {
	oldPoll, oldGet := pollInterval, getCallTimeout
	pollInterval, getCallTimeout = 50*time.Millisecond, 10*time.Second
	t.Cleanup(func() { pollInterval, getCallTimeout = oldPoll, oldGet })
	if got := readTimeout(); got != pollInterval {
		t.Fatalf("readTimeout = %s, want the poll interval %s", got, pollInterval)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answers
	}))
	defer srv.Close()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = WaitReady(context.Background(), cs, "cp1", 4)
	if err == nil || !strings.Contains(err.Error(), "did not become Ready") {
		t.Fatalf("WaitReady = %v, want the Ready timeout", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("4 attempts at %s took %s; a stalled read held each one for its full timeout", pollInterval, took)
	}
}

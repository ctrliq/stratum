package ha

import (
	"bytes"

	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

func ciliumObjects(rolledOut bool) (*appsv1.DaemonSet, *appsv1.Deployment) {
	one := int32(1)
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "cilium", Namespace: "kube-system", Generation: 2},
		Spec:   appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}}},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 2, DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberAvailable: 3}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cilium-operator", Namespace: "kube-system", Generation: 2},
		Spec: appsv1.DeploymentSpec{Replicas: &one,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "operator"}}}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}}
	if !rolledOut {
		ds.Status.UpdatedNumberScheduled = 1
	}
	return ds, dep
}

// The Cilium repoint writes and waits through the client it is given, with no
// kubectl and no admin.conf.
func TestRepointCiliumWithUsesTheGivenClient(t *testing.T) {
	servingVIP(t)
	ds, dep := ciliumObjects(true)
	cs := fake.NewSimpleClientset(ds, dep)
	if _, err := RepointCiliumWith(t.Context(), io.Discard, cs, "10.0.0.10"); err != nil {
		t.Fatal(err)
	}
	updates := map[string]bool{}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" {
			updates[a.GetResource().Resource] = true
		}
	}
	if !updates["daemonsets"] || !updates["deployments"] {
		t.Errorf("want the DaemonSet and Deployment updated through the given client, got %v", cs.Actions())
	}
}

// The rollout wait is what `kubectl rollout status` waited for, and a rollout
// that does not finish is reported, not waited on forever.
func TestCiliumRolloutWait(t *testing.T) {
	oldPoll := rolloutPoll
	t.Cleanup(func() { rolloutPoll = oldPoll })
	rolloutPoll = 10 * time.Millisecond

	done, _ := ciliumObjects(true)
	stuck, dep := ciliumObjects(false)
	ctx := t.Context()
	if ok, _, err := daemonSetRolledOut(ctx, fake.NewSimpleClientset(done), "cilium"); err != nil || !ok {
		t.Errorf("a finished DaemonSet: %v, %v", ok, err)
	}
	if ok, _, err := daemonSetRolledOut(ctx, fake.NewSimpleClientset(stuck), "cilium"); err != nil || ok {
		t.Errorf("a DaemonSet with pods still on the old spec: %v, %v", ok, err)
	}
	if ok, _, err := deploymentRolledOut(ctx, fake.NewSimpleClientset(dep), "cilium-operator"); err != nil || !ok {
		t.Errorf("a finished Deployment: %v, %v", ok, err)
	}
	cs := fake.NewSimpleClientset(stuck)
	err := waitRolledOut(ctx, 50*time.Millisecond, func(ctx context.Context) (bool, string, error) {
		return daemonSetRolledOut(ctx, cs, "cilium")
	})
	if err == nil || !strings.Contains(err.Error(), "1 of 3 updated") || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a rollout that never finishes: %v, want the deadline and the last progress", err)
	}
}

// An error waiting cannot clear ends the wait at once, rather than after the
// whole timeout.
func TestCiliumRolloutWaitStopsOnTerminalErrors(t *testing.T) {
	stalled := func() *appsv1.Deployment {
		_, dep := ciliumObjects(true)
		dep.Status.UpdatedReplicas = 0
		dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing,
			Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
		return dep
	}()
	for name, status := range map[string]func(context.Context) (bool, string, error){
		"forbidden": func(context.Context) (bool, string, error) {
			return false, "", apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, "cilium", errors.New("no rule"))
		},
		"missing": func(ctx context.Context) (bool, string, error) {
			return daemonSetRolledOut(ctx, fake.NewSimpleClientset(), "cilium")
		},
		"progress deadline": func(ctx context.Context) (bool, string, error) {
			return deploymentRolledOut(ctx, fake.NewSimpleClientset(stalled), "cilium-operator")
		},
	} {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			if err := waitRolledOut(t.Context(), time.Minute, status); err == nil || time.Since(start) > 5*time.Second {
				t.Errorf("got %v after %v, want an error at once", err, time.Since(start))
			}
		})
	}
}

// A rollout that does not finish is reported to a caller that asked for its
// own client as a warning beside a nil error — the env change is already
// applied, so the repoint is not a failure — while the WARN line the operator
// reads stays printed exactly as before. The Deployment's progress-deadline
// condition makes the wait end at once instead of out the timeout.
func TestRepointCiliumWithReportsAnIncompleteRollout(t *testing.T) {
	servingVIP(t)
	oldPoll := rolloutPoll
	t.Cleanup(func() { rolloutPoll = oldPoll })
	rolloutPoll = 10 * time.Millisecond

	ds, dep := ciliumObjects(true)
	dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing,
		Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
	cs := fake.NewSimpleClientset(ds, dep)

	var warnings []string
	var err error
	var buf bytes.Buffer
	warnings, err = RepointCiliumWith(t.Context(), &buf, cs, "10.0.0.10")
	if err != nil {
		t.Fatalf("RepointCiliumWith = %v; a stuck rollout is a warning, the env change is already applied", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "deploy/cilium-operator") {
		t.Errorf("warnings = %q, want one warning naming deploy/cilium-operator", warnings)
	}
	if !strings.Contains(buf.String(), "WARN: cilium deploy/cilium-operator did not roll out within") {
		t.Errorf("the writer does not warn about the stuck rollout:\n%s", buf.String())
	}
	updated := false
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "deployments" {
			updated = true
		}
	}
	if !updated {
		t.Error("the operator env change was not applied although the repoint reported only a warning")
	}
}

// captureStdout swaps os.Stdout for a pipe for the duration of fn and returns
// what was printed, so a test can pin the printed text beside the returned
// outcome.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

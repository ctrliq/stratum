package k8sutil

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var gateTime = time.Date(2026, 10, 31, 12, 0, 0, 0, time.FixedZone("EDT", -4*3600))

// foreignTaint is a taint another writer owns, which the gate must not touch.
var foreignTaint = corev1.Taint{Key: "nvidia.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule}

func gateNode(taints []corev1.Taint, annotations map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu1", Annotations: annotations},
		Spec:       corev1.NodeSpec{Taints: taints},
	}
}

func getNode(t *testing.T, cs *fake.Clientset) *corev1.Node {
	t.Helper()
	n, err := cs.CoreV1().Nodes().Get(context.Background(), "gpu1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReleaseNode(t *testing.T) {
	unverified := []corev1.Taint{
		{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule},
		foreignTaint,
		{Key: UnverifiedTaint, Effect: corev1.TaintEffectNoExecute},
	}
	cs := fake.NewClientset(gateNode(unverified, map[string]string{HeldAnnotation: "ops@then", HeldReasonAnnotation: "reimage", "keep": "me"}))
	res, err := ReleaseNode(context.Background(), cs, "gpu1", "alice", false, gateTime)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Removed || res.Cordoned {
		t.Errorf("release = %+v, want the taint removed and no cordon", res)
	}
	n := getNode(t, cs)
	if len(n.Spec.Taints) != 1 || n.Spec.Taints[0] != foreignTaint {
		t.Errorf("taints after release = %+v, want only the foreign taint", n.Spec.Taints)
	}
	if got := n.Annotations[VerifiedAnnotation]; got != "alice@2026-10-31T16:00:00Z" {
		t.Errorf("verified = %q, want alice@2026-10-31T16:00:00Z", got)
	}
	_, held := n.Annotations[HeldAnnotation]
	_, reason := n.Annotations[HeldReasonAnnotation]
	if held || reason || n.Annotations["keep"] != "me" {
		t.Errorf("annotations after release = %v, want the hold record and reason gone and every other annotation kept", n.Annotations)
	}
}

func TestReleaseNodeRefusesANodeThatIsNotHeld(t *testing.T) {
	cs := fake.NewClientset(gateNode([]corev1.Taint{foreignTaint}, nil))
	_, err := ReleaseNode(context.Background(), cs, "gpu1", "alice", false, gateTime)
	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("release of an untainted node = %v, want ErrNotHeld", err)
	}
	if _, ok := getNode(t, cs).Annotations[VerifiedAnnotation]; ok {
		t.Error("a refused release wrote the verified annotation")
	}
	res, err := ReleaseNode(context.Background(), cs, "gpu1", "alice", true, gateTime)
	if err != nil {
		t.Fatalf("forced release: %v", err)
	}
	if res.Removed {
		t.Errorf("forced release of an untainted node = %+v, want nothing reported removed", res)
	}
	n := getNode(t, cs)
	if n.Annotations[VerifiedAnnotation] == "" || len(n.Spec.Taints) != 1 {
		t.Errorf("forced release left %+v, want the annotation written and the foreign taint kept", n)
	}
}

// A writer that changes the Node between the read and the update makes the
// update conflict; the gate reads again and applies to the newer Node, so the
// other writer's change survives.
func TestNodeGateRetriesAConflict(t *testing.T) {
	cs := fake.NewClientset(gateNode([]corev1.Taint{{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}}, nil))
	updates := 0
	cs.PrependReactor("update", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			nodes := corev1.SchemeGroupVersion.WithResource("nodes")
			obj, err := cs.Tracker().Get(nodes, "", "gpu1")
			if err != nil {
				return true, nil, err
			}
			n := obj.(*corev1.Node)
			n.Spec.Taints = append(n.Spec.Taints, foreignTaint)
			if err := cs.Tracker().Update(nodes, n, ""); err != nil {
				return true, nil, err
			}
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "gpu1", errors.New("the object has been modified"))
		}
		return false, nil, nil
	})
	if _, err := ReleaseNode(context.Background(), cs, "gpu1", "alice", false, gateTime); err != nil {
		t.Fatalf("release across a conflict: %v", err)
	}
	if updates != 2 {
		t.Errorf("updates = %d, want the conflicting one and a retry", updates)
	}
	if n := getNode(t, cs); len(n.Spec.Taints) != 1 || n.Spec.Taints[0] != foreignTaint || n.Annotations[VerifiedAnnotation] == "" {
		t.Errorf("after the retried release the node is %+v, want only the other writer's taint and the verified record", n)
	}
}

// A Node deleted and registered again under the same name between a
// conflicting update and its retry is a different machine, or a reinstall;
// the gate refuses it rather than releasing or holding the node the operator
// did not check.
func TestNodeGateRefusesANodeReplacedDuringARetry(t *testing.T) {
	for name, gate := range map[string]func(*fake.Clientset) error{
		"release": func(cs *fake.Clientset) error {
			_, err := ReleaseNode(context.Background(), cs, "gpu1", "alice", false, gateTime)
			return err
		},
		"hold": func(cs *fake.Clientset) error {
			return HoldNode(context.Background(), cs, "gpu1", "bob", "repair", gateTime)
		},
	} {
		first := gateNode([]corev1.Taint{{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}}, nil)
		first.UID = "first"
		cs := fake.NewClientset(first)
		updates := 0
		cs.PrependReactor("update", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
			updates++
			if updates > 1 {
				return false, nil, nil
			}
			nodes := corev1.SchemeGroupVersion.WithResource("nodes")
			if err := cs.Tracker().Delete(nodes, "", "gpu1"); err != nil {
				return true, nil, err
			}
			second := gateNode([]corev1.Taint{{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}}, nil)
			second.UID = "second"
			if err := cs.Tracker().Add(second); err != nil {
				return true, nil, err
			}
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "gpu1", errors.New("the object has been modified"))
		})
		if err := gate(cs); !errors.Is(err, ErrNodeReplaced) {
			t.Errorf("%s across a replacement = %v, want ErrNodeReplaced", name, err)
		}
		if n := getNode(t, cs); len(n.Spec.Taints) != 1 || len(n.Annotations) != 0 {
			t.Errorf("%s changed the replacement Node: %+v", name, n)
		}
	}
}

func TestHoldNode(t *testing.T) {
	cs := fake.NewClientset(gateNode([]corev1.Taint{foreignTaint}, map[string]string{VerifiedAnnotation: "alice@then", "keep": "me"}))
	if err := HoldNode(context.Background(), cs, "gpu1", "bob", "  RMA with ops@vendor.example ", gateTime); err != nil {
		t.Fatal(err)
	}
	n := getNode(t, cs)
	want := corev1.Taint{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}
	if len(n.Spec.Taints) != 2 || n.Spec.Taints[0] != foreignTaint || n.Spec.Taints[1] != want {
		t.Errorf("taints after hold = %+v, want the foreign taint and %+v", n.Spec.Taints, want)
	}
	if got := n.Annotations[HeldAnnotation]; got != "bob@2026-10-31T16:00:00Z" {
		t.Errorf("held = %q, want only the stamp, which readers split at its last '@'", got)
	}
	if got := n.Annotations[HeldReasonAnnotation]; got != "RMA with ops@vendor.example" {
		t.Errorf("held-reason = %q", got)
	}
	if _, ok := n.Annotations[VerifiedAnnotation]; ok || n.Annotations["keep"] != "me" {
		t.Errorf("annotations after hold = %v, want verified gone and every other annotation kept", n.Annotations)
	}

	if err := HoldNode(context.Background(), cs, "gpu1", "carol", "still broken", gateTime); err != nil {
		t.Fatal(err)
	}
	n = getNode(t, cs)
	if len(n.Spec.Taints) != 2 || n.Annotations[HeldAnnotation] != "carol@2026-10-31T16:00:00Z" || n.Annotations[HeldReasonAnnotation] != "still broken" {
		t.Errorf("a second hold left %+v and %v, want one startup taint and the new record", n.Spec.Taints, n.Annotations)
	}
}

func TestNodeGateRefusals(t *testing.T) {
	cs := fake.NewClientset(gateNode(nil, nil))
	ctx := context.Background()
	for name, err := range map[string]error{
		"release with no actor":       releaseErr(ReleaseNode(ctx, cs, "gpu1", "", true, gateTime)),
		"release with a spaced actor": releaseErr(ReleaseNode(ctx, cs, "gpu1", "alice smith", true, gateTime)),
		"hold with no reason":         HoldNode(ctx, cs, "gpu1", "bob", "  ", gateTime),
		"release of a missing node":   releaseErr(ReleaseNode(ctx, cs, "absent", "alice", true, gateTime)),
	} {
		if err == nil {
			t.Errorf("%s succeeded", name)
		}
	}
	if n := getNode(t, cs); len(n.Spec.Taints) != 0 || len(n.Annotations) != 0 {
		t.Errorf("refused calls changed the node: %+v", n)
	}
}

// A PreferNoSchedule startup taint only discourages scheduling, so a hold
// adds the NoSchedule one beside it, and a release removes both.
func TestHoldNodeAddsNoScheduleBesideASofterStartupTaint(t *testing.T) {
	soft := corev1.Taint{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectPreferNoSchedule}
	cs := fake.NewClientset(gateNode([]corev1.Taint{soft}, nil))
	if err := HoldNode(context.Background(), cs, "gpu1", "bob", "burn-in", gateTime); err != nil {
		t.Fatal(err)
	}
	hold := corev1.Taint{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}
	if n := getNode(t, cs); len(n.Spec.Taints) != 2 || n.Spec.Taints[0] != soft || n.Spec.Taints[1] != hold {
		t.Errorf("taints after hold = %+v, want %+v and %+v", n.Spec.Taints, soft, hold)
	}
	if _, err := ReleaseNode(context.Background(), cs, "gpu1", "alice", false, gateTime); err != nil {
		t.Fatal(err)
	}
	if n := getNode(t, cs); len(n.Spec.Taints) != 0 {
		t.Errorf("taints after release = %+v, want none", n.Spec.Taints)
	}
}

// A NoSchedule startup taint with another value still holds pods off, but a
// burn-in job tolerates the value the startup gate gives, so a hold sets it to
// true rather than leaving it.
func TestHoldNodeSetsTheStartupTaintsValue(t *testing.T) {
	cs := fake.NewClientset(gateNode([]corev1.Taint{{Key: UnverifiedTaint, Value: "false", Effect: corev1.TaintEffectNoSchedule}}, nil))
	if err := HoldNode(context.Background(), cs, "gpu1", "bob", "burn-in", gateTime); err != nil {
		t.Fatal(err)
	}
	hold := corev1.Taint{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}
	if n := getNode(t, cs); len(n.Spec.Taints) != 1 || n.Spec.Taints[0] != hold {
		t.Errorf("taints after hold = %+v, want only %+v", n.Spec.Taints, hold)
	}
}

func releaseErr(_ ReleaseResult, err error) error { return err }

// A released node that a drain cordoned is reported as still cordoned, and
// the cordon is left for the operator.
func TestReleaseNodeReportsACordon(t *testing.T) {
	n := gateNode([]corev1.Taint{{Key: UnverifiedTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule}}, nil)
	n.Spec.Unschedulable = true
	cs := fake.NewClientset(n)
	res, err := ReleaseNode(context.Background(), cs, "gpu1", "alice", false, gateTime)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Removed || !res.Cordoned || !getNode(t, cs).Spec.Unschedulable {
		t.Errorf("release of a cordoned node = %+v, want the taint removed, the cordon reported and kept", res)
	}
}

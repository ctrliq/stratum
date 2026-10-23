package operator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var epoch = time.Date(2026, 9, 24, 22, 51, 0, 0, time.UTC)

func dnsPod(name, node string, age time.Duration, ready bool) corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: corednsNamespace,
			Labels:            map[string]string{"k8s-app": "kube-dns"},
			CreationTimestamp: metav1.NewTime(epoch.Add(-age))},
		Spec: corev1.PodSpec{NodeName: node, Tolerations: []corev1.Toleration{
			{Key: "CriticalAddonsOnly", Operator: corev1.TolerationOpExists},
			{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule},
		}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}},
	}
}

func node(name string, ready bool, taints ...corev1.Taint) corev1.Node {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:   corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}}}
}

// TestCoreDNSToMove is the rule table. The first case is the one
// seen in practice: both replicas on the first node, still there after the
// others joined.
func TestCoreDNSToMove(t *testing.T) {
	older, newer := dnsPod("coredns-a", "cp1", 2*time.Hour, true), dnsPod("coredns-b", "cp1", time.Hour, true)
	cordoned := node("cp2", true)
	cordoned.Spec.Unschedulable = true
	terminating := dnsPod("coredns-c", "cp2", 0, true)
	terminating.DeletionTimestamp = &metav1.Time{Time: epoch}
	cpTaint := corev1.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: corev1.TaintEffectNoSchedule}
	gpuTaint := corev1.Taint{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}
	soft := corev1.Taint{Key: "example.com/prefer-not", Effect: corev1.TaintEffectPreferNoSchedule}

	for _, tc := range []struct {
		name  string
		pods  []corev1.Pod
		nodes []corev1.Node
		want  string
	}{
		{"both on cp1, cp2 and cp3 joined: move the newer one", []corev1.Pod{older, newer},
			[]corev1.Node{node("cp1", true), node("cp2", true), node("cp3", true)}, "coredns-b"},
		{"single node: nothing to move to", []corev1.Pod{older, newer}, []corev1.Node{node("cp1", true)}, ""},
		{"already spread", []corev1.Pod{older, dnsPod("coredns-b", "cp2", time.Hour, true)},
			[]corev1.Node{node("cp1", true), node("cp2", true)}, ""},
		{"a replica not Ready: never evict the last serving one", []corev1.Pod{older, dnsPod("coredns-b", "cp1", time.Hour, false)},
			[]corev1.Node{node("cp1", true), node("cp2", true)}, ""},
		{"a replica not yet scheduled: a rollout is in progress", []corev1.Pod{older, dnsPod("coredns-b", "", time.Hour, false)},
			[]corev1.Node{node("cp1", true), node("cp2", true)}, ""},
		{"a terminating replica does not count", []corev1.Pod{older, terminating}, []corev1.Node{node("cp1", true), node("cp2", true)}, ""},
		{"only other node is NotReady", []corev1.Pod{older, newer}, []corev1.Node{node("cp1", true), node("cp2", false)}, ""},
		{"only other node is cordoned", []corev1.Pod{older, newer}, []corev1.Node{node("cp1", true), cordoned}, ""},
		{"only other node has an untolerated taint", []corev1.Pod{older, newer}, []corev1.Node{node("cp1", true), node("gpu1", true, gpuTaint)}, ""},
		{"a control-plane taint is tolerated", []corev1.Pod{older, newer}, []corev1.Node{node("cp1", true), node("cp2", true, cpTaint)}, "coredns-b"},
		{"PreferNoSchedule does not rule a node out", []corev1.Pod{older, newer}, []corev1.Node{node("cp1", true), node("cp2", true, soft)}, "coredns-b"},
		{"one replica: nothing to spread", []corev1.Pod{older}, []corev1.Node{node("cp1", true), node("cp2", true)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := corednsToMove(tc.pods, tc.nodes)
			if got != tc.want {
				t.Errorf("corednsToMove = %q (%s), want %q", got, why, tc.want)
			}
		})
	}
}

// freshSpreader returns a spreader whose cooldown has never fired: a fresh
// spreader value starts with a zero last move and needs no reset at all.
func freshSpreader() *corednsSpreader { return &corednsSpreader{} }

// TestReconcileCoreDNSSpreadEvictsOnceThenCoolsDown: the decision reaches the
// API as an eviction, and a second pass inside the cooldown does nothing.
func TestReconcileCoreDNSSpreadEvictsOnceThenCoolsDown(t *testing.T) {
	sp := freshSpreader()
	cs, evicted := spreadCluster(corednsDeploy(map[string]string{group + "/stack": CoreDNSStack}))

	sp.reconcileCoreDNSSpread(context.Background(), cs, epoch)
	sp.reconcileCoreDNSSpread(context.Background(), cs, epoch.Add(20*time.Second))
	if len(*evicted) != 1 || (*evicted)[0] != "coredns-b" {
		t.Fatalf("evictions = %v, want exactly [coredns-b]", *evicted)
	}
	sp.reconcileCoreDNSSpread(context.Background(), cs, epoch.Add(spreadCooldown+time.Second))
	if len(*evicted) != 2 {
		t.Errorf("after the cooldown the still-colocated replicas were not retried: %v", *evicted)
	}
}

func corednsDeploy(labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: corednsDeployment, Namespace: corednsNamespace, Labels: labels},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}}},
	}
}

// spreadCluster is two replicas on cp1 with cp2 joined, behind a fake API that
// records evictions. deploy nil means no CoreDNS Deployment.
func spreadCluster(deploy *appsv1.Deployment) (*fake.Clientset, *[]string) {
	a, b := dnsPod("coredns-a", "cp1", 2*time.Hour, true), dnsPod("coredns-b", "cp1", time.Hour, true)
	n1, n2 := node("cp1", true), node("cp2", true)
	objs := []runtime.Object{&a, &b, &n1, &n2}
	if deploy != nil {
		objs = append(objs, deploy)
	}
	cs := fake.NewSimpleClientset(objs...)
	evicted := &[]string{}
	cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		*evicted = append(*evicted, action.(k8stesting.CreateAction).GetObject().(metav1.Object).GetName())
		return true, nil, nil
	})
	return cs, evicted
}

// TestCoreDNSSpreadCooldownWindow pins the cooldown itself: a move — completed
// or refused — buys spreadCooldown of quiet for every later pass, a pass at
// the window's edge runs again, and the shipped window is five minutes.
func TestCoreDNSSpreadCooldownWindow(t *testing.T) {
	if spreadCooldown != 5*time.Minute {
		t.Errorf("spreadCooldown = %v, want 5m — the shipped pacing of replica moves", spreadCooldown)
	}

	t.Run("an eviction at the window's edge runs again", func(t *testing.T) {
		sp := freshSpreader()
		cs, evicted := spreadCluster(corednsDeploy(map[string]string{group + "/stack": CoreDNSStack}))

		sp.reconcileCoreDNSSpread(context.Background(), cs, epoch)
		// A millisecond inside the window: dropped, though the replicas are
		// still colocated and a node could take one.
		sp.reconcileCoreDNSSpread(context.Background(), cs, epoch.Add(spreadCooldown-time.Millisecond))
		if len(*evicted) != 1 || (*evicted)[0] != "coredns-b" {
			t.Fatalf("evictions inside the window = %v, want exactly [coredns-b]", *evicted)
		}
		// Exactly the window after the move: allowed again.
		sp.reconcileCoreDNSSpread(context.Background(), cs, epoch.Add(spreadCooldown))
		if len(*evicted) != 2 || (*evicted)[1] != "coredns-b" {
			t.Errorf("evictions at the boundary = %v, want coredns-b moved again", *evicted)
		}
	})

	t.Run("a failed eviction still waits out the cooldown", func(t *testing.T) {
		sp := freshSpreader()
		cs, evicted := spreadCluster(corednsDeploy(map[string]string{group + "/stack": CoreDNSStack}))
		attempts := 0
		cs.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.GetSubresource() != "eviction" {
				return false, nil, nil
			}
			attempts++
			return true, nil, apierrors.NewInternalError(errors.New("eviction refused"))
		})

		// The refused attempt still counts as a move: the next pass inside the
		// window makes no attempt at all, and the window's edge tries again.
		sp.reconcileCoreDNSSpread(context.Background(), cs, epoch)
		sp.reconcileCoreDNSSpread(context.Background(), cs, epoch.Add(spreadCooldown-time.Millisecond))
		if attempts != 1 {
			t.Errorf("eviction attempts = %d, want 1 — a refused move must not retry inside the window", attempts)
		}
		sp.reconcileCoreDNSSpread(context.Background(), cs, epoch.Add(spreadCooldown))
		if attempts != 2 {
			t.Errorf("eviction attempts after the window = %d, want 2", attempts)
		}
		if len(*evicted) != 0 {
			t.Errorf("evictions = %v, want none — every attempt failed", *evicted)
		}
	})
}

// TestReconcileCoreDNSSpreadOnlyTouchesStratumsCoreDNS: kube-dns pods this
// deployer did not apply are never evicted, even in the shape it would fix.
func TestReconcileCoreDNSSpreadOnlyTouchesStratumsCoreDNS(t *testing.T) {
	for _, tc := range []struct {
		name   string
		deploy *appsv1.Deployment
	}{
		{"no CoreDNS Deployment at all", nil},
		{"a CoreDNS Deployment from somewhere else", corednsDeploy(map[string]string{"app.kubernetes.io/managed-by": "Helm"})},
		{"another stack's label", corednsDeploy(map[string]string{group + "/stack": "local-path"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := freshSpreader()
			cs, evicted := spreadCluster(tc.deploy)
			sp.reconcileCoreDNSSpread(context.Background(), cs, epoch)
			if len(*evicted) != 0 {
				t.Errorf("evicted %v from a CoreDNS this operator does not manage", *evicted)
			}
		})
	}
}

// TestReconcileCoreDNSSpreadMovesWhatTheDeployerLabels pins the shared stack
// name end to end: the deployer stamps the objects of the bundled coredns/
// directory with stratum.ciq.com/stack=coredns, and the spreader's gate moves
// exactly the CoreDNS carrying that label. Both sides read the one exported
// constant CoreDNSStack; if either side ever drifts from it, Stratum's own
// CoreDNS stops being spread — or a foreign one starts being.
func TestReconcileCoreDNSSpreadMovesWhatTheDeployerLabels(t *testing.T) {
	// The deployer applies the shipped coredns bundle: stack "coredns", named
	// by the bundle's first-level directory under version.ManifestsDir.
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	dir := r.dir
	writeManifest(t, filepath.Join(dir, "coredns", "coredns.yaml"), configMap("coredns", "kube-system"))
	r.reconcile(context.Background(), "10.0.0.1", "10.0.0.11")
	patches := assertApplied(t, r, "coredns")
	label, ok := stackLabel(t, patches[0])
	if !ok || label != "coredns" {
		t.Fatalf("deployer stamped stack label %q (present %v) on the coredns/ bundle, want coredns", label, ok)
	}

	// The spreader moves the Deployment carrying exactly that label.
	sp := freshSpreader()
	cs, evicted := spreadCluster(corednsDeploy(map[string]string{group + "/stack": label}))
	sp.reconcileCoreDNSSpread(context.Background(), cs, epoch)
	if len(*evicted) != 1 || (*evicted)[0] != "coredns-b" {
		t.Errorf("evictions = %v, want exactly [coredns-b] — the spreader must move the CoreDNS the deployer labelled", *evicted)
	}
}

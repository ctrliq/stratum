package upgrade

// The upgrade plan is owned here. Plan is the orchestrator's own
// classification, Preflight and ordering, over any kubernetes.Interface, so
// `upgrade --cluster`'s dry run and the MCP upgrade_plan tool render one plan
// instead of two. Classification is canonical: a node is a control plane only
// when the Stratum label is exactly "true", with the etcd-roster fallback;
// the kubeadm role label alone does not qualify, because counting a
// mislabeled or unlabeled node would put it into the serial batch that must
// keep etcd quorum.

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// planKubeadmCPLabel is the kubeadm role label, spelled here rather than
// imported, so the test shows the exact label whose presence must not qualify.
const planKubeadmCPLabel = "node-role.kubernetes.io/control-plane"

// planNodeAPI is one Node object as the API reports it: Ready, with the given
// labels, kubelet version and in-progress record.
func planNodeAPI(name string, labels map[string]string, kubeletVersion, inProgress string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: kubeletVersion},
		},
	}
	if inProgress != "" {
		n.Annotations = map[string]string{k8sutil.UpgradeInProgressAnnotation: inProgress}
	}
	return n
}

// TestNodeFromAPIControlPlaneOnlyWhenTheStratumLabelIsTrue: the exported
// classifier carries the orchestrator's rule. The kubeadm role label, and any
// Stratum label but exactly "true", plans the node as a worker.
func TestNodeFromAPIControlPlaneOnlyWhenTheStratumLabelIsTrue(t *testing.T) {
	for _, c := range []struct {
		name   string
		labels map[string]string
		wantCP bool
	}{
		{"label true", map[string]string{k8sutil.ControlPlaneLabel: "true"}, true},
		{"label false", map[string]string{k8sutil.ControlPlaneLabel: "false"}, false},
		{"label empty", map[string]string{k8sutil.ControlPlaneLabel: ""}, false},
		{"only the kubeadm label", map[string]string{planKubeadmCPLabel: ""}, false},
		{"no labels", nil, false},
	} {
		n := NodeFromAPI(planNodeAPI("n1", c.labels, "v1.37.9", ""))
		if n.IsCP != c.wantCP {
			t.Errorf("%s: NodeFromAPI(...).IsCP = %v, want %v", c.name, n.IsCP, c.wantCP)
		}
	}
}

// TestNodeFromAPITrimsTheKubeletBuildSuffix: an SKE kubelet reports
// "v<k8s>-stratum"; compared raw, the version never equalled the bare
// in-progress record, so an interrupted node looked finished.
func TestNodeFromAPITrimsTheKubeletBuildSuffix(t *testing.T) {
	n := NodeFromAPI(planNodeAPI("w1", nil, "v1.37.9-stratum", "v1.37.9"))
	if n.Version != "v1.37.9" || n.Interrupted() != "v1.37.9" {
		t.Fatalf("NodeFromAPI = %+v; want version v1.37.9, interrupted at v1.37.9", n)
	}
}

// TestPlanClassifiesOrdersAndSteps: Plan over a kubernetes.Interface classifies
// the way the orchestrator does, orders control planes before workers in the
// order the API served them, and steps the rollout: one serial control plane
// at a time, then workers in batches of at most maxUnavailable.
func TestPlanClassifiesOrdersAndSteps(t *testing.T) {
	cs := fake.NewSimpleClientset(
		planNodeAPI("cp-1", map[string]string{k8sutil.ControlPlaneLabel: "true"}, "v1.37.9", ""),
		planNodeAPI("m-2", map[string]string{planKubeadmCPLabel: ""}, "v1.37.9", ""),
		planNodeAPI("w-1", nil, "v1.37.9", ""),
		planNodeAPI("z-1", map[string]string{k8sutil.ControlPlaneLabel: "false"}, "v1.37.9", ""),
	)

	plan, err := Plan(context.Background(), cs, "v1.37.10", 2)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !plan.Allowed || plan.Blocker != "" {
		t.Fatalf("Plan = allowed %v, blocker %q; want a clean plan", plan.Allowed, plan.Blocker)
	}
	if plan.NodeCount != 4 {
		t.Errorf("NodeCount = %d, want 4", plan.NodeCount)
	}

	wantNodes := []struct {
		name string
		cp   bool
	}{
		{"cp-1", true},
		{"m-2", false},
		{"w-1", false},
		{"z-1", false},
	}
	if len(plan.Nodes) != len(wantNodes) {
		t.Fatalf("Nodes = %+v, want %d classified nodes", plan.Nodes, len(wantNodes))
	}
	for i, w := range wantNodes {
		if got := (struct {
			name string
			cp   bool
		}{plan.Nodes[i].Name, plan.Nodes[i].IsCP}); got != w {
			t.Errorf("Nodes[%d] = %v, want %v (the kubeadm label and the \"false\" label are workers)", i, got, w)
		}
	}

	wantSteps := []PlanStep{
		{Order: 1, Nodes: []string{"cp-1"}, Role: "control-plane", Serial: true},
		{Order: 2, Nodes: []string{"m-2", "w-1"}, Role: "worker"},
		{Order: 3, Nodes: []string{"z-1"}, Role: "worker"},
	}
	if !reflect.DeepEqual(plan.Steps, wantSteps) {
		t.Errorf("Steps = %+v, want %+v", plan.Steps, wantSteps)
	}

	// A maxUnavailable below one means one: every worker rolls serially.
	solo, err := Plan(context.Background(), cs, "v1.37.10", 0)
	if err != nil {
		t.Fatalf("Plan with maxUnavailable 0: %v", err)
	}
	if len(solo.Steps) != 4 {
		t.Fatalf("maxUnavailable 0 gave %d steps, want the serial 4: %+v", len(solo.Steps), solo.Steps)
	}
	for i, s := range solo.Steps[1:] {
		if len(s.Nodes) != 1 || !s.Serial {
			t.Errorf("worker step %d = %+v, want one serial worker", i+2, s)
		}
	}
}

// TestPlanRunsPreflight: a cluster the orchestrator refuses plans as refused —
// Allowed false, the orchestrator's reason named, and no steps offered.
func TestPlanRunsPreflight(t *testing.T) {
	cs := fake.NewSimpleClientset(
		planNodeAPI("cp-1", map[string]string{k8sutil.ControlPlaneLabel: "true"}, "v1.37.9", ""),
		planNodeAPI("w-1", nil, "v1.37.9-stratum", "v1.37.9"),
	)
	plan, err := Plan(context.Background(), cs, "v1.37.10", 1)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Allowed {
		t.Fatalf("Plan = allowed; want the unfinished-upgrade refusal")
	}
	if !strings.Contains(plan.Blocker, "unfinished upgrade to v1.37.9") {
		t.Errorf("Blocker = %q, want the orchestrator's refusal naming the interrupted upgrade", plan.Blocker)
	}
	if len(plan.Steps) != 0 {
		t.Errorf("a refused plan carries %d steps; want none", len(plan.Steps))
	}
	if plan.NodeCount != 2 {
		t.Errorf("NodeCount = %d, want the cluster still counted (2)", plan.NodeCount)
	}
}

func TestPlanUsesTheEtcdRosterForUnlabeledControlPlanes(t *testing.T) {
	for _, degraded := range []bool{false, true} {
		t.Run(map[bool]string{false: "serial control planes", true: "no quorum margin"}[degraded], func(t *testing.T) {
			swap(t, &fallbackNodeIP, func() (string, error) { return "192.0.2.1", nil })
			rosterCalls := 0
			swap(t, &fallbackMemberIPs, func(ip string) ([]string, error) {
				rosterCalls++
				if ip != "192.0.2.1" {
					t.Fatalf("roster addressed %q", ip)
				}
				return []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, nil
			})
			cs := fake.NewSimpleClientset()
			for i, name := range []string{"cp-1", "cp-2", "cp-3", "worker"} {
				n := planNodeAPI(name, nil, "v1.37.9", "")
				n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.9"}[i]}}
				if degraded && i == 2 {
					n.Status.Conditions[0].Status = corev1.ConditionFalse
				}
				if _, err := cs.CoreV1().Nodes().Create(t.Context(), n, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := Plan(t.Context(), cs, "v1.37.10", 3)
			if err != nil {
				t.Fatal(err)
			}
			if rosterCalls != 1 {
				t.Fatalf("Plan read the etcd roster %d times, want once before preflight", rosterCalls)
			}
			var dryRun bytes.Buffer
			err = UpgradeCluster(&dryRun, ClusterOptions{Client: cs, Target: "v1.37.10", From: "staged-stratum", WorkersMaxUnavailable: 3, DryRun: true})
			if degraded {
				if plan.Allowed || plan.Blocker == "" || len(plan.Steps) != 0 {
					t.Fatalf("degraded etcd plan = %+v; want a preflight refusal", plan)
				}
				if err == nil || err.Error() != "preflight: "+plan.Blocker {
					t.Fatalf("dry run error = %v, want the plan's blocker %q", err, plan.Blocker)
				}
				return
			}
			if err != nil || !plan.Allowed || len(plan.Steps) != 4 {
				t.Fatalf("plan = %+v, dry run error = %v", plan, err)
			}
			for i, step := range plan.Steps[:3] {
				if step.Role != "control-plane" || !step.Serial || len(step.Nodes) != 1 {
					t.Errorf("etcd member step %d = %+v; must upgrade serially", i, step)
				}
				if !strings.Contains(dryRun.String(), step.Nodes[0]+strings.Repeat(" ", 20-len(step.Nodes[0]))+" control-plane") {
					t.Errorf("dry run does not classify %s as a control plane:\n%s", step.Nodes[0], dryRun.String())
				}
			}
		})
	}
}

// TestPlanRefusesCollidingUpgradeJobNames: the orchestrator's Job-name gate
// travels with the plan, so upgrade_plan reports the same refusal the rollout
// makes. A long node's hashed Job name can equal a short node's own name —
// the pair TestValidateUpgradeJobNames constructs — and before this gate moved
// into the plan, the MCP preview called that cluster allowed while
// upgrade_apply refused it.
func TestPlanRefusesCollidingUpgradeJobNames(t *testing.T) {
	const prefix = "stratum-upgrade-"
	long := strings.Repeat("a", 63) + ".worker-1"
	if errs := validation.IsDNS1123Subdomain(long); len(errs) != 0 {
		t.Fatalf("long node name is invalid: %v", errs)
	}
	short := strings.TrimPrefix(jobName(long), prefix)
	if len(prefix+short) > maxJobNameLen || jobName(short) != jobName(long) {
		t.Fatalf("fixture does not collide: %q and %q", long, short)
	}
	if errs := validation.IsDNS1123Subdomain(short); len(errs) != 0 {
		t.Fatalf("short node name is invalid: %v", errs)
	}

	plan := planFromNodes("v1.37.10", []Node{{Name: long}, {Name: "worker-2"}, {Name: short}}, 1)
	if plan.Allowed {
		t.Fatalf("plan = allowed; want the Job-name collision refusal")
	}
	if !strings.Contains(plan.Blocker, long) || !strings.Contains(plan.Blocker, short) {
		t.Errorf("Blocker = %q, want the collision naming both nodes", plan.Blocker)
	}
	if len(plan.Steps) != 0 {
		t.Errorf("a refused plan carries %d steps; want none", len(plan.Steps))
	}
	if plan.NodeCount != 3 {
		t.Errorf("NodeCount = %d, want the cluster still counted (3)", plan.NodeCount)
	}
}

func TestPlanReturnsAnUnavailableRoster(t *testing.T) {
	want := errors.New("etcd is unavailable")
	swap(t, &fallbackNodeIP, func() (string, error) { return "192.0.2.1", nil })
	swap(t, &fallbackMemberIPs, func(string) ([]string, error) { return nil, want })
	cs := fake.NewSimpleClientset(planNodeAPI("cp-1", nil, "v1.37.9", ""))
	if _, err := Plan(t.Context(), cs, "v1.37.10", 1); !errors.Is(err, want) {
		t.Fatalf("Plan error = %v, want roster failure %v", err, want)
	}
}

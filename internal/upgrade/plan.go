package upgrade

import (
	"context"

	"k8s.io/client-go/kubernetes"
)

// PlanStep is one step of the rollout.
type PlanStep struct {
	Order  int
	Nodes  []string
	Role   string // "control-plane" or "worker"
	Serial bool
}

// A refused plan (Allowed false) names the reason in Blocker and carries no steps.
type ClusterPlan struct {
	Target    string
	Allowed   bool
	Blocker   string
	NodeCount int
	Nodes     []Node // classified, in the order the API served them
	Steps     []PlanStep
}

// Plan classifies the cluster, runs Preflight and the Job-name gate, and orders the rollout.
func Plan(ctx context.Context, cs kubernetes.Interface, target string, workersMaxUnavailable int) (ClusterPlan, error) {
	nodes, err := classifyNodes(ctx, cs)
	if err != nil {
		return ClusterPlan{}, err
	}
	if err := controlPlaneFallback(nodes); err != nil {
		return ClusterPlan{}, err
	}
	return planFromNodes(target, nodes, workersMaxUnavailable), nil
}

func planFromNodes(target string, nodes []Node, workersMaxUnavailable int) ClusterPlan {
	p := ClusterPlan{Target: target, NodeCount: len(nodes), Nodes: nodes}
	if reason := Preflight(target, nodes); reason != "" {
		p.Blocker = reason
		return p
	}
	// The gate travels with the plan, so upgrade_plan reports the same refusal the rollout makes.
	if err := validateUpgradeJobNames(nodes); err != nil {
		p.Blocker = err.Error()
		return p
	}
	p.Allowed = true
	p.Steps = planSteps(orderedNodes(nodes), workersMaxUnavailable)
	return p
}

func planSteps(ordered []Node, workersMaxUnavailable int) []PlanStep {
	if workersMaxUnavailable < 1 {
		workersMaxUnavailable = 1
	}
	var steps []PlanStep
	add := func(nodes []string, role string, serial bool) {
		steps = append(steps, PlanStep{Order: len(steps) + 1, Nodes: nodes, Role: role, Serial: serial})
	}
	var workers []string
	for _, n := range ordered {
		if n.IsCP {
			add([]string{n.Name}, "control-plane", true)
			continue
		}
		workers = append(workers, n.Name)
	}
	for i := 0; i < len(workers); i += workersMaxUnavailable {
		end := min(i+workersMaxUnavailable, len(workers))
		add(workers[i:end], "worker", workersMaxUnavailable == 1)
	}
	return steps
}

package upgrade

// Plan owns the classification, preflight and ordering shared by the CLI
// dry run, MCP preview and cluster rollout. Only the Stratum control-plane
// label set to "true" qualifies; when no node has it, the local etcd member
// roster identifies control planes before preflight counts quorum.

import (
	"context"

	"k8s.io/client-go/kubernetes"
)

// PlanStep is one step of the rollout: the nodes it upgrades together, their
// role, and whether it must not run alongside another step.
type PlanStep struct {
	Order  int
	Nodes  []string
	Role   string // "control-plane" or "worker"
	Serial bool
}

// ClusterPlan is what a cluster upgrade to Target would do: the nodes as
// classified, whether preflight allows the upgrade, and the steps it would
// roll in. A refused plan (Allowed false) carries the reason in Blocker and
// no steps.
type ClusterPlan struct {
	Target    string
	Allowed   bool
	Blocker   string
	NodeCount int
	Nodes     []Node // classified, in the order the API served them
	Steps     []PlanStep
}

// Plan classifies the cluster by the Stratum label rule (NodeFromAPI), falling
// back to this node's etcd roster for an unlabeled cluster, runs Preflight and
// the upgrade-Job-name gate, and orders the rollout: control planes one at a
// time in API order, then workers in batches of at most workersMaxUnavailable
// (below one means one). A node-list or roster failure is an error; a preflight
// or Job-name refusal is Allowed false with Blocker set.
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

// planFromNodes is Preflight, the Job-name gate and the rollout ordering over
// already-classified nodes.
func planFromNodes(target string, nodes []Node, workersMaxUnavailable int) ClusterPlan {
	p := ClusterPlan{Target: target, NodeCount: len(nodes), Nodes: nodes}
	if reason := Preflight(target, nodes); reason != "" {
		p.Blocker = reason
		return p
	}
	// The Job-name gate travels with the plan, so upgrade_plan reports the
	// same refusal the rollout makes: a long node's hashed Job name can equal
	// a short node's own name, and the rollout refuses the pair before any
	// node is cordoned.
	if err := validateUpgradeJobNames(nodes); err != nil {
		p.Blocker = err.Error()
		return p
	}
	p.Allowed = true
	p.Steps = planSteps(orderedNodes(nodes), workersMaxUnavailable)
	return p
}

// planSteps is the rollout's ordering as steps, from orderedNodes' list:
// every control plane serially first, then workers in batches of at most
// workersMaxUnavailable.
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

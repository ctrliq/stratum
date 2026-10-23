package upgrade

import "github.com/ctrliq/stratum/internal/nodelock"

// lockNode takes this node's maintenance lock for the life of one node-side
// upgrade or rollback, and refuses rather than waits when another holds it.
//
// It is the fence the cluster-upgrade lease cannot be. The lease
// binds only orchestrators that know about it, and clearStaleUpgradeJobs'
// read-then-create leaves a window an unfenced orchestrator can create a
// second Job in. Both Jobs are privileged and nsenter into PID 1, so they meet
// here, on the host, whatever created them — and so does an operator's
// by-hand `stratum upgrade --from` or `rollback` beside a running Job. Taking
// it here moves the fence to where the damage would happen, and it needs
// nothing from the other side but a binary that also takes it.
//
// The mechanics — the flock, the recorded holder, the refusal wording — live
// in internal/nodelock, the leaf package every node-maintenance operation
// shares, so every binary meets on the same path and the same rules.
func lockNode(cmd string) (release func(), err error) {
	return nodelock.Acquire(cmd)
}

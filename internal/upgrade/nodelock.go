package upgrade

import "github.com/ctrliq/stratum/internal/nodelock"

// Refuses rather than waits; the cluster lease binds only orchestrators that know about it.
func lockNode(cmd string) (release func(), err error) {
	return nodelock.Acquire(cmd)
}

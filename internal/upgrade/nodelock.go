package upgrade

import "github.com/ctrliq/stratum/internal/nodelock"

// Refuses rather than waits; the lease binds only its orchestrators, so all meet on the host lock.
func lockNode(cmd string) (release func(), err error) {
	return nodelock.Acquire(cmd)
}

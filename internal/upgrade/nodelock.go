package upgrade

import "github.com/ctrliq/stratum/internal/nodelock"

// lockNode fences the node itself on the host: the cluster lease cannot bind any other run.
func lockNode(cmd string) (release func(), err error) {
	return nodelock.Acquire(cmd)
}

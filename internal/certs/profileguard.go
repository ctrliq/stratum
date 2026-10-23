package certs

import "github.com/ctrliq/stratum/internal/nodestate"

// RequireNodeProfile refuses a certificate operation on a node whose saved
// profile this binary does not register, before the operation writes
// anything. The renderers an operation reaches later — the static-pod
// restarts, the finalize preflight — stop on the same profile, but mid-run,
// with leaves reissued and kubeconfigs rewritten. A node with no state yet
// runs no profile. op names the operation the way its command is spelled.
func RequireNodeProfile(op string) error {
	return nodestate.RequireNodeProfile("certs " + op)
}

package certs

import "github.com/ctrliq/stratum/internal/nodestate"

// Stops before any write; the later renderers stop only after leaves are reissued.
func RequireNodeProfile(op string) error {
	return nodestate.RequireNodeProfile("certs " + op)
}

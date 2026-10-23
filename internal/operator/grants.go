// grants.go — the operator's pass over the stratum-certs grant entries. The
// Secret carries one ciphertext entry per live control-plane join grant, keyed
// by bootstrap token ID, beside the legacy certs entry. Owner references, not
// data entries, are what garbage collection acts on, so an expired or revoked
// grant's entry stays until something drops it. Every mint drops them inside
// its own update (ha.UploadCerts); this pass drops them as well, so they go
// even when no one mints. Which entries are grants, and when a grant is gone,
// is internal/ha's rule (ha.PruneExpiredGrants), the package that writes the
// entries. The pass rides the operator's reconcile loop as part of the cleanup
// step Run binds with cleanupStepFor, beside departed-worker removal.
package operator

import (
	"context"
	"log"

	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/ha"
)

// reconcileGrantEntries drops the stratum-certs grant entries whose bootstrap
// token no longer exists, leaving the legacy certs entry, every live grant's
// entry and every owner reference untouched, and logs what kept it from
// finishing; the next pass tries again.
func reconcileGrantEntries(ctx context.Context, cs kubernetes.Interface) {
	if err := ha.PruneExpiredGrants(ctx, cs); err != nil {
		log.Printf("stratum-operator: grant entries: %v", err)
	}
}

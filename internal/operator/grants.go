// The apiserver garbage-collects owner references, not data entries, so stale grant entries linger.
package operator

import (
	"context"
	"log"

	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/ha"
)

// Drops the grant entries whose bootstrap token is gone; errors wait for the next pass.
func reconcileGrantEntries(ctx context.Context, cs kubernetes.Interface) {
	if err := ha.PruneExpiredGrants(ctx, cs); err != nil {
		log.Printf("stratum-operator: grant entries: %v", err)
	}
}

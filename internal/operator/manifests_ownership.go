package operator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ctrliq/stratum/internal/addons"
)

// recordBundledOwnership persists trusted intent before applying configurable
// bundles. A crash or a later failed document must not orphan their resources.
// Client-writable labels carry no ownership authority: only staged manifests
// and the administrator-protected inventory can grant deletion permission.
func (d *manifestDeployer) recordBundledOwnership(ctx context.Context, stack string, ref objRef) error {
	if stack != addons.DashboardStack && stack != "local-path" && stack != addons.RuntimeClassStack {
		return nil
	}
	legacy, err := readMigrationRecord(filepath.Join(d.dir, appliedRecordName))
	if err != nil && !os.IsNotExist(err) {
		inv, ierr := loadInventory(ctx, d.cs)
		if ierr != nil || !inv.Recovery {
			return fmt.Errorf("read local inventory before bundled apply: %w", err)
		}
		legacy = nil // explicit recovery accepts the unreadable contribution
	}
	current := map[string][]objRef{stack: {ref}}
	if err := writeInventory(ctx, d.cs, func(inv pruneInventory) pruneInventory {
		// Contributing this node can close migration. Preserve its complete
		// legacy history before doing so, including failed-delete retries.
		inv.addNode(d.node, legacy)
		inv.addNode(d.node, current)
		d.updateLooseOwnership(&inv, current, nil)
		return inv
	}); err != nil {
		return fmt.Errorf("persist bundled ownership before apply: %w", err)
	}
	return nil
}

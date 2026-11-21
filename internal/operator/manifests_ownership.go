package operator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ctrliq/stratum/internal/addons"
)

// persist trusted intent before applying: a crash must not orphan the bundle's resources.
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
		// preserve the legacy history before contributing, or migration closes on incomplete evidence.
		inv.addNode(d.node, legacy)
		inv.addNode(d.node, current)
		d.updateLooseOwnership(&inv, current, nil)
		return inv
	}); err != nil {
		return fmt.Errorf("persist bundled ownership before apply: %w", err)
	}
	return nil
}

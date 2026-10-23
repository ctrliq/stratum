// Package profilefiles writes a node profile's config files where the
// renderers read them.
package profilefiles

import (
	"fmt"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// Write writes p's config files at version.ProfileConfigFile, 0644 (none
// holds a secret). Every file renders before any is written, so a profile
// that cannot render one changes nothing, and each write is atomic, so a
// write that fails part way leaves the file already at the path.
func Write(p profile.Profile) error {
	ctx := profile.Context{KubernetesVersion: version.K8sVersion}
	bodies := make([][]byte, len(p.ConfigFiles))
	for i, f := range p.ConfigFiles {
		b, err := f.Render(ctx)
		if err != nil {
			return fmt.Errorf("render the %s profile's %s: %w", p.Name, f.Name, err)
		}
		bodies[i] = b
	}
	for i, f := range p.ConfigFiles {
		if err := atomicfile.WriteFile(version.ProfileConfigFile(f.Name), bodies[i], 0o644); err != nil {
			return fmt.Errorf("write the %s profile's %s: %w", p.Name, f.Name, err)
		}
	}
	return nil
}

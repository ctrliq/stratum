// Package profilefiles writes a node profile's config files where the renderers read them.
package profilefiles

import (
	"fmt"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// Write renders every file before any is written, so a render failure changes nothing.
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

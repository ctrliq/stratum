// This fixed consumer covers the public declarations, methods and fields of
// pkg/cli at tag v1.37.1-stratum.0-rc.6 (commit
// 96c53c6e9a131cd531b69bda5b2dba95db9154a1), the 1.37 line's newest release
// (root.go and toolset.go).
// Keep it independent of the regeneratable API golden: a deliberate breaking
// change must review and update the released-consumer contract separately.
package releasedconsumer

import (
	"github.com/ctrliq/stratum/pkg/cli"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
	cobra "github.com/spf13/cobra"
)

var (
	componentVersion string
	exitCode         int
	_                error                                                                             = cli.ErrNoCommand
	_                error                                                                             = cli.ErrCheckFailed
	_                error                                                                             = &cli.ErrExitCode{Code: exitCode}
	_                error                                                                             = &cli.ErrUsage{}
	_                func(*cli.ErrExitCode) string                                                     = (*cli.ErrExitCode).Error
	_                func(*cli.ErrUsage) string                                                        = (*cli.ErrUsage).Error
	_                func(*cli.ErrUsage) error                                                         = (*cli.ErrUsage).Unwrap
	_                func(cli.Tier) string                                                             = cli.Tier.String
	_                func(*cli.Registrar, cli.Tier) bool                                               = (*cli.Registrar).Allows
	_                func(*cli.Registrar) []string                                                     = (*cli.Registrar).Registered
	_                func(...cli.Option) *cobra.Command                                                = cli.NewRootCommand
	_                func(string) cli.Option                                                           = cli.WithVersionOverride
	_                func(cli.ComponentVersions) cli.Option                                            = cli.WithComponentVersions
	_                func(...cli.ToolSet) cli.Option                                                   = cli.WithToolSets
	_                func(string, string) *mcp.Tool                                                    = cli.ReadTool
	_                func(string, string) *mcp.Tool                                                    = cli.WriteTool
	_                func(string, string) *mcp.Tool                                                    = cli.DestructiveTool
	_                func(*cli.Registrar, cli.Tier, *mcp.Tool, mcp.ToolHandlerFor[struct{}, struct{}]) = cli.AddTool[struct{}, struct{}]
	_                                                                                                  = []cli.Tier{cli.TierRead, cli.TierWrite, cli.TierDestructive}
	_                                                                                                  = cli.ComponentVersions{
		Containerd: componentVersion, Runc: componentVersion, KubeVip: componentVersion,
		Cilium: componentVersion, CoreDNS: componentVersion, Konnectivity: componentVersion,
	}
)

// Both assignment directions pin field types, including defined string/int
// types that would still accept untyped literals but break released callers.
func consumeFields(v cli.ComponentVersions, e cli.ErrExitCode) {
	componentVersion = v.Containerd
	componentVersion = v.Runc
	componentVersion = v.KubeVip
	componentVersion = v.Cilium
	componentVersion = v.CoreDNS
	componentVersion = v.Konnectivity
	exitCode = e.Code
}

// An old extension must still implement ToolSet without adopting new methods.
type extension struct{}

func (extension) Name() string            { return "released-extension" }
func (extension) Register(*cli.Registrar) {}

var _ cli.ToolSet = extension{}

// ToolSet's existing methods remain callable through the public interface.
func consumeToolSet(set cli.ToolSet, registrar *cli.Registrar) string {
	set.Register(registrar)
	return set.Name()
}

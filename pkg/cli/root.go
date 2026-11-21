// Package cli builds the stratum command tree: the engine's one deliberate public API surface.
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/version"
)

// Lets main exit 2 without a redundant error line on top of the usage already printed.
var ErrNoCommand = errors.New("no command given")

// Lets main exit 2 for a bad flag without repeating what the FlagErrorFunc printed.
type ErrUsage struct{ err error }

func (e *ErrUsage) Error() string { return e.err.Error() }
func (e *ErrUsage) Unwrap() error { return e.err }

// Lets main exit 1 with no extra line: the report is printed and pkg/cli must not os.Exit.
var ErrCheckFailed = errors.New("the check found failures")

// Carries the exact code so main can propagate it; the message was already printed.
type ErrExitCode struct{ Code int }

func (e *ErrExitCode) Error() string {
	return fmt.Sprintf("exited with status %d", e.Code)
}

// Per-field overrides for the stamped component versions an importer may still supply.
type ComponentVersions struct {
	Containerd string
	Runc       string
	KubeVip    string
	Cilium     string
	CoreDNS    string
	// Deprecated: konnectivity is not shipped; the field stays for released importers.
	Konnectivity string
}

type rootConfig struct {
	displayVersion string
	components     ComponentVersions
	toolSets       []ToolSet
	stacks         []StackFunc
	profiles       []Profile
}

type Option func(*rootConfig)

// Lets an importer report its own product version; empty leaves the stamped version.
func WithVersionOverride(display string) Option {
	return func(c *rootConfig) {
		if display != "" {
			c.displayVersion = display
		}
	}
}

// Deprecated: internal/version stamps the defaults; the option still overrides.
func WithComponentVersions(v ComponentVersions) Option {
	return func(c *rootConfig) { c.components = v }
}

// For importers: the stamps live in an internal package they cannot read directly.
func StampedVersions() ComponentVersions {
	return ComponentVersions{
		Containerd: version.ContainerdVersion,
		Runc:       version.RuncVersion,
		KubeVip:    version.KubeVipVersion,
		Cilium:     version.CiliumVersion,
		CoreDNS:    version.CorednsVersion,
	}
}

// The linker does not report a dead -X variable, so an unstamped binary fails silently here.
func refuseUnstampedBuild(op string) error {
	if version.K8sVersion != "unknown" {
		return nil
	}
	return fmt.Errorf("%s: this binary was built without the engine's link-time stamps: its Kubernetes version stamp is "+
		"\"unknown\", so it would render unpullable :unknown images and target Kubernetes \"unknown\". "+
		"Build with the engine's Makefile so the -X stamps are linked (make engine-ldflags publishes them), then retry", op)
}

// Importers add their own commands with AddCommand before Execute.
func NewRootCommand(opts ...Option) *cobra.Command {
	cfg := rootConfig{displayVersion: version.StratumVersion}
	for _, opt := range opts {
		opt(&cfg)
	}

	root := &cobra.Command{
		Use:   "stratum",
		Short: "Stratum Kubernetes Engine",
		Long: `stratum — Stratum Kubernetes Engine

A self-extracting node agent and installer: it knows its own version, knows
what payload it carries, can stage that payload, and runs a node.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			pinNodeUmask(cmd, args)
			registerProfilesForRender(cfg)
		},
		Args: unknownCommand,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SetOut(cmd.ErrOrStderr())
			if err := cmd.Help(); err != nil {
				return err
			}
			return ErrNoCommand
		},
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		fmt.Fprintln(cmd.ErrOrStderr(), err)
		_ = cmd.Usage()
		return &ErrUsage{err: err}
	})

	root.AddCommand(
		newVersionCommand(cfg),
		newStageCommand(),
		newWhichCommand(),
		newUpCommand(cfg),
		newOperatorCommand(),
		newTokenCommand(),
		newCertsCommand(),
		newSnapshotCommand(),
		newUpgradeCommand(),
		newRollbackCommand(),
		newRegistryCommand(),
		newRuntimeCommand(),
		newNodeCommand(),
		newHARepointCommand(),
		newUninstallCommand(),
		newKubectlCommand(),
		newEtcdctlCommand(),
		newMCPCommand(cfg),
	)

	return root
}

// Centralized so every group refuses an unknown verb with usage, not cobra's exit-0 help.
func newGroupCommand(use, short string, subs ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  unknownCommand,
		RunE:  requireSubcommand,
	}
	cmd.AddCommand(subs...)
	return cmd
}

// Its presence makes cobra treat the group as Runnable, so Args validation runs at all.
func requireSubcommand(cmd *cobra.Command, args []string) error {
	cmd.SetOut(cmd.ErrOrStderr())
	if err := cmd.Help(); err != nil {
		return err
	}
	return fmt.Errorf("%s requires a subcommand; see '%s --help'", cmd.CommandPath(), cmd.CommandPath())
}

// Cobra's legacyArgs prints nothing under SilenceUsage; this prints usage first.
func unknownCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	cmd.SetOut(cmd.ErrOrStderr())
	_ = cmd.Help()
	return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
}

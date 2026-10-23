// Package cli builds the stratum command tree as a *cobra.Command. It is
// stratum's one deliberate public API surface: an importer (e.g. a downstream build)
// can call NewRootCommand, then AddCommand additional subcommands onto the
// returned root before calling Execute, without reimplementing any of the
// base commands. tools/check-cli-api-boundary.sh enforces that this
// package's exported surface never leaks an internal/* type back out, which
// is what makes that safe to import from outside this module.
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/version"
)

// ErrNoCommand is returned when stratum is invoked with no subcommand, so
// main can exit 2 (usage error) instead of 1 without printing a redundant
// "stratum: <err>" line on top of the usage cmd.Help() already printed —
// a bare `stratum` invocation is a usage error, not a run failure.
var ErrNoCommand = errors.New("no command given")

// ErrUsage wraps a flag-parsing failure so main can exit 2 (usage error)
// instead of 1, without printing a redundant "stratum: <err>" line on top of
// the error + usage text root's FlagErrorFunc already printed — a bad
// flag is a usage error, not a run failure.
type ErrUsage struct{ err error }

func (e *ErrUsage) Error() string { return e.err.Error() }
func (e *ErrUsage) Unwrap() error { return e.err }

// ErrCheckFailed is returned by a check command whose report found a
// failure, such as an importer's audit command, so main can exit 1 (a bare
// lint-style nonzero exit — the full report has already been printed, so no
// additional "stratum: ..." line is wanted) without an os.Exit call inside
// pkg/cli itself. pkg/cli is a public, importable
// package, so an os.Exit from inside a RunE would terminate the
// importing process outright — a host binary couldn't observe the failure,
// run its own cleanup, or apply its own exit policy, because the process
// would already be gone.
var ErrCheckFailed = errors.New("the check found failures")

// ErrExitCode is returned when a command's answer is its exit status: the
// kubectl/etcdctl passthrough when the child exits non-zero, and
// `upgrade --check` when the node needs the upgrade. It carries the exact code
// so main can propagate it precisely, without pkg/cli itself calling os.Exit —
// for the same importability reason as ErrCheckFailed above. Whoever returns it
// has already told the operator what it needed to (the passthrough's child
// inherits stdout/stderr; --check prints its answer), so main prints nothing
// additional for this error.
type ErrExitCode struct{ Code int }

func (e *ErrExitCode) Error() string {
	return fmt.Sprintf("exited with status %d", e.Code)
}

// ComponentVersions carries the five component versions `stratum version
// --components` reports. The stamped values live in internal/version
// (ContainerdVersion, RuncVersion, ...) and the command reads them as its
// defaults, so this struct is only the per-field override an importer can
// still supply through WithComponentVersions.
type ComponentVersions struct {
	Containerd string
	Runc       string
	KubeVip    string
	Cilium     string
	CoreDNS    string
	// Deprecated: konnectivity is not shipped, so nothing reads this. The
	// field stays only because released importers compile against it.
	Konnectivity string
}

type rootConfig struct {
	displayVersion string
	components     ComponentVersions
	// toolSets are contributed by importers through WithToolSets and
	// registered onto `stratum mcp`. See toolset.go.
	toolSets []ToolSet
	// stacks are contributed by importers through WithStacks. See stacks.go.
	stacks []StackFunc
	// profiles are contributed by importers through WithProfiles. See profiles.go.
	profiles []Profile
}

// Option configures the root command returned by NewRootCommand.
type Option func(*rootConfig)

// WithVersionOverride replaces the top-line version string `stratum version`
// reports, letting an importer report its own product version while reusing
// every base command unchanged. Empty (the default) leaves
// internal/version.StratumVersion, the OSS ldflags-stamped version.
func WithVersionOverride(display string) Option {
	return func(c *rootConfig) {
		if display != "" {
			c.displayVersion = display
		}
	}
}

// WithComponentVersions supplies per-field overrides for the component
// versions `stratum version --components` reports: a non-empty field
// replaces the stamped default internal/version carries, an empty field
// leaves the stamp.
//
// Deprecated: the engine stamps its component versions into internal/version,
// which `version --components` reads as its defaults, so an importer no
// longer needs to stamp main-package vars and hand them here. The option is
// not a no-op — it still overrides — and it goes away only through a
// deliberate breaking-release adoption.
func WithComponentVersions(v ComponentVersions) Option {
	return func(c *rootConfig) { c.components = v }
}

// StampedVersions returns the engine's link-time-stamped component versions
// (internal/version) as a value: the same stamps `version --components`
// reports as its defaults. ComponentVersions is a plain struct of strings,
// so the caller receives a copy and cannot reach the package state behind
// it — read-only, because the stamps are this binary's build identity, not
// configuration. It exists because the stamps live in an internal package an
// importer cannot see: a downstream build reporting its own components would
// otherwise have to re-parse `version --components` output. Every field
// reads "unknown" in a build linked without the engine's -X stamp list.
func StampedVersions() ComponentVersions {
	return ComponentVersions{
		Containerd: version.ContainerdVersion,
		Runc:       version.RuncVersion,
		KubeVip:    version.KubeVipVersion,
		Cilium:     version.CiliumVersion,
		CoreDNS:    version.CorednsVersion,
	}
}

// refuseUnstampedBuild is the stamp guard the mutating node commands run
// first (`up`, `upgrade --cluster`): a binary linked without the engine's
// -X stamp list carries K8sVersion "unknown", and starting or rolling a
// cluster from it renders unpullable :unknown image references
// (internal/version stamps every image tag from the same list) and targets
// Kubernetes version "unknown". It refuses before any host or cluster
// mutation and names the missing stamp, so the failure of a downstream
// build's -X flags is diagnosable from the error alone — the Go linker does
// not report an -X flag whose variable is gone, so a silently unstamped
// binary is the failure mode this catches.
func refuseUnstampedBuild(op string) error {
	if version.K8sVersion != "unknown" {
		return nil
	}
	return fmt.Errorf("%s: this binary was built without the engine's link-time stamps: its Kubernetes version stamp is "+
		"\"unknown\", so it would render unpullable :unknown images and target Kubernetes \"unknown\". "+
		"Build with the engine's Makefile so the -X stamps are linked (make engine-ldflags publishes them), then retry", op)
}

// NewRootCommand builds the full stratum command tree. An importer adds its
// own commands with root.AddCommand(...) before calling root.Execute().
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
		newHARepointCommand(),
		newUninstallCommand(),
		newKubectlCommand(),
		newEtcdctlCommand(),
		newMCPCommand(cfg),
	)

	return root
}

// newGroupCommand builds a command whose only job is to group subcommands
// (certs, snapshot, token, registry) — it has no logic of its
// own. unknownCommand and requireSubcommand together make an invalid
// invocation (no verb, or an unmatched one) print usage and report a clear
// error, the same way root itself handles the identical mistake, instead of
// cobra's default of silently printing help and exiting 0 for an unmatched
// verb on a non-root command. Centralizing this here means a future command
// group gets correct behavior automatically instead of depending on each
// constructor to copy the same two field assignments.
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

// requireSubcommand is the RunE every command group (via newGroupCommand)
// uses when invoked with no verb at all — the case unknownCommand itself
// ignores (len(args) == 0) so that Args validation can still reach a
// group's children on a real invocation. RunE's presence is also what makes
// cobra treat the command as Runnable at all, which is a precondition for
// Args to be consulted in the first place. Root handles this identical
// zero-args case with its own separate RunE (see NewRootCommand), not this
// function.
func requireSubcommand(cmd *cobra.Command, args []string) error {
	cmd.SetOut(cmd.ErrOrStderr())
	if err := cmd.Help(); err != nil {
		return err
	}
	return fmt.Errorf("%s requires a subcommand; see '%s --help'", cmd.CommandPath(), cmd.CommandPath())
}

// unknownCommand is an Args validator — used by root and by every command
// group via newGroupCommand — that rejects an unmatched first argument.
// Cobra's own default for a command with subcommands (legacyArgs) does the
// same rejection but, with SilenceUsage set, prints nothing before doing
// so; this prints full usage before reporting an unknown command
// (e.g. `stratum bogus`, or `stratum registry typo`), while a bare invocation
// (no args) still falls through unaffected to the command's own RunE (root's
// ErrNoCommand path, or a group's requireSubcommand).
func unknownCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	cmd.SetOut(cmd.ErrOrStderr())
	_ = cmd.Help()
	return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
}

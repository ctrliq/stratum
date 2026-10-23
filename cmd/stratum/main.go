// Command stratum is the single self-extracting artifact for Stratum Kubernetes
// Engine. It knows its own version, knows what it carries, can stage its
// payload, and runs a node: the control plane as kubelet-managed static pods
// plus the supervised host processes under `stratum up`.
package main

import (
	"os"

	"github.com/ctrliq/stratum/internal/dashboard"
	"github.com/ctrliq/stratum/pkg/cli"
)

func main() {
	// The component stamps `version --components` reports are stamped into
	// internal/version by the Makefile's -X list, so no main-package wiring
	// is needed here; WithComponentVersions remains the deprecated override
	// for an importer that stamps its own vars.
	root := cli.NewRootCommand()
	// `dash` is added here rather than inside NewRootCommand on purpose:
	// pkg/cli is the one package a downstream build imports, so registering
	// the dashboard there would compile its server, proxy and embedded SPA
	// into every embedding binary too.
	root.AddCommand(dashboard.Command())
	// root.go sets SilenceErrors/SilenceUsage so cobra never prints its own
	// "Error: ..." + usage-dump format. The mapping from pkg/cli's error
	// sentinels to exit codes lives in cli.ExitCode — the one package an
	// embedding build imports, so a new sentinel is mapped once where every
	// embedding binary can adopt it instead of in a hand copy here — and
	// ExitCode itself writes the one "stratum: <msg>" line a failing
	// command's error prints when no sentinel has already reported.
	if code := cli.ExitCode(root.Execute(), os.Stderr); code != 0 {
		os.Exit(code)
	}
}

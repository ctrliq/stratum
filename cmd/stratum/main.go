// Command stratum is the single self-extracting Stratum Kubernetes Engine artifact.
package main

import (
	"os"

	"github.com/ctrliq/stratum/internal/dashboard"
	"github.com/ctrliq/stratum/pkg/cli"
)

func main() {
	// Component versions are stamped by the Makefile -X list; no wiring here.
	root := cli.NewRootCommand()
	// Registered here so binaries importing only pkg/cli do not link the dashboard.
	root.AddCommand(dashboard.Command())
	// The sentinel-to-exit-code mapping lives in cli.ExitCode; main owns none.
	if code := cli.ExitCode(root.Execute(), os.Stderr); code != 0 {
		os.Exit(code)
	}
}

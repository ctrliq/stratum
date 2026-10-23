// Command stratum-dash is the dashboard server alone, built into the image the
// cluster runs it from. `stratum dash` is the same command inside the full binary.
package main

import (
	"fmt"
	"os"

	"github.com/ctrliq/stratum/internal/dashboard"
)

func main() {
	cmd := dashboard.Command()
	cmd.Use = "stratum-dash"
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "stratum-dash:", err)
		os.Exit(1)
	}
}

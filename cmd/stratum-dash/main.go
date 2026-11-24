// Command stratum-dash is the dashboard server alone; `stratum dash` is the same command.
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

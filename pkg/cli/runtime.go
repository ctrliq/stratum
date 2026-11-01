package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/cluster"
)

func newRuntimeCommand() *cobra.Command {
	return newGroupCommand("runtime", "Containerd runtime handlers on this node",
		newRuntimeApplyCommand(), newRuntimeCheckCommand(), newRuntimeRestartCommand())
}

// newRuntimeApplyCommand saves this node's runtime handlers and writes
// containerd's drop-in for them, restarting containerd when it changed.
func newRuntimeApplyCommand() *cobra.Command {
	var handlers []string
	var clear bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Write containerd's runtime handlers and restart it when they changed",
		Long: "Writes the containerd drop-in for this node's runtime handlers and restarts containerd when the drop-in changed\n" +
			"or the running containerd has not loaded it.\n\n" +
			"On a worker, --runtime-handler replaces the handlers it saved and --clear removes them; with neither, the saved\n" +
			"handlers are written again. A control plane reads spec.node.runtimeHandlers from the config file it was started\n" +
			"with, so edit that file and run apply with no flags.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			o := cluster.RuntimeApplyOptions{Clear: clear, Restart: true}
			if len(handlers) > 0 {
				parsed, err := cluster.ParseKeyValues("--runtime-handler", handlers)
				if err != nil {
					return err
				}
				o.Handlers = parsed
			}
			res, err := cluster.ApplyRuntimeHandlers(cmd.OutOrStdout(), o)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "runtime handlers from %s: %s\n", res.Source, cluster.FormatKeyValues(res.Handlers))
			switch {
			case res.Restarted:
				fmt.Fprintln(out, "containerd restarted to load the runtime handlers")
			case res.Unloaded:
				fmt.Fprintln(out, "containerd has not loaded the runtime handlers; run `stratum runtime restart`")
			case res.Changed:
				fmt.Fprintln(out, "containerd is not running; it loads the runtime handlers when it next starts")
			default:
				fmt.Fprintln(out, "containerd already has these runtime handlers; nothing restarted")
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&handlers, "runtime-handler", nil, "worker only: a runtime handler, name=/absolute/path/to/runtime (repeatable); the handlers given replace the saved ones")
	cmd.Flags().BoolVar(&clear, "clear", false, "worker only: remove every saved runtime handler")
	return cmd
}

// newRuntimeCheckCommand reports whether containerd will run this node's
// runtime handlers, and exits 1 when it will not.
func newRuntimeCheckCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Report whether containerd will run this node's runtime handlers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := cluster.CheckRuntimeHandlers(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, f := range r.Findings {
				fmt.Fprintf(out, "%-5s %s\n", strings.ToUpper(string(f.Severity)), f.Message)
			}
			if r.Failed() {
				return ErrCheckFailed
			}
			return nil
		},
	}
}

// newRuntimeRestartCommand restarts the supervisor's containerd and relabels
// the node for the runtime handlers it loaded.
func newRuntimeRestartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart containerd and update this node's runtime handler labels; running containers keep running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cluster.RestartRuntime(cmd.ErrOrStderr()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "containerd restarted")
			return nil
		},
	}
}

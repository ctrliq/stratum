package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/version"
)

// The groups `stratum --help` lists the engine's commands under. An importer
// places a command it adds in one by setting the command's GroupID to one of
// these, or adds its own group with root.AddGroup; a command with no group is
// listed under "Additional Commands".
const (
	GroupCluster  = "cluster"
	GroupTools    = "tools"
	GroupAdvanced = "advanced"
)

// commandGroups places each engine command in a help group: the commands
// people run to install and operate a cluster first, the bundled tools next,
// and the ones Stratum runs itself or that only a specialist needs last.
var commandGroups = map[string]string{
	"up":         GroupCluster,
	"token":      GroupCluster,
	"upgrade":    GroupCluster,
	"rollback":   GroupCluster,
	"snapshot":   GroupCluster,
	"certs":      GroupCluster,
	"uninstall":  GroupCluster,
	"version":    GroupCluster,
	"kubectl":    GroupTools,
	"etcdctl":    GroupTools,
	"mcp":        GroupTools,
	"operator":   GroupAdvanced,
	"stage":      GroupAdvanced,
	"which":      GroupAdvanced,
	"node":       GroupAdvanced,
	"runtime":    GroupAdvanced,
	"registry":   GroupAdvanced,
	"ha-repoint": GroupAdvanced,
}

func groupCommands(root *cobra.Command) {
	for _, c := range root.Commands() {
		if id, ok := commandGroups[c.Name()]; ok {
			c.GroupID = id
		}
	}
}

// rootLong is the top of `stratum --help`: what the product is, then the two
// commands a new user runs first, written to paste as they are, with sudo and
// the full path because sudo's PATH usually leaves out /usr/local/bin.
func rootLong(cfg rootConfig) string {
	return fmt.Sprintf(`%s
Kubernetes for the AI era. One signed file installs and upgrades your cluster,
and lets AI agents work on it within limits you set.

Get started (as root):
  sudo %[2]s up              Install and start Stratum on this server
  sudo %[2]s token create    Add another server to the cluster

Docs: https://docs.ciq.com/stratum`, cfg.headline(), version.StratumInstallPath)
}

package etcdsnap

import (
	"fmt"
	"strings"
)

// stillToDoItem is one thing left for the operator once the node's own data is
// restored, whether by `stratum snapshot restore` or by an upgrade rollback that
// restores etcd. body follows the item's number. otherControlPlanes marks the
// items that only apply when there may be other control planes to rejoin: a
// rollback restores a single-member cluster, so it has none.
type stillToDoItem struct {
	otherControlPlanes bool
	body               func(otherControlPlanes bool) string
}

var stillToDoItems = []stillToDoItem{
	{true, func(otherControlPlanes bool) string {
		return "Rejoin every OTHER control plane (on each, in turn):\n" +
			"       stratum snapshot rejoin --server https://<this-seed-ip>:6443 --yes --force\n" +
			"     (--force: their etcd still holds the pre-restore cluster, which a rejoin refuses to\n" +
			"     discard without it. Without --force a rejoin only proceeds once that cluster has\n" +
			"     lost quorum.)\n"
	}},
	{false, func(otherControlPlanes bool) string {
		nodes := "workers"
		if otherControlPlanes {
			nodes = "workers, and each control plane once\n     rejoined"
		}
		return "Restart kubelet on every OTHER node (" + nodes + "). It also holds a pre-restore watch, and until it is restarted it\n" +
			"     runs the pods it started after the snapshot and not the restored ones.\n" +
			"     stratum has no command for this yet; killing it makes the supervisor\n" +
			"     restart it:\n" +
			"       sudo pkill -x kubelet\n"
	}},
	{false, func(otherControlPlanes bool) string {
		return "Check that every Pod is Running with a current IP:\n" +
			"       stratum kubectl get pods -A -o wide\n"
	}},
	{true, func(bool) string { return RestoreRestartSet().RepeatItem() }},
	{false, func(otherControlPlanes bool) string {
		// Only a restore that may have other control planes to rejoin has a window
		// in which the VIP can lead to one that still holds the old data.
		seed := "\n"
		if otherControlPlanes {
			seed = " Until the other control planes have\n     rejoined, run it on the seed with --server https://127.0.0.1:6443.\n"
		}
		return "Controllers and operators you installed yourself (cert-manager, an ingress\n" +
			"     or GitOps controller, a database operator) hold watches too. The restore\n" +
			"     does not restart them. For each, restart it and check its pods:\n" +
			"       stratum kubectl -n <namespace> rollout restart deploy/<name>\n" +
			"     (ds/<name> or sts/<name> for a DaemonSet or StatefulSet; find them with\n" +
			"     stratum kubectl get deploy,ds,sts -A)." + seed
	}},
}

// CommandLines is commands, one per line, indented as the guidance is.
func CommandLines(commands []string) string {
	return "       " + strings.Join(commands, "\n       ")
}

// RepeatItem is the guidance's item about repeating the restore's restarts once
// every other control plane has rejoined, rendered from the set: the
// components it names are the set's host-side watchers, and the two command
// blocks are its per-control-plane and once-for-the-cluster commands.
func (set RestartSet) RepeatItem() string {
	watchers, _ := set.split()
	return "If the cluster has other control planes: once EVERY one has rejoined, repeat\n" +
		"     the restarts. " + englishList(labels(watchers)) + "\n" +
		"     may reach the API server through the VIP, which until then can lead to a\n" +
		"     control plane that still holds the old data. On each control plane (kubelet\n" +
		"     recreates the controller containers once they exit, and the supervisor\n" +
		"     restarts the operator):\n" +
		CommandLines(set.RepeatCommands()) + "\n" +
		"     and the in-cluster workloads once for the cluster:\n" +
		CommandLines(set.RolloutRestartCommands("stratum kubectl")) + "\n"
}

// StillToDo is the list of what the operator still has to do on the other nodes
// after the restore: the same list whether the restore restarted every watcher or
// not, and for `stratum snapshot restore`, the MCP tool and an upgrade rollback
// that restores etcd. otherControlPlanes is false for the rollback, whose
// single-member cluster has none to rejoin, and drops the two items about them.
func StillToDo(otherControlPlanes bool) string {
	var b strings.Builder
	b.WriteString("Still to do:\n")
	n := 0
	for _, it := range stillToDoItems {
		if it.otherControlPlanes && !otherControlPlanes {
			continue
		}
		n++
		fmt.Fprintf(&b, "  %d. %s", n, it.body(otherControlPlanes))
	}
	return b.String()
}

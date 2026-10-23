package upgrade

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// The record's host-bound parts, indirected for tests.
var (
	supervisorStarted           = staticpod.StartTicks
	unreplaceableSupervisorPath = func() string {
		return filepath.Join(version.DataDir, "upgrade", "unreplaceable-supervisor.json")
	}
)

// unreplaceableSupervisor records a supervisor the handoff found it cannot
// replace: it predates restarting itself in place, and systemd does not run
// it. Only asking it to restart tells that, and that process's answer cannot
// change, so a rerun at the target trusts the record instead of asking again.
// Without it --check answers "needs the upgrade" for the stale supervisor,
// and the cluster upgrade drains the node on every rerun for a handoff that
// only warns. Start, the process's start time, keeps a reused pid from
// inheriting the answer.
type unreplaceableSupervisor struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"`
}

// restartByHand says how to put a supervisor systemd does not run on the
// installed stratum.
func restartByHand(cur comps.RunningSupervisor) string {
	return fmt.Sprintf("it runs %s until it is restarted. Stop it (the containers keep running) and start it again "+
		"without the join flags: `sudo %s up --worker` on a worker, `sudo %s up --config <path>` (with the --profile "+
		"it was started with, if any) on a controller", cur.Exe, version.StratumInstallPath, version.StratumInstallPath)
}

// recordUnreplaceable records the supervisor at pid as one the handoff cannot
// replace. Best-effort: without the record a rerun only asks again, so a
// failure is a warning about that, not an error in an upgrade that is done.
func recordUnreplaceable(w io.Writer, pid int) {
	err := writeUnreplaceable(pid)
	if err != nil {
		fmt.Fprintf(w, "  WARN: could not record that the supervisor (pid %d) cannot be handed over (%v); until it is "+
			"restarted, `stratum upgrade --check` reports this node as needing the upgrade\n", pid, err)
	}
}

func writeUnreplaceable(pid int) error {
	start, alive, err := supervisorStarted(pid)
	if err != nil {
		return err
	}
	if !alive {
		return errors.New("it is no longer running")
	}
	data, err := json.Marshal(unreplaceableSupervisor{PID: pid, Start: start})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(unreplaceableSupervisorPath()), 0o700); err != nil {
		return err
	}
	return atomicfile.WriteFile(unreplaceableSupervisorPath(), data, 0o600)
}

// knownUnreplaceable reports whether cur is the supervisor process a handoff
// recorded as one it cannot replace. Anything it cannot read or match answers
// no, which leaves the answer as it was without the record: ask it again.
func knownUnreplaceable(cur comps.RunningSupervisor) bool {
	data, err := os.ReadFile(unreplaceableSupervisorPath())
	if err != nil {
		return false
	}
	var r unreplaceableSupervisor
	if err := json.Unmarshal(data, &r); err != nil || r.PID != cur.PID {
		return false
	}
	start, alive, err := supervisorStarted(cur.PID)
	return err == nil && alive && start == r.Start
}

// forgetUnreplaceable removes the record. Best-effort: one left behind names
// a process by pid and start time, so it can match nothing else.
func forgetUnreplaceable() {
	_ = os.Remove(unreplaceableSupervisorPath())
}

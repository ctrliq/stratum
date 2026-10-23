package upgrade

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// The supervisor handoff's host-bound steps, indirected for tests.
var (
	runningSupervisor   = comps.Supervisor
	reexecSupervisor    = func() error { return supervisor.DialReExec(version.ControlSocket) }
	serviceMainPID      = host.UnitMainPID
	serviceExecPath     = host.UnitExecPath
	restartService      = host.RestartUnit
	supervisorStatus    = comps.Status
	supervisorBackAfter = 3 * time.Minute
	supervisorPollEvery = 2 * time.Second
	supervisorSleep     = time.Sleep
)

// handOverSupervisor puts this node under a supervisor running the installed
// stratum. Replacing the binary on disk does not replace the running
// supervisor: it keeps running the previous build, and so do the arguments,
// paths and hooks it gives everything it starts, until it is restarted.
//
// A supervisor that supports it re-execs itself in place, keeping its pid. One
// that predates that is restarted through systemd when systemd is what runs
// it; otherwise it is left running, with a warning saying how to restart it,
// and recorded so a rerun at the target does not ask it again
// (unreplaceableSupervisor).
// It reports whether the supervisor was replaced, in which case every
// component it runs was restarted and the caller gates the node again. An
// error means the supervisor was asked to go and did not come back as it
// should.
func handOverSupervisor(w io.Writer) (bool, error) {
	cur, err := runningSupervisor()
	if err != nil {
		fmt.Fprintf(w, "  WARN: could not tell which stratum the supervisor runs (%v); if it is not %s, restart it to run "+
			"the installed build\n", err, version.StratumInstallPath)
		return false, nil
	}
	// A record of an earlier answer must not outlive a run that identified
	// the supervisor: it is current, or is asked again below.
	forgetUnreplaceable()
	if !cur.Stale() {
		return false, nil
	}
	k8sutil.Step(w, fmt.Sprintf("Restart the supervisor (pid %d, running %s) on the installed %s", cur.PID, cur.Exe,
		version.StratumInstallPath))
	err = reexecSupervisor()
	switch {
	case err == nil:
	case errors.Is(err, supervisor.ErrReExecUnsupported):
		main, merr := serviceMainPID()
		if merr != nil {
			return false, fmt.Errorf("the supervisor (pid %d) predates restarting itself in place, and systemd could "+
				"not say whether it runs it (%w); it runs %s until it is restarted: `sudo systemctl restart stratum` if "+
				"systemd runs it", cur.PID, merr, cur.Exe)
		}
		if main != cur.PID {
			fmt.Fprintf(w, "  WARN: the supervisor (pid %d) predates restarting itself in place, and systemd does not run it; %s\n",
				cur.PID, restartByHand(cur))
			recordUnreplaceable(w, cur.PID)
			return false, nil
		}
		path, perr := serviceExecPath()
		if perr != nil {
			return false, fmt.Errorf("read the program the stratum service starts before restarting it: %w", perr)
		}
		if path != version.StratumInstallPath {
			return false, fmt.Errorf("the supervisor (pid %d) predates restarting itself in place, and the stratum "+
				"service starts %s, not the installed %s, so restarting the service would start the previous build "+
				"again: point the unit's ExecStart at %s, run `sudo systemctl daemon-reload`, then `sudo systemctl "+
				"restart stratum`", cur.PID, path, version.StratumInstallPath, version.StratumInstallPath)
		}
		if err := restartService(); err != nil {
			return false, fmt.Errorf("restart the stratum service onto the installed build: %w", err)
		}
	default:
		return false, fmt.Errorf("ask the supervisor to restart on the installed build: %w", err)
	}
	return true, waitSupervisorBack()
}

// previewSupervisorHandoff says, for a dry run, whether the real run would end
// by restarting the supervisor on the installed stratum. restoresStratum is
// whether the real run first restores the stratum binary, as a rollback does:
// the supervisor then no longer runs the installed build, whatever it runs now.
// A restart the real run might not do is hedged with what it would do instead
// (handoffCaveat).
func previewSupervisorHandoff(w io.Writer, restoresStratum bool) {
	cur, err := runningSupervisor()
	switch {
	case restoresStratum && err == nil:
		fmt.Fprintf(w, "would restart the supervisor (pid %d) on the restored %s%s\n", cur.PID, version.StratumInstallPath,
			handoffCaveat(cur.PID))
	case restoresStratum:
		fmt.Fprintf(w, "would restart the supervisor on the restored %s if it can then tell which stratum the "+
			"supervisor runs, and it can restart itself in place or systemd runs it with a service that starts that "+
			"build (could not now: %v)\n",
			version.StratumInstallPath, err)
	case err != nil:
		fmt.Fprintf(w, "would restart the supervisor if it does not run the installed %s (could not tell: %v)\n",
			version.StratumInstallPath, err)
	case cur.Stale():
		fmt.Fprintf(w, "would restart the supervisor (pid %d, running %s) on the installed %s%s\n", cur.PID, cur.Exe,
			version.StratumInstallPath, handoffCaveat(cur.PID))
	}
}

// handoffCaveat qualifies a previewed restart of the supervisor at pid with
// what handOverSupervisor does when that supervisor predates restarting itself
// in place, which only asking it to restart would tell: one systemd does not
// run is left running, and the run ends with an error, keeping the installed
// build (handOverAndGate), when systemd cannot say whether it runs it, or
// does but its service would not start the installed build. It is empty when
// systemd runs it and would start the installed build, so the real run
// restarts it either way.
func handoffCaveat(pid int) string {
	const inPlace = " if it can restart itself in place"
	const fails = "; for one that predates that, the run ends with an error, keeping the installed build, "
	main, err := serviceMainPID()
	switch {
	case err != nil:
		return fmt.Sprintf("%s%sif systemd still cannot say whether it runs it (%v)", inPlace, fails, err)
	case main != pid:
		return inPlace + "; one that predates that is left running, with a warning saying how to restart it, since " +
			"systemd does not run it"
	}
	path, err := serviceExecPath()
	switch {
	case err != nil:
		return fmt.Sprintf("%s%sif the program the stratum service starts still cannot be read (%v)", inPlace, fails, err)
	case path != version.StratumInstallPath:
		return fmt.Sprintf("%s%ssince the stratum service starts %s, not the installed %s", inPlace, fails, path,
			version.StratumInstallPath)
	}
	return ""
}

// waitSupervisorBack waits for a supervisor running the installed stratum to
// answer with every component it wants running.
func waitSupervisorBack() error {
	deadline := time.Now().Add(supervisorBackAfter)
	for {
		state, done := supervisorBack()
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the supervisor did not come back on %s with every component running within %v (%s)",
				version.StratumInstallPath, supervisorBackAfter, state)
		}
		supervisorSleep(supervisorPollEvery)
	}
}

func supervisorBack() (string, bool) {
	cur, err := runningSupervisor()
	if err != nil {
		return err.Error(), false
	}
	if cur.Stale() {
		return fmt.Sprintf("pid %d still runs %s", cur.PID, cur.Exe), false
	}
	procs, err := supervisorStatus()
	if err != nil {
		return err.Error(), false
	}
	for name, p := range procs {
		if p.Wanted && (!p.Running || p.Foreign) {
			return name + " is not running yet", false
		}
	}
	return "", true
}

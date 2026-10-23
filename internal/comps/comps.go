// Package comps speaks systemctl-shaped verbs (restart, stop, is-active) to
// the stratum supervisor's control socket, for the components that are still
// supervised HOST processes: containerd, kubelet, and the operator.
//
// The control plane is not a host process here: it runs as
// kubelet-managed static pods, and internal/staticpod owns its lifecycle
// (manifest re-render to restart, manifest removal to stop).
package comps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ctrliq/stratum/internal/procid"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// Units is the canonical component roster in start/restart order — the
// supervisor equivalent of a systemd unit list. Upgrade and
// uninstall derive their lists from this so adding a component is a one-place
// change. Not every node runs every entry (workers have no control plane;
// isolated controllers have no kubelet); callers filter by IsActive.
var Units = []string{
	"containerd",
	"kubelet",
	"operator",
}

// Normalize maps a legacy unit name (stratum-kubelet) or an alias onto the
// supervisor's component name (kubelet). Unknown names pass through.
func Normalize(unit string) string {
	return strings.TrimSuffix(strings.TrimPrefix(unit, "stratum-"), ".service")
}

func dial(op, name string) error {
	_, err := supervisor.Dial(version.ControlSocket, op, Normalize(name))
	return err
}

// Restart bounces a component and waits until it is RUNNING AGAIN — systemctl
// semantics, which the ported call sites assume. "all" keeps the
// fire-and-forget behaviour.
func Restart(name string) error {
	if Normalize(name) == "all" {
		return dial("restart", name)
	}
	before, err := checkedPID(name)
	if err != nil {
		return fmt.Errorf("status before restart %s: %w", Normalize(name), err)
	}
	if err := dial("restart", name); err != nil {
		return err
	}
	return waitForReplacement(name, before, "restart", 90*time.Second)
}

func waitForReplacement(name string, before int, operation string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pid := pidOf(name); pid != 0 && pid != before {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s did not come back within %s of %s", Normalize(name), timeout, operation)
}

// find looks a component up in the supervisor's roster. ok is false when the
// supervisor manages no such component — or when there is no supervisor to ask,
// which for every caller here means the same thing: not running on this node.
func find(name string) (supervisor.ProcStatus, bool) {
	p, err := lookup(name)
	return p, err == nil
}

func lookup(name string) (supervisor.ProcStatus, error) {
	return lookupContext(context.Background(), name)
}

func lookupContext(ctx context.Context, name string) (supervisor.ProcStatus, error) {
	procs, err := supervisor.DialContext(ctx, version.ControlSocket, "status", "")
	if err != nil {
		return supervisor.ProcStatus{}, err
	}
	want := Normalize(name)
	for _, p := range procs {
		if p.Name == want {
			return p, nil
		}
	}
	return supervisor.ProcStatus{}, fmt.Errorf("no supervised component %q", want)
}

func pidOf(name string) int {
	pid, _ := checkedPID(name)
	return pid
}

// A failed baseline query must not be mistaken for a stopped component: zero
// would let the still-running old PID satisfy the replacement check.
func checkedPID(name string) (int, error) {
	p, err := lookup(name)
	if err != nil {
		return 0, err
	}
	if p.Running {
		return p.Pid, nil
	}
	return 0, nil
}

// Stop takes a component down and keeps it down until Start.
func Stop(name string) error { return dial("stop", name) }

// EnableNow starts a component (enablement is the supervisor's default state,
// so this is just start — the name survives from the systemd call sites).
func EnableNow(name string) error { return dial("start", name) }

// Reload updates a supervised component's exec definition (path/args/env) in the
// running supervisor and waits for a new running PID, just like Restart.
// In-place `stratum upgrade` uses this so a
// new binary's changed args — e.g. containerd's --config path — take
// effect immediately instead of the supervisor keeping the old argv until the
// next full `stratum up`.
func Reload(name string, spec supervisor.ProcSpec) error {
	before, err := checkedPID(name)
	if err != nil {
		return fmt.Errorf("status before reload %s: %w", Normalize(name), err)
	}
	if err := supervisor.DialReload(version.ControlSocket, Normalize(name), spec); err != nil {
		return err
	}
	return waitForReplacement(name, before, "reload", 90*time.Second)
}

// StopAllIfPresent disables every roster entry, including pending starts.
// Missing/refusing sockets require independent process checks; other errors
// must not be interpreted as successful shutdown.
func StopAllIfPresent() error {
	err := dial("stop", "all")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil
	}
	return err
}

// StartAndWaitActive starts a component and polls until it is running.
func StartAndWaitActive(name string, timeout time.Duration) error {
	if err := dial("start", name); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if IsActive(name) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become active within %v", Normalize(name), timeout)
}

// IsActive reports whether the component is currently running under the
// supervisor. No socket (controller not running) means not active, and neither
// does a component whose socket another process serves.
func IsActive(name string) bool { return IsActiveContext(context.Background(), name) }

// IsActiveContext is IsActive with the supervisor request bounded by ctx.
func IsActiveContext(ctx context.Context, name string) bool {
	p, err := lookupContext(ctx, name)
	return err == nil && p.Running && !p.Foreign
}

// Status is the supervisor's roster as one snapshot, by component name. An
// error means the supervisor could not be asked, never that nothing runs:
// callers deciding what is safe to stop fail closed on it.
func Status() (map[string]supervisor.ProcStatus, error) {
	procs, err := supervisor.Dial(version.ControlSocket, "status", "")
	if err != nil {
		return nil, err
	}
	st := make(map[string]supervisor.ProcStatus, len(procs))
	for _, p := range procs {
		st[p.Name] = p
	}
	return st, nil
}

// UnitExists reports whether this node's supervisor manages the component at
// all (workers have no operator entry).
func UnitExists(name string) bool {
	_, ok := find(name)
	return ok
}

// SupervisorRunning reports whether a stratum supervisor is serving the control
// socket on this node at all.
func SupervisorRunning() bool {
	_, err := supervisor.Dial(version.ControlSocket, "status", "")
	return err == nil
}

// ProcessRunning reports whether a Stratum-managed host process is still live,
// even if its supervisor and control socket have died. It deliberately matches
// the payload paths for containerd and kubelet so a distro-owned process with
// the same basename does not block uninstall. The operator is recognized only
// as the stratum binary's explicit subcommand. A probe error means liveness is
// unknown; callers that would delete the process's state must refuse to act.
func ProcessRunning(name string) (bool, error) {
	pids, err := processPIDs("/proc", name, 1)
	return len(pids) > 0, err
}

// ProcessPIDs returns the PIDs of every live Stratum-managed process that
// ProcessRunning would match for name.
func ProcessPIDs(name string) ([]int, error) { return processPIDs("/proc", name, 0) }

// processPIDs scans procRoot for name, stopping after max matches when
// max > 0. The walk and the exited-process policy are procid's; this only
// applies name's predicate and collects PIDs.
func processPIDs(procRoot, name string, max int) ([]int, error) {
	var pids []int
	err := procid.Walk(procRoot, func(p procid.Process) (bool, error) {
		if isStratumProcess(name, p) {
			pids = append(pids, p.PID)
			if max > 0 && len(pids) >= max {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return pids, nil
}

// processPredicates is the identity table: one predicate per name uninstall
// and the liveness probes may ask about, keyed by the supervisor's component
// name. Every comps.Units entry must have one — checked in init — because a
// roster unit without a predicate would answer false for every process and
// uninstall would leave it running once the supervisor has died. The
// top-level stratum name and runtime-shim are not roster units, but uninstall
// probes both after the supervisor is gone, so they hold predicates here too;
// their boundaries are procid's tests.
var processPredicates = map[string]func(procid.Process) bool{
	"containerd":   func(p procid.Process) bool { return procid.HasPayloadPath("containerd", p) },
	"kubelet":      func(p procid.Process) bool { return procid.HasPayloadPath("kubelet", p) },
	"operator":     procid.IsOperator,
	"stratum":      procid.IsStratumTopLevel,
	"runtime-shim": procid.IsRuntimeShim,
}

func init() {
	for _, unit := range Units {
		if _, ok := processPredicates[Normalize(unit)]; !ok {
			panic(fmt.Sprintf("comps: roster unit %q has no process predicate; isStratumProcess would answer false for every process and uninstall would leave it running once the supervisor has died", unit))
		}
	}
}

// isStratumProcess answers the uninstall probe: is the named roster unit the
// process running here? Each predicate lives in procid beside the /proc walk
// both it and the supervisor share.
func isStratumProcess(name string, p procid.Process) bool {
	if pred, ok := processPredicates[Normalize(name)]; ok {
		return pred(p)
	}
	return false
}

// OperatorCommandLinePattern is procid.IsOperator as an extended regular
// expression over a process's whole command line, for `pkill -f`, which can match
// nothing else. The executable is any single word, so a release artifact name
// like stratum-linux-amd64 matches as the supervisor accepts it; what makes it
// the operator is argv[1] and the Stratum-owned flags, built from the same
// constants. Those flags are matched in the order cluster.Processes assembles
// them, where procid.IsOperator accepts any order: TestOperatorPatternAgrees
// pins the two together, and a cluster test pins the pattern to the real argv.
func OperatorCommandLinePattern() string {
	q := regexp.QuoteMeta
	const any = "( [^[:space:]]+)*"
	return "^[^[:space:]]+ operator" + any +
		" --kubeconfig (" + q(version.SuperAdminConf()) + "|" + q(version.AdminConf) + ")" + any +
		" --manifests-dir " + q(version.ManifestsDir) + any +
		" --helm-cache " + q(version.HelmCacheDir) + any +
		" --leader-elect([[:space:]]|$)"
}

// procExe is a seam over reading /proc/<pid>/exe, so the supervisor-build
// check is testable without a real process tree.
var procExe = func(pid int) (string, error) {
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
}

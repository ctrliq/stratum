// Package comps drives the host processes; the control plane is static pods in internal/staticpod.
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

// In start/restart order, which upgrade and uninstall reuse; not every node runs every entry.
var Units = []string{
	"containerd",
	"kubelet",
	"operator",
}

func Normalize(unit string) string {
	return strings.TrimSuffix(strings.TrimPrefix(unit, "stratum-"), ".service")
}

func dial(op, name string) error {
	_, err := supervisor.Dial(version.ControlSocket, op, Normalize(name))
	return err
}

// Return once the replacement runs, as the ported call sites assume; "all" stays fire-and-forget.
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

// ok is false too when there is no supervisor to ask: not running on this node.
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

// A failed baseline must not read as stopped: zero would satisfy the PID check.
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

func Stop(name string) error { return dial("stop", name) }

// The name survives from the systemd call sites; the supervisor starts by default.
func EnableNow(name string) error { return dial("start", name) }

// Used by in-place upgrade so changed args take effect without a full restart.
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

// A missing or refused socket is not a failed shutdown; other errors are real.
func StopAllIfPresent() error {
	err := dial("stop", "all")
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil
	}
	return err
}

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

// No socket or a foreign socket owner means not active.
func IsActive(name string) bool { return IsActiveContext(context.Background(), name) }

func IsActiveContext(ctx context.Context, name string) bool {
	p, err := lookupContext(ctx, name)
	return err == nil && p.Running && !p.Foreign
}

// An error means the supervisor could not be asked, never that nothing runs.
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

func UnitExists(name string) bool {
	_, ok := find(name)
	return ok
}

func SupervisorRunning() bool {
	_, err := supervisor.Dial(version.ControlSocket, "status", "")
	return err == nil
}

// Error means liveness unknown: refuse to delete state; payload-path match ignores a distro twin.
func ProcessRunning(name string) (bool, error) {
	pids, err := processPIDs("/proc", name, 1)
	return len(pids) > 0, err
}

func ProcessPIDs(name string) ([]int, error) { return processPIDs("/proc", name, 0) }

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

// Uninstall probes stratum and runtime-shim after the supervisor dies, so they are here too.
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

func isStratumProcess(name string, p procid.Process) bool {
	if pred, ok := processPredicates[Normalize(name)]; ok {
		return pred(p)
	}
	return false
}

// Flags are matched in cluster.Processes' order, which IsOperator does not require.
func OperatorCommandLinePattern() string {
	q := regexp.QuoteMeta
	const any = "( [^[:space:]]+)*"
	return "^[^[:space:]]+ operator" + any +
		" --kubeconfig (" + q(version.SuperAdminConf()) + "|" + q(version.AdminConf) + ")" + any +
		" --manifests-dir " + q(version.ManifestsDir) + any +
		" --helm-cache " + q(version.HelmCacheDir) + any +
		" --leader-elect([[:space:]]|$)"
}

// A seam so the supervisor-build check is testable without a real process tree.
var procExe = func(pid int) (string, error) {
	return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
}

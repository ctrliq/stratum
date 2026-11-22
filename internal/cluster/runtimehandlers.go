package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"strings"

	"github.com/ctrliq/stratum/internal/apparmor"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/runtime"
)

// RuntimeApplyOptions is `stratum runtime apply`'s request.
type RuntimeApplyOptions struct {
	Handlers map[string]string
	Clear    bool
	Restart  bool
}

// RuntimeApplyResult is what ApplyRuntimeHandlers applied.
type RuntimeApplyResult struct {
	runtime.ApplyHandlersResult
	Handlers map[string]string
	Source   string
	Saved    bool // node.yaml changed
}

var (
	applyHandlers         = runtime.ApplyHandlers
	requireHandlersImport = runtime.RequireHandlersImport
	requireSharedConfD    = runtime.RequireSharedConfDParses
	requireOwnDropIn      = runtime.RequireOwnDropIn
	restartContainerd     = runtime.RestartContainerd
)

// ApplyRuntimeHandlers saves node.yaml only once the drop-in lands; the lock spans read to relabel.
func ApplyRuntimeHandlers(w io.Writer, o RuntimeApplyOptions) (RuntimeApplyResult, error) {
	if o.Clear && o.Handlers != nil {
		return RuntimeApplyResult{}, errors.New("--clear and --runtime-handler cannot be combined")
	}
	release, err := nodelock.Acquire("runtime apply")
	if err != nil {
		return RuntimeApplyResult{}, err
	}
	defer release()
	st, err := LoadState()
	if err != nil {
		return RuntimeApplyResult{}, fmt.Errorf("read the node state (has this node run `stratum up`?): %w", err)
	}
	want, source, err := declaredRuntimeHandlers(st, o, w)
	if err != nil {
		return RuntimeApplyResult{}, err
	}
	node := NodeSpec{RuntimeHandlers: want}
	if err := node.Validate(); err != nil {
		return RuntimeApplyResult{}, err
	}
	if err := node.CheckBinaries(); err != nil {
		return RuntimeApplyResult{}, fmt.Errorf("%w; install it and run `stratum runtime apply` again", err)
	}
	if err := requireHandlersImport(); err != nil {
		return RuntimeApplyResult{}, err
	}
	if err := requireSharedConfD(); err != nil {
		return RuntimeApplyResult{}, err
	}
	if err := requireOwnDropIn(want); err != nil {
		return RuntimeApplyResult{}, err
	}
	out := RuntimeApplyResult{Handlers: want, Source: source}
	res, err := applyHandlers(w, want, o.Restart)
	out.ApplyHandlersResult = res
	if err != nil {
		if res.Written && !maps.Equal(st.RuntimeHandlers, want) {
			if serr := nodestate.Mutate(func(cur *NodeState) error { cur.RuntimeHandlers = want; return nil }); serr != nil {
				return out, fmt.Errorf("%w; saving the runtime handlers the drop-in now declares failed too: %v", err, serr)
			}
			out.Saved = true
		}
		return out, err
	}
	if !maps.Equal(st.RuntimeHandlers, want) {
		if err := nodestate.Mutate(func(cur *NodeState) error { cur.RuntimeHandlers = want; return nil }); err != nil {
			return out, fmt.Errorf("save runtime handlers: %w; the drop-in declares them, and the next start writes it again from node.yaml", err)
		}
		out.Saved = true
	}
	applied := *st
	applied.RuntimeHandlers = want
	if err := reconcileNodeLabels(context.Background(), &applied); err != nil {
		fmt.Fprintf(w, "WARN: could not update this node's runtime-handler labels (%v); RuntimeClass pods may not schedule here until its next start\n", err)
	}
	return out, nil
}

// RestartRuntime restarts containerd, then relabels for the loaded handlers; holds the node lock.
func RestartRuntime(w io.Writer) error {
	release, err := nodelock.Acquire("runtime restart")
	if err != nil {
		return err
	}
	defer release()
	if err := requireSharedConfD(); err != nil {
		return err
	}
	if err := restartContainerd(w); err != nil {
		return err
	}
	st, err := LoadState()
	if err != nil {
		fmt.Fprintf(w, "WARN: could not read the node state (%v); this node's runtime-handler labels update at its next start\n", err)
		return nil
	}
	if err := reconcileNodeLabels(context.Background(), st); err != nil {
		fmt.Fprintf(w, "WARN: could not update this node's runtime-handler labels (%v); RuntimeClass pods may not schedule here until its next start\n", err)
	}
	return nil
}

// declaredRuntimeHandlers refuses a recorded config that is gone, not reads it as no handlers.
func declaredRuntimeHandlers(st *NodeState, o RuntimeApplyOptions, w io.Writer) (map[string]string, string, error) {
	if st.Role == "worker" {
		switch {
		case o.Clear:
			return nil, "--clear", nil
		case o.Handlers != nil:
			return o.Handlers, "--runtime-handler", nil
		}
		return st.RuntimeHandlers, "the handlers this worker saved", nil
	}
	path, _ := activeConfigPath(func() (*NodeState, error) { return st, nil }, w)
	if o.Clear || o.Handlers != nil {
		return nil, "", fmt.Errorf("a control plane takes its runtime handlers from spec.node.runtimeHandlers in %s at every start; edit them there and run `stratum runtime apply` with no flags", path)
	}
	if path != DefaultConfigPath {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("%s, the config file this control plane was started with, no longer exists; restore it, "+
				"or start this node again with `stratum up --config` naming the intended one. Nothing was changed", path)
		}
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, "", err
	}
	return cfg.Spec.Node.RuntimeHandlers, "spec.node.runtimeHandlers in " + path, nil
}

// vars so a test can answer for the host's AppArmor.
var (
	checkAppArmor   = apparmor.Check
	stackedAppArmor = apparmor.Stacked
)

func stackedMessage(procs []apparmor.StackedProcess) string {
	const listed = 5
	var names []string
	for _, p := range procs[:min(len(procs), listed)] {
		names = append(names, fmt.Sprintf("%s (pid %d, %s)", p.Comm, p.PID, p.Label))
	}
	if len(procs) > listed {
		names = append(names, fmt.Sprintf("and %d more", len(procs)-listed))
	}
	return fmt.Sprintf("%d container processes run with an AppArmor label stacked on unconfined, so they cannot signal their own processes: %s; "+
		"those runc started before it ran under the runc profile keep that label until their pods are recreated, "+
		"and rebooting the node recreates every one, the control plane's static pods included; "+
		"a pod whose runtime handler runs its own OCI runtime is stacked again unless /etc/apparmor.d/runc also attaches to that runtime", len(procs), strings.Join(names, ", "))
}

// CheckRuntimeHandlers reports this node's handlers, the AppArmor profile they need, and drift.
func CheckRuntimeHandlers(w io.Writer) (runtime.HandlerReport, error) {
	st, err := LoadState()
	if err != nil {
		return runtime.HandlerReport{}, fmt.Errorf("read the node state (has this node run `stratum up`?): %w", err)
	}
	r := runtime.CheckHandlers(st.RuntimeHandlers)
	switch err := checkAppArmor(); {
	case err == nil:
	case errors.Is(err, apparmor.ErrNotCovered):
		r.Findings = append(r.Findings, runtime.HandlerFinding{Severity: runtime.SeverityError,
			Message: fmt.Sprintf("%v; a running stratum loads the profile again within a minute, and `journalctl -u stratum` says why when it cannot", err)})
	case errors.Is(err, apparmor.ErrHostProfileNotWidened):
		r.Findings = append(r.Findings, runtime.HandlerFinding{Severity: runtime.SeverityWarn, Message: err.Error()})
	default:
		r.Findings = append(r.Findings, runtime.HandlerFinding{Severity: runtime.SeverityWarn,
			Message: fmt.Sprintf("cannot check the AppArmor runc profile: %v", err)})
	}
	switch procs, err := stackedAppArmor(); {
	case err != nil:
		r.Findings = append(r.Findings, runtime.HandlerFinding{Severity: runtime.SeverityWarn,
			Message: fmt.Sprintf("cannot list the processes whose AppArmor label is stacked: %v", err)})
	case len(procs) > 0:
		r.Findings = append(r.Findings, runtime.HandlerFinding{Severity: runtime.SeverityError,
			Message: stackedMessage(procs)})
	}
	if st.Role == "worker" {
		return r, nil
	}
	path, _ := activeConfigPath(func() (*NodeState, error) { return st, nil }, w)
	cfg, err := LoadConfig(path)
	switch {
	case err != nil:
		r.Findings = append(r.Findings, runtime.HandlerFinding{Severity: runtime.SeverityWarn,
			Message: fmt.Sprintf("cannot read %s to compare its spec.node.runtimeHandlers: %v", path, err)})
	case !maps.Equal(cfg.Spec.Node.RuntimeHandlers, st.RuntimeHandlers):
		r.Findings = append(r.Findings, runtime.HandlerFinding{Severity: runtime.SeverityError,
			Message: fmt.Sprintf("spec.node.runtimeHandlers in %s differ from the handlers this node saved; run `stratum runtime apply`", path)})
	}
	return r, nil
}

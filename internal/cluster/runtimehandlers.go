package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/runtime"
)

// RuntimeApplyOptions is `stratum runtime apply`'s request. Handlers, when
// non-nil, replaces a worker's saved handlers; Clear removes them all. A
// control plane takes neither: its handlers are its config file's. Restart
// restarts a running containerd when the drop-in changed.
type RuntimeApplyOptions struct {
	Handlers map[string]string
	Clear    bool
	Restart  bool
}

// RuntimeApplyResult is what ApplyRuntimeHandlers applied: the handlers the
// node now declares, where they came from, and the drop-in's outcome.
type RuntimeApplyResult struct {
	runtime.ApplyHandlersResult
	Handlers map[string]string
	Source   string
	Saved    bool // node.yaml changed
}

var (
	applyHandlers         = runtime.ApplyHandlers
	requireHandlersImport = runtime.RequireHandlersImport
	restartContainerd     = runtime.RestartContainerd
)

// ApplyRuntimeHandlers resolves the runtime handlers this node declares,
// saves them to node.yaml when they changed, and writes containerd's drop-in.
// A worker's are its saved handlers, replaced by o.Handlers or cleared by
// o.Clear. A control plane rebuilds its handlers from spec.node.runtimeHandlers
// at every `stratum up`, so it re-reads the config file it was started with
// and refuses o.Handlers and o.Clear: saving them to node.yaml alone would be
// undone by its next start. Every handler is validated, its binary checked,
// and containerd's config checked to import the drop-in before anything is
// written. The Node's runtime-handler labels follow for the handlers its
// containerd has loaded, so a RuntimeClass schedules onto the node without
// waiting for its next start but not before containerd can run the handler;
// a label update that fails only warns.
func ApplyRuntimeHandlers(w io.Writer, o RuntimeApplyOptions) (RuntimeApplyResult, error) {
	if o.Clear && o.Handlers != nil {
		return RuntimeApplyResult{}, errors.New("--clear and --runtime-handler cannot be combined")
	}
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
	out := RuntimeApplyResult{Handlers: want, Source: source}
	if !maps.Equal(st.RuntimeHandlers, want) {
		if err := nodestate.Mutate(func(cur *NodeState) error { cur.RuntimeHandlers = want; return nil }); err != nil {
			return out, fmt.Errorf("save runtime handlers: %w", err)
		}
		out.Saved = true
	}
	res, err := applyHandlers(w, want, o.Restart)
	out.ApplyHandlersResult = res
	if err != nil {
		return out, err
	}
	if err := reconcileNodeLabels(context.Background(), st); err != nil {
		fmt.Fprintf(w, "WARN: could not update this node's runtime-handler labels (%v); RuntimeClass pods may not schedule here until its next start\n", err)
	}
	return out, nil
}

// RestartRuntime restarts the supervisor's containerd, then labels this node
// for the runtime handlers the restart loaded. A label update that fails only
// warns.
func RestartRuntime(w io.Writer) error {
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

// declaredRuntimeHandlers is the handler set ApplyRuntimeHandlers applies and
// a description of its source.
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
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, "", err
	}
	return cfg.Spec.Node.RuntimeHandlers, "spec.node.runtimeHandlers in " + path, nil
}

// CheckRuntimeHandlers reports on this node's runtime handlers. On a control
// plane it also reports a config file whose spec.node.runtimeHandlers differ
// from what node.yaml saved, which the next `stratum up` or `stratum runtime
// apply` would write.
func CheckRuntimeHandlers(w io.Writer) (runtime.HandlerReport, error) {
	st, err := LoadState()
	if err != nil {
		return runtime.HandlerReport{}, fmt.Errorf("read the node state (has this node run `stratum up`?): %w", err)
	}
	r := runtime.CheckHandlers(st.RuntimeHandlers)
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

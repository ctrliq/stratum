package cluster

import (
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/runtime"
)

// runtimeApplyHost saves st as this node's state on a scratch host and stubs
// the drop-in write, returning the handler sets the stub was asked to write.
func runtimeApplyHost(t *testing.T, st *NodeState) *[]map[string]string {
	t.Helper()
	workerScratchHost(t)
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	var written []map[string]string
	old, oldLabels, oldImport := applyHandlers, reconcileHandlerLabels, requireHandlersImport
	t.Cleanup(func() { applyHandlers, reconcileHandlerLabels, requireHandlersImport = old, oldLabels, oldImport })
	requireHandlersImport = func() error { return nil }
	reconcileHandlerLabels = func(*NodeState, map[string]string) error { return nil }
	applyHandlers = func(_ io.Writer, h map[string]string, _ bool) (runtime.ApplyHandlersResult, error) {
		written = append(written, h)
		return runtime.ApplyHandlersResult{Changed: true}, nil
	}
	return &written
}

func savedHandlers(t *testing.T) map[string]string {
	t.Helper()
	st, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	return st.RuntimeHandlers
}

func TestApplyRuntimeHandlersOnAWorker(t *testing.T) {
	saved := map[string]string{"gvisor": "/opt/runsc"}
	t.Run("no_flags_refuses_a_saved_handler_whose_binary_is_missing", func(t *testing.T) {
		written := runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: saved})
		res, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{})
		if err == nil {
			t.Fatalf("applied %+v though the saved handler's binary is missing", res)
		}
		if len(*written) != 0 {
			t.Errorf("the drop-in was written for a handler whose binary is missing")
		}
	})
	t.Run("flags_replace_and_save", func(t *testing.T) {
		written := runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: saved})
		want := map[string]string{"nvidia": executable(t)}
		res, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{Handlers: want})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Saved || !maps.Equal(savedHandlers(t), want) {
			t.Errorf("saved %v (Saved=%v), want %v", savedHandlers(t), res.Saved, want)
		}
		if len(*written) != 1 || !maps.Equal((*written)[0], want) {
			t.Errorf("wrote %v, want %v once", *written, want)
		}
	})
	t.Run("clear_removes_every_handler", func(t *testing.T) {
		written := runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: saved})
		res, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{Clear: true})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Saved || len(savedHandlers(t)) != 0 || len(*written) != 1 || len((*written)[0]) != 0 {
			t.Errorf("after --clear saved %v and wrote %v, want none of either", savedHandlers(t), *written)
		}
	})
	t.Run("unchanged_handlers_are_not_saved_again", func(t *testing.T) {
		h := map[string]string{"nvidia": executable(t)}
		written := runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: h})
		res, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Saved || len(*written) != 1 {
			t.Errorf("an unchanged apply saved=%v and wrote %d times, want no save and one write", res.Saved, len(*written))
		}
	})
	for name, o := range map[string]RuntimeApplyOptions{
		"invalid_name":       {Handlers: map[string]string{"runc": "/usr/bin/runc"}},
		"relative_path":      {Handlers: map[string]string{"nvidia": "bin/nvidia"}},
		"missing_binary":     {Handlers: map[string]string{"nvidia": "/nonexistent/nvidia-container-runtime"}},
		"clear_and_handlers": {Clear: true, Handlers: map[string]string{"nvidia": "/usr/bin/nvidia"}},
	} {
		t.Run("refuses_"+name+"_before_writing", func(t *testing.T) {
			written := runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: saved})
			if _, err := ApplyRuntimeHandlers(io.Discard, o); err == nil {
				t.Fatal("applied a refused request")
			}
			if !maps.Equal(savedHandlers(t), saved) || len(*written) != 0 {
				t.Errorf("a refused request saved %v and wrote %v", savedHandlers(t), *written)
			}
		})
	}
}

// writeControlPlaneConfig writes a cluster config declaring handlers and
// records it as the config the controller was started with.
func writeControlPlaneConfig(t *testing.T, handlers map[string]string) (*NodeState, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	var b strings.Builder
	b.WriteString("spec:\n  node:\n    runtimeHandlers:\n")
	for name, p := range handlers {
		b.WriteString("      " + name + ": " + p + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return &NodeState{Role: "controller", Hostname: "cp1", ConfigPath: path}, path
}

// A control plane rebuilds its handlers from its config file at every start,
// so apply re-reads that file and refuses flags that node.yaml alone would
// hold only until the next `stratum up`.
func TestApplyRuntimeHandlersOnAControlPlane(t *testing.T) {
	want := map[string]string{"nvidia": executable(t)}
	t.Run("reads_the_recorded_config", func(t *testing.T) {
		st, path := writeControlPlaneConfig(t, want)
		written := runtimeApplyHost(t, st)
		res, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(savedHandlers(t), want) || len(*written) != 1 || !maps.Equal((*written)[0], want) {
			t.Errorf("saved %v and wrote %v, want %v from %s", savedHandlers(t), *written, want, path)
		}
		if !strings.Contains(res.Source, path) {
			t.Errorf("source %q does not name %s", res.Source, path)
		}
	})
	for name, o := range map[string]RuntimeApplyOptions{
		"handlers": {Handlers: want},
		"clear":    {Clear: true},
	} {
		t.Run("refuses_"+name, func(t *testing.T) {
			st, path := writeControlPlaneConfig(t, want)
			written := runtimeApplyHost(t, st)
			_, err := ApplyRuntimeHandlers(io.Discard, o)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("a control plane applied %+v (%v), want a refusal naming %s", o, err, path)
			}
			if len(savedHandlers(t)) != 0 || len(*written) != 0 {
				t.Errorf("a refused request saved %v and wrote %v", savedHandlers(t), *written)
			}
		})
	}
}

func TestCheckRuntimeHandlersReportsAControlPlaneConfigDrift(t *testing.T) {
	st, path := writeControlPlaneConfig(t, map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime"})
	runtimeApplyHost(t, st)
	r, err := CheckRuntimeHandlers(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var errs []string
	for _, f := range r.Findings {
		if f.Severity == runtime.SeverityError {
			errs = append(errs, f.Message)
		}
	}
	if !strings.Contains(strings.Join(errs, "\n"), "spec.node.runtimeHandlers in "+path+" differ") {
		t.Errorf("errors %q do not report the config file's drift", errs)
	}
}

// apply relabels the Node with the handlers it applied, so a RuntimeClass
// schedules onto it now; an API server it cannot reach only warns.
func TestApplyRuntimeHandlersRelabelsTheNode(t *testing.T) {
	runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1", NodeLabels: map[string]string{"zone": "a"}})
	want := map[string]string{"nvidia": executable(t)}
	var labelled []map[string]string
	reconcileHandlerLabels = func(st *NodeState, h map[string]string) error {
		if st.Hostname != "w1" || st.NodeLabels["zone"] != "a" {
			t.Errorf("relabelled with %+v, want the node's own state", st)
		}
		labelled = append(labelled, h)
		return errors.New("apiserver unreachable")
	}
	var w strings.Builder
	if _, err := ApplyRuntimeHandlers(&w, RuntimeApplyOptions{Handlers: want}); err != nil {
		t.Fatalf("an unreachable API server failed the apply: %v", err)
	}
	if len(labelled) != 1 || !maps.Equal(labelled[0], want) {
		t.Errorf("relabelled with %v, want %v once", labelled, want)
	}
	if !strings.Contains(w.String(), "could not update this node's runtime handler labels") {
		t.Errorf("progress %q does not warn about the labels", w.String())
	}
}

// A containerd config that does not import the drop-in refuses the apply
// before node.yaml changes, so the refusal leaves the node as it was.
func TestApplyRuntimeHandlersRefusesAConfigWithoutTheImportBeforeSaving(t *testing.T) {
	written := runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1"})
	requireHandlersImport = func() error { return errors.New("containerd.toml does not import the drop-in") }
	if _, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{Handlers: map[string]string{"nvidia": executable(t)}}); err == nil {
		t.Fatal("applied against a config that does not import the drop-in")
	}
	if len(savedHandlers(t)) != 0 || len(*written) != 0 {
		t.Errorf("a refused apply saved %v and wrote %v", savedHandlers(t), *written)
	}
}

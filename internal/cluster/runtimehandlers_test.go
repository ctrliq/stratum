package cluster

import (
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/version"
)

// runtimeApplyHost saves st as this node's state on a scratch host and stubs
// the drop-in write, returning the handler sets the stub was asked to write.
func runtimeApplyHost(t *testing.T, st *NodeState) *[]map[string]string {
	t.Helper()
	workerScratchHost(t)
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	var written []map[string]string
	old, oldLabels, oldImport := applyHandlers, reconcileOwnNodeLabels, requireHandlersImport
	t.Cleanup(func() { applyHandlers, reconcileOwnNodeLabels, requireHandlersImport = old, oldLabels, oldImport })
	requireHandlersImport = func() error { return nil }
	reconcileOwnNodeLabels = func(context.Context, string, map[string]string, []string) error { return nil }
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
		if msg := err.Error(); !strings.Contains(msg, "run `stratum runtime apply` again") || strings.Contains(msg, "joins") {
			t.Errorf("refusal %q, want the apply hint and not the join one", msg)
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
	t.Run("refuses_a_recorded_config_that_is_gone", func(t *testing.T) {
		st, path := writeControlPlaneConfig(t, want)
		st.RuntimeHandlers = want
		written := runtimeApplyHost(t, st)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{})
		if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "no longer exists") {
			t.Fatalf("apply with the recorded config gone = %v, want a refusal naming %s", err, path)
		}
		if !maps.Equal(savedHandlers(t), want) || len(*written) != 0 {
			t.Errorf("a refused apply saved %v and wrote %v, want the saved handlers kept and nothing written", savedHandlers(t), *written)
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
	var labelled [][]string
	reconcileOwnNodeLabels = func(_ context.Context, host string, labels map[string]string, handlers []string) error {
		if host != "w1" || labels["zone"] != "a" {
			t.Errorf("relabelled %s with %v, want the node's own state", host, labels)
		}
		labelled = append(labelled, handlers)
		return errors.New("apiserver unreachable")
	}
	var w strings.Builder
	if _, err := ApplyRuntimeHandlers(&w, RuntimeApplyOptions{Handlers: want}); err != nil {
		t.Fatalf("an unreachable API server failed the apply: %v", err)
	}
	if len(labelled) != 1 || !slices.Equal(labelled[0], []string{"nvidia"}) {
		t.Errorf("relabelled with %v, want [nvidia] once", labelled)
	}
	if !strings.Contains(w.String(), "could not update this node's runtime-handler labels") {
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

// `stratum runtime restart` relabels the node from node.yaml once containerd
// is back, since the restart is what loads a handler written without one.
func TestRestartRuntimeRelabelsTheNode(t *testing.T) {
	saved := map[string]string{"nvidia": executable(t)}
	runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: saved})
	oldRestart := restartContainerd
	t.Cleanup(func() { restartContainerd = oldRestart })
	var order []string
	restartContainerd = func(io.Writer) error { order = append(order, "restart"); return nil }
	reconcileOwnNodeLabels = func(_ context.Context, _ string, _ map[string]string, handlers []string) error {
		if !slices.Equal(handlers, []string{"nvidia"}) {
			t.Errorf("relabelled with %v, want [nvidia]", handlers)
		}
		order = append(order, "label")
		return nil
	}
	if err := RestartRuntime(io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "restart,label" {
		t.Errorf("RestartRuntime did %v, want the restart then the labels", order)
	}

	order = nil
	restartContainerd = func(io.Writer) error { return errors.New("containerd is not running") }
	if err := RestartRuntime(io.Discard); err == nil || len(order) != 0 {
		t.Errorf("a failed restart returned %v and did %v, want the error and no relabel", err, order)
	}
}

// A node is labelled for a runtime handler only once its containerd has
// loaded it, on every path that labels: an apply that left containerd running
// without the new handler, a restart, and a kubelet start.
func TestNodeLabelsFollowOnlyTheHandlersContainerdLoaded(t *testing.T) {
	runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1"})
	oldLoaded, oldRestart := loadedHandlers, restartContainerd
	t.Cleanup(func() { loadedHandlers, restartContainerd = oldLoaded, oldRestart })
	loadedHandlers = func(names []string) []string {
		return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n != "gvisor" })
	}
	restartContainerd = func(io.Writer) error { return nil }
	var labelled [][]string
	reconcileOwnNodeLabels = func(_ context.Context, _ string, _ map[string]string, handlers []string) error {
		labelled = append(labelled, handlers)
		return nil
	}
	declared := map[string]string{"gvisor": executable(t), "kata": executable(t)}
	if _, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{Handlers: declared}); err != nil {
		t.Fatal(err)
	}
	if err := RestartRuntime(io.Discard); err != nil {
		t.Fatal(err)
	}
	labelOwnNode(context.Background(), &NodeState{Hostname: "w1"})
	if len(labelled) != 3 {
		t.Fatalf("labelled %d times, want once per path", len(labelled))
	}
	for i, got := range labelled {
		if !slices.Equal(got, []string{"gvisor"}) {
			t.Errorf("path %d labelled for %v, want only the loaded [gvisor]", i, got)
		}
	}
}

// Two applies at once could save one handler set and leave containerd and
// the labels on the other, so an apply or a restart refuses while another
// node operation holds the maintenance lock, before it reads or writes
// anything.
func TestRuntimeApplyAndRestartRefuseWhileTheNodeIsLocked(t *testing.T) {
	saved := map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime"}
	written := runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: saved})
	oldRestart := restartContainerd
	t.Cleanup(func() { restartContainerd = oldRestart })
	restarts := 0
	restartContainerd = func(io.Writer) error { restarts++; return nil }
	release, err := nodelock.Acquire("runtime apply")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{Clear: true}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("apply under the lock = %v, want a refusal naming the holder", err)
	}
	if err := RestartRuntime(io.Discard); err == nil {
		t.Error("restart under the lock succeeded")
	}
	if len(*written) != 0 || restarts != 0 || !maps.Equal(savedHandlers(t), saved) {
		t.Errorf("under the lock: wrote %v, restarted %d times, saved %v; want nothing touched", *written, restarts, savedHandlers(t))
	}
	release()
	if _, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{Clear: true}); err != nil {
		t.Fatal(err)
	}
	if err := RestartRuntime(io.Discard); err != nil {
		t.Fatal(err)
	}
}

// A drop-in that could not be written leaves node.yaml as it was, so no later
// start labels the node for handlers containerd was never given.
func TestApplyRuntimeHandlersSavesNothingWhenTheDropInWriteFails(t *testing.T) {
	saved := map[string]string{"gvisor": executable(t)}
	runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: saved})
	applyHandlers = func(io.Writer, map[string]string, bool) (runtime.ApplyHandlersResult, error) {
		return runtime.ApplyHandlersResult{}, errors.New("disk full")
	}
	res, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{Handlers: map[string]string{"kata": executable(t)}})
	if err == nil || res.Saved {
		t.Fatalf("apply = %+v, %v; want the write's error and nothing saved", res, err)
	}
	if got := savedHandlers(t); !maps.Equal(got, saved) {
		t.Errorf("node.yaml declares %v after the failed write, want the earlier %v", got, saved)
	}
}

// A drop-in written but whose containerd restart failed is what the next
// containerd start loads, so node.yaml saves its handlers all the same: left
// naming the old ones, a later `runtime restart` would label the node for a
// handler containerd no longer has.
func TestApplyRuntimeHandlersSavesAWrittenDropInWhoseRestartFailed(t *testing.T) {
	kata := map[string]string{"kata": executable(t)}
	runtimeApplyHost(t, &NodeState{Role: "worker", Hostname: "w1", RuntimeHandlers: map[string]string{"gvisor": executable(t)}})
	applyHandlers = func(io.Writer, map[string]string, bool) (runtime.ApplyHandlersResult, error) {
		return runtime.ApplyHandlersResult{Changed: true, Written: true, Unloaded: true}, errors.New("containerd did not come back")
	}
	res, err := ApplyRuntimeHandlers(io.Discard, RuntimeApplyOptions{Handlers: kata, Restart: true})
	if err == nil || !strings.Contains(err.Error(), "containerd did not come back") {
		t.Fatalf("apply = %v, want the restart's error", err)
	}
	if !res.Saved {
		t.Error("the written drop-in's handlers were not reported saved")
	}
	if got := savedHandlers(t); !maps.Equal(got, kata) {
		t.Errorf("node.yaml declares %v, want %v, which the drop-in on disk declares", got, kata)
	}
}

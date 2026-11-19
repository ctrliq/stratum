package runtime

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/version"
)

// handlersHost redirects every path ApplyHandlers and CheckHandlers read to a
// temp tree: containerd's config, importing both drop-in directories unless
// withImport is false, the runtime-handler drop-in, the shared conf.d with the
// registry drop-in in it, and the CDI spec directories. The supervisor socket
// points at an absent path, so containerd reports not running until a test
// starts a fakeSupervisor. It returns the shared conf.d directory.
func handlersHost(t *testing.T, withImport bool) string {
	t.Helper()
	root := t.TempDir()
	oldCfg, oldRuntime, oldConfd, oldSock, oldCDI := version.ContainerdConfig, version.ContainerdRuntimeConfD, version.ContainerdConfD, version.ControlSocket, cdiSpecDirs
	t.Cleanup(func() {
		version.ContainerdConfig, version.ContainerdRuntimeConfD, version.ContainerdConfD, version.ControlSocket, cdiSpecDirs = oldCfg, oldRuntime, oldConfd, oldSock, oldCDI
	})
	version.ContainerdConfig = filepath.Join(root, "containerd.toml")
	version.ContainerdRuntimeConfD = filepath.Join(root, "containerd.d", "stratum-runtime.toml")
	version.ContainerdConfD = filepath.Join(root, "conf.d", "stratum-registry.toml")
	version.ControlSocket = filepath.Join(root, "absent.sock")
	cdiSpecDirs = []string{filepath.Join(root, "cdi"), filepath.Join(root, "run-cdi")}
	imports := "'" + confdImportGlob + "'"
	if withImport {
		imports += ", '" + handlersImportGlob() + "'"
	}
	if err := os.WriteFile(version.ContainerdConfig, []byte("version = 3\nimports = ["+imports+"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	confd := filepath.Dir(version.ContainerdConfD)
	if err := os.MkdirAll(confd, 0o755); err != nil {
		t.Fatal(err)
	}
	return confd
}

// TestApplyHandlers_ReportsWhetherItRestarted pins ApplyHandlers' contract,
// cloned from ApplyRegistry's: Restarted is true only when this call restarted
// containerd, which takes a changed drop-in, a running containerd and a caller
// that asked for the restart. Changed alone only means the drop-in changed.
func TestApplyHandlers_ReportsWhetherItRestarted(t *testing.T) {
	t.Run("changed_and_running_restarts_once", func(t *testing.T) {
		handlersHost(t, true)
		restarts := fakeSupervisor(t, false)
		h := map[string]string{"gvisor": fakeBinary(t, "runsc")}
		res, err := ApplyHandlers(io.Discard, h, true)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Changed || !res.Restarted || restarts() != 1 {
			t.Fatalf("first apply = %+v after %d restarts, want changed and restarted once", res, restarts())
		}
		res, err = ApplyHandlers(io.Discard, h, true)
		if err != nil {
			t.Fatal(err)
		}
		if res.Changed || res.Restarted || restarts() != 1 {
			t.Errorf("unchanged apply = %+v after %d restarts, want nothing changed and no second restart", res, restarts())
		}
	})
	t.Run("changed_but_not_running_reports_no_restart", func(t *testing.T) {
		handlersHost(t, true)
		res, err := ApplyHandlers(io.Discard, map[string]string{"gvisor": fakeBinary(t, "runsc")}, true)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Changed || res.Restarted {
			t.Errorf("apply with containerd stopped = %+v, want changed and not restarted", res)
		}
	})
	t.Run("restart_not_asked_for", func(t *testing.T) {
		handlersHost(t, true)
		restarts := fakeSupervisor(t, false)
		res, err := ApplyHandlers(io.Discard, map[string]string{"gvisor": fakeBinary(t, "runsc")}, false)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Changed || res.Restarted || restarts() != 0 {
			t.Errorf("apply without restart = %+v after %d restarts, want changed and no restart", res, restarts())
		}
	})
	t.Run("refused_restart_is_an_error", func(t *testing.T) {
		handlersHost(t, true)
		fakeSupervisor(t, true)
		res, err := ApplyHandlers(io.Discard, map[string]string{"gvisor": fakeBinary(t, "runsc")}, true)
		if err == nil || res.Restarted || !res.Changed || !res.Written {
			t.Errorf("apply with a refused restart = %+v, %v; want written, changed, not restarted, and an error", res, err)
		}
	})
}

func TestRestartContainerd(t *testing.T) {
	handlersHost(t, true)
	if err := RestartContainerd(io.Discard); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("restart with no supervisor = %v, want the not-running refusal", err)
	}
	restarts := fakeSupervisor(t, false)
	if err := RestartContainerd(io.Discard); err != nil {
		t.Fatal(err)
	}
	if restarts() != 1 {
		t.Errorf("restart served %d restart ops, want 1", restarts())
	}
}

// findings is r's findings at severity s, joined for matching.
func findings(r HandlerReport, s Severity) string {
	var out []string
	for _, f := range r.Findings {
		if f.Severity == s {
			out = append(out, f.Message)
		}
	}
	return strings.Join(out, "\n")
}

func TestCheckHandlers(t *testing.T) {
	writeDropIn := func(t *testing.T, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(version.ContainerdRuntimeConfD), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(version.ContainerdRuntimeConfD, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCDISpec := func(t *testing.T) {
		t.Helper()
		if err := os.MkdirAll(cdiSpecDirs[1], 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cdiSpecDirs[1], "nvidia.yaml"), []byte("cdiVersion: 0.6.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("nothing_declared_without_the_import_only_warns", func(t *testing.T) {
		handlersHost(t, false)
		r := CheckHandlers(nil)
		if r.Failed() || !strings.Contains(findings(r, SeverityWarn), "does not import") {
			t.Errorf("a node with no handlers and an old config = %+v, want a warning and no error", r.Findings)
		}
	})
	t.Run("nothing_declared_with_a_config_that_does_not_parse_fails", func(t *testing.T) {
		handlersHost(t, true)
		if err := os.WriteFile(version.ContainerdConfig, []byte("imports = [\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := CheckHandlers(nil); !r.Failed() || !strings.Contains(findings(r, SeverityError), "does not parse") {
			t.Errorf("a node with no handlers and a config containerd cannot read = %+v, want an error", r.Findings)
		}
	})
	t.Run("a_registry_drop_in_that_does_not_parse_fails", func(t *testing.T) {
		handlersHost(t, true)
		if err := os.MkdirAll(filepath.Dir(version.ContainerdConfD), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(version.ContainerdConfD, []byte("[plugins\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if r := CheckHandlers(nil); !r.Failed() || !strings.Contains(findings(r, SeverityError), version.ContainerdConfD+" does not parse") {
			t.Errorf("a registry drop-in containerd cannot read = %+v, want an error naming it", r.Findings)
		}
	})
	t.Run("nothing_declared", func(t *testing.T) {
		handlersHost(t, true)
		r := CheckHandlers(nil)
		if r.Failed() || !strings.Contains(findings(r, SeverityOK), "no runtime handlers declared") {
			t.Errorf("a node with no handlers = %+v, want only ok findings", r.Findings)
		}
	})
	t.Run("in_sync", func(t *testing.T) {
		handlersHost(t, true)
		h := map[string]string{"gvisor": fakeBinary(t, "runsc")}
		writeDropIn(t, renderHandlers(h, CgroupDriver()))
		writeCDISpec(t)
		r := CheckHandlers(h)
		if r.Failed() || findings(r, SeverityWarn) != "" {
			t.Fatalf("an in-sync node = %+v, want only ok findings", r.Findings)
		}
		if !strings.Contains(findings(r, SeverityOK), "1 spec(s)") {
			t.Errorf("ok findings %q do not count the CDI spec", findings(r, SeverityOK))
		}
	})
	for _, tc := range []struct {
		name    string
		dropIn  func(h map[string]string) string
		saved   bool
		missing bool
		noImp   bool
		want    string
	}{
		{name: "drop_in_missing", saved: true, want: "is missing"},
		{name: "drop_in_not_ours", dropIn: func(map[string]string) string { return "version = 3\n" }, saved: true, want: "was not written by stratum"},
		{name: "drop_in_stale", dropIn: func(h map[string]string) string { return renderHandlers(h, CgroupDriver()) }, want: "no longer saves"},
		{name: "drop_in_drifted", dropIn: func(h map[string]string) string {
			return renderHandlers(map[string]string{"kata": h["gvisor"]}, CgroupDriver())
		}, saved: true, want: "does not match"},
		{name: "binary_missing", dropIn: func(h map[string]string) string { return renderHandlers(h, CgroupDriver()) }, saved: true, missing: true, want: "pods using it will not start"},
		{name: "config_without_import", dropIn: func(h map[string]string) string { return renderHandlers(h, CgroupDriver()) }, saved: true, noImp: true, want: "does not import"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handlersHost(t, !tc.noImp)
			bin := fakeBinary(t, "runsc")
			h := map[string]string{"gvisor": bin}
			if tc.dropIn != nil {
				writeDropIn(t, tc.dropIn(h))
			}
			if tc.missing {
				if err := os.Remove(bin); err != nil {
					t.Fatal(err)
				}
			}
			saved := h
			if !tc.saved {
				saved = nil
			}
			r := CheckHandlers(saved)
			if !r.Failed() || !strings.Contains(findings(r, SeverityError), tc.want) {
				t.Errorf("errors = %q, want one containing %q", findings(r, SeverityError), tc.want)
			}
		})
	}
	t.Run("no_cdi_specs_warns", func(t *testing.T) {
		handlersHost(t, true)
		h := map[string]string{"gvisor": fakeBinary(t, "runsc")}
		writeDropIn(t, renderHandlers(h, CgroupDriver()))
		r := CheckHandlers(h)
		if r.Failed() || !strings.Contains(findings(r, SeverityWarn), "no CDI specs") {
			t.Errorf("a node without CDI specs = %+v, want a warning and no error", r.Findings)
		}
	})
	t.Run("shared_conf_d_contends", func(t *testing.T) {
		confd := handlersHost(t, true)
		h := map[string]string{"nvidia": fakeBinary(t, "nvidia-container-runtime")}
		writeDropIn(t, renderHandlers(h, CgroupDriver()))
		writeCDISpec(t)
		nvidiaCtk := `version = 3
[plugins."io.containerd.cri.v1.runtime".containerd]
  default_runtime_name = "nvidia"
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.nvidia]
  runtime_type = "io.containerd.runc.v2"
`
		for name, content := range map[string]string{
			"99-nvidia.toml":         nvidiaCtk,
			"stratum-registry.toml":  "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.nvidia]\n",
			"unrelated-runtime.toml": "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.kata]\n",
		} {
			if err := os.WriteFile(filepath.Join(confd, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		r := CheckHandlers(h)
		warns := findings(r, SeverityWarn)
		if r.Failed() {
			t.Errorf("contending conf.d files failed the check: %q", findings(r, SeverityError))
		}
		for _, want := range []string{"99-nvidia.toml also defines runtime nvidia", "stratum's definition wins", "default runtime to nvidia"} {
			if !strings.Contains(warns, want) {
				t.Errorf("warnings %q lack %q", warns, want)
			}
		}
		for _, notWant := range []string{"stratum-registry.toml", "unrelated-runtime.toml"} {
			if strings.Contains(warns, notWant) {
				t.Errorf("warnings %q name %s, which does not contend", warns, notWant)
			}
		}
	})
	t.Run("unparseable_conf_d_fails", func(t *testing.T) {
		confd := handlersHost(t, true)
		h := map[string]string{"nvidia": fakeBinary(t, "nvidia-container-runtime")}
		writeDropIn(t, renderHandlers(h, CgroupDriver()))
		writeCDISpec(t)
		if err := os.WriteFile(filepath.Join(confd, "broken.toml"), []byte("[plugins\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		r := CheckHandlers(h)
		if !r.Failed() || !strings.Contains(findings(r, SeverityError), "broken.toml does not parse") {
			t.Errorf("a conf.d file containerd refuses to start with = %+v, want an error naming it", r.Findings)
		}
	})
}

// A drop-in written without restarting containerd stays pending until a
// restart: a later apply restarts even though the file no longer changes, and
// the node is labelled only for the handlers the running containerd loaded.
func TestApplyHandlersRestartsAContainerdThatHasNotLoadedTheDropIn(t *testing.T) {
	handlersHost(t, true)
	restarts := fakeSupervisor(t, false)
	gvisor := fakeBinary(t, "runsc")
	kata := fakeBinary(t, "kata-runtime")
	if _, err := ApplyHandlers(io.Discard, map[string]string{"gvisor": gvisor}, true); err != nil {
		t.Fatal(err)
	}
	both := map[string]string{"gvisor": gvisor, "kata": kata}
	res, err := ApplyHandlers(io.Discard, both, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || !res.Unloaded || res.Restarted {
		t.Fatalf("apply without a restart = %+v, want changed and unloaded", res)
	}
	if got, err := LoadedHandlers(context.Background(), []string{"gvisor", "kata"}); err != nil || !slices.Equal(got, []string{"gvisor"}) {
		t.Errorf("LoadedHandlers before the restart = %v, %v; want [gvisor]", got, err)
	}
	if r := CheckHandlers(both); !strings.Contains(findings(r, SeverityError), "has not loaded the runtime handlers kata") {
		t.Errorf("check before the restart = %+v, want an error naming kata and the restart", r.Findings)
	}
	res, err = ApplyHandlers(io.Discard, both, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || !res.Restarted || res.Unloaded || restarts() != 2 {
		t.Fatalf("retried apply = %+v after %d restarts, want the pending restart done", res, restarts())
	}
	if got, err := LoadedHandlers(context.Background(), []string{"gvisor", "kata"}); err != nil || !slices.Equal(got, []string{"gvisor", "kata"}) {
		t.Errorf("LoadedHandlers after the restart = %v, %v; want both", got, err)
	}
}

// The loaded set is containerd's own answer, not a record stratum keeps: a
// drop-in removed by hand before a restart leaves containerd with no handler
// loaded, and the node is labelled for none.
func TestLoadedHandlersFollowsContainerdNotTheDropIn(t *testing.T) {
	handlersHost(t, true)
	fakeSupervisor(t, false)
	h := map[string]string{"gvisor": fakeBinary(t, "runsc")}
	if _, err := ApplyHandlers(io.Discard, h, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(version.ContainerdRuntimeConfD); err != nil {
		t.Fatal(err)
	}
	if err := RestartContainerd(io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadedHandlers(context.Background(), []string{"gvisor"}); err != nil || len(got) != 0 {
		t.Errorf("LoadedHandlers after a restart without the drop-in = %v, %v; want none", got, err)
	}
	if r := CheckHandlers(h); !strings.Contains(findings(r, SeverityError), "has not loaded the runtime handlers gvisor") {
		t.Errorf("check = %+v, want an error that gvisor is not loaded", r.Findings)
	}
}

// A handler removed from the drop-in keeps running in containerd until a
// restart: an apply that removes it restarts, though the remaining handler
// is loaded.
func TestApplyHandlersRestartsAContainerdThatStillRunsARemovedHandler(t *testing.T) {
	handlersHost(t, true)
	restarts := fakeSupervisor(t, false)
	gvisor, kata := fakeBinary(t, "runsc"), fakeBinary(t, "kata-runtime")
	if _, err := ApplyHandlers(io.Discard, map[string]string{"gvisor": gvisor, "kata": kata}, true); err != nil {
		t.Fatal(err)
	}
	only := map[string]string{"gvisor": gvisor}
	if _, err := ApplyHandlers(io.Discard, only, false); err != nil {
		t.Fatal(err)
	}
	if r := CheckHandlers(only); !strings.Contains(findings(r, SeverityWarn), "still runs the runtime handlers kata") {
		t.Errorf("check = %+v, want a warning that kata still runs", r.Findings)
	}
	res, err := ApplyHandlers(io.Discard, only, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || !res.Restarted || restarts() != 2 {
		t.Errorf("apply = %+v after %d restarts, want the restart that unloads kata", res, restarts())
	}
}

// A runtime another conf.d file defines is containerd's, not a stale stratum
// handler, so it never makes an apply restart.
func TestApplyHandlersLeavesARuntimeAnotherConfDFileDefines(t *testing.T) {
	confd := handlersHost(t, true)
	restarts := fakeSupervisor(t, false)
	nvidia := "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.nvidia]\nruntime_type = 'io.containerd.runc.v2'\n"
	if err := os.WriteFile(filepath.Join(confd, "99-nvidia.toml"), []byte(nvidia), 0o644); err != nil {
		t.Fatal(err)
	}
	old := criRuntimeHandlers
	t.Cleanup(func() { criRuntimeHandlers = old })
	criRuntimeHandlers = func(ctx context.Context) ([]string, error) {
		names, err := old(ctx)
		return append(names, "nvidia", "runc"), err
	}
	h := map[string]string{"gvisor": fakeBinary(t, "runsc")}
	if _, err := ApplyHandlers(io.Discard, h, true); err != nil {
		t.Fatal(err)
	}
	res, err := ApplyHandlers(io.Discard, h, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restarted || restarts() != 1 {
		t.Errorf("second apply = %+v after %d restarts, want no restart for runc or another file's runtime", res, restarts())
	}
}

// A containerd that cannot say which handlers it loaded is a restart still
// pending, not a containerd that runs them all: apply restarts it, and
// LoadedHandlers reports the error rather than every name.
func TestAContainerdThatCannotSayIsPending(t *testing.T) {
	handlersHost(t, true)
	restarts := fakeSupervisor(t, false)
	h := map[string]string{"gvisor": fakeBinary(t, "runsc")}
	if _, err := ApplyHandlers(io.Discard, h, true); err != nil {
		t.Fatal(err)
	}
	failCRI(t)
	if got, err := LoadedHandlers(context.Background(), []string{"gvisor"}); err == nil {
		t.Errorf("LoadedHandlers = %v with containerd unable to say, want an error", got)
	}
	res, err := ApplyHandlers(io.Discard, h, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Restarted || restarts() != 2 {
		t.Errorf("apply = %+v after %d restarts, want a restart", res, restarts())
	}
	if r := CheckHandlers(h); !strings.Contains(findings(r, SeverityWarn), "cannot tell whether it runs the saved handlers") {
		t.Errorf("check = %+v, want a warning that containerd could not say", r.Findings)
	}
}

// A drop-in renamed into place whose directory sync failed is on disk, so a
// retry finds nothing to write; containerd has not loaded it, so the retry
// restarts it.
func TestApplyHandlersThatCouldNotSyncTheDropInLeavesTheRestartPending(t *testing.T) {
	handlersHost(t, true)
	restarts := fakeSupervisor(t, false)
	gvisor := map[string]string{"gvisor": fakeBinary(t, "runsc")}
	failDirSync(t)
	res, err := ApplyHandlers(io.Discard, gvisor, true)
	if !errors.Is(err, atomicfile.ErrNotDurable) || !res.Changed || !res.Unloaded || res.Restarted || res.Written {
		t.Fatalf("apply = %+v, %v; want a changed, unloaded drop-in and the write's ErrNotDurable", res, err)
	}
	syncDir = atomicfile.SyncDir
	res, err = ApplyHandlers(io.Discard, gvisor, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || !res.Restarted || restarts() != 1 {
		t.Fatalf("retried apply = %+v after %d restarts, want the pending restart done", res, restarts())
	}
}

// A conf.d file that does not parse stops containerd whether or not this node
// declares handlers, so check reports it with none declared too.
func TestCheckHandlersReadsTheSharedConfDWithNoHandlers(t *testing.T) {
	confd := handlersHost(t, true)
	if err := os.WriteFile(filepath.Join(confd, "99-broken.toml"), []byte("version = [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := CheckHandlers(nil)
	if !r.Failed() || !strings.Contains(findings(r, SeverityError), "does not parse") {
		t.Errorf("check with no handlers = %+v, want an error for the conf.d file that does not parse", r.Findings)
	}
}

// The drop-in's path is stratum's, but a file there it did not write may hold
// an operator's configuration: RequireOwnDropIn, which runtime apply runs,
// refuses to replace it, while a node start, which must not fail on it,
// replaces it and says so.
func TestAForeignDropInIsRefusedByApplyAndReplacedByAStart(t *testing.T) {
	handlersHost(t, true)
	mine := "[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.kata]\nruntime_type = 'io.containerd.kata.v2'\n"
	if err := os.MkdirAll(filepath.Dir(version.ContainerdRuntimeConfD), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(version.ContainerdRuntimeConfD, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	gvisor := map[string]string{"gvisor": fakeBinary(t, "runsc")}
	if err := RequireOwnDropIn(nil); err != nil {
		t.Errorf("no handlers = %v, want nothing to refuse", err)
	}
	if err := RequireOwnDropIn(gvisor); err == nil || !strings.Contains(err.Error(), "not written by stratum") {
		t.Fatalf("RequireOwnDropIn over a foreign drop-in = %v, want a refusal", err)
	}
	var out strings.Builder
	if _, err := ApplyHandlers(&out, gvisor, false); err != nil {
		t.Fatalf("a start over a foreign drop-in = %v, want it replaced", err)
	}
	if !strings.Contains(out.String(), "was not written by stratum; replacing it") {
		t.Errorf("a start replaced the foreign drop-in without saying so:\n%s", out.String())
	}
	if got, _ := os.ReadFile(version.ContainerdRuntimeConfD); !strings.HasPrefix(string(got), handlersMarker) {
		t.Errorf("the drop-in after a start = %q, want stratum's", got)
	}
	if err := RequireOwnDropIn(gvisor); err != nil {
		t.Errorf("RequireOwnDropIn over stratum's own drop-in = %v", err)
	}
}

// containerd imports every shared conf.d file, so one that does not parse is
// refused by name.
func TestRequireSharedConfDParses(t *testing.T) {
	handlersHost(t, true)
	if err := RequireSharedConfDParses(); err != nil {
		t.Fatalf("a clean conf.d = %v", err)
	}
	bad := filepath.Join(filepath.Dir(version.ContainerdConfD), "broken.toml")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("[plugins\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RequireSharedConfDParses(); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("a conf.d with %s = %v, want a refusal naming it", bad, err)
	}
}

// runtime apply's preflight and runtime check read the shared conf.d through
// one reader, so they agree: valid files, stratum's registry drop-in and an
// nvidia-ctk drop-in among them, pass both, and a file that does not parse
// fails both, by name.
func TestApplyAndCheckAgreeOnTheSharedConfD(t *testing.T) {
	handlersHost(t, true)
	confd := filepath.Dir(version.ContainerdConfD)
	if err := os.MkdirAll(confd, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		filepath.Base(version.ContainerdConfD): "[plugins.'io.containerd.cri.v1.images'.registry]\nconfig_path = '/etc/containerd/certs.d'\n",
		"99-nvidia.toml": "[plugins.'io.containerd.cri.v1.runtime'.containerd]\ndefault_runtime_name = 'runc'\n" +
			"[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.nvidia.options]\nBinaryName = '/usr/bin/nvidia-container-runtime'\n",
	} {
		if err := os.WriteFile(filepath.Join(confd, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := RequireSharedConfDParses(); err != nil {
		t.Errorf("apply's preflight refused a valid conf.d: %v", err)
	}
	if got := findings(CheckHandlers(nil), SeverityError); strings.Contains(got, "does not parse") {
		t.Errorf("check reported a valid conf.d as broken: %s", got)
	}

	bad := filepath.Join(confd, "broken.toml")
	if err := os.WriteFile(bad, []byte("[plugins\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RequireSharedConfDParses(); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("apply's preflight = %v, want a refusal naming %s", err, bad)
	}
	if got := findings(CheckHandlers(nil), SeverityError); !strings.Contains(got, bad+" does not parse") {
		t.Errorf("check = %s, want %s reported as not parsing", got, bad)
	}
}

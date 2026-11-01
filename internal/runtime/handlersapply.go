package runtime

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/version"
)

// ApplyHandlersResult is ApplyHandlers' outcome. Changed means the drop-in
// changed on disk; Restarted is true only when this call restarted a running
// containerd, so a caller reports the restart from Restarted, never Changed.
// Unloaded means the running containerd started before the drop-in's latest
// change, by this call or an earlier one that did not restart it.
type ApplyHandlersResult struct {
	Changed   bool
	Restarted bool
	Unloaded  bool
}

// ApplyHandlers writes the runtime-handler drop-in for handlers and, with
// restart, restarts a running containerd when the drop-in changed. It refuses,
// writing nothing, when containerd's config does not import the drop-in's
// directory: a config rendered before runtime handlers existed would never
// read the file, and `stratum up` or an upgrade renders one that does.
func ApplyHandlers(w io.Writer, handlers map[string]string, restart bool) (ApplyHandlersResult, error) {
	var out ApplyHandlersResult
	if err := RequireHandlersImport(); err != nil {
		return out, err
	}
	behind := loadedHandlerNames()
	loaded := behind.names
	if behind.pid == 0 {
		loaded = dropInHandlerNames()
	}
	changed, err := writeHandlers(w, handlers)
	out.Changed = changed
	if err != nil {
		return out, err
	}
	if (!changed && behind.pid == 0) || !Active() {
		return out, nil
	}
	if restart {
		restarted, err := restartContainerd(" after a runtime handler change")
		out.Restarted = restarted
		if restarted {
			clearUnloaded()
		}
		if err != nil {
			if !restarted {
				out.Unloaded = true
				recordUnloaded(loaded)
			}
			return out, err
		}
		return out, nil
	}
	out.Unloaded = true
	recordUnloaded(loaded)
	return out, nil
}

// unloadedPath records which runtime handlers the running containerd loaded
// when the drop-in changed without restarting it: that containerd's PID, then
// one handler name per line. containerd imports only *.toml from the
// drop-in's directory, so it never reads the record as config.
func unloadedPath() string { return version.ContainerdRuntimeConfD + ".unloaded" }

// loadedHandlers is the runtime handlers a running containerd loaded. A zero
// pid means it loaded the drop-in on disk, or that no containerd runs.
type loadedHandlers struct {
	pid   int
	names []string
}

// containerdPID is the PID of the containerd the supervisor runs, or 0.
func containerdPID() int {
	st, err := comps.Status()
	if err != nil {
		return 0
	}
	if p := st[comps.Normalize("containerd")]; p.Running && !p.Foreign {
		return p.Pid
	}
	return 0
}

// loadedHandlerNames reads the unloaded record. It counts only while the
// containerd it names still runs: any restart since, by `stratum runtime
// restart`, `stratum registry apply`, an upgrade or the supervisor, loaded
// the drop-in on disk.
func loadedHandlerNames() loadedHandlers {
	b, err := os.ReadFile(unloadedPath())
	if err != nil {
		return loadedHandlers{}
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	pid, err := strconv.Atoi(lines[0])
	if err != nil || pid == 0 || pid != containerdPID() {
		return loadedHandlers{}
	}
	return loadedHandlers{pid: pid, names: slices.DeleteFunc(lines[1:], func(n string) bool { return n == "" })}
}

// dropInHandlerNames is the runtime handlers the drop-in on disk declares.
func dropInHandlerNames() []string {
	var c sharedConfD
	if _, err := toml.DecodeFile(version.ContainerdRuntimeConfD, &c); err != nil {
		return nil
	}
	var names []string
	for _, p := range c.Plugins {
		names = append(names, slices.Collect(maps.Keys(p.Containerd.Runtimes))...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// recordUnloaded notes that the running containerd has not loaded the drop-in
// on disk, only the runtime handlers names.
func recordUnloaded(names []string) {
	pid := containerdPID()
	if pid == 0 {
		return
	}
	body := strconv.Itoa(pid) + "\n" + strings.Join(names, "\n") + "\n"
	if err := atomicfile.WriteFileWith(unloadedPath(), []byte(body), 0o644, atomicfileSeams()); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: record that containerd has not loaded the runtime handlers: %v\n", err)
	}
}

func clearUnloaded() {
	if err := os.Remove(unloadedPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "WARN: remove %s: %v\n", unloadedPath(), err)
	}
}

// LoadedHandlers narrows names to the runtime handlers the running containerd
// has loaded, so a node is labelled for a handler only once its containerd
// can run it.
func LoadedHandlers(names []string) []string {
	loaded := loadedHandlerNames()
	if loaded.pid == 0 {
		return names
	}
	return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !slices.Contains(loaded.names, n) })
}

// RequireHandlersImport reports whether containerd's config imports the
// runtime-handler drop-in's directory after the shared conf.d.
func RequireHandlersImport() error {
	cfg, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		return fmt.Errorf("read containerd config: %w", err)
	}
	if !importsDropIns(string(cfg)) {
		return fmt.Errorf("%s does not import %s after %s, so containerd would not read runtime handlers; run `stratum up` or upgrade this node to render it again",
			version.ContainerdConfig, handlersImportGlob(), confdImportGlob)
	}
	return nil
}

// RestartContainerd restarts the supervisor's containerd and waits for it to
// report active again. Running containers keep running: their shims outlive
// containerd.
func RestartContainerd() error {
	if !Active() {
		return errors.New("containerd is not running under the stratum supervisor; start the node with `stratum up`")
	}
	restarted, err := restartContainerd("")
	if restarted {
		clearUnloaded()
	}
	return err
}

// Severity grades a HandlerFinding.
type Severity string

const (
	SeverityOK    Severity = "ok"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// HandlerFinding is one line of CheckHandlers' report.
type HandlerFinding struct {
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// HandlerReport is CheckHandlers' report on the node's runtime handlers.
type HandlerReport struct {
	Handlers map[string]string `json:"handlers,omitempty"`
	Findings []HandlerFinding  `json:"findings"`
}

// Failed reports whether any finding is an error.
func (r HandlerReport) Failed() bool {
	return slices.ContainsFunc(r.Findings, func(f HandlerFinding) bool { return f.Severity == SeverityError })
}

func (r *HandlerReport) add(s Severity, format string, args ...any) {
	r.Findings = append(r.Findings, HandlerFinding{Severity: s, Message: fmt.Sprintf(format, args...)})
}

// CheckHandlers reports whether containerd will run the handlers this node
// saved: the drop-in matches them, each binary is executable, containerd's
// config imports the drop-in, CDI has specs to resolve, and which other
// conf.d files define the same runtime names. It changes nothing.
func CheckHandlers(saved map[string]string) HandlerReport {
	r := HandlerReport{Handlers: saved}
	checkDropIn(&r, saved)
	if loaded := loadedHandlerNames(); loaded.pid != 0 {
		r.add(SeverityError, "containerd (pid %d) started before %s last changed and runs only the handlers %s; run `stratum runtime restart`",
			loaded.pid, version.ContainerdRuntimeConfD, cmp.Or(strings.Join(loaded.names, ", "), "runc"))
	}
	if err := RequireHandlersImport(); err != nil {
		severity := SeverityError
		if len(saved) == 0 {
			severity = SeverityWarn
		}
		r.add(severity, "%v", err)
	} else {
		r.add(SeverityOK, "%s imports %s after %s", version.ContainerdConfig, handlersImportGlob(), confdImportGlob)
	}
	if len(saved) == 0 {
		return r
	}
	for _, name := range slices.Sorted(maps.Keys(saved)) {
		if err := CheckHandlerBinary(saved[name]); err != nil {
			r.add(SeverityError, "runtime handler %s: %v; pods using it will not start", name, err)
		} else {
			r.add(SeverityOK, "runtime handler %s runs %s", name, saved[name])
		}
	}
	checkCDISpecs(&r)
	checkSharedConfD(&r, saved)
	return r
}

// checkDropIn compares the drop-in on disk with what ApplyHandlers would write
// for saved.
func checkDropIn(r *HandlerReport, saved map[string]string) {
	path := version.ContainerdRuntimeConfD
	got, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && len(saved) == 0:
		r.add(SeverityOK, "no runtime handlers declared")
	case errors.Is(err, fs.ErrNotExist):
		r.add(SeverityError, "%s is missing; run `stratum runtime apply` to write it", path)
	case err != nil:
		r.add(SeverityError, "read %s: %v", path, err)
	case !strings.HasPrefix(string(got), handlersMarker):
		r.add(SeverityError, "%s was not written by stratum; move it aside and run `stratum runtime apply`", path)
	case len(saved) == 0:
		r.add(SeverityError, "%s declares runtime handlers this node no longer saves; run `stratum runtime apply` to remove it", path)
	case string(got) != renderHandlers(saved, CgroupDriver()):
		r.add(SeverityError, "%s does not match the runtime handlers this node saved; run `stratum runtime apply`", path)
	default:
		r.add(SeverityOK, "%s declares %s", path, strings.Join(slices.Sorted(maps.Keys(saved)), ", "))
	}
}

// cdiSpecExts are the file extensions containerd reads CDI specs from.
var cdiSpecExts = []string{".json", ".yaml", ".yml"}

// checkCDISpecs counts the CDI specs in the drop-in's spec directories. None
// is a warning, not an error: a node can run handlers that need no devices.
func checkCDISpecs(r *HandlerReport) {
	count := 0
	for _, dir := range cdiSpecDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && slices.Contains(cdiSpecExts, filepath.Ext(e.Name())) {
				count++
			}
		}
	}
	if count == 0 {
		r.add(SeverityWarn, "no CDI specs in %s; a pod asking for a device by CDI name will not start until a device plugin or `nvidia-ctk cdi generate` writes one",
			strings.Join(cdiSpecDirs, " or "))
		return
	}
	r.add(SeverityOK, "CDI is enabled with %d spec(s) in %s", count, strings.Join(cdiSpecDirs, " and "))
}

// sharedConfD is the slice of a conf.d drop-in that can contend with
// stratum's: the runtimes it defines and the default it sets, under both the
// CRI plugin containerd 2 reads and the one containerd 1 used.
type sharedConfD struct {
	Plugins map[string]struct {
		Containerd struct {
			DefaultRuntimeName string              `toml:"default_runtime_name"`
			Runtimes           map[string]struct{} `toml:"runtimes"`
		} `toml:"containerd"`
	} `toml:"plugins"`
}

// checkSharedConfD warns about every file in the shared conf.d that defines
// one of saved's runtime names, which `nvidia-ctk runtime configure` does, or
// changes containerd's default runtime. stratum's drop-in is imported after
// conf.d, so its definition of a name wins, but a changed default runs every
// pod without a runtimeClassName under that runtime.
func checkSharedConfD(r *HandlerReport, saved map[string]string) {
	dir := filepath.Dir(version.ContainerdConfD)
	files, _ := filepath.Glob(filepath.Join(dir, "*.toml"))
	for _, f := range files {
		if f == version.ContainerdConfD {
			continue
		}
		var c sharedConfD
		if _, err := toml.DecodeFile(f, &c); err != nil {
			r.add(SeverityError, "%s does not parse (%v); containerd will refuse to start with it", f, err)
			continue
		}
		for _, plugin := range slices.Sorted(maps.Keys(c.Plugins)) {
			p := c.Plugins[plugin].Containerd
			for _, name := range slices.Sorted(maps.Keys(p.Runtimes)) {
				if _, ok := saved[name]; ok {
					r.add(SeverityWarn, "%s also defines runtime %s; containerd reads %s after it, so stratum's definition wins", f, name, version.ContainerdRuntimeConfD)
				}
			}
			if p.DefaultRuntimeName != "" && p.DefaultRuntimeName != "runc" {
				r.add(SeverityWarn, "%s sets containerd's default runtime to %s, so every pod without a runtimeClassName runs under it", f, p.DefaultRuntimeName)
			}
		}
	}
}

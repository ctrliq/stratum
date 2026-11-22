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

// ApplyHandlersResult: a caller reports the restart from Restarted, never Changed.
type ApplyHandlersResult struct {
	Changed   bool
	Written   bool
	Restarted bool
	Unloaded  bool
}

// ApplyHandlers writes the drop-in; a caller runs RequireHandlersImport first or it is never read.
func ApplyHandlers(w io.Writer, handlers map[string]string, restart bool) (ApplyHandlersResult, error) {
	var out ApplyHandlersResult
	behind := loadedHandlerNames()
	loaded := behind.names
	if behind.pid == 0 {
		loaded = dropInHandlerNames()
	}
	changed, err := writeHandlers(w, handlers)
	out.Changed = changed
	if err != nil {
		if changed && Active() {
			out.Unloaded = true
			recordUnloaded(w, loaded)
		}
		return out, err
	}
	out.Written = true
	if (!changed && behind.pid == 0) || !Active() {
		return out, nil
	}
	if restart {
		restarted, err := restartContainerd(" after a runtime handler change")
		out.Restarted = restarted
		if restarted {
			clearUnloaded(w)
		}
		if err != nil {
			if !restarted {
				out.Unloaded = true
				recordUnloaded(w, loaded)
			}
			return out, err
		}
		return out, nil
	}
	out.Unloaded = true
	recordUnloaded(w, loaded)
	return out, nil
}

// containerd imports only *.toml here, so it never reads the record as config.
func unloadedPath() string { return version.ContainerdRuntimeConfD + ".unloaded" }

// loadedHandlers: a zero pid means the drop-in on disk, or no containerd runs.
type loadedHandlers struct {
	pid   int
	names []string
}

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

// processStart: a reboot can reuse the PID the record names, never the start.
var processStart = func(pid int) string {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	_, rest, ok := strings.Cut(string(stat), ") ")
	fields := strings.Fields(rest)
	if !ok || len(fields) < 20 {
		return ""
	}
	return strings.TrimSpace(string(boot)) + "/" + fields[19]
}

// loadedHandlerNames counts only while the named containerd still runs.
func loadedHandlerNames() loadedHandlers {
	b, err := os.ReadFile(unloadedPath())
	if err != nil {
		return loadedHandlers{}
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	pidText, start, _ := strings.Cut(lines[0], " ")
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid == 0 || pid != containerdPID() || start != processStart(pid) {
		return loadedHandlers{}
	}
	return loadedHandlers{pid: pid, names: slices.DeleteFunc(lines[1:], func(n string) bool { return n == "" })}
}

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

// A record it cannot write leaves the node labelled for every handler, so it says so.
func recordUnloaded(w io.Writer, names []string) {
	pid := containerdPID()
	if pid == 0 {
		return
	}
	body := strconv.Itoa(pid) + " " + processStart(pid) + "\n" + strings.Join(names, "\n") + "\n"
	if err := atomicfile.WriteFileWith(unloadedPath(), []byte(body), 0o644, atomicfileSeams()); err != nil {
		fmt.Fprintf(w, "WARN: could not record that containerd has not loaded the runtime handlers (%v); this node may be labelled for them before containerd can run them. Run `stratum runtime restart` to load them\n", err)
	}
}

// clearUnloaded: a record it cannot remove names the replaced containerd, already ignored.
func clearUnloaded(w io.Writer) {
	if err := os.Remove(unloadedPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(w, "WARN: could not remove %s (%v); it names the containerd this restart replaced, so it is already ignored\n", unloadedPath(), err)
	}
}

// LoadedHandlers: a node is labelled for a handler only once its containerd can run it.
func LoadedHandlers(names []string) []string {
	loaded := loadedHandlerNames()
	if loaded.pid == 0 {
		return names
	}
	return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !slices.Contains(loaded.names, n) })
}

// errNoHandlersImport: only a missing import is harmless on a node that declares none.
var errNoHandlersImport = errors.New("does not import the runtime-handler drop-in")

func RequireHandlersImport() error {
	cfg, err := os.ReadFile(version.ContainerdConfig)
	if err != nil {
		return fmt.Errorf("read containerd config: %w", err)
	}
	if _, err := configImports(string(cfg)); err != nil {
		return fmt.Errorf("%s does not parse (%v); containerd will refuse to start with it", version.ContainerdConfig, err)
	}
	if !importsDropIns(string(cfg)) {
		return fmt.Errorf("%s %w %s after %s, so containerd would not read runtime handlers; run `stratum up` or upgrade this node to render it again",
			version.ContainerdConfig, errNoHandlersImport, handlersImportGlob(), confdImportGlob)
	}
	return nil
}

// RestartContainerd: running containers keep running, since their shims outlive containerd.
func RestartContainerd(w io.Writer) error {
	if !Active() {
		return errors.New("containerd is not running under the stratum supervisor; start the node with `stratum up`")
	}
	restarted, err := restartContainerd("")
	if restarted {
		clearUnloaded(w)
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

// The shared conf.d is checked even with no handlers declared: a bad file there stops containerd.
func CheckHandlers(saved map[string]string) HandlerReport {
	r := HandlerReport{Handlers: saved}
	checkDropIn(&r, saved)
	if loaded := loadedHandlerNames(); loaded.pid != 0 {
		r.add(SeverityError, "containerd (pid %d) started before %s last changed and runs only the handlers %s; run `stratum runtime restart`",
			loaded.pid, version.ContainerdRuntimeConfD, cmp.Or(strings.Join(loaded.names, ", "), "runc"))
	}
	if err := RequireHandlersImport(); err != nil {
		severity := SeverityError
		if len(saved) == 0 && errors.Is(err, errNoHandlersImport) {
			severity = SeverityWarn
		}
		r.add(severity, "%v", err)
	} else {
		r.add(SeverityOK, "%s imports %s after %s", version.ContainerdConfig, handlersImportGlob(), confdImportGlob)
	}
	checkSharedConfD(&r, saved)
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
	return r
}

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

var cdiSpecExts = []string{".json", ".yaml", ".yml"}

// checkCDISpecs: none is a warning; a node can run handlers that need no devices.
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

// sharedConfD is the slice of a conf.d drop-in that can contend with stratum's.
type sharedConfD struct {
	Plugins map[string]struct {
		Containerd struct {
			DefaultRuntimeName string              `toml:"default_runtime_name"`
			Runtimes           map[string]struct{} `toml:"runtimes"`
		} `toml:"containerd"`
	} `toml:"plugins"`
}

// RequireSharedConfDParses: restarting with one bad conf.d file stops a working containerd.
func RequireSharedConfDParses() error {
	if err := sharedConfDParseError(); err != nil {
		return fmt.Errorf("%w, so nothing was changed. Fix or remove it and run again", err)
	}
	return nil
}

func sharedConfDParseError() error {
	for _, f := range readSharedConfD() {
		if f.err != nil {
			return fmt.Errorf("%s does not parse (%v); containerd would refuse to start with it", f.path, f.err)
		}
	}
	return nil
}

// sharedConfDFile: the part of a conf.d file that can contend with stratum's, or its parse error.
type sharedConfDFile struct {
	path string
	conf sharedConfD
	err  error
}

// readSharedConfD is the one reader, so apply and check agree on what does not parse.
func readSharedConfD() []sharedConfDFile {
	paths, _ := filepath.Glob(filepath.Join(filepath.Dir(version.ContainerdConfD), "*.toml"))
	files := make([]sharedConfDFile, 0, len(paths))
	for _, path := range paths {
		f := sharedConfDFile{path: path}
		_, f.err = toml.DecodeFile(path, &f.conf)
		files = append(files, f)
	}
	return files
}

// stratum's drop-in wins on names; a changed default runs every pod without a runtimeClassName.
func checkSharedConfD(r *HandlerReport, saved map[string]string) {
	for _, f := range readSharedConfD() {
		if f.err != nil {
			r.add(SeverityError, "%s does not parse (%v); containerd will refuse to start with it", f.path, f.err)
			continue
		}
		if f.path == version.ContainerdConfD {
			continue
		}
		for _, plugin := range slices.Sorted(maps.Keys(f.conf.Plugins)) {
			p := f.conf.Plugins[plugin].Containerd
			for _, name := range slices.Sorted(maps.Keys(p.Runtimes)) {
				if _, ok := saved[name]; ok {
					r.add(SeverityWarn, "%s also defines runtime %s; containerd reads %s after it, so stratum's definition wins", f.path, name, version.ContainerdRuntimeConfD)
				}
			}
			if p.DefaultRuntimeName != "" && p.DefaultRuntimeName != "runc" {
				r.add(SeverityWarn, "%s sets containerd's default runtime to %s, so every pod without a runtimeClassName runs under it", f.path, p.DefaultRuntimeName)
			}
		}
	}
}

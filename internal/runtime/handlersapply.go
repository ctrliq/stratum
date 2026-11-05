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
// changed on disk; Written means the drop-in on disk now declares the
// handlers, even when the restart after it failed; Restarted is true only
// when this call restarted a running containerd, so a caller reports the
// restart from Restarted, never Changed. Unloaded means the running
// containerd started before the drop-in's latest change, by this call or an
// earlier one that did not restart it.
type ApplyHandlersResult struct {
	Changed   bool
	Written   bool
	Restarted bool
	Unloaded  bool
}

// ApplyHandlers writes the runtime-handler drop-in for handlers and, with
// restart, restarts a running containerd when the drop-in changed, or when an
// earlier apply left it running without the drop-in on disk. A drop-in that
// changed on disk but could not be made durable is still recorded as
// unloaded, so a retry restarts containerd. Its caller runs
// RequireHandlersImport first, before anything it saves: a config rendered
// before runtime handlers existed would never read the file.
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

// unloadedPath records which runtime handlers the running containerd loaded
// when the drop-in changed without restarting it: that containerd's PID and
// processStart, then one handler name per line. containerd imports only *.toml from the
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

// processStart tells one run of a process from another that reuses its PID:
// the boot it ran in and its start time since that boot, from /proc. A reboot
// or a container restart can hand a new containerd the PID the record names;
// it never hands it the same start. Empty when /proc cannot be read.
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

// loadedHandlerNames reads the unloaded record. It counts only while the
// containerd it names still runs: any restart since, by `stratum runtime
// restart`, `stratum registry apply`, an upgrade, the supervisor or a reboot,
// loaded the drop-in on disk.
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
// on disk, only the runtime handlers in names. A record it cannot write leaves
// the node labelled for every declared handler, so it says so on w.
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

// clearUnloaded removes the record once a restart has loaded the drop-in. A
// record it cannot remove names the containerd the restart replaced, so
// loadedHandlerNames already ignores it.
func clearUnloaded(w io.Writer) {
	if err := os.Remove(unloadedPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(w, "WARN: could not remove %s (%v); it names the containerd this restart replaced, so it is already ignored\n", unloadedPath(), err)
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

// errNoHandlersImport is RequireHandlersImport's report of a containerd
// config that parses but does not import the runtime-handler drop-in: one
// rendered before runtime handlers existed. Only that is harmless on a node
// that declares none; a config that cannot be read or parsed stops
// containerd whatever it declares.
var errNoHandlersImport = errors.New("does not import the runtime-handler drop-in")

// RequireHandlersImport reports whether containerd's config imports the
// runtime-handler drop-in's directory after the shared conf.d.
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

// RestartContainerd restarts the supervisor's containerd and waits for it to
// report active again. Running containers keep running: their shims outlive
// containerd.
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

// CheckHandlers reports whether containerd will run the handlers this node
// saved: the drop-in matches them, each binary is executable, containerd's
// config imports the drop-in, CDI has specs to resolve, and which other
// conf.d files define the same runtime names. The shared conf.d is checked
// with no handlers declared too: a file there that does not parse stops
// containerd, and one that changes its default runtime changes every pod's.
// It changes nothing.
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

// RequireSharedConfDParses reports the first file in the shared conf.d that
// does not parse. containerd imports every one, so a runtime apply or restart
// that restarted it with one there would stop a working containerd for one
// that cannot start.
func RequireSharedConfDParses() error {
	if err := sharedConfDParseError(); err != nil {
		return fmt.Errorf("%w, so nothing was changed. Fix or remove it and run again", err)
	}
	return nil
}

// sharedConfDParseError is the first file in the shared conf.d that does not
// parse, as an error naming it, or nil.
func sharedConfDParseError() error {
	for _, f := range readSharedConfD() {
		if f.err != nil {
			return fmt.Errorf("%s does not parse (%v); containerd would refuse to start with it", f.path, f.err)
		}
	}
	return nil
}

// sharedConfDFile is one file in the shared conf.d as stratum reads it: the
// slice of its config that can contend with stratum's, or the error it does
// not parse with.
type sharedConfDFile struct {
	path string
	conf sharedConfD
	err  error
}

// readSharedConfD decodes every file in the shared conf.d, all of which
// containerd imports, in name order. RequireSharedConfDParses and
// checkSharedConfD both read it, so runtime apply and runtime check agree on
// which files do not parse.
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

// checkSharedConfD warns about every file in the shared conf.d that defines
// one of saved's runtime names, which `nvidia-ctk runtime configure` does, or
// changes containerd's default runtime. stratum's drop-in is imported after
// conf.d, so its definition of a name wins, but a changed default runs every
// pod without a runtimeClassName under that runtime. Every file there must
// parse, stratum's registry drop-in included, since containerd imports them
// all; the registry drop-in is stratum's own, so it is spared the warnings.
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

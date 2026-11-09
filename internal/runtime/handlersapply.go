package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

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
// restart, restarts a running containerd whose loaded handlers differ from
// them, or that cannot say which it loaded. Without restart such a containerd
// is reported Unloaded. Its caller runs RequireHandlersImport first, before
// anything it saves: a config rendered before runtime handlers existed would
// never read the file.
func ApplyHandlers(w io.Writer, handlers map[string]string, restart bool) (ApplyHandlersResult, error) {
	var out ApplyHandlersResult
	changed, err := writeHandlers(w, handlers)
	out.Changed = changed
	if err != nil {
		out.Unloaded = changed && Active()
		return out, err
	}
	out.Written = true
	if !Active() || (!changed && !handlersPending(handlers)) {
		return out, nil
	}
	if !restart {
		out.Unloaded = true
		return out, nil
	}
	restarted, err := restartContainerd(" after a runtime handler change")
	out.Restarted = restarted
	out.Unloaded = !restarted
	return out, err
}

// criTimeout bounds one CRI status call to the running containerd.
const criTimeout = 10 * time.Second

// criRuntimeHandlers is the named runtime handlers the running containerd
// reports through the CRI status the kubelet reads; the default handler has
// no name and is left out.
var criRuntimeHandlers = func(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, criTimeout)
	defer cancel()
	conn, err := grpc.NewClient(version.ContainerdSock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := runtimeapi.NewRuntimeServiceClient(conn).Status(ctx, &runtimeapi.StatusRequest{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, h := range resp.GetRuntimeHandlers() {
		if h.GetName() != "" {
			names = append(names, h.GetName())
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// loadedRuntimeHandlers is the runtime handlers the running containerd loaded.
// An error means it could not say, which every caller treats as a restart
// still pending.
func loadedRuntimeHandlers(ctx context.Context) ([]string, error) {
	if !Active() {
		return nil, errors.New("containerd is not running under the stratum supervisor")
	}
	names, err := criRuntimeHandlers(ctx)
	if err != nil {
		return nil, fmt.Errorf("ask containerd which runtime handlers it loaded: %w", err)
	}
	return names, nil
}

// handlersPending reports whether the running containerd needs a restart to
// run exactly handlers: one of them is not loaded, or it still runs a handler
// stratum declared before, one that is neither runc nor defined by another
// file in the shared conf.d. A containerd that cannot say is pending.
func handlersPending(handlers map[string]string) bool {
	loaded, err := loadedRuntimeHandlers(context.Background())
	if err != nil {
		return true
	}
	return len(missingHandlers(handlers, loaded)) > 0 || len(staleHandlers(handlers, loaded)) > 0
}

// missingHandlers is the declared handlers containerd has not loaded.
func missingHandlers(handlers map[string]string, loaded []string) []string {
	return slices.DeleteFunc(slices.Sorted(maps.Keys(handlers)), func(n string) bool { return slices.Contains(loaded, n) })
}

// staleHandlers is the loaded handlers that only an earlier drop-in can have
// declared: not declared now, not runc, and not defined by another file in
// the shared conf.d.
func staleHandlers(handlers map[string]string, loaded []string) []string {
	foreign := map[string]bool{"runc": true}
	for _, f := range readSharedConfD() {
		if f.err != nil {
			continue
		}
		for _, p := range f.conf.Plugins {
			for n := range p.Containerd.Runtimes {
				foreign[n] = true
			}
		}
	}
	return slices.DeleteFunc(slices.Clone(loaded), func(n string) bool {
		_, declared := handlers[n]
		return declared || foreign[n]
	})
}

// LoadedHandlers narrows names to the runtime handlers the running containerd
// has loaded, so a node is labelled for a handler only once its containerd
// can run it. An error means containerd could not say; the caller labels
// nothing new until it can.
func LoadedHandlers(ctx context.Context, names []string) ([]string, error) {
	loaded, err := loadedRuntimeHandlers(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(names), func(n string) bool { return !slices.Contains(loaded, n) }), nil
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

// checkLoaded reports the saved handlers the running containerd has not
// loaded and the ones it still runs that are no longer declared. A node whose
// containerd is not running has nothing loaded to compare.
func checkLoaded(r *HandlerReport, saved map[string]string) {
	if !Active() {
		return
	}
	loaded, err := loadedRuntimeHandlers(context.Background())
	if err != nil {
		r.add(SeverityWarn, "%v; cannot tell whether it runs the saved handlers", err)
		return
	}
	if missing := missingHandlers(saved, loaded); len(missing) > 0 {
		r.add(SeverityError, "containerd has not loaded the runtime handlers %s; run `stratum runtime restart`", strings.Join(missing, ", "))
	}
	if stale := staleHandlers(saved, loaded); len(stale) > 0 {
		r.add(SeverityWarn, "containerd still runs the runtime handlers %s, which this node no longer declares; run `stratum runtime restart`", strings.Join(stale, ", "))
	}
}

// RestartContainerd restarts the supervisor's containerd and waits for it to
// report active again. Running containers keep running: their shims outlive
// containerd.
func RestartContainerd(w io.Writer) error {
	if !Active() {
		return errors.New("containerd is not running under the stratum supervisor; start the node with `stratum up`")
	}
	_, err := restartContainerd("")
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
	checkLoaded(&r, saved)
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

// Package procid answers one question for every /proc scanner in the tree:
// which live process is Stratum's, and which component does it belong to?
//
// It owns the single /proc walk — one listing of procRoot yielding each live
// process's pid, argv and exe target, with one exited-process policy — and
// the per-component identity predicates built on it. The supervisor's
// legacy-handoff check, comps' liveness probes and staticpod's process
// scanners all walk through Walk and decide identity through these
// predicates, so a change to how a process is recognized happens in one
// place.
//
// This is a leaf package: it imports version's path constants and nothing
// else from the tree, so any package may import it without a cycle.
package procid

import (
	"debug/buildinfo"
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/ctrliq/stratum/internal/version"
)

// Process is one live process as the walk yields it. Exe is the /proc/<pid>/exe
// symlink's target, empty when it could not be read — the process may have
// exited between the cmdline read and the readlink, or the reader may not
// own the process. A predicate that needs Exe treats empty as "cannot
// confirm", never as a match.
type Process struct {
	PID  int
	Argv []string
	Exe  string
}

// readProcFile is os.ReadFile, indirected so a test can make a process exit
// mid-scan.
var readProcFile = os.ReadFile

// Exited reports a /proc read that failed because the process is gone: its
// directory was removed (ENOENT), or it is exiting and the kernel no longer
// serves its files (ESRCH). Gone is not an unreadable process table: such a
// read means skip, and every other error means the walk fails.
func Exited(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ESRCH)
}

// Walk visits every live process under procRoot, in directory-listing order.
// Entries that are not processes — non-numeric names, numeric names that are
// not directories, kernel threads and zombies with an empty cmdline — never
// reach visit. A process that exits between the listing and its cmdline read
// is skipped; any other cmdline read error fails the walk, because a caller
// deciding what is safe to stop must not read an unreadable table as an
// empty one. visit returns whether to keep walking: false stops the walk
// after the process it answered, an error aborts it.
func Walk(procRoot string, visit func(Process) (bool, error)) error {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return fmt.Errorf("read %s: %w", procRoot, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procRoot, entry.Name())
		raw, err := readProcFile(filepath.Join(dir, "cmdline"))
		if Exited(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s/cmdline: %w", dir, err)
		}
		if len(raw) == 0 {
			continue // a kernel thread or zombie has no argv
		}
		p := Process{PID: pid, Argv: strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")}
		// The kernel appends " (deleted)" to the target once the underlying
		// file is unlinked; the raw target is yielded, and the predicate that
		// compares it decides how to treat the marker.
		if target, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
			p.Exe = target
		}
		keep, err := visit(p)
		if err != nil {
			return fmt.Errorf("scan pid %d: %w", pid, err)
		}
		if !keep {
			return nil
		}
	}
	return nil
}

// BuildInfo reads the Go build information of the executable at path —
// normally /proc/<pid>/exe, whose symlink target the kernel keeps valid even
// after the binary is replaced. A valid native (non-Go) ELF image is not an
// error and returns nil: a native program may legitimately run an argument a
// scanner is matching on. A malformed or unreadable image is an error, so a
// caller deciding what to signal fails closed.
func BuildInfo(path string) (*buildinfo.BuildInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := buildinfo.Read(f)
	if err != nil {
		// Native Linux programs (e.g. a Python docker-compose up) cannot
		// host the Go CLI. Distinguish valid non-Go ELF images from unreadable
		// or malformed candidates, which must still fail closed.
		image, elfErr := elf.NewFile(f)
		if elfErr == nil && image.Section(".go.buildinfo") == nil && image.Section(".note.go.buildid") == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect candidate executable %s: %w", path, err)
	}
	return info, nil
}

// stratumModule is the module path a binary must be built from — as the main
// module, or as a dependency — for build information to identify it as
// Stratum's.
const stratumModule = "github.com/ctrliq/stratum"

// IsStratumBuild reports whether build information identifies a binary built
// from the Stratum module: as the main module, so a renamed binary keeps its
// identity, or as a dependency, so a distribution CLI embedding stratum still
// hosts it. A binary with no readable build information is not Stratum's.
func IsStratumBuild(info *buildinfo.BuildInfo) bool {
	if info == nil {
		return false
	}
	if info.Main.Path == stratumModule || strings.HasPrefix(info.Path, stratumModule+"/") {
		return true
	}
	for _, dep := range info.Deps {
		if dep.Path == stratumModule {
			return true
		}
	}
	return false
}

// isUpSubcommand is the one stratum-up scan: it reports whether args selects
// the `up` subcommand, following Cobra root command selection — leading flags
// with '=' consume one token; long flags without '=' and single-letter flags
// consume a value (the Stratum root has no persistent flags with optional
// values). It stops at the first command so a resource name or flag value
// passed to another Stratum command — for example, kubectl get pod up
// --watch — can never read as `up`. The scan runs from args[1]: argv[0] is
// the program name. IsUpCommand and IsStratumTopLevel both answer the
// subcommand question through this scan, so the two stratum-up rules cannot
// drift on it.
func isUpSubcommand(args []string) bool {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if strings.HasPrefix(arg, "-") {
			if !strings.Contains(arg, "=") && (strings.HasPrefix(arg, "--") || len(arg) == 2) {
				i++
			}
			continue
		}
		if arg != "" {
			return arg == "up"
		}
	}
	return false
}

// IsUpCommand reports whether args selects the `up` subcommand, through the
// one stratum-up scan isUpSubcommand performs. The legacy-supervisor handoff
// check pairs it with IsStratumBuild: any binary built from the Stratum
// module running up counts. That is the deliberate difference from
// IsStratumTopLevel, which shares this scan and then requires the process's
// exe to be this very binary — a difference about whose up it is, never
// about what running up means.
func IsUpCommand(args []string) bool {
	return isUpSubcommand(args)
}

// HasPayloadPath reports whether the process runs the component's staged
// payload binary: argv[0] must be exactly the binary under BinDir. A
// distribution-owned binary with the same basename, or anything else under
// BinDir, is not the component, and a caller deciding what to signal must
// refuse it.
func HasPayloadPath(component string, p Process) bool {
	return len(p.Argv) > 0 && p.Argv[0] == filepath.Join(version.BinDir, component)
}

// IsOperator reports whether the process is the stratum operator, by its flag
// shape rather than its executable name, which may be a release artifact (for
// example stratum-linux-amd64): argv[0]'s basename is not a stable identity.
// Its kubeconfig is super-admin.conf, or admin.conf for a process an earlier
// release started.
// Every Stratum-owned flag must be present, in any order; any program without
// the full shape — another program's operator, another subcommand, or the
// pkill command itself — is refused. OperatorCommandLinePattern in comps is
// this predicate as a `pkill -f` regular expression, pinned to it by a test.
func IsOperator(p Process) bool {
	args := p.Argv
	if len(args) < 2 || args[0] == "" || args[1] != "operator" {
		return false
	}
	return (hasArgPair(args, "--kubeconfig", version.SuperAdminConf()) || hasArgPair(args, "--kubeconfig", version.AdminConf)) &&
		hasArgPair(args, "--manifests-dir", version.ManifestsDir) &&
		hasArgPair(args, "--helm-cache", version.HelmCacheDir) &&
		hasArg(args, "--leader-elect")
}

// IsStratumTopLevel reports whether the process is the top-level `stratum up`
// process — the one a human or a systemd unit runs, parent of the supervisor
// that runs containerd/kubelet/operator in-process. Unlike containerd/kubelet,
// there is no staged path to match against: this binary can be running from
// anywhere (there is no equivalent of version.BinDir for it), and, like the
// operator, may be named after a release artifact rather than "stratum".
//
// The up subcommand is decided by the same flag-skipping scan IsUpCommand
// runs (isUpSubcommand), which rules out this same binary's own "stratum
// uninstall" invocation, running as this very call — but is not identity by
// itself, since a distro-owned or third-party program with an "up"
// subcommand would match it too. What makes this specific is
// /proc/<pid>/exe resolving to the exact same file `stratum uninstall` itself
// is running from (os.Executable()): the two are normally the same binary. An
// unresolvable exe is not a match: better to leave a process alone than to
// signal one this call could not actually confirm.
//
// The kernel appends " (deleted)" to that readlink's target once the
// underlying file is unlinked — an upgrade replacing the binary out from
// under a still-running `stratum up`, for example — so both sides are
// compared with that suffix stripped, or a routine upgrade would make this
// stop matching the exact process it most needs to find.
func IsStratumTopLevel(p Process) bool {
	if len(p.Argv) < 2 || p.Argv[0] == "" || !isUpSubcommand(p.Argv) {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	return trimDeletedSuffix(p.Exe) == trimDeletedSuffix(self)
}

// IsRuntimeShim reports whether the process is a runtime-v2 shim serving
// Stratum's containerd. Shims outlive containerd and are attributed by the
// runtime socket, not their executable path (a distro can install the same
// binary), and only in either address form the shim accepts.
func IsRuntimeShim(p Process) bool {
	if len(p.Argv) == 0 {
		return false
	}
	sock := strings.TrimPrefix(version.ContainerdSock, "unix://")
	return strings.HasPrefix(filepath.Base(p.Argv[0]), "containerd-shim-") &&
		(hasArgPair(p.Argv, "-address", sock) || hasArg(p.Argv, "-address="+sock))
}

// HasArgv0Basename reports whether the process runs the component by argv[0]
// basename alone, whatever path it runs from. This is the static-pod
// scanners' deliberately loose rule: the component runs as a container image's
// entrypoint with no staged host path, and a distro-owned same-named binary is
// still that component running on this node. A different basename is not.
func HasArgv0Basename(component string, p Process) bool {
	return len(p.Argv) > 0 && filepath.Base(p.Argv[0]) == component
}

// trimDeletedSuffix strips the " (deleted)" marker the kernel appends to
// /proc/<pid>/exe's readlink target once the underlying file is unlinked.
func trimDeletedSuffix(path string) string {
	return strings.TrimSuffix(path, " (deleted)")
}

func hasArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

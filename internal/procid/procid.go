// Package procid identifies Stratum's processes among the live process table.
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

// Exe is empty when the readlink failed; treat that as unconfirmed, never a match.
type Process struct {
	PID  int
	Argv []string
	Exe  string
}

// readProcFile is os.ReadFile, indirected so a test can make a process exit mid-scan.
var readProcFile = os.ReadFile

// Exited reports a read failed because the process is gone, not an unreadable table.
func Exited(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ESRCH)
}

// Walk visits every live process; an unreadable cmdline fails the walk, never reads as empty.
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

// BuildInfo reads Go build info; a valid native ELF returns nil, a malformed one is an error.
func BuildInfo(path string) (*buildinfo.BuildInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := buildinfo.Read(f)
	if err != nil {
		image, elfErr := elf.NewFile(f)
		if elfErr == nil && image.Section(".go.buildinfo") == nil && image.Section(".note.go.buildid") == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect candidate executable %s: %w", path, err)
	}
	return info, nil
}

const stratumModule = "github.com/ctrliq/stratum"

// IsStratumBuild reports whether a binary was built from the Stratum module.
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

// isUpSubcommand follows Cobra root selection: first command decides; no optional-value root flags.
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

// IsUpCommand reports whether args selects up, whatever binary; the handoff adds IsStratumBuild.
func IsUpCommand(args []string) bool {
	return isUpSubcommand(args)
}

// HasPayloadPath reports whether argv[0] is exactly the staged binary under BinDir.
func HasPayloadPath(component string, p Process) bool {
	return len(p.Argv) > 0 && p.Argv[0] == filepath.Join(version.BinDir, component)
}

// IsOperator identifies the operator by flag shape; its exe name may be a release artifact.
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

// IsStratumTopLevel reports whether the process is this exact binary running up.
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

// IsRuntimeShim matches by the runtime socket, not the exe path, which a distro can install.
func IsRuntimeShim(p Process) bool {
	if len(p.Argv) == 0 {
		return false
	}
	sock := strings.TrimPrefix(version.ContainerdSock, "unix://")
	return strings.HasPrefix(filepath.Base(p.Argv[0]), "containerd-shim-") &&
		(hasArgPair(p.Argv, "-address", sock) || hasArg(p.Argv, "-address="+sock))
}

// HasArgv0Basename matches by argv[0] basename alone; a distro binary is still the component.
func HasArgv0Basename(component string, p Process) bool {
	return len(p.Argv) > 0 && filepath.Base(p.Argv[0]) == component
}

// The kernel appends " (deleted)" to /proc/<pid>/exe once the file is unlinked.
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

package runtime

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/version"
)

// handlersMarker marks the drop-in as stratum-written so only stratum files are removed.
const handlersMarker = version.GeneratedMarker + " (source: spec.node.runtimeHandlers)\n"

// confdImportGlob: the registry drop-in depends on this import in containerd's default config.
const confdImportGlob = "/etc/containerd/conf.d/*.toml"

// handlersImportGlob is the stratum-owned import, listed after confdImportGlob so its handlers win.
func handlersImportGlob() string {
	return filepath.Join(filepath.Dir(version.ContainerdRuntimeConfD), "*.toml")
}

// containerd migrates an import with no version line on every start.
const handlersConfigVersion = 4

// cdiSpecDirs are containerd's defaults, pinned so a later default cannot move them.
var cdiSpecDirs = []string{"/etc/cdi", "/var/run/cdi"}

// The path renders into a TOML literal containerd executes, so it is refused, not escaped.
var handlerPathChars = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// ValidateHandler checks name and path without looking at the host; CheckHandlerBinary does that.
func ValidateHandler(name, path string) error {
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("runtime handler name %q: %s", name, strings.Join(errs, "; "))
	}
	if name == "runc" {
		return fmt.Errorf("runtime handler name %q is containerd's default handler and cannot be redeclared", name)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("runtime handler %s: path %q must be absolute and clean", name, path)
	}
	if !handlerPathChars.MatchString(path) {
		return fmt.Errorf("runtime handler %s: path %q may contain only letters, digits and . _ / + -", name, path)
	}
	return nil
}

// CheckHandlerBinary reports whether path is an executable regular file on this host.
func CheckHandlerBinary(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", path)
	}
	return nil
}

func renderHandlers(handlers map[string]string, cgroup string) string {
	var b strings.Builder
	b.WriteString(handlersMarker)
	fmt.Fprintf(&b, "version = %d\n\n", handlersConfigVersion)
	fmt.Fprintf(&b, "[plugins.'io.containerd.cri.v1.runtime']\n  enable_cdi = true\n  cdi_spec_dirs = ['%s']\n",
		strings.Join(cdiSpecDirs, "', '"))
	for _, name := range slices.Sorted(maps.Keys(handlers)) {
		table := "plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes." + name
		fmt.Fprintf(&b, "\n[%s]\n  runtime_type = 'io.containerd.runc.v2'\n  sandboxer = 'podsandbox'\n\n  [%s.options]\n    BinaryName = '%s'\n    SystemdCgroup = %t\n",
			table, table, handlers[name], cgroup == "systemd")
	}
	return b.String()
}

type handlerConfig struct {
	Version int `toml:"version"`
	Plugins map[string]struct {
		EnableCDI   bool     `toml:"enable_cdi"`
		CDISpecDirs []string `toml:"cdi_spec_dirs"`
		Containerd  struct {
			Runtimes map[string]struct {
				RuntimeType string `toml:"runtime_type"`
				Sandboxer   string `toml:"sandboxer"`
				Options     struct {
					BinaryName    string `toml:"BinaryName"`
					SystemdCgroup bool   `toml:"SystemdCgroup"`
				} `toml:"options"`
			} `toml:"runtimes"`
		} `toml:"containerd"`
	} `toml:"plugins"`
}

func verifyHandlers(rendered string, handlers map[string]string, cgroup string) error {
	var c handlerConfig
	if err := toml.Unmarshal([]byte(rendered), &c); err != nil {
		return fmt.Errorf("rendered runtime handlers do not parse: %w", err)
	}
	if c.Version != handlersConfigVersion {
		return fmt.Errorf("rendered runtime handlers are config version %d, want %d", c.Version, handlersConfigVersion)
	}
	cri, ok := c.Plugins["io.containerd.cri.v1.runtime"]
	if !ok || !cri.EnableCDI || !slices.Equal(cri.CDISpecDirs, cdiSpecDirs) {
		return errors.New("rendered runtime handlers do not enable CDI on io.containerd.cri.v1.runtime")
	}
	if len(cri.Containerd.Runtimes) != len(handlers) {
		return fmt.Errorf("rendered runtime handlers declare %d runtimes, want %d", len(cri.Containerd.Runtimes), len(handlers))
	}
	for name, path := range handlers {
		rt, ok := cri.Containerd.Runtimes[name]
		switch {
		case !ok:
			return fmt.Errorf("rendered runtime handlers do not declare %s", name)
		case rt.RuntimeType != "io.containerd.runc.v2" || rt.Sandboxer != "podsandbox" || rt.Options.BinaryName != path:
			return fmt.Errorf("rendered runtime handler %s does not run %s through the runc v2 shim", name, path)
		case rt.Options.SystemdCgroup != (cgroup == "systemd"):
			return fmt.Errorf("rendered runtime handler %s does not use the %s cgroup driver", name, cgroup)
		}
	}
	return nil
}

// writeHandlers writes a handler whose binary is missing anyway: other pods must keep running.
func writeHandlers(w io.Writer, handlers map[string]string) (bool, error) {
	path := version.ContainerdRuntimeConfD
	prev, prevErr := os.ReadFile(path)
	if prevErr != nil && !errors.Is(prevErr, fs.ErrNotExist) {
		return false, prevErr
	}
	if len(handlers) == 0 {
		if prevErr != nil || !strings.HasPrefix(string(prev), handlersMarker) {
			return false, nil
		}
		if err := os.Remove(path); err != nil {
			return false, err
		}
		return true, nil
	}
	for _, name := range slices.Sorted(maps.Keys(handlers)) {
		if err := ValidateHandler(name, handlers[name]); err != nil {
			return false, err
		}
		if err := CheckHandlerBinary(handlers[name]); err != nil {
			fmt.Fprintf(w, "  WARN: runtime handler %s: %v; pods using it will not start until the binary is installed\n", name, err)
		}
	}
	cgroup := CgroupDriver()
	rendered := renderHandlers(handlers, cgroup)
	if err := verifyHandlers(rendered, handlers, cgroup); err != nil {
		return false, err
	}
	if prevErr == nil && string(prev) == rendered {
		return false, nil
	}
	if prevErr == nil && !strings.HasPrefix(string(prev), handlersMarker) {
		fmt.Fprintf(w, "  WARN: %s was not written by stratum; replacing it with the runtime handlers this node declares\n", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := atomicfile.WriteFileWith(path, []byte(rendered), 0o644, atomicfileSeams()); err != nil {
		return errors.Is(err, atomicfile.ErrNotDurable), err
	}
	return true, nil
}

// RequireOwnDropIn refuses to replace a file stratum did not write; an operator may own it.
func RequireOwnDropIn(handlers map[string]string) error {
	if len(handlers) == 0 {
		return nil
	}
	path := version.ContainerdRuntimeConfD
	prev, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(prev), handlersMarker) {
		return fmt.Errorf("%s was not written by stratum, and writing the runtime handlers would replace it; move it aside and run again", path)
	}
	return nil
}

func configImports(cfg string) ([]string, error) {
	var c struct {
		Imports []string `toml:"imports"`
	}
	if err := toml.Unmarshal([]byte(cfg), &c); err != nil {
		return nil, err
	}
	return c.Imports, nil
}

// importsDropIns treats an unparseable config as not current.
func importsDropIns(cfg string) bool {
	imports, err := configImports(cfg)
	if err != nil {
		return false
	}
	shared, own := slices.Index(imports, confdImportGlob), slices.Index(imports, handlersImportGlob())
	return shared >= 0 && own > shared
}

// importsLine matches the top-level imports array `containerd config default` renders.
var importsLine = regexp.MustCompile(`(?m)^imports = \[(.*)\]$`)

func addHandlersImport(cfg string) string {
	return importsLine.ReplaceAllStringFunc(cfg, func(line string) string {
		inner := importsLine.FindStringSubmatch(line)[1]
		own := "'" + handlersImportGlob() + "'"
		if strings.TrimSpace(inner) == "" {
			return "imports = [" + own + "]"
		}
		return "imports = [" + inner + ", " + own + "]"
	})
}

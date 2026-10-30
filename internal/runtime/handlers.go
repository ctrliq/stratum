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

// handlersMarker opens the runtime-handler drop-in, so removing the last
// handler deletes only a file stratum wrote.
const handlersMarker = version.GeneratedMarker + " (source: spec.node.runtimeHandlers)\n"

// confdImportGlob is the import containerd's default config carries and both
// stratum drop-ins (registry mirrors, runtime handlers) depend on.
const confdImportGlob = "/etc/containerd/conf.d/*.toml"

// handlersConfigVersion is the containerd config version the drop-in is
// written in. containerd migrates an import by its own version, and one
// without a version line is read as version 0 and migrated on every start.
const handlersConfigVersion = 4

// cdiSpecDirs are containerd's default CDI spec directories, pinned in the
// drop-in so a device plugin's CDI specs keep resolving if a later containerd
// default changes them.
var cdiSpecDirs = []string{"/etc/cdi", "/var/run/cdi"}

// handlerPathChars is the character set a handler binary path may use. The
// path renders into a single-quoted TOML literal and is executed by
// containerd, so anything outside it is refused rather than escaped.
var handlerPathChars = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// ValidateHandler reports whether name and path can be declared as a
// containerd runtime handler: name a DNS-1123 label other than runc (the
// default handler containerd already defines), path absolute, clean and free
// of shell and TOML metacharacters. It does not look at the host; CheckHandlerBinary does.
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

// CheckHandlerBinary reports whether path is an executable regular file on
// this host. A join refuses a handler that fails it; a restart only warns,
// since the binary may be mid-reinstall and the node's other pods must keep
// running.
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

// renderHandlers renders the conf.d drop-in declaring each handler as a runc
// v2 shim running its own binary in a pod sandbox like runc's, with the
// cgroup driver kubelet and containerd share, and CDI on so device plugins can
// inject devices.
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

// handlerConfig is the slice of containerd's config the drop-in sets, read the
// way containerd reads it.
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

// verifyHandlers parses a rendered drop-in and checks that every handler lands
// on the table path containerd reads, so a render that drifts out of its
// table fails here instead of leaving pods with an unknown handler.
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

// writeHandlers writes the runtime-handler drop-in and reports whether it
// changed the file, which only takes effect when containerd restarts. With no
// handlers it removes a drop-in stratum wrote and leaves any other file at
// that path alone. A handler whose binary is missing is written anyway and
// reported to w: the node's other pods must keep running, and a join has
// already refused it.
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := atomicfile.WriteFileWith(path, []byte(rendered), 0o644, atomicfileSeams()); err != nil {
		return errors.Is(err, atomicfile.ErrNotDurable), err
	}
	return true, nil
}

// importsConfD reports whether a containerd config imports conf.d, read as
// containerd reads it.
func importsConfD(cfg string) bool {
	var c struct {
		Imports []string `toml:"imports"`
	}
	if err := toml.Unmarshal([]byte(cfg), &c); err != nil {
		return false
	}
	return slices.Contains(c.Imports, confdImportGlob)
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	// The Go linker does not report an -X flag whose variable is missing.
	makefile   = "../../Makefile"
	modulePath = "github.com/ctrliq/stratum/"
	moduleRoot = "../.."
)

var xFlag = regexp.MustCompile(`-X[ \t]+([A-Za-z0-9_./-]+)\.([A-Za-z_][A-Za-z0-9_]*)=`)

// -X applies only to string vars with a constant or absent initializer.
func packageVars(t *testing.T, dir string) map[string]bool {
	t.Helper()
	// Match the bare binary linked below, GOOS/GOARCH, CGO and GOFLAGS included.
	cmd := exec.Command("go", "list", "-tags", "noui", "-deps", "-export", "-json", ".")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("listing build-selected files in %s: %v\n%s", dir, err, &stderr)
	}
	exports := make(map[string]string)
	var target string
	var filenames []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg struct {
			ImportPath string
			Export     string
			DepOnly    bool
			Dir        string
			GoFiles    []string
			CgoFiles   []string
		}
		if err := dec.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[pkg.ImportPath] = pkg.Export
		if !pkg.DepOnly {
			target = pkg.ImportPath
			for _, name := range append(pkg.GoFiles, pkg.CgoFiles...) {
				filenames = append(filenames, filepath.Join(pkg.Dir, name))
			}
		}
	}
	if target == "" {
		t.Fatalf("no package found in %s", dir)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range filenames {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files = append(files, file)
	}
	info := &types.Info{Defs: make(map[*ast.Ident]types.Object), Types: make(map[ast.Expr]types.TypeAndValue)}
	config := types.Config{
		IgnoreFuncBodies: true,
		Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			return os.Open(exports[path])
		}),
	}
	if _, err := config.Check(target, fset, files, info); err != nil {
		t.Fatalf("type-checking stamp initializers in %s: %v", dir, err)
	}
	vars := make(map[string]bool)
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					// No IsExported filter: the linker addresses vars by name.
					obj, ok := info.Defs[name].(*types.Var)
					stampable := ok && types.Identical(obj.Type(), types.Typ[types.String])
					if len(vs.Values) > 0 {
						// One tuple expression initializes every name.
						stampable = stampable && i < len(vs.Values)
						if stampable {
							value := info.Types[vs.Values[i]].Value
							stampable = value != nil && value.Kind() == constant.String
						}
					}
					vars[name.Name] = stampable
				}
			}
		}
	}
	return vars
}

// Source declarations cannot prove the assembled -X flags reach the artifact.
func TestLinkStampsReachTheRunningBinary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	printFlags := filepath.Join(dir, "flags.mk")
	if err := os.WriteFile(printFlags, []byte(".PHONY: guard-link-flags\nguard-link-flags:\n\t@printf '%s\\n' '$(GO_LDFLAGS)'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "make", "--no-print-directory", "-s", "-f", "Makefile", "-f", printFlags,
		"guard-link-flags", "HOST_ARCH="+runtime.GOARCH, "VERSION=guard-release", "BUILD_ID=guard-build",
		"kubernetes_version=1.91.1", "etcd_version=3.91.2", "containerd_version=2.91.3", "runc_version=1.91.4")
	cmd.Dir = moduleRoot
	flags, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expand Makefile link flags: %v\n%s", err, flags)
	}
	binary := filepath.Join(dir, "stratum")
	cmd = exec.CommandContext(ctx, "go", "build", "-tags", "noui", "-ldflags", strings.TrimSpace(string(flags)), "-o", binary, "./cmd/stratum")
	cmd.Dir = moduleRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stamped binary: %v\n%s", err, out)
	}
	cmd = exec.CommandContext(ctx, binary, "version", "--components")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read stamped versions: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "guard-release ") {
		t.Errorf("release stamp missing from version output:\n%s", out)
	}
	components := make(map[string]string)
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			components[fields[0]] = fields[1]
		}
	}
	// Their expectation is the image list's own version, the payload's surface.
	for name, want := range map[string]string{
		"kubernetes": "1.91.1", "etcd": "3.91.2", "containerd": "2.91.3", "runc": "1.91.4",
		"kube-vip": imageListVersion(t, "kube-vip"), "cilium": imageListVersion(t, "cilium"),
		"coredns": imageListVersion(t, "coredns"),
	} {
		if got := components[name]; got != want {
			t.Errorf("%s version = %q, want link stamp %q", name, got, want)
		}
	}
	cmd = exec.CommandContext(ctx, binary, "version", "--build")
	out, err = cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "guard-build" {
		t.Errorf("build ID = %q, error %v; want guard-build", out, err)
	}
}

// An importer links the published asset in place of GO_LDFLAGS, less BuildID.
func TestEngineLdflagsAssetIsGoLdflagsLessBuildID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	printFlags := filepath.Join(dir, "flags.mk")
	if err := os.WriteFile(printFlags, []byte(".PHONY: guard-link-flags\nguard-link-flags:\n\t@printf '%s\\n' '$(GO_LDFLAGS)'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "make", "--no-print-directory", "-s", "-f", "Makefile", "-f", printFlags,
		"guard-link-flags", "engine-ldflags", "DIST="+dir, "HOST_ARCH="+runtime.GOARCH, "VERSION=guard-release", "BUILD_ID=guard-build")
	cmd.Dir = moduleRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("expand Makefile link flags: %v", err)
	}
	var want []string
	for _, m := range regexp.MustCompile(`-X[ \t]+(\S+)`).FindAllStringSubmatch(string(out), -1) {
		if !strings.Contains(m[1], "version.BuildID=") {
			want = append(want, "-X "+m[1])
		}
	}
	asset, err := os.ReadFile(filepath.Join(dir, "stratum-ldflags.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(asset)), "\n")
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("engine-ldflags asset:\n%s\nwant GO_LDFLAGS' stamps less BuildID:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(want) < 10 {
		t.Errorf("only %d stamps parsed from GO_LDFLAGS; the comparison proves nothing", len(want))
	}
}

func imageListVersion(t *testing.T, image string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot, "build", "images.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "/"+image+":") {
			continue
		}
		tag := line
		if i := strings.Index(tag, "@"); i >= 0 {
			tag = tag[:i]
		}
		if i := strings.LastIndex(tag, ":"); i >= 0 {
			tag = tag[i+1:]
		}
		if image == "kube-vip" || image == "cilium" {
			tag = strings.TrimPrefix(tag, "v")
		}
		return tag
	}
	t.Fatalf("build/images.txt names no %s image", image)
	return ""
}

func TestPackageVarsUsesBuildSelectionAndConstantValues(t *testing.T) {
	dir := t.TempDir()
	put := func(name, source string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("go.mod", "module example.test/stamps\n\ngo 1.26.0\n")
	put("vars.go", `package stamps
import "runtime"
const defaultVersion = "dev"
type Alias = string
type Named string
var Literal = "dev"
var Constant = defaultVersion
var Imported = runtime.GOOS
var Concat = (defaultVersion + "-build")
var Zero string
var Aliased Alias = defaultVersion
var Defined Named = defaultVersion
var Called = runtime.Version()
var Copied = Called
var First, Second string = pair()
var One, Two = "one", defaultVersion
var Number = 1
func pair() (string, string) { return "first", "second" }
`)
	put("excluded.go", "//go:build guard_excluded\n\npackage stamps\nvar Excluded = \"dev\"\n")
	otherOS := "windows"
	if runtime.GOOS == otherOS {
		otherOS = "linux"
	}
	put("vars_"+otherOS+".go", "package stamps\nvar OtherOS = \"dev\"\n")
	put("vars_noui.go", "//go:build noui\n\npackage stamps\nvar Bare = \"dev\"\n")
	put("vars_test.go", "package stamps\nvar TestOnly = \"dev\"\n")
	got := packageVars(t, dir)
	for name, want := range map[string]bool{
		"Literal": true, "Constant": true, "Imported": true, "Concat": true,
		"Zero": true, "Aliased": true, "One": true, "Two": true, "Bare": true,
		"Defined": false, "Called": false, "Copied": false, "First": false,
		"Second": false, "Number": false,
	} {
		if actual, ok := got[name]; !ok || actual != want {
			t.Errorf("stampable %s = %v (present %v), want %v", name, actual, ok, want)
		}
	}
	for _, name := range []string{"Excluded", "OtherOS", "TestOnly"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s was included even though the binary does not build its file", name)
		}
	}
}

func TestLinkStampsNameRealVariables(t *testing.T) {
	raw, err := os.ReadFile(makefile)
	if err != nil {
		t.Fatal(err)
	}
	matches := xFlag.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatalf("parsed no -X flags from %s — the flag grammar no longer matches the Makefile", makefile)
	}

	// Remember which packages the Makefile addresses, so a parsing regression
	checked := make(map[string]map[string]bool)
	touched := make(map[string]bool)
	for _, m := range matches {
		pkgPath, name := m[1], m[2]
		if pkgPath == "main" {
			t.Errorf("-X main.%s= stamps a main-package variable — the component stamps live in internal/version; stamp it there", name)
			continue
		}
		var dir string
		if rel, ok := strings.CutPrefix(pkgPath, modulePath); ok {
			dir = filepath.Join(moduleRoot, filepath.FromSlash(rel))
		} else {
			t.Errorf("-X %s.%s= names a package outside this module", pkgPath, name)
			continue
		}
		vars, ok := checked[dir]
		if !ok {
			vars = packageVars(t, dir)
			checked[dir] = vars
		}
		touched[dir] = true
		if !vars[name] {
			names := make([]string, 0, len(vars))
			for n := range vars {
				names = append(names, n)
			}
			sort.Strings(names)
			t.Errorf("-X %s.%s= names no linker-stampable string variable of %s (stamped vars there: %s)",
				pkgPath, name, dir, strings.Join(names, ", "))
		}
	}
	for _, mustTouch := range []string{
		filepath.Join(moduleRoot, "internal", "version"),
		filepath.Join(moduleRoot, "internal", "addons"),
		filepath.Join(moduleRoot, "internal", "runtime"),
	} {
		if !touched[mustTouch] {
			t.Errorf("the Makefile's -X flags no longer reach %s — the check may be parsing a stale flag list", mustTouch)
		}
	}
}

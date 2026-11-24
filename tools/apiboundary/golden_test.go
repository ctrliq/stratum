package main

import (
	"flag"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// An independent fixture the golden cannot update: compilation only, not runtime or compatibility.
func TestReleasedPkgCLIConsumerStillCompiles(t *testing.T) {
	cmd := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "consumer.a"),
		filepath.Join("testdata", "released_consumer.go"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the released pkg/cli consumer no longer compiles: %v\n%s", err, out)
	}
}

var updateGolden = flag.Bool("update", false,
	"rewrite testdata/pkgcli.api from pkg/cli's current exported API")

func pathQualifier(p *types.Package) string { return p.Path() }

// Structs hide private fields behind an opaque marker: presence, not shape.
func goldenLines(pkg *types.Package) []string {
	scope := pkg.Scope()
	var lines []string
	for _, name := range scope.Names() {
		if !token.IsExported(name) {
			continue
		}
		obj := scope.Lookup(name)
		line := types.ObjectString(obj, pathQualifier)
		if c, ok := obj.(*types.Const); ok {
			line += " = " + c.Val().ExactString()
		}
		if tn, ok := obj.(*types.TypeName); ok && !tn.IsAlias() {
			if st, ok := tn.Type().Underlying().(*types.Struct); ok {
				var fields []string
				private := false
				for i := 0; i < st.NumFields(); i++ {
					f := st.Field(i)
					if !f.Exported() {
						private = true
						continue
					}
					field := types.TypeString(f.Type(), pathQualifier)
					if !f.Embedded() {
						field = f.Name() + " " + field
					}
					if tag := st.Tag(i); tag != "" {
						field += " " + strconv.Quote(tag)
					}
					fields = append(fields, field)
				}
				if private {
					// Their presence prevents unkeyed struct literals.
					fields = append(fields, "<private>")
				}
				prefix, _, _ := strings.Cut(line, "struct{")
				line = prefix + "struct{" + strings.Join(fields, "; ") + "}"
			}
		}
		lines = append(lines, line)
		if tn, ok := obj.(*types.TypeName); ok && !tn.IsAlias() {
			if named, ok := tn.Type().(*types.Named); ok {
				for i := 0; i < named.NumMethods(); i++ {
					if m := named.Method(i); m.Exported() {
						lines = append(lines, types.ObjectString(m, pathQualifier))
					}
				}
			}
		}
	}
	sort.Strings(lines)
	return lines
}

func TestGoldenPinsImportPathsWithTheSamePackageName(t *testing.T) {
	lines := func(path string) string {
		dep := types.NewPackage(path, "shared")
		typ := types.NewNamed(types.NewTypeName(token.NoPos, dep, "Value", nil), types.Typ[types.Int], nil)
		pkg := types.NewPackage("example.test/api", "api")
		param := types.NewVar(token.NoPos, pkg, "value", typ)
		sig := types.NewSignatureType(nil, nil, nil, types.NewTuple(param), nil, false)
		pkg.Scope().Insert(types.NewFunc(token.NoPos, pkg, "Use", sig))
		return strings.Join(goldenLines(pkg), "\n")
	}
	before, after := lines("example.test/first"), lines("example.test/second")
	if before == after || !strings.Contains(before, "example.test/first.Value") || !strings.Contains(after, "example.test/second.Value") {
		t.Fatalf("changing the import path did not change the pinned type identity:\n%s\n%s", before, after)
	}
}

func TestGoldenPinsConstantValues(t *testing.T) {
	lines := func(value int64) string {
		pkg := types.NewPackage("example.test/api", "api")
		pkg.Scope().Insert(types.NewConst(token.NoPos, pkg, "TierRead", types.Typ[types.Int], constant.MakeInt64(value)))
		return strings.Join(goldenLines(pkg), "\n")
	}
	before, after := lines(0), lines(1)
	if before == after || !strings.HasSuffix(before, " = 0") || !strings.HasSuffix(after, " = 1") {
		t.Fatalf("changing a constant value did not move the golden:\n%s\n%s", before, after)
	}
}

func TestGoldenIgnoresPrivateFieldsAndPinsPublicFields(t *testing.T) {
	lines := func(source string) string {
		t.Helper()
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "api.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		config := types.Config{}
		pkg, err := config.Check("example.test/api", fset, []*ast.File{file}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(goldenLines(pkg), "\n")
	}
	before := lines("package api; type Public struct { Value string; cache int }")
	privateChanged := lines("package api; type Public struct { Value string; state map[string]bool; extra int }")
	if before != privateChanged {
		t.Errorf("private implementation change moved the golden:\n%s\n%s", before, privateChanged)
	}
	for _, source := range []string{
		"package api; type Public struct { Value int; cache int }",
		"package api; type Public struct { Renamed string; cache int }",
		"package api; type Public struct { Value string }",
	} {
		if after := lines(source); before == after {
			t.Errorf("consumer-visible struct change did not move the golden: %s", source)
		}
	}
}

func TestPkgCLIExportedAPIMatchesTheGolden(t *testing.T) {
	pkg, _, _, err := loadExportedTypes(filepath.Join("..", "..", "pkg", "cli"))
	if err != nil {
		t.Fatalf("loading pkg/cli's exported types: %v", err)
	}
	lines := goldenLines(pkg)

	goldenPath := filepath.Join("testdata", "pkgcli.api")
	if *updateGolden {
		body := "# Exported API of github.com/ctrliq/stratum/pkg/cli, one declaration per line.\n" +
			"# Regenerate with: go test ./tools/apiboundary/ -run TestPkgCLIExportedAPIMatchesTheGolden -update\n" +
			strings.Join(lines, "\n") + "\n"
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("reading the golden: %v (regenerate with -update)", err)
	}
	var want []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			want = append(want, line)
		}
	}

	var missing, unexpected []string
	wanted := make(map[string]bool, len(want))
	for _, w := range want {
		wanted[w] = true
	}
	for _, l := range lines {
		if !wanted[l] {
			unexpected = append(unexpected, l)
		}
	}
	for _, w := range want {
		found := false
		for _, l := range lines {
			if l == w {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 || len(unexpected) > 0 {
		t.Errorf("pkg/cli's exported API does not match the golden (testdata/pkgcli.api).\n"+
			"missing from the package:\n  %s\n"+
			"not in the golden:\n  %s\n"+
			"If the change is deliberate, regenerate the golden with:\n"+
			"  go test ./tools/apiboundary/ -run TestPkgCLIExportedAPIMatchesTheGolden -update",
			strings.Join(missing, "\n  "), strings.Join(unexpected, "\n  "))
	}
}

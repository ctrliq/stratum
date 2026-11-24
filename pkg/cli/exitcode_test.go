package cli

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// Pins the sentinel-to-exit-code mapping and where the final error line is written.
func TestExitCode(t *testing.T) {
	usageErr := &ErrUsage{err: errors.New("unknown flag: --bogus")}
	for _, tc := range []struct {
		name     string
		err      error
		wantCode int
		// The exact text the writer must carry; empty when the sentinel already reported.
		wantLine string
	}{
		{
			name:     "a nil error is success",
			err:      nil,
			wantCode: 0,
		},
		{
			name:     "ErrNoCommand exits 2 without a stratum line",
			err:      ErrNoCommand,
			wantCode: 2,
		},
		{
			name:     "a wrapped ErrNoCommand is seen through errors.Is",
			err:      fmt.Errorf("dispatch: %w", ErrNoCommand),
			wantCode: 2,
		},
		{
			name:     "ErrUsage exits 2 without a stratum line",
			err:      usageErr,
			wantCode: 2,
		},
		{
			name:     "a wrapped ErrUsage is seen through errors.As",
			err:      fmt.Errorf("up: %w", usageErr),
			wantCode: 2,
		},
		{
			name:     "ErrCheckFailed exits 1 without a stratum line",
			err:      ErrCheckFailed,
			wantCode: 1,
		},
		{
			name:     "a wrapped ErrCheckFailed is seen through errors.Is",
			err:      fmt.Errorf("audit: %w", ErrCheckFailed),
			wantCode: 1,
		},
		{
			name:     "ErrExitCode propagates its own code",
			err:      &ErrExitCode{Code: 7},
			wantCode: 7,
		},
		{
			name:     "a wrapped ErrExitCode is seen through errors.As",
			err:      fmt.Errorf("kubectl: %w", &ErrExitCode{Code: 7}),
			wantCode: 7,
		},
		{
			name:     "an ErrExitCode with code 0 is success",
			err:      &ErrExitCode{Code: 0},
			wantCode: 0,
		},
		{
			name:     "any other error exits 1 with a stratum line",
			err:      errors.New("disk full"),
			wantCode: 1,
			wantLine: "stratum: disk full\n",
		},
		{
			name:     "a wrapped plain error reports the whole chain",
			err:      fmt.Errorf("up: %w", errors.New("disk full")),
			wantCode: 1,
			wantLine: "stratum: up: disk full\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := ExitCode(tc.err, &stderr); got != tc.wantCode {
				t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.wantCode)
			}
			if got := stderr.String(); got != tc.wantLine {
				if tc.wantLine == "" {
					t.Errorf("ExitCode wrote %q; the sentinel already reported itself, so the writer must stay empty", got)
				} else {
					t.Errorf("ExitCode wrote %q, want %q", got, tc.wantLine)
				}
			}
		})
	}
}

// Fails while any exported sentinel has no case in ExitCode's body.
func TestExitCodeCoversEveryExportedSentinel(t *testing.T) {
	sentinels := exportedErrorSentinels(t)
	covered := exitCodeMappedNames(t)

	for _, name := range sentinels {
		if !covered[name] {
			t.Errorf("exported error sentinel cli.%s has no case in ExitCode; a sentinel without a mapping falls through to the default exit-1-plus-stratum-line case — map it deliberately", name)
		}
	}
	for _, name := range []string{"ErrNoCommand", "ErrUsage", "ErrCheckFailed", "ErrExitCode"} {
		if !slices.Contains(sentinels, name) {
			t.Errorf("the sentinel inventory did not find cli.%s; the AST walk over pkg/cli's declarations is broken and the coverage check above cannot be trusted", name)
		}
	}
}

// Pins the var-filter shapes, asserted directly over parsed source snippets.
func TestSentinelVarFilters(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		// What isErrorsNew and isFmtErrorf must report for the snippet's var spec.
		wantErrorsNew bool
		wantFmtErrorf bool
	}{
		{"errors.New literal", `var ErrX = errors.New("x")`, true, false},
		{"fmt.Errorf literal", `var ErrX = fmt.Errorf("x")`, false, true},
		{"fmt.Errorf with format args", `var ErrX = fmt.Errorf("x %d", 1)`, false, true},
		{"a typed error var built by errors.New", `var ErrX error = errors.New("x")`, true, false},
		{"fmt.Sprintf is not the constructor idiom", `var ErrX = fmt.Sprintf("x")`, false, false},
		{"errors.Join is not the constructor idiom", `var ErrX = errors.Join()`, false, false},
		{"a multi-value spec is not a sentinel", `var ErrX, n = errors.New("x"), 1`, false, false},
		{"a non-error var is not a sentinel", `var ErrX = 3`, false, false},
		{"a call through a variable is not the idiom", `var ErrX = b.New("x")`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := parseVarSpec(t, tc.src)
			if got := isErrorsNew(spec.Values); got != tc.wantErrorsNew {
				t.Errorf("isErrorsNew(%s) = %v, want %v", tc.src, got, tc.wantErrorsNew)
			}
			if got := isFmtErrorf(spec.Values); got != tc.wantFmtErrorf {
				t.Errorf("isFmtErrorf(%s) = %v, want %v", tc.src, got, tc.wantFmtErrorf)
			}
		})
	}
}

// Parses a one-declaration snippet so the shape filters assert over real source.
func parseVarSpec(t *testing.T, src string) *ast.ValueSpec {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "snippet.go", "package p\n\n"+src, 0)
	if err != nil {
		t.Fatalf("parsing %q: %v", src, err)
	}
	spec, ok := file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.ValueSpec)
	if !ok {
		t.Fatalf("%q is not a var spec", src)
	}
	return spec
}

// Test binaries run with the working directory set to the package directory.
func pkgSourceFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files = append(files, file)
	}
	return files
}

// Exported error sentinels: package-level vars and named types with an Error method.
func exportedErrorSentinels(t *testing.T) []string {
	t.Helper()
	files := pkgSourceFiles(t)
	var varNames []string
	typeNames := map[string]bool{}
	methodReceivers := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						if d.Tok != token.VAR {
							continue
						}
						// The two constructor idioms: explicit error type, or errors.New/fmt.Errorf inferred.
						if !isErrorInterface(s.Type) && !isErrorsNew(s.Values) && !isFmtErrorf(s.Values) {
							continue
						}
						for _, name := range s.Names {
							if name.IsExported() {
								varNames = append(varNames, name.Name)
							}
						}
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							typeNames[s.Name.Name] = true
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || d.Name.Name != "Error" {
					continue
				}
				if recv := receiverTypeName(d.Recv); recv != "" {
					methodReceivers[recv] = true
				}
			}
		}
	}
	var names []string
	for _, name := range varNames {
		names = append(names, name)
	}
	for name := range typeNames {
		if methodReceivers[name] {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func isErrorInterface(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "error"
}

// True when a var spec's sole value is a call to <pkg>.<fun>, with no explicit type.
func isSoleCall(values []ast.Expr, pkg, fun string) bool {
	if len(values) != 1 {
		return false
	}
	call, ok := values[0].(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg && sel.Sel.Name == fun
}

func isErrorsNew(values []ast.Expr) bool {
	return isSoleCall(values, "errors", "New")
}

func isFmtErrorf(values []ast.Expr) bool {
	return isSoleCall(values, "fmt", "Errorf")
}

func receiverTypeName(fields *ast.FieldList) string {
	if fields == nil || len(fields.List) != 1 {
		return ""
	}
	expr := fields.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

// A sentinel counts as covered when ExitCode's body names it; a new one without a case fails.
func exitCodeMappedNames(t *testing.T) map[string]bool {
	t.Helper()
	var body *ast.BlockStmt
	for _, file := range pkgSourceFiles(t) {
		for _, decl := range file.Decls {
			if d, ok := decl.(*ast.FuncDecl); ok && d.Recv == nil && d.Name.Name == "ExitCode" {
				body = d.Body
			}
		}
	}
	if body == nil {
		t.Fatal("pkg/cli does not declare func ExitCode(err error, stderr io.Writer) int; the exit-code mapping must be exported from this package")
	}
	names := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.IsExported() {
			names[id.Name] = true
		}
		return true
	})
	return names
}

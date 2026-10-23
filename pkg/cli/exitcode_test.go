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

// TestExitCode pins the mapping ExitCode applies from pkg/cli's error
// sentinels to the process exit code, and whether it writes a final
// "stratum: ..." line on top of what the command already reported. The four
// sentinels each get one direct row and one wrapped row (through errors.Is
// for the plain sentinel vars, errors.As for the pointer-receiver types),
// because a command can return a wrapped sentinel and the code must not
// change with the wrapping. Anything that is not a sentinel takes the
// default: exit 1 with a "stratum: <message>" line — the one place a
// failing command's error prints, in the CLI's report style. Every
// sentinel case is silent: the command has already told the operator what
// it needed to (help, usage, a report, or inherited child stdio), so the
// mapping prints nothing for them.
func TestExitCode(t *testing.T) {
	usageErr := &ErrUsage{err: errors.New("unknown flag: --bogus")}
	for _, tc := range []struct {
		name string
		err  error
		// wantCode is the exit code the mapping must produce.
		wantCode int
		// wantLine is the exact text the writer must carry, or empty when
		// the sentinel already reported itself and the writer must stay
		// empty.
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

// TestExitCodeCoversEveryExportedSentinel walks pkg/cli's exported error
// sentinels — every exported package-level var typed error or built by
// errors.New or fmt.Errorf, and every exported type with an Error method —
// and fails if any of them has no case in ExitCode's body. The exit-code
// mapping is the contract an embedding build depends on, so a sentinel added
// to pkg/cli without a mapping would silently fall through to the default case
// and print a spurious "stratum: ..." line on top of what the command
// already reported. This inventory makes that escape a failing test instead.
// The known sentinels are asserted present so a blind or broken AST walk
// cannot pass vacuously while a real escape goes unnoticed.
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

// TestSentinelVarFilters pins the shapes the inventory's var filter accepts,
// asserting the filter functions directly over parsed source snippets: the
// two sentinel idioms (errors.New and fmt.Errorf — the latter counted because
// a `var ErrX = fmt.Errorf(...)` sentinel was invisible to the walk and so
// escaped the mapping) and the look-alike shapes that must stay out (another
// constructor on either package, a multi-value spec, a non-error var, a call
// through anything that is not a plain package selector). A filter widened
// past these shapes would misreport an ordinary var as a sentinel; narrowed
// past them, it would drop a real sentinel out of the inventory again.
func TestSentinelVarFilters(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		// wantErrorsNew and wantFmtErrorf are what isErrorsNew and
		// isFmtErrorf must report for the snippet's var spec.
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

// parseVarSpec parses a one-declaration snippet and returns its var spec, so
// the shape filters can be asserted against exactly the source they will see.
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

// pkgSourceFiles parses the package's own non-test Go sources. Test binaries
// run with the working directory set to the package directory, so "." is
// pkg/cli when this test runs.
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

// exportedErrorSentinels returns the names of pkg/cli's exported error
// sentinels as declared in source: exported package-level vars whose type is
// the error interface or whose sole value is an errors.New or fmt.Errorf call
// (ErrNoCommand, ErrCheckFailed), and exported named types carrying an Error
// method (ErrUsage, ErrExitCode).
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
						// A sentinel var either declares the error
						// interface (`var ErrX error`) or infers it from
						// errors.New or fmt.Errorf (`var ErrX =
						// errors.New(...)` / `fmt.Errorf(...)`, whose AST
						// carries no explicit type at all) — the two
						// constructor idioms this package's sentinels use.
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

// isErrorInterface reports whether the expression is the predeclared error
// interface's identifier.
func isErrorInterface(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "error"
}

// isSoleCall reports whether the sole value of a var spec is a call to
// <pkg>.<fun> — the shape an inferred sentinel var takes in source, with no
// explicit type on the spec.
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

// isErrorsNew reports whether the sole value of a var spec is an
// errors.New(...) call.
func isErrorsNew(values []ast.Expr) bool {
	return isSoleCall(values, "errors", "New")
}

// isFmtErrorf reports whether the sole value of a var spec is a
// fmt.Errorf(...) call — a sentinel whose message is built from format pieces
// rather than a literal, the second constructor idiom this package's
// sentinels use.
func isFmtErrorf(values []ast.Expr) bool {
	return isSoleCall(values, "fmt", "Errorf")
}

// receiverTypeName extracts the named type a method is declared on, from
// either a value receiver (ErrUsage) or a pointer one (*ErrUsage).
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

// exitCodeMappedNames collects every exported identifier ExitCode's body
// references. A sentinel counts as covered when the mapping names it — in a
// case's errors.Is check, an errors.As target's declaration for a
// pointer-receiver type, or a type switch — so a new sentinel with no case
// is absent here and the inventory test fails.
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

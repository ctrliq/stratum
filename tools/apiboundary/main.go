// Command apiboundary asserts that a package's exported API (exported func,
// type, var, and const declarations) never references a type from one of
// this module's own internal/... packages.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: apiboundary <package-dir>")
		os.Exit(2)
	}
	dir := os.Args[1]

	violations, err := checkDir(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apiboundary:", err)
		os.Exit(2)
	}
	if len(violations) == 0 {
		fmt.Printf("apiboundary: %s exports no internal/* types\n", dir)
		return
	}
	sort.Strings(violations)
	for _, v := range violations {
		fmt.Fprintln(os.Stderr, v)
	}
	fmt.Fprintf(os.Stderr, "\napiboundary: %d exported-API leak(s) of an internal/* type in %s\n", len(violations), dir)
	os.Exit(1)
}

func checkDir(dir string) ([]string, error) {
	fset := token.NewFileSet()

	// loadExportedTypes compiles dir and its whole dependency graph (compiler
	// export data only exists for something that built), so it needs the
	// package to build even to check the syntactically simplest exported
	// declaration — see its own doc comment for what that costs a standalone
	// invocation vs. one running after `go vet`/`go test` in this repo's
	// Makefile.
	pkg, files, mod, err := loadExportedTypes(dir)
	if err != nil {
		return nil, err
	}

	var violations []string
	// Parse exactly the files selected by go list, including its OS, build-tag,
	// and CGO settings. Inactive declarations must not enter either check.
	for _, filename := range files {
		file, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			return nil, err
		}
		byName := importPathsByName(file, pkg)
		for _, decl := range file.Decls {
			violations = append(violations, checkExportedDecl(decl, pkg, byName, fset, filename, mod)...)
		}
	}
	return violations, nil
}

// importPathsByName uses each file's explicit aliases and the compiler's
// declared package names for unaliased imports. Import bindings are file-local.
func importPathsByName(file *ast.File, pkg *types.Package) map[string]string {
	names := make(map[string]string, len(pkg.Imports()))
	for _, imp := range pkg.Imports() {
		names[imp.Path()] = imp.Name()
	}
	m := make(map[string]string, len(file.Imports))
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value) // go list already validated these imports.
		name := names[path]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name != "" && name != "_" && name != "." {
			m[name] = path
		}
	}
	return m
}

// checkExportedDecl looks up each of decl's exported top-level names in pkg
// — the type-checked package, already loaded from compiler export data — and
// checks its fully resolved type for an internal/* reference via
// internalTypeRef.
func checkExportedDecl(decl ast.Decl, pkg *types.Package, byName map[string]string, fset *token.FileSet, filename string, mod moduleScope) []string {
	var violations []string
	report := func(name string, pos token.Pos, path string) {
		p := fset.Position(pos)
		violations = append(violations, fmt.Sprintf("%s:%d: exported %s references internal package %q",
			filepath.Base(filename), p.Line, name, path))
	}
	check := func(name string, pos token.Pos) bool {
		obj := pkg.Scope().Lookup(name)
		if obj == nil {
			return false
		}
		if path, ok := internalTypeRef(obj.Type(), make(map[types.Type]bool), mod); ok {
			report(name, pos, path)
			return true
		}
		return false
	}

	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil || !d.Name.IsExported() {
			// Methods aren't in package scope by name (two types in the same
			// package can both have a method called the same thing), so
			// they're not looked up here — they're already covered by
			// internalTypeRef walking their receiver type's own
			// NumMethods()/Method(i).Type(), the same way struct fields are
			// covered by walking the type's Underlying().
			return nil
		}
		check(d.Name.Name, d.Name.Pos())
	case *ast.GenDecl:
		switch d.Tok {
		case token.TYPE:
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				found := check(ts.Name.Name, ts.Name.Pos())
				if !found && ts.Assign == token.NoPos { // a defined type, not `type X = Y`
					if path, ok := definedFromInternal(ts.Type, byName, mod); ok {
						report(ts.Name.Name, ts.Name.Pos(), path)
					}
				}
			}
		case token.VAR, token.CONST:
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, n := range vs.Names {
					if n.IsExported() {
						check(n.Name, n.Pos())
					}
				}
			}
		}
	}
	return violations
}

// definedFromInternal reports whether typ — a defined (non-alias) type
// declaration's right-hand side — directly names a type from an internal
// package, e.g. `type Defined internal.Token` or `type Defined
// *internal.Token`. Deliberately narrow: this exists only to catch that one
// specific, textually-obvious pattern, not to re-implement general type
// resolution — internalTypeRef already does that correctly for every case
// that's an actual API-surface leak.
func definedFromInternal(typ ast.Expr, byName map[string]string, mod moduleScope) (string, bool) {
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	sel, ok := typ.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	path, ok := byName[ident.Name]
	if !ok || !mod.isInternal(path) {
		return "", false
	}
	return path, true
}

// moduleScope is this module's path plus, for every package in the
// dependency graph, the module that actually owns it.
type moduleScope struct {
	path  string
	owner map[string]string
}

// isInternal reports whether path is one of THIS module's own internal
// packages.
func (m moduleScope) isInternal(path string) bool {
	if m.path == "" || m.owner[path] != m.path {
		return false
	}
	// Scan the part after the module path, not the whole import path.
	for _, segment := range strings.Split(strings.TrimPrefix(path[len(m.path):], "/"), "/") {
		if segment == "internal" {
			return true
		}
	}
	return false
}

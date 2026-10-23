// typecheck.go loads a package's fully resolved types from compiler export
// data and walks them looking for an internal/* reference.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// go list supplies module-aware compiler export files to the standard
// library importer.
func loadExportedTypes(dir string) (*types.Package, []string, moduleScope, error) {
	cmd := exec.Command("go", "list", "-deps", "-export", "-json", ".")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, moduleScope{}, fmt.Errorf("loading exported API types (the package and its dependencies must build for this check to run — try `go build ./...` first): %w: %s", err, &stderr)
	}
	exports := make(map[string]string)
	// owner is each package's own module, straight from go list.
	owner := make(map[string]string)
	var target, module string
	var files []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var pkg struct {
			ImportPath string
			Export     string
			DepOnly    bool
			Dir        string
			GoFiles    []string
			CgoFiles   []string
			Module     *struct{ Path string }
		}
		if err := dec.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, moduleScope{}, fmt.Errorf("reading package export paths: %w", err)
		}
		exports[pkg.ImportPath] = pkg.Export
		if pkg.Module != nil {
			owner[pkg.ImportPath] = pkg.Module.Path
		}
		if !pkg.DepOnly {
			target = pkg.ImportPath
			if pkg.Module != nil {
				module = pkg.Module.Path
			}
			for _, name := range append(pkg.GoFiles, pkg.CgoFiles...) {
				files = append(files, filepath.Join(pkg.Dir, name))
			}
		}
	}
	if target == "" {
		return nil, nil, moduleScope{}, fmt.Errorf("no package found in %s", dir)
	}
	// A package outside any module has no internal packages of "ours" to
	// leak, and silently checking nothing would be worse than saying so.
	if module == "" {
		return nil, nil, moduleScope{}, fmt.Errorf("%s is not in a module, so there is no module scope to check against", target)
	}
	imp := importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, fmt.Errorf("no compiler export data for %s", path)
		}
		return os.Open(file)
	})
	pkg, err := imp.Import(target)
	return pkg, files, moduleScope{path: module, owner: owner}, err
}

func internalTypeRef(t types.Type, seen map[types.Type]bool, mod moduleScope) (string, bool) {
	if t == nil || seen[t] {
		return "", false
	}
	seen[t] = true
	var children []types.Type
	switch t := t.(type) {
	case *types.Alias:
		if owner := t.Obj().Pkg(); owner != nil && mod.isInternal(owner.Path()) {
			return owner.Path(), true
		}
		// Unlike *types.Named below, always unwrap: an alias has no identity
		// of its own beyond Rhs(), so this is the only way to see that e.g. a
		// sibling public package's `type Public = internal.Foo` denotes an
		// internal type — Obj().Pkg() only tells us who declared the alias
		// name, not what it denotes.
		children = append(children, t.Rhs())
	case *types.Named:
		if owner := t.Obj().Pkg(); owner != nil && mod.isInternal(owner.Path()) {
			return owner.Path(), true
		}
		// TypeArgs is non-empty for an instantiated use (Foo[int]); TypeParams
		// is non-empty for the generic declaration itself (type Foo[T
		// Constraint] ...) — a constraint interface can reference an internal
		// type just as much as a field can, so both need walking.
		for i := 0; i < t.TypeArgs().Len(); i++ {
			children = append(children, t.TypeArgs().At(i))
		}
		for i := 0; i < t.TypeParams().Len(); i++ {
			children = append(children, t.TypeParams().At(i).Constraint())
		}
		// A public wrapper's exported members are reachable from our API too,
		// even when another package declares the wrapper. seen breaks cycles.
		children = append(children, t.Underlying())
		for i := 0; i < t.NumMethods(); i++ {
			if m := t.Method(i); m.Exported() {
				children = append(children, m.Type())
			}
		}
	case *types.Pointer:
		children = append(children, t.Elem())
	case *types.Slice:
		children = append(children, t.Elem())
	case *types.Array:
		children = append(children, t.Elem())
	case *types.Map:
		children = append(children, t.Key(), t.Elem())
	case *types.Chan:
		children = append(children, t.Elem())
	case *types.Signature:
		children = append(children, t.Params(), t.Results())
		// A generic function's own type parameters carry constraint interfaces (e.g.
		for i := 0; i < t.TypeParams().Len(); i++ {
			children = append(children, t.TypeParams().At(i).Constraint())
		}
	case *types.Tuple:
		for i := 0; i < t.Len(); i++ {
			children = append(children, t.At(i).Type())
		}
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			if f := t.Field(i); f.Exported() || f.Embedded() {
				children = append(children, f.Type())
			}
		}
	case *types.Interface:
		for i := 0; i < t.NumMethods(); i++ {
			if m := t.Method(i); m.Exported() {
				children = append(children, m.Type())
			}
		}
		// A constraint interface's type-set terms (~int | internal.Foo) are
		// embeddeds, not methods — NumMethods alone misses them entirely.
		for i := 0; i < t.NumEmbeddeds(); i++ {
			children = append(children, t.EmbeddedType(i))
		}
	case *types.Union:
		for i := 0; i < t.Len(); i++ {
			children = append(children, t.Term(i).Type())
		}
	}
	for _, child := range children {
		if path, ok := internalTypeRef(child, seen, mod); ok {
			return path, true
		}
	}
	return "", false
}

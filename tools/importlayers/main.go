// Command importlayers fails when an internal package imports a higher layer, pkg/… or cmd/….
package main

import (
	"bufio"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Longest prefix wins, so a test-helper subpackage can sit beneath its parent.
type layerRule struct {
	prefix string
	layer  int
	line   int
	used   bool
}

// The edge is the exception's identity; the reason need not be kept.
type exception struct {
	importer string
	imported string
	line     int
}

type edgeKey struct {
	importer string
	imported string
}

type occurrence struct {
	path string
	line int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "importlayers: name exactly one root directory to check")
		return 2
	}
	root := filepath.Clean(args[0])
	if fi, err := os.Stat(root); err != nil {
		fmt.Fprintf(stderr, "importlayers: %v\n", err)
		return 2
	} else if !fi.IsDir() {
		fmt.Fprintf(stderr, "importlayers: %s: not a directory: the guard checks a package tree\n", root)
		return 2
	}
	// Rooted at the checked tree, so `go run ./tools/importlayers .` needs no configuration.
	config := filepath.Join(root, "tools", "importlayers", "layers.txt")
	exceptionsPath := filepath.Join(root, "tools", "importlayers", "exceptions.txt")

	rules, err := parseLayers(config)
	if err != nil {
		fmt.Fprintf(stderr, "importlayers: %v\n", err)
		return 2
	}
	excs, err := parseExceptions(exceptionsPath)
	if err != nil {
		fmt.Fprintf(stderr, "importlayers: %v\n", err)
		return 2
	}
	module, err := readModulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		fmt.Fprintf(stderr, "importlayers: %v\n", err)
		return 2
	}

	edges, boundary, errs := scan(root, module, rules)
	for _, err := range errs {
		fmt.Fprintf(stderr, "importlayers: %v\n", err)
	}
	unused := unusedRules(rules)
	for _, r := range unused {
		fmt.Fprintf(stderr, "importlayers: %s:%d: rule %q matches no package in the tree: the layer table has drifted from the code. Fix the prefix, or drop the rule, in the same change that moved or renamed the package.\n", config, r.line, r.prefix)
	}
	if len(errs) > 0 || len(unused) > 0 {
		return 2
	}

	// Same layer or downward is legal; an upward edge is a violation unless excepted.
	type violation struct {
		key edgeKey
		occ occurrence
	}
	var violations []violation
	excepted := make(map[edgeKey]bool)
	for key, occs := range edges {
		importerLayer := ruleFor(key.importer, rules).layer
		importedLayer := ruleFor(key.imported, rules).layer
		if importedLayer <= importerLayer {
			continue
		}
		if _, ok := excs[key]; ok {
			excepted[key] = true
			continue
		}
		for _, occ := range occs {
			violations = append(violations, violation{key: key, occ: occ})
		}
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].occ.path != violations[j].occ.path {
			return violations[i].occ.path < violations[j].occ.path
		}
		return violations[i].occ.line < violations[j].occ.line
	})
	for _, v := range violations {
		importerLayer := ruleFor(v.key.importer, rules).layer
		importedLayer := ruleFor(v.key.imported, rules).layer
		fmt.Fprintf(stdout, "%s:%d: %s (layer %d) imports %s (layer %d): an internal package may import only packages of its own or a strictly lower layer. Move the imported code into layer %d or below, move the importing code up to the layer its purpose belongs to, or add a reviewed exception to %s stating the reason the upward import stands.\n",
			v.occ.path, v.occ.line, v.key.importer, importerLayer, v.key.imported, importedLayer,
			importerLayer, exceptionsPath)
	}
	sort.Strings(boundary)
	for _, b := range boundary {
		fmt.Fprintln(stdout, b)
	}
	var stale []exception
	for key, exc := range excs {
		if excepted[key] {
			continue
		}
		// Gone or no longer a violation: the fix is the same either way.
		stale = append(stale, exc)
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].line < stale[j].line })
	for _, exc := range stale {
		fmt.Fprintf(stdout, "%s:%d: stale exception %s -> %s: the edge no longer exists, or the packages' layers no longer make it a violation. Drop the entry — or re-justify it with the reason the upward import now stands — in the same change.\n",
			exceptionsPath, exc.line, exc.importer, exc.imported)
	}
	// excepted holds only keys from excs, so its length is the exercised count.
	exceptedCount := len(excepted)
	if len(violations) > 0 || len(boundary) > 0 || len(stale) > 0 {
		fmt.Fprintf(stderr, "importlayers: %d internal import-layering violation(s), %d boundary violation(s), %d stale exception(s)\n",
			len(violations), len(boundary), len(stale))
		return 1
	}
	fmt.Fprintf(stdout, "importlayers: every internal package imports its own or a lower layer only (%d reviewed exception(s) honored)\n", exceptedCount)
	return 0
}

// A typo'd rule fails the check instead of silently unassigning a package.
func parseLayers(path string) ([]layerRule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open layer table: %w", err)
	}
	defer f.Close()

	var rules []layerRule
	seen := make(map[string]int) // prefix -> line of first rule
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 || !strings.HasPrefix(fields[0], "layer") {
			return nil, fmt.Errorf("%s:%d: want `layer<N> <import-path-prefix>`, got %q", path, line, text)
		}
		numText := strings.TrimPrefix(fields[0], "layer")
		n, err := strconv.Atoi(numText)
		// The round-trip rejects a sign or leading zeros, which Atoi accepts.
		if err != nil || n < 1 || strconv.Itoa(n) != numText {
			return nil, fmt.Errorf("%s:%d: want a layer number of 1 or more, got %q", path, line, fields[0])
		}
		prefix := fields[1]
		if prev, dup := seen[prefix]; dup {
			return nil, fmt.Errorf("%s:%d: duplicate rule for %q (first at line %d)", path, line, prefix, prev)
		}
		seen[prefix] = line
		rules = append(rules, layerRule{prefix: prefix, layer: n, line: line})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read layer table: %w", err)
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("%s: no layer rules", path)
	}
	// Longest prefix first, so lookup is a scan that stops at the first hit.
	sort.Slice(rules, func(i, j int) bool { return len(rules[i].prefix) > len(rules[j].prefix) })
	return rules, nil
}

// An exception is a reviewed decision, and the reason is its record.
func parseExceptions(path string) (map[edgeKey]exception, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open exception file: %w", err)
	}
	defer f.Close()

	excs := make(map[edgeKey]exception)
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		reason := ""
		if i := strings.Index(text, "#"); i >= 0 {
			reason = strings.TrimSpace(text[i+1:])
			text = strings.TrimSpace(text[:i])
		}
		importer, imported, ok := strings.Cut(text, "->")
		importer = strings.TrimSpace(importer)
		imported = strings.TrimSpace(imported)
		if !ok || importer == "" || imported == "" {
			return nil, fmt.Errorf("%s:%d: want `<importer> -> <imported>  # reason`, got %q", path, line, sc.Text())
		}
		// Cut stops at the first "->"; a chained line must not pass.
		if strings.Contains(imported, "->") {
			return nil, fmt.Errorf("%s:%d: want one `<importer> -> <imported>` edge, got %q", path, line, sc.Text())
		}
		if reason == "" {
			return nil, fmt.Errorf("%s:%d: exception %s -> %s states no reason: an exception without its reason is not reviewed, state why the upward import stands", path, line, importer, imported)
		}
		key := edgeKey{importer: importer, imported: imported}
		if _, dup := excs[key]; dup {
			return nil, fmt.Errorf("%s:%d: duplicate exception for %s -> %s", path, line, importer, imported)
		}
		excs[key] = exception{importer: importer, imported: imported, line: line}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read exception file: %w", err)
	}
	return excs, nil
}

// No go tool: the check must run before generated assets exist.
func readModulePath(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	return "", fmt.Errorf("%s: no module line", path)
}

// Every internal/ package must match a rule, even importing nothing internal.
func scan(root, module string, rules []layerRule) (map[edgeKey][]occurrence, []string, []error) {
	var errs []error
	var boundary []string
	edges := make(map[edgeKey][]occurrence)
	reported := make(map[string]bool) // internal packages whose missing rule was already reported

	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return nil
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		// Scratch or editor leftovers, never a package the go tool builds.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			strings.HasPrefix(d.Name(), "_") || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		// The go tool compiles a .go symlink as the file it points to.
		if d.Type()&fs.ModeSymlink != 0 {
			fi, err := os.Stat(path)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", path, err))
				return nil
			}
			if !fi.Mode().IsRegular() {
				return nil
			}
		} else if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		pkgPath := module
		if dir != "." {
			pkgPath = module + "/" + dir
		}
		inScope := isModuleDir(pkgPath, module, "internal")
		if !inScope {
			return nil
		}
		// An import naming a missing package must not keep its rule alive.
		if r := ruleFor(pkgPath, rules); r != nil {
			r.used = true
		} else if !reported[pkgPath] {
			// Reported once per package, not once per file.
			reported[pkgPath] = true
			errs = append(errs, fmt.Errorf("internal package %s is assigned no layer in the layer table: add the rule that states the package's layer, or drop the package", pkgPath))
		}

		// No build-tag filtering: a file gated to another platform still names the package's imports.
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rel, err))
			return nil
		}
		line := func(pos token.Pos) int { return fset.Position(pos).Line }
		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", rel, err))
				continue
			}
			if isModuleDir(importPath, module, "internal") {
				// A blank import still runs the imported package's init.
				if ruleFor(importPath, rules) == nil && !reported[importPath] {
					reported[importPath] = true
					errs = append(errs, fmt.Errorf("internal package %s is assigned no layer in the layer table: add the rule that states the package's layer, or drop the package", importPath))
				}
				key := edgeKey{importer: pkgPath, imported: importPath}
				edges[key] = append(edges[key], occurrence{path: rel, line: line(imp.Pos())})
			} else if isModuleDir(importPath, module, "pkg") || isModuleDir(importPath, module, "cmd") {
				boundary = append(boundary, fmt.Sprintf("%s:%d: internal package %s imports %s, a package outside internal/: internal/ is the implementation beneath this module's commands and interfaces, and code below them does not reach up into them. Move the shared code into internal/, or depend on an interface defined beside the importer.",
					rel, line(imp.Pos()), pkgPath, importPath))
			}
		}
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}
	return edges, boundary, errs
}

func ruleFor(pkgPath string, rules []layerRule) *layerRule {
	for i := range rules {
		r := &rules[i]
		if pkgPath == r.prefix || strings.HasPrefix(pkgPath, r.prefix+"/") {
			return r
		}
	}
	return nil
}

func unusedRules(rules []layerRule) []*layerRule {
	var out []*layerRule
	for i := range rules {
		if !rules[i].used {
			out = append(out, &rules[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

func isModuleDir(pkgPath, module, dir string) bool {
	base := module + "/" + dir
	return pkgPath == base || strings.HasPrefix(pkgPath, base+"/")
}

func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

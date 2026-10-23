// Command importlayers is the internal import-layering rule's lint guard,
// run by make lint over the whole repository. The rule: the module's
// internal/... packages are assigned to layers by purpose — layer 1 the
// foundations, layer 2 the small primitives, layer 3 the host/PKI/node-state
// helpers, layer 4 the orchestration and domain services, layer 5 the
// user-facing interfaces — and a package may import only packages of its own
// or a strictly lower layer. Same-layer imports are legal on purpose: the
// orchestration packages legitimately share each other, and forbidding that
// would degenerate layer 4 into micro-layers. An
// upward import is a violation: the finding names the importer's file:line,
// both packages, both layers, and the two ways out — move the code to the
// layer its purpose belongs to, or add a reviewed exception stating the
// reason. The membership table is checked in beside this guard
// (tools/importlayers/layers.txt, one layer<N> prefix rule per line, longest
// prefix wins), so the layers and the gate that enforces them cannot drift
// apart. The table is renamed-package-proof in both directions: an internal
// package matching no rule fails the check, and so does a rule matching no
// package.
//
// A second, absolute boundary rides along: an internal package must not
// import this module's pkg/... or cmd/... packages. internal/ is the
// implementation beneath them, and code below the interfaces reaching up
// into them is the same shape of defect the layer rule catches, so it fails
// here rather than at review time.
//
// The exceptions file (tools/importlayers/exceptions.txt) holds the
// deliberate upward imports, one `importer -> imported  # reason` line per
// edge. Every entry states the reason the upward import stands; the guard
// scans the repo like any other checker and fails on a stale entry — an edge
// the code no longer has, or one that is no longer a violation — so an
// exception cannot outlive the code that justified it and a new upward
// import cannot hide behind an entry written for a different edge.
//
// Only files under the module's internal/ are read: go/parser (ImportsOnly)
// parses the imports of every non-test .go file in those packages, regardless
// of build tags and GOOS — a file gated to another platform still names the
// imports its package really carries — and testdata, vendor, node_modules,
// hidden- and underscore-prefixed files and directories are skipped, as the
// go tool skips them.
//
// The go tool is not invoked: the check must run on a tree that has not
// built its generated assets. Test files are exempt: _test.go files may
// legally import upward (a table-driven fixture is deliberate), and the
// rule governs production code.
//
// Exit codes: 0 when every internal package is clean; 1 when an upward
// import, an internal import of pkg/... or cmd/..., or a stale exception
// was found; 2 when the check cannot run — a bad argument, an unreadable
// file, a file that does not parse, a package the
// table assigns no layer, a rule that matches no package, a malformed entry
// in either data file. A package the guard cannot vouch for must fail the
// check, never pass it silently.
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

// layerRule is one membership rule from layers.txt: every package whose
// import path has this prefix sits in this layer. Longest prefix wins, so a
// test-helper subpackage can sit in a layer of its own beneath its parent.
type layerRule struct {
	prefix string
	layer  int
	line   int // the rule's line in layers.txt, for error messages
	used   bool
}

// exception is one deliberate upward import from exceptions.txt. The entry's
// reason is validated at parse time — an exception without a stated reason
// is a config error, not a loophole — but need not be kept: the edge is the
// exception's identity.
type exception struct {
	importer string
	imported string
	line     int // the entry's line in exceptions.txt, for error messages
}

// edgeKey identifies one directed import between two internal packages.
type edgeKey struct {
	importer string
	imported string
}

// occurrence is one place an edge is imported: the file's path relative to
// the scanned root and the import's line.
type occurrence struct {
	path string
	line int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run checks args and returns the process exit code: 0 clean, 1 violations
// (an upward import, an internal import of pkg/... or cmd/..., a stale
// exception), 2 tool error.
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
	// Both data files live beside this guard's own code, rooted at the tree
	// being checked, so `go run ./tools/importlayers .` from the repository
	// root finds them with no configuration.
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

	// Classify every edge: same layer or downward is legal; an upward edge
	// is a violation unless an exception covers it. An exception whose edge
	// is gone, or that covers an edge the layers no longer call a
	// violation, is stale and fails the check like a finding.
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
		// The edge is either gone from the code or no longer an upward
		// one; the message need not distinguish, the fix is the same.
		stale = append(stale, exc)
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].line < stale[j].line })
	for _, exc := range stale {
		fmt.Fprintf(stdout, "%s:%d: stale exception %s -> %s: the edge no longer exists, or the packages' layers no longer make it a violation. Drop the entry — or re-justify it with the reason the upward import now stands — in the same change.\n",
			exceptionsPath, exc.line, exc.importer, exc.imported)
	}
	// excepted holds only keys read out of excs, so its length is the
	// number of reviewed exceptions the code still exercises.
	exceptedCount := len(excepted)
	if len(violations) > 0 || len(boundary) > 0 || len(stale) > 0 {
		fmt.Fprintf(stderr, "importlayers: %d internal import-layering violation(s), %d boundary violation(s), %d stale exception(s)\n",
			len(violations), len(boundary), len(stale))
		return 1
	}
	fmt.Fprintf(stdout, "importlayers: every internal package imports its own or a lower layer only (%d reviewed exception(s) honored)\n", exceptedCount)
	return 0
}

// parseLayers reads the layer membership table: one `layer<N> <prefix>` rule
// per line, blank lines and #-comments free. A line in any other shape is a
// config error, so a typo'd rule fails the check instead of silently
// unassigning a package.
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
		// The round-trip rejects a sign or leading zeros ("layer+2",
		// "layer02"): Atoi accepts them, but neither is a layer number.
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

// parseExceptions reads the exception file: one `importer -> imported`
// edge per line, the reason after a # on the same line. A missing or empty
// reason is a config error: an exception is a reviewed decision, and the
// reason is its record.
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
		// Cut stops at the first "->", so a chained line would silently
		// name a middle package as the imported side; an entry is one edge.
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

// readModulePath reads the module path from go.mod: the import paths of the
// tree's packages are derived from it, directory by directory, without
// invoking the go tool (the check must run before the generated assets that
// a `go list` build would need exist).
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

// scan walks the root, parses the imports of every non-test .go file under
// the module's internal/... (files outside internal/ contribute nothing), and
// returns the internal->internal edge set with the places each edge is
// imported, the boundary findings (an internal package importing pkg/... or
// cmd/...), and any tool errors (unreadable or unparseable files). Every
// scanned package under internal/ must match a layer rule, so an unassigned
// package fails the check even if it imports nothing internal.
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
		// The go tool ignores files whose base name begins with "_" or "."
		// just as it does such directories: they are scratch or editor
		// leftovers, never a package it would build.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			strings.HasPrefix(d.Name(), "_") || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		// The go tool compiles a .go symlink as the file it points to, so the
		// guard reads it too; a symlink to a directory is no source file.
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
		// A rule counts as used only for a package found in the tree: an
		// import naming a package that does not exist must not keep its rule
		// alive past a rename.
		if r := ruleFor(pkgPath, rules); r != nil {
			r.used = true
		} else if !reported[pkgPath] {
			// Reported once per package; the message names the package,
			// not every file in it.
			reported[pkgPath] = true
			errs = append(errs, fmt.Errorf("internal package %s is assigned no layer in the layer table: add the rule that states the package's layer, or drop the package", pkgPath))
		}

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
				// An import is a dependency whatever its local name —
				// including a blank import, whose init still runs the
				// imported package's code.
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

// ruleFor returns the rule with the longest prefix matching the package
// path, or nil. Matching is on path boundaries: the rule for internal/pki
// does not match internal/pkia.
func ruleFor(pkgPath string, rules []layerRule) *layerRule {
	for i := range rules {
		r := &rules[i]
		if pkgPath == r.prefix || strings.HasPrefix(pkgPath, r.prefix+"/") {
			return r
		}
	}
	return nil
}

// unusedRules returns the rules no scanned package matched, in table order.
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

// isModuleDir reports whether pkgPath is this module's own package at or
// under the named top directory: <module>/<dir> itself or any deeper path
// below it — but not a sibling that merely shares a prefix.
func isModuleDir(pkgPath, module, dir string) bool {
	base := module + "/" + dir
	return pkgPath == base || strings.HasPrefix(pkgPath, base+"/")
}

func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// Command nodefilewrites is the node-file write rule's lint guard for make lint.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ctrliq/stratum/internal/version"
)

var guarded = func() map[string]bool {
	pkgs := make(map[string]bool, len(version.NodeFileOwners))
	for _, pkg := range version.NodeFileOwners {
		pkgs[pkg] = true
	}
	return pkgs
}()

type allowEntry struct {
	reason string
	want   int
}

var allowlist = map[string]allowEntry{
	"internal/pki/pki.go": {reason: "CA-rotation scratch, deliberately not start-critical: zero-byte carotate markers and .pre-carotate backups written and read only by the rotation; the live file behind each backup is replaced by an atomic rename", want: 5},

	"internal/certs/carotate.go": {reason: "the carotate run id is a best-effort node-local marker, deliberately not start-critical: it only schedules leftover-key cleanup, and its writer already downgrades a failure to a WARN", want: 1},

	"internal/upgrade/node.go": {reason: "writes only inside the CIS-config backup's hidden staging directory, deliberately not start-critical: one rename publishes the backup whole, and no live node path is written raw", want: 1},

	"internal/cluster/run.go": {reason: "the image-import stamp marker is best-effort, deliberately not start-critical: it only makes the next kubelet restart's import re-check cheap, its writer already discards the error, and a torn marker costs one re-import of the archive", want: 1},

	"internal/host/systemd.go": {reason: "the uninstall teardown's runtime drop-in under /run is deliberately not start-critical: it disarms Restart= only for the unit being torn down, systemd reads it through the daemon-reload the same function runs after the write, and a reboot clears /run on its own", want: 1},
}

// clientcmd.WriteToFile is a plain os.WriteFile underneath: no temp file, no fsync.
var rawWrites = map[string]string{
	"os":                               "WriteFile",
	"k8s.io/client-go/tools/clientcmd": "WriteToFile",
}

type finding struct {
	path    string
	line    int
	call    string
	allowed int
	reason  string
}

type staleEntry struct {
	path string
	want int
	got  int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "nodefilewrites: no arguments: name files, directories, or directory/... patterns to check")
		return 2
	}
	var errs []error
	files := collect(args, &errs)

	fset := token.NewFileSet()
	var findings []finding
	var stale []staleEntry
	for _, path := range files {
		// Parsed before the guarded check: an unparseable argv file fails the run.
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		if !guardedDir(filepath.Dir(path)) {
			continue
		}
		if entry, ok := allowlist[path]; ok {
			found := scan(fset, f, path)
			switch {
			case len(found) == entry.want:
			case len(found) > entry.want:
				for _, fd := range found {
					fd.allowed = entry.want
					fd.reason = entry.reason
					findings = append(findings, fd)
				}
			default:
				stale = append(stale, staleEntry{path: path, want: entry.want, got: len(found)})
			}
			continue
		}
		findings = append(findings, scan(fset, f, path)...)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].path != findings[j].path {
			return findings[i].path < findings[j].path
		}
		return findings[i].line < findings[j].line
	})
	for _, f := range findings {
		if f.reason != "" {
			fmt.Fprintf(stdout, "%s:%d: %s writes a node file raw in a package that writes node files, but the file is allowlisted for only %d raw write(s): %s. The entry no longer covers this write: move the write onto the shared atomic writer (internal/atomicfile), or update the allowlist entry — count and reason — in tools/nodefilewrites in the same change.\n",
				f.path, f.line, f.call, f.allowed, f.reason)
			continue
		}
		fmt.Fprintf(stdout, "%s:%d: %s writes a node file raw in a package that writes node files. Each node file is owned by its owning package — the one internal/version's owning-package record names beside the file's path — and that package writes it through the shared atomic writer (internal/atomicfile): a plain write can be torn by a crash, leaving a half-written file where a working one was. Move the write onto the shared atomic writer, or allowlist this file in tools/nodefilewrites with the stated reason the writer is deliberately not start-critical.\n",
			f.path, f.line, f.call)
	}
	for _, s := range stale {
		fmt.Fprintf(stdout, "%s: the allowlist entry covers %d raw write(s) but the file now has %d: the entry is stale. Shrink it — count and reason — to the writes its reason still covers, or drop it.\n",
			s.path, s.want, s.got)
	}
	if len(errs) > 0 {
		for _, err := range errs {
			fmt.Fprintf(stderr, "nodefilewrites: %v\n", err)
		}
		return 2
	}
	if len(findings) > 0 || len(stale) > 0 {
		fmt.Fprintf(stderr, "nodefilewrites: %d node-file write rule violation(s) in packages that write node files\n", len(findings)+len(stale))
		return 1
	}
	return 0
}

func guardedDir(dir string) bool {
	for pkg := range guarded {
		if dir == pkg || strings.HasPrefix(dir, pkg+"/") {
			return true
		}
	}
	return false
}

func collect(args []string, errs *[]error) []string {
	var files []string
	consider := func(path string) {
		path = filepath.Clean(path)
		switch {
		case strings.HasSuffix(path, "_test.go"):
			// Test files write fixtures on purpose.
		case strings.HasSuffix(path, ".go"):
			files = append(files, path)
		}
	}
	for _, arg := range args {
		a := filepath.Clean(arg)
		if a == "..." || strings.HasSuffix(a, "/...") {
			dir := strings.TrimSuffix(strings.TrimSuffix(a, "..."), "/")
			if dir == "" {
				dir = "."
			}
			walk(dir, consider, errs)
			continue
		}
		fi, err := os.Stat(a)
		if err != nil {
			*errs = append(*errs, fmt.Errorf("%s: %w", a, err))
			continue
		}
		if fi.IsDir() {
			walk(a, consider, errs)
			continue
		}
		if !strings.HasSuffix(a, ".go") {
			*errs = append(*errs, fmt.Errorf("%s: not a Go file: the guard checks Go source only", a))
			continue
		}
		consider(a)
	}
	return files
}

func walk(root string, consider func(string), errs *[]error) {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Keep walking; the recorded error already fails the run.
			*errs = append(*errs, fmt.Errorf("%s: %w", path, err))
			return nil
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		consider(path)
		return nil
	})
	if err != nil {
		*errs = append(*errs, err)
	}
}

func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".")
}

func scan(fset *token.FileSet, file *ast.File, path string) []finding {
	callees := make(map[string]string)
	dot := make(map[string]bool)
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		switch {
		case imp.Name == nil:
			callees[defaultName(importPath)] = importPath
		case imp.Name.Name == ".":
			dot[importPath] = true
		case imp.Name.Name == "_":
			// A blank import cannot be called through.
		default:
			callees[imp.Name.Name] = importPath
		}
	}

	var findings []finding
	add := func(pos token.Pos, importPath, sel string) {
		findings = append(findings, finding{
			path: path,
			line: fset.Position(pos).Line,
			call: canonical(importPath, sel),
		})
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			id, ok := fn.X.(*ast.Ident)
			if !ok {
				return true
			}
			importPath, ok := callees[id.Name]
			if !ok {
				return true
			}
			if sel, ok := rawWrites[importPath]; ok && fn.Sel.Name == sel {
				add(call.Pos(), importPath, sel)
			}
		case *ast.Ident:
			for importPath, sel := range rawWrites {
				if dot[importPath] && fn.Name == sel {
					add(call.Pos(), importPath, sel)
				}
			}
		}
		return true
	})
	return findings
}

func canonical(importPath, sel string) string {
	return importPath[strings.LastIndex(importPath, "/")+1:] + "." + sel
}

// defaultName: the path's last element, below a semantic major-version suffix.
func defaultName(importPath string) string {
	base := importPath[strings.LastIndex(importPath, "/")+1:]
	if len(base) > 1 && base[0] == 'v' && allDigits(base[1:]) {
		parent := importPath[:strings.LastIndex(importPath, "/")]
		base = parent[strings.LastIndex(parent, "/")+1:]
	}
	return base
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

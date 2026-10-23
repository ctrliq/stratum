// Command nodefilewrites is the node-file write rule's lint guard, run by
// make lint over the whole repository. The rule: every node file — the paths
// internal/version's owning-package record carries — is written by its owning
// package through the shared atomic writer (internal/atomicfile), so a failed
// or crashed write can never leave a torn file where a working one was. A raw
// os.WriteFile or clientcmd.WriteToFile, in any import spelling, in a package
// that writes node files is a violation: the finding names the call's
// file:line and states the owning-package rule, so the next writer finds the
// rule at lint time instead of rediscovering it at incident time.
//
// Arguments name files, directories, or directory/... package patterns,
// resolved relative to the working directory — the repository root in every
// real invocation. Test files and testdata are exempt (the rule governs
// production writers, and a _test.go fixture write is deliberate), as are
// vendor, node_modules and hidden directories. An allowlisted file is a
// deliberate exception whose entry states the reason its raw writers are not
// start-critical and the exact number of raw writes that reason covers: the
// guard scans allowlisted files like any other and fails when the file's raw
// writes no longer match the entry, so a new raw write cannot hide behind an
// exception written for a different write, and an entry whose write has
// moved onto the shared atomic writer is re-justified or dropped in the same
// change.
//
// Exit codes: 0 when every guarded package is clean; 1 when a raw node-file
// write was found; 2 when the check cannot run — a bad argument, an
// unreadable file, a file that does not parse. A file the guard cannot vouch
// for must fail the check, never pass it silently.
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

// guarded is the set of packages that write node files, derived from the
// owning-package record in internal/version: the record puts each node file's
// owning package beside its path, so the rule and its guard cannot drift
// apart. A scanned file belongs to a guarded package when that package's
// directory is the file's directory or an ancestor of it, compared with "/"
// separators on directory boundaries — internal/pkia is not internal/pki.
var guarded = func() map[string]bool {
	pkgs := make(map[string]bool, len(version.NodeFileOwners))
	for _, pkg := range version.NodeFileOwners {
		pkgs[pkg] = true
	}
	return pkgs
}()

// allowEntry is one deliberate exception: the stated reason the file's raw
// writer is not start-critical, and the exact number of raw writes the
// reason covers. The guard scans an allowlisted file like any other and
// fails when the file's raw writes no longer match the entry, so the stated
// exception and the enforced one cannot drift apart.
type allowEntry struct {
	reason string
	want   int
}

// allowlist holds the deliberate exceptions, keyed by the file's path
// relative to the working directory. An exception is per file, never per
// package: a writer that belongs on the shared atomic writer gets no entry
// here.
var allowlist = map[string]allowEntry{
	// The CA rotation's scratch: the zero-byte carotate progress markers and
	// the .pre-carotate backups are written once mid-rotation and read only
	// by the rotation itself; the live file behind every backup is replaced
	// by an atomic rename. A torn marker or backup costs a rotation step,
	// never a bring-up.
	"internal/pki/pki.go": {reason: "CA-rotation scratch, deliberately not start-critical: zero-byte carotate markers and .pre-carotate backups written and read only by the rotation; the live file behind each backup is replaced by an atomic rename", want: 5},

	// The carotate run id is best-effort by design: its writer already
	// downgrades a failure to a WARN, because it only schedules the
	// leftover-key cleanup and gates no start.
	"internal/certs/carotate.go": {reason: "the carotate run id is a best-effort node-local marker, deliberately not start-critical: it only schedules leftover-key cleanup, and its writer already downgrades a failure to a WARN", want: 1},

	// The CIS-config backup record is written into the backup's hidden
	// staging directory, which one rename publishes whole; no live node path
	// is ever written raw.
	"internal/upgrade/node.go": {reason: "writes only inside the CIS-config backup's hidden staging directory, deliberately not start-critical: one rename publishes the backup whole, and no live node path is written raw", want: 1},

	// The image-import stamp marker is best-effort by design: writeStamp
	// discards the error because the marker only makes the next kubelet
	// restart's import re-check cheap (stampCurrent compares it to skip the
	// re-import), and a stale or torn marker costs one re-import of the
	// archive, never a bring-up.
	"internal/cluster/run.go": {reason: "the image-import stamp marker is best-effort, deliberately not start-critical: it only makes the next kubelet restart's import re-check cheap, its writer already discards the error, and a torn marker costs one re-import of the archive", want: 1},

	// SuppressRestart's runtime drop-in lives under /run, is written only
	// during uninstall's teardown of the unit it disarms Restart= for, and is
	// read by systemd only through the daemon-reload the same function runs
	// after the write; a reboot clears /run on its own. Nothing reads it at
	// bring-up, so a torn one cannot break a start.
	"internal/host/systemd.go": {reason: "the uninstall teardown's runtime drop-in under /run is deliberately not start-critical: it disarms Restart= only for the unit being torn down, systemd reads it through the daemon-reload the same function runs after the write, and a reboot clears /run on its own", want: 1},
}

// rawWrites are the calls the rule rejects, by import path and selector.
// clientcmd.WriteToFile stands beside os.WriteFile because it is a plain
// os.WriteFile underneath: no temp file, no fsync, a torn kubeconfig where a
// working one was.
var rawWrites = map[string]string{
	"os":                               "WriteFile",
	"k8s.io/client-go/tools/clientcmd": "WriteToFile",
}

// finding is one raw node-file write.
type finding struct {
	path string // the file's path relative to the working directory
	line int    // the call's line
	call string // the call's canonical spelling ("os.WriteFile")
	// allowed and reason are set only when the finding sits in an
	// allowlisted file: the entry covers allowed raw writes and its reason
	// states why they are not start-critical, so the finding names the
	// entry's own terms and the fix is to re-justify it in the same change.
	allowed int
	reason  string
}

// staleEntry is an allowlist entry whose file no longer carries the raw
// writes the entry's reason covers.
type staleEntry struct {
	path string
	want int
	got  int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run checks args and returns the process exit code: 0 clean, 1 findings,
// 2 tool error.
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
		// Parse every file the argv names, guarded or not: a file the guard
		// cannot vouch for is a tool error, whatever package it sits in.
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		if !guardedDir(filepath.Dir(path)) {
			continue
		}
		if entry, ok := allowlist[path]; ok {
			// An allowlisted file is scanned like any other: the entry
			// covers an exact number of raw writes, so a new write fails
			// the lint instead of hiding behind the entry, and an entry
			// whose writes have moved onto the atomic writer fails until
			// it is shrunk.
			found := scan(fset, f, path)
			switch {
			case len(found) == entry.want:
				// The exception the entry states still exists.
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

// guardedDir reports whether dir is a guarded package or inside one.
func guardedDir(dir string) bool {
	for pkg := range guarded {
		if dir == pkg || strings.HasPrefix(dir, pkg+"/") {
			return true
		}
	}
	return false
}

// collect resolves the argv to the Go files to check, appending tool errors
// (a missing path, an explicitly named non-Go file, an unreadable directory)
// to errs. Test files are dropped here: the rule governs production writers.
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

// walk walks root recursively, considering every regular .go file. A skipped
// directory is not a judgment call: testdata is invisible to the go tool,
// vendor and node_modules are not ours to guard, and a hidden directory is
// .git and its friends.
func walk(root string, consider func(string), errs *[]error) {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Keep the rest of the tree; the recorded error already fails
			// the run.
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

// scan reports the raw node-file writes in one parsed file. Calls are
// resolved through the file's imports, so an import under another name — or
// a dot import, which puts the write at its bare name — is still the call the
// rule rejects.
func scan(fset *token.FileSet, file *ast.File, path string) []finding {
	callees := make(map[string]string) // local name -> import path
	dot := make(map[string]bool)       // dot-imported paths
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

// canonical spells the call the way the rule names it, whatever the file
// called its import.
func canonical(importPath, sel string) string {
	return importPath[strings.LastIndex(importPath, "/")+1:] + "." + sel
}

// defaultName is the local name a file calls an import through when it names
// no alias: the path's last element, below a semantic major-version suffix.
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

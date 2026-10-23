package main

// These tests are the spec for the import-layers guard (make lint's check
// that internal/ packages only import sideways or downward); the tool itself
// may not exist yet. The contract under test:
//
//	run(args []string, stdout, stderr io.Writer) int
//
//	    args[0] names the root directory to scan — the repository root in
//	    every real invocation. The layer table and the exception list are
//	    read from <root>/tools/importlayers/layers.txt and exceptions.txt,
//	    the checked-in locations beside the guard itself; it takes no flags.
//	    Exit 0 when every in-scope import is legal. Exit 1 when an upward
//	    import, an internal→pkg/… or internal→cmd/… import, or a stale
//	    exception was found. Exit 2 when the check cannot run — a missing or
//	    unreadable config file, an internal package no layer rule assigns, a
//	    rule that matches no package (rename protection), or a file that
//	    does not parse. A file the guard cannot vouch for must fail the
//	    check, never pass it silently.
//
//	layers.txt: one rule per line, `layer<N> <module-path-prefix>`, longest
//	matching prefix wins; `#` starts a whole-line comment — a `#` after
//	anything on a rule line is not one. exceptions.txt: one
//	exception per line, `<importer> -> <imported>  # reason`, where the edge
//	is a real, current upward import; an exception whose edge has vanished
//	or is no longer a violation is stale and fails the run.
//
//	The rule: an internal/ package of this module may import packages of the
//	same layer or any lower layer; importing a higher layer is a violation.
//	An internal package importing this module's pkg/… or cmd/… is always a
//	violation. Importers out of scope — everything outside internal/ — are
//	not judged. Scan discipline: every non-test .go file under the root,
//	whatever its build tags or GOOS (a !linux file's imports are still the
//	repo's imports), parsed with go/parser ImportsOnly — never `go list`,
//	which fails on the un-built dashboard embed. testdata, vendor,
//	node_modules and hidden directories are skipped; _test.go files are
//	skipped (test-only imports are the test's own business).
//
//	Every violation message carries the importer's file:line, both package
//	paths, both layers (spelled "layer N"), and the fix: move the code down
//	a layer or add a reviewed exception.
//
// The fixtures are parsed, not compiled: they need valid Go syntax, not
// resolvable imports.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// app is the fixture module's path.
const app = "example.com/app"

// threeLayerConfig is a layer table for a three-layer fixture module, in the
// same format the real repo's tools/importlayers/layers.txt uses.
const threeLayerConfig = `# layer1 — foundations: no internal deps
layer1 ` + app + `/internal/base
layer1 ` + app + `/internal/leaf
# layer2 — primitives above the foundations
layer2 ` + app + `/internal/mid
layer2 ` + app + `/internal/sibling
# layer3 — orchestration on top
layer3 ` + app + `/internal/top
`

// baseImportsMid is an upward import (layer 1 importing layer 2); the import
// sits on line 3.
const baseImportsMid = `package base

import "` + app + `/internal/mid"

func use() string { return mid.Name }
`

// midImportsBase is a legal downward import.
const midImportsBase = `package mid

import "` + app + `/internal/base"

func use() string { return base.Name }
`

// topImportsEverything is legal: layer 3 may import layers 2 and 1.
const topImportsEverything = `package top

import (
	"` + app + `/internal/base"
	"` + app + `/internal/mid"
)

func use() string { return mid.Name + base.Name }
`

// siblingImportsMid is a same-layer import: legal by design, without which
// an orchestration layer's packages could not share each other.
const siblingImportsMid = `package sibling

import "` + app + `/internal/mid"

func use() string { return mid.Name }
`

// tagGatedImportsMid carries //go:build !linux: on a linux host it never
// compiles, on any other GOOS it does. Either way its imports are the
// package's, so the guard must parse it regardless of tags.
const tagGatedImportsMid = `//go:build !linux

package base

import "` + app + `/internal/mid"

func use() string { return mid.Name }
`

// tagGatedLinuxImportsTop is the mirror case: a linux-only file. A guard
// that trusted the host GOOS would miss it on a darwin workstation.
const tagGatedLinuxImportsTop = `//go:build linux

package sibling

import "` + app + `/internal/top"

func use() string { return top.Name }
`

// baseImportsPkg breaks the other internal boundary: internal code must not
// reach this module's pkg/… or cmd/….
const baseImportsPkg = `package base

import "` + app + `/pkg/pub"

func use() string { return pub.Name }
`

const midImportsCmd = `package mid

import "` + app + `/cmd/prog"

func use() string { return prog.Name }
`

// pubImportsTop is legal scoping-wise: pkg is not the importer the rule
// governs.
const pubImportsTop = `package pub

import "` + app + `/internal/top"

func use() string { return top.Name }
`

// unparseable is a .go file whose import block the parser rejects: the
// guard cannot vouch for it, so it must be a tool error rather than a
// silent pass. ImportsOnly parsing reads the import block and stops — a
// broken function body is go vet's business, not this guard's.
const unparseable = "package base\n\nimport \"unterminated\n"

// stubPackages are minimal packages for every rule in threeLayerConfig: the
// guard's rename protection fails any run whose layer table names a package
// the tree lacks, so every fixture must carry one package per rule.
var stubPackages = map[string]string{
	"internal/base/base.go":   "package base\n\nconst Name = \"base\"\n",
	"internal/leaf/leaf.go":   "package leaf\n",
	"internal/mid/mid.go":     "package mid\n\nconst Name = \"mid\"\n",
	"internal/sibling/sib.go": "package sibling\n",
	"internal/top/top.go":     "package top\n\nconst Name = \"top\"\n",
}

// writeModule writes a fixture module — a go.mod, the given files, and the
// given layer table and exception list at the tool's default config paths —
// and returns the module root. A stub package is written for every rule in
// the layer table unless files provides one.
func writeModule(t *testing.T, config, exceptions string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if files == nil {
		files = make(map[string]string)
	}
	for name, contents := range stubPackages {
		if _, ok := files[name]; !ok {
			files[name] = contents
		}
	}
	files["go.mod"] = "module " + app + "\n\ngo 1.24.0\n"
	files["tools/importlayers/layers.txt"] = config
	files["tools/importlayers/exceptions.txt"] = exceptions
	for name, contents := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// check runs the guard over root and returns its exit code with both output
// streams combined — findings may go to either.
func check(t *testing.T, root string) (int, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run([]string{root}, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// want reports whether out carries every substring in subs, failing the test
// with the full output when one is missing.
func want(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Errorf("output is missing %q; got:\n%s", s, out)
		}
	}
}

// TestLegalImportsPass covers the three shapes the rule allows: a downward
// import, a same-layer import, and a package two layers down. A non-internal
// importer (pkg) is out of scope and not judged either way. The default
// config locations inside the scanned root are used, as make lint does.
func TestLegalImportsPass(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/base/base.go":   "package base\n\nconst Name = \"base\"\n",
		"internal/leaf/leaf.go":   "package leaf\n",
		"internal/mid/mid.go":     midImportsBase,
		"internal/sibling/sib.go": siblingImportsMid,
		"internal/top/top.go":     topImportsEverything,
		"pkg/pub/pub.go":          pubImportsTop,
	})
	code, out := check(t, root)
	if code != 0 {
		t.Fatalf("rejected legal imports (exit %d):\n%s", code, out)
	}
}

// TestUpwardImportFails is the rule the guard exists for: a lower layer
// importing a higher one fails, and the finding carries the file:line, both
// packages, both layers, and the fix.
func TestUpwardImportFails(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/base/base.go": baseImportsMid,
		"internal/mid/mid.go":   "package mid\n\nconst Name = \"mid\"\n",
	})
	code, out := check(t, root)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	// The import sits on line 3 of base.go.
	want(t, out, "internal/base/base.go:3", app+"/internal/base", app+"/internal/mid",
		"layer 1", "layer 2", "exception")
}

// TestSameLayerImportPasses pins same-layer legality on its own, so a
// refactor of the legal-import fixture cannot silently forbid it.
func TestSameLayerImportPasses(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/mid/mid.go":     "package mid\n\nconst Name = \"mid\"\n",
		"internal/sibling/sib.go": siblingImportsMid,
	})
	code, out := check(t, root)
	if code != 0 {
		t.Fatalf("rejected a same-layer import (exit %d):\n%s", code, out)
	}
}

// TestUnassignedPackageFails: a package no rule assigns cannot be judged, so
// a new internal package without a layer rule fails the run even when it
// imports nothing internal.
func TestUnassignedPackageFails(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/base/base.go":     "package base\n\nconst Name = \"base\"\n",
		"internal/orphan/orphan.go": "package orphan\n",
	})
	code, out := check(t, root)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (cannot judge an unassigned package):\n%s", code, out)
	}
	want(t, out, "internal/orphan")
}

// TestUnusedRuleFails is rename protection: a rule no package matches means
// the table and the tree have drifted (a package was renamed or deleted), so
// the table must be fixed in the same change.
func TestUnusedRuleFails(t *testing.T) {
	// A rule for a package the tree does not contain — the shape a rename or
	// deletion leaves behind.
	config := threeLayerConfig + "layer2 " + app + "/internal/ghost\n"
	root := writeModule(t, config, "# no exceptions\n", nil)
	code, out := check(t, root)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (rule matches no package):\n%s", code, out)
	}
	want(t, out, "internal/ghost")
}

// TestUnusedRuleFailsWhenOnlyImported: an import naming a package the tree
// lacks — a dangling import left by a rename, perhaps in a file gated to
// another platform — must not count as the package existing, or the rule it
// matches would never be reported as stale.
func TestUnusedRuleFailsWhenOnlyImported(t *testing.T) {
	config := threeLayerConfig + "layer2 " + app + "/internal/ghost\n"
	root := writeModule(t, config, "# no exceptions\n", map[string]string{
		"internal/top/top.go": "package top\n\nimport \"" + app + "/internal/ghost\"\n\nfunc use() string { return ghost.Name }\n",
	})
	code, out := check(t, root)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (rule matches no package in the tree):\n%s", code, out)
	}
	want(t, out, "internal/ghost")
}

// TestSymlinkedSourceIsScanned: the go tool compiles a .go symlink as the file
// it points to, so an upward import behind one is the package's own import and
// must fail like any other.
func TestSymlinkedSourceIsScanned(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/base/upward.txt": baseImportsMid,
	})
	if err := os.Symlink("upward.txt", filepath.Join(root, "internal/base/upward.go")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	code, out := check(t, root)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (upward import in a symlinked source):\n%s", code, out)
	}
	want(t, out, "internal/base/upward.go:3", app+"/internal/base", app+"/internal/mid")
}

// TestStaleExceptionFails covers both ways an exception can rot: its edge no
// longer exists, or the edge became legal again (so the exception papers over
// nothing and hides its own reason). Neither may pass silently.
func TestStaleExceptionFails(t *testing.T) {
	t.Run("edgeVanished", func(t *testing.T) {
		root := writeModule(t, threeLayerConfig,
			app+`/internal/top -> `+app+`/internal/ghost # no longer imported
`, map[string]string{
				"internal/base/base.go": "package base\n\nconst Name = \"base\"\n",
				"internal/mid/mid.go":   "package mid\n\nconst Name = \"mid\"\n",
				"internal/top/top.go":   topImportsEverything,
			})
		code, out := check(t, root)
		if code != 1 {
			t.Fatalf("exit %d, want 1 (stale exception):\n%s", code, out)
		}
		want(t, out, "internal/top", "internal/ghost")
	})
	t.Run("edgeNoLongerAViolation", func(t *testing.T) {
		root := writeModule(t, threeLayerConfig,
			app+`/internal/mid -> `+app+`/internal/base # no longer needed
`, map[string]string{
				"internal/base/base.go": "package base\n\nconst Name = \"base\"\n",
				"internal/mid/mid.go":   midImportsBase,
			})
		code, out := check(t, root)
		if code != 1 {
			t.Fatalf("exit %d, want 1 (exception on a legal edge):\n%s", code, out)
		}
		want(t, out, "internal/mid", "internal/base")
	})
}

// TestExceptionAllowsAnUpwardImport is the other half of the exception
// contract: a reviewed exception turns a real upward import into a pass, and
// the real-repo test below holds the exception file to the same standard.
func TestExceptionAllowsAnUpwardImport(t *testing.T) {
	root := writeModule(t, threeLayerConfig,
		app+`/internal/base -> `+app+`/internal/mid # reviewed: base serves mid's paths
`, map[string]string{
			"internal/base/base.go": baseImportsMid,
			"internal/mid/mid.go":   "package mid\n\nconst Name = \"mid\"\n",
		})
	code, out := check(t, root)
	if code != 0 {
		t.Fatalf("rejected an exceptioned upward import (exit %d):\n%s", code, out)
	}
}

// TestTagGatedFilesAreSeen parses both tag-gated fixtures no matter the host
// GOOS: a !linux file's upward import must fail on linux, a linux-only
// file's upward import must fail everywhere else.
func TestTagGatedFilesAreSeen(t *testing.T) {
	t.Run("notLinux", func(t *testing.T) {
		root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
			"internal/base/base.go":  "package base\n\nconst Name = \"base\"\n",
			"internal/mid/mid.go":    "package mid\n\nconst Name = \"mid\"\n",
			"internal/base/other.go": tagGatedImportsMid,
		})
		code, out := check(t, root)
		if code != 1 {
			t.Fatalf("exit %d, want 1 (a build-tag-gated file is still the package's imports):\n%s", code, out)
		}
		want(t, out, "internal/base/other.go")
	})
	t.Run("linuxOnly", func(t *testing.T) {
		root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
			"internal/mid/mid.go":     "package mid\n\nconst Name = \"mid\"\n",
			"internal/sibling/sib.go": tagGatedLinuxImportsTop,
			"internal/top/top.go":     "package top\n\nconst Name = \"top\"\n",
		})
		code, out := check(t, root)
		if code != 1 {
			t.Fatalf("exit %d, want 1 (a linux-gated file must fail on any GOOS):\n%s", code, out)
		}
		want(t, out, "internal/sibling/sib.go")
	})
}

// TestInternalImportingPkgOrCmdFails pins the other boundary: internal code
// reaching this module's pkg/… or cmd/… is a violation regardless of layers.
func TestInternalImportingPkgOrCmdFails(t *testing.T) {
	t.Run("pkg", func(t *testing.T) {
		root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
			"internal/base/base.go": baseImportsPkg,
			"pkg/pub/pub.go":        "package pub\n\nconst Name = \"pub\"\n",
		})
		code, out := check(t, root)
		if code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, out)
		}
		want(t, out, "internal/base/base.go:3", app+"/pkg/pub")
	})
	t.Run("cmd", func(t *testing.T) {
		root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
			"internal/mid/mid.go": midImportsCmd,
			"cmd/prog/main.go":    "package prog\n\nconst Name = \"prog\"\n",
		})
		code, out := check(t, root)
		if code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, out)
		}
		want(t, out, "internal/mid/mid.go:3", app+"/cmd/prog")
	})
}

// TestTestAndTestdataFilesAreSkipped: test-only imports are the test's own
// business, and a testdata fixture is invisible to the go tool, so both must
// be skipped — here both carry upward imports that would fail if scanned.
func TestTestAndTestdataFilesAreSkipped(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/base/base.go": "package base\n\nconst Name = \"base\"\n",
		"internal/mid/mid.go":   "package mid\n\nconst Name = \"mid\"\n",
		"internal/base/base_test.go": `package base

import "` + app + `/internal/mid"

func use() string { return mid.Name }
`,
		"internal/base/testdata/bad.go": baseImportsMid,
	})
	code, out := check(t, root)
	if code != 0 {
		t.Fatalf("judged test or testdata files (exit %d):\n%s", code, out)
	}
}

// TestUnparseableFileIsAToolError: a file the guard cannot parse must fail
// the check, never pass it silently.
func TestUnparseableFileIsAToolError(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/base/base.go": unparseable,
	})
	code, out := check(t, root)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (unparseable file):\n%s", code, out)
	}
}

// TestUnderscoreDirIsSkipped: the go tool ignores directories whose names
// begin with "_", so a scratch directory holds no package the tool would
// ever build and the guard must not judge its files either — here a file
// under one carries an upward import that must not fail the run.
func TestUnderscoreDirIsSkipped(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/base/base.go":             "package base\n\nconst Name = \"base\"\n",
		"internal/mid/mid.go":               "package mid\n\nconst Name = \"mid\"\n",
		"internal/base/_scratch/scratch.go": baseImportsMid,
	})
	code, out := check(t, root)
	if code != 0 {
		t.Fatalf("judged a _-prefixed directory (exit %d):\n%s", code, out)
	}
}

// TestUnderscoreAndHiddenFilesAreSkipped: the go tool also ignores FILES
// whose base name begins with "_" or ".", not just such directories, so a
// scratch or editor-drop file holds no import the tool would ever build and
// the guard must not judge it either — here both carry an upward import
// that must not fail the run.
func TestUnderscoreAndHiddenFilesAreSkipped(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/base/base.go":     "package base\n\nconst Name = \"base\"\n",
		"internal/mid/mid.go":       "package mid\n\nconst Name = \"mid\"\n",
		"internal/base/_scratch.go": baseImportsMid,
		"internal/base/.hidden.go":  baseImportsMid,
	})
	code, out := check(t, root)
	if code != 0 {
		t.Fatalf("judged a _- or .-prefixed file (exit %d):\n%s", code, out)
	}
}

// TestMalformedLayerNumberFails: a sign after "layer" must be a config
// error, not a silently accepted rule — strconv.Atoi takes "layer+2" as
// layer 2, so the number text has to round-trip through strconv.Itoa to
// prove it is bare digits.
func TestMalformedLayerNumberFails(t *testing.T) {
	root := writeModule(t, threeLayerConfig+"layer+2 "+app+"/internal/extra\n",
		"# no exceptions\n", map[string]string{
			"internal/extra/extra.go": "package extra\n",
		})
	code, out := check(t, root)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (a sign after \"layer\" is not a layer number):\n%s", code, out)
	}
	want(t, out, "layer+2")
}

// TestMalformedExceptionEdgeFails: an exception line whose imported side
// still contains "->" does not name one edge, and splitting on the first
// "->" would silently read the middle package as the imported side — the
// entry must be rejected as malformed, not turned into a stale finding.
func TestMalformedExceptionEdgeFails(t *testing.T) {
	root := writeModule(t, threeLayerConfig,
		app+`/internal/base -> `+app+`/internal/mid -> `+app+`/internal/top # reviewed
`, map[string]string{
			"internal/base/base.go": "package base\n\nconst Name = \"base\"\n",
			"internal/mid/mid.go":   "package mid\n\nconst Name = \"mid\"\n",
			"internal/top/top.go":   "package top\n\nconst Name = \"top\"\n",
		})
	code, out := check(t, root)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (an exception edge is one `importer -> imported`):\n%s", code, out)
	}
	want(t, out, "internal/base", "internal/mid")
}

// TestMissingConfigIsAToolError: without the layer table nothing can be
// judged, so a missing config is a tool error, not a pass.
func TestMissingConfigIsAToolError(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/base/base.go": "package base\n\nconst Name = \"base\"\n",
	})
	if err := os.Remove(filepath.Join(root, "tools", "importlayers", "layers.txt")); err != nil {
		t.Fatal(err)
	}
	code, out := check(t, root)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (missing layer table):\n%s", code, out)
	}
}

// TestRealRepoPasses runs the guard over this repository: the checked-in
// layer table must assign every internal package, cover every rule, carry no
// stale exception, and leave every current import legal. It is also proof
// the guard never leans on `go list`, which cannot list this module while
// the dashboard embed is unbuilt.
func TestRealRepoPasses(t *testing.T) {
	code, out := check(t, "../..")
	if code != 0 {
		t.Fatalf("the real repository failed the layer guard (exit %d):\n%s", code, out)
	}
}

package main

// Fixtures are parsed, not compiled: they need valid Go syntax, not resolvable imports.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const app = "example.com/app"

const threeLayerConfig = `# layer1 — foundations: no internal deps
layer1 ` + app + `/internal/base
layer1 ` + app + `/internal/leaf
# layer2 — primitives above the foundations
layer2 ` + app + `/internal/mid
layer2 ` + app + `/internal/sibling
# layer3 — orchestration on top
layer3 ` + app + `/internal/top
`

const baseImportsMid = `package base

import "` + app + `/internal/mid"

func use() string { return mid.Name }
`

const midImportsBase = `package mid

import "` + app + `/internal/base"

func use() string { return base.Name }
`

const topImportsEverything = `package top

import (
	"` + app + `/internal/base"
	"` + app + `/internal/mid"
)

func use() string { return mid.Name + base.Name }
`

const siblingImportsMid = `package sibling

import "` + app + `/internal/mid"

func use() string { return mid.Name }
`

const tagGatedImportsMid = `//go:build !linux

package base

import "` + app + `/internal/mid"

func use() string { return mid.Name }
`

const tagGatedLinuxImportsTop = `//go:build linux

package sibling

import "` + app + `/internal/top"

func use() string { return top.Name }
`

const baseImportsPkg = `package base

import "` + app + `/pkg/pub"

func use() string { return pub.Name }
`

const midImportsCmd = `package mid

import "` + app + `/cmd/prog"

func use() string { return prog.Name }
`

const pubImportsTop = `package pub

import "` + app + `/internal/top"

func use() string { return top.Name }
`

// ImportsOnly reads the import block; a broken body is go vet's business.
const unparseable = "package base\n\nimport \"unterminated\n"

// Rename protection fails any run whose table names a package the tree lacks.
var stubPackages = map[string]string{
	"internal/base/base.go":   "package base\n\nconst Name = \"base\"\n",
	"internal/leaf/leaf.go":   "package leaf\n",
	"internal/mid/mid.go":     "package mid\n\nconst Name = \"mid\"\n",
	"internal/sibling/sib.go": "package sibling\n",
	"internal/top/top.go":     "package top\n\nconst Name = \"top\"\n",
}

// A stub package is written for every rule unless files provides one.
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

func check(t *testing.T, root string) (int, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run([]string{root}, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

func want(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Errorf("output is missing %q; got:\n%s", s, out)
		}
	}
}

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

func TestUnusedRuleFails(t *testing.T) {
	config := threeLayerConfig + "layer2 " + app + "/internal/ghost\n"
	root := writeModule(t, config, "# no exceptions\n", nil)
	code, out := check(t, root)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (rule matches no package):\n%s", code, out)
	}
	want(t, out, "internal/ghost")
}

// A dangling import must not count as the package existing.
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

func TestUnparseableFileIsAToolError(t *testing.T) {
	root := writeModule(t, threeLayerConfig, "# no exceptions\n", map[string]string{
		"internal/base/base.go": unparseable,
	})
	code, out := check(t, root)
	if code != 2 {
		t.Fatalf("exit %d, want 2 (unparseable file):\n%s", code, out)
	}
}

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

// strconv.Atoi takes "layer+2" as layer 2, so the number text must round-trip.
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

// Splitting on the first "->" would read the middle package as imported.
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

// Also proof the guard never leans on `go list`, which fails on the unbuilt embed.
func TestRealRepoPasses(t *testing.T) {
	code, out := check(t, "../..")
	if code != 0 {
		t.Fatalf("the real repository failed the layer guard (exit %d):\n%s", code, out)
	}
}

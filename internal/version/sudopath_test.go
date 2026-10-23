package version

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoMessageRunsStratumByNameUnderSudo fails on any string in the module's
// non-test Go code that tells an operator to run `sudo stratum`. sudo on stock
// Rocky Linux searches only its secure_path, which leaves out the
// /usr/local/bin stratum is installed in, so that command fails with "command
// not found". A message names the binary by StratumInstallPath instead.
func TestNoMessageRunsStratumByNameUnderSudo(t *testing.T) {
	const bare = "sudo stratum"

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root: %v", err)
	}

	fset := token.NewFileSet()
	var checked int
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		checked++
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(src, []byte(bare)) {
			return nil
		}
		// Only string literals count: a comment may name the failing command
		// to explain why the full path is used.
		file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(s, bare) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d tells the operator to run %q, which sudo does not find on stock Rocky Linux; "+
					"name the binary by version.StratumInstallPath", rel, fset.Position(lit.Pos()).Line, bare)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if checked == 0 {
		t.Fatal("no Go files walked; the guard is not actually checking anything")
	}
}

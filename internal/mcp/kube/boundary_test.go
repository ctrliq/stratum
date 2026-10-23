package kube

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoMCPSDKImport enforces the split this package exists for: it is a
// Kubernetes client library, and it must stay testable against a real
// apiserver with no protocol harness in the way.
func TestNoMCPSDKImport(t *testing.T) {
	const forbidden = "modelcontextprotocol"

	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var checked int

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++

		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if strings.Contains(p, forbidden) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s imports %q: internal/mcp/kube must not depend on the MCP SDK", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// A walk that found nothing would pass silently, which is the one way
	// this test could stop meaning anything.
	if checked == 0 {
		t.Fatal("no Go files walked; the guard is not actually checking anything")
	}
}

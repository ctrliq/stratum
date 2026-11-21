// Command commentcheck fails on Go comments that break the rules in AGENTS.md ("Comments").
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
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const maxCommentLen = 100

var skipDirs = map[string]bool{
	"testdata":               true,
	"node_modules":           true,
	"vendor":                 true,
	"lint-licenses-testdata": true,
}

var directive = regexp.MustCompile(`^//(go:|line |export |nolint|lint:|\+build )`)

var generated = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// go test compares an Example's output block line by line, so the block is test data.
var exampleOutput = regexp.MustCompile(`^//\s*(Unordered output|Output):`)

var banned = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`§`), "spec section reference"},
	{regexp.MustCompile(`\bKUB-[0-9]+\b`), "tracker reference"},
	{regexp.MustCompile(`(^|[\s(])#[0-9]+\b`), "issue or pull request reference"},
	{regexp.MustCompile(`(?i)\b(TODO|FIXME|XXX)\b`), "task note"},
	{regexp.MustCompile(`(?i)\b(red|fails to build) until\b|\bwritten first\b|\bdoes not exist yet\b|\bnot yet implemented\b|\bthis (PR|pull request|ticket)\b|\bfor now\b`), "task-state narration"},
	{regexp.MustCompile(`(?i)\bused to\b|\boriginally\b|\bhistorically\b|\bthe old (code|path|behaviou?r|version|way|format|implementation)\b|\b(after|before|until) (the|this) (fix|change|refactor)\b|\bthe bug\b|\bbug (where|that|in)\b|\bwas (changed|fixed|introduced|rewritten)\b|\bno longer (reads|uses|finds|calls|runs|writes|checks)\b|\bonce (accepted|allowed|returned|failed|passed) (a|an|the)\b|\bregression guard\b`), "history narration"},
	{regexp.MustCompile(`\b[a-z]\w* now (uses|returns|refuses|reads|writes|fails|checks|needs|runs)\b`), "history narration"},
	{regexp.MustCompile(`(?i)\b(we|we're|we've|let's)\b`), "first-person narration"},
	{regexp.MustCompile(`(?i)^\s*(//|/\*|\*)?\s*step [0-9]`), "step-by-step narration; extract named functions"},
}

type finding struct {
	pos token.Position
	msg string
}

func main() {
	roots := os.Args[1:]
	if len(roots) == 0 {
		roots = []string{"."}
	}
	n, err := run(roots, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "commentcheck:", err)
		os.Exit(2)
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "commentcheck: %d comment(s) break the rules in AGENTS.md (\"Comments\")\n", n)
		os.Exit(1)
	}
}

func run(roots []string, out io.Writer) (int, error) {
	var all []finding
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			found, err := checkFile(path, src)
			all = append(all, found...)
			return err
		})
		if err != nil {
			return 0, err
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].pos.Filename != all[j].pos.Filename {
			return all[i].pos.Filename < all[j].pos.Filename
		}
		return all[i].pos.Line < all[j].pos.Line
	})
	for _, f := range all {
		fmt.Fprintf(out, "%s:%d: %s\n", f.pos.Filename, f.pos.Line, f.msg)
	}
	return len(all), nil
}

func checkFile(path string, src []byte) ([]finding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	multiLine := map[*ast.CommentGroup]bool{}
	if publicAPIFile(path) {
		multiLine = exportedDocs(file)
	}
	var out []finding
	for _, g := range file.Comments {
		out = append(out, checkGroup(fset, g, multiLine[g])...)
	}
	return out, nil
}

// Importers build against these packages and read their exported docs in full.
var publicAPIDirs = []string{"pkg/cli"}

func publicAPIFile(path string) bool {
	p := filepath.ToSlash(filepath.Clean(path))
	if strings.HasSuffix(p, "_test.go") {
		return false
	}
	dir := filepath.ToSlash(filepath.Dir(p))
	for _, d := range publicAPIDirs {
		if dir == d || strings.HasSuffix(dir, "/"+d) {
			return true
		}
	}
	return false
}

func exportedDocs(file *ast.File) map[*ast.CommentGroup]bool {
	docs := map[*ast.CommentGroup]bool{}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Doc != nil && d.Name.IsExported() {
				docs[d.Doc] = true
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				if !specExported(spec) {
					continue
				}
				if d.Doc != nil {
					docs[d.Doc] = true
				}
				if doc := specDoc(spec); doc != nil {
					docs[doc] = true
				}
				if ts, ok := spec.(*ast.TypeSpec); ok {
					memberDocs(ts.Type, docs)
				}
			}
		}
	}
	return docs
}

func memberDocs(t ast.Expr, docs map[*ast.CommentGroup]bool) {
	var fields *ast.FieldList
	switch x := t.(type) {
	case *ast.StructType:
		fields = x.Fields
	case *ast.InterfaceType:
		fields = x.Methods
	}
	if fields == nil {
		return
	}
	for _, f := range fields.List {
		if f.Doc == nil {
			continue
		}
		for _, n := range f.Names {
			if n.IsExported() {
				docs[f.Doc] = true
				break
			}
		}
	}
}

func specExported(spec ast.Spec) bool {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return s.Name.IsExported()
	case *ast.ValueSpec:
		for _, n := range s.Names {
			if n.IsExported() {
				return true
			}
		}
	}
	return false
}

func specDoc(spec ast.Spec) *ast.CommentGroup {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return s.Doc
	case *ast.ValueSpec:
		return s.Doc
	}
	return nil
}

func checkGroup(fset *token.FileSet, g *ast.CommentGroup, multiLine bool) []finding {
	for _, c := range g.List {
		if exampleOutput.MatchString(c.Text) {
			return nil
		}
	}
	var out []finding
	lines := 0
	for _, c := range g.List {
		if c.Text == "//" || directive.MatchString(c.Text) || generated.MatchString(c.Text) {
			continue
		}
		pos := fset.Position(c.Slash)
		for i, line := range strings.Split(c.Text, "\n") {
			lines++
			p := pos
			p.Line += i
			if n := utf8.RuneCountInString(strings.TrimSpace(line)); n > maxCommentLen {
				out = append(out, finding{p, fmt.Sprintf("comment line is %d characters; the limit is %d", n, maxCommentLen)})
			}
			for _, b := range banned {
				if m := b.re.FindString(line); m != "" {
					out = append(out, finding{p, fmt.Sprintf("%s %q", b.why, strings.TrimSpace(m))})
				}
			}
		}
	}
	if lines > 1 && !multiLine {
		out = append(out, finding{fset.Position(g.Pos()), fmt.Sprintf("comment is %d lines; the limit is one", lines)})
	}
	return out
}

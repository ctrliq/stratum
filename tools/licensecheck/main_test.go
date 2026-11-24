package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Findings are one line starting with FAIL or WARN (or ::error/::warning in CI).
const (
	failLine      = "FAIL"
	warnLine      = "WARN"
	noteLine      = "NOTE"
	annotatedFail = "::error"
	annotatedWarn = "::warning"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type result struct {
	code int
	out  string
}

// GITHUB_ACTIONS unset, so findings print plain even when the test runs in CI.
func check(t *testing.T, args ...string) result {
	t.Helper()
	unsetenv(t, "GITHUB_ACTIONS")
	return invoke(t, args...)
}

func invoke(t *testing.T, args ...string) result {
	t.Helper()
	var out lockedBuffer
	code := run(args, &out, &out)
	r := result{code: code, out: out.String()}
	t.Logf("licensecheck %s\nexit %d\n%s", strings.Join(args, " "), code, r.out)
	return r
}

func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "") // restores the original value when the test ends
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func (r result) lines(prefix string) []string {
	var got []string
	for _, line := range strings.Split(r.out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			got = append(got, line)
		}
	}
	return got
}

func (r result) naming(prefix, name string) []string {
	var got []string
	for _, line := range r.lines(prefix) {
		if mentions(line, name) {
			got = append(got, line)
		}
	}
	return got
}

// Whole-token match: example.com/a never matches example.com/a/b, MIT not MIT-0.
func mentions(line, s string) bool {
	re := regexp.MustCompile(`(?:^|[^\w./@-])` + regexp.QuoteMeta(s) + `(?:$|[^\w./-]|\.(?:$|\s))`)
	return re.MatchString(line)
}

func wantExit(t *testing.T, r result, code int) {
	t.Helper()
	if r.code != code {
		t.Errorf("exit code %d, want %d; output:\n%s", r.code, code, r.out)
	}
}

func wantFail(t *testing.T, r result, prefix, pkg string, licenses ...string) {
	t.Helper()
	lines := r.naming(prefix, pkg)
	for _, line := range lines {
		for _, id := range licenses {
			if mentions(line, id) {
				return
			}
		}
	}
	t.Errorf("no %s line names %s with %s; %s lines naming it: %q\noutput:\n%s",
		prefix, pkg, strings.Join(licenses, " or "), prefix, lines, r.out)
}

func wantWarn(t *testing.T, r result, pkg string) {
	t.Helper()
	if len(r.naming(warnLine, pkg)) == 0 {
		t.Errorf("no WARN line names %s; output:\n%s", pkg, r.out)
	}
	if got := r.naming(failLine, pkg); len(got) > 0 {
		t.Errorf("%s warns, but it failed: %q", pkg, got)
	}
}

func wantPass(t *testing.T, r result, pkg string) {
	t.Helper()
	for _, prefix := range []string{failLine, warnLine} {
		if got := r.naming(prefix, pkg); len(got) > 0 {
			t.Errorf("%s passes, but got: %q", pkg, got)
		}
	}
}

// Every prefix line must name one of names: nothing else is reported.
func wantOnly(t *testing.T, r result, prefix string, names ...string) {
	t.Helper()
	for _, line := range r.lines(prefix) {
		named := false
		for _, name := range names {
			named = named || mentions(line, name)
		}
		if !named {
			t.Errorf("unexpected %s line (want only %s): %q", prefix, strings.Join(names, ", "), line)
		}
	}
}

// A verbatim license file from testdata/licenses, copied from real modules.
func licenseText(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "licenses", id+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// unknownText is license-shaped prose that matches no known license.
const unknownText = `Permission is granted to anyone who asks the author nicely,
and to nobody else, on alternate Tuesdays only.
`

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// module is one dependency module of a fixture.
type module struct {
	path     string
	dir      string
	requires []string          // module paths its go.mod requires, at v0.0.0
	files    map[string]string // every file but go.mod, relative to dir
	copyFrom string            // a testdata tree to copy instead, with its own go.mod
}

// Lays out the main module in root/app, each dependency replaced by directory.
func fixture(t *testing.T, app map[string]string, deps ...module) string {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	replace := map[string]string{}
	for _, m := range deps {
		dir := filepath.Join(root, filepath.FromSlash(m.dir))
		if m.copyFrom != "" {
			if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.CopyFS(dir, os.DirFS(m.copyFrom)); err != nil {
				t.Fatal(err)
			}
		} else {
			files := map[string]string{"go.mod": goMod(m.path, m.requires, nil)}
			for name, contents := range m.files {
				files[name] = contents
			}
			writeTree(t, dir, files)
		}
		rel, err := filepath.Rel(appDir, dir)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, "../") {
			rel = "./" + rel
		}
		replace[m.path] = rel
	}
	files := map[string]string{"go.mod": goMod("example.com/app", nil, replace)}
	for name, contents := range app {
		files[name] = contents
	}
	writeTree(t, appDir, files)
	return appDir
}

// A go.mod requiring every module in requires and replace at v0.0.0.
func goMod(path string, requires []string, replace map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "module %s\n\ngo 1.22\n", path)
	reqs := append([]string(nil), requires...)
	for mod := range replace {
		reqs = append(reqs, mod)
	}
	sort.Strings(reqs)
	if len(reqs) > 0 {
		b.WriteString("\nrequire (\n")
		for _, mod := range reqs {
			fmt.Fprintf(&b, "\t%s v0.0.0\n", mod)
		}
		b.WriteString(")\n")
	}
	if len(replace) > 0 {
		b.WriteString("\nreplace (\n")
		for _, mod := range reqs {
			if dir, ok := replace[mod]; ok {
				fmt.Fprintf(&b, "\t%s => %s\n", mod, dir)
			}
		}
		b.WriteString(")\n")
	}
	return b.String()
}

func spdx(expr string) string { return "// SPDX-License-Identifier: " + expr }

func src(header, name string, imports ...string) string {
	var b strings.Builder
	if header != "" {
		b.WriteString(header + "\n\n")
	}
	fmt.Fprintf(&b, "package %s\n", name)
	for _, imp := range imports {
		fmt.Fprintf(&b, "\nimport _ %q\n", imp)
	}
	return b.String()
}

func mainFile(imports ...string) string {
	return src("", "main", imports...) + "\nfunc main() {}\n"
}

// The shipped build's first target, passed explicitly so no test depends on defaults.
var goTarget = []string{"-goos", "linux", "-goarch", "amd64"}

func goArgs(dir string, extra ...string) []string {
	args := append([]string{"-C", dir}, goTarget...)
	return append(args, extra...)
}

// A tool error is never a pass or an ordinary failure: the result means nothing.
func TestToolErrorsExit2(t *testing.T) {
	scratch := t.TempDir()
	missing := filepath.Join(scratch, "absent")
	malformed := filepath.Join(scratch, "malformed.json")
	writeTree(t, scratch, map[string]string{
		"malformed.json": `[{"name": "left-pad", "version": "1.3.0", "license": "MIT"`,
		"empty.json":     `[]`,
	})
	for name, args := range map[string][]string{
		"unknown flag":         {"-no-such-flag", "."},
		"missing -C directory": goArgs(missing, "."),
		"missing npm file":     {"-npm", filepath.Join(missing, "licenses.json")},
		"malformed npm file":   {"-npm", malformed},
		"empty npm list":       {"-npm", filepath.Join(scratch, "empty.json")},
	} {
		t.Run(name, func(t *testing.T) {
			r := check(t, args...)
			wantExit(t, r, 2)
		})
	}
}

// Each finding becomes a workflow command, shown as an annotation in CI.
func TestGitHubActionsAnnotations(t *testing.T) {
	list := writeNpmList(t, `[
  {"name": "mpl-thing", "version": "0.1.0", "license": "MPL-2.0"},
  {"name": "no-license", "version": "1.0.0", "license": ""}
]`)

	t.Run("GITHUB_ACTIONS=true", func(t *testing.T) {
		t.Setenv("GITHUB_ACTIONS", "true")
		r := invoke(t, "-npm", list)
		wantExit(t, r, 1)
		wantFail(t, r, annotatedFail, "mpl-thing", "MPL-2.0")
		if len(r.naming(annotatedWarn, "no-license")) == 0 {
			t.Errorf("no ::warning line names no-license; output:\n%s", r.out)
		}
	})

	t.Run("outside GitHub Actions", func(t *testing.T) {
		r := check(t, "-npm", list)
		wantExit(t, r, 1)
		wantFail(t, r, failLine, "mpl-thing", "MPL-2.0")
		wantWarn(t, r, "no-license")
		if got := r.lines("::"); len(got) > 0 {
			t.Errorf("printed workflow commands outside GitHub Actions: %q", got)
		}
	})
}

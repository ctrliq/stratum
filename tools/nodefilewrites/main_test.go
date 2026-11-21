package main

// Fixtures are parsed, not compiled: they need valid Go syntax, not resolvable imports.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const rawWriteFile = `package pki

import "os"

func save(data []byte) error {
	return os.WriteFile("/var/lib/stratum/pki/ca.crt", data, 0o600)
}
`

const rawWriteFileLine = 6

const rawKubeconfigWrite = `package kubeconfig

import (
	"k8s.io/client-go/tools/clientcmd"
)

func write(cfg clientcmdapi.Config, path string) error {
	return clientcmd.WriteToFile(cfg, path)
}
`

const rawKubeconfigWriteLine = 8

const aliasedWrite = `package pki

import fs "os"

func save(data []byte) error {
	return fs.WriteFile("/var/lib/stratum/pki/ca.crt", data, 0o600)
}
`

const aliasedKubeconfigWrite = `package kubeconfig

import cc "k8s.io/client-go/tools/clientcmd"

func write(cfg clientcmdapi.Config, path string) error {
	return cc.WriteToFile(cfg, path)
}
`

const atomicWrite = `package pki

import (
	"os"

	"github.com/ctrliq/stratum/internal/atomicfile"
)

func save(data []byte) error {
	if err := atomicfile.WriteFile("/var/lib/stratum/pki/ca.crt", data, 0o600); err != nil {
		return err
	}
	_, err := os.ReadFile("/var/lib/stratum/pki/ca.crt")
	return err
}
`

const unparseable = "package pki\n\nfunc {"

func writeTree(t *testing.T, files map[string]string) {
	t.Helper()
	root := t.TempDir()
	for name, contents := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
}

func withGuarded(t *testing.T, dirs ...string) {
	t.Helper()
	old := guarded
	guarded = make(map[string]bool, len(dirs))
	for _, d := range dirs {
		guarded[d] = true
	}
	t.Cleanup(func() { guarded = old })
}

func withAllowlist(t *testing.T, entries map[string]allowEntry) {
	t.Helper()
	old := allowlist
	allowlist = entries
	t.Cleanup(func() { allowlist = old })
}

type captured struct {
	stdout strings.Builder
	stderr strings.Builder
}

func (c *captured) all() string { return c.stdout.String() + c.stderr.String() }

func TestPlainWriteFileInWriterPackageFailsLint(t *testing.T) {
	withGuarded(t, "internal/pki")
	// writer.go, not pki.go: the real pki.go is in the package-level allowlist.
	writeTree(t, map[string]string{"internal/pki/writer.go": rawWriteFile})

	var out captured
	code := run([]string{"."}, &out.stdout, &out.stderr)
	if code != 1 {
		t.Fatalf("run exit = %d, want 1; output:\n%s", code, out.all())
	}
	for _, want := range []string{
		"internal/pki/writer.go:" + fmt.Sprint(rawWriteFileLine),
		"os.WriteFile",
		"owning package",
	} {
		if !strings.Contains(out.all(), want) {
			t.Errorf("finding does not mention %q; got:\n%s", want, out.all())
		}
	}
}

func TestClientcmdWriteToFileFailsLint(t *testing.T) {
	withGuarded(t, "internal/kubeconfig")
	// writer.go, not kubeconfig.go: a fixture at that real path must stay clean.
	writeTree(t, map[string]string{"internal/kubeconfig/writer.go": rawKubeconfigWrite})

	var out captured
	code := run([]string{"."}, &out.stdout, &out.stderr)
	if code != 1 {
		t.Fatalf("run exit = %d, want 1; output:\n%s", code, out.all())
	}
	for _, want := range []string{
		"internal/kubeconfig/writer.go:" + fmt.Sprint(rawKubeconfigWriteLine),
		"clientcmd.WriteToFile",
		"owning package",
	} {
		if !strings.Contains(out.all(), want) {
			t.Errorf("finding does not mention %q; got:\n%s", want, out.all())
		}
	}
}

func TestEveryNodeFileWriterPackageIsGuarded(t *testing.T) {
	writerPkgs := []string{
		"internal/pki",
		"internal/kubeconfig",
		"internal/profilefiles",
		"internal/certs",
		"internal/nodestate",
		"internal/runtime",
		"internal/staticpod",
		"internal/upgrade",
	}
	files := make(map[string]string, len(writerPkgs))
	wantNames := make([]string, 0, len(writerPkgs))
	for _, dir := range writerPkgs {
		pkg := dir[strings.LastIndex(dir, "/")+1:]
		files[dir+"/writer.go"] = fmt.Sprintf(`package %s

import "os"

func save(data []byte) error {
	return os.WriteFile("/var/lib/stratum/state", data, 0o600)
}
`, pkg)
		wantNames = append(wantNames, dir+"/writer.go:6")
	}
	withGuarded(t, writerPkgs...)
	writeTree(t, files)

	var out captured
	if code := run([]string{"."}, &out.stdout, &out.stderr); code != 1 {
		t.Fatalf("run exit = %d, want 1; output:\n%s", code, out.all())
	}
	for _, want := range wantNames {
		if !strings.Contains(out.all(), want) {
			t.Errorf("no finding for %s; got:\n%s", want, out.all())
		}
	}
}

func TestAtomicWriterAndReadsAreNotViolations(t *testing.T) {
	withGuarded(t, "internal/pki")
	// pki.go is a real allowlisted path, so drop the allowlist for this test.
	withAllowlist(t, nil)
	writeTree(t, map[string]string{"internal/pki/pki.go": atomicWrite})

	var out captured
	if code := run([]string{"."}, &out.stdout, &out.stderr); code != 0 {
		t.Fatalf("run exit = %d, want 0 (atomic writer passes the guard); output:\n%s", code, out.all())
	}
}

func TestWritesOutsideTheWriterPackagesPass(t *testing.T) {
	withGuarded(t, "internal/pki")
	writeTree(t, map[string]string{
		"internal/registry/registry.go": strings.Replace(rawWriteFile, "package pki", "package registry", 1),
		"internal/pkia/notpki.go":       strings.Replace(rawWriteFile, "package pki", "package pkia", 1),
	})

	var out captured
	if code := run([]string{"."}, &out.stdout, &out.stderr); code != 0 {
		t.Fatalf("run exit = %d, want 0 (unowned packages are not enforced); output:\n%s", code, out.all())
	}
}

func TestTestFilesAndTestDataAreExempt(t *testing.T) {
	withGuarded(t, "internal/pki")
	// pki.go is a real allowlisted path, so drop the allowlist for this test.
	withAllowlist(t, nil)
	writeTree(t, map[string]string{
		"internal/pki/pki.go":            atomicWrite,
		"internal/pki/pki_test.go":       rawWriteFile,
		"internal/pki/testdata/x.go":     rawWriteFile,
		"internal/pki/fixture/x.go":      rawWriteFile,
		"internal/pki/vendor/v/x.go":     rawWriteFile,
		"internal/pki/.hidden/x.go":      rawWriteFile,
		"internal/pki/node_modules/x.go": rawWriteFile,
	})

	var out captured
	if code := run([]string{"."}, &out.stdout, &out.stderr); code != 1 {
		t.Fatalf("run exit = %d, want 1 (fixture/x.go is still production code); output:\n%s", code, out.all())
	}
	if !strings.Contains(out.all(), "internal/pki/fixture/x.go:6") {
		t.Errorf("finding does not mention the violating file; got:\n%s", out.all())
	}
	if strings.Contains(out.all(), "pki_test.go") || strings.Contains(out.all(), "testdata") {
		t.Errorf("test files or testdata were flagged; got:\n%s", out.all())
	}
}

func TestRenamedImportDoesNotDodgeTheGuard(t *testing.T) {
	testCases := []struct {
		name string
		file string
		dir  string
	}{
		{"aliased os", aliasedWrite, "internal/pki"},
		{"aliased clientcmd", aliasedKubeconfigWrite, "internal/kubeconfig"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			withGuarded(t, tc.dir)
			writeTree(t, map[string]string{tc.dir + "/writer.go": tc.file})

			var out captured
			if code := run([]string{"."}, &out.stdout, &out.stderr); code != 1 {
				t.Fatalf("run exit = %d, want 1; output:\n%s", code, out.all())
			}
		})
	}
}

func TestAllowlistedWriterPasses(t *testing.T) {
	withGuarded(t, "internal/pki")
	writeTree(t, map[string]string{
		"internal/pki/pki.go":   rawWriteFile,
		"internal/pki/other.go": rawWriteFile,
	})

	t.Run("allowlisted file passes", func(t *testing.T) {
		withAllowlist(t, map[string]allowEntry{
			"internal/pki/pki.go": {"deliberately not start-critical: writes a zero-byte marker", 1},
		})
		var out captured
		if code := run([]string{"."}, &out.stdout, &out.stderr); code != 1 {
			t.Fatalf("run exit = %d, want 1 (only other.go is not allowlisted); output:\n%s", code, out.all())
		}
		if !strings.Contains(out.all(), "internal/pki/other.go") {
			t.Errorf("finding does not name the un-allowlisted file; got:\n%s", out.all())
		}
		if strings.Contains(out.all(), "internal/pki/pki.go") {
			t.Errorf("allowlisted file was flagged; got:\n%s", out.all())
		}
	})

	t.Run("every file allowlisted passes", func(t *testing.T) {
		withAllowlist(t, map[string]allowEntry{
			"internal/pki/pki.go":   {"deliberately not start-critical: writes a zero-byte marker", 1},
			"internal/pki/other.go": {"deliberately not start-critical: writes a zero-byte marker", 1},
		})
		var out captured
		if code := run([]string{"."}, &out.stdout, &out.stderr); code != 0 {
			t.Fatalf("run exit = %d, want 0 (both files allowlisted); output:\n%s", code, out.all())
		}
	})
}

func TestAllowlistedFileWithANewRawWriteFails(t *testing.T) {
	withGuarded(t, "internal/pki")
	writeTree(t, map[string]string{"internal/pki/pki.go": `package pki

import "os"

func save(data []byte) error {
	return os.WriteFile("/var/lib/stratum/pki/ca.crt", data, 0o600)
}

func extra(data []byte) error {
	return os.WriteFile("/var/lib/stratum/pki/extra.crt", data, 0o600)
}
`})

	withAllowlist(t, map[string]allowEntry{
		"internal/pki/pki.go": {"deliberately not start-critical: writes a zero-byte marker", 1},
	})

	var out captured
	if code := run([]string{"."}, &out.stdout, &out.stderr); code != 1 {
		t.Fatalf("run exit = %d, want 1 (the entry covers one write, the file has two); output:\n%s", code, out.all())
	}
	for _, want := range []string{
		"internal/pki/pki.go:10",
		"allowlisted for only 1",
		"update the allowlist entry",
	} {
		if !strings.Contains(out.all(), want) {
			t.Errorf("output does not mention %q; got:\n%s", want, out.all())
		}
	}
}

func TestStaleAllowlistEntryFails(t *testing.T) {
	withGuarded(t, "internal/pki")
	writeTree(t, map[string]string{"internal/pki/pki.go": rawWriteFile})

	withAllowlist(t, map[string]allowEntry{
		"internal/pki/pki.go": {"deliberately not start-critical: writes a zero-byte marker", 2},
	})

	var out captured
	if code := run([]string{"."}, &out.stdout, &out.stderr); code != 1 {
		t.Fatalf("run exit = %d, want 1 (the entry covers two writes, the file has one); output:\n%s", code, out.all())
	}
	for _, want := range []string{
		"internal/pki/pki.go",
		"covers 2 raw write(s) but the file now has 1",
		"stale",
	} {
		if !strings.Contains(out.all(), want) {
			t.Errorf("output does not mention %q; got:\n%s", want, out.all())
		}
	}
}

func TestEveryAllowlistEntryStatesItsReason(t *testing.T) {
	for file, entry := range allowlist {
		if strings.TrimSpace(entry.reason) == "" {
			t.Errorf("allowlist entry for %s carries no stated reason", file)
		}
		if entry.want < 1 {
			t.Errorf("allowlist entry for %s covers no raw write (want=%d): an entry exists to cover the writes its reason names", file, entry.want)
		}
	}
}

func TestTheNewWriterPackagesAreGuardedByTheDerivedSet(t *testing.T) {
	writeTree(t, map[string]string{
		"internal/host/writer.go":    strings.Replace(rawWriteFile, "package pki", "package host", 1),
		"internal/cluster/writer.go": strings.Replace(rawWriteFile, "package pki", "package cluster", 1),
	})

	var out captured
	if code := run([]string{"."}, &out.stdout, &out.stderr); code != 1 {
		t.Fatalf("run exit = %d, want 1: a raw os.WriteFile in internal/host or internal/cluster must fail make lint — the owning-package record must name a node file in each so the guard's derived set scans them; output:\n%s", code, out.all())
	}
	for _, want := range []string{"internal/host/writer.go:6", "internal/cluster/writer.go:6"} {
		if !strings.Contains(out.all(), want) {
			t.Errorf("no finding for %s; got:\n%s", want, out.all())
		}
	}
}

func TestTheStampMarkerIsAllowlistedAndTheBootCriticalWritesAreNot(t *testing.T) {
	entry, ok := allowlist["internal/cluster/run.go"]
	if !ok {
		t.Fatal("no allowlist entry for internal/cluster/run.go: the import stamp marker is best-effort and deliberately not start-critical, and must be allowlisted with that stated reason covering exactly its one raw write")
	}
	if entry.want != 1 {
		t.Errorf("the internal/cluster/run.go entry covers %d raw write(s), want exactly 1: the stamp marker is the only raw write the conversion leaves behind", entry.want)
	}
	for _, want := range []string{"stamp", "not start-critical"} {
		if !strings.Contains(entry.reason, want) {
			t.Errorf("the internal/cluster/run.go entry's reason %q does not state that the stamp marker is deliberately not start-critical", entry.reason)
		}
	}
	if entry, ok := allowlist["internal/host/host.go"]; ok {
		t.Errorf("internal/host/host.go is allowlisted (%q): the sysctl and modules-load writes are boot-critical and must go through the shared atomic writer, not an exception", entry.reason)
	}
}

func TestWholeRepoIsCleanAtHead(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))

	var out captured
	if code := run([]string{"."}, &out.stdout, &out.stderr); code != 0 {
		t.Fatalf("guard found raw node-file writes in the repo (exit %d):\n%s", code, out.all())
	}
}

func TestLintTargetRunsTheGuard(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	recipe, ok := lintRecipe(string(data))
	if !ok {
		t.Fatal("Makefile has no lint target")
	}
	if !strings.Contains(recipe, "nodefilewrites") {
		t.Errorf("the lint target does not run the node-file write guard; lint recipe:\n%s", recipe)
	}
}

func lintRecipe(makefile string) (string, bool) {
	lines := strings.Split(makefile, "\n")
	start := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, "lint:") {
			start = i + 1
			break
		}
	}
	if start == -1 {
		return "", false
	}
	var recipe []string
	for _, ln := range lines[start:] {
		if !strings.HasPrefix(ln, "\t") {
			break
		}
		recipe = append(recipe, ln)
	}
	return strings.Join(recipe, "\n"), true
}

func TestArgvForms(t *testing.T) {
	testCases := []struct {
		name string
		args []string
	}{
		{"single file", []string{"internal/pki/writer.go"}},
		{"package directory", []string{"internal/pki"}},
		{"package pattern", []string{"./internal/..."}},
		{"whole tree", []string{"."}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			withGuarded(t, "internal/pki")
			// writer.go, not pki.go: the real pki.go is in the package-level allowlist.
			writeTree(t, map[string]string{
				"internal/pki/writer.go": rawWriteFile,
				"internal/pki/clean.go":  atomicWrite,
			})

			var out captured
			if code := run(tc.args, &out.stdout, &out.stderr); code != 1 {
				t.Fatalf("run(%v) exit = %d, want 1; output:\n%s", tc.args, code, out.all())
			}
			if !strings.Contains(out.all(), "internal/pki/writer.go:6") {
				t.Errorf("finding does not name the violating file; got:\n%s", out.all())
			}
		})
	}
}

func TestToolErrorsAreExitTwo(t *testing.T) {
	testCases := []struct {
		name  string
		files map[string]string
		args  []string
	}{
		{"missing path", nil, []string{"internal/pki/nope.go"}},
		{"not a Go file", map[string]string{"internal/pki/notes.txt": "hello"}, []string{"internal/pki/notes.txt"}},
		{"unparseable Go file", map[string]string{"internal/pki/broken.go": unparseable}, []string{"."}},
		{"no arguments", nil, nil},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			withGuarded(t, "internal/pki")
			writeTree(t, tc.files)

			var out captured
			if code := run(tc.args, &out.stdout, &out.stderr); code != 2 {
				t.Fatalf("run(%v) exit = %d, want 2; output:\n%s", tc.args, code, out.all())
			}
			if out.stderr.Len() == 0 {
				t.Errorf("tool error wrote nothing to stderr")
			}
		})
	}
}

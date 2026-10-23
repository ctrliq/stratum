package main

// These tests are the spec for the node-file write guard (make lint's check
// that no package sneaks a raw write past a node file's owning package); the
// tool itself does not exist yet. The contract under test:
//
//   run(args []string, stdout, stderr io.Writer) int
//
//       argv names files, directories, or directory/... package patterns,
//       resolved relative to the working directory — the repository root in
//       every real invocation. Exit 0 when every guarded package is clean,
//       1 when a raw node-file write was found, 2 when the check cannot run
//       (a bad argument, an unreadable file, a file that does not parse): a
//       file the guard cannot read must fail the check, never pass it
//       silently.
//
//   var guarded map[string]bool
//
//       The packages that write node files, derived from the owning-package
//       record in internal/version — the record that puts each node file's
//       owning package beside its path, so the guard and the rule cannot
//       drift apart. A scanned file belongs to a guarded package when its
//       directory is that package or ends in /<package>, compared with "/"
//       separators and on directory boundaries.
//
//   var allowlist map[string]allowEntry
//
//       Deliberate exceptions, keyed by the file's path relative to the
//       working directory (internal/pki/pki.go). Each entry states the
//       reason the writer in that file is not start-critical and the exact
//       number of raw writes the reason covers; the guard scans allowlisted
//       files like any other and fails when the file's raw writes no longer
//       match the entry, so the stated exception and the enforced one
//       cannot drift apart.
//
// A violation is a call to os.WriteFile or clientcmd.WriteToFile, in any
// import spelling, in a non-test .go file of a guarded package. Each finding
// carries the file:line of the call and states the owning-package rule: node
// files are owned by the package the record names and are written through
// the shared atomic writer, so the next writer finds the rule instead of
// rediscovering it.
//
// The fixtures below are parsed, not compiled: they need valid Go syntax,
// not resolvable imports.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures. Each raw write sits on a known line so the tests can pin the
// file:line the finding must carry.

const rawWriteFile = `package pki

import "os"

func save(data []byte) error {
	return os.WriteFile("/var/lib/stratum/pki/ca.crt", data, 0o600)
}
`

// The os.WriteFile call is on line 6 of rawWriteFile.
const rawWriteFileLine = 6

const rawKubeconfigWrite = `package kubeconfig

import (
	"k8s.io/client-go/tools/clientcmd"
)

func write(cfg clientcmdapi.Config, path string) error {
	return clientcmd.WriteToFile(cfg, path)
}
`

// The clientcmd.WriteToFile call is on line 8 of rawKubeconfigWrite.
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

// unparseable is a .go file the parser rejects: the guard cannot vouch for
// it, so it must be a tool error rather than a silent pass.
const unparseable = "package pki\n\nfunc {"

// writeTree writes files (relative path to contents) into a fresh directory
// and makes it the working directory, the way the guard is invoked from the
// repository root.
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

// withGuarded scopes the guarded packages to dirs for one test.
func withGuarded(t *testing.T, dirs ...string) {
	t.Helper()
	old := guarded
	guarded = make(map[string]bool, len(dirs))
	for _, d := range dirs {
		guarded[d] = true
	}
	t.Cleanup(func() { guarded = old })
}

// withAllowlist scopes the allowlist to entries for one test.
func withAllowlist(t *testing.T, entries map[string]allowEntry) {
	t.Helper()
	old := allowlist
	allowlist = entries
	t.Cleanup(func() { allowlist = old })
}

// captured collects what run writes, keeping the streams apart so tests can
// assert on findings and tool errors without pinning the exact split.
type captured struct {
	stdout strings.Builder
	stderr strings.Builder
}

func (c *captured) all() string { return c.stdout.String() + c.stderr.String() }

// TestPlainWriteFileInWriterPackageFailsLint pins the core rule: a plain
// os.WriteFile in a package that writes node files is a violation, reported
// with the call's file:line and the owning-package rule, and the run exits
// non-zero so make lint fails.
func TestPlainWriteFileInWriterPackageFailsLint(t *testing.T) {
	withGuarded(t, "internal/pki")
	// A synthetic fixture name, not internal/pki/pki.go: that real file
	// carries a deliberately-left allowlist entry for the CA-rotation
	// scratch, and the allowlist is per file.
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

// TestClientcmdWriteToFileFailsLint pins the kubeconfig half of the rule:
// clientcmd.WriteToFile is a plain os.WriteFile underneath (no temp file, no
// fsync), so it is rejected in a writer package exactly like os.WriteFile.
func TestClientcmdWriteToFileFailsLint(t *testing.T) {
	withGuarded(t, "internal/kubeconfig")
	// A synthetic fixture name, not internal/kubeconfig/kubeconfig.go: that
	// real file writes every kubeconfig — admin.conf's copy included —
	// through the shared atomic writer and carries no allowlist entry, so a
	// fixture of its name must stay clean.
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

// TestEveryNodeFileWriterPackageIsGuarded pins the guard's coverage: a plain
// os.WriteFile in each package the task names — pki, kubeconfig, profilefiles,
// certs, nodestate, staticpod, upgrade — fails the run. The guarded set
// comes from the owning-package record, so a red here means the record does
// not yet own a node file in that package.
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

// TestAtomicWriterAndReadsAreNotViolations pins the rule's replacement: the
// shared atomic writer (internal/atomicfile) is how a guarded package writes
// a node file, and reads never were writes. A clean guarded package passes
// with exit 0.
func TestAtomicWriterAndReadsAreNotViolations(t *testing.T) {
	withGuarded(t, "internal/pki")
	// No allowlist: the fixture sits at the real allowlisted path pki.go,
	// and this test is about the scan, not the exceptions.
	withAllowlist(t, nil)
	writeTree(t, map[string]string{"internal/pki/pki.go": atomicWrite})

	var out captured
	if code := run([]string{"."}, &out.stdout, &out.stderr); code != 0 {
		t.Fatalf("run exit = %d, want 0 (atomic writer passes the guard); output:\n%s", code, out.all())
	}
}

// TestWritesOutsideTheWriterPackagesPass pins the guard's scope: it runs on
// the whole repo, but only the writer packages are enforced — a package that
// owns no node file writes freely, and a name that merely CONTAINS a guarded
// package's path (internal/pkia against internal/pki) is not matched, so the
// directory rule matches on boundaries.
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

// TestTestFilesAndTestDataAreExempt pins what the rule governs: production
// writers. Test files write fixtures on purpose, and testdata is invisible
// to the go tool, so a raw write in either never fails the guard — otherwise
// the whole-repo run could never pass.
func TestTestFilesAndTestDataAreExempt(t *testing.T) {
	withGuarded(t, "internal/pki")
	// No allowlist: the fixture sits at the real allowlisted path pki.go,
	// and this test is about the exemptions, not the exceptions.
	withAllowlist(t, nil)
	writeTree(t, map[string]string{
		"internal/pki/pki.go":            atomicWrite,
		"internal/pki/pki_test.go":       rawWriteFile,
		"internal/pki/testdata/x.go":     rawWriteFile,
		"internal/pki/fixture/x.go":      rawWriteFile, // a plain directory is NOT exempt
		"internal/pki/vendor/v/x.go":     rawWriteFile, // vendored code is not ours to judge
		"internal/pki/.hidden/x.go":      rawWriteFile, // .git and friends
		"internal/pki/node_modules/x.go": rawWriteFile, // a dependency tree is not ours to guard either
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

// TestRenamedImportDoesNotDodgeTheGuard pins the call detection: the guard
// resolves the selector through the file's imports, so importing os or
// clientcmd under another name and writing through it is still a violation.
// A rule you dodge with an alias is not a rule.
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

// TestAllowlistedWriterPasses pins the exception mechanism: a file whose
// path carries an allowlist entry passes even though it writes raw, and a
// file whose path does not, does not. Keys are per file, so an exception is
// one deliberate writer, never a package.
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

// TestAllowlistedFileWithANewRawWriteFails pins the exception's verification:
// an allowlist entry covers an exact number of raw writes — the ones its
// reason names — so a NEW raw write in an allowlisted file fails the lint and
// the entry has to be re-justified in the same change. A whole-file exemption
// would let a new live-path writer hide behind an entry written for a
// different write, which is exactly the drift the rule exists to stop.
func TestAllowlistedFileWithANewRawWriteFails(t *testing.T) {
	withGuarded(t, "internal/pki")
	// rawWriteFile with a second raw write appended: the entry below covers
	// exactly one write (line 6), so the write on line 10 is uncovered.
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

// TestStaleAllowlistEntryFails pins the other direction of the same check:
// when the file's raw writes drop below the number the entry covers — a
// write moved onto the atomic writer — the entry no longer states an
// exception that exists, so the lint fails until the entry is shrunk in the
// same change.
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

// TestEveryAllowlistEntryStatesItsReason pins the exception discipline: an
// allowlist entry exists to record WHY this writer is deliberately not
// start-critical and WHICH writes the reason covers, so an entry without a
// stated reason, or one that covers no write at all, is a bug in the guard
// itself.
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

// TestTheNewWriterPackagesAreGuardedByTheDerivedSet pins the two writer
// packages this task adds — internal/host (the modules-load file and both
// sysctl files) and internal/cluster (the Cilium settings record and the
// component .args files) — against the guard's REAL derived set: no
// withGuarded override, so the finding appears only when the owning-package
// record in internal/version names a node file in each package. A package
// missing from the record writes outside the rule, and its raw writes pass
// make lint unseen — exactly how internal/host's and internal/cluster's raw
// writes passed before the record covered them.
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

// TestTheStampMarkerIsAllowlistedAndTheBootCriticalWritesAreNot pins the
// exception discipline for the two packages this task adds. run.go's import
// stamp marker (`_ = os.WriteFile` in writeStamp) is best-effort by design —
// it only makes the next kubelet restart's import check cheap, and its writer
// already discards the error — so it is the one raw write run.go may keep,
// covered by an entry stating exactly that reason and covering exactly its
// one write. host.go's sysctl and modules-load writes are the opposite:
// boot-critical, so host.go gets no entry at all and the writes must move
// onto the shared atomic writer — an allowlist there would turn the
// boot-critical rule into an option.
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

// TestWholeRepoIsCleanAtHead pins the run the guard exists for: pointed at
// the whole repository — the way make lint runs it — the guard passes at
// HEAD. A red here means a raw os.WriteFile or clientcmd.WriteToFile landed
// in a guarded package: move the write to the shared atomic writer, or
// allowlist the file with the stated reason it is not start-critical.
func TestWholeRepoIsCleanAtHead(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))

	var out captured
	if code := run([]string{"."}, &out.stdout, &out.stderr); code != 0 {
		t.Fatalf("guard found raw node-file writes in the repo (exit %d):\n%s", code, out.all())
	}
}

// TestLintTargetRunsTheGuard pins the wiring: make lint invokes the guard,
// so the next writer meets the rule at lint time, not at incident time.
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

// lintRecipe returns the recipe lines of the `lint:` target: everything from
// its declaration to the first line that is not recipe (not tab-indented).
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

// TestArgvForms pins the interface: the guard takes files, directories, and
// directory/... package patterns, and one violating file fails every form.
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
			// Synthetic fixture names, not internal/pki/pki.go: that real
			// file carries a deliberately-left allowlist entry for the
			// CA-rotation scratch, and the allowlist is per file.
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

// TestToolErrorsAreExitTwo pins the failure mode of the check itself: a bad
// argument, a non-Go file, or a file that does not parse is exit 2 with a
// message on stderr — never a silent pass, which would drop a file out of
// the check.
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

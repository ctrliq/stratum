package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// The child hands its argv to main() through unit-separator-separated
// environment variables (exec refuses a NUL in an environment value, and the
// unit separator cannot appear in a real argv element either, so the split is
// unambiguous), and points every internal/version path the invoked commands
// read at a scratch directory so the child sees a bare, non-node filesystem
// deterministically on any machine.
const (
	childArgsEnv  = "STRATUM_MAIN_TEST_ARGS"
	childStateEnv = "STRATUM_MAIN_TEST_STATE"
)

// childArgv decodes the argv the parent passed. An empty value means a bare
// invocation — no argv element at all, which is the row under test.
func childArgv(encoded string) []string {
	if encoded == "" {
		return nil
	}
	return strings.Split(encoded, "\x1f")
}

// runAsStratum is the child half of the re-exec pattern (the one
// internal/certs/interrupt_test.go uses to re-run a test binary as the real
// binary): it rewrites os.Args to a stratum invocation, redirects the version
// paths a command would read away from the real node layout, and calls main().
// main() ends every failing path in os.Exit, so the child process ends with
// exactly the exit code under test; the success rows end it too when the test
// returns, still with code 0.
func runAsStratum(args []string, state string) {
	os.Args = append([]string{"stratum"}, args...)
	version.K8sConfDir = filepath.Join(state, "config")
	version.PKIDir = filepath.Join(state, "pki")
	version.AdminConf = filepath.Join(state, "pki", "kubeconfig", "admin.conf")
	version.EncryptionConfigPath = filepath.Join(state, "pki", "encryption-config.yaml")
	version.BinDir = state
	main()
}

// fakeState builds the scratch node state a child runs against. With binScript
// set, it is written as the payload's kubectl (version.BinDir is redirected
// there), so the multicall passthrough can be driven to any exit code.
func fakeState(t *testing.T, binScript string) string {
	t.Helper()
	state := t.TempDir()
	if binScript != "" {
		path := filepath.Join(state, "kubectl")
		if err := os.WriteFile(path, []byte(binScript), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// TestMain_ExitCodes re-runs this test binary as the stratum binary and pins,
// for every error sentinel pkg/cli exports, the exit code main.go maps it to
// and whether main prints its own "stratum: ..." line on top of what the
// command already reported. Each sentinel an engine command returns, and the
// plain-error default of main's exit-code switch, is exercised by one row: a
// bare invocation (ErrNoCommand), a bad flag (ErrUsage), the kubectl
// passthrough (*ErrExitCode, both child exit codes), and an unmatched command
// (the default case). ErrCheckFailed, which only an importer's check command
// returns, is pinned in pkg/cli's TestExitCode. A change to any
// code, or to which cases print a "stratum:" line, fails here instead of
// silently diverging an embedding binary's copy of this contract.
func TestMain_ExitCodes(t *testing.T) {
	if encoded, ok := os.LookupEnv(childArgsEnv); ok {
		runAsStratum(childArgv(encoded), os.Getenv(childStateEnv))
		return // unreachable: main() exits the process on every pinned path
	}

	for _, tc := range []struct {
		name string
		args []string
		// state prepares the child's redirected node state; nil is a bare one.
		state func(t *testing.T) string
		// wantCode is the exit code the mapping must produce.
		wantCode int
		// wantLine prefixes the one "stratum: ..." line main must print on
		// stderr, or empty when main must print no such line at all (the
		// sentinel already reported itself).
		wantLine string
	}{
		{
			// Help has already printed; ErrNoCommand is exit 2, silent.
			name:     "a bare invocation exits 2 without a stratum line",
			args:     []string{},
			wantCode: 2,
		},
		{
			// The flag error and usage have already printed; ErrUsage is exit
			// 2, silent.
			name:     "a bad flag exits 2 without a stratum line",
			args:     []string{"up", "--not-a-real-flag"},
			wantCode: 2,
		},
		{
			// No sentinel reported it: the default case prints and exits 1.
			name:     "an unknown command exits 1 with a stratum line",
			args:     []string{"bogus"},
			wantCode: 1,
			wantLine: `stratum: unknown command "bogus"`,
		},
		{
			// The child's stdio was inherited; *ErrExitCode propagates the
			// child's exact code, silently.
			name:     "the kubectl passthrough propagates the child's exit code",
			args:     []string{"kubectl", "get", "pods"},
			state:    func(t *testing.T) string { return fakeState(t, "#!/bin/sh\nexit 3\n") },
			wantCode: 3,
		},
		{
			// A child that exits 0 must reach the success path: exit 0,
			// no error printed.
			name:     "the kubectl passthrough succeeds when the child does",
			args:     []string{"kubectl", "version"},
			state:    func(t *testing.T) string { return fakeState(t, "#!/bin/sh\nexit 0\n") },
			wantCode: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := fakeState(t, "")
			if tc.state != nil {
				state = tc.state(t)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestMain_ExitCodes$")
			cmd.Env = append(os.Environ(),
				childArgsEnv+"="+strings.Join(tc.args, "\x1f"),
				childStateEnv+"="+state,
			)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()

			code := 0
			if err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("re-running the test binary as stratum: %v", err)
				}
				code = ee.ExitCode()
			}
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %q)", code, tc.wantCode, stderr.String())
			}

			var gotLine string
			for _, line := range strings.Split(stderr.String(), "\n") {
				if strings.HasPrefix(line, "stratum:") {
					gotLine = line
					break
				}
			}
			if tc.wantLine == "" {
				if gotLine != "" {
					t.Errorf("stderr carries a %q line; the sentinel already reported itself, so main must print nothing", gotLine)
				}
				return
			}
			if !strings.HasPrefix(gotLine, tc.wantLine) {
				t.Errorf("stderr %q has no line starting %q — main's default case must report the error as stratum: <msg>", stderr.String(), tc.wantLine)
			}
		})
	}
}

// TestMain_DelegatesTheExitCodeMapping pins main's shape: it owns no
// exit-code mapping of its own. The mapping from pkg/cli's error sentinels
// to exit codes and "stratum: ..." lines lives in pkg/cli.ExitCode — the one
// package an embedding build imports — so a new sentinel is mapped once,
// where every embedding binary can adopt it. This test reads main.go itself
// so a second hand copy of the mapping cannot drift back into this file
// unnoticed: main must ask ExitCode exactly once, and it must not match
// sentinels (errors.Is/errors.As) or branch on them with a switch of its
// own.
func TestMain_DelegatesTheExitCodeMapping(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}
	var mainBody *ast.BlockStmt
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "main" && d.Recv == nil {
			mainBody = d.Body
		}
	}
	if mainBody == nil {
		t.Fatal("main.go declares no func main")
	}

	var exitCodeCalls, switchStmts, sentinelMatches int
	ast.Inspect(mainBody, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			if _, ok := n.(*ast.SwitchStmt); ok {
				switchStmts++
			}
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch {
		case sel.Sel.Name == "ExitCode":
			exitCodeCalls++
		case sel.Sel.Name == "Is" || sel.Sel.Name == "As":
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "errors" {
				sentinelMatches++
			}
		}
		return true
	})
	if exitCodeCalls != 1 {
		t.Errorf("main calls ExitCode %d times, want exactly once — the mapping lives in pkg/cli, and main must ask it rather than keep its own copy", exitCodeCalls)
	}
	if switchStmts != 0 {
		t.Errorf("main carries %d switch statement(s); the exit-code mapping lives in pkg/cli.ExitCode, and a switch of main's own must not come back", switchStmts)
	}
	if sentinelMatches != 0 {
		t.Errorf("main matches sentinels with errors.Is/errors.As %d time(s); recognizing a sentinel is ExitCode's job, not main's", sentinelMatches)
	}
}

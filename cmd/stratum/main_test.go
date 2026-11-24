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

// exec refuses a NUL in an env value; \x1f never appears in argv, so the split is unambiguous.
const (
	childArgsEnv  = "STRATUM_MAIN_TEST_ARGS"
	childStateEnv = "STRATUM_MAIN_TEST_STATE"
)

// An empty value encodes a bare invocation: no argv element at all.
func childArgv(encoded string) []string {
	if encoded == "" {
		return nil
	}
	return strings.Split(encoded, "\x1f")
}

func runAsStratum(args []string, state string) {
	os.Args = append([]string{"stratum"}, args...)
	version.K8sConfDir = filepath.Join(state, "config")
	version.PKIDir = filepath.Join(state, "pki")
	version.AdminConf = filepath.Join(state, "pki", "kubeconfig", "admin.conf")
	version.EncryptionConfigPath = filepath.Join(state, "pki", "encryption-config.yaml")
	version.BinDir = state
	main()
}

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

// Re-runs this binary as stratum, pinning exit rows for ErrNoCommand, ErrUsage and *ErrExitCode.
func TestMain_ExitCodes(t *testing.T) {
	if encoded, ok := os.LookupEnv(childArgsEnv); ok {
		runAsStratum(childArgv(encoded), os.Getenv(childStateEnv))
		return // the success row reaches this; main returns instead of exiting when ExitCode is 0
	}

	for _, tc := range []struct {
		name     string
		args     []string
		state    func(t *testing.T) string
		wantCode int
		wantLine string
	}{
		{
			name:     "a bare invocation exits 2 without a stratum line",
			args:     []string{},
			wantCode: 2,
		},
		{
			name:     "a bad flag exits 2 without a stratum line",
			args:     []string{"up", "--not-a-real-flag"},
			wantCode: 2,
		},
		{
			name:     "an unknown command exits 1 with a stratum line",
			args:     []string{"bogus"},
			wantCode: 1,
			wantLine: `stratum: unknown command "bogus"`,
		},
		{
			name:     "the kubectl passthrough propagates the child's exit code",
			args:     []string{"kubectl", "get", "pods"},
			state:    func(t *testing.T) string { return fakeState(t, "#!/bin/sh\nexit 3\n") },
			wantCode: 3,
		},
		{
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

// Pins that main asks cli.ExitCode once and keeps no exit-code mapping of its own.
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

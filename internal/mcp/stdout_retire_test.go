package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/etcdsnap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Pins on the stdout rules. On the stdio transport os.Stdout is the
// JSON-RPC stream, so no path in this package may redirect it, and every
// operation a tool calls reports its progress through the io.Writer it is
// handed instead of printing to stdout. The handlers and the default wiring
// pass run()'s buffer straight through, because every operation takes its
// io.Writer.

// TestNoMCPPathRedirectsStdout pins the rule that nothing
// under internal/mcp redirects os.Stdout or calls the guard and its
// capture bridges. Once the operations take a writer there is nothing left
// to wrap, so no handler may add a redirect or a capture.
//
// This is a pin on the source, not on behaviour, because for one of the
// wrapped paths — registry_apply — there is nothing a behavioural test can
// observe: registry.Apply prints nothing, so a redirect around it changes no
// output, no stream and no result.
func TestNoMCPPathRedirectsStdout(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	// The banned surface: the swap itself and the guard's three bridges.
	// Comment lines are skipped, so prose that still names the guard
	// does not trip the pin; only code does.
	banned := []string{"os.Stdout =", "guardStdout(", "discardStdout(", "captureRun(", "captureWarns("}

	var hits []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") {
				continue
			}
			for _, b := range banned {
				if strings.Contains(code, b) {
					hits = append(hits, fmt.Sprintf("%s:%d: %s", name, i+1, code))
					break
				}
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("internal/mcp still redirects os.Stdout or runs the stdout guard at %d sites; "+
			"stdout is the JSON-RPC stream on the stdio transport, and the operations take a writer instead:\n%s",
			len(hits), strings.Join(hits, "\n"))
	}
}

// The three pins below share one reading of captureRealStdout, spelled out
// once here. The helper installs a pipe as os.Stdout for the length of the
// call, so the operation's own os.Stdout observation must equal the file the
// capture installed — comparing against os.Stdout read after the call can
// never hold, because the capture has restored the real stream by then. What
// the identity distinguishes is who owned a swap: with no redirect the
// operation observes the capture's pipe, while a redirect underneath it (the
// guardStdout shape) leaves the operation observing the redirector's
// own pipe, and the canary fmt.Println is captured and forwarded to stderr
// instead of reaching the captured stream.

// TestDestructiveOperation_RunsOnTheWriterRunHandsIt pins the CA
// rotation's convention: the handler's operation receives the buffer writer
// run() hands it (io.Writer FIRST, the convention every destructiveDeps
// field follows), writes its progress through it, and runs with nothing
// redirecting os.Stdout under it — a canary fmt.Println from inside the
// operation reaches the stdout stream, because no guard captures and
// forwards it.
//
// Fails if anything reintroduces a redirect around the operations.
func TestDestructiveOperation_RunsOnTheWriterRunHandsIt(t *testing.T) {
	fakeStratumHost(t)

	var (
		gotWriter   io.Writer
		opStdout    *os.File
		progressOut string
	)
	withDestructiveDeps(t, func(deps *destructiveDeps) {
		deps.rotateCA = func(w io.Writer, o certs.CARotateOptions) error {
			gotWriter, opStdout = w, os.Stdout
			progressOut = "leaf rotation progress"
			if w == nil {
				return nil
			}
			fmt.Fprint(w, progressOut)
			// A canary that bypasses the writer: a stdout redirect around
			// the operation would capture it and forward it to stderr, and
			// the captured stream would never see it.
			fmt.Println("CANARY: the CA rotation prints on the process's real stdout")
			return nil
		}
	})

	var out destructiveOut
	var err error
	onStdout, during := captureRealStdout(t, func() {
		_ = captureStderrDiscard(func() {
			_, out, err = CertsRotateCAHandler()(context.Background(), nil, CertsRotateCAIn{Phase: "sign-new"})
		})
	})
	if err != nil {
		t.Fatalf("certs_rotate_ca: %v", err)
	}
	if gotWriter == nil {
		t.Fatal("the handler's operation received no writer: the handler must hand run()'s buffer straight through")
	}
	if out.Progress != progressOut {
		t.Errorf("out.Progress = %q, want what the operation wrote through the writer it was handed (%q)", out.Progress, progressOut)
	}
	if opStdout != during {
		t.Error("the operation observed an os.Stdout other than the one the capture installed: something swapped os.Stdout around the operation, and a redirect there puts the prints on the JSON-RPC stream")
	}
	if !strings.Contains(onStdout, "CANARY") {
		t.Errorf("a canary write to os.Stdout from inside the operation never reached the captured stdout: %q", onStdout)
	}
}

// TestSnapshotSave_RunsOnTheWriterItIsHanded pins the write-tier save's half
// of the convention: snapshot_save passes its save effect a writer and files
// what the effect wrote through it into the result's Progress, instead of
// wrapping the effect in a stdout redirect and searching the captured text.
//
// Fails if anything reintroduces a redirect around the save; the
// identity and canary assertions read exactly as the CA rotation pin's do.
func TestSnapshotSave_RunsOnTheWriterItIsHanded(t *testing.T) {
	fakeStratumHost(t)

	resolved := t.TempDir()
	oldCfg, oldSave := loadSnapshotConfig, snapshotSave
	t.Cleanup(func() {
		loadSnapshotConfig, snapshotSave = oldCfg, oldSave
	})
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return etcdsnap.Config{Retain: 5, Dir: resolved}, nil }

	var (
		gotWriter   io.Writer
		saveStdout  *os.File
		progressOut string
	)
	snapshotSave = func(ctx context.Context, w io.Writer, o etcdsnap.SaveOptions) (string, error) {
		gotWriter, saveStdout = w, os.Stdout
		progressOut = "snapshot progress line"
		if w == nil {
			return "", nil
		}
		fmt.Fprint(w, progressOut)
		// The canary bypasses the writer on purpose: a stdout redirect
		// around the save would capture it and forward it to stderr, and
		// the captured stream would never see it.
		fmt.Println("CANARY: the snapshot save prints on the process's real stdout")
		return filepath.Join(o.Dir, "manual.db"), nil
	}

	var out SnapshotSaveOut
	var err error
	onStdout, during := captureRealStdout(t, func() {
		_ = captureStderrDiscard(func() {
			_, out, err = SnapshotSaveHandler()(context.Background(), nil, SnapshotSaveIn{Name: "manual.db", SkipS3: true})
		})
	})
	if err != nil {
		t.Fatalf("snapshot_save: %v", err)
	}
	if gotWriter == nil {
		t.Fatal("snapshot_save passed its save effect no writer: the handler must hand its per-call buffer straight through")
	}
	if out.Progress != progressOut {
		t.Errorf("out.Progress = %q, want what the save wrote through the writer it was handed (%q)", out.Progress, progressOut)
	}
	if out.Path != filepath.Join(resolved, "manual.db") {
		t.Errorf("out.Path = %q, want the save's returned path", out.Path)
	}
	if saveStdout != during {
		t.Error("the save observed an os.Stdout other than the one the capture installed: something swapped os.Stdout around the save, and a redirect there puts the prints on the JSON-RPC stream")
	}
	if !strings.Contains(onStdout, "CANARY") {
		t.Errorf("a canary write to os.Stdout from inside the save never reached the captured stdout: %q", onStdout)
	}
}

// TestTokenCreate_RunsOnTheRealStdout pins token_create's half of the
// convention: the handler hands the mint a writer (io.Writer FIRST, like
// every destructiveDeps field) instead of wrapping the whole token.Issue
// call in a stdout redirect, and a canary fmt.Println from inside the mint
// reaches the captured stdout stream, because nothing redirects os.Stdout
// around the mint any more.
//
// Fails if anything reintroduces a redirect around the mint; the
// identity and canary assertions read exactly as the CA rotation pin's do.
func TestTokenCreate_RunsOnTheRealStdout(t *testing.T) {
	fakeStratumHost(t)

	var (
		gotWriter  io.Writer
		mintStdout *os.File
	)
	deps := tokenDeps{
		create: func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			gotWriter, mintStdout = w, os.Stdout
			if w == nil {
				return "abcdef.0123456789abcdef", "sha256:" + strings.Repeat("a", 64), nil, nil
			}
			fmt.Fprint(w, "mint progress")
			// The canary bypasses the writer on purpose: a stdout redirect
			// around the mint would capture it and forward it to stderr,
			// and the captured stream would never see it.
			fmt.Println("CANARY: the mint prints on the process's real stdout")
			return "abcdef.0123456789abcdef", "sha256:" + strings.Repeat("a", 64), nil, nil
		},
		newCertKey:  func() (string, error) { return "", nil },
		uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error { return nil },
		nodeIP:      func() (string, error) { return "10.0.0.1", nil },
	}
	handler := tokenCreateWith(deps)

	var err error
	onStdout, during := captureRealStdout(t, func() {
		_ = captureStderrDiscard(func() {
			_, _, err = handler(context.Background(), nil, TokenCreateIn{})
		})
	})
	if err != nil {
		t.Fatalf("token_create: %v", err)
	}
	if gotWriter == nil {
		t.Fatal("token_create handed the mint no writer: the handler must hand its per-call buffer straight through")
	}
	if mintStdout != during {
		t.Error("the mint observed an os.Stdout other than the one the capture installed: something swapped os.Stdout around the mint, and a redirect there puts the prints on the JSON-RPC stream")
	}
	if !strings.Contains(onStdout, "CANARY") {
		t.Errorf("a canary write to os.Stdout from inside the mint never reached the captured stdout: %q", onStdout)
	}
}

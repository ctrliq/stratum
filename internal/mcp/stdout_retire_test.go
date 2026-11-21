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

// On the stdio transport os.Stdout is the JSON-RPC stream: nothing here may redirect it.

// A source pin: registry_apply has no behavior a redirect could change for a test to observe.
func TestNoMCPPathRedirectsStdout(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read this package's directory: %v", err)
	}
	// Comment lines are skipped, so only code trips the pin.
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

// The operation must observe the pipe captureRealStdout installed, not os.Stdout read after it.

// TestDestructiveOperation_RunsOnTheWriterRunHandsIt pins the io.Writer-first convention.
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
			// A canary: a redirect around the operation would forward it to stderr instead.
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

// TestSnapshotSave_RunsOnTheWriterItIsHanded pins the save's io.Writer-first convention.
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
		// A canary: a redirect around the save would forward it to stderr instead.
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

// TestTokenCreate_RunsOnTheRealStdout pins the mint's io.Writer-first convention.
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
			// A canary: a redirect around the mint would forward it to stderr instead.
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

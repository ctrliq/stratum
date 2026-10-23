package upgrade

import (
	"archive/zip"
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// stageToTempDir redirects the staging layout for one test through the
// documented SetRoots + t.Cleanup helper: version.SetRoots recomputes
// version.StageDir from the redirected data root, and Resolve resolves the
// stage directory at call time, so staging follows. It returns the redirected
// stage directory. This package declares no StageDir var of its own to
// redirect.
func stageToTempDir(t *testing.T) string {
	t.Helper()
	setRoots(t, t.TempDir(), t.TempDir(), t.TempDir())
	return version.StageDir
}

// fakeUpgradeBinary builds a stand-in upgrade binary: leading bytes for the
// ELF, then the ZIP-append payload (same shape as internal/assets' tests).
func fakeUpgradeBinary(t *testing.T, members map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("#!/bin/sh\nexit 0\n")
	zw := zip.NewWriter(&buf)
	for name, body := range members {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "stratum-new")
	if err := os.WriteFile(path, buf.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveFromUpgradeBinary(t *testing.T) {
	staged := stageToTempDir(t)
	bin := fakeUpgradeBinary(t, map[string]string{
		"bin/containerd":            "containerd-binary",
		"bin/kubelet":               "kubelet-binary",
		"images/stratum-images.tar": "oci-archive",
	})

	src, err := Resolve(bin, []string{"containerd", "kubelet"})
	if err != nil {
		t.Fatalf("Resolve(%s): %v", bin, err)
	}
	if want := filepath.Join(staged, "bin"); src.Dir != want {
		t.Errorf("Dir = %q, want %q", src.Dir, want)
	}
	if want := filepath.Join(staged, "images"); src.Images != want {
		t.Errorf("Images = %q, want %q", src.Images, want)
	}
	got, err := os.ReadFile(filepath.Join(src.Dir, "kubelet"))
	if err != nil || string(got) != "kubelet-binary" {
		t.Errorf("staged kubelet = %q, %v", got, err)
	}
}

func TestResolveFromMissingFile(t *testing.T) {
	stageToTempDir(t)
	if _, err := Resolve(filepath.Join(t.TempDir(), "no-such-binary"), nil); err == nil {
		t.Fatal("expected an error for a missing --from file")
	}
}

func TestResolveFromDirectory(t *testing.T) {
	staged := stageToTempDir(t)
	_, err := Resolve(t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected an error for a directory --from")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error %q should say --from was a directory", err)
	}
	if entries, _ := os.ReadDir(staged); len(entries) != 0 {
		t.Errorf("refusal must happen before any change; staged %v", entries)
	}
}

func TestResolveFromPayloadlessBinary(t *testing.T) {
	stageToTempDir(t)
	bin := filepath.Join(t.TempDir(), "not-stratum")
	if err := os.WriteFile(bin, []byte("no zip appended here"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(bin, nil)
	if err == nil {
		t.Fatal("expected an error for a payload-less --from binary")
	}
	if !strings.Contains(err.Error(), "payload") {
		t.Errorf("error %q should say the binary carries no payload", err)
	}
}

func TestResolveFromPayloadMissingRequired(t *testing.T) {
	stageToTempDir(t)
	bin := fakeUpgradeBinary(t, map[string]string{"bin/containerd": "x"})
	_, err := Resolve(bin, []string{"containerd", "kubelet"})
	if err == nil {
		t.Fatal("expected an error for a payload missing a required binary")
	}
	if !strings.Contains(err.Error(), "kubelet") {
		t.Errorf("error %q should name the missing binary", err)
	}
}

// TestResolveStagingFollowsTheDataRoot: Resolve resolves the stage directory
// through version.StageDir at call time, so a SetRoots redirect of the data
// root moves where the payload is extracted — binaries under its bin/, image
// archives under its images/. Pinned across two redirects: a second call after
// a second redirect must answer the NEW version.StageDir, which is what
// separates a call-time derivation from one frozen at package init or
// memoized at the first call — either would go to the compiled-in
// /var/lib/stratum/upgrade/staged and never follow the redirect.
func TestResolveStagingFollowsTheDataRoot(t *testing.T) {
	bin := fakeUpgradeBinary(t, map[string]string{
		"bin/kubelet":               "kubelet-binary",
		"images/stratum-images.tar": "oci-archive",
	})
	for i := 0; i < 2; i++ {
		setRoots(t, t.TempDir(), t.TempDir(), t.TempDir())
		wantDir := filepath.Join(version.StageDir, "bin")
		wantImages := filepath.Join(version.StageDir, "images")
		src, err := Resolve(bin, []string{"kubelet"})
		if err != nil {
			t.Fatalf("Resolve after redirect %d: %v", i+1, err)
		}
		if src.Dir != wantDir {
			t.Errorf("redirect %d: Dir = %q, want %q — staging did not follow the redirected data root", i+1, src.Dir, wantDir)
		}
		if src.Images != wantImages {
			t.Errorf("redirect %d: Images = %q, want %q", i+1, src.Images, wantImages)
		}
		got, err := os.ReadFile(filepath.Join(wantDir, "kubelet"))
		if err != nil || string(got) != "kubelet-binary" {
			t.Errorf("redirect %d: staged kubelet at %s = %q, %v", i+1, wantDir, got, err)
		}
	}
}

// TestStagingDirHasOneHome parses payload.go and fails when this package
// declares a StageDir var of its own or compiles the staging path in as a
// literal. The one home for the staging directory is version.StageDir,
// derived from the data root; a second declaration here drifts from every
// redirected layout, and the same pattern pins node.go's hostProcs
// (TestHostProcsIsInitializedFromTheDerivation).
func TestStagingDirHasOneHome(t *testing.T) {
	t.Parallel() // host-free: parses only its own file
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "payload.go", nil, 0)
	if err != nil {
		t.Fatalf("parse payload.go: %v", err)
	}
	if init := varInit(f, "StageDir"); init != nil {
		t.Errorf("payload.go declares StageDir = %s: the staging directory's one home is version.StageDir — retire this var", types.ExprString(init))
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "/var/lib/stratum/upgrade/staged") {
			t.Errorf("payload.go compiles in the staging path %s at %s: staging must resolve version.StageDir at call time", lit.Value, fset.Position(lit.Pos()))
		}
		return true
	})
}

package apparmor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func redirectBinDir(t *testing.T, dir string) {
	t.Helper()
	old := version.BinDir
	t.Cleanup(func() { version.BinDir = old })
	version.BinDir = dir
}

func TestProbeRuncLabelStopsReadsAndReapsTheChild(t *testing.T) {
	src, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Skipf("no /bin/true to stand in for runc: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "runc"), src, 0o755); err != nil {
		t.Fatal(err)
	}
	redirectBinDir(t, dir)
	label, err := probeRuncLabel()
	if errors.Is(err, fs.ErrPermission) {
		t.Skipf("this kernel does not allow tracing: %v", err)
	}
	if err != nil && (strings.Contains(err.Error(), "did not stop") || strings.Contains(err.Error(), "wait for it")) {
		t.Fatalf("probeRuncLabel() = %v, want the child stopped at exec", err)
	}
	t.Logf("label %q, err %v", label, err)
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Errorf("after probeRuncLabel(), wait4 = %v, want ECHILD: the stopped child must be killed and reaped", err)
	}
}

func TestProbeRuncLabelWithoutRunc(t *testing.T) {
	redirectBinDir(t, t.TempDir())
	if _, err := probeRuncLabel(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("probeRuncLabel() without runc = %v, want fs.ErrNotExist", err)
	}
}

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/procid"
)

func TestLegacySupervisorHandoff(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *debug.BuildInfo
	}{
		{"renamed OSS", &debug.BuildInfo{Main: debug.Module{Path: "github.com/ctrliq/stratum"}}},
		{"embedding CLI", &debug.BuildInfo{Main: debug.Module{Path: "example/embedder"}, Deps: []*debug.Module{{Path: "github.com/ctrliq/stratum"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "123")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte("/renamed/binary\x00up\x00--worker\x00"), 0600); err != nil {
				t.Fatal(err)
			}
			read := func(string) (*debug.BuildInfo, error) { return tc.info, nil }
			// Parent cleanup can still unlink a replacement; refuse until the parent exits.
			if pid, err := findLegacySupervisor(root, 999, read); err != nil || pid != 123 {
				t.Fatalf("pid=%d err=%v", pid, err)
			}
			if pid, err := findLegacySupervisor(root, 123, read); err != nil || pid != 0 {
				t.Fatalf("self blocked: pid=%d err=%v", pid, err)
			}
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			if pid, err := findLegacySupervisor(root, 999, read); err != nil || pid != 0 {
				t.Fatalf("exited parent blocked: pid=%d err=%v", pid, err)
			}
		})
	}
}

func TestLegacySupervisorInspectionFailures(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "123")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte("binary\x00up\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		info      *debug.BuildInfo
		err       error
		wantError bool
	}{
		{"unrelated Go executable", &debug.BuildInfo{Main: debug.Module{Path: "example/unrelated"}}, nil, false},
		{"exited during scan", nil, os.ErrNotExist, false},
		{"exiting during scan", nil, &os.PathError{Op: "open", Path: "/proc/123/exe", Err: syscall.ESRCH}, false},
		{"unreadable executable", nil, os.ErrPermission, true},
		{"unknown executable", nil, fmt.Errorf("unknown image"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pid, err := findLegacySupervisor(root, 999, func(string) (*debug.BuildInfo, error) { return tc.info, tc.err })
			if pid != 0 || (err != nil) != tc.wantError {
				t.Fatalf("pid=%d err=%v", pid, err)
			}
		})
	}
}

// A process gone between the listing and the cmdline read is skipped, not unverifiable.
func TestLegacySupervisorSkipsAProcessExitingMidScan(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "2396"), 0o700); err != nil {
		t.Fatal(err)
	} // exited: the directory remains, its cmdline does not
	dir := filepath.Join(root, "123")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte("binary\x00up\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspected := 0
	pid, err := findLegacySupervisor(root, 1, func(string) (*debug.BuildInfo, error) {
		inspected++
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/ctrliq/stratum"}}, nil
	})
	if err != nil || pid != 123 || inspected != 1 {
		t.Fatalf("pid=%d err=%v inspected=%d; want only the live pid 123 inspected", pid, err, inspected)
	}
}

func TestLegacySupervisorMatchesOnlyUpSubcommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantPID int
	}{
		{"node start", []string{"stratum", "up", "--worker"}, 123},
		{"leading boolean flag", []string{"stratum", "--worker=true", "up"}, 123},
		{"leading config flag", []string{"stratum", "--config=/etc/cluster.yaml", "up"}, 123},
		{"leading separate value", []string{"stratum", "--config", "/etc/cluster.yaml", "up"}, 123},
		{"leading value named up", []string{"stratum", "--config", "up", "kubectl", "get", "pods"}, 0},
		{"watched pod named up", []string{"stratum", "kubectl", "get", "pod", "up", "--watch"}, 0},
		{"flag value up", []string{"stratum", "operator", "--node-name", "up"}, 0},
		{"no subcommand", []string{"stratum"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "123")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(tc.args, "\x00")+"\x00"), 0600); err != nil {
				t.Fatal(err)
			}
			inspected := false
			pid, err := findLegacySupervisor(root, 999, func(string) (*debug.BuildInfo, error) {
				inspected = true
				return &debug.BuildInfo{Main: debug.Module{Path: "github.com/ctrliq/stratum"}}, nil
			})
			if err != nil || pid != tc.wantPID {
				t.Fatalf("pid=%d err=%v, want pid=%d", pid, err, tc.wantPID)
			}
			if tc.wantPID == 0 && inspected {
				t.Fatal("inspected executable of unrelated command")
			}
		})
	}
}

// The legacy handoff check and uninstall must decide the up subcommand identically.
func TestLegacySupervisorAgreesWithUninstallOnUpDetection(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	read := func(string) (*debug.BuildInfo, error) {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/ctrliq/stratum"}}, nil
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"flags after up", []string{"stratum", "up", "--worker"}},
		{"flag with = before up", []string{"stratum", "--config=/etc/cluster.yaml", "up"}},
		{"flag with a separate value before up", []string{"stratum", "--config", "/etc/cluster.yaml", "up"}},
		{"single-letter flag with a value before up", []string{"stratum", "-c", "/etc/cluster.yaml", "up"}},
		{"boolean flag with = before up", []string{"stratum", "--worker=true", "up"}},
		{"terminator after up", []string{"stratum", "up", "--", "node1"}},
		{"terminator hides up", []string{"stratum", "--", "up"}},
		{"boolean flag reads the subcommand as its value", []string{"stratum", "--worker", "up"}},
		{"another subcommand", []string{"stratum", "uninstall", "--yes"}},
		{"no subcommand", []string{"stratum"}},
		{"empty argv", nil},
		{"watched pod named up", []string{"stratum", "kubectl", "get", "pod", "up", "--watch"}},
		{"flag value named up", []string{"stratum", "--config", "up", "kubectl", "get", "pods"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "123")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(tc.args, "\x00")+"\x00"), 0o600); err != nil {
				t.Fatal(err)
			}
			legacy, err := findLegacySupervisor(root, 999, read)
			if err != nil {
				t.Fatal(err)
			}
			want := procid.IsStratumTopLevel(procid.Process{Argv: tc.args, Exe: self})
			if got := legacy != 0; got != want {
				t.Errorf("findLegacySupervisor found pid %d for %q, but uninstall's IsStratumTopLevel = %v — the stratum-up rules must not drift on up detection", legacy, tc.args, want)
			}
		})
	}
}

func TestLegacySupervisorSkipsOwnChildren(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "124")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte("/usr/local/bin/stratum\x00up\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("124 (str) um (x)) R 123 124 1 0 -1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	read := func(string) (*debug.BuildInfo, error) {
		return &debug.BuildInfo{Main: debug.Module{Path: "github.com/ctrliq/stratum"}}, nil
	}
	if pid, err := findLegacySupervisor(root, 123, read); err != nil || pid != 0 {
		t.Fatalf("own child mid-exec blocked: pid=%d err=%v", pid, err)
	}
	if pid, err := findLegacySupervisor(root, 999, read); err != nil || pid != 124 {
		t.Fatalf("another process's up was not refused: pid=%d err=%v", pid, err)
	}
}

package host

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/toolenv"
	"github.com/ctrliq/stratum/internal/version"
)

// The stub script below reads FAKE_SYSTEMCTL_* env vars, all unset by default.
func fakeSystemd(t *testing.T) (callLog string) {
	t.Helper()
	dir := t.TempDir()

	marker := filepath.Join(dir, "run-systemd-system")
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	old := systemdMarkerPath
	systemdMarkerPath = marker
	t.Cleanup(func() { systemdMarkerPath = old })

	unit := filepath.Join(dir, "stratum.service")
	oldUnit := version.SystemdUnitPath
	version.SystemdUnitPath = unit
	t.Cleanup(func() { version.SystemdUnitPath = oldUnit })

	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	callLog = filepath.Join(dir, "systemctl-calls.log")
	stub := `#!/bin/sh
verb=$1
echo "$verb" >> ` + callLog + `
for v in $FAKE_SYSTEMCTL_FAIL; do
  [ "$verb" = "$v" ] && { echo "fake systemctl: $verb failed" >&2; exit 1; }
done
if [ "$verb" = "show" ]; then
  case "$3" in
    LoadState,ActiveState)
      load="$FAKE_SYSTEMCTL_LOAD_STATE"
      active="$FAKE_SYSTEMCTL_ACTIVE_STATE"
      if [ -z "$load" ] && [ -z "$active" ]; then
        if [ -f "` + unit + `" ]; then load="loaded"; active="active"; else load="not-found"; active="inactive"; fi
      fi
      echo "$load"
      echo "$active"
      ;;
    UnitFileState)
      state="$FAKE_SYSTEMCTL_UNIT_FILE_STATE"
      if [ -z "$state" ]; then
        if [ -f "` + unit + `" ]; then
          if [ "$FAKE_SYSTEMCTL_ENABLED" = "0" ]; then state="disabled"; else state="enabled"; fi
        else
          state="not-found"
        fi
      fi
      echo "$state"
      ;;
  esac
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	oldEnv, oldPath := toolEnv, toolPath
	toolEnv = nil // the stub reads FAKE_SYSTEMCTL_* from the caller's
	toolPath = bin
	t.Cleanup(func() { toolEnv, toolPath = oldEnv, oldPath })
	t.Setenv("FAKE_SYSTEMCTL_ENABLED", "")
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "")
	t.Setenv("FAKE_SYSTEMCTL_LOAD_STATE", "")
	t.Setenv("FAKE_SYSTEMCTL_ACTIVE_STATE", "")
	t.Setenv("FAKE_SYSTEMCTL_UNIT_FILE_STATE", "")

	dropIn := filepath.Join(dir, "stratum.service.d", "uninstall-no-restart.conf")
	oldDropIn := systemdDropInPath
	systemdDropInPath = dropIn
	t.Cleanup(func() { systemdDropInPath = oldDropIn })

	// UnitArgv's drop-in search, kept off the host's /run and vendor trees.
	oldDirs := unitDropInDirs
	unitDropInDirs = func() []string {
		return []string{unit + ".d", filepath.Join(dir, "run", "stratum.service.d"), filepath.Join(dir, "lib", "service.d")}
	}
	t.Cleanup(func() { unitDropInDirs = oldDirs })

	return callLog
}

func callCount(t *testing.T, callLog, verb string) int {
	t.Helper()
	b, err := os.ReadFile(callLog)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == verb {
			n++
		}
	}
	return n
}

func TestInstallUnitNoopWithoutSystemd(t *testing.T) {
	dir := t.TempDir()
	old := systemdMarkerPath
	systemdMarkerPath = filepath.Join(dir, "absent")
	defer func() { systemdMarkerPath = old }()

	unit := filepath.Join(dir, "stratum.service")
	oldUnit := version.SystemdUnitPath
	version.SystemdUnitPath = unit
	defer func() { version.SystemdUnitPath = oldUnit }()

	if err := InstallUnit([]string{"/opt/stratum/stratum", "up", "--worker"}); err != nil {
		t.Fatalf("InstallUnit on a non-systemd host: %v", err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatalf("InstallUnit wrote %s on a host with no systemd", unit)
	}
}

func TestInstallUnitRendersRestartExecStart(t *testing.T) {
	fakeSystemd(t)

	argv := []string{"/opt/stratum/stratum", "up", "--config", "/etc/stratum/config.yaml", "--cis"}
	if err := InstallUnit(argv); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}

	got, err := os.ReadFile(version.SystemdUnitPath)
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	want := "ExecStart=" + strings.Join(argv, " ")
	if !strings.Contains(string(got), want) {
		t.Errorf("unit file missing %q:\n%s", want, got)
	}
	if strings.Contains(string(got), "--join") || strings.Contains(string(got), "--token") {
		t.Errorf("unit file must never persist one-shot join credentials:\n%s", got)
	}
	if !strings.Contains(string(got), "Restart=always") {
		t.Errorf("unit does not restart the node process on crash/reboot:\n%s", got)
	}
	if !strings.Contains(string(got), "KillMode=process") {
		t.Errorf("unit must not use KillMode=mixed/control-group: that would kill containerd's shims"+
			" (and the containers they hold) on every restart, not just a crash:\n%s", got)
	}
}

func TestInstallUnitRendersAnOptionalEnvironmentFile(t *testing.T) {
	fakeSystemd(t)

	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	got, err := os.ReadFile(version.SystemdUnitPath)
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	want := "EnvironmentFile=-" + filepath.Join(version.ConfDir, "stratum.env")
	if !strings.Contains(string(got), want) {
		t.Errorf("unit file missing %q:\n%s", want, got)
	}
}

func TestEscapeSystemdArg(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/etc/stratum/config.yaml", "/etc/stratum/config.yaml"},
		{"/etc/100%-stratum/config.yaml", "/etc/100%%-stratum/config.yaml"},
		{"/etc/$HOME/config.yaml", "/etc/$$HOME/config.yaml"},
		{`C:\stratum\config.yaml`, `C:\\stratum\\config.yaml`},
		{"/etc/stratum config/config.yaml", `"/etc/stratum config/config.yaml"`},
		{`/etc/stratum "quoted"/config.yaml`, `"/etc/stratum \"quoted\"/config.yaml"`},
		{"/etc/o'brien/config.yaml", `"/etc/o'brien/config.yaml"`},
		{"", `""`},
	} {
		got, err := escapeSystemdArg(tc.in)
		if err != nil {
			t.Errorf("escapeSystemdArg(%q) returned an error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("escapeSystemdArg(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEscapeSystemdArgRefusesCRAndLF(t *testing.T) {
	for _, in := range []string{
		"/etc/stratum/config\n.yaml",
		"/etc/stratum/config\r.yaml",
		"/etc/stratum/config\r\n.yaml",
	} {
		if _, err := escapeSystemdArg(in); err == nil {
			t.Errorf("escapeSystemdArg(%q) did not refuse a CR/LF", in)
		}
	}
}

func TestInstallUnitIdempotent(t *testing.T) {
	callLog := fakeSystemd(t)
	argv := []string{"/opt/stratum/stratum", "up", "--worker"}
	if err := InstallUnit(argv); err != nil {
		t.Fatalf("first InstallUnit: %v", err)
	}
	before := callCount(t, callLog, "daemon-reload")

	t.Setenv("FAKE_SYSTEMCTL_FAIL", "enable")
	if err := InstallUnit(argv); err != nil {
		t.Fatalf("second InstallUnit (unchanged argv, already enabled) re-ran enable: %v", err)
	}
	after := callCount(t, callLog, "daemon-reload")
	if after <= before {
		t.Errorf("second InstallUnit did not reload systemd (removeStaleRestartOverride): %d calls before, %d after", before, after)
	}
}

func TestInstallUnitRetriesEnableAfterAFailure(t *testing.T) {
	fakeSystemd(t)
	argv := []string{"/opt/stratum/stratum", "up"}

	t.Setenv("FAKE_SYSTEMCTL_FAIL", "enable")
	if err := InstallUnit(argv); err == nil {
		t.Fatal("InstallUnit swallowed a systemctl enable failure")
	}

	// The file was written before the failing enable, so a content match is not proof.
	t.Setenv("FAKE_SYSTEMCTL_ENABLED", "0")
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "")
	if err := InstallUnit(argv); err != nil {
		t.Fatalf("InstallUnit did not retry enable on the next `stratum up`: %v", err)
	}
}

func TestInstallUnitReturnsTheQueryErrorWhenContentAlreadyMatches(t *testing.T) {
	callLog := fakeSystemd(t)
	argv := []string{"/opt/stratum/stratum", "up"}
	if err := InstallUnit(argv); err != nil {
		t.Fatalf("first InstallUnit: %v", err)
	}
	before := callCount(t, callLog, "enable")

	t.Setenv("FAKE_SYSTEMCTL_FAIL", "show")
	if err := InstallUnit(argv); err == nil {
		t.Fatal("InstallUnit swallowed unitEnabled's query failure instead of returning it")
	}
	after := callCount(t, callLog, "enable")
	if after != before {
		t.Fatalf("InstallUnit reinstalled over a query failure instead of just returning it: enable called %d times before, %d after", before, after)
	}
}

func TestInstallUnitRefusesWhenShowFailsEvenForStaleContent(t *testing.T) {
	callLog := fakeSystemd(t)
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up", "--worker"}); err != nil {
		t.Fatalf("first InstallUnit: %v", err)
	}
	before := callCount(t, callLog, "enable")

	t.Setenv("FAKE_SYSTEMCTL_FAIL", "show")
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up", "--config", "/etc/stratum/config.yaml"}); err == nil {
		t.Fatal("InstallUnit swallowed a systemctl show failure instead of refusing")
	}
	after := callCount(t, callLog, "enable")
	if after != before {
		t.Fatalf("InstallUnit enabled a unit it could not confirm was unmasked: enable called %d times before, %d after", before, after)
	}
}

func TestInstallUnitFailedWriteLeavesOldUnitIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions this test relies on")
	}
	fakeSystemd(t)
	old := []byte("OLD UNIT CONTENT, from a working install\n")
	if err := os.WriteFile(version.SystemdUnitPath, old, 0o644); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(version.SystemdUnitPath)
	// Readable but not writable: CreateTemp is denied without filling the disk.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	// Different argv, so the content comparison fails and InstallUnit must write.
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up", "--worker"}); err == nil {
		t.Fatal("InstallUnit succeeded despite an unwritable directory")
	}

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(version.SystemdUnitPath)
	if err != nil {
		t.Fatalf("unit file gone after the failed write: %v", err)
	}
	if string(got) != string(old) {
		t.Fatalf("failed write corrupted the old unit file: got %q, want %q", got, old)
	}
}

func TestInstallUnitSkipsAMaskedUnit(t *testing.T) {
	callLog := fakeSystemd(t)
	if err := os.Symlink(os.DevNull, version.SystemdUnitPath); err != nil {
		t.Fatal(err)
	}

	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err == nil {
		t.Fatal("InstallUnit did not refuse a masked unit")
	}
	if n := callCount(t, callLog, "enable"); n != 0 {
		t.Errorf("InstallUnit enabled a masked unit: enable called %d times", n)
	}
	if n := callCount(t, callLog, "daemon-reload"); n != 0 {
		t.Errorf("InstallUnit reloaded systemd for a masked unit: daemon-reload called %d times", n)
	}
	fi, err := os.Lstat(version.SystemdUnitPath)
	if err != nil {
		t.Fatalf("mask symlink gone: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("InstallUnit replaced the mask symlink with a regular file")
	}
	if target, err := os.Readlink(version.SystemdUnitPath); err != nil || target != os.DevNull {
		t.Fatalf("mask symlink target changed: target=%q err=%v", target, err)
	}
}

func TestInstallUnitSkipsWhenSystemctlReportsMasked(t *testing.T) {
	callLog := fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_UNIT_FILE_STATE", "masked")

	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err == nil {
		t.Fatal("InstallUnit did not refuse a unit systemctl reports masked")
	}
	if n := callCount(t, callLog, "enable"); n != 0 {
		t.Errorf("InstallUnit enabled a unit systemctl reports masked: enable called %d times", n)
	}
}

func TestInstallUnitSkipsWhenSystemctlReportsMaskedRuntime(t *testing.T) {
	callLog := fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_UNIT_FILE_STATE", "masked-runtime")

	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err == nil {
		t.Fatal("InstallUnit did not refuse a unit systemctl reports masked-runtime")
	}
	if n := callCount(t, callLog, "enable"); n != 0 {
		t.Errorf("InstallUnit enabled a unit systemctl reports masked-runtime: enable called %d times", n)
	}
	if _, err := os.Lstat(version.SystemdUnitPath); !os.IsNotExist(err) {
		t.Fatalf("InstallUnit wrote a unit file for a masked-runtime unit: Lstat err=%v", err)
	}
}

func TestDisableAndStopUnitNoopWithoutSystemd(t *testing.T) {
	dir := t.TempDir()
	old := systemdMarkerPath
	systemdMarkerPath = filepath.Join(dir, "absent")
	defer func() { systemdMarkerPath = old }()

	if err := DisableUnit(); err != nil {
		t.Fatalf("DisableUnit without systemd: %v", err)
	}
	if err := StopUnit(); err != nil {
		t.Fatalf("StopUnit without systemd: %v", err)
	}
	if enabled, err := UnitEnabled(); err != nil || enabled {
		t.Fatalf("UnitEnabled = %v, err=%v without systemd", enabled, err)
	}
	if err := EnableUnit(); err != nil {
		t.Fatalf("EnableUnit without systemd: %v", err)
	}
	if err := SuppressRestart(); err != nil {
		t.Fatalf("SuppressRestart without systemd: %v", err)
	}
	if err := AllowRestart(); err != nil {
		t.Fatalf("AllowRestart without systemd: %v", err)
	}
	if err := ReloadSystemd(); err != nil {
		t.Fatalf("ReloadSystemd without systemd: %v", err)
	}
}

func TestUnitEnabledAndEnableUnitRoundTrip(t *testing.T) {
	fakeSystemd(t)
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	if enabled, err := UnitEnabled(); err != nil || !enabled {
		t.Fatalf("UnitEnabled = %v, err=%v right after InstallUnit enabled it", enabled, err)
	}

	if err := DisableUnit(); err != nil {
		t.Fatalf("DisableUnit: %v", err)
	}
	t.Setenv("FAKE_SYSTEMCTL_ENABLED", "0")
	if enabled, err := UnitEnabled(); err != nil || enabled {
		t.Fatalf("UnitEnabled = %v, err=%v right after DisableUnit disabled it", enabled, err)
	}

	t.Setenv("FAKE_SYSTEMCTL_ENABLED", "")
	if err := EnableUnit(); err != nil {
		t.Fatalf("EnableUnit: %v", err)
	}
	if enabled, err := UnitEnabled(); err != nil || !enabled {
		t.Fatalf("UnitEnabled = %v, err=%v after EnableUnit restored it", enabled, err)
	}
}

func TestUnitEnabledReturnsErrorWhenQueryFails(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "run-systemd-system")
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	old := systemdMarkerPath
	systemdMarkerPath = marker
	t.Cleanup(func() { systemdMarkerPath = old })

	// Systemd present, but no systemctl anywhere in toolPath.
	oldPath := toolPath
	toolPath = t.TempDir()
	t.Cleanup(func() { toolPath = oldPath })
	if _, err := unitEnabled(); err == nil {
		t.Fatal("unitEnabled swallowed a systemctl invocation failure (PATH lookup)")
	}
}

func TestUnitEnabledUnexpectedUnitFileStateIsAnError(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_UNIT_FILE_STATE", "static")
	if _, err := unitEnabled(); err == nil {
		t.Fatal("unitEnabled swallowed an unrecognized UnitFileState")
	}
}

func TestUnitEnabledMaskedCountsAsDisabled(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_UNIT_FILE_STATE", "masked")
	if enabled, err := unitEnabled(); err != nil || enabled {
		t.Fatalf("unitEnabled(masked) = %v, err=%v, want false, nil", enabled, err)
	}
}

func TestUnitEnabledMaskedRuntimeIsStillAnError(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_UNIT_FILE_STATE", "masked-runtime")
	if _, err := unitEnabled(); err == nil {
		t.Fatal("unitEnabled swallowed masked-runtime")
	}
}

func TestReloadSystemdSurfacesRealFailures(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "daemon-reload")
	if err := ReloadSystemd(); err == nil {
		t.Fatal("ReloadSystemd swallowed a real systemctl failure")
	}
}

func TestEnableUnitAcceptsAbsentUnitButSurfacesRealFailures(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "enable")
	if err := EnableUnit(); err != nil {
		t.Fatalf("EnableUnit on an absent unit: %v", err)
	}

	t.Setenv("FAKE_SYSTEMCTL_FAIL", "")
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "enable")
	if err := EnableUnit(); err == nil {
		t.Fatal("EnableUnit swallowed a real systemctl failure")
	}
}

func TestSuppressRestartWritesADropInAllowRestartRemovesIt(t *testing.T) {
	fakeSystemd(t)
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	if err := SuppressRestart(); err != nil {
		t.Fatalf("SuppressRestart: %v", err)
	}
	got, err := os.ReadFile(systemdDropInPath)
	if err != nil {
		t.Fatalf("drop-in not written: %v", err)
	}
	if !strings.Contains(string(got), "Restart=no") {
		t.Errorf("drop-in does not disarm Restart=:\n%s", got)
	}

	if err := AllowRestart(); err != nil {
		t.Fatalf("AllowRestart: %v", err)
	}
	if _, err := os.Stat(systemdDropInPath); !os.IsNotExist(err) {
		t.Fatalf("AllowRestart left the drop-in in place: err=%v", err)
	}
}

func TestSuppressAndAllowRestartSurfaceRealFailures(t *testing.T) {
	fakeSystemd(t)
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "daemon-reload")
	if err := SuppressRestart(); err == nil {
		t.Fatal("SuppressRestart swallowed a real systemctl failure")
	}
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "")
	if err := SuppressRestart(); err != nil {
		t.Fatalf("SuppressRestart: %v", err)
	}
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "daemon-reload")
	if err := AllowRestart(); err == nil {
		t.Fatal("AllowRestart swallowed a real systemctl failure")
	}
}

func TestAllowRestartRetriesReloadWhenFileAlreadyAbsent(t *testing.T) {
	callLog := fakeSystemd(t)
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	if err := SuppressRestart(); err != nil {
		t.Fatalf("SuppressRestart: %v", err)
	}

	t.Setenv("FAKE_SYSTEMCTL_FAIL", "daemon-reload")
	if err := AllowRestart(); err == nil {
		t.Fatal("AllowRestart should have failed its own daemon-reload")
	}
	if _, err := os.Stat(systemdDropInPath); !os.IsNotExist(err) {
		t.Fatalf("drop-in should be removed even though the reload after it failed: err=%v", err)
	}
	before := callCount(t, callLog, "daemon-reload")

	t.Setenv("FAKE_SYSTEMCTL_FAIL", "")
	if err := AllowRestart(); err != nil {
		t.Fatalf("AllowRestart retry (drop-in already absent): %v", err)
	}
	after := callCount(t, callLog, "daemon-reload")
	if after <= before {
		t.Fatalf("retry did not reload: daemon-reload called %d times before, %d after", before, after)
	}
}

func TestReinstallAfterUninstallLeavesNoRestartOverride(t *testing.T) {
	fakeSystemd(t)
	argv := []string{"/opt/stratum/stratum", "up"}

	if err := InstallUnit(argv); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	if err := SuppressRestart(); err != nil {
		t.Fatalf("SuppressRestart: %v", err)
	}
	if err := StopUnit(); err != nil {
		t.Fatalf("StopUnit: %v", err)
	}
	if err := AllowRestart(); err != nil {
		t.Fatalf("AllowRestart: %v", err)
	}
	if _, err := os.Stat(systemdDropInPath); !os.IsNotExist(err) {
		t.Fatalf("drop-in still present after a successful uninstall: err=%v", err)
	}

	if err := InstallUnit(argv); err != nil {
		t.Fatalf("reinstall InstallUnit: %v", err)
	}
	if _, err := os.Stat(systemdDropInPath); !os.IsNotExist(err) {
		t.Fatalf("reinstall inherited a stale Restart=no override: err=%v", err)
	}
}

func TestUnitPresentFromSystemdStateSurvivesFileDeletion(t *testing.T) {
	callLog := fakeSystemd(t)
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	if err := os.Remove(version.SystemdUnitPath); err != nil {
		t.Fatalf("remove unit file: %v", err)
	}
	// What a real systemctl would say without a daemon-reload: still loaded, active.
	t.Setenv("FAKE_SYSTEMCTL_LOAD_STATE", "loaded")
	t.Setenv("FAKE_SYSTEMCTL_ACTIVE_STATE", "active")

	if err := StopUnit(); err != nil {
		t.Fatalf("StopUnit: %v", err)
	}
	if n := callCount(t, callLog, "stop"); n != 1 {
		t.Errorf("StopUnit skipped a unit systemd still has loaded and active (file-existence check, not systemd state): stop called %d times", n)
	}
	if err := SuppressRestart(); err != nil {
		t.Fatalf("SuppressRestart: %v", err)
	}
	if _, err := os.Stat(systemdDropInPath); err != nil {
		t.Errorf("SuppressRestart skipped a unit systemd still has loaded and active: drop-in not written: %v", err)
	}
}

func TestUnitPresentFromSystemdStateNoopWhenSystemdAlsoForgotIt(t *testing.T) {
	callLog := fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_LOAD_STATE", "not-found")
	t.Setenv("FAKE_SYSTEMCTL_ACTIVE_STATE", "inactive")
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "disable stop")

	if err := DisableUnit(); err != nil {
		t.Fatalf("DisableUnit on a unit absent from systemd's own state: %v", err)
	}
	if err := StopUnit(); err != nil {
		t.Fatalf("StopUnit on a unit absent from systemd's own state: %v", err)
	}
	if n := callCount(t, callLog, "stop"); n != 0 {
		t.Errorf("StopUnit called systemctl stop on a unit systemd itself reports not-found/inactive: %d calls", n)
	}
}

func TestUnitPresentReturnsErrorWhenShowFails(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "show")
	if _, err := unitPresent(); err == nil {
		t.Fatal("unitPresent swallowed a systemctl show failure")
	}
}

func TestUnitPresentNotFoundActiveCountsAsPresent(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_LOAD_STATE", "not-found")
	t.Setenv("FAKE_SYSTEMCTL_ACTIVE_STATE", "active")
	present, err := unitPresent()
	if err != nil {
		t.Fatalf("unitPresent: %v", err)
	}
	if !present {
		t.Error("unitPresent(LoadState=not-found, ActiveState=active) = false, want true")
	}
}

func TestUnitPresentNotFoundInactiveCountsAsAbsent(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_LOAD_STATE", "not-found")
	t.Setenv("FAKE_SYSTEMCTL_ACTIVE_STATE", "inactive")
	present, err := unitPresent()
	if err != nil {
		t.Fatalf("unitPresent: %v", err)
	}
	if present {
		t.Error("unitPresent(LoadState=not-found, ActiveState=inactive) = true, want false")
	}
}

func TestUnitActionsFailClosedWhenShowFails(t *testing.T) {
	fakeSystemd(t)
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "show")

	if err := DisableUnit(); err == nil {
		t.Error("DisableUnit swallowed a systemctl show failure (fail-open)")
	}
	if err := StopUnit(); err == nil {
		t.Error("StopUnit swallowed a systemctl show failure (fail-open)")
	}
	if err := SuppressRestart(); err == nil {
		t.Error("SuppressRestart swallowed a systemctl show failure (fail-open)")
	}
	if err := EnableUnit(); err == nil {
		t.Error("EnableUnit swallowed a systemctl show failure (fail-open)")
	}
}

func TestInstallUnitRemovesLeftoverRestartOverride(t *testing.T) {
	fakeSystemd(t)
	if err := os.MkdirAll(filepath.Dir(systemdDropInPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(systemdDropInPath, []byte("[Service]\nRestart=no\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	if _, err := os.Stat(systemdDropInPath); !os.IsNotExist(err) {
		t.Fatalf("InstallUnit left a leftover Restart=no override in place: err=%v", err)
	}
}

func TestInstallUnitRetriesRemoveStaleOverrideReloadWhenFileAlreadyAbsent(t *testing.T) {
	callLog := fakeSystemd(t)
	argv := []string{"/opt/stratum/stratum", "up"}
	if err := InstallUnit(argv); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	if err := SuppressRestart(); err != nil {
		t.Fatalf("SuppressRestart: %v", err)
	}

	t.Setenv("FAKE_SYSTEMCTL_FAIL", "daemon-reload")
	if err := InstallUnit(argv); err == nil {
		t.Fatal("InstallUnit should have failed removeStaleRestartOverride's own daemon-reload")
	}
	if _, err := os.Stat(systemdDropInPath); !os.IsNotExist(err) {
		t.Fatalf("drop-in should be removed even though the reload after it failed: err=%v", err)
	}
	before := callCount(t, callLog, "daemon-reload")

	t.Setenv("FAKE_SYSTEMCTL_FAIL", "")
	if err := InstallUnit(argv); err != nil {
		t.Fatalf("InstallUnit retry (drop-in already absent): %v", err)
	}
	after := callCount(t, callLog, "daemon-reload")
	if after <= before {
		t.Fatalf("retry did not reload: daemon-reload called %d times before, %d after", before, after)
	}
}

func TestDisableAndStopUnitAcceptAbsentUnit(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "disable stop")
	if err := DisableUnit(); err != nil {
		t.Fatalf("DisableUnit on an absent unit: %v", err)
	}
	if err := StopUnit(); err != nil {
		t.Fatalf("StopUnit on an absent unit: %v", err)
	}
}

func TestDisableAndStopUnitSurfaceRealFailures(t *testing.T) {
	fakeSystemd(t)
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "disable stop")
	if err := DisableUnit(); err == nil {
		t.Fatal("DisableUnit swallowed a real systemctl failure")
	}
	if err := StopUnit(); err == nil {
		t.Fatal("StopUnit swallowed a real systemctl failure")
	}
}

func TestWriteFileAtomicallySyncsTheDirectoryAfterTheRename(t *testing.T) {
	saved := syncDir
	t.Cleanup(func() { syncDir = saved })
	dir := t.TempDir()
	path := filepath.Join(dir, "f")

	var synced []string
	syncDir = func(d string) error {
		if b, err := os.ReadFile(path); err != nil || string(b) != "new" {
			t.Errorf("directory synced before the rename put the file in place (%q, %v)", b, err)
		}
		synced = append(synced, d)
		return saved(d)
	}
	if err := WriteFileAtomically(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("synced %q, want exactly the file's directory %q", synced, dir)
	}

	boom := errors.New("EIO")
	syncDir = func(string) error { return boom }
	if err := WriteFileAtomically(path, []byte("again"), 0o600); !errors.Is(err, boom) || !errors.Is(err, ErrNotDurable) {
		t.Fatalf("err = %v, want the directory sync's error, marked ErrNotDurable", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "again" {
		t.Fatalf("%s = %q, want the new contents, which the rename put in place", path, b)
	}
}

func TestWriteFileAtomicallyFailureBeforeTheRename(t *testing.T) {
	savedSync, savedRemove := syncFile, removeTempName
	t.Cleanup(func() { syncFile, removeTempName = savedSync, savedRemove })
	boom := errors.New("EIO")
	syncFile = func(*os.File) error { return boom }
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomically(path, []byte("new"), 0o600); !errors.Is(err, boom) || errors.Is(err, ErrStrayCopy) || errors.Is(err, ErrNotDurable) {
		t.Fatalf("err = %v, want the sync's error alone", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Errorf("%s = %q after a failed write, want %q", path, b, "old")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %d entries after a failed write, want only %s", len(entries), path)
	}

	removeTempName = func(string) error { return errors.New("EROFS") }
	err := WriteFileAtomically(path, []byte("new"), 0o600)
	if !errors.Is(err, boom) || !errors.Is(err, ErrStrayCopy) || !strings.Contains(err.Error(), "EROFS") {
		t.Fatalf("err = %v, want the sync's error, ErrStrayCopy and the removal's error", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "f" && !strings.Contains(err.Error(), filepath.Join(dir, e.Name())) {
			t.Errorf("error does not name the temp file %s: %v", e.Name(), err)
		}
	}
}

func TestSyncDirReportsAnUnopenableDirectory(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("syncDir reported success for a directory that does not exist")
	}
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir on a real directory: %v", err)
	}
}

// A mode changed after the fsync is not covered by it.
func TestAtomicWritesSetTheModeBeforeTheSync(t *testing.T) {
	saved := syncFile
	t.Cleanup(func() { syncFile = saved })
	var modes []os.FileMode
	syncFile = func(f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		modes = append(modes, fi.Mode().Perm())
		return saved(f)
	}
	dir := t.TempDir()
	if err := WriteFileAtomically(filepath.Join(dir, "unit"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateFileExclusively(filepath.Join(dir, "key"), []byte("k"), 0o640); err != nil {
		t.Fatal(err)
	}
	if len(modes) != 2 || modes[0] != 0o644 || modes[1] != 0o640 {
		t.Fatalf("modes at sync time = %v, want [-rw-r--r-- -rw-r-----]", modes)
	}
}

func TestCreateFileExclusively(t *testing.T) {
	savedSync := syncDir
	t.Cleanup(func() { syncDir = savedSync })
	onlyEntries := func(t *testing.T, dir string, want ...string) {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("directory holds %v, want %v (a temp file left behind?)", got, want)
		}
	}

	t.Run("creates the file at its mode, durably, and returns it", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "key")
		var synced []string
		syncDir = func(d string) error {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("directory synced before the file was linked in: %v", err)
			}
			// The temp name is gone before the sync, so the sync covers its removal too.
			if entries, _ := os.ReadDir(d); len(entries) != 1 {
				t.Errorf("directory synced with %d entries, want only the file (temp name still there?)", len(entries))
			}
			synced = append(synced, d)
			return savedSync(d)
		}
		created, err := CreateFileExclusively(path, []byte("k"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
		cur, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(created, cur) {
			t.Fatal("the returned FileInfo is not the file at path")
		}
		if cur.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v, want 0600", cur.Mode().Perm())
		}
		if len(synced) != 1 || synced[0] != dir {
			t.Fatalf("synced %v, want exactly %s", synced, dir)
		}
		onlyEntries(t, dir, "key")
	})
	t.Run("refuses to replace a file that is already there", func(t *testing.T) {
		syncDir = savedSync
		dir := t.TempDir()
		path := filepath.Join(dir, "key")
		if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
			t.Fatal(err)
		}
		created, err := CreateFileExclusively(path, []byte("mine"), 0o600)
		if !errors.Is(err, fs.ErrExist) || created != nil {
			t.Fatalf("got %v, %v; want nil and an fs.ErrExist error", created, err)
		}
		if b, _ := os.ReadFile(path); string(b) != "theirs" {
			t.Fatalf("the existing file now holds %q", b)
		}
		onlyEntries(t, dir, "key")
	})
	t.Run("a failed directory sync still reports what it created", func(t *testing.T) {
		boom := errors.New("EIO")
		syncDir = func(string) error { return boom }
		dir := t.TempDir()
		path := filepath.Join(dir, "key")
		created, err := CreateFileExclusively(path, []byte("k"), 0o600)
		if !errors.Is(err, boom) || created == nil {
			t.Fatalf("got %v, %v; want the created file and the sync's error", created, err)
		}
		onlyEntries(t, dir, "key")
	})
	t.Run("a temp file that outlives a failed link is reported", func(t *testing.T) {
		savedRemove := removeTempName
		removeTempName = func(string) error { return errors.New("EIO") }
		t.Cleanup(func() { removeTempName = savedRemove })
		dir := t.TempDir()
		path := filepath.Join(dir, "key")
		if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
			t.Fatal(err)
		}
		created, err := CreateFileExclusively(path, []byte("mine"), 0o600)
		if !errors.Is(err, fs.ErrExist) || !errors.Is(err, ErrStrayCopy) || created != nil || !strings.Contains(err.Error(), "EIO") {
			t.Fatalf("got %v, %v; want nil, and fs.ErrExist with ErrStrayCopy and the removal's error", created, err)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != "key" && !strings.Contains(err.Error(), filepath.Join(dir, e.Name())) {
				t.Errorf("error does not name the temp file %s: %v", e.Name(), err)
			}
		}
	})
	t.Run("a temp name that outlives the link is reported, not retried silently", func(t *testing.T) {
		synced := 0
		syncDir = func(string) error { synced++; return nil }
		savedRemove := removeTempName
		removeTempName = func(string) error { return errors.New("EIO") }
		t.Cleanup(func() { removeTempName = savedRemove })
		dir := t.TempDir()
		path := filepath.Join(dir, "key")
		created, err := CreateFileExclusively(path, []byte("k"), 0o600)
		if !errors.Is(err, ErrStrayCopy) || created == nil || !strings.Contains(err.Error(), "EIO") {
			t.Fatalf("got %v, %v; want the created file and ErrStrayCopy with the removal's error", created, err)
		}
		if synced != 0 {
			t.Errorf("synced the directory %d times with a stray copy outstanding", synced)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 2 {
			t.Fatalf("dir holds %d entries, want the key and its temp name", len(entries))
		}
		for _, e := range entries {
			if e.Name() != "key" && !strings.Contains(err.Error(), filepath.Join(dir, e.Name())) {
				t.Errorf("error does not name the temp file %s: %v", e.Name(), err)
			}
		}
	})
}

func TestExecStartPath(t *testing.T) {
	for show, want := range map[string]string{
		"{ path=/usr/local/bin/stratum ; argv[]=/usr/local/bin/stratum up --worker ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n": "/usr/local/bin/stratum",
		"{ path=/dist/stratum ; argv[]=/dist/stratum up --config /etc/stratum/cluster.yaml ; ignore_errors=no }":                                                                   "/dist/stratum",
	} {
		if got, err := execStartPath(show); err != nil || got != want {
			t.Errorf("execStartPath(%q) = %q, %v; want %q", show, got, err, want)
		}
	}
	for _, show := range []string{"", "\n", "{ argv[]=x ; }", "{ path= ; argv[]=x }"} {
		if got, err := execStartPath(show); err == nil {
			t.Errorf("execStartPath(%q) = %q, want an error", show, got)
		}
	}
}

func TestUnitArgvReadsBackWhatInstallUnitWrote(t *testing.T) {
	fakeSystemd(t)
	for _, cfg := range []string{
		"/etc/stratum/config.yaml",
		"/etc/stratum config/config.yaml",
		"/etc/100%-stratum/config.yaml",
		"/etc/$HOME/config.yaml",
		`/etc/stratum "quoted"/config.yaml`,
		"/etc/o'brien/config.yaml",
		`/etc/back\slash/config.yaml`,
		"",
	} {
		argv := []string{"/usr/local/bin/stratum", "up", "--config", cfg, "--cis"}
		if err := InstallUnit(argv); err != nil {
			t.Fatalf("InstallUnit(%q): %v", argv, err)
		}
		got, err := UnitArgv()
		if err != nil || !slices.Equal(got, argv) {
			t.Errorf("UnitArgv() after InstallUnit(%q) = %q, %v", argv, got, err)
		}
	}
}

func TestUnitArgvWithNoUnit(t *testing.T) {
	fakeSystemd(t)
	if _, err := UnitArgv(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("UnitArgv() with no unit = %v, want fs.ErrNotExist", err)
	}
}

func TestExecStartArgvRefusesWhatItCannotRead(t *testing.T) {
	for _, unit := range []string{
		"[Service]\nType=simple\n",
		"[Service]\nExecStart=\n",
		"[Service]\nExecStart=/s up --config \"/etc/unterminated\n",
		"[Service]\nExecStart=/s up --config /etc/%h/config.yaml\n",
		"[Service]\nExecStart=/s up --config /etc/$HOME/config.yaml\n",
		"[Service]\nExecStart=/s up --config /etc/a\\tb.yaml\n",
		"[Service]\nExecStart=/s up --config /etc/a.yaml\nExecStart=/s up --config /etc/b.yaml\n",
		"[Unit]\nExecStart=/s up --config /etc/a.yaml\n",
	} {
		if got, err := execStartArgv(unit); err == nil {
			t.Errorf("execStartArgv(%q) = %q, want an error", unit, got)
		}
	}
	for unit, want := range map[string][]string{
		// Only [Service] counts: systemd ignores ExecStart= anywhere else.
		"[Service]\nExecStart=/s up --config /etc/a.yaml\n[Install]\nExecStart=/s up --config /etc/b.yaml\n": {"/s", "up", "--config", "/etc/a.yaml"},
		// systemd strips the whitespace around the =.
		"[Service]\nExecStart = /s up --config /etc/a.yaml\n": {"/s", "up", "--config", "/etc/a.yaml"},
		// An empty assignment resets the list.
		"[Service]\nExecStart=/old up\nExecStart=\nExecStart=/s up --config '/etc/single quoted.yaml'\n": {"/s", "up", "--config", "/etc/single quoted.yaml"},
	} {
		if got, err := execStartArgv(unit); err != nil || !slices.Equal(got, want) {
			t.Errorf("execStartArgv(%q) = %q, %v; want %q", unit, got, err, want)
		}
	}
}

func TestUnitArgvRefusesADropInExecStart(t *testing.T) {
	fakeSystemd(t)
	if err := InstallUnit([]string{"/usr/local/bin/stratum", "up", "--config", "/etc/stratum/config.yaml"}); err != nil {
		t.Fatal(err)
	}
	dirs := unitDropInDirs()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dirs[0], "restart.conf"), []byte("[Service]\nRestartSec=10s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := UnitArgv(); err != nil {
		t.Fatalf("UnitArgv() with a drop-in that leaves ExecStart= alone = %v", err)
	}
	for i, dir := range dirs {
		for _, line := range []string{"ExecStart=/usr/local/bin/stratum up --config /etc/other.yaml", "ExecStart = /usr/local/bin/stratum up --config /etc/other.yaml"} {
			conf := filepath.Join(dir, "override.conf")
			if err := os.WriteFile(conf, []byte("[Service]\nExecStart=\n"+line+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, err := UnitArgv(); err == nil || !strings.Contains(err.Error(), conf) {
				t.Errorf("UnitArgv() with %q in drop-in dir %d = %q, %v; want an error naming it", line, i, got, err)
			}
			if err := os.Remove(conf); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestUnitDropInDirs(t *testing.T) {
	got := unitDropInDirs()
	for _, want := range []string{
		"/etc/systemd/system/stratum.service.d", "/etc/systemd/system/service.d",
		"/run/systemd/system/stratum.service.d", "/run/systemd/system/service.d",
		"/usr/local/lib/systemd/system/stratum.service.d", "/usr/lib/systemd/system/stratum.service.d",
		"/usr/lib/systemd/system/service.d",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("unitDropInDirs() = %q, missing %s", got, want)
		}
	}
}

func TestUnitArgvTreatsAMaskAsNoUnit(t *testing.T) {
	fakeSystemd(t)
	if err := os.Symlink(os.DevNull, version.SystemdUnitPath); err != nil {
		t.Fatal(err)
	}
	if got, err := UnitArgv(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("UnitArgv() of a masked unit = %q, %v; want fs.ErrNotExist", got, err)
	}
}

func TestUnitArgvSkipsAVanishedDropIn(t *testing.T) {
	fakeSystemd(t)
	argv := []string{"/usr/local/bin/stratum", "up", "--config", "/etc/stratum/config.yaml"}
	if err := InstallUnit(argv); err != nil {
		t.Fatal(err)
	}
	dir := unitDropInDirs()[0]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone.conf"), filepath.Join(dir, "10-override.conf")); err != nil {
		t.Fatal(err)
	}
	if got, err := UnitArgv(); err != nil || !slices.Equal(got, argv) {
		t.Fatalf("UnitArgv() with a dangling drop-in = %q, %v; want the unit's argv", got, err)
	}
}

// Tools run as root: caller-set variables that load code must not reach them.
func TestHostCmdEnvIsExplicit(t *testing.T) {
	t.Setenv("XTABLES_LIBDIR", "/evil")
	t.Setenv("TC_LIB_DIR", "/evil")

	got := hostCmd("systemctl", "show").Env
	if want := []string{"PATH=" + toolenv.Path, "LC_ALL=C"}; !slices.Equal(got, want) {
		t.Errorf("Env = %q, want exactly %q", got, want)
	}
}

func TestHostCmdIgnoresTheCallersPath(t *testing.T) {
	shim, real := t.TempDir(), t.TempDir()
	for _, d := range []string{shim, real} {
		if err := os.WriteFile(filepath.Join(d, "systemctl"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", shim)
	oldPath := toolPath
	toolPath = real
	t.Cleanup(func() { toolPath = oldPath })

	if want := filepath.Join(real, "systemctl"); hostCmd("systemctl").Path != want {
		t.Errorf("Path = %q, want %q", hostCmd("systemctl").Path, want)
	}
	if err := hostCmd("absent").Err; !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("Err = %v, want ErrNotFound for a tool that is not in toolPath", err)
	}
}

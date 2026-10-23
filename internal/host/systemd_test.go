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

// fakeSystemd points SystemdAvailable at a temp marker (present) and puts a
// scriptable systemctl stub on PATH, so these tests exercise the real
// install/disable/stop logic without a real systemd or root. Returns the path
// to a log of every verb the stub was invoked with, one per line, for tests
// that need to prove a call actually happened rather than just its result.
//
// The stub reads these env vars, all empty/unset by default:
//   - FAKE_SYSTEMCTL_ENABLED=0    makes "show -p UnitFileState" report
//     "disabled" instead of "enabled" when the fake unit file exists
//   - FAKE_SYSTEMCTL_FAIL="v1 v2" makes those systemctl verbs fail
//   - FAKE_SYSTEMCTL_LOAD_STATE / FAKE_SYSTEMCTL_ACTIVE_STATE override "show
//     -p LoadState,ActiveState"'s answer; unset, it reports loaded/active
//     when the fake unit file exists on disk and not-found/inactive when it
//     doesn't — real systemd only if that file existence tracked its own
//     state, which is exactly the gap unitPresent() covers, so a test for
//     that gap sets these explicitly instead of just deleting the file.
//   - FAKE_SYSTEMCTL_UNIT_FILE_STATE overrides "show -p UnitFileState"'s
//     answer outright, for a test that needs a state other than
//     enabled/disabled/not-found (static, masked, ...).
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

	// UnitArgv's drop-in search, kept off the host's /run and vendor trees:
	// the unit's own beside it, and stand-ins for /run's and a shared one.
	oldDirs := unitDropInDirs
	unitDropInDirs = func() []string {
		return []string{unit + ".d", filepath.Join(dir, "run", "stratum.service.d"), filepath.Join(dir, "lib", "service.d")}
	}
	t.Cleanup(func() { unitDropInDirs = oldDirs })

	return callLog
}

// callCount returns how many times verb appears in a fakeSystemd call log.
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

// TestInstallUnitNoopWithoutSystemd is the out-of-scope contract for
// non-systemd hosts: no marker, no unit file, no error.
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

// TestInstallUnitRendersRestartExecStart is a regression test: the
// unit's ExecStart must be the steady-state restart command, not whatever
// one-shot join flags first brought the node up — FAILS if a caller ever
// passes --join/--token straight through instead of the resolved argv.
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
	// KillMode=process, not mixed: mixed would SIGKILL containerd's shims on
	// every ordinary restart too (Delegate=yes puts them in this same
	// cgroup), breaking "containers survive a supervisor stop". A crash
	// without a reboot still just gets refused by pkg/cli's
	// preflightNoRunningNode and asks for a human; reaping those
	// orphans is out of scope here (a reboot leaves none) and is a
	// follow-up.
	if !strings.Contains(string(got), "KillMode=process") {
		t.Errorf("unit must not use KillMode=mixed/control-group: that would kill containerd's shims"+
			" (and the containers they hold) on every restart, not just a crash:\n%s", got)
	}
}

// TestInstallUnitRendersAnOptionalEnvironmentFile is a regression test: the
// generated unit's near-empty environment silently drops any environment-only
// S3 credentials a scheduled etcd snapshot upload needs once systemd, not an
// operator's login shell, is what restarts `stratum up`.
// EnvironmentFile=-/etc/stratum/stratum.env gives an operator a place to put
// them back; the leading `-` is load-bearing — without it, an operator who has
// never created the file would have this unit refuse to start at all.
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

// TestEscapeSystemdArg is the regression test for ExecStart= escaping:
// unescaped, `%` starts a specifier and `$` starts a variable reference in
// systemd's command-line grammar, so an operator-supplied --config path
// containing either would be silently reinterpreted or truncated rather than
// passed through — a value would change with no error anywhere. Backslash is
// escaped too, since it is systemd's own C-style escape lead-in. A bare `'`
// is the same kind of gap: unquoted, it starts a quoted
// span of its own in systemd's grammar, same as an unquoted `"` — so a path
// containing one but no space/tab/`"` must not go through unquoted, or
// systemd, not this function, decides where the "quoted" span ended.
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

// TestEscapeSystemdArgRefusesCRAndLF is a regression test:
// systemd's command-line grammar has no way to express a literal CR/LF
// inside a double-quoted argument (only a line continuation), so passing one
// through would corrupt ExecStart= into extra directive lines rather than
// carry the character — an error is the honest answer, not silent mangling.
// A --config path is never legitimately going to contain one.
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

// TestInstallUnitIdempotent guards the early-return: re-installing the same
// argv when the unit is ALREADY enabled must not re-run enable (a caller
// that did that on every restart, forever, would just be noise — and in a
// container with no real systemd, an error). It still checks enablement
// every time (TestInstallUnitRetriesEnableAfterAFailure is what that buys),
// so only enable is asserted skipped here, not systemctl entirely.
//
// daemon-reload is different: removeStaleRestartOverride
// now reloads systemd on every InstallUnit call, unconditionally, whether or
// not the unit itself changed — so this test asserts that call still
// happens, not that it is skipped.
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

// TestInstallUnitRetriesEnableAfterAFailure is a regression
// test: matching unit bytes alone must never be trusted as proof of
// enablement. Fails if a first `systemctl enable` that failed
// (permissions, a wedged dbus) is accepted forever because the file
// on disk already matched, leaving the node unarmed for its next reboot with
// no further sign anything was wrong.
func TestInstallUnitRetriesEnableAfterAFailure(t *testing.T) {
	fakeSystemd(t)
	argv := []string{"/opt/stratum/stratum", "up"}

	t.Setenv("FAKE_SYSTEMCTL_FAIL", "enable")
	if err := InstallUnit(argv); err == nil {
		t.Fatal("InstallUnit swallowed a systemctl enable failure")
	}

	// The unit file was written before the failing enable call, so a naive
	// content-match check would now treat this as "already installed" even
	// though it was never actually enabled.
	t.Setenv("FAKE_SYSTEMCTL_ENABLED", "0")
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "")
	if err := InstallUnit(argv); err != nil {
		t.Fatalf("InstallUnit did not retry enable on the next `stratum up`: %v", err)
	}
}

// TestInstallUnitReturnsTheQueryErrorWhenContentAlreadyMatches is a
// regression test for InstallUnit's other caller: when the
// unit file on disk already matches what this call would write, a query
// failure from unitEnabled is not a reason to rewrite and re-enable a unit
// that has nothing wrong with its content — InstallUnit returns the query
// error itself (which runUp turns into a warning), rather than paying for a
// reinstall that answers a question the query never got to ask.
// Fails if a query failure is treated as "not proof of enablement"
// unconditionally and always falls through to reinstall, even when the
// content already matched.
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

// TestInstallUnitRefusesWhenShowFailsEvenForStaleContent is the other half:
// a `show` failure blocks InstallUnit before it ever gets to
// compare content, because unitMasked (which runs first, to catch a mask
// before anything is written) cannot tell a query failure apart from an
// actual mask, and fails closed the same way unitPresent and unitEnabled do
// elsewhere in this file. Delaying a routine reinstall by one cycle is the
// acceptable cost of never risking a write over a mask this call could not
// rule out. FAILS if InstallUnit ever goes back to swallowing this error and
// reinstalling regardless.
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

// TestInstallUnitFailedWriteLeavesOldUnitIntact is a regression test for the
// atomic write: a write interrupted partway through (disk full,
// the process killed) must not leave a truncated or corrupt unit file where a
// working one was — the exact node that most needs its
// reboot-persistence unit intact, one that just failed a reinstall.
// WriteFileAtomically's temp-file-then-rename means a failure before the rename
// never touches the file already at path. Fails with a direct
// os.WriteFile, which has already truncated the destination by the time
// CreateTemp's equivalent failure would occur, so the old content is gone even
// though the write as a whole failed.
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
	// Readable and searchable but not writable: CreateTemp in this directory
	// is denied, simulating a write failure without needing to actually fill
	// the disk.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	// Different argv, so the content comparison fails and InstallUnit must
	// try to write — the unitEnabled/content-matches short-circuit is not
	// what is under test here.
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

// TestInstallUnitSkipsAMaskedUnit is a regression test: a
// persistent `systemctl mask` replaces the unit file with a symlink to
// /dev/null; InstallUnit must detect that symlink BEFORE writing anything
// and refuse, rather than silently clobbering the mask with a fresh,
// newly-enabled unit — which is exactly what renaming a new file into that
// path would otherwise do. Fails if the symlink is read through (an empty
// read from /dev/null) and the content mismatch answered by writing,
// reloading, and enabling a brand new unit over the mask.
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

// TestInstallUnitSkipsWhenSystemctlReportsMasked is the other half: even
// without a local symlink InstallUnit can Lstat, systemctl's own
// UnitFileState=masked answer must be enough on its own to refuse.
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

// TestInstallUnitSkipsWhenSystemctlReportsMaskedRuntime is a
// regression test: masked-runtime (a /run-only mask, no symlink at
// version.SystemdUnitPath for Lstat to see at all) must count as masked for
// InstallUnit's write-protection the same as a persistent mask does —
// unitMasked's job is "may this call write here", not "is the unit file
// enabled" (that distinction belongs to unitEnabled, which still treats
// masked-runtime as an error via TestUnitEnabledMaskedRuntimeIsStillAnError).
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

	// Must not panic or shell out to a systemctl that doesn't exist.
	if err := DisableUnit(); err != nil {
		t.Fatalf("DisableUnit without systemd: %v", err)
	}
	if err := StopUnit(); err != nil {
		t.Fatalf("StopUnit without systemd: %v", err)
	}
	// UnitEnabled must fail closed to "not enabled, no error" here — never
	// shell out to a systemctl this host may not even have.
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

// TestUnitEnabledAndEnableUnitRoundTrip is a regression
// test for uninstall's rollback: DisableUnit followed by EnableUnit must put
// enablement back exactly where UnitEnabled found it, so a failed teardown's
// abort can restore what it saw before touching anything.
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

// TestUnitEnabledReturnsErrorWhenQueryFails is a regression
// test: unitEnabled must distinguish a clean non-zero exit (systemctl ran and
// reported not-enabled — a real, deterministic answer) from an invocation
// that never ran at all (systemctl missing from PATH, here) — the latter
// must surface as an error, not get folded into "disabled". Fails if
// unitEnabled returns false, nil for any non-nil error, including this one.
func TestUnitEnabledReturnsErrorWhenQueryFails(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "run-systemd-system")
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	old := systemdMarkerPath
	systemdMarkerPath = marker
	t.Cleanup(func() { systemdMarkerPath = old })

	// Systemd present (so the SystemdAvailable guard does not short-circuit
	// first), but no systemctl anywhere in toolPath to run it with.
	oldPath := toolPath
	toolPath = t.TempDir()
	t.Cleanup(func() { toolPath = oldPath })
	if _, err := unitEnabled(); err == nil {
		t.Fatal("unitEnabled swallowed a systemctl invocation failure (PATH lookup)")
	}
}

// TestUnitEnabledUnexpectedUnitFileStateIsAnError is a regression test for
// unitEnabled's other error path: a UnitFileState this function does not
// recognize (static, masked, ...) needs a human, not a silent "go ahead and
// enable it anyway".
func TestUnitEnabledUnexpectedUnitFileStateIsAnError(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_UNIT_FILE_STATE", "static")
	if _, err := unitEnabled(); err == nil {
		t.Fatal("unitEnabled swallowed an unrecognized UnitFileState")
	}
}

// TestUnitEnabledMaskedCountsAsDisabled is a regression
// test: an operator's own `systemctl mask` reports UnitFileState=masked,
// which must count as "not boot-enabled" (false, nil) — the same as
// disabled/not-found — rather than an error. A masked unit still refuses
// `systemctl enable` on its own, so unitEnabled does not need to duplicate
// that refusal; treating it as an error here would only block uninstall's
// unitWasEnabled capture (and InstallUnit's retry path) on a node an
// operator deliberately masked.
func TestUnitEnabledMaskedCountsAsDisabled(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_UNIT_FILE_STATE", "masked")
	if enabled, err := unitEnabled(); err != nil || enabled {
		t.Fatalf("unitEnabled(masked) = %v, err=%v, want false, nil", enabled, err)
	}
}

// TestUnitEnabledMaskedRuntimeIsStillAnError is the other half: unlike a
// persistent mask, a /run-only mask on this unit would be unusual (nothing
// in this codebase writes one), so it stays in the error bucket rather than
// being silently folded into "disabled" alongside its persistent sibling.
func TestUnitEnabledMaskedRuntimeIsStillAnError(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_UNIT_FILE_STATE", "masked-runtime")
	if _, err := unitEnabled(); err == nil {
		t.Fatal("unitEnabled swallowed masked-runtime")
	}
}

// TestReloadSystemdSurfacesRealFailures is a regression
// test: a real daemon-reload failure must be returned so uninstall
// (which folds it into firstErr) does not report success while systemd still
// serves a just-deleted unit's last-loaded definition.
func TestReloadSystemdSurfacesRealFailures(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "daemon-reload")
	if err := ReloadSystemd(); err == nil {
		t.Fatal("ReloadSystemd swallowed a real systemctl failure")
	}
}

// TestEnableUnitAcceptsAbsentUnitButSurfacesRealFailures mirrors
// TestDisableAndStopUnitAcceptAbsentUnit/SurfaceRealFailures for EnableUnit:
// rollback calls it unconditionally, so a node that was never installed must
// not error, but a genuine systemctl failure must not be swallowed.
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

// TestSuppressRestartWritesADropInAllowRestartRemovesIt is a
// regression test for the restart race: `systemctl disable` alone does not
// stop Restart=always from firing on the ACTIVE unit, so uninstall needs a
// separate mechanism — a runtime drop-in — to disarm it for the teardown
// window, and a way to put it back.
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

// TestAllowRestartRetriesReloadWhenFileAlreadyAbsent is a regression
// test: a call that removes the drop-in and then fails its own daemon-reload
// leaves the file gone but systemd's LIVE config still holding the old
// Restart=no — a retry must still reload, not treat "no file to remove" as
// "nothing to do". Fails if the second call sees the drop-in
// already absent and returns success without ever invoking daemon-reload
// again.
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

// TestReinstallAfterUninstallLeavesNoRestartOverride is a
// regression test for a same-boot reinstall: uninstall's teardown sequence
// (SuppressRestart, then — on success — StopUnit followed by AllowRestart)
// must leave no Restart=no override behind for the NEXT InstallUnit to
// inherit. Fails when AllowRestart is missing from the success
// path: the drop-in survives the uninstall, and a `stratum up` run before
// the next real reboot clears /run on its own installs a fresh unit that is
// silently unable to recover from a crash until then.
func TestReinstallAfterUninstallLeavesNoRestartOverride(t *testing.T) {
	fakeSystemd(t)
	argv := []string{"/opt/stratum/stratum", "up"}

	// stratum up.
	if err := InstallUnit(argv); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	// stratum uninstall's teardown sequence, success path.
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

	// stratum up again, same boot — no reboot in between to clear /run.
	if err := InstallUnit(argv); err != nil {
		t.Fatalf("reinstall InstallUnit: %v", err)
	}
	if _, err := os.Stat(systemdDropInPath); !os.IsNotExist(err) {
		t.Fatalf("reinstall inherited a stale Restart=no override: err=%v", err)
	}
}

// TestUnitPresentFromSystemdStateSurvivesFileDeletion is a regression test:
// systemd only re-reads unit files on daemon-reload, so a unit file removed by
// hand (or a teardown step that got this far and then failed before
// ReloadSystemd) does not by itself unload or stop a unit systemd already has
// in memory. Fails if DisableUnit, StopUnit and SuppressRestart
// decide "nothing to do" purely from os.Stat on the unit file, silently
// skipping a still-loaded, still ACTIVE unit the moment its file is
// gone, whether or not systemd has actually forgotten it.
func TestUnitPresentFromSystemdStateSurvivesFileDeletion(t *testing.T) {
	callLog := fakeSystemd(t)
	if err := InstallUnit([]string{"/opt/stratum/stratum", "up"}); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	if err := os.Remove(version.SystemdUnitPath); err != nil {
		t.Fatalf("remove unit file: %v", err)
	}
	// The file is gone, but tell the fake systemctl what a real one would
	// still say without a daemon-reload: the unit is still loaded and active.
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

// TestUnitPresentFromSystemdStateNoopWhenSystemdAlsoForgotIt is the other
// half: a unit gone from both the unit file AND systemd's own state
// (LoadState=not-found) is genuinely absent, matching
// TestDisableAndStopUnitAcceptAbsentUnit's file-based contract.
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

// TestUnitPresentReturnsErrorWhenShowFails is a regression
// test: a systemctl show failure (dbus wedged, systemctl missing) must be
// returned as an error, not swallowed into "absent" — the exact fail-open bug
// unitPresent's callers (DisableUnit/StopUnit/SuppressRestart/EnableUnit)
// cannot afford. Fails if unitPresent returns false, nil.
func TestUnitPresentReturnsErrorWhenShowFails(t *testing.T) {
	fakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "show")
	if _, err := unitPresent(); err == nil {
		t.Fatal("unitPresent swallowed a systemctl show failure")
	}
}

// TestUnitPresentNotFoundActiveCountsAsPresent and
// TestUnitPresentNotFoundInactiveCountsAsAbsent pin the two edges of
// unitPresent's decision, straight from systemctl's own field names: an
// odd-looking not-found/active combination (a unit systemd still has an
// active instance of, even once its LoadState no longer resolves — a rare
// but real transition state) must still count as present, while
// not-found/inactive — genuinely gone — must count as absent, matching
// TestDisableAndStopUnitAcceptAbsentUnit's contract.
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

// TestUnitActionsFailClosedWhenShowFails is a regression
// test for the caller side of the same fix: DisableUnit, StopUnit and
// SuppressRestart (and EnableUnit, for the same reason) must surface a
// systemctl show failure as their own error rather than silently treating an
// unknowable unit as absent and reporting success — uninstall's whole point
// in checking is to not proceed while it cannot tell whether the unit is
// still armed. EnableUnit is included here too even though only the
// other three were reported: it shares the exact same unitPresent() guard,
// and abort() already tolerates an EnableUnit failure (folds it into its
// "failed" account rather than panicking), so there is no reason to leave it
// fail-open while its three siblings fail closed.
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

// TestInstallUnitRemovesLeftoverRestartOverride is a regression test for the
// other half of the same-boot reinstall: when uninstall's own AllowRestart
// cannot clean up the Restart=no override (a warning, not an abort, once
// StopUnit has already succeeded — see internal/uninstall.Run), the next
// InstallUnit must still strip it, so a `stratum up` before the next real
// reboot clears /run never silently inherits Restart=no. Fails if
// InstallUnit does not look at systemdDropInPath at all.
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

// TestInstallUnitRetriesRemoveStaleOverrideReloadWhenFileAlreadyAbsent is a
// regression test, in the same style as
// TestAllowRestartRetriesReloadWhenFileAlreadyAbsent: removeStaleRestartOverride
// must reload systemd every time it runs, not only when it actually removed a
// file. A call that removes the drop-in and then fails its OWN daemon-reload
// leaves the file gone but systemd's LIVE config still holding the old
// Restart=no; the next `stratum up` must still reload even though there is
// nothing left to remove. Fails if the retry sees the drop-in
// already absent and skips daemon-reload entirely.
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

// TestDisableAndStopUnitAcceptAbsentUnit is a contract:
// uninstall runs on every node, including one where the unit was never
// installed (e.g. it was always non-systemd, or InstallUnit never got that
// far) — that must not be an error, only a REAL systemctl failure should be.
func TestDisableAndStopUnitAcceptAbsentUnit(t *testing.T) {
	fakeSystemd(t)
	// No InstallUnit call: version.SystemdUnitPath does not exist on disk.
	t.Setenv("FAKE_SYSTEMCTL_FAIL", "disable stop")
	if err := DisableUnit(); err != nil {
		t.Fatalf("DisableUnit on an absent unit: %v", err)
	}
	if err := StopUnit(); err != nil {
		t.Fatalf("StopUnit on an absent unit: %v", err)
	}
}

// TestDisableAndStopUnitSurfaceRealFailures is the other half of that
// contract: uninstall must not paper over a genuine stop/disable
// failure and proceed to delete state while the unit could still be armed.
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

// TestWriteFileAtomicallySyncsTheDirectoryAfterTheRename: the
// rename is durable only once the directory holding it is synced, so the sync
// has to happen, on that directory, after the file is in place -- and a failed
// sync is a failed write, since the caller cannot know the rename will last.
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

// TestWriteFileAtomicallyFailureBeforeTheRename: the file at path is left
// byte for byte, the temp file is removed, and a temp file that cannot be
// removed is named in the error.
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

// TestSyncDirReportsAnUnopenableDirectory: the real sync, not the seam, fails
// on a directory it cannot open rather than reporting a sync that never ran.
func TestSyncDirReportsAnUnopenableDirectory(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("syncDir reported success for a directory that does not exist")
	}
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir on a real directory: %v", err)
	}
}

// TestAtomicWritesSetTheModeBeforeTheSync: a mode changed
// after the fsync is not covered by it, so a crash could leave a write that
// reported success at the temp file's 0600. Both writers must have the final
// mode on the file when it is synced.
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

// TestCreateFileExclusively: the at-rest key is minted on an
// earlier look at its path, so the write must never replace a file that
// appeared since; and no temp file is left behind unreported.
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
			// The temp name is gone before the sync, so the
			// sync covers its removal too.
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

// TestUnitArgvReadsBackWhatInstallUnitWrote: the upgrade's missing-config
// guard reads the --config a restart of the unit will run from the unit file,
// so the reader must give back exactly the argv the writer was handed —
// including the paths escapeSystemdArg quotes or escapes.
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

// TestExecStartArgvRefusesWhatItCannotRead: anything outside the grammar the
// writer uses, or a unit systemd would not run as written, is an error, never
// a guessed argv.
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

// TestUnitArgvRefusesADropInExecStart: systemd runs a drop-in's ExecStart=,
// which UnitArgv does not read, so it says it cannot tell instead of
// answering with the unit file's — from any drop-in directory, however the
// assignment is spaced.
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

// TestUnitDropInDirs pins where systemd looks for the unit's drop-ins on a
// real host: the unit's own directory under /etc, /run and the vendor trees,
// and the directory every service shares beside each.
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

// TestUnitArgvTreatsAMaskAsNoUnit: a masked unit is a symlink to /dev/null,
// which reads as an empty file. systemd never starts it and InstallUnit keeps
// the mask, so it is reported as absent, not as a unit with no ExecStart=.
func TestUnitArgvTreatsAMaskAsNoUnit(t *testing.T) {
	fakeSystemd(t)
	if err := os.Symlink(os.DevNull, version.SystemdUnitPath); err != nil {
		t.Fatal(err)
	}
	if got, err := UnitArgv(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("UnitArgv() of a masked unit = %q, %v; want fs.ErrNotExist", got, err)
	}
}

// TestUnitArgvSkipsAVanishedDropIn: systemd skips a drop-in it cannot open
// because it is gone, and runs the unit's ExecStart=. A dangling drop-in must
// not read as fs.ErrNotExist, which callers take to mean there is no unit.
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

// The tools run as root, so a variable that loads code (XTABLES_LIBDIR,
// TC_LIB_DIR) in the caller's environment must not reach them.
func TestHostCmdEnvIsExplicit(t *testing.T) {
	t.Setenv("XTABLES_LIBDIR", "/evil")
	t.Setenv("TC_LIB_DIR", "/evil")

	got := hostCmd("systemctl", "show").Env
	if want := []string{"PATH=" + toolenv.Path, "LC_ALL=C"}; !slices.Equal(got, want) {
		t.Errorf("Env = %q, want exactly %q", got, want)
	}
}

// A shim of systemctl first on the caller's PATH must not be what runs: the
// tool is looked up in toolPath.
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

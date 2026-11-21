package host

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fakeProcSys(t *testing.T, live map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for key, v := range live {
		p := filepath.Join(dir, strings.ReplaceAll(key, ".", "/"))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := procSys
	procSys = dir
	t.Cleanup(func() { procSys = old })
}

func TestRenderSysctlsRaisesTheInotifyLimits(t *testing.T) {
	fakeProcSys(t, map[string]string{
		"fs.inotify.max_user_instances": "128",
		"fs.inotify.max_user_watches":   "8192",
	})
	got := renderSysctls()
	for _, want := range []string{
		"fs.inotify.max_user_instances=8192\n",
		"fs.inotify.max_user_watches=524288\n",
		"net.ipv4.ip_forward=1\n",
		"net.bridge.bridge-nf-call-iptables=1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered sysctls lack %q:\n%s", strings.TrimSpace(want), got)
		}
	}
}

func TestRenderSysctlsNeverLowersATunedHost(t *testing.T) {
	fakeProcSys(t, map[string]string{
		"fs.inotify.max_user_instances": "65536",
		"fs.inotify.max_user_watches":   "1048576",
	})
	got := renderSysctls()
	for _, want := range []string{"fs.inotify.max_user_instances=65536\n", "fs.inotify.max_user_watches=1048576\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered sysctls lowered a tuned host; want %q in:\n%s", strings.TrimSpace(want), got)
		}
	}
}

func TestRenderSysctlsWritesTheFloorWhenTheLiveValueIsUnreadable(t *testing.T) {
	fakeProcSys(t, map[string]string{"fs.inotify.max_user_watches": "not-a-number"})
	got := renderSysctls()
	for _, want := range []string{"fs.inotify.max_user_instances=8192\n", "fs.inotify.max_user_watches=524288\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", strings.TrimSpace(want), got)
		}
	}
}

func fakeSysctl(t *testing.T, answer func(args []string) (string, error)) *[]string {
	t.Helper()
	var calls []string
	old := runSysctl
	runSysctl = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return answer(args)
	}
	t.Cleanup(func() { runSysctl = old })
	return &calls
}

func TestApplySysctlsFallsBackWhenSystemIsUnsupported(t *testing.T) {
	calls := fakeSysctl(t, func(args []string) (string, error) {
		if args[0] == "--system" {
			return "sysctl: unrecognized option: system\nBusyBox v1.37.0 multi-call binary.", errors.New("exit status 1")
		}
		return "", nil
	})
	if err := applySysctls("/etc/sysctl.d/99-stratum-cis.conf"); err != nil {
		t.Fatal(err)
	}
	want := []string{"--system", "-p /etc/sysctl.d/99-stratum.conf", "-p /etc/sysctl.d/99-stratum-cis.conf"}
	if !slices.Equal(*calls, want) {
		t.Errorf("sysctl calls = %q, want %q", *calls, want)
	}
}

func TestApplySysctlsDoesNotFallBackOnOtherFailures(t *testing.T) {
	calls := fakeSysctl(t, func([]string) (string, error) {
		return "sysctl: cannot stat /proc/sys/net/bridge/bridge-nf-call-iptables: No such file or directory", errors.New("exit status 1")
	})
	err := applySysctls("")
	if err == nil || !strings.Contains(err.Error(), "cannot stat /proc/sys/net/bridge/bridge-nf-call-iptables") {
		t.Errorf("applySysctls = %v, want sysctl's own diagnostic", err)
	}
	if !slices.Equal(*calls, []string{"--system"}) {
		t.Errorf("sysctl calls = %q; a failing key must not load stratum's file on top", *calls)
	}
}

func TestApplySysctlsUsesSystemWhenItWorks(t *testing.T) {
	calls := fakeSysctl(t, func([]string) (string, error) { return "", nil })
	if err := applySysctls(""); err != nil || !slices.Equal(*calls, []string{"--system"}) {
		t.Errorf("applySysctls = %v, calls %q; want --system alone", err, *calls)
	}
}

func TestApplySysctlsReportsAFailedFallback(t *testing.T) {
	fakeSysctl(t, func(args []string) (string, error) {
		if args[0] == "--system" {
			return "sysctl: unrecognized option: system", errors.New("exit status 1")
		}
		return "sysctl: error: 'fs.inotify.max_user_instances' is an unknown key", errors.New("exit status 1")
	})
	err := applySysctls("")
	if err == nil || !strings.Contains(err.Error(), "99-stratum.conf") || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("applySysctls = %v, want the file and sysctl's diagnostic", err)
	}
}

func TestRunSysctlKeepsTheProcessStderr(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'sysctl: permission denied on key fs.inotify.max_user_instances' >&2\nexit 255\n"
	if err := os.WriteFile(filepath.Join(dir, "sysctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	err := applySysctls("")
	if err == nil || !strings.Contains(err.Error(), "permission denied on key fs.inotify.max_user_instances") {
		t.Errorf("applySysctls = %v, want the process's stderr in it", err)
	}
}

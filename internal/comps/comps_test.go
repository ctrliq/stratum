package comps

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/procid"
	"github.com/ctrliq/stratum/internal/version"
)

func TestIsStratumProcess(t *testing.T) {
	operatorArgs := func(exe string) []string {
		return []string{exe, "operator", "--kubeconfig", version.AdminConf, "--manifests-dir", version.ManifestsDir, "--helm-cache", version.HelmCacheDir, "--leader-elect"}
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		unit string
		p    procid.Process
		want bool
	}{
		{"payload containerd", "containerd", procid.Process{Argv: []string{filepath.Join(version.BinDir, "containerd")}}, true},
		{"distro containerd", "containerd", procid.Process{Argv: []string{"/usr/bin/containerd"}}, false},
		{"owned surviving shim", "runtime-shim", procid.Process{Argv: []string{"/usr/bin/containerd-shim-runc-v2", "-namespace", "k8s.io", "-address", strings.TrimPrefix(version.ContainerdSock, "unix://")}}, true},
		{"foreign shim", "runtime-shim", procid.Process{Argv: []string{"/usr/bin/containerd-shim-runc-v2", "-address", "/run/containerd/containerd.sock"}}, false},
		{"payload kubelet", "kubelet", procid.Process{Argv: []string{filepath.Join(version.BinDir, "kubelet")}}, true},
		{"operator subcommand", "operator", procid.Process{Argv: operatorArgs("/usr/local/bin/stratum")}, true},
		{"release artifact operator", "operator", procid.Process{Argv: operatorArgs("/tmp/stratum-linux-amd64")}, true},
		{"unrelated stratum", "operator", procid.Process{Argv: []string{"/usr/local/bin/stratum", "up"}}, false},
		{"unrelated operator", "operator", procid.Process{Argv: []string{"/usr/local/bin/other", "operator"}}, false},
		{"top-level up, same exe as this process", "stratum", procid.Process{Argv: []string{"/usr/local/bin/stratum", "up"}, Exe: self}, true},
		{"top-level worker up", "stratum", procid.Process{Argv: []string{"/usr/local/bin/stratum", "up", "--worker"}, Exe: self}, true},
		{"top-level up, exe deleted by an upgrade but still the same file", "stratum", procid.Process{Argv: []string{self, "up"}, Exe: self + " (deleted)"}, true},
		{"top-level uninstall is not up (this call's own subcommand)", "stratum", procid.Process{Argv: []string{self, "uninstall", "--yes"}, Exe: self}, false},
		{"top-level with no subcommand", "stratum", procid.Process{Argv: []string{self}, Exe: self}, false},
		{"non-stratum other up process", "stratum", procid.Process{Argv: []string{"/usr/local/bin/other", "up"}, Exe: "/usr/local/bin/other"}, false},
		{"top-level up but exe unreadable", "stratum", procid.Process{Argv: []string{"/usr/local/bin/stratum", "up"}}, false},
		{"legacy unit name resolves to the component", "stratum-kubelet", procid.Process{Argv: []string{filepath.Join(version.BinDir, "kubelet")}}, true},
		{"unknown unit matches nothing", "conmon", procid.Process{Argv: []string{"/usr/bin/conmon"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStratumProcess(tc.unit, tc.p); got != tc.want {
				t.Errorf("isStratumProcess(%q, %+v) = %v, want %v", tc.unit, tc.p, got, tc.want)
			}
		})
	}
}

func TestEveryUnitHasAPredicate(t *testing.T) {
	if len(processPredicates) == 0 {
		t.Fatal("no process predicates are registered")
	}
	for _, unit := range Units {
		if _, ok := processPredicates[Normalize(unit)]; !ok {
			t.Errorf("roster unit %q has no process predicate — isStratumProcess would answer false for every process and uninstall would leave it running once the supervisor has died", unit)
		}
	}
}

func TestEveryUnitHasAnIdentityRule(t *testing.T) {
	table := map[string]struct {
		accept []string
		refuse []string
	}{
		"containerd": {
			accept: []string{filepath.Join(version.BinDir, "containerd")},
			refuse: []string{"/usr/bin/containerd"},
		},
		"kubelet": {
			accept: []string{filepath.Join(version.BinDir, "kubelet")},
			refuse: []string{"/usr/bin/kubelet"},
		},
		"operator": {
			accept: []string{"/usr/local/bin/stratum", "operator", "--kubeconfig", version.AdminConf,
				"--manifests-dir", version.ManifestsDir, "--helm-cache", version.HelmCacheDir, "--leader-elect"},
			refuse: []string{"/usr/local/bin/other", "operator"},
		},
	}
	if len(table) != len(Units) {
		t.Errorf("identity table has %d entries, want %d", len(table), len(Units))
	}
	for _, unit := range Units {
		row, ok := table[unit]
		if !ok {
			t.Errorf("unit %q has no identity rule — uninstall would not recognise it once the supervisor has died", unit)
			continue
		}
		if !isStratumProcess(unit, procid.Process{Argv: row.accept}) {
			t.Errorf("isStratumProcess(%q, accept) = false; uninstall would leave the live process running", unit)
		}
		if isStratumProcess(unit, procid.Process{Argv: row.refuse}) {
			t.Errorf("isStratumProcess(%q, refuse) = true; uninstall would signal a process that is not Stratum's", unit)
		}
	}
}

func TestOperatorPatternAgrees(t *testing.T) {
	re := regexp.MustCompile(OperatorCommandLinePattern())
	canonical := func(exe string) []string {
		return []string{exe, "operator", "--kubeconfig", version.AdminConf, "--manifests-dir", version.ManifestsDir,
			"--helm-cache", version.HelmCacheDir, "--leader-elect", "--node-ip", "10.0.0.11", "--apiserver-vip", "10.0.0.10"}
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"canonical name", canonical("/usr/local/bin/stratum")},
		{"release artifact", canonical("/tmp/stratum-linux-amd64")},
		{"bare name", canonical("stratum")},
		{"no trailing flags", canonical("/usr/local/bin/stratum")[:9]},
		{"another subcommand", append([]string{"/usr/local/bin/stratum", "up"}, canonical("x")[2:]...)},
		{"another program's operator", []string{"/usr/local/bin/other", "operator"}},
		{"missing leader election", slices.DeleteFunc(canonical("/usr/local/bin/stratum"), func(a string) bool { return a == "--leader-elect" })},
		{"another kubeconfig", []string{"/usr/local/bin/stratum", "operator", "--kubeconfig", "/etc/other.conf", "--manifests-dir", version.ManifestsDir, "--helm-cache", version.HelmCacheDir, "--leader-elect"}},
		{"another manifests dir", []string{"/usr/local/bin/stratum", "operator", "--kubeconfig", version.AdminConf, "--manifests-dir", "/tmp/m", "--helm-cache", version.HelmCacheDir, "--leader-elect"}},
		{"the pkill command itself", []string{"sudo", "pkill", "-TERM", "-f", OperatorCommandLinePattern()}},
		{"a shell running it", []string{"sh", "-c", "stratum operator --kubeconfig " + version.AdminConf}},
		{"a longer flag", []string{"/usr/local/bin/stratum", "operator", "--kubeconfig", version.AdminConf + ".bak", "--manifests-dir", version.ManifestsDir, "--helm-cache", version.HelmCacheDir, "--leader-elect"}},
	} {
		want := procid.IsOperator(procid.Process{Argv: tc.args})
		if got := re.MatchString(strings.Join(tc.args, " ")); got != want {
			t.Errorf("%s: pattern matches = %v, but the operator predicate = %v for %q", tc.name, got, want, tc.args)
		}
	}
	// The cases above are only a check if both sides say yes to something.
	if !procid.IsOperator(procid.Process{Argv: canonical("/tmp/stratum-linux-amd64")}) || !re.MatchString(strings.Join(canonical("/tmp/stratum-linux-amd64"), " ")) {
		t.Error("an artifact-named operator is not matched")
	}
}

func TestUninstallStratumPredicateAgreesWithUpDetection(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
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
			p := procid.Process{Argv: tc.args, Exe: self}
			got := isStratumProcess("stratum", p)
			if want := procid.IsStratumTopLevel(p); got != want {
				t.Errorf("isStratumProcess(%q) = %v, but procid.IsStratumTopLevel = %v — uninstall's stratum entry must be the shared predicate, not a copy", tc.args, got, want)
			}
			if want := procid.IsUpCommand(tc.args); got != want {
				t.Errorf("isStratumProcess(%q) = %v, but IsUpCommand = %v — the stratum-up rules must not drift on up detection", tc.args, got, want)
			}
		})
	}
}

func seedProc(t *testing.T, root, pid string, args ...string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProcessPIDsWalksTheFixtureRoot(t *testing.T) {
	root := t.TempDir()
	seedProc(t, root, "100", filepath.Join(version.BinDir, "containerd"))
	seedProc(t, root, "200", "/usr/bin/containerd") // a distro-owned same-named binary
	seedProc(t, root, "300", filepath.Join(version.BinDir, "containerd"))

	pids, err := processPIDs(root, "containerd", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pids, []int{100, 300}) {
		t.Fatalf("processPIDs = %v, want only the owned containerd processes [100 300]", pids)
	}

	pids, err = processPIDs(root, "containerd", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pids, []int{100}) {
		t.Fatalf("processPIDs with max = %v, want the scan to stop after the first match", pids)
	}

	pids, err = processPIDs(root, "kubelet", 0)
	if err != nil {
		t.Fatalf("a component with no live process: %v", err)
	}
	if len(pids) != 0 {
		t.Fatalf("processPIDs(kubelet) = %v, want none", pids)
	}
}

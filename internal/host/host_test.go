// Package host_test is external so it can import internal/uninstall, which imports host.
package host_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/uninstall"
	"github.com/ctrliq/stratum/internal/version"
)

// Paths version declares are named via variables; the /etc and /var ones are literals.
var footprintWant = []string{
	"/etc/modules-load.d/k8s.conf",
	"/etc/sysctl.d/99-stratum.conf",
	"/etc/sysctl.d/99-stratum-cis.conf",
	"/etc/cni/net.d",
	"/var/lib/kubelet",
	version.AuditLogDir,
	version.SystemdUnitPath,
	version.CertsDir,
	version.ContainerdConfD,
}

func coveredBy(path string, targets []string) bool {
	for _, target := range targets {
		if path == target || strings.HasPrefix(path, strings.TrimSuffix(target, "/")+"/") {
			return true
		}
	}
	return false
}

func TestFootprintDeclaresBringUpsPathsOutsideTheRoots(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	got := host.Footprint()
	want := slices.Clone(footprintWant)
	have := slices.Clone(got)
	slices.Sort(want)
	slices.Sort(have)
	if !slices.Equal(have, want) {
		t.Errorf("host.Footprint() = %q, want exactly %q: a missing entry leaves a bring-up path undeclared and an extra one makes it an uninstall delete target", have, want)
	}
	for _, p := range got {
		for _, root := range []string{version.DataDir, version.ConfDir, version.RunDir} {
			if p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/") {
				t.Errorf("host.Footprint() includes %q, which is inside the %s root the footprint is meant to sit outside; the roots are removed wholesale already", p, root)
			}
		}
		if p == "/opt/local-path-provisioner" {
			t.Errorf("host.Footprint() includes /opt/local-path-provisioner; SELinux only relabels it, stratum does not write it, so it is not part of the footprint")
		}
	}
}

// The kubelet dir is excused: removeLegacyKubeletDir keeps non-stratum state.
func TestUninstallTargetsCoverTheFootprint(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	targets := uninstall.Targets()
	kept := map[string]string{
		host.KubeletDir: "not a delete target: removeLegacyKubeletDir is the directory's only remover, and it keeps whatever is not Stratum's kubelet's",
	}
	for _, p := range host.Footprint() {
		if coveredBy(p, targets) {
			continue
		}
		if _, excused := kept[p]; excused {
			continue
		}
		t.Errorf("no uninstall target covers host.Footprint() path %q; uninstall would leave it on a node it wipes — name it in dirsAndFiles or excuse it in this test's kept set", p)
	}
}

// A source pin: no external test can stand in for Prereqs's whole host stack.
func TestPrereqsWritesTheBootCriticalFilesThroughTheAtomicWriter(t *testing.T) {
	src, err := os.ReadFile("host.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "os.WriteFile(") {
		t.Error("host.go still writes a node file raw; the modules-load and sysctl writes are boot-critical and must go through the shared atomic writer, not os.WriteFile")
	}
	for _, spelling := range []string{"/etc/modules-load.d/", "/etc/sysctl.d/"} {
		if strings.Contains(string(src), spelling) {
			t.Errorf("host.go spells a sysctl or modules-load path (%s...) itself; the one home of the three paths is beside version.SystemdUnitPath, and host reads them through the version variables", spelling)
		}
	}
	i := strings.Index(string(src), "func Prereqs(")
	if i < 0 {
		t.Fatal("host.go has no Prereqs: the boot-critical writes live there")
	}
	body := string(src)[i:]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}
	for _, want := range []string{"version.ModulesLoadFile", "version.SysctlConf", "version.SysctlCISConf"} {
		if !strings.Contains(body, want) {
			t.Errorf("Prereqs must name %s: the modules-load and sysctl writes must stay in Prereqs, named through the version path variables", want)
		}
	}
}

// sysctl --system sets the AppArmor restriction sysctl that decides runc's profile.
func TestPrereqsLoadsTheAppArmorProfileAfterTheSysctls(t *testing.T) {
	src, err := os.ReadFile("host.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)[strings.Index(string(src), "func Prereqs("):]
	body = body[:strings.Index(body, "\n}\n")]
	sysctls, apparmor := strings.Index(body, "applySysctls("), strings.Index(body, "apparmor.Ensure(")
	if sysctls < 0 || apparmor < 0 || apparmor < sysctls {
		t.Errorf("Prereqs calls applySysctls at %d and apparmor.Ensure at %d; want both, the AppArmor profile after the sysctls", sysctls, apparmor)
	}
}

func TestFootprintAndUninstallCoverEveryProfilesSysctlDropIn(t *testing.T) {
	prev := profile.All()
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
	if err := profile.SetRegistered([]profile.Profile{
		{Name: "cis", Sysctls: []string{"kernel.panic=10"}},
		{Name: "edge", Sysctls: []string{"vm.swappiness=10"}},
		{Name: "bare"},
	}); err != nil {
		t.Fatal(err)
	}
	edge := "/etc/sysctl.d/99-stratum-edge.conf"
	if got := host.ProfileSysctlFiles(); !slices.Equal(got, []string{edge}) {
		t.Errorf("ProfileSysctlFiles = %q, want only %q: cis's is SysctlCISConf, and a profile without sysctls writes none", got, edge)
	}
	if !slices.Contains(host.Footprint(), edge) {
		t.Errorf("host.Footprint() omits %s, which Prereqs writes on an edge node", edge)
	}
	if !coveredBy(edge, uninstall.Targets()) {
		t.Errorf("no uninstall target covers %s; uninstall would leave the edge profile's kernel settings applied on every later boot", edge)
	}
}

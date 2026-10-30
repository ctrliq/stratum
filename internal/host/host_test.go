// Package host_test pins host.Footprint: the one declaration of the host
// paths bring-up writes outside stratum's three roots (version.DataDir,
// version.ConfDir, version.RunDir), and its coupling to uninstall — every
// path the footprint declares, uninstall must remove. It is an external test
// package so it can import internal/uninstall, which itself imports host; an
// in-package test file would be an import cycle.
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

// footprintWant is the complete set of paths bring-up writes outside the
// three roots, and so the complete set host.Footprint() may declare: the
// modules-load file, both sysctl files (99-stratum.conf always,
// 99-stratum-cis.conf under --cis), the CNI dir, the kubelet dir, the audit
// log dir, the systemd unit, and containerd's certs.d and its registry and
// runtime-handler conf.d files. The paths version already declares are named through their variables,
// so a path's two mentions cannot drift apart; the /etc and /var drop-ins
// have no version variable and are pinned as literals.
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
	version.ContainerdRuntimeConfD,
}

// coveredBy reports whether path is target itself or lies under a target
// that is a directory root — the same covering rule
// TestTargetsCoverTheEnvironmentFile applies.
func coveredBy(path string, targets []string) bool {
	for _, target := range targets {
		if path == target || strings.HasPrefix(path, strings.TrimSuffix(target, "/")+"/") {
			return true
		}
	}
	return false
}

// TestFootprintDeclaresBringUpsPathsOutsideTheRoots pins the footprint to
// exactly the paths bring-up writes outside the three roots. An entry missing
// here leaves a file undeclared, and uninstall — which derives from the
// footprint — leaves it on the node; an extra entry here becomes an
// uninstall delete target for a path bring-up does not own, so the set is
// exact, each path once. /opt/local-path-provisioner stays out on purpose:
// SELinux only relabels that dir (spec.storage.localPathProvisioner), stratum
// never writes it, so it is not part of the footprint and must not become a
// delete target.
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

// TestUninstallTargetsCoverTheFootprint is the coupling that keeps uninstall
// and bring-up honest: every path host.Footprint() declares must be covered by
// uninstall, the same way the roots are — as a delete target, or, for the one
// exception, by the teardown's guarded removal. When a footprint path is
// missing here, a node keeps a file bring-up wrote after an uninstall that
// claims to wipe it — the gap this closes for the CIS sysctl file, which
// host.Prereqs writes under --cis and no target covered. The kubelet dir is
// deliberately not a delete target: removeLegacyKubeletDir is its only
// remover, keeping whatever is not Stratum's kubelet's (its stale sockets and
// stratum-CA-signed cert pairs go), so a co-hosted kubelet's state survives
// the wipe; the same decision lives in TestTargetsOutsideEveryRootAreListed's
// footprintKept set.
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

// TestPrereqsWritesTheBootCriticalFilesThroughTheAtomicWriter pins the
// conversion of the three boot-critical writes in Prereqs — the modules-load
// file and both sysctl files, whose torn state breaks pod networking or the
// kubelet's protectKernelDefaults boot gate after a crash. A source pin,
// because Prereqs runs its whole host stack (the cgroup-v2 floor check, the
// SELinux mode probe, a real sysctl --system) before the writes, which no
// external test can stand in for: it reads host.go the way
// TestCertRenewJitterAddsNoLockOrLease reads run.go. Three properties:
//
//   - no raw write call in host.go: the shared atomic writer (the
//     temp-file-then-rename with the file and directory fsyncs) is how the
//     boot-critical files are written, so a crash can never leave a torn
//     sysctl file where a working one was;
//   - no second spelling of the three paths: they are declared once, beside
//     version.SystemdUnitPath, and every mention here names the version
//     variable — the writer, the sysctl loader and uninstall's delete list
//     cannot drift apart;
//   - Prereqs still writes all three: the write sites name the paths through
//     the version variables, so a conversion that dropped a write fails here.
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

package host

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func interfaceRecord(t *testing.T) []trustedInterface {
	t.Helper()
	r, err := readFirewalldRecord()
	if err != nil {
		t.Fatal(err)
	}
	var podInterfaces []trustedInterface
	for _, iface := range r.TrustedInterfaces {
		if iface.Interface == ciliumPodInterface {
			podInterfaces = append(podInterfaces, iface)
		}
	}
	return podInterfaces
}

func TestTrustPodCIDRsScopesTrustToCiliumIngress(t *testing.T) {
	f := newFakeFirewalld(t)
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16,fd00:10:245::/56") })
	if f.ifaceRuntime[ciliumPodInterface] != firewalldZone || f.ifacePerm[ciliumPodInterface] != firewalldZone {
		t.Fatalf("Cilium interface not trusted in both scopes: runtime=%v permanent=%v", f.ifaceRuntime, f.ifacePerm)
	}
	if f.ifaceRuntime[lxcBridgeInterface] != "public" || f.ifacePerm[lxcBridgeInterface] != "public" {
		t.Fatalf("LXC bridge was not excluded in both scopes: runtime=%v permanent=%v", f.ifaceRuntime, f.ifacePerm)
	}
	if len(f.runtime) != 0 || len(f.perm) != 0 {
		t.Fatalf("trusted an address range on every ingress interface: runtime=%v permanent=%v", f.runtime, f.perm)
	}
	if got := interfaceRecord(t); !slices.Equal(got, []trustedInterface{{Interface: ciliumPodInterface, Zone: firewalldZone, Runtime: true, Permanent: true}}) {
		t.Errorf("ownership = %+v", got)
	}
	if got, err := OwnedFirewalldInterfaces(); err != nil || !slices.Equal(got, []string{"lxcbr+ (zone public)", "lxc+ (zone trusted)"}) {
		t.Errorf("preview = %v, %v", got, err)
	}
	if f.reloads != 0 {
		t.Errorf("reloaded firewalld %d times", f.reloads)
	}
}

func TestTrustPodCIDRsFailsClosedWithoutBridgeExclusion(t *testing.T) {
	f := newFakeFirewalld(t)
	f.defaultZone = firewalldZone
	out := quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if len(f.ifaceRuntime) != 0 || len(f.ifacePerm) != 0 || !strings.Contains(out, "did not add pod trust") {
		t.Errorf("installed trust without a safe bridge zone: runtime=%v permanent=%v output=%s", f.ifaceRuntime, f.ifacePerm, out)
	}
}

func TestTrustPodCIDRsRemovesOldWildcardIfBridgeExclusionFails(t *testing.T) {
	f := newFakeFirewalld(t)
	f.ifaceRuntime[ciliumPodInterface], f.ifacePerm[ciliumPodInterface] = firewalldZone, firewalldZone
	if err := writeFirewalldRecord(firewalldRecord{TrustedInterfaces: []trustedInterface{{Interface: ciliumPodInterface, Zone: firewalldZone, Runtime: true, Permanent: true}}}); err != nil {
		t.Fatal(err)
	}
	f.failAdd["runtime"] = true
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if _, ok := f.ifaceRuntime[ciliumPodInterface]; ok {
		t.Fatal("left old runtime wildcard trusted after bridge exclusion failed")
	}
	if _, ok := f.ifacePerm[ciliumPodInterface]; ok {
		t.Fatal("left old permanent wildcard trusted after bridge exclusion failed")
	}
}

func TestTrustPodCIDRsWarnsWhenOldWildcardCannotBeRemoved(t *testing.T) {
	f := newFakeFirewalld(t)
	f.ifaceRuntime[ciliumPodInterface] = firewalldZone
	if err := writeFirewalldRecord(firewalldRecord{TrustedInterfaces: []trustedInterface{{Interface: ciliumPodInterface, Zone: firewalldZone, Runtime: true}}}); err != nil {
		t.Fatal(err)
	}
	f.failAdd["runtime"], f.failRemove["runtime"] = true, true
	out := quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if !strings.Contains(out, "old lxc+ binding may still be trusted; remove with: firewall-cmd --zone=trusted --remove-interface=lxc+") {
		t.Errorf("missing explicit warning about retained broad binding: %s", out)
	}
	if got := interfaceRecord(t); len(got) != 1 || !got[0].Runtime {
		t.Errorf("lost ownership after failed wildcard removal: %+v", got)
	}
}

func TestTrustPodCIDRsRemovesOldWildcardOfflineBeforeDaemonRestart(t *testing.T) {
	f := newFakeFirewalld(t)
	fakeIface(t, "lxcbr0", "bridge") // appeared after the wildcard was recorded
	f.ifacePerm[ciliumPodInterface] = firewalldZone
	if err := writeFirewalldRecord(firewalldRecord{TrustedInterfaces: []trustedInterface{{Interface: ciliumPodInterface, Zone: firewalldZone, Permanent: true}}}); err != nil {
		t.Fatal(err)
	}
	f.running = false
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if _, ok := f.ifacePerm[ciliumPodInterface]; ok || len(interfaceRecord(t)) != 0 {
		t.Errorf("old wildcard survived offline cleanup: permanent=%v record=%+v", f.ifacePerm, interfaceRecord(t))
	}
}

func TestTrustPodCIDRsRetainsOldWildcardOwnershipWhenOfflineRemovalFails(t *testing.T) {
	f := newFakeFirewalld(t)
	fakeIface(t, "lxcbr0", "bridge")
	f.ifacePerm[ciliumPodInterface] = firewalldZone
	if err := writeFirewalldRecord(firewalldRecord{TrustedInterfaces: []trustedInterface{{Interface: ciliumPodInterface, Zone: firewalldZone, Permanent: true}}}); err != nil {
		t.Fatal(err)
	}
	f.running, f.offlineMissing = false, true
	out := quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if f.ifacePerm[ciliumPodInterface] != firewalldZone {
		t.Errorf("fake permanent rule disappeared despite missing offline tool: %v", f.ifacePerm)
	}
	if got := interfaceRecord(t); len(got) != 1 || !got[0].Permanent {
		t.Errorf("lost retry ownership after failed offline removal: %+v", got)
	}
	if !strings.Contains(out, "firewall-offline-cmd --zone=trusted --remove-interface=lxc+") {
		t.Errorf("missing offline cleanup command: %s", out)
	}
}

func TestTrustPodCIDRsPreservesOperatorBridgeZone(t *testing.T) {
	f := newFakeFirewalld(t)
	f.ifaceRuntime[lxcBridgeInterface], f.ifacePerm[lxcBridgeInterface] = "internal", "internal"
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if f.ifaceRuntime[lxcBridgeInterface] != "internal" || f.ifacePerm[lxcBridgeInterface] != "internal" {
		t.Errorf("changed operator bridge zone: runtime=%v permanent=%v", f.ifaceRuntime, f.ifacePerm)
	}
	if _, err := untrust(t); err != nil {
		t.Fatal(err)
	}
	if f.ifaceRuntime[lxcBridgeInterface] != "internal" || f.ifacePerm[lxcBridgeInterface] != "internal" {
		t.Errorf("removed operator bridge zone: runtime=%v permanent=%v", f.ifaceRuntime, f.ifacePerm)
	}
}

func TestTrustPodCIDRsMigratesOwnedSourceToInterface(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if len(f.runtime) != 0 || len(f.perm) != 0 || len(record(t)) != 0 {
		t.Errorf("old source binding survived migration: runtime=%v permanent=%v record=%+v", f.runtime, f.perm, record(t))
	}
	if f.ifaceRuntime[ciliumPodInterface] != firewalldZone || f.ifacePerm[ciliumPodInterface] != firewalldZone {
		t.Errorf("interface binding missing after migration: runtime=%v permanent=%v", f.ifaceRuntime, f.ifacePerm)
	}
}

func TestTrustPodCIDRsKeepsFailedLegacyRemovalRecorded(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	f.failRemove["runtime"] = true
	out := quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if !strings.Contains(out, "remove it with") || f.runtime["10.245.0.0/16"] != firewalldZone {
		t.Errorf("failed legacy removal was hidden: %s", out)
	}
	if got := record(t); !slices.Equal(got, []trustedSource{{Source: "10.245.0.0/16", Zone: firewalldZone, Runtime: true}}) {
		t.Errorf("lost retry ownership: %+v", got)
	}
}

func TestTrustPodCIDRsDoesNotClaimOperatorsInterface(t *testing.T) {
	f := newFakeFirewalld(t)
	f.ifaceRuntime[ciliumPodInterface], f.ifacePerm[ciliumPodInterface] = firewalldZone, firewalldZone
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if len(interfaceRecord(t)) != 0 {
		t.Fatal("claimed an operator-owned interface")
	}
	if _, err := untrust(t); err != nil {
		t.Fatal(err)
	}
	if f.ifaceRuntime[ciliumPodInterface] != firewalldZone || f.ifacePerm[ciliumPodInterface] != firewalldZone {
		t.Fatal("removed an operator-owned interface")
	}
}

func TestTrustPodCIDRsRecordsOnlyScopeItAdded(t *testing.T) {
	f := newFakeFirewalld(t)
	f.ifaceRuntime[ciliumPodInterface] = firewalldZone
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if got := interfaceRecord(t); !slices.Equal(got, []trustedInterface{{Interface: ciliumPodInterface, Zone: firewalldZone, Permanent: true}}) {
		t.Errorf("ownership = %+v", got)
	}
	if _, err := untrust(t); err != nil {
		t.Fatal(err)
	}
	if f.ifaceRuntime[ciliumPodInterface] != firewalldZone || len(f.ifacePerm) != 0 {
		t.Errorf("wrong cleanup: runtime=%v permanent=%v", f.ifaceRuntime, f.ifacePerm)
	}
}

func TestTrustPodCIDRsLeavesAnotherInterfaceZoneAlone(t *testing.T) {
	f := newFakeFirewalld(t)
	f.ifacePerm[ciliumPodInterface] = "internal"
	out := quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if _, bound := f.ifaceRuntime[ciliumPodInterface]; bound || f.ifacePerm[ciliumPodInterface] != "internal" || len(interfaceRecord(t)) != 0 {
		t.Errorf("overrode operator zone: runtime=%v permanent=%v", f.ifaceRuntime, f.ifacePerm)
	}
	if !strings.Contains(out, "zone \"internal\"") {
		t.Errorf("missing conflict warning: %s", out)
	}
}

func TestTrustPodCIDRsRollsBackUnrecordedInterface(t *testing.T) {
	f := newFakeFirewalld(t)
	writeFirewalldRecord = func(firewalldRecord) error { return errors.New("write failed") }
	out := quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if len(f.ifaceRuntime) != 0 || len(f.ifacePerm) != 0 || !strings.Contains(out, "rollback") {
		t.Errorf("unrecorded binding remained: runtime=%v permanent=%v output=%s", f.ifaceRuntime, f.ifacePerm, out)
	}
}

func TestTrustPodCIDRsIsIdempotentForInterface(t *testing.T) {
	f := newFakeFirewalld(t)
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	f.calls = nil
	out := quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if out != "" || len(interfaceRecord(t)) != 1 {
		t.Errorf("second start changed interface: output=%s record=%+v", out, interfaceRecord(t))
	}
	for _, call := range f.calls {
		if strings.Contains(call, "--add-interface") {
			t.Errorf("second start added the binding again: %s", call)
		}
	}
}

func TestUntrustPodCIDRsRemovesOwnedInterface(t *testing.T) {
	f := newFakeFirewalld(t)
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if _, err := untrust(t); err != nil {
		t.Fatal(err)
	}
	if len(f.ifaceRuntime) != 0 || len(f.ifacePerm) != 0 || len(interfaceRecord(t)) != 0 {
		t.Errorf("interface survived cleanup: runtime=%v permanent=%v record=%+v", f.ifaceRuntime, f.ifacePerm, interfaceRecord(t))
	}
}

func TestUntrustPodCIDRsRetainsPermanentInterfaceWithoutOfflineTool(t *testing.T) {
	f := newFakeFirewalld(t)
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	f.running, f.offlineMissing = false, true
	if _, err := untrust(t); err == nil || !strings.Contains(err.Error(), "firewall-offline-cmd") {
		t.Errorf("untrust = %v, want offline cleanup failure", err)
	}
	if got := interfaceRecord(t); !slices.Equal(got, []trustedInterface{{Interface: ciliumPodInterface, Zone: firewalldZone, Permanent: true}}) {
		t.Errorf("ownership after failed offline cleanup = %+v", got)
	}
}

func TestUntrustPodCIDRsRetriesFailedInterfaceCleanup(t *testing.T) {
	for _, failure := range []string{"query", "remove"} {
		t.Run(failure, func(t *testing.T) {
			f := newFakeFirewalld(t)
			quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
			if failure == "query" {
				f.failQuery = true
			} else {
				f.failRemove["runtime"] = true
			}
			if _, err := untrust(t); err == nil || !strings.Contains(err.Error(), "--remove-interface=lxc+") {
				t.Fatalf("untrust = %v, want removal guidance", err)
			}
			want := trustedInterface{Interface: ciliumPodInterface, Zone: firewalldZone, Runtime: true}
			if failure == "query" {
				want.Permanent = true
			}
			if got := interfaceRecord(t); !slices.Equal(got, []trustedInterface{want}) {
				t.Fatalf("lost ownership needed for retry: %+v", got)
			}
			if f.ifaceRuntime[lxcBridgeInterface] != "public" || f.ifacePerm[lxcBridgeInterface] != "public" {
				t.Fatalf("removed bridge exclusion while wildcard cleanup failed: runtime=%v permanent=%v", f.ifaceRuntime, f.ifacePerm)
			}
			f.failQuery, f.failRemove["runtime"] = false, false
			if _, err := untrust(t); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if len(f.ifaceRuntime) != 0 || len(interfaceRecord(t)) != 0 {
				t.Errorf("retry left binding or ownership: runtime=%v record=%+v", f.ifaceRuntime, interfaceRecord(t))
			}
		})
	}
}

func TestNoFirewalldAndNoOwnershipNeedsNoCleanup(t *testing.T) {
	f := newFakeFirewalld(t)
	f.cmdMissing = true
	if out := quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") }); out != "" {
		t.Errorf("warned on a host without firewalld or owned bindings: %s", out)
	}
	if _, err := untrust(t); err != nil {
		t.Errorf("uninstall on a host without firewalld or bindings: %v", err)
	}
}

func TestMissingFirewallCmdRetainsExistingOwnership(t *testing.T) {
	f := newFakeFirewalld(t)
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	f.cmdMissing = true
	if _, err := untrust(t); err == nil || !strings.Contains(err.Error(), "--state") {
		t.Errorf("untrust = %v, want failed state check", err)
	}
	if got := interfaceRecord(t); !slices.Equal(got, []trustedInterface{{Interface: ciliumPodInterface, Zone: firewalldZone, Runtime: true, Permanent: true}}) {
		t.Errorf("ownership changed while firewalld state was unknown: %+v", got)
	}
}

// fakeIface adds an interface to the faked /sys/class/net: a veth has no
// marker, a bridge a bridge/ directory, a physical NIC a device link.
func fakeIface(t *testing.T, name, marker string) {
	t.Helper()
	dir := filepath.Join(sysClassNet, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	switch marker {
	case "bridge":
		if err := os.Mkdir(filepath.Join(dir, "bridge"), 0o755); err != nil {
			t.Fatal(err)
		}
	case "device":
		if err := os.Symlink("../../devices/pci0000:00/0000:00:03.0", filepath.Join(dir, "device")); err != nil {
			t.Fatal(err)
		}
	}
}

// Cilium's own veths, lxc_health included, are what lxc+ is for.
func TestTrustPodCIDRsBindsWhenOnlyCiliumVethsMatch(t *testing.T) {
	f := newFakeFirewalld(t)
	fakeIface(t, "lxc_health", "")
	fakeIface(t, "lxc1a2b3c4d5e6f", "")
	fakeIface(t, "eth0", "device")
	quiet(t, func() { TrustPodCIDRs("") })
	if f.ifaceRuntime[ciliumPodInterface] != firewalldZone || f.ifacePerm[ciliumPodInterface] != firewalldZone {
		t.Errorf("Cilium interfaces not trusted: runtime=%v permanent=%v", f.ifaceRuntime, f.ifacePerm)
	}
}

// The standard LXC bridge is kept outside trusted, even if created after
// pod trust, while Cilium's veths remain trusted.
func TestTrustPodCIDRsKeepsLxcBridgeInDefaultZone(t *testing.T) {
	f := newFakeFirewalld(t)
	fakeIface(t, "lxc1a2b3c4d5e6f", "")
	quiet(t, func() { TrustPodCIDRs("") })
	fakeIface(t, "lxcbr0", "bridge")
	quiet(t, func() { TrustPodCIDRs("") })
	if f.ifaceRuntime[ciliumPodInterface] != firewalldZone || f.ifaceRuntime[lxcBridgeInterface] != "public" {
		t.Errorf("pod or bridge zone wrong: runtime=%v", f.ifaceRuntime)
	}
}

// An unusual lxc-prefixed physical NIC has no reserved exclusion pattern.
// The next start revokes an older owned wildcard and warns.
func TestTrustPodCIDRsRevokesWildcardForOtherLxcLookalike(t *testing.T) {
	f := newFakeFirewalld(t)
	fakeIface(t, "lxc1a2b3c4d5e6f", "")
	quiet(t, func() { TrustPodCIDRs("") })
	fakeIface(t, "lxcnic0", "device")
	out := quiet(t, func() { TrustPodCIDRs("") })
	if _, ok := f.ifaceRuntime[ciliumPodInterface]; ok {
		t.Error("lxc+ remained trusted alongside lxcnic0")
	}
	if _, ok := f.ifacePerm[ciliumPodInterface]; ok {
		t.Error("permanent lxc+ remained trusted alongside lxcnic0")
	}
	if got := interfaceRecord(t); len(got) != 0 {
		t.Errorf("wildcard ownership remained after removal: %+v", got)
	}
	if !strings.Contains(out, "lxcnic0") || !strings.Contains(out, "--add-interface=lxc") {
		t.Errorf("warning lacks reason or manual fix: %q", out)
	}
}

func TestTrustPodCIDRsFailsClosedWithoutSysClassNet(t *testing.T) {
	f := newFakeFirewalld(t)
	sysClassNet = filepath.Join(t.TempDir(), "missing")
	out := quiet(t, func() { TrustPodCIDRs("") })
	if len(f.ifaceRuntime) != 0 || len(f.ifacePerm) != 0 {
		t.Errorf("trusted pod interfaces without checking sysfs: runtime=%v permanent=%v", f.ifaceRuntime, f.ifacePerm)
	}
	if !strings.Contains(out, "cannot list interfaces") {
		t.Errorf("missing sysfs warning not shown: %s", out)
	}
}

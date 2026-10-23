package host

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Cilium names the host end of each pod veth lxcXXXXXX. firewalld accepts a
// trailing + as an interface-prefix wildcard on both nftables and iptables.
// Unlike a pod-CIDR source binding, this cannot admit traffic from a physical
// NIC merely because its source address was spoofed. The more-specific LXC
// bridge prefix must be pinned outside trusted before this binding is added.
const (
	ciliumPodInterface = "lxc+"
	lxcBridgeInterface = "lxcbr+"
)

// A stopped daemon can still have the old broad wildcard in permanent
// configuration. Remove it before a later daemon start can activate it
// without the LXC bridge exclusion in place.
func removeOwnedCiliumWildcardOffline() {
	r, err := readFirewalldRecord()
	if err != nil {
		fmt.Printf("  WARN: firewalld: cannot read interface ownership record: %v\n", err)
		return
	}
	owned := firewalldRecord{}
	for _, t := range r.TrustedInterfaces {
		if t.Interface == ciliumPodInterface && t.Zone == firewalldZone {
			owned.TrustedInterfaces = append(owned.TrustedInterfaces, t)
		}
	}
	var fixes, recordFailures []string
	untrustCiliumInterfaces(os.Stdout, owned, false, &fixes, &recordFailures)
	if len(fixes) > 0 || len(recordFailures) > 0 {
		fmt.Printf("  WARN: firewalld: could not remove owned pod wildcard offline: %s %s\n", strings.Join(fixes, "; "), strings.Join(recordFailures, "; "))
	}
}

// sysClassNet lists the host's interfaces; a var so a test can fake it.
var sysClassNet = "/sys/class/net"

// lxcLookalikes are the interfaces lxc+ would match that are not Cilium's.
// Cilium's lxcXXXXXX and lxc_health are veths. LXC's own lxcbr0 is a bridge,
// and binding lxc+ would trust every container behind it.
// A bridge has a bridge/ directory and a physical NIC a device link; a veth
// has neither. ifindex against iflink is no test: a veth's peer index lives
// in another namespace and can equal its own.
func lxcLookalikes() ([]string, error) {
	ents, err := os.ReadDir(sysClassNet)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", sysClassNet, err)
	}
	var odd []string
	for _, e := range ents {
		name := e.Name()
		if !strings.HasPrefix(name, "lxc") {
			continue
		}
		for _, marker := range []string{"bridge", "device"} {
			if _, err := os.Lstat(filepath.Join(sysClassNet, name, marker)); err == nil {
				odd = append(odd, name)
				break
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
	}
	return odd, nil
}

// Remove an older owned wildcard without touching a more-specific bridge
// guard. Keep and report ownership when removal cannot be confirmed.
func revokeOwnedPodWildcard(r firewalldRecord) {
	owned := firewalldRecord{}
	for _, t := range r.TrustedInterfaces {
		if t.Interface == ciliumPodInterface && t.Zone == firewalldZone {
			owned.TrustedInterfaces = append(owned.TrustedInterfaces, t)
		}
	}
	var fixes, recordFailures []string
	untrustCiliumInterfaces(os.Stdout, owned, true, &fixes, &recordFailures)
	if len(fixes) > 0 {
		fmt.Printf("  WARN: firewalld: old %s binding may still be trusted; remove with: %s\n", ciliumPodInterface, strings.Join(fixes, "; "))
	}
	if len(recordFailures) > 0 {
		fmt.Printf("  WARN: firewalld: old %s ownership could not be updated: %s\n", ciliumPodInterface, strings.Join(recordFailures, "; "))
	}
}

func trustCiliumInterface() {
	r, err := readFirewalldRecord()
	if err != nil {
		fmt.Printf("  WARN: firewalld: cannot read interface ownership record: %v\n", err)
		return
	}
	// Checked on every start, so LXC installed after the cluster is caught at
	// the next one. The standard lxcbr prefix is handled by the more-specific
	// firewalld zone binding; other bridge and physical interfaces fail closed.
	odd, err := lxcLookalikes()
	var unsafe []string
	for _, name := range odd {
		if !strings.HasPrefix(name, "lxcbr") {
			unsafe = append(unsafe, name)
		}
	}
	if err != nil || len(unsafe) > 0 {
		why := fmt.Sprintf("%s also matches %s, which is not a Cilium pod interface", ciliumPodInterface, strings.Join(unsafe, ", "))
		if err != nil {
			why = fmt.Sprintf("cannot list interfaces to check what %s would match: %v", ciliumPodInterface, err)
		}
		fmt.Printf("  WARN: firewalld: %s; did not add pod trust, so pods may not reach this node.\n"+
			"  Bind each Cilium interface yourself (firewall-cmd --zone=%s --add-interface=lxc<id>), or move the other interfaces off the lxc prefix.\n",
			why, firewalldZone)
		revokeOwnedPodWildcard(r)
		return
	}
	if !excludeLXCBridge() {
		revokeOwnedPodWildcard(r)
		return
	}
	owned := slices.ContainsFunc(r.TrustedInterfaces, func(t trustedInterface) bool {
		return t.Interface == ciliumPodInterface && t.Zone == firewalldZone && (t.Runtime || t.Permanent)
	})
	var need []scope
	for _, s := range scopes {
		zone, code := firewallCmd(s.args("--get-zone-of-interface=" + ciliumPodInterface)...)
		switch {
		case code == 0 && zone == firewalldZone:
		case code == 0:
			fmt.Printf("  WARN: firewalld: interface %s belongs to zone %q (%s); left it unchanged\n", ciliumPodInterface, zone, s.name)
			return
		case code == exitNoZone:
			need = append(need, s)
		default:
			fmt.Printf("  WARN: firewalld: cannot check interface %s (%s): %s (exit %d); left it unchanged\n", ciliumPodInterface, s.name, zone, code)
			return
		}
	}
	if len(need) == 0 && !owned {
		return // operator-owned binding
	}
	for _, s := range need {
		if out, code := firewallCmd(s.args("--zone="+firewalldZone, "--add-interface="+ciliumPodInterface)...); code != 0 {
			fmt.Printf("  WARN: firewalld: cannot trust Cilium interfaces (%s): %s; add with: firewall-cmd %s\n", s.name, out,
				strings.Join(s.args("--zone="+firewalldZone, "--add-interface="+ciliumPodInterface), " "))
			return
		}
		if err := recordTrustedInterface(ciliumPodInterface, firewalldZone, s); err != nil {
			out, code := firewallCmd(s.args("--zone="+firewalldZone, "--remove-interface="+ciliumPodInterface)...)
			if code != 0 {
				fmt.Printf("  WARN: firewalld: trusted Cilium interfaces (%s), but could not record ownership (%v) or roll back (%s); remove with: firewall-cmd %s\n",
					s.name, err, out, strings.Join(s.args("--zone="+firewalldZone, "--remove-interface="+ciliumPodInterface), " "))
			} else {
				fmt.Printf("  WARN: firewalld: could not record Cilium interface ownership (%s): %v; rolled back that binding\n", s.name, err)
			}
			return
		}
		fmt.Printf("  firewalld: trusted Cilium pod interfaces %s (%s)\n", ciliumPodInterface, s.name)
	}
}

// Firewalld orders longer interface prefixes before shorter ones. Pin the
// standard LXC bridge prefix to the host's existing default zone so lxcbr0
// cannot inherit the lxc+ trusted binding. A failure prevents pod trust.
func excludeLXCBridge() bool {
	defaultZone, code := firewallCmd("--get-default-zone")
	if code != 0 || defaultZone == "" || defaultZone == firewalldZone {
		fmt.Printf("  WARN: firewalld: cannot establish a non-trusted zone for %s (default %q, exit %d); did not add pod trust\n", lxcBridgeInterface, defaultZone, code)
		return false
	}
	for _, s := range scopes {
		zone, code := firewallCmd(s.args("--get-zone-of-interface=" + lxcBridgeInterface)...)
		if code == 0 {
			if zone == firewalldZone {
				fmt.Printf("  WARN: firewalld: %s already belongs to trusted (%s); did not add pod trust\n", lxcBridgeInterface, s.name)
				return false
			}
			continue // operator-owned exclusion
		}
		if code != exitNoZone {
			fmt.Printf("  WARN: firewalld: cannot check %s (%s): %s (exit %d); did not add pod trust\n", lxcBridgeInterface, s.name, zone, code)
			return false
		}
		args := s.args("--zone="+defaultZone, "--add-interface="+lxcBridgeInterface)
		if out, code := firewallCmd(args...); code != 0 {
			fmt.Printf("  WARN: firewalld: cannot exclude LXC bridges (%s): %s; did not add pod trust\n", s.name, out)
			return false
		}
		if err := recordTrustedInterface(lxcBridgeInterface, defaultZone, s); err != nil {
			out, code := firewallCmd(s.args("--zone="+defaultZone, "--remove-interface="+lxcBridgeInterface)...)
			fmt.Printf("  WARN: firewalld: cannot record LXC bridge exclusion (%s): %v; rollback exit %d: %s\n", s.name, err, code, out)
			return false
		}
	}
	return true
}

func recordTrustedInterface(iface, zone string, s scope) error {
	r, err := readFirewalldRecord()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(r.TrustedInterfaces, func(t trustedInterface) bool {
		return t.Interface == iface && t.Zone == zone
	})
	if i < 0 {
		r.TrustedInterfaces = append(r.TrustedInterfaces, trustedInterface{Interface: iface, Zone: zone})
		i = len(r.TrustedInterfaces) - 1
	}
	if s.name == "permanent" {
		r.TrustedInterfaces[i].Permanent = true
	} else {
		r.TrustedInterfaces[i].Runtime = true
	}
	return writeFirewalldRecord(r)
}

func forgetTrustedInterface(iface, zone string, s scope) error {
	r, err := readFirewalldRecord()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(r.TrustedInterfaces, func(t trustedInterface) bool {
		return t.Interface == iface && t.Zone == zone
	})
	if i < 0 {
		return nil
	}
	if s.name == "permanent" {
		r.TrustedInterfaces[i].Permanent = false
	} else {
		r.TrustedInterfaces[i].Runtime = false
	}
	if !r.TrustedInterfaces[i].Runtime && !r.TrustedInterfaces[i].Permanent {
		r.TrustedInterfaces = slices.Delete(r.TrustedInterfaces, i, i+1)
	}
	return writeFirewalldRecord(r)
}

// OwnedFirewalldInterfaces lists interface bindings Stratum will remove, with
// their zones. This includes the non-trusted LXC bridge exclusion.
func OwnedFirewalldInterfaces() ([]string, error) {
	r, err := readFirewalldRecord()
	var names []string
	for _, t := range r.TrustedInterfaces {
		names = append(names, fmt.Sprintf("%s (zone %s)", t.Interface, t.Zone))
	}
	return names, err
}

func untrustCiliumInterfaces(w io.Writer, r firewalldRecord, running bool, fixes, recordFailures *[]string) {
	forget := func(t trustedInterface, s scope) {
		if err := forgetTrustedInterface(t.Interface, t.Zone, s); err != nil {
			fmt.Fprintf(w, "  WARN: firewalld: could not update interface ownership for %s (%s): %v\n", t.Interface, s.name, err)
			*recordFailures = append(*recordFailures, fmt.Sprintf("%s (%s): %v", t.Interface, s.name, err))
		}
	}
	// Remove broad pod trust before the bridge exclusion. Otherwise a failed
	// wildcard removal followed by a successful exclusion removal would
	// expose co-located LXC traffic to the trusted zone.
	bindings := slices.Clone(r.TrustedInterfaces)
	slices.SortStableFunc(bindings, func(a, b trustedInterface) int {
		if a.Interface == ciliumPodInterface {
			return -1
		}
		if b.Interface == ciliumPodInterface {
			return 1
		}
		return 0
	})
	for _, t := range bindings {
		if t.Interface == lxcBridgeInterface && len(*fixes) > 0 {
			// A failed wildcard removal must not be followed by removal of
			// its more-specific bridge protection.
			continue
		}
		for _, s := range scopes {
			if !t.owns(s) {
				continue
			}
			bin := "firewall-cmd"
			args := s.args("--zone="+t.Zone, "--remove-interface="+t.Interface)
			if !running {
				if s.name == "runtime" {
					forget(t, s)
					continue
				}
				bin = "firewall-offline-cmd"
				args = []string{"--zone=" + t.Zone, "--remove-interface=" + t.Interface}
			} else {
				out, code := firewallCmd(s.args("--zone="+t.Zone, "--query-interface="+t.Interface)...)
				if code == exitNo {
					forget(t, s)
					continue
				}
				if code != 0 {
					fmt.Fprintf(w, "  WARN: firewalld: cannot check interface %s (%s): %s\n", t.Interface, s.name, out)
					*fixes = append(*fixes, strings.Join(append([]string{bin}, args...), " "))
					continue
				}
			}
			out, code := runFirewall(bin, args...)
			if code == 0 || (bin == "firewall-offline-cmd" && strings.Contains(out, "NOT_ENABLED")) {
				forget(t, s)
				continue
			}
			fmt.Fprintf(w, "  WARN: firewalld: cannot remove interface %s (%s): %s\n", t.Interface, s.name, out)
			*fixes = append(*fixes, strings.Join(append([]string{bin}, args...), " "))
		}
	}
}

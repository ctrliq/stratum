package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ctrliq/stratum/internal/version"
)

// With firewalld running, pod traffic to the node itself is rejected: Cilium's
// per-pod lxc interfaces belong to no zone, so firewalld files them under the
// default zone (public), whose target rejects everything not listed. Kube-proxy
// replacement turns a pod's call to the kubernetes Service (10.96.0.1:443) into
// <node IP>:6443 arriving on an lxc interface, so CoreDNS and local-path never
// reach the API server. RLC and RHEL ship firewalld on, so this is the
// stock host there, not an exotic one; Rocky's cloud images ship without it.
//
// Bind Cilium's host-side lxc+ pod interfaces to the trusted zone. An earlier
// source-based rule trusted spoofed pod addresses on external interfaces even
// with reverse-path filtering; startup removes any such rule Stratum recorded.

// firewalldZone is the zone Cilium pod interfaces are bound to. A built-in.
const firewalldZone = "trusted"

// firewall-cmd exit codes this code branches on. 0 is success or "yes".
const (
	exitNo           = 1   // --query-source or --query-interface: not bound
	exitNoZone       = 2   // --get-zone-of-source or --get-zone-of-interface: not bound
	exitNotRunning   = 252 // --state: daemon is stopped
	exitNotInstalled = -2  // the binary is not on this host (runFirewall)
	exitNotRun       = -1  // it could not be run at all; the output is the error
)

// firewalldRecordPath lists bindings Stratum added, per zone and scope, so
// uninstall removes those rather than bindings the operator made. It lives
// under DataDir and is read before uninstall removes node state.
var firewalldRecordPath = filepath.Join(version.DataDir, "firewalld.json")

var errFirewalldNotInstalled = errors.New("firewall-cmd is not installed")

type firewalldRecord struct {
	TrustedSources    []trustedSource    `json:"trustedSources"`
	TrustedInterfaces []trustedInterface `json:"trustedInterfaces,omitempty"`
}

// trustedSource is one source stratum bound, and in which configurations. An
// operator who had already bound it in one of them keeps that half at
// uninstall: it is not recorded as stratum's.
type trustedSource struct {
	Source    string `json:"source"`
	Zone      string `json:"zone"`
	Runtime   bool   `json:"runtime,omitempty"`
	Permanent bool   `json:"permanent,omitempty"`
}

type trustedInterface struct {
	Interface string `json:"interface"`
	Zone      string `json:"zone"`
	Runtime   bool   `json:"runtime,omitempty"`
	Permanent bool   `json:"permanent,omitempty"`
}

// scope is one of firewalld's two configurations: the running one, and the
// permanent one it loads at start.
type scope struct {
	name  string
	flags []string
}

var scopes = []scope{{"runtime", nil}, {"permanent", []string{"--permanent"}}}

func (s scope) args(a ...string) []string { return append(slices.Clone(s.flags), a...) }

func (t trustedSource) owns(s scope) bool {
	if s.name == "permanent" {
		return t.Permanent
	}
	return t.Runtime
}

func (t trustedInterface) owns(s scope) bool {
	if s.name == "permanent" {
		return t.Permanent
	}
	return t.Runtime
}

// runFirewall runs firewall-cmd or firewall-offline-cmd and returns its trimmed
// output and exit code. When the command could not be run, the output is the
// reason, so no warning built from it ends in an empty colon. A seam for tests.
var runFirewall = func(bin string, args ...string) (string, int) {
	out, err := hostCmd(bin, args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	var ee *exec.ExitError
	switch {
	case err == nil:
		return s, 0
	case errors.As(err, &ee):
		return s, ee.ExitCode()
	case errors.Is(err, exec.ErrNotFound):
		return err.Error(), exitNotInstalled
	default:
		return err.Error(), exitNotRun
	}
}

func firewallCmd(args ...string) (string, int) { return runFirewall("firewall-cmd", args...) }

// firewalldState distinguishes an explicitly stopped daemon from a failed
// status check. A failed check must not discard runtime ownership: the rules
// may still be active even when D-Bus or permissions prevent querying them.
func firewalldState() (bool, error) {
	out, code := firewallCmd("--state")
	switch {
	case code == 0 && out == "running":
		return true, nil
	case code == exitNotRunning:
		return false, nil
	case code == exitNotInstalled:
		return false, fmt.Errorf("firewall-cmd --state: %w: %s", errFirewalldNotInstalled, out)
	default:
		return false, fmt.Errorf("firewall-cmd --state: %s (exit %d)", out, code)
	}
}

// TrustPodCIDRs trusts Cilium's host-side pod interfaces, not the pod source
// range. A source binding also accepts spoofed pod addresses arriving on an
// external NIC. The argument is retained for existing callers; migration uses
// the ownership record. It never reloads firewalld: under the iptables backend a
// reload flushes Cilium's rules with it.
//
// Idempotent, and called on every start of every role. It warns rather than
// fails: a host whose firewall cannot be changed still gets a cluster, and the
// warning carries the exact command, where failing would take down a node that
// may be working.
func TrustPodCIDRs(_ string) {
	running, err := firewalldState()
	if err != nil {
		if errors.Is(err, errFirewalldNotInstalled) {
			r, recordErr := readFirewalldRecord()
			if recordErr == nil && len(r.TrustedSources) == 0 && len(r.TrustedInterfaces) == 0 {
				return // ordinary host without firewalld
			}
		}
		fmt.Printf("  WARN: firewalld: cannot check daemon state: %v; left pod trust unchanged\n", err)
		return
	}
	if !running {
		removeUnsafeOwnedSourcesOffline()
		removeOwnedCiliumWildcardOffline()
		return
	}
	r, err := readFirewalldRecord()
	if err != nil {
		fmt.Printf("  WARN: firewalld: cannot read source ownership record %s: %v; left pod trust unchanged\n", firewalldRecordPath, err)
		return
	}
	// Revoke the old broad source bindings before adding the scoped interface
	// binding. Keep failed removals recorded so uninstall can retry them.
	for _, t := range r.TrustedSources {
		if t.Runtime || t.Permanent {
			removeUnsafeOwnedSource(t.Source, r)
		}
	}
	trustCiliumInterface()
}

// A stopped daemon has no runtime rules, but its permanent configuration can
// still contain a legacy source binding. Remove that binding before the next
// daemon start; keep ownership when the offline edit cannot be confirmed.
func removeUnsafeOwnedSourcesOffline() {
	r, err := readFirewalldRecord()
	if err != nil {
		fmt.Printf("  WARN: firewalld: cannot read source ownership record %s: %v; left pod trust unchanged\n", firewalldRecordPath, err)
		return
	}
	for _, t := range r.TrustedSources {
		if t.Runtime {
			if err := forgetTrustedSource(t.Source, scopes[0]); err != nil {
				fmt.Printf("  WARN: firewalld: could not update runtime source ownership for %s: %v\n", t.Source, err)
			}
		}
		if !t.Permanent {
			continue
		}
		cmd := "firewall-offline-cmd --zone=" + t.Zone + " --remove-source=" + t.Source
		out, code := runFirewall("firewall-offline-cmd", "--zone="+t.Zone, "--remove-source="+t.Source)
		if code != 0 && !strings.Contains(out, "NOT_ENABLED") {
			fmt.Printf("  WARN: firewalld: could not remove unsafe trusted source %s offline: %s; remove it with: %s\n", t.Source, out, cmd)
			continue
		}
		if err := forgetTrustedSource(t.Source, scopes[1]); err != nil {
			fmt.Printf("  WARN: firewalld: removed unsafe source %s offline, but could not update its ownership record: %v\n", t.Source, err)
		}
	}
}

// removeUnsafeOwnedSource revokes only Stratum's legacy CIDR source bindings.
// Forget each half after it is gone, so an operator's later binding in that
// scope cannot be mistaken for Stratum's.
func removeUnsafeOwnedSource(cidr string, r firewalldRecord) {
	for _, t := range r.TrustedSources {
		if t.Source != cidr || t.Zone != firewalldZone {
			continue
		}
		for _, s := range scopes {
			if !t.owns(s) {
				continue
			}
			cmd := strings.Join(append([]string{"firewall-cmd"}, s.args("--zone="+firewalldZone, "--remove-source="+cidr)...), " ")
			out, code := firewallCmd(s.args("--zone="+firewalldZone, "--query-source="+cidr)...)
			if code == exitNo {
				if err := forgetTrustedSource(cidr, s); err != nil {
					fmt.Printf("  WARN: firewalld: source %s (%s) is gone, but could not update its ownership record: %v\n", cidr, s.name, err)
				}
				continue
			}
			if code != 0 {
				fmt.Printf("  WARN: firewalld: could not check unsafe trusted source %s (%s): %s; remove it with: %s\n", cidr, s.name, out, cmd)
				continue
			}
			if out, code := firewallCmd(s.args("--zone="+firewalldZone, "--remove-source="+cidr)...); code != 0 {
				fmt.Printf("  WARN: firewalld: could not remove unsafe trusted source %s (%s): %s; remove it with: %s\n", cidr, s.name, out, cmd)
				continue
			}
			if err := forgetTrustedSource(cidr, s); err != nil {
				fmt.Printf("  WARN: firewalld: removed unsafe source %s (%s), but could not update its ownership record: %v\n", cidr, s.name, err)
			}
			fmt.Printf("  firewalld: removed unsafe trusted source %s (%s)\n", cidr, s.name)
		}
	}
}

func readFirewalldRecord() (firewalldRecord, error) {
	var r firewalldRecord
	b, err := os.ReadFile(firewalldRecordPath)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("%s: %w", firewalldRecordPath, err)
	}
	return r, nil
}

func forgetTrustedSource(cidr string, s scope) error {
	r, err := readFirewalldRecord()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(r.TrustedSources, func(t trustedSource) bool {
		return t.Source == cidr && t.Zone == firewalldZone
	})
	if i < 0 {
		return nil
	}
	if s.name == "permanent" {
		r.TrustedSources[i].Permanent = false
	} else {
		r.TrustedSources[i].Runtime = false
	}
	if !r.TrustedSources[i].Runtime && !r.TrustedSources[i].Permanent {
		r.TrustedSources = slices.Delete(r.TrustedSources, i, i+1)
	}
	return writeFirewalldRecord(r)
}

var writeFirewalldRecord = func(r firewalldRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(firewalldRecordPath), 0o755); err != nil {
		return err
	}
	// Atomic: a torn record would stop uninstall removing anything at all.
	return WriteFileAtomically(firewalldRecordPath, b, 0o644)
}

// TrustedPodCIDRs returns the sources stratum added to firewalld, for
// uninstall's dry-run disclosure. An unreadable record is an error, not an
// empty list: the real run would fail on it, and the preview must say so.
func TrustedPodCIDRs() ([]string, error) {
	r, err := readFirewalldRecord()
	var cidrs []string
	for _, t := range r.TrustedSources {
		cidrs = append(cidrs, t.Source)
	}
	return cidrs, err
}

// UntrustPodCIDRs removes source and interface bindings Stratum added, from
// the configurations it added them to and no others. With firewalld stopped,
// runtime configuration is already gone and permanent configuration is edited
// offline, so the binding does
// not come back at the next firewalld start. Self-logging; returns an error
// carrying the exact commands for whatever it could not remove.
func UntrustPodCIDRs() error { return UntrustPodCIDRsWithWriter(os.Stdout) }

// UntrustPodCIDRsWithWriter removes owned firewall bindings and reports progress to w.
func UntrustPodCIDRsWithWriter(w io.Writer) error {
	r, err := readFirewalldRecord()
	if err != nil {
		// Name the interface binding as well as sources: it is what this
		// change normally owns, and without the record nothing says lxc+ may
		// still be trusted permanently.
		return fmt.Errorf("firewalld: read what stratum added: %w; list what is trusted with: "+
			"firewall-cmd --permanent --zone=%[2]s --list-interfaces; firewall-cmd --permanent --zone=%[2]s --list-sources "+
			"(and the same without --permanent for the running config); remove Stratum's binding with: "+
			"firewall-cmd --permanent --zone=%[2]s --remove-interface=%[3]s; firewall-cmd --zone=%[2]s --remove-interface=%[3]s",
			err, firewalldZone, ciliumPodInterface)
	}
	if len(r.TrustedSources) == 0 && len(r.TrustedInterfaces) == 0 {
		return nil
	}
	var fixes []string
	var recordFailures []string
	forget := func(t trustedSource, s scope) {
		if err := forgetTrustedSource(t.Source, s); err != nil {
			fmt.Fprintf(w, "  WARN: firewalld: could not update ownership record for %s (%s): %v\n", t.Source, s.name, err)
			recordFailures = append(recordFailures, fmt.Sprintf("%s (%s): %v", t.Source, s.name, err))
		}
	}
	running, err := firewalldState()
	if err != nil {
		return fmt.Errorf("firewalld: cannot safely clean owned bindings: %w; ownership retained for retry", err)
	}
	for _, t := range r.TrustedSources {
		var removed []string
		failed := false
		fail := func(cmd string) { fixes = append(fixes, cmd); failed = true }
		if running {
			for _, s := range scopes {
				if !t.owns(s) {
					continue
				}
				fix := strings.Join(append([]string{"firewall-cmd"}, s.args("--zone="+t.Zone, "--remove-source="+t.Source)...), " ")
				out, code := firewallCmd(s.args("--zone="+t.Zone, "--query-source="+t.Source)...)
				switch code {
				case 0:
				case exitNo:
					forget(t, s)
					continue // already gone
				default:
					fmt.Fprintf(w, "  WARN: firewalld: could not check trusted source %s (%s): %s\n", t.Source, s.name, out)
					fail(fix)
					continue
				}
				if out, code := firewallCmd(s.args("--zone="+t.Zone, "--remove-source="+t.Source)...); code != 0 {
					fmt.Fprintf(w, "  WARN: firewalld: could not remove trusted source %s (%s): %s\n", t.Source, s.name, out)
					fail(fix)
					continue
				}
				removed = append(removed, s.name)
				forget(t, s)
			}
		} else {
			// Runtime configuration disappears when firewalld stops.
			if t.Runtime {
				forget(t, scopes[0])
			}
			if t.Permanent {
				out, code := runFirewall("firewall-offline-cmd", "--zone="+t.Zone, "--remove-source="+t.Source)
				switch {
				case strings.Contains(out, "NOT_ENABLED"):
					forget(t, scopes[1]) // already absent
				case code != 0:
					fmt.Fprintf(w, "  WARN: firewalld: stopped, and could not remove trusted source %s offline: %s\n", t.Source, out)
					fail("firewall-offline-cmd --zone=" + t.Zone + " --remove-source=" + t.Source)
				default:
					removed = append(removed, "permanent")
					forget(t, scopes[1])
				}
			}
		}
		switch {
		case len(removed) > 0:
			fmt.Fprintf(w, "  firewalld: removed trusted source %s (zone %s, %s)\n", t.Source, t.Zone, strings.Join(removed, " and "))
		case !failed:
			fmt.Fprintf(w, "  firewalld: trusted source %s was already gone (zone %s)\n", t.Source, t.Zone)
		}
	}
	untrustCiliumInterfaces(w, r, running, &fixes, &recordFailures)
	if len(fixes) > 0 {
		err := fmt.Errorf("firewalld may still trust bindings stratum added; remove them with: %s", strings.Join(fixes, "; "))
		if len(recordFailures) == 0 {
			return err
		}
		return errors.Join(err, fmt.Errorf("firewalld could not update %s: %s", firewalldRecordPath, strings.Join(recordFailures, "; ")))
	}
	if len(recordFailures) > 0 {
		return fmt.Errorf("firewalld could not update %s: %s", firewalldRecordPath, strings.Join(recordFailures, "; "))
	}
	return nil
}

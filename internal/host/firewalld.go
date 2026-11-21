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

// Pods reach the node on Cilium lxc interfaces, which firewalld's default zone rejects.
const firewalldZone = "trusted"

const (
	exitNo           = 1   // --query-source or --query-interface: not bound
	exitNoZone       = 2   // --get-zone-of-source or --get-zone-of-interface: not bound
	exitNotRunning   = 252 // --state: daemon is stopped
	exitNotInstalled = -2
	exitNotRun       = -1 // it could not be run at all; the output is the error
)

// Read before uninstall removes node state.
var firewalldRecordPath = filepath.Join(version.DataDir, "firewalld.json")

var errFirewalldNotInstalled = errors.New("firewall-cmd is not installed")

type firewalldRecord struct {
	TrustedSources    []trustedSource    `json:"trustedSources"`
	TrustedInterfaces []trustedInterface `json:"trustedInterfaces,omitempty"`
}

// Only stratum's own bindings are recorded; an operator's keep their scope at uninstall.
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

// One of firewalld's two configurations; the permanent one loads at daemon start.
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

// When the command could not be run at all, the output is the error text.
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

// A failed check must not discard ownership: the rules may still be active.
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

// Never reloads firewalld: under the iptables backend a reload flushes Cilium's rules.
func TrustPodCIDRs(_ string) {
	running, err := firewalldState()
	if err != nil {
		if errors.Is(err, errFirewalldNotInstalled) {
			r, recordErr := readFirewalldRecord()
			if recordErr == nil && len(r.TrustedSources) == 0 && len(r.TrustedInterfaces) == 0 {
				return
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
	// Keep failed removals recorded so uninstall can retry them.
	for _, t := range r.TrustedSources {
		if t.Runtime || t.Permanent {
			removeUnsafeOwnedSource(t.Source, r)
		}
	}
	trustCiliumInterface()
}

// A stopped daemon can still hold a legacy source binding in its permanent configuration.
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

// Forget each scope after removal, so an operator's later binding is not mistaken for stratum's.
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
	// A torn record would stop uninstall removing anything at all.
	return WriteFileAtomically(firewalldRecordPath, b, 0o644)
}

// An unreadable record is an error, not an empty list: the preview must match the run.
func TrustedPodCIDRs() ([]string, error) {
	r, err := readFirewalldRecord()
	var cidrs []string
	for _, t := range r.TrustedSources {
		cidrs = append(cidrs, t.Source)
	}
	return cidrs, err
}

// With firewalld stopped it edits the permanent configuration offline.
func UntrustPodCIDRs() error { return UntrustPodCIDRsWithWriter(os.Stdout) }

func UntrustPodCIDRsWithWriter(w io.Writer) error {
	r, err := readFirewalldRecord()
	if err != nil {
		// Without the record nothing says lxc+ may still be trusted permanently.
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
					continue
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
					forget(t, scopes[1])
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

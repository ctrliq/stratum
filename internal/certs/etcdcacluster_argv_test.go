package certs

import (
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// TestEtcdNodeArgvHasNoShell is the shell-free argv shape, pinned for this command the
// same way cacluster_argv_test.go and orchestrate_test.go pin it for theirs.
//
// An argv built as one string and handed to `/bin/bash -c` would make the
// payload executable text on every node in the cluster, as root, in the host
// namespaces. Nothing here is attacker-influenced today — phase is
// one of four constants and version.BinDir is fixed at compile time — so this
// closes a shape rather than a live injection. That is exactly why it is worth
// pinning: the guarantee should not depend on who happens to call it.
func TestEtcdNodeArgvHasNoShell(t *testing.T) {
	for _, phase := range append(slices.Clone(EtcdCAPhases), pki.EtcdPhaseRollback) {
		argv := etcdNodeArgv(phase, true, true)
		for _, shell := range []string{"sh", "bash", "/bin/sh", "/bin/bash", "-c", "eval"} {
			if slices.Contains(argv, shell) {
				t.Errorf("phase %s: argv invokes a shell (%q): %v", phase, shell, argv)
			}
		}
		if argv[0] != "nsenter" {
			t.Errorf("phase %s: argv[0] = %q, want nsenter", phase, argv[0])
		}
		// The binary must be the on-host stratum, reached through PID 1's mount
		// namespace, not whatever the container image happens to carry -- the
		// installed one, since BinDir holds no stratum.
		if !slices.Contains(argv, version.StratumInstallPath) {
			t.Errorf("phase %s: argv does not exec the on-host stratum binary: %v", phase, argv)
		}
	}
}

// TestEtcdNodeArgvKeepsHostileValuesAsSingleElements: even a phase carrying
// shell metacharacters must survive as ONE argv element. nsenter execs the
// binary directly, so no interpreter ever re-parses it — this asserts that the
// builder does not itself split or join.
func TestEtcdNodeArgvKeepsHostileValuesAsSingleElements(t *testing.T) {
	hostile := "trust-both; rm -rf / #"
	argv := etcdNodeArgv(hostile, false, false)
	if !slices.Contains(argv, hostile) {
		t.Fatalf("the phase was not carried as a single argv element: %v", argv)
	}
	for _, a := range argv {
		if a != hostile && strings.ContainsAny(a, ";|&$`") {
			t.Errorf("argv element %q carries shell metacharacters", a)
		}
	}
}

// TestEtcdNodeArgvCarriesTheRequiredFlags. The Job runs unattended, so --yes
// must always be present; --fetch-shared and --force must appear only when the
// orchestrator asked for them. A missing --fetch-shared makes a follower try to
// mint its own etcd CA, which pki correctly refuses — a stray one makes the
// INITIATOR refuse to mint the anchor nobody else can then receive.
func TestEtcdNodeArgvCarriesTheRequiredFlags(t *testing.T) {
	for _, tc := range []struct{ fetchShared, force bool }{
		{false, false}, {true, false}, {false, true}, {true, true},
	} {
		argv := etcdNodeArgv(pki.EtcdPhaseTrustBoth, tc.fetchShared, tc.force)
		if !slices.Contains(argv, "--yes") {
			t.Errorf("%+v: argv is missing --yes, so the Job would stop and ask: %v", tc, argv)
		}
		if got := slices.Contains(argv, "--fetch-shared"); got != tc.fetchShared {
			t.Errorf("%+v: --fetch-shared present = %v", tc, got)
		}
		if got := slices.Contains(argv, "--force"); got != tc.force {
			t.Errorf("%+v: --force present = %v", tc, got)
		}
		// --phase must be followed by the phase, as two elements.
		i := slices.Index(argv, "--phase")
		if i < 0 || i+1 >= len(argv) || argv[i+1] != pki.EtcdPhaseTrustBoth {
			t.Errorf("%+v: --phase is not followed by the phase: %v", tc, argv)
		}
	}
}

// TestEtcdNodeArgvIsAcceptedByTheCLIItInvokes closes the loop the other argv
// tests leave open.
//
// Those assert the SHAPE of the argv — shell-free, --yes present, --force
// present when asked. None of them assert that the binary on the other end
// accepts it, and the binary is `stratum certs migrate-etcd-ca` itself: the Job
// nsenters into the host and runs this very command. A validation rule added to
// the CLI can therefore make every follower Job fail while every test here
// still passes — for instance by rejecting `--force` unless the separate
// `--rollback` bool is also set, while the argv builder emits
// `--phase rollback --force`.
//
// The initiator does NOT go through cobra (clusterDriver runs it in-process),
// so the failure is asymmetric and worse than a plain outage: the initiator
// un-migrates and every follower refuses, splitting the cluster's trust.
//
// It lives here rather than in pkg/cli because this is the file that owns the
// argv; pkg/cli cannot import internal/certs.
func TestEtcdNodeArgvIsAcceptedByTheCLIItInvokes(t *testing.T) {
	// Exactly the combinations the orchestrator emits, and no others: runPhase
	// sets fetchShared only for a follower's trust-both, and force only on the
	// rollback path. Testing the full cross-product would assert that the CLI
	// accepts argv the driver cannot build — and then a correct validation rule
	// would look like a failure.
	for _, tc := range []struct {
		phase              string
		fetchShared, force bool
	}{
		{pki.EtcdPhaseTrustBoth, false, false}, // initiator
		{pki.EtcdPhaseTrustBoth, true, false},  // follower
		{pki.EtcdPhaseIssueNew, false, false},
		{pki.EtcdPhaseDropRoot, false, false},
		{pki.EtcdPhaseRollback, false, false},
		{pki.EtcdPhaseRollback, false, true}, // --force, the documented escape hatch
		{EtcdPhaseReAddRoot, false, true},    // pass 1 of a forced un-migration
	} {
		argv := etcdNodeArgv(tc.phase, tc.fetchShared, tc.force)
		// Drop the nsenter prefix, the binary path and the two subcommand
		// verbs: what cobra binds is the flags after them.
		var args []string
		for i, a := range argv {
			if strings.HasSuffix(a, "/stratum") {
				args = argv[i+1:]
				break
			}
		}
		if len(args) < 2 || args[0] != "certs" || args[1] != "migrate-etcd-ca" {
			t.Fatalf("phase %s: argv does not invoke `certs migrate-etcd-ca`: %v", tc.phase, argv)
		}
		if err := parseMigrateFlags(t, args[2:]).Validate(); err != nil {
			t.Errorf("phase=%s fetchShared=%v force=%v: the CLI rejects the argv the orchestrator sends: %v\n  argv: %v",
				tc.phase, tc.fetchShared, tc.force, err, args)
		}
	}
}

// parseMigrateFlags turns an argv tail back into the flag set the CLI would
// bind, so the assertion runs against the REAL validation rather than a copy of
// it. pkg/cli binds these same names onto these same fields.
func parseMigrateFlags(t *testing.T, args []string) EtcdCAFlags {
	t.Helper()
	var f EtcdCAFlags
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--cluster":
			f.Cluster = true
		case a == "--rollback":
			f.Rollback = true
		case a == "--force":
			f.Force = true
		case a == "--resume":
			f.Resume = true
		case a == "--fetch-shared":
			f.FetchShared = true
		case a == "--yes":
		case a == "--phase":
			if i+1 >= len(args) {
				t.Fatalf("--phase with no value in %v", args)
			}
			f.Phase = args[i+1]
			i++
		default:
			if v, ok := strings.CutPrefix(a, "--phase="); ok {
				f.Phase = v
				continue
			}
			t.Fatalf("argv carries a flag this test does not know: %q in %v", a, args)
		}
	}
	return f
}

// TestMigrateEtcdCAFlagValidation pins the combinations an operator can reach.
func TestMigrateEtcdCAFlagValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		f       EtcdCAFlags
		wantErr string // substring; "" means accepted
	}{
		{"cluster run", EtcdCAFlags{Cluster: true}, ""},
		{"cluster resume", EtcdCAFlags{Cluster: true, Resume: true}, ""},
		{"cluster rollback", EtcdCAFlags{Cluster: true, Rollback: true}, ""},
		{"cluster forced rollback", EtcdCAFlags{Cluster: true, Rollback: true, Force: true}, ""},
		{"single-node phase", EtcdCAFlags{Phase: pki.EtcdPhaseIssueNew}, ""},
		{"follower trust-both", EtcdCAFlags{Phase: pki.EtcdPhaseTrustBoth, FetchShared: true}, ""},
		// The orchestrator's own spelling of a forced rollback.
		{"phase rollback with force", EtcdCAFlags{Phase: pki.EtcdPhaseRollback, Force: true}, ""},
		{"re-add-root with force", EtcdCAFlags{Phase: EtcdPhaseReAddRoot, Force: true}, ""},
		// Bare, it re-opens the etcd trust hole (etcd accepting any client
		// certificate signed by the cluster root, which also signs every
		// kubelet's certificate) on one node with no guard and no warning.
		{"re-add-root without force", EtcdCAFlags{Phase: EtcdPhaseReAddRoot}, "RE-OPENS the etcd trust hole"},

		{"cluster and phase", EtcdCAFlags{Cluster: true, Phase: pki.EtcdPhaseIssueNew}, "not both"},
		{"rollback and a forward phase", EtcdCAFlags{Rollback: true, Phase: pki.EtcdPhaseIssueNew}, "opposite directions"},
		{"force without a rollback", EtcdCAFlags{Cluster: true, Force: true}, "--force only applies"},
		{"resume without cluster", EtcdCAFlags{Phase: pki.EtcdPhaseIssueNew, Resume: true}, "--resume only applies"},
		{"fetch-shared on the wrong phase", EtcdCAFlags{Phase: pki.EtcdPhaseIssueNew, FetchShared: true}, "--fetch-shared only applies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.f.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want accepted, got: %v", err)
			case tc.wantErr == "":
				return
			case err == nil:
				t.Fatalf("want an error containing %q, got accepted", tc.wantErr)
			case !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestEtcdClientAddrBracketsIPv6 — the address the final proof that the etcd
// trust hole is closed dials. Unbracketed, it would report a correctly migrated
// dual-stack cluster as unverified.
func TestEtcdClientAddrBracketsIPv6(t *testing.T) {
	for _, tc := range []struct{ ip, want string }{
		{"10.0.0.1", "10.0.0.1:2379"},
		{"fd00::1", "[fd00::1]:2379"},
	} {
		if got := etcdClientAddr(tc.ip); got != tc.want {
			t.Errorf("etcdClientAddr(%q) = %q, want %q", tc.ip, got, tc.want)
		}
	}
}

// The Job execs the stratum installed on the node. BinDir holds the payload's
// binaries and never stratum, so a Job that ran BinDir/stratum failed on every
// node but the initiator, and a rollback stopped halfway.
func TestEtcdNodeArgvExecsTheInstalledStratum(t *testing.T) {
	argv := etcdNodeArgv(pki.EtcdPhaseTrustBoth, false, false)
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

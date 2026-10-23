package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/token"
	"github.com/ctrliq/stratum/internal/version"
)

const testCertKey = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

// freshPKI redirects every path `certs rotate` reads onto a temp tree and
// generates the PKI `stratum up` leaves — the setup of internal/certs' prep
// helper, inlined because it lives in another package, with the config paths
// (node state, netconfig, the supervisor socket) redirected too so the command
// cannot touch the host's own /var/lib/stratum. Returns the PKI dir.
func freshPKI(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	origPKI, origConf, origNet, origSock := version.PKIDir, version.K8sConfDir, version.NetConfigPath, version.ControlSocket
	t.Cleanup(func() {
		version.PKIDir, version.K8sConfDir, version.NetConfigPath, version.ControlSocket = origPKI, origConf, origNet, origSock
	})
	version.PKIDir = base
	version.K8sConfDir = filepath.Join(base, "config")
	version.NetConfigPath = filepath.Join(base, "netconfig")    // absent: the IPv4 defaults
	version.ControlSocket = filepath.Join(base, "stratum.sock") // absent: nothing to restart
	t.Setenv("NODE_IP", "10.0.0.9")                             // host.NodeIP() returns $NODE_IP if set
	if err := pki.GenerateAll("cp1", "10.0.0.9", nil); err != nil {
		t.Fatal(err)
	}
	return base
}

// pkiContents records every file under the PKI dir by content, so a test can
// prove a command left the certificates untouched.
func pkiContents(t *testing.T) map[string]string {
	t.Helper()
	got := map[string]string{}
	if err := filepath.WalkDir(version.PKIDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(strings.TrimPrefix(p, version.PKIDir+string(filepath.Separator)))] = string(b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestRunRekeySharedHandsOverByFileNeverStdout pins the property:
// a successful re-key writes the new key to a file and prints only the PATH — the
// raw certificate-key must never reach stdout.
func TestRunRekeySharedHandsOverByFileNeverStdout(t *testing.T) {
	var out bytes.Buffer
	var uploaded string
	reUpload := func(k string) error { uploaded = k; return nil }
	writeKeyFile := func(string) (string, error) { return "/run/stratum/ha-certificate-key-test", nil }

	if err := runRekeyShared(&out, func() (string, error) { return testCertKey, nil }, reUpload, writeKeyFile); err != nil {
		t.Fatalf("runRekeyShared: %v", err)
	}
	if uploaded != testCertKey {
		t.Errorf("re-upload received %q, want the freshly minted key", uploaded)
	}
	if s := out.String(); !strings.Contains(s, "/run/stratum/ha-certificate-key-test") {
		t.Errorf("output does not disclose the key-file path:\n%s", s)
	}
	if strings.Contains(out.String(), testCertKey) {
		t.Error("the raw certificate-key was printed to stdout — it must only be handed over by file")
	}
}

// TestRunRekeySharedFailsClosedBeforeHandover: if the re-encrypt fails, the key
// file is never written and nothing is printed — no dangling key, no half-done
// message an operator might act on.
func TestRunRekeySharedFailsClosedBeforeHandover(t *testing.T) {
	var out bytes.Buffer
	boom := errors.New("apiserver down")
	wrote := false
	writeKeyFile := func(string) (string, error) { wrote = true; return "x", nil }

	err := runRekeyShared(&out,
		func() (string, error) { return testCertKey, nil },
		func(string) error { return boom },
		writeKeyFile)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap the re-encrypt failure", err)
	}
	if wrote {
		t.Error("the key file was written even though the re-encrypt failed")
	}
	if out.Len() != 0 {
		t.Errorf("nothing should be printed on failure, got:\n%s", out.String())
	}
}

// TestCertsRotateRekeySharedGating pins the two guard branches that return before
// any cluster call: --rekey-shared needs --yes, and rejects --if-expiring (a
// leaf-rotation-only flag).
func TestCertsRotateRekeySharedGating(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"needs --yes", []string{"--rekey-shared"}, "re-run with --yes"},
		{"rejects explicit zero", []string{"--rekey-shared", "--yes", "--if-expiring=0"}, "does not apply"},
		{"rejects separate zero", []string{"--rekey-shared", "--yes", "--if-expiring", "0"}, "does not apply"},
		{"rejects --if-expiring", []string{"--rekey-shared", "--yes", "--if-expiring=5"}, "does not apply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newCertsRotateCommand()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestCertsRotateRekeySharedSurfacesTheRekeyGuardRefusal pins the delegation:
// --rekey-shared reads no rotation state itself, it returns the one re-key
// entry point's refusal — behind the rekeyGuard seam, certs.GuardRekey in
// production — prefixed so the operator can tell which command asked. The
// guard's two legs (node-local mid-rotation phase, cluster-wide
// stratum-carotate Secret) are pinned in internal/certs/rekey_test.go, and
// TestRekeySharedRefusesWithARotationStagedOnDisk keeps the production
// node-local leg end-to-end through the real command.
func TestCertsRotateRekeySharedSurfacesTheRekeyGuardRefusal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		guard string // what the re-key entry point refuses with
	}{
		{"mid-rotation on this node", "a cluster CA rotation is in flight (sign-new)"},
		{"staged key cluster-wide", "the stratum-carotate Secret exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldFence := rekeyMaintenanceFence
			rekeyMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) { return &k8sutil.MaintenanceLease{Release: func() {}}, nil }
			t.Cleanup(func() { rekeyMaintenanceFence = oldFence })
			old := rekeyGuard
			rekeyGuard = func() error { return errors.New(tc.guard) }
			t.Cleanup(func() { rekeyGuard = old })

			cmd := newCertsRotateCommand()
			cmd.SetArgs([]string{"--rekey-shared", "--yes"})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.guard) {
				t.Fatalf("err = %v, want the command to surface the re-key guard's refusal (%q)", err, tc.guard)
			}
			if !strings.Contains(err.Error(), "--rekey-shared") {
				t.Errorf("err = %v, want it named for the command that asked", err)
			}
		})
	}
}

// TestCertsRotateRekeySharedRefusesAnUnregisteredProfile: a re-key republishes
// the shared bundle with the registered profiles' secrets, so on a cis
// control plane run by an engine binary it would publish a bundle without the
// at-rest encryption key. The command resolves the node's profile before the
// fence, the guard and the mint, and refuses with the cis note.
func TestCertsRotateRekeySharedRefusesAnUnregisteredProfile(t *testing.T) {
	registerProfilesForTest(t)
	oldConf := version.K8sConfDir
	version.K8sConfDir = t.TempDir()
	t.Cleanup(func() { version.K8sConfDir = oldConf })
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", Profile: profile.CISName}); err != nil {
		t.Fatal(err)
	}
	oldFence, oldGuard, oldMint, oldUpload := rekeyMaintenanceFence, rekeyGuard, rekeyNewCertKey, rekeySharedCerts
	t.Cleanup(func() {
		rekeyMaintenanceFence, rekeyGuard, rekeyNewCertKey, rekeySharedCerts = oldFence, oldGuard, oldMint, oldUpload
	})
	reached := func(what string) {
		t.Errorf("the re-key reached its %s although the node's profile is not registered", what)
	}
	rekeyMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
		reached("maintenance fence")
		return &k8sutil.MaintenanceLease{Release: func() {}}, nil
	}
	rekeyGuard = func() error { reached("re-key guard"); return nil }
	rekeyNewCertKey = func() (string, error) { reached("mint"); return testCertKey, nil }
	rekeySharedCerts = func(string, func(context.Context) error) error { reached("upload"); return nil }

	cmd := newCertsRotateCommand()
	cmd.SetArgs([]string{"--rekey-shared", "--yes"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if !errors.Is(err, profile.ErrUnregistered) {
		t.Fatalf("err = %v, want the refusal wrapping profile.ErrUnregistered", err)
	}
	for _, want := range []string{profile.CISUnregisteredNote, "--rekey-shared", "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q: %v", want, err)
		}
	}
}

// TestTokenCreateControlPlaneRefusesMidRotation pins the mid-rotation re-key
// refusal: `token create --control-plane` mints a new certificate-key and
// re-encrypts stratum-certs, so it consults the same re-key entry point `certs
// rotate --rekey-shared` does and refuses while a CA rotation is staged or
// mid-phase — a token minted then locks the rotation's follower control planes
// out of the bundle. The refusal precedes the mint, which also republishes
// cluster-info, so no credential or trust publication is changed. A worker
// token re-keys nothing, but its mint still republishes cluster-info and the
// kubelet client CA from local PKI, so it consults the guard too: a worker mint
// from a controller the rotation has not reached would replace the published
// old‖new trust with old-only and strand every joiner pinned to the staged CA.
func TestTokenCreateControlPlaneRefusesMidRotation(t *testing.T) {
	refusal := errors.New("a cluster-wide CA rotation or etcd-CA migration is staging shared material (the stratum-carotate Secret exists)")

	t.Run("control-plane refuses before minting", func(t *testing.T) {
		var out bytes.Buffer
		var revoked []string
		c := okCmd(&out, &revoked)
		c.rekeyGuard = func() error { return refusal }
		c.create = func(_ io.Writer, _ string, _ time.Duration, _ bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			t.Error("mint/publication ran although the guard refused")
			return "", "", nil, errors.New("unreachable")
		}
		c.newCertKey = func() (string, error) {
			t.Error("newCertKey ran although the re-key guard refused")
			return "", errors.New("unreachable")
		}
		c.uploadCerts = func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
			t.Error("uploadCerts ran although the re-key guard refused")
			return errors.New("unreachable")
		}

		err := createToken("https://cp:6443", true, token.DefaultTokenTTL, c)
		if !errors.Is(err, refusal) {
			t.Fatalf("err = %v, want the re-key guard's refusal", err)
		}
		if len(revoked) != 0 {
			t.Errorf("revoked = %v, want no rollback before a mint", revoked)
		}
		if out.Len() != 0 {
			t.Errorf("printed %q before the command succeeded; the operator now holds a token a refused command minted", out.String())
		}
	})

	// A worker mint DOES publish trust: token.Create republishes
	// cluster-info and the kubelet client CA from
	// local PKI for a worker token too, so a worker mint on a controller the
	// rotation has not reached would roll the published old‖new trust back to
	// old-only. The guard refuses before the mint, and a refusal revokes
	// nothing — no token exists.
	t.Run("a worker token consults the guard too", func(t *testing.T) {
		var out bytes.Buffer
		var revoked []string
		c := okCmd(&out, &revoked)
		c.rekeyGuard = func() error { return refusal }
		c.create = func(_ io.Writer, _ string, _ time.Duration, _ bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
			t.Error("mint/publication ran although the guard refused")
			return "", "", nil, errors.New("unreachable")
		}
		c.newCertKey = func() (string, error) {
			t.Error("newCertKey ran for a worker token")
			return "", errors.New("unreachable")
		}
		c.uploadCerts = func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
			t.Error("uploadCerts ran for a worker token")
			return errors.New("unreachable")
		}

		err := createToken("https://cp:6443", false, token.DefaultTokenTTL, c)
		if !errors.Is(err, refusal) {
			t.Fatalf("err = %v, want the re-key guard's refusal for a worker mint too", err)
		}
		if len(revoked) != 0 {
			t.Errorf("revoked = %v, want no rollback before a mint", revoked)
		}
		if out.Len() != 0 {
			t.Errorf("printed %q for a mint that never happened", out.String())
		}
	})

	// The guard is a refusal gate, not a worker prohibition: with the cluster
	// idle, a worker mint proceeds and prints the join line.
	t.Run("a worker token mints when the guard passes", func(t *testing.T) {
		var out bytes.Buffer
		var revoked []string
		c := okCmd(&out, &revoked)
		c.rekeyGuard = func() error { return nil }

		if err := createToken("https://cp:6443", false, token.DefaultTokenTTL, c); err != nil {
			t.Fatalf("worker token create: %v", err)
		}
		if !strings.Contains(out.String(), "Worker join") {
			t.Errorf("output lacks the worker join line:\n%s", out.String())
		}
		if len(revoked) != 0 {
			t.Errorf("revoked %v on a successful mint", revoked)
		}
	})
}

// TestCertsRotate_SaysWhenNothingIsCloseToExpiry pins the conditional no-op
// path: with fresh PKI, `--if-expiring=30` prints the "nothing to rotate"
// line and returns nil. certs.Rotate returns the verdict as data beside the
// error, and the CLI prints the line from it — the printed line and the
// untouched PKI tree stay the operator-facing half of the contract
// (TestCertsRotate_NoOpVerdictTravelsAsData pins the data half).
func TestCertsRotate_SaysWhenNothingIsCloseToExpiry(t *testing.T) {
	freshPKI(t)
	before := pkiContents(t)

	var err error
	out := captureStdout(t, func() {
		cmd := newCertsRotateCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--if-expiring=30"})
		err = cmd.Execute()
	})
	if err != nil {
		t.Fatalf("rotate returned %v; the conditional no-op path must succeed", err)
	}
	if !strings.Contains(out, "nothing to rotate") {
		t.Errorf("stdout does not say nothing needed rotating:\n%s", out)
	}
	if after := pkiContents(t); !maps.Equal(before, after) {
		t.Errorf("the no-op path rewrote the PKI: %d files before, %d after", len(before), len(after))
	}
}

// TestCertsRotate_NoOpVerdictTravelsAsData pins the caller side of the rotate
// outcome contract on this package: the verdict `stratum certs rotate
// --if-expiring` reports (the pin above holds the printed "nothing to rotate"
// line) must also exist as data on the operation itself, so a caller — the MCP
// tool especially — can tell nothing-to-rotate from a rotation without reading
// the progress text. Same fixture as the printed-line pin, so the two halves
// of the contract stay one behavior. The no-op path returns before the
// kubeconfig rewrite, so version.AdminConf is never read.
func TestCertsRotate_NoOpVerdictTravelsAsData(t *testing.T) {
	freshPKI(t)
	before := pkiContents(t)

	rotated, err := certs.Rotate(io.Discard, certs.Options{IfExpiringDays: 30, Quiet: true})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rotated {
		t.Error("Rotate reported a rotation although no leaf is within 30 days of expiry")
	}
	if after := pkiContents(t); !maps.Equal(before, after) {
		t.Errorf("the no-op run rewrote the PKI: %d files before, %d after", len(before), len(after))
	}
}

// TestRekeySharedRefusesWithARotationStagedOnDisk pins the node-local leg of
// the --rekey-shared guard through the production phase read: rotationPhase
// and carotateKeyStaged keep their production values, and a rotation staged on
// disk (pki.RotateCAMaterial, phase trust-new) is what makes the command
// refuse. Re-keying then would strand the staged key the rotation's follower
// jobs decrypt stratum-certs with. The node-local phase read runs before the
// cluster-wide stratum-carotate check in newCertsRotateCommand, so the
// refusal also proves no apiserver was reached.
func TestRekeySharedRefusesWithARotationStagedOnDisk(t *testing.T) {
	oldFence := rekeyMaintenanceFence
	rekeyMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) { return &k8sutil.MaintenanceLease{Release: func() {}}, nil }
	t.Cleanup(func() { rekeyMaintenanceFence = oldFence })
	freshPKI(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}

	cmd := newCertsRotateCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--rekey-shared", "--yes"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "CA rotation is in flight") {
		t.Fatalf("err = %v, want a refusal while a CA rotation is staged on disk", err)
	}
}

type rekeyFailWriter struct{ accepted int }

func (w rekeyFailWriter) Write(p []byte) (int, error) {
	return min(w.accepted, len(p)), io.ErrClosedPipe
}

func TestRunRekeySharedReportsCompletedRekeyOnOutputFailure(t *testing.T) {
	for _, accepted := range []int{0, 20} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			uploaded, written := false, false
			const path = "/run/stratum/ha-certificate-key-test"
			err := runRekeyShared(rekeyFailWriter{accepted: accepted},
				func() (string, error) { return testCertKey, nil },
				func(string) error { uploaded = true; return nil },
				func(string) (string, error) { written = true; return path, nil })
			if !uploaded || !written {
				t.Fatal("test did not reach output after re-key and file handover")
			}
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("error=%v, want closed pipe", err)
			}
			if !strings.Contains(err.Error(), "was re-keyed") || !strings.Contains(err.Error(), path) {
				t.Fatalf("missing completion state or recovery path: %v", err)
			}
			if strings.Contains(err.Error(), testCertKey) {
				t.Fatal("error exposed the certificate key")
			}
		})
	}
}

// TestLeftoverCarotateWarning pins the `certs check` leftover-rotation flag:
// it fires only when this node is idle and the Secret exists, and a failed
// lookup is reported.
func TestLeftoverCarotateWarning(t *testing.T) {
	staged := func(present bool, err error) func() (bool, error) {
		return func() (bool, error) { return present, err }
	}
	for _, tc := range []struct {
		name   string
		phase  string
		staged func() (bool, error)
		want   string // substring; "" = no output
	}{
		{"idle and present", "idle", staged(true, nil), "kubectl -n kube-system delete secret stratum-carotate"},
		{"idle and absent", "idle", staged(false, nil), ""},
		{"rotation in flight is expected", "sign-new (new signer active)", staged(true, nil), ""},
		{"lookup failure is reported", "idle", staged(false, errors.New("boom")), "could not read from the apiserver: boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := leftoverCarotateWarning(tc.phase, tc.staged)
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Errorf("got %q, want substring %q", got, tc.want)
			}
		})
	}
}

// TestRotateCAWarnsAboutWorkloadRestart: the --cluster and --phase paths each
// print their own restart warning, and the two refusals the CLI makes itself
// (missing --yes, --cluster with --phase) print neither. Refusals from
// internal/certs (an unknown phase, a worker or mid-rotation node) happen after
// the warning has printed, and are not covered here. The --rollback path calls
// certs.RollbackCA directly and is not covered either.
func TestRotateCAWarnsAboutWorkloadRestart(t *testing.T) {
	origCluster, origPhase := rotateCACluster, rotateCA
	t.Cleanup(func() { rotateCACluster, rotateCA = origCluster, origPhase })
	rotateCACluster = func(context.Context, certs.CARotateOptions) error { return nil }
	rotateCA = func(io.Writer, certs.CARotateOptions) error { return nil }

	for _, tc := range []struct {
		name    string
		args    []string
		want    string   // exact warning expected; "" means none
		mention []string // what only this path's warning says
		wantErr bool
	}{
		{"cluster", []string{"--cluster", "--yes"}, caRotateClusterWarning,
			[]string{"Stratum restarts its own add-ons before sign-new", "--restart-workloads", "--skip-workload-check",
				"any node still mounts", "use --phase runs for that window"}, false},
		{"phase", []string{"--phase=sign-new", "--yes"}, caRotatePhaseWarning,
			[]string{"Stratum's own add-ons included", "--skip-workload-check",
				"that control plane only", "finalize on a worker does not check", "scan every node before the first control plane finalizes"}, false},
		{"missing --yes", []string{"--cluster"}, "", nil, true},
		{"both flags", []string{"--cluster", "--phase=sign-new", "--yes"}, "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := newCertsRotateCACommand()
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); (err != nil) != tc.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v", err, tc.wantErr)
			}
			got := out.String()
			if tc.want == "" && strings.Contains(got, "WARNING:") {
				t.Fatalf("refused invocation printed a warning:\n%s", got)
			}
			if tc.want != "" && !strings.HasPrefix(got, tc.want) {
				t.Fatalf("output:\n%s\nwant prefix:\n%s", got, tc.want)
			}
			// The new rotation order: restart before sign-new, nothing after for a
			// client that re-reads its token; the residual is cached tokens and legacy Secrets.
			flat := strings.Join(strings.Fields(tc.want), " ") // the wrapping is not under test
			for _, w := range tc.mention {
				if !strings.Contains(flat, w) {
					t.Errorf("warning missing %q:\n%s", w, tc.want)
				}
			}
			for _, w := range []string{"before sign-new", "needs no restart after sign-new, if it was restarted before it", "caches its token in memory",
				"after sign-new has completed on every control plane and before finalize", "mounted token files", "401", "Cilium", "kubernetes.io/service-account-token"} {
				if tc.want != "" && !strings.Contains(flat, w) {
					t.Errorf("warning missing %q:\n%s", w, tc.want)
				}
			}
			if strings.Contains(tc.want, "second restart") {
				t.Errorf("warning still tells the operator to restart a second time:\n%s", tc.want)
			}
			// A workload skipped by --skip-workload-check was not restarted before sign-new.
			if strings.Contains(flat, "Nothing needs restarting after sign-new") {
				t.Errorf("warning says no restart is needed after sign-new without the precondition:\n%s", tc.want)
			}
		})
	}
}

// TestRotateCAWarningsAreTheOperations pins provenance of the restart
// warnings: the CLI prints the warnings internal/certs owns, not its own copy,
// so the MCP tool returns the same text and the two front ends cannot drift.
// The operations are stubbed, so the printed warning is the whole output.
func TestRotateCAWarningsAreTheOperations(t *testing.T) {
	origCluster, origPhase := rotateCACluster, rotateCA
	t.Cleanup(func() { rotateCACluster, rotateCA = origCluster, origPhase })
	rotateCACluster = func(context.Context, certs.CARotateOptions) error { return nil }
	rotateCA = func(io.Writer, certs.CARotateOptions) error { return nil }

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"cluster", []string{"--cluster", "--yes"}, certs.CARotateClusterWarning},
		{"phase", []string{"--phase=sign-new", "--yes"}, certs.CARotatePhaseWarning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := newCertsRotateCACommand()
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tc.want {
				t.Errorf("output:\n%s\nwant exactly the internal/certs warning:\n%s", got, tc.want)
			}
		})
	}
}

// TestRotateCAWorkloadFlags: --restart-workloads and --skip-workload-check reach
// the options, and the combinations that make no sense are refused before anything runs.
func TestRotateCAWorkloadFlags(t *testing.T) {
	origCluster, origPhase := rotateCACluster, rotateCA
	t.Cleanup(func() { rotateCACluster, rotateCA = origCluster, origPhase })
	var got certs.CARotateOptions
	rotateCACluster = func(_ context.Context, o certs.CARotateOptions) error { got = o; return nil }
	rotateCA = func(_ io.Writer, o certs.CARotateOptions) error { got = o; return nil }

	for _, tc := range []struct {
		name    string
		args    []string
		want    certs.CARotateOptions
		wantErr string
	}{
		{"restart", []string{"--cluster", "--yes", "--restart-workloads"}, certs.CARotateOptions{Cluster: true, RestartWorkloads: true}, ""},
		{"skip, cluster", []string{"--cluster", "--yes", "--skip-workload-check"}, certs.CARotateOptions{Cluster: true, SkipWorkloadCheck: true}, ""},
		{"skip, phase", []string{"--phase=sign-new", "--yes", "--skip-workload-check"}, certs.CARotateOptions{Phase: "sign-new", SkipWorkloadCheck: true}, ""},
		{"skip tokens, cluster", []string{"--cluster", "--yes", "--force", "--skip-token-check"}, certs.CARotateOptions{Cluster: true, Force: true, SkipTokenCheck: true}, ""},
		{"skip tokens, phase", []string{"--phase=finalize", "--yes", "--force", "--skip-token-check"}, certs.CARotateOptions{Phase: "finalize", Force: true, SkipTokenCheck: true}, ""},
		{"skip tokens without --force", []string{"--cluster", "--yes", "--skip-token-check"}, certs.CARotateOptions{}, "goes with --force"},
		{"skip tokens without --force, phase", []string{"--phase=finalize", "--yes", "--skip-token-check"}, certs.CARotateOptions{}, "goes with --force"},
		// What --cluster's node Jobs pass: no --force, and not the operator's flag.
		{"tokens checked", []string{"--phase=finalize", "--yes", "--tokens-checked"}, certs.CARotateOptions{Phase: "finalize", SkipTokenCheck: true}, ""},
		// Not an operator's flag: alone or with --cluster it would skip the token
		// gate without --force.
		{"tokens checked with --cluster", []string{"--cluster", "--yes", "--tokens-checked"}, certs.CARotateOptions{}, "goes with --phase"},
		{"tokens checked with no phase", []string{"--yes", "--tokens-checked"}, certs.CARotateOptions{}, "goes with --phase"},
		{"tokens checked with --cluster and --force", []string{"--cluster", "--yes", "--force", "--tokens-checked"}, certs.CARotateOptions{}, "goes with --phase"},
		{"neither", []string{"--cluster", "--yes"}, certs.CARotateOptions{Cluster: true}, ""},
		{"both", []string{"--cluster", "--yes", "--restart-workloads", "--skip-workload-check"}, certs.CARotateOptions{}, "contradict"},
		{"restart without --cluster", []string{"--phase=sign-new", "--yes", "--restart-workloads"}, certs.CARotateOptions{}, "goes with --cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got = certs.CARotateOptions{}
			cmd := newCertsRotateCACommand()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("options = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// --scan-tokens is the node scan Job: its answer is its exit status, 10 for a
// node with a token signed by the old key, as `upgrade --check`'s is.
func TestRotateCAScanTokens(t *testing.T) {
	orig := scanNodeTokens
	t.Cleanup(func() { scanNodeTokens = orig })
	var gotKid string
	var gotRestart bool
	for _, tc := range []struct {
		name     string
		args     []string
		scan     error
		wantCode int // -1: no exit status, an error or success
		wantErr  string
	}{
		{"clean", []string{"--scan-tokens", "--old-kid", "k", "--yes"}, nil, -1, ""},
		{"stale", []string{"--scan-tokens", "--old-kid", "k", "--restart-kubelet", "--yes"}, certs.ErrStaleTokens, certs.ExitStaleTokens, ""},
		{"a failed scan is not an answer", []string{"--scan-tokens", "--old-kid", "k", "--yes"}, errors.New("unreadable"), -1, "unreadable"},
		{"with a phase", []string{"--scan-tokens", "--phase=finalize", "--yes"}, nil, -1, "stands alone"},
		{"with --cluster", []string{"--scan-tokens", "--cluster", "--yes"}, nil, -1, "stands alone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scanNodeTokens = func(w io.Writer, kid string, restart bool) error { gotKid, gotRestart = kid, restart; return tc.scan }
			cmd := newCertsRotateCACommand()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			var ec *ErrExitCode
			switch {
			case tc.wantCode >= 0:
				if !errors.As(err, &ec) || ec.Code != tc.wantCode {
					t.Fatalf("error = %v, want exit status %d", err, tc.wantCode)
				}
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			}
			if tc.name == "stale" && (gotKid != "k" || !gotRestart) {
				t.Errorf("scan got kid %q, restart %v", gotKid, gotRestart)
			}
		})
	}
}

// stageCertsCheckHost points the paths `stratum certs check` reads at a temp
// host view — a generated PKI, the node state that pins the node IP, and no
// admin.conf — so the command runs offline: the cluster-facing reads (the
// shared-cert key's age, the leftover stratum-carotate flag) fail fast on
// the missing kubeconfig and are reported, not fatal. Same shape as
// rotationEnv in internal/cluster/run_test.go.
func stageCertsCheckHost(t *testing.T) {
	t.Helper()
	origPKI, origKC, origConf, origAdmin, origNet, origSock :=
		version.PKIDir, version.KubeconfigDir, version.K8sConfDir, version.AdminConf, version.NetConfigPath, version.ControlSocket
	t.Cleanup(func() {
		version.PKIDir, version.KubeconfigDir, version.K8sConfDir, version.AdminConf, version.NetConfigPath, version.ControlSocket =
			origPKI, origKC, origConf, origAdmin, origNet, origSock
	})
	base := t.TempDir()
	version.PKIDir = filepath.Join(base, "pki")
	version.KubeconfigDir = filepath.Join(base, "kubeconfig")
	version.K8sConfDir = filepath.Join(base, "kubernetes")
	version.AdminConf = filepath.Join(version.K8sConfDir, "admin.conf")
	version.NetConfigPath = filepath.Join(base, "netconfig") // absent: the IPv4 defaults
	version.ControlSocket = filepath.Join(base, "stratum.sock")

	if err := nodestate.SaveState(&nodestate.NodeState{
		Role: "controller", NodeIP: "192.168.99.12", KubeletNodeIP: "192.168.99.12",
		PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16",
	}); err != nil {
		t.Fatal(err)
	}
	h, err := host.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.GenerateAll(h, "192.168.99.12", nil); err != nil {
		t.Fatal(err)
	}
}

// TestCertCheckReportsTheLastRenewalAttemptAndItsResult checks renewal
// visibility on the reading side: the renewal loop records each
// attempt under the node's state area, and `stratum certs check` is where an
// operator looks — so it must print the LAST attempt's time and its result.
// A loop whose last attempt failed is a node drifting toward expired
// certificates, and one with no attempt recorded is a dead loop that looks
// fine today. The record is planted through its own save path (the same one
// the loop writes), and read through the real command.
func TestCertCheckReportsTheLastRenewalAttemptAndItsResult(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		rec  *certs.RenewalRecord // nil: no attempt recorded yet
		want []string
	}{
		{"a successful attempt", &certs.RenewalRecord{AttemptedAt: at},
			[]string{"last renewal attempt", at.UTC().Format("2006-01-02 15:04"), "ok"}},
		{"a failed attempt", &certs.RenewalRecord{AttemptedAt: at, Result: "rotate: control socket unreachable"},
			[]string{"last renewal attempt", at.UTC().Format("2006-01-02 15:04"), "failed", "rotate: control socket unreachable"}},
		{"no attempt recorded", nil, []string{"no renewal attempt recorded"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stageCertsCheckHost(t)
			if tc.rec != nil {
				if err := certs.SaveRenewalRecord(*tc.rec); err != nil {
					t.Fatal(err)
				}
			}
			// The one cluster-facing seam the command already has, stubbed
			// off; the shared-cert read that stays real fails fast on the
			// missing admin.conf and is reported, not fatal.
			oldStaged := carotateKeyStaged
			t.Cleanup(func() { carotateKeyStaged = oldStaged })
			carotateKeyStaged = func() (bool, error) { return false, nil }

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			origOut := os.Stdout
			os.Stdout = w
			cmd := newCertsCheckCommand()
			cmd.SetOut(w) // either output path — fmt.Printf or OutOrStdout — is captured
			cmd.SetArgs(nil)
			execErr := cmd.Execute()
			os.Stdout = origOut
			w.Close()
			out, _ := io.ReadAll(r)
			r.Close()
			if execErr != nil {
				t.Fatalf("stratum certs check failed: %v", execErr)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(out), want) {
					t.Errorf("certs check output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestClusterCertCommandsPreserveParentCancellation(t *testing.T) {
	oldRotate, oldMigrate := rotateCACluster, migrateEtcdCACluster
	t.Cleanup(func() { rotateCACluster, migrateEtcdCACluster = oldRotate, oldMigrate })
	type traceKey struct{}
	called := false
	run := func(ctx context.Context) error {
		called = true
		if ctx.Value(traceKey{}) != "parent trace" || ctx.Err() != context.Canceled {
			t.Fatalf("command dropped its parent: trace = %v, err = %v", ctx.Value(traceKey{}), ctx.Err())
		}
		return ctx.Err()
	}
	rotateCACluster = func(ctx context.Context, _ certs.CARotateOptions) error { return run(ctx) }
	migrateEtcdCACluster = func(ctx context.Context, _ certs.EtcdCAOptions) error { return run(ctx) }
	for _, name := range []string{"rotate-ca", "migrate-etcd-ca"} {
		t.Run(name, func(t *testing.T) {
			called = false
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), traceKey{}, "parent trace"))
			cancel()
			cmd := newCertsRotateCACommand()
			if name == "migrate-etcd-ca" {
				cmd = newCertsMigrateEtcdCACommand()
			}
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"--cluster", "--yes"})
			err := cmd.ExecuteContext(ctx)
			var exit *ErrExitCode
			if !called || !errors.Is(err, context.Canceled) || errors.As(err, &exit) || interrupt.CleanStop(err) {
				t.Fatalf("called = %v, error = %v; host cancellation must remain a failure", called, err)
			}
		})
	}
}

func TestClusterCertCommandsSignalExitPreservesFailuresAndGuidance(t *testing.T) {
	if name := os.Getenv("STRATUM_CERT_CLI_COMMAND"); name != "" {
		n, err := strconv.Atoi(os.Getenv("STRATUM_CERT_CLI_SIGNAL"))
		if err != nil {
			t.Fatal(err)
		}
		failure := errors.New("API unavailable")
		joined := os.Getenv("STRATUM_CERT_CLI_FAILURE") == "1"
		const guidance = "halted in trust-new; run stratum certs check; --rollback --yes"
		run := func(ctx context.Context) error {
			// API and maintenance calls derive child contexts. They return the
			// plain cancellation while the operation retains its signal cause.
			child, cancel := context.WithTimeout(ctx, time.Hour)
			defer cancel()
			if err := syscall.Kill(syscall.Getpid(), syscall.Signal(n)); err != nil {
				t.Fatal(err)
			}
			<-child.Done()
			err := child.Err()
			if joined {
				err = errors.Join(err, failure)
			}
			return fmt.Errorf("%s: %w", guidance, err)
		}
		oldRotate, oldMigrate := rotateCACluster, migrateEtcdCACluster
		defer func() { rotateCACluster, migrateEtcdCACluster = oldRotate, oldMigrate }()
		rotateCACluster = func(ctx context.Context, _ certs.CARotateOptions) error { return run(ctx) }
		migrateEtcdCACluster = func(ctx context.Context, _ certs.EtcdCAOptions) error { return run(ctx) }
		cmd := newCertsRotateCACommand()
		if name == "migrate-etcd-ca" {
			cmd = newCertsMigrateEtcdCACommand()
		}
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"--cluster", "--yes"})
		err = cmd.Execute()
		var exit *ErrExitCode
		if joined {
			if !errors.Is(err, failure) || errors.As(err, &exit) {
				t.Fatalf("joined API failure was swallowed: %v", err)
			}
		} else if !errors.As(err, &exit) || exit.Code != 0 || !errors.Is(err, context.Canceled) || !strings.Contains(out.String(), guidance) {
			t.Fatalf("signal stop lost its clean exit or recovery guidance: error = %v, output = %q", err, out.String())
		}
		return
	}
	for _, name := range []string{"rotate-ca", "migrate-etcd-ca"} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
			for _, joined := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/joined=%v", name, sig, joined), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClusterCertCommandsSignalExitPreservesFailuresAndGuidance$")
					failureMode := "0"
					if joined {
						failureMode = "1"
					}
					cmd.Env = append(os.Environ(), "STRATUM_CERT_CLI_COMMAND="+name, "STRATUM_CERT_CLI_SIGNAL="+strconv.Itoa(int(sig)), "STRATUM_CERT_CLI_FAILURE="+failureMode)
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("signal child failed: %v\n%s", err, out)
					}
				})
			}
		}
	}
}

func TestExplicitRekeyHoldsMaintenanceFenceThroughHandover(t *testing.T) {
	for _, failAt := range []string{"", "fence", "guard", "mint", "upload", "key file", "handover"} {
		t.Run("failure="+failAt, func(t *testing.T) {
			oldFence, oldGuard := rekeyMaintenanceFence, rekeyGuard
			oldMint, oldUpload, oldWrite := rekeyNewCertKey, rekeySharedCerts, rekeyWriteKeyFile
			t.Cleanup(func() {
				rekeyMaintenanceFence, rekeyGuard = oldFence, oldGuard
				rekeyNewCertKey, rekeySharedCerts, rekeyWriteKeyFile = oldMint, oldUpload, oldWrite
			})
			var order []string
			boom := errors.New("injected " + failAt + " failure")
			step := func(name string) error {
				order = append(order, name)
				if name == failAt {
					return boom
				}
				return nil
			}
			rekeyMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
				if err := step("fence"); err != nil {
					return nil, err
				}
				return &k8sutil.MaintenanceLease{Release: func() { order = append(order, "release") }}, nil
			}
			rekeyGuard = func() error { return step("guard") }
			rekeyNewCertKey = func() (string, error) { return testCertKey, step("mint") }
			rekeySharedCerts = func(_ string, _ func(context.Context) error) error { return step("upload") }
			rekeyWriteKeyFile = func(string) (string, error) { return "/run/key-test", step("key file") }
			cmd := newCertsRotateCommand()
			cmd.SetArgs([]string{"--rekey-shared", "--yes"})
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetOut(handoverWriter(func(p []byte) (int, error) {
				if err := step("handover"); err != nil {
					return 0, err
				}
				return len(p), nil
			}))
			err := cmd.Execute()
			want := []string{"fence", "guard", "mint", "upload", "key file", "handover"}
			if failAt != "" {
				want = want[:slices.Index(want, failAt)+1]
				if !errors.Is(err, boom) {
					t.Errorf("error = %v, want %v", err, boom)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if failAt != "fence" {
				want = append(want, "release")
			}
			if !slices.Equal(order, want) {
				t.Errorf("effects = %v, want %v", order, want)
			}
		})
	}
}

func TestClusterCertsReportOnAWorkerSkipsBothClusterLookups(t *testing.T) {
	keySetAt := func() (time.Time, bool, error) {
		t.Error("a worker read stratum-certs")
		return time.Time{}, false, errors.New("load admin kubeconfig: no such file")
	}
	staged := func() (bool, error) {
		t.Error("a worker read stratum-carotate")
		return false, errors.New("load admin kubeconfig: no such file")
	}
	got := clusterCertsReport(true, keySetAt, func() string { return "idle" }, staged)
	if strings.Contains(got, "apiserver") || strings.Contains(got, "admin") {
		t.Errorf("worker output still reads like a failure: %q", got)
	}
	if !strings.Contains(got, "control planes only") {
		t.Errorf("worker output does not say where the checks run: %q", got)
	}
}

func TestClusterCertsReportOnAControllerKeepsBothLookups(t *testing.T) {
	idle := func() string { return "idle" }
	for _, tc := range []struct {
		name     string
		keySetAt func() (time.Time, bool, error)
		staged   func() (bool, error)
		want     []string
	}{
		{"both unreadable",
			func() (time.Time, bool, error) { return time.Time{}, false, errors.New("dial tcp: refused") },
			func() (bool, error) { return false, errors.New("dial tcp: refused") },
			[]string{"shared certs (stratum-certs): could not read from the apiserver: dial tcp: refused",
				"transient CA-rotation key (stratum-carotate): could not read from the apiserver: dial tcp: refused"}},
		{"key set, nothing staged",
			func() (time.Time, bool, error) { return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), true, nil },
			func() (bool, error) { return false, nil },
			[]string{"certificate-key last set 2026-01-02"}},
		{"key absent, leftover staged",
			func() (time.Time, bool, error) { return time.Time{}, false, nil },
			func() (bool, error) { return true, nil },
			[]string{"shared certs (stratum-certs): not present", "kubectl -n kube-system delete secret stratum-carotate"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := clusterCertsReport(false, tc.keySetAt, idle, tc.staged)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in %q", w, got)
				}
			}
			if strings.Contains(got, "control planes only") {
				t.Errorf("a controller was told to run the check elsewhere: %q", got)
			}
		})
	}
}

func TestIsWorkerNodeOnlyWhenTheAPIServerManifestIsAbsent(t *testing.T) {
	old := version.K8sConfDir
	t.Cleanup(func() { version.K8sConfDir = old })

	version.K8sConfDir = t.TempDir()
	if !isWorkerNode() {
		t.Error("no apiserver manifest: want a worker")
	}

	if err := os.MkdirAll(filepath.Join(version.K8sConfDir, "manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version.K8sConfDir, "manifests", "kube-apiserver.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if isWorkerNode() {
		t.Error("apiserver manifest present: want a controller")
	}

	notADir := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	version.K8sConfDir = notADir
	if isWorkerNode() {
		t.Error("manifest unreadable (stat fails with ENOTDIR): want a controller, so the error still shows")
	}
}

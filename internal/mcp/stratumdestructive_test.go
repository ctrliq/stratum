package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
	"github.com/ctrliq/stratum/internal/upgrade"
	"github.com/ctrliq/stratum/internal/version"
)

// TestCheckVIP covers the validation in front of ha_repoint.
func TestCheckVIP(t *testing.T) {
	for _, ok := range []string{
		"10.0.0.1",
		"192.168.10.254",
		"fd00::1",
		"::1",
		"api.example.com",
		"api.example.com.",
		"vip-1",
	} {
		t.Run("accepts "+ok, func(t *testing.T) {
			if err := checkVIP(ok); err != nil {
				t.Errorf("checkVIP(%q) = %v, want nil", ok, err)
			}
		})
	}

	for _, bad := range []struct{ vip, why string }{
		{"10.0.0.1:6443", "a port would produce https://10.0.0.1:6443:6443"},
		{"evil.example.com/steal", "a path moves the API path"},
		{"10.0.0.1@evil.example.com", "userinfo makes the attacker's host the authority"},
		{"10.0.0.1#", "a fragment comments the port out"},
		{"10.0.0.1?x=1", "a query is not part of an authority"},
		{"https://10.0.0.1", "a scheme is already supplied"},
		{"10.0.0.1 evil", "whitespace"},
		{"api..example.com", "an empty DNS label"},
		{"api_1.example.com", "an underscore is not a DNS label character"},
	} {
		t.Run("refuses "+bad.vip, func(t *testing.T) {
			err := checkVIP(bad.vip)
			if err == nil {
				t.Fatalf("checkVIP(%q) = nil, want an error: %s", bad.vip, bad.why)
			}
			if !strings.Contains(err.Error(), bad.vip) {
				t.Errorf("error %q does not quote the offending value", err)
			}
		})
	}
}

// TestResolveSnapshotPath covers the path handling snapshot_restore and rollback share.
func TestResolveSnapshotPath(t *testing.T) {
	t.Run("a bare name resolves inside the snapshot directory", func(t *testing.T) {
		old := snapshotDir
		t.Cleanup(func() { snapshotDir = old })
		snapshotDir = func() string { return "/var/backups/etcd" }
		got, err := resolveSnapshotPath("etcd-cp1-20250101.db")
		if err != nil {
			t.Fatalf("resolveSnapshotPath: %v", err)
		}
		want := filepath.Join("/var/backups/etcd", "etcd-cp1-20250101.db")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// snapshot_list's names must resolve where `stratum snapshot restore NAME` resolves them.
	t.Run("a bare name resolves inside a custom spec.snapshots.dir", func(t *testing.T) {
		old := snapshotDir
		t.Cleanup(func() { snapshotDir = old })
		snapshotDir = func() string { return "/srv/etcd-snaps" }
		got, err := resolveSnapshotPath("etcd-cp1-20250101.db")
		if err != nil {
			t.Fatalf("resolveSnapshotPath: %v", err)
		}
		if want := "/srv/etcd-snaps/etcd-cp1-20250101.db"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("an absolute path is taken as given", func(t *testing.T) {
		got, err := resolveSnapshotPath("/root/rescue.db")
		if err != nil {
			t.Fatalf("resolveSnapshotPath: %v", err)
		}
		if got != "/root/rescue.db" {
			t.Errorf("got %q", got)
		}
	})

	// A name from snapshot_list must not become a file outside it.
	for _, bad := range []string{
		"../../etc/kubernetes/pki/ca.key",
		"..",
		"sub/dir.db",
		"./local.db",
	} {
		t.Run("refuses "+bad, func(t *testing.T) {
			got, err := resolveSnapshotPath(bad)
			if err == nil {
				t.Fatalf("resolveSnapshotPath(%q) = %q, want an error", bad, got)
			}
		})
	}
}

// Only the host reaches ha.RejoinEtcd, so a URL carrying anything else names an uncontacted target.
func TestSnapshotRejoinServerValidation(t *testing.T) {
	handler := SnapshotRejoinHandler()

	for _, bad := range []string{
		"http://10.0.0.1:6443",
		"https://user:pw@10.0.0.1:6443",
		"https://10.0.0.1:6443/steal",
		"https://10.0.0.1:6443?x=1",
		"https://10.0.0.1:6443#frag",
		"not-a-url",
	} {
		t.Run("refuses "+bad, func(t *testing.T) {
			_, _, err := handler(t.Context(), nil, SnapshotRejoinIn{Server: bad})
			if err == nil {
				t.Fatalf("SnapshotRejoinIn{Server: %q} was accepted", bad)
			}
			if !strings.Contains(err.Error(), bad) {
				t.Errorf("error %q does not quote the offending value", err)
			}
		})
	}

	// Off a Stratum node it stops at the host check, proving validation did not reject it.
	t.Run("accepts a bare https host and port", func(t *testing.T) {
		// The probe points away from the host's Stratum dirs, so the host check answers everywhere.
		notStratumHost(t)
		_, _, err := handler(t.Context(), nil, SnapshotRejoinIn{Server: "https://10.0.0.1:6443"})
		if err != nil && !errors.Is(err, errNotStratumHost) {
			t.Errorf("a valid server was rejected by validation: %v", err)
		}
	})
}

// snapshotRestore runs the restore tool with the restore replaced by restoreErr.
func snapshotRestore(t *testing.T, restoreErr error) (destructiveOut, error) {
	t.Helper()
	fakeStratumHost(t)
	withDestructiveDeps(t, func(deps *destructiveDeps) {
		deps.restoreLocalSeed = func(_ io.Writer, _ string) error { return restoreErr }
	})
	_, out, err := SnapshotRestoreHandler()(context.Background(), nil, SnapshotRestoreIn{Snapshot: "/var/backups/etcd/x.db"})
	return out, err
}

// flat folds whitespace runs into one space, so assertions do not depend on line wrapping.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// Backtick segments are the commands; [k]-style brackets are ignored, so either spelling counts.
func pkillNames(text, name string) bool {
	for _, seg := range strings.Split(text, "`") {
		if !strings.Contains(seg, "sudo pkill") {
			continue
		}
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "[", ""), "]", "")
		if strings.Contains(seg, name) {
			return true
		}
	}
	return false
}

// restartCommandsInGuidance asserts the restart commands word for word, kube-vip included.
func restartCommandsInGuidance(t *testing.T, text string) {
	t.Helper()
	for _, want := range []string{
		`sudo pkill -x kubelet`,
		`sudo pkill -TERM -f '^([^[:space:]]*/)?[k]ube-controller-manager([[:space:]]|$)'`,
		`sudo pkill -TERM -f '^([^[:space:]]*/)?[k]ube-scheduler([[:space:]]|$)'`,
		// the operator's, which names its subcommand and Stratum's flags, not its executable
		"sudo pkill -TERM -f '^[^[:space:]]+ operator( [^[:space:]]+)* --kubeconfig (/var/lib/stratum/kubeconfig/super-admin\\.conf|/var/lib/stratum/kubeconfig/admin\\.conf)",
		// the in-cluster workloads and the third-party controllers the restore does not restart
		"stratum kubectl -n kube-system rollout restart ds/cilium deploy/cilium-operator deploy/coredns",
		"stratum kubectl -n local-path-storage rollout restart deploy/local-path-provisioner",
		"stratum kubectl -n <namespace> rollout restart deploy/<name>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("guidance missing the restart command %q:\n%s", want, text)
		}
	}
	// Pinned by presence only: the exact pattern is the restart set's to derive and its test to pin.
	if !pkillNames(text, "kube-vip") {
		t.Errorf("guidance has no pkill for kube-vip:\n%s", text)
	}
}

func TestSnapshotRestoreOutcomeSuccess(t *testing.T) {
	out, err := snapshotRestore(t, nil)
	if err != nil || !out.Done {
		t.Fatalf("done=%v err=%v", out.Done, err)
	}
	for _, want := range []string{"This node's kubelet", "were restarted", "snapshot_rejoin", "--yes --force", "kubelet on every other node",
		"repeat the restarts", "after every one has rejoined",
		"rollout restart ds/cilium deploy/cilium-operator deploy/coredns", "CoreDNS", "local-path provisioner",
		"kube-vip"} {
		if !strings.Contains(out.Next, want) {
			t.Errorf("next missing %q: %s", want, out.Next)
		}
	}
	restartCommandsInGuidance(t, out.Next)
}

// TestSnapshotRestoreNextIsRenderedFromTheRestartSet: a new component reaches the answer.
func TestSnapshotRestoreNextIsRenderedFromTheRestartSet(t *testing.T) {
	old := restoreRestartSet
	t.Cleanup(func() { restoreRestartSet = old })
	restoreRestartSet = func() etcdsnap.RestartSet {
		return append(etcdsnap.RestoreRestartSet(),
			etcdsnap.RestartEntry{Component: "synthetic-agent", How: "supervisor", Command: "sudo pkill -x synthetic-agent", Repeat: true},
			etcdsnap.RestartEntry{Component: "synthetic workload", How: "rollout", Command: "-n synthetic-ns rollout restart deploy/synthetic", Repeat: false},
		)
	}

	out, err := snapshotRestore(t, nil)
	if err != nil || !out.Done {
		t.Fatalf("done=%v err=%v", out.Done, err)
	}
	next := flat(out.Next)
	for _, want := range []string{
		"sudo pkill -x synthetic-agent",
		"-n synthetic-ns rollout restart deploy/synthetic",
		"synthetic-agent",
		"synthetic workload",
	} {
		if !strings.Contains(next, want) {
			t.Errorf("next missing %q from the restart set: %s", want, out.Next)
		}
	}

	// The partial outcome's recovery steps render from the same set.
	_, err = snapshotRestore(t, fmt.Errorf("%w: synthetic-agent", etcdsnap.ErrWatchersNotRestarted))
	if !errors.Is(err, etcdsnap.ErrWatchersNotRestarted) {
		t.Fatalf("err = %v", err)
	}
	msg := flat(err.Error())
	for _, want := range []string{
		"sudo pkill -x synthetic-agent",
		"-n synthetic-ns rollout restart deploy/synthetic",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("a partial restore's steps missing %q:\n%v", want, err)
		}
	}
}

// TestSnapshotRestoreOutcomePartial: the call still fails and Done stays false.
func TestSnapshotRestoreOutcomePartial(t *testing.T) {
	out, err := snapshotRestore(t, fmt.Errorf("%w: kubelet", etcdsnap.ErrWatchersNotRestarted))
	if !errors.Is(err, etcdsnap.ErrWatchersNotRestarted) || out.Done {
		t.Fatalf("done=%v err=%v", out.Done, err)
	}
	for _, want := range []string{"do not run the restore again", "snapshot_rejoin", "--yes --force",
		"--server https://127.0.0.1:6443",
		"repeat the restarts", "rollout restart ds/cilium deploy/cilium-operator deploy/coredns",
		"the in-cluster workloads on this node"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	restartCommandsInGuidance(t, err.Error())
	if out.Next != "" {
		t.Errorf("next = %q; a partial restore must not claim the usual outcome", out.Next)
	}
}

func TestSnapshotRestoreOutcomeOrdinaryFailure(t *testing.T) {
	out, err := snapshotRestore(t, errors.New("etcdutl exploded"))
	if err == nil || !strings.Contains(err.Error(), "etcdutl exploded") || out.Done || out.Next != "" {
		t.Fatalf("done=%v next=%q err=%v", out.Done, out.Next, err)
	}
	if strings.Contains(err.Error(), "restore again") || strings.Contains(err.Error(), "is restored") {
		t.Errorf("a failed restore claims the snapshot is restored: %v", err)
	}
}

// rollbackWith runs the rollback tool with RollbackNode replaced by err.
func rollbackWith(t *testing.T, in RollbackIn, err error) (destructiveOut, error) {
	t.Helper()
	fakeStratumHost(t)
	withDestructiveDeps(t, func(deps *destructiveDeps) {
		deps.rollbackNode = func(io.Writer, upgrade.RollbackOptions) error { return err }
	})
	_, out, rerr := RollbackHandler()(context.Background(), nil, in)
	return out, rerr
}

// TestRollbackResultCarriesTheOtherNodeSteps: what is left on the other nodes after a restore.
func TestRollbackResultCarriesTheOtherNodeSteps(t *testing.T) {
	out, err := rollbackWith(t, RollbackIn{EtcdSnapshot: "/var/backups/etcd/x.db"}, nil)
	if err != nil || !out.Done {
		t.Fatalf("done=%v err=%v", out.Done, err)
	}
	for _, want := range []string{"every change made after that snapshot is gone", "Still to do:", "sudo pkill -x kubelet",
		"stratum kubectl -n <namespace> rollout restart deploy/<name>"} {
		if !strings.Contains(out.Next, want) {
			t.Errorf("next lacks %q:\n%s", want, out.Next)
		}
	}
	for _, bad := range []string{"Rejoin every OTHER control plane", "once EVERY one has rejoined"} {
		if strings.Contains(out.Next, bad) {
			t.Errorf("next has %q, but a rollback has no control planes to rejoin", bad)
		}
	}
}

// A rollback that did not restore etcd has no other-node steps to give.
func TestRollbackWithoutASnapshotHasNoOtherNodeSteps(t *testing.T) {
	out, err := rollbackWith(t, RollbackIn{}, nil)
	if err != nil || out.Next != "" {
		t.Fatalf("err=%v next=%q", err, out.Next)
	}
}

func TestRollbackFailureStaysAFailure(t *testing.T) {
	out, err := rollbackWith(t, RollbackIn{EtcdSnapshot: "/var/backups/etcd/x.db"}, errors.New("etcdutl exploded"))
	if err == nil || out.Done || out.Next != "" {
		t.Fatalf("done=%v next=%q err=%v", out.Done, out.Next, err)
	}
}

// TestSnapshotRestoreNextDoesNotPointAtAToolThatDeadEnds: snapshot_rejoin cannot pass --force.
func TestSnapshotRestoreNextDoesNotPointAtAToolThatDeadEnds(t *testing.T) {
	for _, want := range []string{
		"stratum snapshot rejoin",
		"--yes --force",
		"snapshot_rejoin tool cannot",
		"quorum check",
	} {
		if !strings.Contains(snapshotRestoreNext, want) {
			t.Errorf("snapshotRestoreNext lacks %q:\n%s", want, snapshotRestoreNext)
		}
	}
	if strings.Contains(snapshotRestoreNext, "brought back with snapshot_rejoin") {
		t.Error("snapshotRestoreNext still recommends snapshot_rejoin as the way back")
	}
}

// Cluster certificate tools must cancel before loading admin credentials or starting a phase.
func TestClusterCAToolsPreserveRequestCancellation(t *testing.T) {
	fakeStratumHost(t)
	oldAdmin := version.AdminConf
	version.AdminConf = filepath.Join(t.TempDir(), "missing-admin.conf")
	t.Cleanup(func() { version.AdminConf = oldAdmin })
	for _, tc := range []struct {
		name string
		run  func(context.Context) (bool, error)
	}{
		{"rotate-ca", func(ctx context.Context) (bool, error) {
			_, out, err := CertsRotateCAHandler()(ctx, nil, CertsRotateCAIn{Cluster: true})
			return out.Done, err
		}},
		{"migrate-etcd-ca", func(ctx context.Context) (bool, error) {
			_, out, err := CertsMigrateEtcdCAHandler()(ctx, nil, CertsMigrateEtcdCAIn{Cluster: true, Phase: "trust-both"})
			return out.Done, err
		}},
		{"migrate-kubelet-client-ca", func(ctx context.Context) (bool, error) {
			_, out, err := CertsMigrateKubeletClientCAHandler()(ctx, nil, CertsMigrateKubeletClientCAIn{Cluster: true})
			return out.Done, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, deadline := range []bool{false, true} {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				want := context.Canceled
				if deadline {
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
					defer cancel()
					want = context.DeadlineExceeded
				}
				if done, err := tc.run(ctx); done || !errors.Is(err, want) {
					t.Errorf("deadline=%v: Done=%v, err=%v; want no completion and %v before loading admin credentials", deadline, done, err, want)
				}
			}
		})
	}
}

// certs_rotate_ca refuses the combinations the CLI refuses, before it touches the host.
func TestCertsRotateCARefusesWhatTheCLIRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   CertsRotateCAIn
		want string
	}{
		{"both workload flags", CertsRotateCAIn{Cluster: true, RestartWorkloads: true, SkipWorkloadCheck: true}, "contradict each other"},
		{"skip the token check without force", CertsRotateCAIn{Cluster: true, SkipTokenCheck: true}, "goes with --force"},
		{"restart without cluster", CertsRotateCAIn{Phase: "sign-new", RestartWorkloads: true}, "goes with --cluster"},
		// The refusal is the shared validator's, in the CLI's flag names.
		{"cluster with a phase", CertsRotateCAIn{Cluster: true, Phase: "sign-new", SkipWorkloadCheck: true}, "use either --cluster (all phases) or --phase (one node), not both"},
		{"neither", CertsRotateCAIn{}, "phase is required"},
		{"unknown phase", CertsRotateCAIn{Phase: "drop-all"}, "unknown phase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := CertsRotateCAHandler()(context.Background(), nil, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestCATools_RefuseABadPhaseBeforeTheHostCheck: the refusal must also name every phase.
func TestCATools_RefuseABadPhaseBeforeTheHostCheck(t *testing.T) {
	notStratumHost(t)

	// Pinned as literals: a phase list that drops or renames a phase must fail here, not pass.
	rotate := []string{"trust-new", "sign-new", "finalize"}
	migrate := []string{"trust-both", "issue-new", "drop-root", "rollback"}
	kubelet := []string{"trust-both", "issue-new", "drop-root", "re-add-root", "reissue-root", "remove-ca"}

	for _, tc := range []struct {
		name   string
		call   func() error
		phases []string
	}{
		{"certs_rotate_ca without a phase", func() error {
			_, _, err := CertsRotateCAHandler()(t.Context(), nil, CertsRotateCAIn{})
			return err
		}, rotate},
		{"certs_rotate_ca with an unknown phase", func() error {
			_, _, err := CertsRotateCAHandler()(t.Context(), nil, CertsRotateCAIn{Phase: "bogus"})
			return err
		}, rotate},
		{"certs_migrate_etcd_ca without a phase", func() error {
			_, _, err := CertsMigrateEtcdCAHandler()(t.Context(), nil, CertsMigrateEtcdCAIn{})
			return err
		}, migrate},
		{"certs_migrate_etcd_ca with an unknown phase", func() error {
			_, _, err := CertsMigrateEtcdCAHandler()(t.Context(), nil, CertsMigrateEtcdCAIn{Phase: "bogus"})
			return err
		}, migrate},
		{"certs_migrate_kubelet_client_ca without cluster or a phase", func() error {
			_, _, err := CertsMigrateKubeletClientCAHandler()(t.Context(), nil, CertsMigrateKubeletClientCAIn{})
			return err
		}, nil},
		{"certs_migrate_kubelet_client_ca with an unknown phase", func() error {
			_, _, err := CertsMigrateKubeletClientCAHandler()(t.Context(), nil, CertsMigrateKubeletClientCAIn{Phase: "bogus"})
			return err
		}, kubelet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("a missing or unknown phase was accepted")
			}
			if errors.Is(err, errNotStratumHost) {
				t.Fatalf("the host check answered first, so the phase check has moved inside run: %v", err)
			}
			for _, phase := range tc.phases {
				if !strings.Contains(err.Error(), phase) {
					t.Errorf("error does not name the phase %q:\n%v", phase, err)
				}
			}
		})
	}
}

// TestHARepointTool_RefusesAMalformedVIPBeforeRunning: it keeps URL structure out of kubeconfigs.
func TestHARepointTool_RefusesAMalformedVIPBeforeRunning(t *testing.T) {
	notStratumHost(t)

	// Every value TestCheckVIP refuses, driven through the tool: the same check must run first.
	for _, vip := range []string{
		"10.0.0.1:6443",
		"evil.example.com/steal",
		"10.0.0.1@evil.example.com",
		"10.0.0.1#",
		"10.0.0.1?x=1",
		"https://10.0.0.1",
		"10.0.0.1 evil",
		"api..example.com",
		"api_1.example.com",
	} {
		t.Run("refuses "+vip, func(t *testing.T) {
			_, _, err := HARepointHandler(nil)(t.Context(), nil, HARepointIn{VIP: vip})
			if err == nil {
				t.Fatalf("vip %q was accepted", vip)
			}
			if errors.Is(err, errNotStratumHost) {
				t.Fatalf("the host check answered first, so the VIP check has moved inside run: %v", err)
			}
			if !strings.Contains(err.Error(), vip) {
				t.Errorf("error %q does not quote the offending value", err)
			}
		})
	}
}

// TestRun_CleanCallIsDoneWithItsProgress pins run's done/progress reporting in both outcomes.
func TestRun_CleanCallIsDoneWithItsProgress(t *testing.T) {
	fakeStratumHost(t)

	t.Run("a clean call is done with its progress", func(t *testing.T) {
		var out destructiveOut
		var err error
		_ = captureStderrDiscard(func() {
			out, err = run("act", func(w io.Writer) ([]string, error) {
				fmt.Fprintln(w, "line one")
				fmt.Fprintln(w, "line two")
				return nil, nil
			})
		})
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
		if out.Action != "act" || !out.Done || out.Progress != "line one\nline two" {
			t.Fatalf("out = %+v, want action %q, done, and the two progress lines trimmed", out, "act")
		}
	})

	t.Run("a failed call keeps its progress beside the error", func(t *testing.T) {
		boom := errors.New("boom")
		var out destructiveOut
		var err error
		_ = captureStderrDiscard(func() {
			out, err = run("act", func(w io.Writer) ([]string, error) {
				fmt.Fprintln(w, "halfway done")
				return nil, boom
			})
		})
		if err == nil {
			t.Fatal("run swallowed the operation's error")
		}
		if out.Done {
			t.Error("a failed call reported done")
		}
		if !errors.Is(err, boom) {
			t.Errorf("error does not carry the cause: %v", err)
		}
		if !strings.Contains(err.Error(), "halfway done") {
			t.Errorf("error loses the progress:\n%v", err)
		}
	})
}

// TestRun_WarningsMakeAPartialResult: warnings travel as data, not only as captured text.
func TestRun_WarningsMakeAPartialResult(t *testing.T) {
	fakeStratumHost(t)

	t.Run("warnings beside a nil error are a partial result", func(t *testing.T) {
		warns := []string{"WARN: restart kubelet: no such process (this node may go NotReady; check the kubelet log)"}
		var out destructiveOut
		var err error
		_ = captureStderrDiscard(func() {
			out, err = run("act", func(w io.Writer) ([]string, error) {
				fmt.Fprintln(w, "the work itself finished")
				return warns, nil
			})
		})
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
		if out.Action != "act" {
			t.Errorf("out.Action = %q, want %q", out.Action, "act")
		}
		if out.Done {
			t.Error("a call whose operation reported warnings came back done")
		}
		if !slices.Equal(out.Warnings, warns) {
			t.Errorf("out.Warnings = %v, want the operation's warnings carried as data", out.Warnings)
		}
		if out.Progress != "the work itself finished" {
			t.Errorf("out.Progress = %q, want the printed progress kept", out.Progress)
		}
	})

	// A warning that exists only in captured text is invisible.
	t.Run("the warnings travel as data, not only as captured text", func(t *testing.T) {
		var out destructiveOut
		var err error
		_ = captureStderrDiscard(func() {
			out, err = run("act", func(io.Writer) ([]string, error) {
				return []string{"WARN: restart kubelet: no such process"}, nil
			})
		})
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
		if len(out.Warnings) != 1 {
			t.Fatalf("out.Warnings = %v, want the warning as data", out.Warnings)
		}
		if out.Progress != "" {
			t.Errorf("out.Progress = %q, want empty for an operation that printed nothing", out.Progress)
		}
	})

	t.Run("a clean call is still done with no warnings", func(t *testing.T) {
		var out destructiveOut
		var err error
		_ = captureStderrDiscard(func() {
			out, err = run("act", func(io.Writer) ([]string, error) { return nil, nil })
		})
		if err != nil || !out.Done {
			t.Fatalf("done=%v err=%v", out.Done, err)
		}
		if out.Warnings != nil {
			t.Errorf("out.Warnings = %v, want none on a clean call", out.Warnings)
		}
	})

	// A failed call still files its warnings beside the error.
	t.Run("a failed call is not done and keeps its warnings beside the error", func(t *testing.T) {
		boom := errors.New("boom")
		var out destructiveOut
		var err error
		_ = captureStderrDiscard(func() {
			out, err = run("act", func(w io.Writer) ([]string, error) {
				fmt.Fprintln(w, "halfway done")
				return []string{"WARN: restart kubelet: timed out"}, boom
			})
		})
		if err == nil {
			t.Fatal("run swallowed the operation's error")
		}
		if out.Done {
			t.Error("a failed call reported done")
		}
		if !errors.Is(err, boom) {
			t.Errorf("error does not carry the cause: %v", err)
		}
		if !slices.Equal(out.Warnings, []string{"WARN: restart kubelet: timed out"}) {
			t.Errorf("out.Warnings = %v, want the warnings the operation returned before failing", out.Warnings)
		}
	})
}

// TestRun_ProgressTravelsThroughTheWriter pins the writer contract; nothing captures a stream.
func TestRun_ProgressTravelsThroughTheWriter(t *testing.T) {
	fakeStratumHost(t)

	t.Run("the operation's writer output is the progress", func(t *testing.T) {
		var out destructiveOut
		var err error
		_ = captureStderrDiscard(func() {
			out, err = run("act", func(w io.Writer) ([]string, error) {
				fmt.Fprintln(w, "line one")
				fmt.Fprintln(w, "line two")
				return nil, nil
			})
		})
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
		if out.Progress != "line one\nline two" {
			t.Errorf("out.Progress = %q, want the two lines the operation wrote through its writer", out.Progress)
		}
	})

	// On the stdio transport stdout is the JSON-RPC stream, so nothing may swap os.Stdout.
	t.Run("the operation runs with the process's real stdout", func(t *testing.T) {
		var inner *os.File
		_ = captureStderrDiscard(func() {
			_, _ = run("act", func(io.Writer) ([]string, error) {
				inner = os.Stdout
				return nil, nil
			})
		})
		if inner == nil {
			t.Fatal("the operation never ran")
		}
		if inner != os.Stdout {
			t.Error("run redirected os.Stdout for the operation; the operations that take a writer run with the process's own stdout")
		}
	})
}

// TestUninstallTool_TwoCallsNeverOverlap pins opMu as the destructive tools' sole exclusion.
func TestUninstallTool_TwoCallsNeverOverlap(t *testing.T) {
	fakeStratumHost(t)

	var inside, overlapped atomic.Bool
	var calls atomic.Int32
	entered := make(chan int, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var handlers sync.WaitGroup
	// A failed assertion must unblock and join both handlers, or the lock strands later tests.
	defer func() {
		releaseOnce.Do(func() { close(release) })
		handlers.Wait()
	}()

	withDestructiveDeps(t, func(deps *destructiveDeps) {
		deps.uninstallRun = func(io.Writer, bool) error {
			n := int(calls.Add(1))
			if inside.Swap(true) {
				overlapped.Store(true) // a second body started while the first was inside
			}
			defer inside.Store(false)
			entered <- n
			if n == 1 {
				<-release // the barrier: the first body leaves only when the test says so
			}
			return nil
		}
	})

	type result struct {
		out destructiveOut
		err error
	}
	handler := UninstallHandler()
	call := func() result {
		_, out, err := handler(context.Background(), nil, UninstallIn{})
		return result{out, err}
	}
	done := make(chan result, 2)

	launch := func() {
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			done <- call()
		}()
	}
	launch()
	if n := <-entered; n != 1 {
		t.Fatalf("the first call entered the body as call %d", n)
	}

	launch()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	retry := time.NewTicker(time.Millisecond)
	defer retry.Stop()
	var stacks string
contention:
	for {
		// Grow the buffer until every stack fits; a truncated profile could omit the contender.
		for size := 64 << 10; ; size *= 2 {
			buf := make([]byte, size)
			n := runtime.Stack(buf, true)
			if n < len(buf) {
				stacks = string(buf[:n])
				break
			}
		}
		for _, stack := range strings.Split(stacks, "\n\n") {
			if strings.Contains(stack, "TestUninstallTool_TwoCallsNeverOverlap") &&
				strings.Contains(stack, "sync.(*Mutex).") {
				break contention
			}
		}
		select {
		case n := <-entered:
			t.Fatalf("uninstall body %d entered while the first was parked", n)
		case r := <-done:
			t.Fatalf("an uninstall call finished while another was still inside: done=%v err=%v", r.out.Done, r.err)
		case <-deadline.C:
			t.Fatalf("second handler never contended on the operation lock:\n%s", stacks)
		case <-retry.C:
		}
	}

	releaseOnce.Do(func() { close(release) })
	for range 2 {
		select {
		case r := <-done:
			if r.err != nil || !r.out.Done {
				t.Errorf("uninstall call: done=%v err=%v", r.out.Done, r.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a call never finished once the first had released the lock")
		}
	}
	// The authoritative check: a second body never entered while the first was inside.
	if overlapped.Load() {
		t.Fatal("two uninstall bodies ran at once")
	}
}

// TestDestructiveTools_NeverOverlapOnTheOperationLock pins one shared lock, not a per-tool mutex.
func TestDestructiveTools_NeverOverlapOnTheOperationLock(t *testing.T) {
	fakeStratumHost(t)

	var inside, overlapped atomic.Bool
	entered := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var handlers sync.WaitGroup
	// A failed assertion must unblock and join both handlers, or the lock strands later tests.
	defer func() {
		releaseOnce.Do(func() { close(release) })
		handlers.Wait()
	}()

	withDestructiveDeps(t, func(deps *destructiveDeps) {
		deps.uninstallRun = func(io.Writer, bool) error {
			if inside.Swap(true) {
				overlapped.Store(true)
			}
			defer inside.Store(false)
			entered <- "uninstall"
			<-release // the barrier: this body leaves only when the test says so
			return nil
		}
		deps.rollbackNode = func(io.Writer, upgrade.RollbackOptions) error {
			if inside.Swap(true) {
				overlapped.Store(true)
			}
			defer inside.Store(false)
			entered <- "rollback"
			return nil
		}
	})

	type result struct {
		out destructiveOut
		err error
	}
	uninstall := UninstallHandler()
	rollback := RollbackHandler()
	callUninstall := func() result {
		_, out, err := uninstall(context.Background(), nil, UninstallIn{})
		return result{out, err}
	}
	callRollback := func() result {
		_, out, err := rollback(context.Background(), nil, RollbackIn{})
		return result{out, err}
	}
	done := make(chan result, 2)

	launch := func(call func() result) {
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			done <- call()
		}()
	}
	launch(callUninstall)
	if name := <-entered; name != "uninstall" {
		t.Fatalf("the first call entered the body as %q", name)
	}

	launch(callRollback)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	retry := time.NewTicker(time.Millisecond)
	defer retry.Stop()
	var stacks string
contention:
	for {
		// Grow the buffer until every stack fits; a truncated profile could omit the contender.
		for size := 64 << 10; ; size *= 2 {
			buf := make([]byte, size)
			n := runtime.Stack(buf, true)
			if n < len(buf) {
				stacks = string(buf[:n])
				break
			}
		}
		for _, stack := range strings.Split(stacks, "\n\n") {
			if strings.Contains(stack, "TestDestructiveTools_NeverOverlapOnTheOperationLock") &&
				strings.Contains(stack, "sync.(*Mutex).") {
				break contention
			}
		}
		select {
		case name := <-entered:
			t.Fatalf("%s started while uninstall was still parked in its body", name)
		case r := <-done:
			t.Fatalf("a call finished while another was still inside: done=%v err=%v", r.out.Done, r.err)
		case <-deadline.C:
			t.Fatalf("the rollback handler never contended on the operation lock:\n%s", stacks)
		case <-retry.C:
		}
	}

	releaseOnce.Do(func() { close(release) })
	for range 2 {
		select {
		case r := <-done:
			if r.err != nil || !r.out.Done {
				t.Errorf("destructive call: done=%v err=%v", r.out.Done, r.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a call never finished once uninstall had released the lock")
		}
	}
	// The authoritative check: no other tool's body entered while one was inside.
	if overlapped.Load() {
		t.Fatal("two different destructive tools ran their operations at once")
	}
}

// TestHARepointTool_CarriesTheOperationWarnings: warnings travel as data and make done false.
func TestHARepointTool_CarriesTheOperationWarnings(t *testing.T) {
	fakeStratumHost(t)
	conn := connectorToThisNode(t)

	t.Run("a node repoint whose kubelet did not restart", func(t *testing.T) {
		warns := []string{"WARN: restart kubelet: no such process (this node may go NotReady; check the kubelet log)"}
		withDestructiveDeps(t, func(deps *destructiveDeps) {
			deps.nodeRepoint = func(io.Writer, string) ([]string, error) { return warns, nil }
		})
		_, out, err := HARepointHandler(nil)(t.Context(), nil, HARepointIn{VIP: "10.0.0.1"})
		if err != nil {
			t.Fatalf("ha_repoint failed: %v", err)
		}
		if out.Done {
			t.Error("a repoint whose kubelet did not restart reported done")
		}
		if !slices.Equal(out.Warnings, warns) {
			t.Errorf("out.Warnings = %v, want the operation's warnings as data", out.Warnings)
		}
	})

	t.Run("a Cilium repoint whose rollout did not finish", func(t *testing.T) {
		warns := []string{"WARN: cilium ds/cilium did not roll out within 5m0s: still rolling out (2 of 3 updated)"}
		withDestructiveDeps(t, func(deps *destructiveDeps) {
			deps.repointCilium = func(context.Context, io.Writer, kubernetes.Interface, string) ([]string, error) { return warns, nil }
		})
		_, out, err := HARepointHandler(conn)(t.Context(), nil, HARepointIn{VIP: "10.0.0.1", Cilium: true})
		if err != nil {
			t.Fatalf("ha_repoint failed: %v", err)
		}
		if out.Done {
			t.Error("a Cilium repoint whose rollout did not finish reported done")
		}
		if !slices.Equal(out.Warnings, warns) {
			t.Errorf("out.Warnings = %v, want the operation's warnings as data", out.Warnings)
		}
		// The incomplete-rollout guidance must survive, keyed off the warnings.
		for _, want := range []string{"did not finish rolling out", "rollout status", "Do not repeat the repoint"} {
			if !strings.Contains(out.Next, want) {
				t.Errorf("out.Next does not carry the incomplete-rollout guidance %q:\n%s", want, out.Next)
			}
		}
	})

	t.Run("a clean Cilium repoint stays done with its own guidance", func(t *testing.T) {
		withDestructiveDeps(t, func(deps *destructiveDeps) {
			deps.repointCilium = func(context.Context, io.Writer, kubernetes.Interface, string) ([]string, error) { return nil, nil }
		})
		_, out, err := HARepointHandler(conn)(t.Context(), nil, HARepointIn{VIP: "10.0.0.1", Cilium: true})
		if err != nil || !out.Done {
			t.Fatalf("done=%v err=%v", out.Done, err)
		}
		if out.Warnings != nil {
			t.Errorf("out.Warnings = %v, want none on a clean call", out.Warnings)
		}
		if !strings.Contains(out.Next, "once-per-cluster") {
			t.Errorf("out.Next lost the once-per-cluster guidance:\n%s", out.Next)
		}
	})
}

// TestCredentialSources_MutatingTools pins the node credential on node-local mutating tools.
func TestCredentialSources_MutatingTools(t *testing.T) {
	fakeStratumHost(t)

	// A recording server failing every request: no operation may get past its first API call.
	recorder := func() (*httptest.Server, func() []string) {
		var mu sync.Mutex
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		return srv, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), paths...)
		}
	}
	serverA, aPaths := recorder()
	serverB, bPaths := recorder()

	oldConf := version.AdminConf
	version.AdminConf = kubetest.Kubeconfig(t, &rest.Config{Host: serverA.URL})
	t.Cleanup(func() { version.AdminConf = oldConf })
	// The $KUBECONFIG fallback: a tool that moved onto the configurable connector reaches B and fails.
	t.Setenv("KUBECONFIG", kubetest.Kubeconfig(t, &rest.Config{Host: serverB.URL}))

	for _, tc := range []struct {
		name  string
		call  func() error
		under string // the path prefix the node credential must serve
	}{
		{"token_create", func() error {
			deps := defaultTokenDeps()
			deps.nodeIP = func() (string, error) { return "10.0.0.1", nil }
			_, _, err := tokenCreateWith(deps)(t.Context(), nil, TokenCreateIn{})
			return err
			// The mint's first call is the maintenance fence take: a kube-system lease, so A sees it first.
		}, "/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			_ = captureStderrDiscard(func() { err = tc.call() })
			if err == nil {
				t.Fatal("the call succeeded against a server that answers 500 to everything")
			}
			var saw bool
			for _, p := range aPaths() {
				if strings.HasPrefix(p, tc.under) {
					saw = true
				}
			}
			if !saw {
				t.Errorf("%s never reached the admin credential under %s; A saw %v", tc.name, tc.under, aPaths())
			}
			if got := bPaths(); len(got) != 0 {
				t.Errorf("%s used the server's credential: %v", tc.name, got)
			}
		})
	}

	// These must first prove this node belongs to the connector's cluster; neither may bypass that.
	for _, name := range []string{"upgrade_apply cluster", "ha_repoint cilium"} {
		t.Run(name+" uses the connector", func(t *testing.T) {
			setThisNodeName(t, "cp1")
			bConf := kubetest.Kubeconfig(t, &rest.Config{Host: serverB.URL})
			conn := kube.NewConnector(kube.Options{Kubeconfig: bConf})
			beforeA, beforeB := len(aPaths()), len(bPaths())
			var err error
			if name == "upgrade_apply cluster" {
				from := stage(t, stageDir(t), "stratum", 0o755)
				_, _, err = UpgradeApplyHandler(conn)(t.Context(), nil, UpgradeApplyIn{Scope: "cluster", From: from, Target: "v1.31.0"})
			} else {
				_, _, err = HARepointHandler(conn)(t.Context(), nil, HARepointIn{VIP: "10.0.0.1", Cilium: true})
			}
			if err == nil {
				t.Fatal("the operation ignored the connector's API refusal")
			}
			if got := aPaths(); len(got) != beforeA {
				t.Errorf("the operation used the node's admin credential: %v", got[beforeA:])
			}
			if got := bPaths(); len(got) <= beforeB || got[beforeB] != "/api/v1/nodes/cp1" {
				t.Errorf("the operation did not check this node through the connector: %v", got[beforeB:])
			}
		})
	}
}

// TestCATools_RefuseWhatTheValidatorRefuses pins the validators' errors, before the host check.
func TestCATools_RefuseWhatTheValidatorRefuses(t *testing.T) {
	notStratumHost(t)

	for _, tc := range []struct {
		name string
		call func() error
		want string
	}{
		{"certs_rotate_ca with cluster and phase", func() error {
			_, _, err := CertsRotateCAHandler()(t.Context(), nil, CertsRotateCAIn{Cluster: true, Phase: "sign-new"})
			return err
		}, "not both"},
		{"certs_migrate_etcd_ca with cluster and phase", func() error {
			_, _, err := CertsMigrateEtcdCAHandler()(t.Context(), nil, CertsMigrateEtcdCAIn{Cluster: true, Phase: "issue-new"})
			return err
		}, "not both"},
		{"certs_migrate_etcd_ca with force and no rollback", func() error {
			_, _, err := CertsMigrateEtcdCAHandler()(t.Context(), nil, CertsMigrateEtcdCAIn{Cluster: true, Force: true})
			return err
		}, "--force only applies"},
		{"certs_migrate_etcd_ca with resume and no cluster", func() error {
			_, _, err := CertsMigrateEtcdCAHandler()(t.Context(), nil, CertsMigrateEtcdCAIn{Phase: "issue-new", Resume: true})
			return err
		}, "--resume only applies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("a combination the validator refuses was accepted")
			}
			if errors.Is(err, errNotStratumHost) {
				t.Fatalf("the handler built its options without the validator and reached the host check: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestCertsRotateCA_ResultCarriesTheRestartWarnings: the result carries the CLI's warnings.
func TestCertsRotateCA_ResultCarriesTheRestartWarnings(t *testing.T) {
	fakeStratumHost(t)
	withDestructiveDeps(t, func(deps *destructiveDeps) {
		deps.rotateCACluster = func(context.Context, io.Writer, certs.CARotateOptions) error { return nil }
		deps.rotateCA = func(io.Writer, certs.CARotateOptions) error { return nil }
	})

	for _, tc := range []struct {
		name    string
		in      CertsRotateCAIn
		want    string
		alsoHas string // something besides the warning the result must keep carrying
	}{
		{"cluster", CertsRotateCAIn{Cluster: true}, certs.CARotateClusterWarning, ""},
		{"phase", CertsRotateCAIn{Phase: "sign-new"}, certs.CARotatePhaseWarning, "NOT finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out, err := CertsRotateCAHandler()(t.Context(), nil, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.Next, tc.want) {
				t.Errorf("result.Next does not carry the internal/certs warning:\n%s", out.Next)
			}
			if tc.alsoHas != "" && !strings.Contains(out.Next, tc.alsoHas) {
				t.Errorf("result.Next lost the remaining-phases guidance:\n%s", out.Next)
			}
		})
	}
}

// withDestructiveDeps swaps the handlers' operations for the test and restores them after.
func withDestructiveDeps(t *testing.T, tune func(*destructiveDeps)) {
	t.Helper()
	deps := defaultDestructiveDeps()
	if tune != nil {
		tune(&deps)
	}
	setDestructiveDeps(t, deps)
}

// setDestructiveDeps installs a whole deps value and restores the real one after.
func setDestructiveDeps(t *testing.T, deps destructiveDeps) {
	t.Helper()
	old := destructiveOps
	t.Cleanup(func() { destructiveOps = old })
	destructiveOps = deps
}

// recordingDeps records what fired: a refusal must reach nothing, an acceptance only its operation.
func recordingDeps(fired *[]string) destructiveDeps {
	return destructiveDeps{
		upgradeNode:    func(io.Writer, upgrade.NodeOptions) error { *fired = append(*fired, "upgradeNode"); return nil },
		upgradeCluster: func(io.Writer, upgrade.ClusterOptions) error { *fired = append(*fired, "upgradeCluster"); return nil },
		rollbackNode:   func(io.Writer, upgrade.RollbackOptions) error { *fired = append(*fired, "rollbackNode"); return nil },
		certsRotate: func(context.Context, io.Writer, certs.Options) (bool, error) {
			*fired = append(*fired, "certsRotate")
			return false, nil
		},
		rotateCA: func(io.Writer, certs.CARotateOptions) error { *fired = append(*fired, "rotateCA"); return nil },
		rotateCACluster: func(context.Context, io.Writer, certs.CARotateOptions) error {
			*fired = append(*fired, "rotateCACluster")
			return nil
		},
		migrateEtcdCA: func(io.Writer, certs.EtcdCAOptions) error { *fired = append(*fired, "migrateEtcdCA"); return nil },
		migrateEtcdCACluster: func(context.Context, io.Writer, certs.EtcdCAOptions) error {
			*fired = append(*fired, "migrateEtcdCACluster")
			return nil
		},
		restoreLocalSeed: func(io.Writer, string) error { *fired = append(*fired, "restoreLocalSeed"); return nil },
		removeRestoreCopies: func(shown []etcdsnap.RestoreCopy) ([]etcdsnap.RestoreCopy, error) {
			*fired = append(*fired, "removeRestoreCopies")
			return nil, nil
		},
		rejoinEtcd: func(context.Context, io.Writer, string, bool) error {
			*fired = append(*fired, "rejoinEtcd")
			return nil
		},
		nodeRepoint: func(io.Writer, string) ([]string, error) { *fired = append(*fired, "nodeRepoint"); return nil, nil },
		repointCilium: func(context.Context, io.Writer, kubernetes.Interface, string) ([]string, error) {
			*fired = append(*fired, "repointCilium")
			return nil, nil
		},
		uninstallRun: func(io.Writer, bool) error { *fired = append(*fired, "uninstallRun"); return nil },
	}
}

// assertRow: a refusal must carry wantErr and reach nothing; an acceptance must reach only want.
func assertRow(t *testing.T, fired []string, want, wantErr string, err error) {
	t.Helper()
	if want == "" {
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("error = %v, want a refusal containing %q", err, wantErr)
		}
		if len(fired) != 0 {
			t.Fatalf("a refused combination reached %v; no operation may run", fired)
		}
		return
	}
	if err != nil {
		t.Fatalf("accepted arguments were refused: %v", err)
	}
	if !slices.Equal(fired, []string{want}) {
		t.Fatalf("the arguments reached %v, want only [%s]", fired, want)
	}
}

// connectorToThisNode's fake server proves itself this node's cluster, past clusterOfThisNode.
func connectorToThisNode(t *testing.T) *kube.Connector {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/nodes/cp1":
			fmt.Fprint(w, `{"kind":"Node","apiVersion":"v1","metadata":{"name":"cp1"}}`)
		case "/api/v1/namespaces/kube-system":
			fmt.Fprint(w, `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"kube-system","uid":"uid-mcp"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	setThisNodeName(t, "cp1")
	oldLocal := localCluster
	t.Cleanup(func() { localCluster = oldLocal })
	localCluster = func() (kubernetes.Interface, string, error) {
		return k8sfake.NewSimpleClientset(kubeSystem("uid-mcp")), "https://127.0.0.1:6443", nil
	}
	return kube.NewConnector(kube.Options{Kubeconfig: kubetest.Kubeconfig(t, &rest.Config{Host: srv.URL})})
}

// TestDestructiveHandlers_PassTheValidatedOptions pins the argument-to-operation mapping.
func TestDestructiveHandlers_PassTheValidatedOptions(t *testing.T) {
	t.Run("certs_rotate_ca", func(t *testing.T) {
		fakeStratumHost(t)
		var fired []string
		deps := recordingDeps(&fired)
		var gotPhase, gotCluster certs.CARotateOptions
		deps.rotateCA = func(_ io.Writer, o certs.CARotateOptions) error {
			fired = append(fired, "rotateCA")
			gotPhase = o
			return nil
		}
		deps.rotateCACluster = func(_ context.Context, _ io.Writer, o certs.CARotateOptions) error {
			fired = append(fired, "rotateCACluster")
			gotCluster = o
			return nil
		}
		setDestructiveDeps(t, deps)

		for _, tc := range []struct {
			name    string
			in      CertsRotateCAIn
			want    string
			wantErr string
			check   func(*testing.T)
		}{
			{"one phase reaches RotateCA with that phase", CertsRotateCAIn{Phase: "sign-new"}, "rotateCA", "", func(t *testing.T) {
				if want := (certs.CARotateOptions{Phase: "sign-new", Quiet: true}); gotPhase != want {
					t.Errorf("RotateCA received %+v, want %+v", gotPhase, want)
				}
			}},
			{"force and skip_token_check reach RotateCA", CertsRotateCAIn{Phase: "sign-new", Force: true, SkipTokenCheck: true}, "rotateCA", "", func(t *testing.T) {
				if want := (certs.CARotateOptions{Phase: "sign-new", Force: true, SkipTokenCheck: true, Quiet: true}); gotPhase != want {
					t.Errorf("RotateCA received %+v, want %+v", gotPhase, want)
				}
			}},
			{"cluster reaches RotateCACluster", CertsRotateCAIn{Cluster: true}, "rotateCACluster", "", func(t *testing.T) {
				if want := (certs.CARotateOptions{Cluster: true, Quiet: true}); gotCluster != want {
					t.Errorf("RotateCACluster received %+v, want %+v", gotCluster, want)
				}
			}},
			{"restart_workloads reaches RotateCACluster", CertsRotateCAIn{Cluster: true, RestartWorkloads: true}, "rotateCACluster", "", func(t *testing.T) {
				if want := (certs.CARotateOptions{Cluster: true, RestartWorkloads: true, Quiet: true}); gotCluster != want {
					t.Errorf("RotateCACluster received %+v, want %+v", gotCluster, want)
				}
			}},
			{"neither cluster nor phase", CertsRotateCAIn{}, "", "phase is required", nil},
			{"unknown phase", CertsRotateCAIn{Phase: "drop-all"}, "", "unknown phase", nil},
			// Answered before anything runs, with the shared validator's words.
			{"cluster with a phase", CertsRotateCAIn{Cluster: true, Phase: "sign-new"}, "", "not both", nil},
			{"contradicting workload flags", CertsRotateCAIn{Cluster: true, RestartWorkloads: true, SkipWorkloadCheck: true}, "", "contradict each other", nil},
			{"skip_token_check without force", CertsRotateCAIn{Cluster: true, SkipTokenCheck: true}, "", "goes with --force", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fired = nil
				gotPhase, gotCluster = certs.CARotateOptions{}, certs.CARotateOptions{}
				_, _, err := CertsRotateCAHandler()(t.Context(), nil, tc.in)
				assertRow(t, fired, tc.want, tc.wantErr, err)
				if tc.check != nil {
					tc.check(t)
				}
			})
		}
	})

	t.Run("certs_migrate_etcd_ca", func(t *testing.T) {
		fakeStratumHost(t)
		var fired []string
		deps := recordingDeps(&fired)
		var gotNode, gotCluster certs.EtcdCAOptions
		deps.migrateEtcdCA = func(_ io.Writer, o certs.EtcdCAOptions) error {
			fired = append(fired, "migrateEtcdCA")
			gotNode = o
			return nil
		}
		deps.migrateEtcdCACluster = func(_ context.Context, _ io.Writer, o certs.EtcdCAOptions) error {
			fired = append(fired, "migrateEtcdCACluster")
			gotCluster = o
			return nil
		}
		setDestructiveDeps(t, deps)

		for _, tc := range []struct {
			name    string
			in      CertsMigrateEtcdCAIn
			want    string
			wantErr string
			check   func(*testing.T)
		}{
			{"a single phase reaches MigrateEtcdCA with that phase", CertsMigrateEtcdCAIn{Phase: "issue-new"}, "migrateEtcdCA", "", func(t *testing.T) {
				if want := (certs.EtcdCAOptions{Phase: "issue-new", Quiet: true}); gotNode != want {
					t.Errorf("MigrateEtcdCA received %+v, want %+v", gotNode, want)
				}
			}},
			{"a forced rollback reaches MigrateEtcdCA", CertsMigrateEtcdCAIn{Phase: "rollback", Force: true}, "migrateEtcdCA", "", func(t *testing.T) {
				if want := (certs.EtcdCAOptions{Phase: "rollback", Force: true, Quiet: true}); gotNode != want {
					t.Errorf("MigrateEtcdCA received %+v, want %+v", gotNode, want)
				}
			}},
			// The CLI's --cluster form: the whole migration, on the validator's options.
			{"cluster reaches MigrateEtcdCACluster", CertsMigrateEtcdCAIn{Cluster: true}, "migrateEtcdCACluster", "", func(t *testing.T) {
				if want := (certs.EtcdCAOptions{Quiet: true}); gotCluster != want {
					t.Errorf("MigrateEtcdCACluster received %+v, want %+v", gotCluster, want)
				}
			}},
			{"cluster with resume reaches MigrateEtcdCACluster", CertsMigrateEtcdCAIn{Cluster: true, Resume: true}, "migrateEtcdCACluster", "", func(t *testing.T) {
				if want := (certs.EtcdCAOptions{Resume: true, Quiet: true}); gotCluster != want {
					t.Errorf("MigrateEtcdCACluster received %+v, want %+v", gotCluster, want)
				}
			}},
			{"no phase", CertsMigrateEtcdCAIn{}, "", "phase is required", nil},
			{"unknown phase", CertsMigrateEtcdCAIn{Phase: "trust-all"}, "", "unknown phase", nil},
			{"cluster with a phase", CertsMigrateEtcdCAIn{Cluster: true, Phase: "issue-new"}, "", "not both", nil},
			{"force without a rollback", CertsMigrateEtcdCAIn{Cluster: true, Force: true}, "", "--force only applies", nil},
			{"resume without cluster", CertsMigrateEtcdCAIn{Phase: "issue-new", Resume: true}, "", "--resume only applies", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fired = nil
				gotNode, gotCluster = certs.EtcdCAOptions{}, certs.EtcdCAOptions{}
				_, _, err := CertsMigrateEtcdCAHandler()(t.Context(), nil, tc.in)
				assertRow(t, fired, tc.want, tc.wantErr, err)
				if tc.check != nil {
					tc.check(t)
				}
			})
		}
	})

	t.Run("certs_rotate", func(t *testing.T) {
		fakeStratumHost(t)
		var fired []string
		deps := recordingDeps(&fired)
		var got certs.Options
		deps.certsRotate = func(_ context.Context, _ io.Writer, o certs.Options) (bool, error) {
			fired = append(fired, "certsRotate")
			got = o
			return false, nil
		}
		setDestructiveDeps(t, deps)

		for _, tc := range []struct {
			name  string
			in    CertsRotateIn
			check func(*testing.T)
		}{
			{"no threshold rotates regardless", CertsRotateIn{}, func(t *testing.T) {
				if got != (certs.Options{}) {
					t.Errorf("Rotate received %+v, want the arguments alone", got)
				}
			}},
			{"the expiry threshold reaches Rotate", CertsRotateIn{IfExpiringDays: 30}, func(t *testing.T) {
				if want := (certs.Options{IfExpiringDays: 30}); got != want {
					t.Errorf("Rotate received %+v, want %+v", got, want)
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fired = nil
				got = certs.Options{}
				_, _, err := CertsRotateHandler()(t.Context(), nil, tc.in)
				assertRow(t, fired, "certsRotate", "", err)
				tc.check(t)
			})
		}
	})

	t.Run("rollback", func(t *testing.T) {
		fakeStratumHost(t)
		dir := t.TempDir()
		old := snapshotDir
		t.Cleanup(func() { snapshotDir = old })
		snapshotDir = func() string { return dir }

		var fired []string
		deps := recordingDeps(&fired)
		var got upgrade.RollbackOptions
		deps.rollbackNode = func(_ io.Writer, o upgrade.RollbackOptions) error {
			fired = append(fired, "rollbackNode")
			got = o
			return nil
		}
		setDestructiveDeps(t, deps)

		for _, tc := range []struct {
			name    string
			in      RollbackIn
			want    string
			wantErr string
			check   func(*testing.T)
		}{
			{"the soak reaches RollbackNode", RollbackIn{SoakSeconds: 20}, "rollbackNode", "", func(t *testing.T) {
				if want := (upgrade.RollbackOptions{SoakSec: 20}); got != want {
					t.Errorf("RollbackNode received %+v, want %+v", got, want)
				}
			}},
			{"an absolute snapshot path is passed through", RollbackIn{SoakSeconds: 5, EtcdSnapshot: "/var/backups/etcd/x.db"}, "rollbackNode", "", func(t *testing.T) {
				if want := (upgrade.RollbackOptions{SoakSec: 5, EtcdSnapshot: "/var/backups/etcd/x.db"}); got != want {
					t.Errorf("RollbackNode received %+v, want %+v", got, want)
				}
			}},
			{"a bare name resolves into the snapshot directory", RollbackIn{EtcdSnapshot: "snap-1.db"}, "rollbackNode", "", func(t *testing.T) {
				if want := (upgrade.RollbackOptions{EtcdSnapshot: filepath.Join(dir, "snap-1.db")}); got != want {
					t.Errorf("RollbackNode received %+v, want %+v", got, want)
				}
			}},
			{"a relative snapshot path", RollbackIn{EtcdSnapshot: "sub/dir.db"}, "", "relative path", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fired = nil
				got = upgrade.RollbackOptions{}
				_, _, err := RollbackHandler()(context.Background(), nil, tc.in)
				assertRow(t, fired, tc.want, tc.wantErr, err)
				if tc.check != nil {
					tc.check(t)
				}
			})
		}
	})

	t.Run("snapshot_restore", func(t *testing.T) {
		fakeStratumHost(t)
		dir := t.TempDir()
		old := snapshotDir
		t.Cleanup(func() { snapshotDir = old })
		snapshotDir = func() string { return dir }

		var fired []string
		deps := recordingDeps(&fired)
		var got string
		deps.restoreLocalSeed = func(_ io.Writer, p string) error { fired = append(fired, "restoreLocalSeed"); got = p; return nil }
		setDestructiveDeps(t, deps)

		for _, tc := range []struct {
			name    string
			in      SnapshotRestoreIn
			want    string
			wantErr string
			check   func(*testing.T)
		}{
			{"an absolute snapshot path is restored as given", SnapshotRestoreIn{Snapshot: "/var/backups/etcd/x.db"}, "restoreLocalSeed", "", func(t *testing.T) {
				if got != "/var/backups/etcd/x.db" {
					t.Errorf("the restore received %q", got)
				}
			}},
			{"a bare name resolves into the snapshot directory", SnapshotRestoreIn{Snapshot: "x.db"}, "restoreLocalSeed", "", func(t *testing.T) {
				if want := filepath.Join(dir, "x.db"); got != want {
					t.Errorf("the restore received %q, want %q", got, want)
				}
			}},
			{"no snapshot", SnapshotRestoreIn{}, "", "snapshot is required", nil},
			{"a relative snapshot path", SnapshotRestoreIn{Snapshot: "./x.db"}, "", "relative path", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fired = nil
				got = ""
				_, _, err := SnapshotRestoreHandler()(context.Background(), nil, tc.in)
				assertRow(t, fired, tc.want, tc.wantErr, err)
				if tc.check != nil {
					tc.check(t)
				}
			})
		}
	})

	t.Run("snapshot_rejoin", func(t *testing.T) {
		fakeStratumHost(t)
		var fired []string
		deps := recordingDeps(&fired)
		var gotHost string
		var gotForce bool
		deps.rejoinEtcd = func(_ context.Context, _ io.Writer, host string, force bool) error {
			fired = append(fired, "rejoinEtcd")
			gotHost, gotForce = host, force
			return nil
		}
		setDestructiveDeps(t, deps)

		for _, tc := range []struct {
			name    string
			in      SnapshotRejoinIn
			want    string
			wantErr string
		}{
			{"the seed's host reaches RejoinEtcd without force", SnapshotRejoinIn{Server: "https://10.0.0.1:6443"}, "rejoinEtcd", ""},
			{"no server", SnapshotRejoinIn{}, "", "server is required"},
			{"a server with URL structure", SnapshotRejoinIn{Server: "https://user:pw@10.0.0.1:6443"}, "", "must be exactly https://"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fired = nil
				gotHost, gotForce = "", false
				_, _, err := SnapshotRejoinHandler()(context.Background(), nil, tc.in)
				assertRow(t, fired, tc.want, tc.wantErr, err)
				if tc.want != "" && (gotHost != "10.0.0.1" || gotForce) {
					t.Errorf("RejoinEtcd received host %q force %v, want 10.0.0.1 and false", gotHost, gotForce)
				}
			})
		}
	})

	t.Run("snapshot_prune_restores", func(t *testing.T) {
		fakeStratumHost(t)
		oldEtcd := version.EtcdDataDir
		t.Cleanup(func() { version.EtcdDataDir = oldEtcd })
		version.EtcdDataDir = filepath.Join(t.TempDir(), "etcd")
		copyPath := version.EtcdDataDir + ".pre-restore-20260926-120000"
		mod := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

		var fired []string
		deps := recordingDeps(&fired)
		var got []etcdsnap.RestoreCopy
		deps.removeRestoreCopies = func(shown []etcdsnap.RestoreCopy) ([]etcdsnap.RestoreCopy, error) {
			fired = append(fired, "removeRestoreCopies")
			got = shown
			return nil, nil
		}
		setDestructiveDeps(t, deps)

		for _, tc := range []struct {
			name    string
			in      SnapshotPruneRestoresIn
			want    string
			wantErr string
			check   func(*testing.T)
		}{
			{"a named copy reaches RemoveRestoreCopies as listed", SnapshotPruneRestoresIn{Copies: []RestoreCopyRef{
				{Path: copyPath, Modified: mod.Format(time.RFC3339Nano)},
			}}, "removeRestoreCopies", "", func(t *testing.T) {
				if want := []etcdsnap.RestoreCopy{{Path: copyPath, Modified: mod}}; !slices.Equal(got, want) {
					t.Errorf("RemoveRestoreCopies received %+v, want %+v", got, want)
				}
			}},
			{"no copies", SnapshotPruneRestoresIn{}, "", "copies is required", nil},
			{"a path that is not a restore copy", SnapshotPruneRestoresIn{Copies: []RestoreCopyRef{
				{Path: "/tmp/evil", Modified: mod.Format(time.RFC3339Nano)},
			}}, "", "is not a restore copy", nil},
			{"a copy named twice", SnapshotPruneRestoresIn{Copies: []RestoreCopyRef{
				{Path: copyPath, Modified: mod.Format(time.RFC3339Nano)},
				{Path: copyPath, Modified: mod.Format(time.RFC3339Nano)},
			}}, "", "more than once", nil},
			{"a modified time that is not the listing's", SnapshotPruneRestoresIn{Copies: []RestoreCopyRef{
				{Path: copyPath, Modified: "yesterday"},
			}}, "", "not the time", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fired = nil
				got = nil
				_, _, err := SnapshotPruneRestoresHandler()(context.Background(), nil, tc.in)
				assertRow(t, fired, tc.want, tc.wantErr, err)
				if tc.check != nil {
					tc.check(t)
				}
			})
		}
	})

	t.Run("ha_repoint", func(t *testing.T) {
		fakeStratumHost(t)
		conn := connectorToThisNode(t)

		var fired []string
		deps := recordingDeps(&fired)
		var gotVIP, gotCiliumVIP string
		var gotClient kubernetes.Interface
		deps.nodeRepoint = func(_ io.Writer, vip string) ([]string, error) {
			fired = append(fired, "nodeRepoint")
			gotVIP = vip
			return nil, nil
		}
		deps.repointCilium = func(_ context.Context, _ io.Writer, cs kubernetes.Interface, vip string) ([]string, error) {
			fired = append(fired, "repointCilium")
			gotClient, gotCiliumVIP = cs, vip
			return nil, nil
		}
		setDestructiveDeps(t, deps)

		for _, tc := range []struct {
			name    string
			in      HARepointIn
			want    string
			wantErr string
			check   func(*testing.T)
		}{
			{"the node repoint receives the VIP", HARepointIn{VIP: "10.0.0.1"}, "nodeRepoint", "", func(t *testing.T) {
				if gotVIP != "10.0.0.1" {
					t.Errorf("NodeRepoint received %q", gotVIP)
				}
			}},
			{"the Cilium repoint receives the VIP and the connector's cluster", HARepointIn{VIP: "10.0.0.1", Cilium: true}, "repointCilium", "", func(t *testing.T) {
				if gotCiliumVIP != "10.0.0.1" {
					t.Errorf("RepointCiliumWith received vip %q", gotCiliumVIP)
				}
				if gotClient == nil {
					t.Error("RepointCiliumWith received no cluster client")
				}
			}},
			{"no VIP", HARepointIn{}, "", "vip is required", nil},
			{"a VIP with URL structure", HARepointIn{VIP: "https://10.0.0.1"}, "", "must be a bare", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fired = nil
				gotVIP, gotCiliumVIP, gotClient = "", "", nil
				_, _, err := HARepointHandler(conn)(t.Context(), nil, tc.in)
				assertRow(t, fired, tc.want, tc.wantErr, err)
				if tc.check != nil {
					tc.check(t)
				}
			})
		}
	})

	t.Run("uninstall", func(t *testing.T) {
		fakeStratumHost(t)
		var fired []string
		deps := recordingDeps(&fired)
		var gotForce bool
		deps.uninstallRun = func(_ io.Writer, force bool) error {
			fired = append(fired, "uninstallRun")
			gotForce = force
			return nil
		}
		setDestructiveDeps(t, deps)

		_, _, err := UninstallHandler()(context.Background(), nil, UninstallIn{})
		assertRow(t, fired, "uninstallRun", "", err)
		// --allow-destructive is the confirmation, so the tool runs the real teardown, never the preview.
		if !gotForce {
			t.Error("Uninstall received force=false, which is the preview that removes nothing")
		}
	})

	t.Run("upgrade_apply", func(t *testing.T) {
		fakeStratumHost(t)
		conn := connectorToThisNode(t)
		dir := stageDir(t)
		stagedA := stage(t, dir, "stratum-a", 0o755)
		stagedB := stage(t, dir, "stratum-b", 0o755)

		var fired []string
		deps := recordingDeps(&fired)
		var gotNode upgrade.NodeOptions
		var gotCluster upgrade.ClusterOptions
		deps.upgradeNode = func(_ io.Writer, o upgrade.NodeOptions) error {
			fired = append(fired, "upgradeNode")
			gotNode = o
			return nil
		}
		deps.upgradeCluster = func(_ io.Writer, o upgrade.ClusterOptions) error {
			fired = append(fired, "upgradeCluster")
			gotCluster = o
			return nil
		}
		setDestructiveDeps(t, deps)

		for _, tc := range []struct {
			name    string
			in      UpgradeApplyIn
			want    string
			wantErr string
			check   func(*testing.T)
		}{
			{"a bare node scope reaches UpgradeNode with the defaults", UpgradeApplyIn{Scope: "node"}, "upgradeNode", "", func(t *testing.T) {
				if gotNode != (upgrade.NodeOptions{}) {
					t.Errorf("UpgradeNode received %+v, want the arguments alone", gotNode)
				}
			}},
			{"the node arguments reach UpgradeNode", UpgradeApplyIn{Scope: "node", Target: "v1.33.1", From: "stratum-a", SoakSeconds: 30, SkipSnapshot: true}, "upgradeNode", "", func(t *testing.T) {
				if want := (upgrade.NodeOptions{Target: "v1.33.1", From: "stratum-a", SoakSec: 30, SkipSnapshot: true}); gotNode != want {
					t.Errorf("UpgradeNode received %+v, want %+v", gotNode, want)
				}
			}},
			{"an unstated cluster target is defaulted to the binary's version", UpgradeApplyIn{Scope: "cluster", From: stagedA}, "upgradeCluster", "", func(t *testing.T) {
				if gotCluster.Target != version.K8sVersion {
					t.Errorf("UpgradeCluster received target %q, want the binary's %q", gotCluster.Target, version.K8sVersion)
				}
				if gotCluster.From != stagedA || !gotCluster.VerifyStagedOnNode || gotCluster.Client == nil ||
					gotCluster.WorkersMaxUnavailable != 0 || gotCluster.SoakSec != 0 || gotCluster.SkipSnapshot {
					t.Errorf("UpgradeCluster received %+v", gotCluster)
				}
			}},
			{"the cluster arguments reach UpgradeCluster", UpgradeApplyIn{Scope: "cluster", From: stagedB, Target: "v1.40.0", WorkersMaxUnavailable: 2, SoakSeconds: 45, SkipSnapshot: true}, "upgradeCluster", "", func(t *testing.T) {
				if gotCluster.Target != "v1.40.0" {
					t.Errorf("an explicit target was overwritten with %q", gotCluster.Target)
				}
				if gotCluster.From != stagedB || gotCluster.WorkersMaxUnavailable != 2 || gotCluster.SoakSec != 45 || !gotCluster.SkipSnapshot {
					t.Errorf("UpgradeCluster received %+v", gotCluster)
				}
			}},
			{"no scope", UpgradeApplyIn{}, "", "scope is required", nil},
			{"an unknown scope", UpgradeApplyIn{Scope: "weekly"}, "", "unknown scope", nil},
			{"a cluster scope without a staged binary", UpgradeApplyIn{Scope: "cluster"}, "", "scope=cluster requires from", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fired = nil
				gotNode, gotCluster = upgrade.NodeOptions{}, upgrade.ClusterOptions{}
				_, _, err := UpgradeApplyHandler(conn)(t.Context(), nil, tc.in)
				assertRow(t, fired, tc.want, tc.wantErr, err)
				if tc.check != nil {
					tc.check(t)
				}
			})
		}
	})
}

// twinClusterServer: two UIDs make two clusters that both hold a node named cp1.
func twinClusterServer(t *testing.T, nsUID string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	node := `{"apiVersion":"v1","kind":"Node","metadata":{"name":"cp1"}}`
	nodes := `{"apiVersion":"v1","kind":"NodeList","metadata":{"resourceVersion":"1"},"items":[` +
		`{"metadata":{"name":"cp1","labels":{"stratum.ciq.com/control-plane":"true"},"resourceVersion":"1"},` +
		`"status":{"nodeInfo":{"kubeletVersion":"v1.30.0"},` +
		`"conditions":[{"type":"Ready","status":"True"}]}}]}`
	ns := `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"` + nsUID + `"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/nodes":
			fmt.Fprint(w, nodes)
		case "/api/v1/nodes/cp1":
			fmt.Fprint(w, node)
		case "/api/v1/namespaces/kube-system":
			fmt.Fprint(w, ns)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// callToolFor decodes the structured result into out; a tool-reported error's text is returned.
func callToolFor[TOut any](t *testing.T, cs *sdk.ClientSession, name string, args map[string]any, out *TOut) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if !res.IsError {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshal %s result: %v", name, err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode %s result %s: %v", name, raw, err)
		}
		return ""
	}
	var msg strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			msg.WriteString(tc.Text)
		}
	}
	return msg.String()
}

// TestUpgradePlanAndApply_AddressTheSameCluster: both rows must read the node connector's cluster.
func TestUpgradePlanAndApply_AddressTheSameCluster(t *testing.T) {
	fakeStratumHost(t)
	setThisNodeName(t, "cp1")

	// This node's own cluster: the local client holds kube-system uid-mcp, matching A and not B.
	oldLocal := localCluster
	t.Cleanup(func() { localCluster = oldLocal })
	localCluster = func() (kubernetes.Interface, string, error) {
		return k8sfake.NewSimpleClientset(kubeSystem("uid-mcp")), "https://this-node.example:6443", nil
	}

	serverA, aPaths := twinClusterServer(t, "uid-mcp")
	serverB, bPaths := twinClusterServer(t, "uid-twin")

	oldConf := version.AdminConf
	version.AdminConf = kubetest.Kubeconfig(t, &rest.Config{Host: serverA.URL})
	t.Cleanup(func() { version.AdminConf = oldConf })

	// The server's two connectors: the configurable one on B's --kubeconfig, the node one on A.
	conn := kube.NewConnector(kube.Options{Kubeconfig: kubetest.Kubeconfig(t, &rest.Config{Host: serverB.URL})})
	nodeConn := kube.NewConnector(kube.Options{Kubeconfig: version.AdminConf})

	// The rollout is replaced: this guard is about which cluster the tools address.
	withDestructiveDeps(t, func(deps *destructiveDeps) {
		deps.upgradeCluster = func(io.Writer, upgrade.ClusterOptions) error { return nil }
	})

	// The two rows, registered exactly as the catalog builds them.
	srv := NewServer(Options{Version: "v0-test"})
	for _, row := range Catalog("v0-test", conn, nodeConn, WritePolicy{}) {
		if row.Name == "upgrade_plan" || row.Name == "upgrade_apply" {
			row.Add(srv, &sdk.Tool{Name: row.Name, Description: row.Description})
		}
	}
	serverT, clientT := sdk.NewInMemoryTransports()
	go func() { _ = srv.Run(t.Context(), serverT) }()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "0"}, nil).
		Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	staged := stage(t, stageDir(t), "stratum", 0o755)

	t.Run("upgrade_plan previews the node connector's cluster", func(t *testing.T) {
		beforeA, beforeB := len(aPaths()), len(bPaths())
		var out UpgradePlanOut
		if failed := callToolFor(t, cs, "upgrade_plan", map[string]any{"target": "v1.31.0"}, &out); failed != "" {
			t.Fatalf("upgrade_plan failed: %s", failed)
		}
		var saw bool
		for _, p := range aPaths()[beforeA:] {
			if p == "/api/v1/nodes" {
				saw = true
			}
		}
		if !saw {
			t.Errorf("upgrade_plan's node list did not come from the node connector's cluster: A saw %v, B saw %v",
				aPaths()[beforeA:], bPaths()[beforeB:])
		}
		if got := bPaths()[beforeB:]; len(got) != 0 {
			t.Errorf("upgrade_plan read the operator's --kubeconfig cluster (%v); the plan must preview the cluster upgrade_apply rolls", got)
		}
		if out.NodeCount != 1 {
			t.Errorf("upgrade_plan counted %d nodes, want the one node the node connector's cluster serves", out.NodeCount)
		}
	})

	t.Run("upgrade_apply cluster-scope rolls the node connector's cluster", func(t *testing.T) {
		beforeA, beforeB := len(aPaths()), len(bPaths())
		var out destructiveOut
		failed := callToolFor(t, cs, "upgrade_apply",
			map[string]any{"scope": "cluster", "from": staged, "target": "v1.31.0"}, &out)
		var sawNode, sawNS bool
		for _, p := range aPaths()[beforeA:] {
			switch p {
			case "/api/v1/nodes/cp1":
				sawNode = true
			case "/api/v1/namespaces/kube-system":
				sawNS = true
			}
		}
		if !sawNode || !sawNS {
			t.Errorf("upgrade_apply's proof that it rolls this node's own cluster did not come from the node connector: A saw %v, B saw %v",
				aPaths()[beforeA:], bPaths()[beforeB:])
		}
		if got := bPaths()[beforeB:]; len(got) != 0 {
			t.Errorf("upgrade_apply consulted the operator's --kubeconfig cluster (%v); scope=cluster must roll this node's own", got)
		}
		if failed != "" {
			t.Fatalf("upgrade_apply failed against the node connector's cluster: %s", failed)
		}
		if !out.Done {
			t.Errorf("upgrade_apply reported done=%v: the rollout must run on the node connector's cluster", out.Done)
		}
	})
}

// TestNoStaleGuardReferences: a comment sweep has no behavior a test could drive.
func TestNoStaleGuardReferences(t *testing.T) {
	t.Run("the build and CI files drop the guard", func(t *testing.T) {
		// The Makefile and every GitHub workflow, two directories up from the package directory.
		type target struct{ display, path string }
		targets := []target{{display: "Makefile", path: filepath.Join("..", "..", "Makefile")}}
		workflowDir := filepath.Join("..", "..", ".github", "workflows")
		entries, err := os.ReadDir(workflowDir)
		if err != nil {
			t.Fatalf("read the workflow directory: %v", err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
				continue
			}
			targets = append(targets, target{
				display: filepath.Join(".github", "workflows", name),
				path:    filepath.Join(workflowDir, name),
			})
		}

		var hits []string
		for _, target := range targets {
			src, err := os.ReadFile(target.path)
			if err != nil {
				t.Fatalf("read %s: %v", target.display, err)
			}
			for i, line := range strings.Split(string(src), "\n") {
				if strings.Contains(line, "guardStdout") {
					hits = append(hits, fmt.Sprintf("%s:%d: %s", target.display, i+1, strings.TrimSpace(line)))
				}
			}
		}
		if len(hits) > 0 {
			t.Errorf("the build and CI prose still cites guardStdout at %d lines; "+
				"a justification naming it as live behavior describes code that does "+
				"not exist — rewrite those comment "+
				"blocks around the current design (opMu is the destructive tools' sole exclusion, and "+
				"nothing under internal/mcp redirects os.Stdout):\n%s",
				len(hits), strings.Join(hits, "\n"))
		}
	})

	t.Run("internal/mcp keeps no stale gate language", func(t *testing.T) {
		// The needle is split so this pin's own source contains no line matching what it scans for.
		needle := "R22-" + "gated"
		var hits []string
		err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(src), "\n") {
				if strings.Contains(line, needle) {
					hits = append(hits, fmt.Sprintf("%s:%d: %s", path, i+1, strings.TrimSpace(line)))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk this package's directory: %v", err)
		}
		if len(hits) > 0 {
			t.Errorf("internal/mcp still carries stale gate language at %d lines; "+
				"the exclusion comments must speak of the "+
				"explicit operation lock as the destructive tools' sole exclusion — rewrite the stale "+
				"comments to that state:\n%s",
				len(hits), strings.Join(hits, "\n"))
		}
	})
}

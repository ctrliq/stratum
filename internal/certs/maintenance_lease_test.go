package certs

import (
	"io"

	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/rollout"
	"github.com/ctrliq/stratum/internal/version"
)

func TestRestartClientsStopsDispatchAfterLeaseLoss(t *testing.T) {
	for _, when := range []string{"during republish", "between targets"} {
		t.Run(when, func(t *testing.T) {
			_, pem := genCA(t, "new")
			stageNewCA(t, pem)
			cs := client(rootCA("kube-system", pem, republished))
			lost := false
			if when == "during republish" {
				cs.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
					lost = true
					return false, nil, nil
				})
			}
			leaseLost := errors.New("maintenance lease taken by successor")
			var restarted []string
			roller := rollout.Roller{
				Hint: adminKubectlHint, CallTimeout: time.Second, Wait: time.Second,
				Kubectl: func(_ context.Context, _ time.Duration, _ bool, args ...string) ([]byte, error) {
					if i := slices.Index(args, "get"); i >= 0 {
						return []byte(args[i+1]), nil
					}
					if strings.Contains(strings.Join(args, " "), "rollout restart") {
						restarted = append(restarted, args[len(args)-1])
						lost = true
					}
					return nil, nil
				},
			}
			roller = clientRollerWithFence(context.Background(), roller, func(string) error {
				if lost {
					return leaseLost
				}
				return nil
			})
			err := restartClients(context.Background(), io.Discard, cs, CARotateOptions{SkipWorkloadCheck: true}, roller)
			if !errors.Is(err, leaseLost) {
				t.Fatalf("restartClients = %v, want lease loss", err)
			}
			want := 0
			if when == "between targets" {
				want = 1
			}
			if len(restarted) != want {
				t.Fatalf("restarted %v, want %d dispatches before loss", restarted, want)
			}
		})
	}
}

func TestTokenGateStopsDispatchAfterLeaseLoss(t *testing.T) {
	oldWait, oldPoll, oldSettle := tokenGateWait, tokenGatePoll, tokenSettle
	t.Cleanup(func() { tokenGateWait, tokenGatePoll, tokenSettle = oldWait, oldPoll, oldSettle })
	tokenGateWait, tokenGatePoll, tokenSettle = 100*time.Millisecond, time.Millisecond, 0
	oldRun, oldLog := runExitCode, jobLogs
	t.Cleanup(func() { runExitCode, jobLogs = oldRun, oldLog })
	leaseLost := errors.New("maintenance lease taken by successor")
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "next node", true: "restart same node"}[stale], func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			jobs := 0
			runExitCode = func(context.Context, kubernetes.Interface, nodejob.Spec) (int, error) {
				jobs++
				if stale {
					return ExitStaleTokens, nil
				}
				return 0, nil
			}
			jobLogs = func(context.Context, kubernetes.Interface, nodejob.Spec) (string, error) {
				return "", nil // stale exit status still requires a restart
			}
			scan := clusterTokenScanner(io.Discard, cs, "initiator", oldKid, func(string) error {
				if jobs > 0 {
					return leaseLost
				}
				return nil
			})
			err := waitTokensClean(context.Background(), io.Discard, []cnode{{Name: "cp-2"}, {Name: "cp-3"}}, scan)
			if !errors.Is(err, leaseLost) || jobs != 1 {
				t.Fatalf("token gate = %v, jobs = %d; want lease loss and one Job", err, jobs)
			}
		})
	}
}

func TestTokenReissueHoldsNodeMaintenanceLock(t *testing.T) {
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "maintenance.lock"))
	root := t.TempDir()
	mount(t, root, "pod-old", "v", jwtWithKid(oldKid))
	k := &kubelet{root: root, fixes: true}
	k.install(t)
	release, err := nodelock.Acquire("upgrade")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := scanNode(context.Background(), io.Discard, oldKid, true); err == nil || !strings.Contains(err.Error(), "upgrade is already running on this node") {
		t.Fatalf("scanNode = %v, want maintenance contention", err)
	}
	if k.calls != 0 {
		t.Fatal("kubelet restarted while upgrade held the node lock")
	}
	if st, err := scanNode(context.Background(), io.Discard, oldKid, false); err != nil || len(st) != 1 {
		t.Fatalf("read-only scan = %v, %v; want the stale token", st, err)
	}
	release()
	oldRestart := restartKubelet
	restartKubelet = func() error {
		otherRelease, err := nodelock.Acquire("other maintenance")
		if err == nil {
			otherRelease()
			t.Error("node lock was not held through kubelet restart")
		}
		return oldRestart()
	}
	if st, err := scanNode(context.Background(), io.Discard, oldKid, true); err != nil || len(st) != 0 {
		t.Fatalf("scan after release = %v, %v", st, err)
	}
}

func TestTokenGateChecksBeforeLocalKubeletRestart(t *testing.T) {
	root := t.TempDir()
	mount(t, root, "pod-old", "v", jwtWithKid(oldKid))
	k := &kubelet{root: root, fixes: true}
	k.install(t)
	leaseLost := errors.New("maintenance lease taken during local token scan")
	checks := 0
	scan := clusterTokenScanner(io.Discard, fake.NewSimpleClientset(), "initiator", oldKid, func(string) error {
		checks++
		if checks > 1 {
			return leaseLost
		}
		return nil
	})
	if _, err := scan(context.Background(), cnode{Name: "initiator"}, true); !errors.Is(err, leaseLost) || k.calls != 0 {
		t.Fatalf("scan = %v, restarts = %d; want lease loss before restart", err, k.calls)
	}
}

func TestDeferredFetchKeyCleanupPreservesSuccessor(t *testing.T) {
	for _, operation := range []string{"rotation", "migration"} {
		for _, successor := range []string{"own key", "other rotation", "other migration"} {
			t.Run(operation+"/"+successor, func(t *testing.T) {
				cs := fake.NewSimpleClientset()
				runID := "our-rotation"
				if operation == "migration" {
					runID = ""
				}
				// stage puts a key the way its operation's staging does: a
				// rotation under its run id, a migration under its key.
				stage := func(ctx context.Context, keyHex, run string) error {
					owner := rotationStagedKeyOwner(run)
					if run == "" {
						owner = migrationStagedKeyOwner(keyHex)
					}
					return putStagedKey(ctx, cs, keyHex, owner)
				}
				if err := stage(context.Background(), "our-key", runID); err != nil {
					t.Fatal(err)
				}
				stubGate(t, func(ctx context.Context, cs kubernetes.Interface) error {
					if successor == "own key" {
						return nil
					}
					id := "successor-rotation"
					if successor == "other migration" {
						id = ""
					}
					// A successor stages only once our key is gone: a put
					// refuses another operation's key rather than replace it.
					if err := cs.CoreV1().Secrets(caNamespace).Delete(ctx, caRotateKeySecret, metav1.DeleteOptions{}); err != nil {
						return err
					}
					return stage(ctx, "successor-key", id)
				})
				cleanupOwnCarotateKey(context.Background(), io.Discard, cs, runID, "our-key")
				sec, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
				if successor == "own key" {
					if !apierrors.IsNotFound(err) {
						t.Fatalf("our key survived cleanup: %v", err)
					}
				} else if err != nil || string(sec.Data["certificate-key"]) != "successor-key" {
					t.Fatalf("successor key did not survive cleanup: %v, %v", sec, err)
				}
			})
		}
	}
}

func TestOwnedFetchKeyCleanupRetriesWithExactPreconditions(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: caRotateKeySecret, Namespace: caNamespace, UID: "our-uid", ResourceVersion: "7", Annotations: map[string]string{carotateRunAnnotation: "our-run"}},
		Data:       map[string][]byte{"certificate-key": []byte("our-key")},
	})
	stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
	attempts := 0
	cs.PrependReactor("delete", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		attempts++
		opts := a.(k8stesting.DeleteAction).GetDeleteOptions()
		if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != "our-uid" || opts.Preconditions.ResourceVersion == nil || *opts.Preconditions.ResourceVersion != "7" {
			t.Errorf("delete lacked exact Secret ownership: %+v", opts.Preconditions)
		}
		if attempts == 1 {
			return true, nil, errors.New("transient API outage")
		}
		return false, nil, nil
	})
	cleanupOwnCarotateKey(context.Background(), io.Discard, cs, "our-run", "")
	if attempts != 2 || hasSecret(t, cs, caRotateKeySecret) {
		t.Fatalf("attempts = %d; own key survived cleanup", attempts)
	}
}

package certs

import (
	"io"

	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// Regression tests for the restart sequence, interrupts and lock/key
// ownership, kept apart from carotate_cleanup_test.go (leftover-key recovery).

func hasMigrationLock(t *testing.T, cs *fake.Clientset) bool {
	t.Helper()
	_, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), etcdMigrationLockSecret, metav1.GetOptions{})
	return err == nil
}

// RotateCACluster's deferred cleanup passes context.Background();
// the cleanup must still end.
func TestBoundedCleanupCapsAnUnboundedContext(t *testing.T) {
	ctx, cancel := boundedCleanup(context.Background())
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > cleanupBudget {
		t.Errorf("deadline = %v (set %v), want one within %v", dl, ok, cleanupBudget)
	}
	short, cancelShort := context.WithTimeout(context.Background(), time.Second)
	defer cancelShort()
	ctx2, cancel2 := boundedCleanup(short)
	defer cancel2()
	if dl2, _ := ctx2.Deadline(); time.Until(dl2) > time.Second {
		t.Error("a caller's shorter deadline must be kept")
	}
}

// A failed finalize or rollback restart must not skip the
// leftover-key cleanup or the repin, and must still be reported under its phase.
func TestRestartThenCleanupRunsTheTailAfterAFailedRestart(t *testing.T) {
	cause := errors.New("etcd not serving")
	for _, phase := range []string{PhaseFinalize, "rollback"} {
		for _, tc := range []struct {
			name    string
			restart error
		}{{"restart ok", nil}, {"restart fails", cause}} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				var calls []string
				err := restartThenCleanup(io.Discard, CARotateOptions{Quiet: true}, phase,
					func() error { calls = append(calls, "restart"); return tc.restart },
					func(io.Writer) { calls = append(calls, "cleanup") },
					func(io.Writer, CARotateOptions) { calls = append(calls, "repin") })
				if got := strings.Join(calls, ","); got != "restart,cleanup,repin" {
					t.Errorf("calls = %s, want restart,cleanup,repin", got)
				}
				if !errors.Is(err, tc.restart) {
					t.Errorf("error = %v, want %v", err, tc.restart)
				}
				if tc.restart != nil && !strings.Contains(err.Error(), phase+": files are written") {
					t.Errorf("error = %v, want it to name the %s phase", err, phase)
				}
			})
		}
	}
}

// Trust-new, like finalize, must repin after a failed restart and still
// report it.
func TestTrustNewRestartThenRepinsAfterAFailedRestart(t *testing.T) {
	cause := errors.New("etcd not serving")
	for _, tc := range []struct {
		name    string
		restart error
	}{{"restart ok", nil}, {"restart fails", cause}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			err := trustNewRestartThen(io.Discard, CARotateOptions{Quiet: true},
				func() error { calls = append(calls, "restart"); return tc.restart },
				func(io.Writer, CARotateOptions) { calls = append(calls, "repin") })
			if got := strings.Join(calls, ","); got != "restart,repin" {
				t.Errorf("calls = %s, want restart,repin", got)
			}
			if !errors.Is(err, tc.restart) {
				t.Errorf("error = %v, want %v", err, tc.restart)
			}
		})
	}
}

// An expired cleanup context ends the retry loop at once instead
// of sleeping out the remaining delays.
func TestRetryDeleteStopsWhenContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	boom := errors.New("apiserver down")
	start := time.Now()
	err := retryDelete(ctx, 5, time.Hour, func() error {
		calls++
		cancel() // the budget runs out during the first attempt
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the last delete error", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retries once ctx is done)", calls)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %v; the wait must not outlive ctx", took)
	}
}

// A Create that fails AFTER the server stored the lock (the
// caller's context expired mid-request) must not leave a lock nobody holds.
func TestAcquireMigrationLockDropsItsOwnOrphan(t *testing.T) {
	secrets := corev1.SchemeGroupVersion.WithResource("secrets")
	failCreate := func(cs *fake.Clientset, persist bool) {
		cs.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
			if persist {
				if err := cs.Tracker().Create(secrets, a.(k8stesting.CreateAction).GetObject(), caNamespace); err != nil {
					return true, nil, err
				}
			}
			return true, nil, context.DeadlineExceeded
		})
	}
	foreign := func(data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: etcdMigrationLockSecret, Namespace: caNamespace}, Data: data}
	}

	t.Run("persisted then failed: removed", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		failCreate(cs, true)
		release, err := acquireMigrationLock(io.Discard, context.Background(), cs, "cp1")
		if err == nil || release != nil {
			t.Fatalf("release=%v err=%v, want an error and no release closure", release != nil, err)
		}
		if hasMigrationLock(t, cs) {
			t.Error("our orphaned lock was left behind and would block every later migration")
		}
	})
	t.Run("someone else's lock with another token: kept", func(t *testing.T) {
		cs := fake.NewSimpleClientset(foreign(map[string][]byte{"holder": []byte("cp2"), "token": []byte("not-ours")}))
		failCreate(cs, false)
		if _, err := acquireMigrationLock(io.Discard, context.Background(), cs, "cp1"); err == nil {
			t.Fatal("want an error")
		}
		if !hasMigrationLock(t, cs) {
			t.Error("deleted a lock that carries another token")
		}
	})
	t.Run("lock without a token: kept", func(t *testing.T) {
		cs := fake.NewSimpleClientset(foreign(map[string][]byte{"holder": []byte("old-binary")}))
		failCreate(cs, false)
		if _, err := acquireMigrationLock(io.Discard, context.Background(), cs, "cp1"); err == nil {
			t.Fatal("want an error")
		}
		if !hasMigrationLock(t, cs) {
			t.Error("deleted a lock that carries no token")
		}
	})
	t.Run("our own attempt landed and a retry saw AlreadyExists: we hold it", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
			obj := a.(k8stesting.CreateAction).GetObject()
			if err := cs.Tracker().Create(secrets, obj, caNamespace); err != nil {
				return true, nil, err
			}
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, etcdMigrationLockSecret)
		})
		release, err := acquireMigrationLock(io.Discard, context.Background(), cs, "cp1")
		if err != nil || release == nil {
			t.Fatalf("release=%v err=%v, want the lock we stored to count as ours", release != nil, err)
		}
		release()
		if hasMigrationLock(t, cs) {
			t.Error("release left the lock behind")
		}
	})
}

// When the owner lookup after a failed Create cannot be done, the
// lock may be stranded; say so with the way to remove it. A NotFound means
// nothing was stored and stays silent.
func TestRemoveOwnLockWarnsWhenItCannotCheck(t *testing.T) {
	getErr := func(err error) func(*fake.Clientset) {
		return func(c *fake.Clientset) {
			c.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, err
			})
		}
	}
	for _, tc := range []struct {
		name     string
		prep     func(*fake.Clientset)
		wantWarn bool
	}{
		{name: "not found stays silent"},
		{name: "api error warns", prep: getErr(errors.New("apiserver down")), wantWarn: true},
		{name: "timeout warns", prep: getErr(context.DeadlineExceeded), wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			if tc.prep != nil {
				tc.prep(cs)
			}
			out := runCapture(t, func(w io.Writer) { removeOwnLock(w, cs, "tok") })
			if !tc.wantWarn {
				if out != "" {
					t.Errorf("printed %q, want nothing", out)
				}
				return
			}
			for _, want := range []string{"WARN", caNamespace + "/" + etcdMigrationLockSecret, "kubectl -n " + caNamespace + " delete secret " + etcdMigrationLockSecret} {
				if !strings.Contains(out, want) {
					t.Errorf("output %q missing %q", out, want)
				}
			}
		})
	}
	t.Run("a foreign lock stays silent", func(t *testing.T) {
		cs := fake.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: etcdMigrationLockSecret, Namespace: caNamespace},
			Data:       map[string][]byte{"token": []byte("not-ours")},
		})
		if out := runCapture(t, func(w io.Writer) { removeOwnLock(w, cs, "tok") }); out != "" {
			t.Errorf("printed %q, want nothing", out)
		}
		if !hasMigrationLock(t, cs) {
			t.Error("foreign lock was removed")
		}
	})
}

// annotated is a stratum-carotate Secret as rotate-ca --cluster (runID != "") or
// migrate-etcd-ca (runID == "") stages it.
func annotated(runID string) *corev1.Secret {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: caRotateKeySecret, Namespace: caNamespace},
		Data:       map[string][]byte{"certificate-key": []byte("live-key")},
	}
	if runID != "" {
		sec.Annotations = map[string]string{carotateRunAnnotation: runID}
	}
	return sec
}

func keyPresent(t *testing.T, cs *fake.Clientset) bool {
	t.Helper()
	_, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
	return err == nil
}

// The key Secret is shared with the etcd-CA migration, so a failed
// stage may remove it only when it carries THIS attempt's run id.
func TestRemoveOwnCarotateKeyOnlyRemovesItsOwnSecret(t *testing.T) {
	t.Run("own run id: removed", func(t *testing.T) {
		cs := fake.NewSimpleClientset(annotated("run-1"))
		removeOwnCarotateKey(io.Discard, cs, "run-1")
		if keyPresent(t, cs) {
			t.Error("our own stored key was left behind")
		}
	})
	t.Run("another run's id, a migration's key (no id), or no run id: kept", func(t *testing.T) {
		for _, tc := range []struct{ name, stored, ours string }{
			{"another run", "run-2", "run-1"},
			{"migration key", "", "run-1"},
			{"empty run id", "run-1", ""},
		} {
			cs := fake.NewSimpleClientset(annotated(tc.stored))
			out := runCapture(t, func(w io.Writer) { removeOwnCarotateKey(w, cs, tc.ours) })
			if !keyPresent(t, cs) {
				t.Errorf("%s: a Secret that is not ours was removed", tc.name)
			}
			if out != "" {
				t.Errorf("%s: printed %q, want nothing", tc.name, out)
			}
		}
	})
	t.Run("absent: silent", func(t *testing.T) {
		if out := runCapture(t, func(w io.Writer) { removeOwnCarotateKey(w, fake.NewSimpleClientset(), "run-1") }); out != "" {
			t.Errorf("printed %q, want nothing", out)
		}
	})
	t.Run("cannot read it: warns with the manual command", func(t *testing.T) {
		cs := fake.NewSimpleClientset(annotated("run-1"))
		cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver down")
		})
		out := runCapture(t, func(w io.Writer) { removeOwnCarotateKey(w, cs, "run-1") })
		for _, want := range []string{"WARN", caNamespace + "/" + caRotateKeySecret, "ca.key", "kubectl -n " + caNamespace + " delete secret " + caRotateKeySecret} {
			if !strings.Contains(out, want) {
				t.Errorf("output %q missing %q", out, want)
			}
		}
	})
}

// A put that failed because another operation holds the Secret must leave theirs.
func TestStageCarotateKeyLeavesAnotherOperationsSecret(t *testing.T) {
	for _, stored := range []string{"", "other-run"} {
		cs := fake.NewSimpleClientset(annotated(stored))
		cs.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("conflict while updating")
		})
		if err := stageCarotateKey(io.Discard, context.Background(), cs, "new-key", "run-1"); err == nil {
			t.Fatalf("stored %q: want the update failure", stored)
		}
		if !keyPresent(t, cs) {
			t.Errorf("stored %q: a live Secret held by someone else was removed", stored)
		}
	}
}

// Cleanup goes before the repin. The repin's API calls can stall for as long
// as their deadline allows, so a stalled one must not keep the leftover key
// from being reaped. This drives the real RepinCA request through a server that never answers
// until released, rather than an injected blocking function.
func TestRestartThenCleanupRunsCleanupBeforeAStalledRepin(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","reason":"NotFound","code":404}`)
	}))
	defer srv.Close()
	conf := filepath.Join(t.TempDir(), "admin.conf")
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: %s\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n", srv.URL)
	if err := os.WriteFile(conf, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	old := version.AdminConf
	version.AdminConf = conf
	defer func() { version.AdminConf = old }()

	cleaned, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = restartThenCleanup(io.Discard, CARotateOptions{Quiet: true}, "rollback",
			func() error { return errors.New("etcd restart failed") },
			func(io.Writer) { close(cleaned) }, repinBestEffort)
	}()
	select {
	case <-entered:
		select {
		case <-cleaned:
		default:
			t.Error("the RepinCA request was stalled before the leftover-key cleanup ran")
		}
	case <-time.After(5 * time.Second):
		t.Error("the RepinCA request never reached the server")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("restartThenCleanup did not finish after the repin was released")
	}
}

// TestRepinBestEffortRepublishesThroughTheAdminClientSeam: the best-effort
// repin rides the client the package's admin seam builds, so a fixture that
// stands a fake cluster in for admin.conf is the cluster-info it republishes.
// A client built from admin.conf behind the seam's back dials whatever that
// file names — under the VIP fixture, an address nothing answers, waited out
// on every trust-new, finalize and rollback — and republishes nothing.
func TestRepinBestEffortRepublishesThroughTheAdminClientSeam(t *testing.T) {
	prepPinned(t)
	old, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	kc := clientcmdapi.NewConfig()
	kc.Clusters[""] = &clientcmdapi.Cluster{Server: "https://10.0.0.9:6443", CertificateAuthorityData: old}
	raw, err := clientcmd.Write(*kc)
	if err != nil {
		t.Fatal(err)
	}
	cs := fakeAdminClientset(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: "kube-public"},
		Data:       map[string]string{clusterinfo.KubeConfigKey: string(raw)},
	})
	oldConf := version.AdminConf
	t.Cleanup(func() { version.AdminConf = oldConf })
	version.AdminConf = filepath.Join(t.TempDir(), "absent.conf") // only the seam reaches a cluster

	out := runCapture(t, func(w io.Writer) { repinBestEffort(w, CARotateOptions{}) })
	if out != "" {
		t.Errorf("the repin warned: %q", out)
	}
	cm, err := cs.CoreV1().ConfigMaps("kube-public").Get(context.Background(), "cluster-info", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server, ca, err := clusterinfo.Parse([]byte(cm.Data[clusterinfo.KubeConfigKey]))
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := pki.ReadRootTrust()
	if err != nil {
		t.Fatal(err)
	}
	if string(ca) != string(want) {
		t.Errorf("cluster-info publishes %d CA(s), want the current old‖new trust (%d)", countPEM(ca), countPEM(want))
	}
	if server != "https://10.0.0.9:6443" {
		t.Errorf("cluster-info server = %q, want the advertised endpoint preserved", server)
	}
}

// A rollback after a halted trust-new records no run marker on this node (sign-new
// was never reached), so it cannot prove the key is spent: another follower of the
// same run may still be fetching it. It leaves the key and says so, with what the
// key protects and a conditional manual step. Safe automatic removal is a lockless-cleanup
// follow-up.
func TestRollbackWithNoMarkerLeavesTheKeyAndWarns(t *testing.T) {
	for _, restartErr := range []error{nil, errors.New("etcd not serving")} {
		cs := fake.NewSimpleClientset(keyAt("k", "1", run1)) // the run's key, staged and stranded by a halted trust-new
		deletes := preconditionedDeletes(t, cs)
		stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
		withRunMarker(t, "") // no sign-new, so no marker
		var out string
		_ = restartThenCleanup(io.Discard, CARotateOptions{Quiet: true}, "rollback",
			func() error { return restartErr },
			func(w io.Writer) {
				out = runCapture(t, func(w2 io.Writer) { reapLeftoverCarotateKey(w2, context.Background(), cs) })
			},
			func(io.Writer, CARotateOptions) {})
		if !hasSecret(t, cs, caRotateKeySecret) || len(*deletes) != 0 {
			t.Errorf("restart error %v: the key was deleted without ownership evidence (deletes=%+v)", restartErr, *deletes)
		}
		wantKeyWarning(t, out)
		for _, want := range []string{"leaving " + caNamespace + "/" + caRotateKeySecret + " in place", "If none is running, remove it by hand", "kubectl -n " + caNamespace + " delete secret " + caRotateKeySecret} {
			if !strings.Contains(out, want) {
				t.Errorf("restart error %v: output %q missing %q", restartErr, out, want)
			}
		}
	}
}

// wantKeyWarning checks a recovery WARN names the Secret and says what the key
// protects, and that its manual step is conditional.
func wantKeyWarning(t *testing.T, out string) {
	t.Helper()
	for _, want := range []string{"WARN:", caNamespace + "/" + caRotateKeySecret, "ca.key", "sa.key", "front-proxy-ca.key", "kubectl -n " + caNamespace + " delete secret"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
	if !strings.Contains(out, "If ") {
		t.Errorf("output %q: the manual step must be conditional", out)
	}
}

// Every recovery path that leaves the key in the cluster must say what it protects.
func TestRecoveryWarningsExplainWhatTheKeyProtects(t *testing.T) {
	for _, tc := range []struct {
		name string
		objs []runtime.Object
		prep func(*fake.Clientset)
		run  string
	}{
		{name: "unknown ownership (no marker)", objs: []runtime.Object{keyAt("k", "1", run1)}},
		{name: "another run's key", objs: []runtime.Object{keyAt("k", "1", "run-2")}, run: run1},
		{name: "migration lock held", objs: []runtime.Object{keyAt("k", "1", run1), secret(etcdMigrationLockSecret, nil)}, run: run1},
		{name: "lock unreadable", objs: []runtime.Object{keyAt("k", "1", run1)}, run: run1, prep: func(cs *fake.Clientset) {
			cs.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.(k8stesting.GetAction).GetName() == etcdMigrationLockSecret {
					return true, nil, errors.New("apiserver down")
				}
				return false, nil, nil
			})
		}},
		{name: "key unreadable", objs: []runtime.Object{keyAt("k", "1", run1)}, run: run1, prep: func(cs *fake.Clientset) {
			cs.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if a.(k8stesting.GetAction).GetName() == caRotateKeySecret {
					return true, nil, errors.New("apiserver down")
				}
				return false, nil, nil
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(tc.objs...)
			if tc.prep != nil {
				tc.prep(cs)
			}
			stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
			withRunMarker(t, tc.run)
			out := runCapture(t, func(w io.Writer) { reapLeftoverCarotateKey(w, context.Background(), cs) })
			wantKeyWarning(t, out)
		})
	}
}

// TestPublishStagedKeyGuidesEveryFailure: every exit after the initiator's
// trust-new carries the halt guidance, including the ones before any follower job
// runs. The publish steps run in order and each failure is guided.
func TestPublishStagedKeyGuidesEveryFailure(t *testing.T) {
	boom := errors.New("boom")
	ok := stagedKeyOps{
		newKey:   func() (string, error) { return "key", nil },
		upload:   func(string) error { return nil },
		newRunID: func() (string, error) { return "run", nil },
		stage:    func(context.Context, string, string) error { return nil },
	}
	runID, err := publishStagedKey(context.Background(), ok)
	if err != nil || runID != "run" {
		t.Fatalf("runID=%q err=%v, want the run id and no error", runID, err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*stagedKeyOps)
		what string
	}{
		{"new key", func(o *stagedKeyOps) { o.newKey = func() (string, error) { return "", boom } }, "generate the staged-material key"},
		{"upload", func(o *stagedKeyOps) { o.upload = func(string) error { return boom } }, "publish staged material"},
		{"run id", func(o *stagedKeyOps) { o.newRunID = func() (string, error) { return "", boom } }, "generate the rotate-ca run id"},
		{"stage", func(o *stagedKeyOps) { o.stage = func(context.Context, string, string) error { return boom } }, "stage carotate key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := ok
			tc.mut(&ops)
			_, err := publishStagedKey(context.Background(), ops)
			if !errors.Is(err, boom) {
				t.Fatalf("error = %v, want it to wrap the cause", err)
			}
			for _, want := range []string{tc.what, "the run halted in trust-new", "stratum certs check", "--rollback --yes"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
	t.Run("a failed upload stops before a key is staged", func(t *testing.T) {
		staged := false
		ops := ok
		ops.upload = func(string) error { return boom }
		ops.stage = func(context.Context, string, string) error { staged = true; return nil }
		if _, err := publishStagedKey(context.Background(), ops); err == nil || staged {
			t.Errorf("err=%v staged=%v, want a failure and no stage", err, staged)
		}
	})
}

func TestHaltedStep(t *testing.T) {
	if err := haltedStep(PhaseSignNew, "rebuild", nil); err != nil {
		t.Errorf("nil error must stay nil, got %v", err)
	}
	cause := errors.New("tls: unknown authority")
	for _, tc := range []struct{ phase, what string }{
		{PhaseTrustNew, "rebuild admin client (old‖new trust) after trust-new"},
		{PhaseSignNew, "rebuild admin client (new-CA cert) after sign-new"},
		{PhaseSignNew, "finalize refused (cluster left in the safe old‖new state)"},
	} {
		err := haltedStep(tc.phase, tc.what, cause)
		for _, want := range []string{tc.what, "the run halted in " + tc.phase, "stratum certs check", "--rollback --yes"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error = %v, want it to contain %q", tc.phase, err, want)
			}
		}
		if !errors.Is(err, cause) {
			t.Errorf("%s: error = %v, want it to wrap the cause", tc.phase, err)
		}
	}
}

// TestCAJobLimitsCoverTheRestartSequence: the remote phase Job limits must
// cover the worst case of the restart sequence they run, built from the same
// budgets the sequence uses, so raising a gate without the Job fails here.
func TestCAJobLimitsCoverTheRestartSequence(t *testing.T) {
	worst := serveBudget("etcd") +
		3*serveBudget("kube-apiserver") + // apiserver, controller-manager, scheduler
		healthzAttempts*k8sutil.HealthzPollInterval +
		4*downWindowWait + // etcd, apiserver, controller-manager, scheduler
		time.Duration(len(hostRestartOrder))*90*time.Second // kubelet and operator (comps.Restart waits up to 90s each)
	if caJobDeadline <= worst {
		t.Errorf("caJobDeadline = %v, want more than the worst-case restart sequence %v", caJobDeadline, worst)
	}
	// nodejob polls once every 5s; the poll budget must not end before the deadline.
	if polled := time.Duration(caJobPollAttempts) * 5 * time.Second; polled < caJobDeadline {
		t.Errorf("poll budget %v is shorter than the Job deadline %v", polled, caJobDeadline)
	}
}

// TestDeleteWarningReportsTheRealAttemptCount: the delete WARN reports the
// attempts really made when the context ends the retries early.
func TestDeleteWarningReportsTheRealAttemptCount(t *testing.T) {
	cs := fake.NewSimpleClientset(keyAt("k", "1", run1))
	stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
	carotateCleanupDelay = time.Hour // only ctx.Done can end the wait between attempts
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tries := 0
	cs.PrependReactor("delete", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		tries++
		cancel() // the budget runs out during the first attempt
		return true, nil, errors.New("apiserver down")
	})
	out := runCapture(t, func(w io.Writer) { cleanupCarotateKeySnapshot(ctx, w, cs, nil) })
	if tries != 1 {
		t.Fatalf("deletes = %d, want 1", tries)
	}
	if want := fmt.Sprintf("after 1 of up to %d attempts", carotateCleanupAttempts); !strings.Contains(out, want) {
		t.Errorf("output %q, want it to say %q", out, want)
	}
	if strings.Contains(out, fmt.Sprintf("after %d attempts", carotateCleanupAttempts)) {
		t.Errorf("output %q claims every attempt was made", out)
	}
}

// TestHostRestartOrderIsTheUnitsWithoutContainerd pins the rotation's host
// restart set as a relation to the canonical roster: every unit comps.Units
// names but containerd, in Units order — so the operator, which authenticates
// with admin.kubeconfig, is restarted last. containerd holds no rotated
// material; a unit added to the roster that the rotation never restarts, or a
// restart order that stops agreeing with it, fails here.
func TestHostRestartOrderIsTheUnitsWithoutContainerd(t *testing.T) {
	want := make([]string, 0, len(comps.Units))
	for _, u := range comps.Units {
		if u != "containerd" {
			want = append(want, u)
		}
	}
	if !slices.Equal(hostRestartOrder, want) {
		t.Errorf("hostRestartOrder = %v, want comps.Units without containerd, in Units order: %v", hostRestartOrder, want)
	}
}

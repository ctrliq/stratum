package certs

import (
	"bytes"

	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/version"
)

// TestRetryDelete pins the cleanup policy: it retries a failing delete,
// succeeds once the delete succeeds, treats an already-absent Secret as success,
// and returns the last error when every attempt fails (so the caller warns).
func TestRetryDelete(t *testing.T) {
	t.Run("succeeds on a later attempt", func(t *testing.T) {
		calls := 0
		err := retryDelete(context.Background(), 5, 0, func() error {
			calls++
			if calls < 3 {
				return errors.New("apiserver 503")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil once the delete succeeds", err)
		}
		if calls != 3 {
			t.Errorf("calls = %d, want 3 (retried until success)", calls)
		}
	})

	t.Run("NotFound is success, no further retries", func(t *testing.T) {
		calls := 0
		err := retryDelete(context.Background(), 5, 0, func() error {
			calls++
			return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, caRotateKeySecret)
		})
		if err != nil || calls != 1 {
			t.Errorf("NotFound must be treated as done: err=%v calls=%d", err, calls)
		}
	})

	t.Run("returns the last error when all attempts fail", func(t *testing.T) {
		calls := 0
		boom := errors.New("apiserver down")
		err := retryDelete(context.Background(), 4, 0, func() error { calls++; return boom })
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want the last failure surfaced so the caller warns", err)
		}
		if calls != 4 {
			t.Errorf("calls = %d, want 4 (exhausted attempts)", calls)
		}
	})
}

// TestCleanupCarotateKeyNilClientDoesNotPanic: cleanup runs in a defer, so a nil
// client (e.g. a failed AdminClientset rebuild) must warn, not panic.
func TestCleanupCarotateKeyNilClientDoesNotPanic(t *testing.T) {
	cleanupCarotateKey(context.Background(), nil)
}

func secret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: caNamespace}, Data: data}
}

func hasSecret(t *testing.T, cs kubernetes.Interface, name string) bool {
	t.Helper()
	_, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get %s: %v", name, err)
	}
	return err == nil
}

// stubGate replaces the apiserver health gate for the test.
func stubGate(t *testing.T, fn func(context.Context, kubernetes.Interface) error) {
	t.Helper()
	old, oldDelay := waitAPIServer, carotateCleanupDelay
	t.Cleanup(func() { waitAPIServer, carotateCleanupDelay = old, oldDelay })
	waitAPIServer, carotateCleanupDelay = fn, 0
}

// runCapture runs fn on a fresh buffer and returns what it wrote. The cleanup
// helpers take the writer themselves, so the capture is the
// buffer, not an os.Stdout redirect.
func runCapture(t *testing.T, fn func(io.Writer)) string {
	t.Helper()
	var b bytes.Buffer
	fn(&b)
	return b.String()
}

// TestCleanupCarotateKeyWaitsForAPIServer pins the ordering: the delete must not be
// attempted until the apiserver health gate has run, and must still be attempted
// (and succeed) afterwards. A gate that never opens does not skip the delete.
func TestCleanupCarotateKeyWaitsForAPIServer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		gateErr error
	}{
		{"gate opens", nil},
		{"gate never opens", errors.New("apiserver /healthz did not become ok")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(secret(caRotateKeySecret, nil))
			gated := false
			stubGate(t, func(context.Context, kubernetes.Interface) error { gated = true; return tc.gateErr })
			cs.PrependReactor("delete", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				if !gated {
					return true, nil, errors.New("connection reset by peer")
				}
				return false, nil, nil
			})
			cleanupCarotateKey(context.Background(), cs)
			if hasSecret(t, cs, caRotateKeySecret) {
				t.Error("Secret still present after cleanup; the delete must run after the health gate")
			}
		})
	}
}

// TestCleanupCarotateKeyGateLeavesTimeToDelete drives the production gate and a
// real client-go transport against a /healthz that never answers. The gate must
// stop on its own budget so the DELETE still reaches the server on the same
// context (a gate that spent the whole cleanup budget left every
// DELETE with an expired context and the key behind).
func TestCleanupCarotateKeyGateLeavesTimeToDelete(t *testing.T) {
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			<-r.Context().Done() // hang until the client gives up
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldBudget, oldDelay := carotateGateBudget, carotateCleanupDelay
	t.Cleanup(func() { carotateGateBudget, carotateCleanupDelay = oldBudget, oldDelay })
	carotateGateBudget, carotateCleanupDelay = time.Minute, 0 // the ctx deadline, not the cap, must bound the gate

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	cleanupCarotateKey(ctx, cs)
	if got := deletes.Load(); got != 1 {
		t.Errorf("DELETE requests = %d, want 1: a hung /healthz must not consume the delete's time (elapsed %v)", got, time.Since(start))
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("cleanup took %v, want it inside the 2s context", time.Since(start))
	}
}

// TestReapLeftoverCarotateKey: the finalize/rollback reaper deletes a true
// leftover silently, and leaves the key — warning — whenever an etcd-CA migration
// holds its lock or the lock cannot be read (a local idle
// transition deleted the key a live migration's followers still needed).
func TestReapLeftoverCarotateKey(t *testing.T) {
	lock := secret(etcdMigrationLockSecret, map[string][]byte{"holder": []byte("cp1"), "since": []byte("2026-10-05T23:00:00Z")})
	own := runtime.Object(keyAt("old", "1", run1))
	for _, tc := range []struct {
		name     string
		objs     []runtime.Object
		lockErr  error
		wantKey  bool
		wantWarn string // substring; "" = no output at all
	}{
		{name: "leftover is removed silently", objs: []runtime.Object{own}},
		{name: "nothing to remove is silent"},
		{name: "active migration keeps the key", objs: []runtime.Object{own, lock}, wantKey: true, wantWarn: "holder=cp1"},
		{name: "unreadable lock keeps the key", objs: []runtime.Object{own}, lockErr: errors.New("boom"), wantKey: true, wantWarn: "cannot confirm no etcd-CA migration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(tc.objs...)
			stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
			withRunMarker(t, run1)
			if tc.lockErr != nil {
				cs.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
					if a.(k8stesting.GetAction).GetName() == etcdMigrationLockSecret {
						return true, nil, tc.lockErr
					}
					return false, nil, nil
				})
			}
			out := runCapture(t, func(w io.Writer) { reapLeftoverCarotateKey(w, context.Background(), cs) })
			if got := hasSecret(t, cs, caRotateKeySecret); got != tc.wantKey {
				t.Errorf("key present = %v, want %v", got, tc.wantKey)
			}
			if tc.wantWarn == "" && out != "" || !strings.Contains(out, tc.wantWarn) {
				t.Errorf("output = %q, want substring %q", out, tc.wantWarn)
			}
			_, lockErr := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), caNamespace, etcdMigrationLockSecret)
			if wantLock := slices.Contains(tc.objs, runtime.Object(lock)); (lockErr == nil) != wantLock {
				t.Errorf("migration lock present = %v, want %v: the reaper must never touch it", lockErr == nil, wantLock)
			}
		})
	}
}

// preconditionedDeletes makes the fake clientset enforce delete preconditions
// the way the apiserver does, and records each delete's options.
func preconditionedDeletes(t *testing.T, cs *fake.Clientset) *[]metav1.DeleteOptions {
	t.Helper()
	var seen []metav1.DeleteOptions
	cs.PrependReactor("delete", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		da := a.(k8stesting.DeleteAction)
		opts := da.GetDeleteOptions()
		seen = append(seen, opts)
		if da.GetName() != caRotateKeySecret || opts.Preconditions == nil {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(corev1.SchemeGroupVersion.WithResource("secrets"), caNamespace, da.GetName())
		if err != nil {
			return true, nil, err
		}
		cur := obj.(*corev1.Secret)
		if p := opts.Preconditions; (p.UID != nil && *p.UID != cur.UID) || (p.ResourceVersion != nil && *p.ResourceVersion != cur.ResourceVersion) {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, da.GetName(), errors.New("precondition failed"))
		}
		return false, nil, nil
	})
	return &seen
}

// keyAt is the key Secret as the apiserver stores it, staged by rotate-ca run
// `run` ("" = a migrate-etcd-ca key, which carries no run annotation).
func keyAt(uid, rv, run string) *corev1.Secret {
	s := secret(caRotateKeySecret, map[string][]byte{"certificate-key": []byte(uid)})
	s.UID, s.ResourceVersion = types.UID(uid), rv
	if run != "" {
		s.Annotations = map[string]string{carotateRunAnnotation: run}
	}
	return s
}

const run1 = "run-1"

// withRunMarker points the node's PKI dir at a temp dir holding the run marker
// (none if run == ""), the way sign-new leaves it.
func withRunMarker(t *testing.T, run string) {
	t.Helper()
	old := version.PKIDir
	t.Cleanup(func() { version.PKIDir = old })
	version.PKIDir = t.TempDir()
	if run != "" {
		if err := os.WriteFile(carotateRunMarker(), []byte(run+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReapLeftoverCarotateKeyRacingMigration is the race where a
// migrate-etcd-ca takes the lock and publishes its key AFTER recovery has read
// the lock. Recovery deletes only the key it read first, so the replacement
// survives; an unconditional delete (or one that read the key after the lock)
// would remove the live key.
func TestReapLeftoverCarotateKeyRacingMigration(t *testing.T) {
	cs := fake.NewSimpleClientset(keyAt("old", "1", run1))
	deletes := preconditionedDeletes(t, cs)
	stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
	withRunMarker(t, run1)
	secrets := corev1.SchemeGroupVersion.WithResource("secrets")
	cs.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() != etcdMigrationLockSecret {
			return false, nil, nil
		}
		// The lock read answers "absent"; the migration starts right after it.
		if err := cs.Tracker().Update(secrets, keyAt("new", "2", ""), caNamespace); err != nil {
			t.Fatal(err)
		}
		if err := cs.Tracker().Add(secret(etcdMigrationLockSecret, nil)); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, etcdMigrationLockSecret)
	})
	out := runCapture(t, func(w io.Writer) { reapLeftoverCarotateKey(w, context.Background(), cs) })
	got, err := cs.Tracker().Get(secrets, caNamespace, caRotateKeySecret)
	if err != nil || got.(*corev1.Secret).UID != "new" {
		t.Fatalf("the migration's key was deleted by recovery: obj=%v err=%v", got, err)
	}
	if len(*deletes) != 1 || (*deletes)[0].Preconditions == nil {
		t.Errorf("deletes = %+v, want exactly one, carrying preconditions", *deletes)
	}
	if !strings.Contains(out, "replaced while recovery was running") {
		t.Errorf("output = %q, want the leave-alone warning", out)
	}
}

// TestReapLeftoverCarotateKeyAbsentIsNoop: a key that is absent at the first read
// means nothing to do — the lock is not even consulted and nothing is deleted,
// so a migration that publishes later is never touched.
func TestReapLeftoverCarotateKeyAbsentIsNoop(t *testing.T) {
	cs := fake.NewSimpleClientset()
	deletes := preconditionedDeletes(t, cs)
	stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
	withRunMarker(t, run1)
	lockRead := false
	cs.PrependReactor("get", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		lockRead = lockRead || a.(k8stesting.GetAction).GetName() == etcdMigrationLockSecret
		return false, nil, nil
	})
	if out := runCapture(t, func(w io.Writer) { reapLeftoverCarotateKey(w, context.Background(), cs) }); out != "" {
		t.Errorf("output = %q, want silence", out)
	}
	if lockRead || len(*deletes) != 0 {
		t.Errorf("lockRead=%v deletes=%d, want neither for an absent key", lockRead, len(*deletes))
	}
	if readCarotateRun() != "" {
		t.Error("the marker survived although its key is known gone")
	}
}

// TestReapLeftoverCarotateKeyRetriesKeepPreconditions: every retry of a transient
// delete failure carries the same UID/resourceVersion of the snapshot.
func TestReapLeftoverCarotateKeyRetriesKeepPreconditions(t *testing.T) {
	cs := fake.NewSimpleClientset(keyAt("old", "1", run1))
	stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
	withRunMarker(t, run1)
	calls := 0
	cs.PrependReactor("delete", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if calls++; calls <= 2 {
			return true, nil, errors.New("connection reset by peer")
		}
		return false, nil, nil
	})
	deletes := preconditionedDeletes(t, cs) // registered last, so it runs first and records every attempt
	runCapture(t, func(w io.Writer) { reapLeftoverCarotateKey(w, context.Background(), cs) })
	if hasSecret(t, cs, caRotateKeySecret) {
		t.Fatal("leftover key still present after retries")
	}
	if len(*deletes) != 3 {
		t.Fatalf("deletes = %d, want 3 (two transient failures, then success)", len(*deletes))
	}
	for i, o := range *deletes {
		if o.Preconditions == nil || *o.Preconditions.UID != "old" || *o.Preconditions.ResourceVersion != "1" {
			t.Errorf("delete %d preconditions = %+v, want the snapshot's UID/resourceVersion", i, o.Preconditions)
		}
	}
}

// TestReapLeftoverCarotateKeyOwnership: recovery removes only the key of the run
// this node promoted under. Without a marker, with an unannotated key, or with a
// different run's key it leaves the key and says why, including when a later rotation
// publishes before the snapshot.
func TestReapLeftoverCarotateKeyOwnership(t *testing.T) {
	for _, tc := range []struct {
		name      string
		marker    string
		key       *corev1.Secret
		wantMark  string // marker after recovery
		wantWarn  bool
		wantStays bool
	}{
		{"no marker: this node never promoted under a recorded run", "", keyAt("k", "1", run1), "", true, true},
		{"a later rotation published before the snapshot", run1, keyAt("k2", "7", "run-2"), "", true, true},
		{"an unannotated migrate-etcd-ca key", run1, keyAt("k", "1", ""), "", true, true},
		{"the key of this node's run", run1, keyAt("k", "1", run1), "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(tc.key)
			deletes := preconditionedDeletes(t, cs)
			stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
			withRunMarker(t, tc.marker)
			out := runCapture(t, func(w io.Writer) { reapLeftoverCarotateKey(w, context.Background(), cs) })
			if got := hasSecret(t, cs, caRotateKeySecret); got != tc.wantStays {
				t.Errorf("key present = %v, want %v", got, tc.wantStays)
			}
			if tc.wantStays && len(*deletes) != 0 {
				t.Errorf("sent %d deletes for a key that is not this node's", len(*deletes))
			}
			if warned := strings.Contains(out, "kubectl -n kube-system delete secret stratum-carotate"); warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v (output %q)", warned, tc.wantWarn, out)
			}
			if got := readCarotateRun(); got != tc.wantMark {
				t.Errorf("marker = %q, want %q", got, tc.wantMark)
			}
		})
	}
}

// TestReapLeftoverCarotateKeyRetriedFinalize: eligibility lives in the marker, not
// in the PKI scratch files, so it survives a finalize that fails after
// FinalizeCA removed the backups and a finalize whose delete failed.
func TestReapLeftoverCarotateKeyRetriedFinalize(t *testing.T) {
	cs := fake.NewSimpleClientset(keyAt("k", "1", run1))
	stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
	withRunMarker(t, run1) // no ca.pre-carotate.crt anywhere: FinalizeCA already ran
	if signerPromoted() {
		t.Fatal("test setup: the .pre-carotate backup must be gone")
	}
	failing := true
	cs.PrependReactor("delete", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, errors.New("connection reset by peer")
		}
		return false, nil, nil
	})
	out := runCapture(t, func(w io.Writer) { reapLeftoverCarotateKey(w, context.Background(), cs) })
	if !hasSecret(t, cs, caRotateKeySecret) || readCarotateRun() != run1 {
		t.Fatalf("failed delete: key present=%v marker=%q, want the key kept and the marker kept for the retry", hasSecret(t, cs, caRotateKeySecret), readCarotateRun())
	}
	if !strings.Contains(out, "could not remove the transient CA-rotation key") {
		t.Errorf("output = %q, want the could-not-remove warning", out)
	}
	failing = false // the retried finalize
	if out := runCapture(t, func(w io.Writer) { reapLeftoverCarotateKey(w, context.Background(), cs) }); out != "" {
		t.Errorf("retry output = %q, want silence", out)
	}
	if hasSecret(t, cs, caRotateKeySecret) || readCarotateRun() != "" {
		t.Errorf("after the retry: key present=%v marker=%q, want both gone", hasSecret(t, cs, caRotateKeySecret), readCarotateRun())
	}
}

// TestRecordCarotateRun: sign-new records the run id of the key staged at that
// moment, and anything less — no key, an unannotated key — clears an older marker.
func TestRecordCarotateRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		objs []runtime.Object
		want string
	}{
		{"annotated key", []runtime.Object{keyAt("k", "1", run1)}, run1},
		{"no key (manual phase)", nil, ""},
		{"unannotated key", []runtime.Object{keyAt("k", "1", "")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRunMarker(t, "stale-run")
			recordCarotateRunFrom(io.Discard, context.Background(), fake.NewSimpleClientset(tc.objs...))
			if got := readCarotateRun(); got != tc.want {
				t.Errorf("marker = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRecordCarotateRunReadsThroughTheAdminClientSeam: sign-new records the
// run from the client the package's admin seam builds, so a fixture that
// stands a fake cluster in for admin.conf is the cluster sign-new reads. A
// client built from admin.conf behind the seam's back dials whatever that
// file names — under the VIP fixture, an address nothing answers, waited out
// on every sign-new — and records nothing.
func TestRecordCarotateRunReadsThroughTheAdminClientSeam(t *testing.T) {
	key := keyAt("k", "1", run1)
	key.Annotations[carotateOperationAnnotation] = carotateOpRotateCA
	fakeAdminClientset(t, key)
	old := version.AdminConf
	t.Cleanup(func() { version.AdminConf = old })
	version.AdminConf = filepath.Join(t.TempDir(), "absent.conf") // only the seam reaches a cluster
	withRunMarker(t, "stale-run")
	recordCarotateRun(io.Discard)
	if got := readCarotateRun(); got != run1 {
		t.Errorf("marker = %q, want %q: sign-new did not read the staged key through the admin client seam", got, run1)
	}
}

// TestSignerPromoted: the .pre-carotate backup is the node-local evidence that
// sign-new was reached.
func TestSignerPromoted(t *testing.T) {
	old := version.PKIDir
	t.Cleanup(func() { version.PKIDir = old })
	version.PKIDir = t.TempDir()
	if signerPromoted() {
		t.Error("signerPromoted() = true with no backup")
	}
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca.pre-carotate.crt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !signerPromoted() {
		t.Error("signerPromoted() = false with the backup present")
	}
}

// TestStageCarotateKeyRemovesAmbiguousWrite: a Create that lands but whose
// response is lost returns an error before the caller's defer is registered; the
// key must not be left behind.
func TestStageCarotateKeyRemovesAmbiguousWrite(t *testing.T) {
	cs := fake.NewSimpleClientset()
	stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
	wrote := errors.New("response lost")
	cs.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if err := cs.Tracker().Add(a.(k8stesting.CreateAction).GetObject()); err != nil {
			t.Fatal(err)
		}
		return true, nil, wrote
	})
	if err := stageCarotateKey(io.Discard, context.Background(), cs, "deadbeef", run1); !errors.Is(err, wrote) {
		t.Fatalf("err = %v, want the write failure returned", err)
	}
	if hasSecret(t, cs, caRotateKeySecret) {
		t.Error("the key Secret the lost-response Create left behind was not removed")
	}
}

// TestStageCarotateKeyKeepsKeyOnSuccess: the success path leaves the key for the
// followers; the caller's defer removes it at the end of the run.
func TestStageCarotateKeyKeepsKeyOnSuccess(t *testing.T) {
	cs := fake.NewSimpleClientset()
	if err := stageCarotateKey(io.Discard, context.Background(), cs, "deadbeef", run1); err != nil {
		t.Fatal(err)
	}
	got, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("key Secret missing after a successful stage: %v", err)
	}
	if got.Annotations[carotateRunAnnotation] != run1 || got.Annotations[carotateOperationAnnotation] != carotateOpRotateCA {
		t.Errorf("staged annotations = %v, want the rotation's operation and run", got.Annotations)
	}
	// The owner type: a migration's put refuses the rotation's staged key —
	// the rotation's followers may still be fetching with it — instead of
	// overwriting it, and leaves the key as it stands.
	if err := putStagedKey(context.Background(), cs, "cafe", migrationStagedKeyOwner("cafe")); err == nil {
		t.Fatal("a migration's put overwrote a rotate-ca run's staged key")
	}
	got, _ = cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
	if string(got.Data["certificate-key"]) != "deadbeef" || got.Annotations[carotateRunAnnotation] != run1 {
		t.Errorf("staged key = %q annotations = %v, want the rotation's key left untouched", got.Data["certificate-key"], got.Annotations)
	}
}

// TestRemoveLeftoverCarotateKeyWithoutClient: when no admin client can be built
// the recovery step must not panic or fail the phase; it names the manual command.
func TestRemoveLeftoverCarotateKeyWithoutClient(t *testing.T) {
	old := version.AdminConf
	t.Cleanup(func() { version.AdminConf = old })
	version.AdminConf = filepath.Join(t.TempDir(), "absent.conf")
	out := runCapture(t, removeLeftoverCarotateKey)
	if !strings.Contains(out, "kubectl -n kube-system delete secret stratum-carotate") {
		t.Errorf("output = %q, want the manual delete command", out)
	}
}

// The staged-key owner annotations (carotateOperationAnnotation with
// carotateRunAnnotation) name the operation and the run that own every staged
// key Secret; the constants live beside the owner type in cacluster.go.

// ownerAnnotations is the annotation pair that names a staged key's owner.
func ownerAnnotations(op, run string) map[string]string {
	return map[string]string{
		carotateOperationAnnotation: op,
		carotateRunAnnotation:       run,
	}
}

// migrationKeyAt is the key Secret as the apiserver stores it, staged by
// migrate-etcd-ca: the operation annotation names the migration, and the run
// annotation is the one-way digest of the unique certificate key that names
// the migration's run.
func migrationKeyAt(uid, rv, certKey string) *corev1.Secret {
	s := keyAt(uid, rv, "")
	s.Annotations = migrationStagedKeyOwner(certKey).annotations()
	s.Data["certificate-key"] = []byte(certKey)
	return s
}

// TestKeyOperationsTouchOnlyTheirOwnKey pins one staged-key owner type. Every
// staged key carries the operation and run annotations that name its owner; a
// put refuses a Secret the other operation staged instead of overwriting it,
// and replaces a key of its own operation (a dead run's leftover, or this
// run's lost-response retry — the resume path); each operation's cleanup and
// the migration's reap remove only the key their own run put; and every delete
// carries the UID/resourceVersion preconditions of the snapshot it read.
func TestKeyOperationsTouchOnlyTheirOwnKey(t *testing.T) {
	// stageRotation is the rotation's staging entry, exactly as
	// publishStagedKey calls it; stageMigration is the owner-type put the way
	// PublishEtcdCA calls it, whose run annotation is a one-way digest of the
	// migration's own unique certificate key.
	stageRotation := func(ctx context.Context, cs kubernetes.Interface, keyHex string) error {
		return stageCarotateKey(io.Discard, ctx, cs, keyHex, run1)
	}
	stageMigration := func(ctx context.Context, cs kubernetes.Interface, keyHex string) error {
		return putStagedKey(ctx, cs, keyHex, migrationStagedKeyOwner(keyHex))
	}
	for _, tc := range []struct {
		name       string
		stage      func(context.Context, kubernetes.Interface, string) error
		op, ownRun string // the annotations the operation's own staged key carries
	}{
		{"rotate-ca --cluster", stageRotation, carotateOpRotateCA, run1},
		{"migrate-etcd-ca", stageMigration, carotateOpMigrateEtcdCA, migrationStagedKeyOwner("deadbeef").run},
	} {
		t.Run(tc.name+" stages a key carrying its operation and run", func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			if err := tc.stage(context.Background(), cs, "deadbeef"); err != nil {
				t.Fatal(err)
			}
			got, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Annotations[carotateOperationAnnotation] != tc.op {
				t.Errorf("operation annotation = %q, want %q: without it a put cannot tell whose key is staged", got.Annotations[carotateOperationAnnotation], tc.op)
			}
			if got.Annotations[carotateRunAnnotation] != tc.ownRun {
				t.Errorf("run annotation = %q, want %q: the reaper and the cleanups tell a leftover from a live key by it", got.Annotations[carotateRunAnnotation], tc.ownRun)
			}
		})
		t.Run(tc.name+" refuses a key the other operation staged", func(t *testing.T) {
			foreign := map[string]func(context.Context, kubernetes.Interface, string) error{
				carotateOpRotateCA:      stageRotation,
				carotateOpMigrateEtcdCA: stageMigration,
			}
			foreignOp := carotateOpMigrateEtcdCA
			if tc.op == carotateOpMigrateEtcdCA {
				foreignOp = carotateOpRotateCA
			}
			cs := fake.NewSimpleClientset()
			if err := foreign[foreignOp](context.Background(), cs, "their-key"); err != nil {
				t.Fatal(err)
			}
			deletes := preconditionedDeletes(t, cs)
			err := tc.stage(context.Background(), cs, "our-key")
			if err == nil {
				t.Fatalf("the put overwrote a key the %s operation staged: one operation's put must refuse another operation's key", foreignOp)
			}
			if !strings.Contains(err.Error(), "refusing") {
				t.Errorf("error = %v, want it to say the put refused", err)
			}
			got, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Data["certificate-key"]) != "their-key" {
				t.Errorf("staged key = %q, want the %s operation's key left untouched", got.Data["certificate-key"], foreignOp)
			}
			if len(*deletes) != 0 {
				t.Errorf("the refusal issued %d delete(s): a foreign key is left for its own run", len(*deletes))
			}
		})
		t.Run(tc.name+" replaces a leftover run of its own operation", func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			older := map[string]func(context.Context, kubernetes.Interface, string) error{
				carotateOpRotateCA: func(ctx context.Context, cs kubernetes.Interface, keyHex string) error {
					return stageCarotateKey(io.Discard, ctx, cs, keyHex, "earlier-run")
				},
				carotateOpMigrateEtcdCA: stageMigration,
			}
			if err := older[tc.op](context.Background(), cs, "older-run-key"); err != nil {
				t.Fatal(err)
			}
			// The maintenance lease excludes a second live run of the
			// operation, so a key of another run of the SAME operation is a
			// dead run's leftover — the resume re-stages over it.
			if err := tc.stage(context.Background(), cs, "deadbeef"); err != nil {
				t.Fatalf("the re-stage over this operation's own leftover run: %v", err)
			}
			got, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Data["certificate-key"]) != "deadbeef" || got.Annotations[carotateOperationAnnotation] != tc.op || got.Annotations[carotateRunAnnotation] != tc.ownRun {
				t.Errorf("staged key = %q annotations = %v, want this run's key under its own operation and run", got.Data["certificate-key"], got.Annotations)
			}
		})
	}
	t.Run("the rotation's cleanup leaves a migration's key", func(t *testing.T) {
		cs := fake.NewSimpleClientset(migrationKeyAt("k", "1", "their-key"))
		deletes := preconditionedDeletes(t, cs)
		stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
		cleanupOwnCarotateKey(context.Background(), io.Discard, cs, run1, "")
		if !hasSecret(t, cs, caRotateKeySecret) {
			t.Fatal("the rotation's cleanup deleted a key the migration staged")
		}
		if len(*deletes) != 0 {
			t.Errorf("sent %d delete(s) for a key the rotation did not stage", len(*deletes))
		}
	})
	t.Run("the migration's cleanup leaves a rotation's key", func(t *testing.T) {
		cs := fake.NewSimpleClientset(keyAt("k", "1", run1))
		deletes := preconditionedDeletes(t, cs)
		stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
		cleanupOwnCarotateKey(context.Background(), io.Discard, cs, "", "their-key")
		if !hasSecret(t, cs, caRotateKeySecret) {
			t.Fatal("the migration's cleanup deleted a key the rotation staged")
		}
		if len(*deletes) != 0 {
			t.Errorf("sent %d delete(s) for a key the migration did not stage", len(*deletes))
		}
	})
	t.Run("every delete carries the snapshot it read", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			key     *corev1.Secret
			cleanup func(context.Context, kubernetes.Interface)
		}{
			{"rotate-ca's own key", keyAt("k", "1", run1), func(ctx context.Context, cs kubernetes.Interface) {
				cleanupOwnCarotateKey(ctx, io.Discard, cs, run1, "")
			}},
			{"migrate-etcd-ca's own key", migrationKeyAt("k", "2", "k-mig"), func(ctx context.Context, cs kubernetes.Interface) {
				cleanupOwnCarotateKey(ctx, io.Discard, cs, "", "k-mig")
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cs := fake.NewSimpleClientset(tc.key)
				deletes := preconditionedDeletes(t, cs)
				stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
				tc.cleanup(context.Background(), cs)
				if hasSecret(t, cs, caRotateKeySecret) {
					t.Fatal("the operation's own key survived its cleanup")
				}
				if len(*deletes) == 0 {
					t.Fatal("no delete was issued")
				}
				for i, o := range *deletes {
					if o.Preconditions == nil || *o.Preconditions.UID != tc.key.UID || *o.Preconditions.ResourceVersion != tc.key.ResourceVersion {
						t.Errorf("delete %d preconditions = %+v, want the snapshot's UID and resourceVersion", i, o.Preconditions)
					}
				}
			})
		}
	})
	t.Run("the migration's reap leaves a key a rotation staged", func(t *testing.T) {
		cs := fake.NewSimpleClientset(keyAt("k", "1", run1))
		deletes := preconditionedDeletes(t, cs)
		d := &clusterDriver{ctx: context.Background(), cs: cs}
		out := runCapture(t, func(w io.Writer) { d.w = w; d.ReapStaleFetchKey() })
		if !hasSecret(t, cs, caRotateKeySecret) {
			t.Fatal("the migration's start-of-run reap deleted a key a rotate-ca run staged: its followers may still be fetching with it")
		}
		if len(*deletes) != 0 {
			t.Errorf("sent %d delete(s) for a rotation's key", len(*deletes))
		}
		for _, want := range []string{"rotate-ca", "Leaving it"} {
			if !strings.Contains(out, want) {
				t.Errorf("output = %q, want it to say the rotation's key is left", out)
			}
		}
	})
	t.Run("the migration's reap still reaps its own operation's key", func(t *testing.T) {
		cs := fake.NewSimpleClientset(migrationKeyAt("k", "1", "k-mig"))
		deletes := preconditionedDeletes(t, cs)
		d := &clusterDriver{ctx: context.Background(), cs: cs}
		runCapture(t, func(w io.Writer) { d.w = w; d.ReapStaleFetchKey() })
		if hasSecret(t, cs, caRotateKeySecret) {
			t.Error("a dead migration's own staged key survived the reap")
		}
		if len(*deletes) == 0 {
			t.Error("no delete was issued for the migration's own leftover key")
		}
	})
	t.Run("a put that loses the read-create race refuses a foreign key", func(t *testing.T) {
		// The race: this put's read says the key Secret is absent, but a
		// stager of the OTHER operation creates it before our Create lands, so
		// the Create comes back AlreadyExists. The vetting the owner type
		// exists for must then see the key that appeared: a foreign one is
		// refused, never overwritten, and the put says so.
		cs := fake.NewSimpleClientset(migrationKeyAt("k", "1", "their-key"))
		gots, creates := 0, 0
		cs.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			gots++
			if gots == 1 { // the read that lost the race: the Secret "was" absent
				return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, caRotateKeySecret)
			}
			return false, nil, nil // later reads see the foreign key that appeared
		})
		cs.PrependReactor("create", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
			creates++
			if creates == 1 {
				return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, caRotateKeySecret)
			}
			return false, nil, nil
		})
		err := putStagedKey(context.Background(), cs, "our-key", rotationStagedKeyOwner(run1))
		if err == nil {
			t.Fatal("the put reported success over a foreign key it never vetted")
		}
		if !strings.Contains(err.Error(), "refusing") || !strings.Contains(err.Error(), carotateOpMigrateEtcdCA) {
			t.Errorf("error = %v, want it to refuse and name the foreign operation", err)
		}
		got, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Data["certificate-key"]) != "their-key" || got.Annotations[carotateOperationAnnotation] != carotateOpMigrateEtcdCA {
			t.Errorf("staged key = %q annotations = %v, want the migration's key left untouched", got.Data["certificate-key"], got.Annotations)
		}
	})
}

// TestEachOperationStoresAndRemovesItsOwnKey pins the staged-key lifecycle of
// the two operations that share the stratum-carotate Secret: rotate-ca
// --cluster stores its key under its run id (stageCarotateKey), and
// migrate-etcd-ca stores the same Secret under the owner type's migration
// owner (putStagedKey, as PublishEtcdCA calls it), whose run annotation is the
// unique certificate key that names the migration's run. The operation and run
// annotations are what the finalize/rollback reaper and each operation's
// cleanup rely on to tell one operation's leftover from the other's live key,
// so every staged key carries both. Each row then runs the operation's own
// cleanup — the cleanupOwnCarotateKey the deferred cleanups of RotateCACluster
// (cacluster.go) and PublishEtcdCA (etcdcacluster.go) register — which must
// remove the Secret: a success path that strands it leaves the AES key that
// decrypts the cluster CA material readable in the cluster.
func TestEachOperationStoresAndRemovesItsOwnKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stage   func(ctx context.Context, cs kubernetes.Interface, keyHex string) error
		op      string
		run     string // rotate-ca's run id; migrate-etcd-ca's run is a digest of its unique certificate key
		cleanup func(ctx context.Context, cs kubernetes.Interface)
	}{
		{"rotate-ca --cluster", func(ctx context.Context, cs kubernetes.Interface, keyHex string) error {
			return stageCarotateKey(io.Discard, ctx, cs, keyHex, run1)
		}, carotateOpRotateCA, run1, func(ctx context.Context, cs kubernetes.Interface) {
			cleanupOwnCarotateKey(ctx, io.Discard, cs, run1, "")
		}},
		{"migrate-etcd-ca", func(ctx context.Context, cs kubernetes.Interface, keyHex string) error {
			return putStagedKey(ctx, cs, keyHex, migrationStagedKeyOwner(keyHex))
		}, carotateOpMigrateEtcdCA, migrationStagedKeyOwner("deadbeef").run, func(ctx context.Context, cs kubernetes.Interface) {
			cleanupOwnCarotateKey(ctx, io.Discard, cs, "", "deadbeef")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			stubGate(t, func(context.Context, kubernetes.Interface) error { return nil })
			if err := tc.stage(context.Background(), cs, "deadbeef"); err != nil {
				t.Fatal(err)
			}
			got, err := cs.CoreV1().Secrets(caNamespace).Get(context.Background(), caRotateKeySecret, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("key Secret missing after staging: %v", err)
			}
			if string(got.Data["certificate-key"]) != "deadbeef" {
				t.Errorf("certificate-key = %q, want the staged key", got.Data["certificate-key"])
			}
			if got.Annotations[carotateOperationAnnotation] != tc.op {
				t.Errorf("operation annotation = %q, want %q", got.Annotations[carotateOperationAnnotation], tc.op)
			}
			if got.Annotations[carotateRunAnnotation] != tc.run {
				t.Errorf("run annotation = %q, want %q", got.Annotations[carotateRunAnnotation], tc.run)
			}
			// The operation's own cleanup, on the detached context both
			// operations use for it.
			ctx, cancel := cleanupContext()
			defer cancel()
			tc.cleanup(ctx, cs)
			if hasSecret(t, cs, caRotateKeySecret) {
				t.Error("the key Secret survived its own operation's cleanup")
			}
		})
	}
}

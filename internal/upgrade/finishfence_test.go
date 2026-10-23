package upgrade

import (
	"io"

	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodejob"
)

// A run that took the lease over owns the node's cordon and records, and a run
// that lost it must not finish the node, however long its retries go on. The
// first run's write keeps failing after its lease check (an API outage), the
// successor takes the lease and claims the node, and the first run's next
// attempt must leave all of it alone.
func TestFinishRetryNeverFinishesASuccessorsNode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
		holder, secs, now := "me", int32(60), metav1.NewMicroTime(time.Now())
		if _, err := cs.CoordinationV1().Leases(cuNamespace).Create(t.Context(), &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: upgradeLeaseName, Namespace: cuNamespace},
			Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &secs, RenewTime: &now},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		g := &leaseGuard{lastRenew: time.Now(), holder: "me"}
		// The outage starts at the finish: the first lease check after the Job.
		confirmed, confirms, jobDone := time.Time{}, 0, false
		g.renew = func(ctx context.Context) error {
			err := renewUpgradeLease(ctx, cs, "me")
			if jobDone {
				confirms++
				if err == nil && confirmed.IsZero() {
					confirmed = time.Now()
				}
			}
			return err
		}
		oldRun := runNodeJob
		t.Cleanup(func() { runNodeJob = oldRun })
		runNodeJob = func(context.Context, kubernetes.Interface, nodejob.Spec) error { jobDone = true; return nil }

		taken := false
		cs.PrependReactor("update", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
			if confirmed.IsZero() {
				return false, nil, nil
			}
			if time.Since(confirmed) <= leaseDuration+leaseRenew {
				return true, nil, errAPIDown // the finish write cannot reach the API server
			}
			if !taken {
				taken = true
				leaseGVR := coordinationv1.SchemeGroupVersion.WithResource("leases")
				obj, err := cs.Tracker().Get(leaseGVR, cuNamespace, upgradeLeaseName)
				if err != nil {
					t.Fatal(err)
				}
				l := obj.(*coordinationv1.Lease)
				other, renewed := "successor", metav1.NewMicroTime(time.Now())
				l.Spec.HolderIdentity, l.Spec.RenewTime = &other, &renewed
				if err := cs.Tracker().Update(leaseGVR, l, cuNamespace); err != nil {
					t.Fatal(err)
				}
				changeNode(t, cs, func(n *corev1.Node) {
					n.Spec.Unschedulable = true
					n.Annotations[k8sutil.UpgradeCordonAnnotation] = cordonClaim("v1.37.1", "successor")
					n.Annotations[k8sutil.UpgradeInProgressAnnotation] = "v1.37.1"
					n.Annotations["successor"] = "active"
				})
				// Even a background loop that has seen the takeover cannot be what stops it.
				g.observe(errLeaseTaken, time.Now())
			}
			return false, nil, nil
		})
		n := Node{Name: "cp1", Version: "v1.36.5", Ready: true, IsCP: true}
		o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-next", DrainGraceSec: 1, lease: g}
		_, err := upgradeOneNode(io.Discard, t.Context(), cs, n, o)
		if !errors.Is(err, errLeaseTaken) {
			t.Fatalf("err = %v, want the takeover (run A must not report success)", err)
		}
		got := getNode(t, cs)
		if !taken || !got.Spec.Unschedulable || got.Annotations["successor"] != "active" ||
			got.Annotations[k8sutil.UpgradeInProgressAnnotation] != "v1.37.1" ||
			got.Annotations[k8sutil.UpgradeCordonAnnotation] != cordonClaim("v1.37.1", "successor") ||
			got.Annotations[k8sutil.VersionAnnotation] != "" {
			t.Fatalf("the successor's state did not survive: taken=%v unschedulable=%v annotations=%v", taken, got.Spec.Unschedulable, got.Annotations)
		}
		if confirms < 2 {
			t.Errorf("the lease was confirmed %d time(s); it must be on every finish attempt", confirms)
		}
	})
}

// The node claim is its own fence: with a lease check that passes (the
// successor took over after it), a write for a node claimed by another run is
// refused and changes nothing.
func TestFinishRefusesANodeClaimedByAnotherRun(t *testing.T) {
	const target = "v1.37.1"
	for _, c := range []struct {
		name       string
		claim      string // "" = no claim
		inProgress string
		wantErr    bool
	}{
		{"this run's claim", cordonClaim(target, "me"), target, false},
		{"another run's claim for the same target", cordonClaim(target, "successor"), target, true},
		{"another run's admin-cordon claim", cordonClaim(cordonPreexisting, "successor"), target, true},
		{"another run's released claim", cordonClaim(cordonReleased, "successor"), target, true},
		{"an older claim with no holder", target, target, false},
		{"no claim: an older orchestrator's cordon", "", target, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ann := map[string]string{k8sutil.UpgradeInProgressAnnotation: c.inProgress}
			if c.claim != "" {
				ann[k8sutil.UpgradeCordonAnnotation] = c.claim
			}
			cs := fakeNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1", ResourceVersion: "1", Annotations: ann}, Spec: corev1.NodeSpec{Unschedulable: true}})
			err := finishNode(context.Background(), cs, "cp1", target, "", "me")
			got := getNode(t, cs)
			if c.wantErr {
				if !errors.Is(err, errClaimTaken) || !terminalAPIError(err) || !got.Spec.Unschedulable ||
					got.Annotations[k8sutil.UpgradeCordonAnnotation] != c.claim || got.Annotations[k8sutil.UpgradeInProgressAnnotation] != target ||
					got.Annotations[k8sutil.VersionAnnotation] != "" {
					t.Errorf("err=%v node=%v %v, want it refused and untouched", err, got.Spec.Unschedulable, got.Annotations)
				}
				return
			}
			if err != nil || got.Annotations[k8sutil.VersionAnnotation] != target || got.Annotations[k8sutil.UpgradeCordonAnnotation] != "" {
				t.Errorf("err=%v annotations=%v, want the node finished", err, got.Annotations)
			}
		})
	}
}

// A run that takes the lease over takes over a halted run's claim, and the
// claim names it from then on; an admin's cordon stays an admin's.
func TestCordonForUpgradeTakesOverAHaltedRunsClaim(t *testing.T) {
	const target = "v1.37.1"
	for _, c := range []struct {
		name          string
		unschedulable bool
		claim         string
		inProgress    string
		wantClaim     string
		wantCordoned  bool
	}{
		{"schedulable node", false, "", "", cordonClaim(target, "me"), true},
		{"halted run's cordon", true, cordonClaim(target, "dead"), target, cordonClaim(target, "me"), true},
		{"older orchestrator's cordon", true, "", target, cordonClaim(target, "me"), true},
		{"halted run's cordon for an earlier target", true, cordonClaim("v1.36.9", "dead"), "", cordonClaim(target, "me"), true},
		{"admin's cordon", true, "", "", cordonClaim(cordonPreexisting, "me"), true},
		{"admin's cordon found by a halted run", true, cordonClaim(cordonPreexisting, "dead"), target, cordonClaim(cordonPreexisting, "me"), true},
		{"admin's cordon after the Job released its own", true, cordonClaim(cordonReleased, "dead"), target, cordonClaim(cordonPreexisting, "me"), true},
		{"this run's own claim: no write", true, cordonClaim(target, "me"), target, cordonClaim(target, "me"), true},
		{"schedulable, a stale claim of this run", false, cordonClaim(target, "me"), target, cordonClaim(target, "me"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ann := map[string]string{}
			if c.claim != "" {
				ann[k8sutil.UpgradeCordonAnnotation] = c.claim
			}
			if c.inProgress != "" {
				ann[k8sutil.UpgradeInProgressAnnotation] = c.inProgress
			}
			cs := fakeNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1", ResourceVersion: "1", Annotations: ann}, Spec: corev1.NodeSpec{Unschedulable: c.unschedulable}})
			if err := cordonForUpgrade(context.Background(), cs, "cp1", target, &leaseGuard{holder: "me"}); err != nil {
				t.Fatal(err)
			}
			got := getNode(t, cs)
			if got.Spec.Unschedulable != c.wantCordoned || got.Annotations[k8sutil.UpgradeCordonAnnotation] != c.wantClaim {
				t.Errorf("cordoned=%v claim=%q, want %v %q", got.Spec.Unschedulable, got.Annotations[k8sutil.UpgradeCordonAnnotation], c.wantCordoned, c.wantClaim)
			}
		})
	}
}

// Once the renewal window has passed the guard is stale, and a renewal that
// then fails for good (Forbidden, NotFound) is still returned as it came, so
// the retry loop stops at once instead of running out its window.
func TestConfirmReturnsATerminalRenewalErrorWhenStale(t *testing.T) {
	res := schema.GroupResource{Resource: "leases"}
	for name, renewErr := range map[string]error{
		"forbidden": apierrors.NewForbidden(res, upgradeLeaseName, errors.New("denied")),
		"not found": apierrors.NewNotFound(res, upgradeLeaseName),
	} {
		t.Run(name, func(t *testing.T) {
			g := &leaseGuard{lastRenew: time.Now().Add(-leaseDuration), renew: func(context.Context) error { return renewErr }}
			calls := 0
			err := retryAPI(context.Background(), func(ctx context.Context) error { calls++; return g.confirm(ctx) })
			if calls != 1 || !errors.Is(err, renewErr) || !terminalAPIError(err) {
				t.Errorf("calls=%d err=%v, want one call and the renewal error as it came", calls, err)
			}
		})
	}
}

// The cordon step is fenced like the finish: while it retries through an API
// outage, a run that loses the lease must not overwrite the claim of the run
// that took it over, whether that claim is for the upgrade's cordon or for an
// admin's. The successor's own finish then still succeeds.
func TestCordonRetryNeverOverwritesASuccessorsClaim(t *testing.T) {
	for _, kind := range []string{"v1.37.1", cordonPreexisting} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cs, _ := singleNode(t, 0, 0, batchv1.JobStatus{})
				holder, secs, now := "me", int32(60), metav1.NewMicroTime(time.Now())
				if _, err := cs.CoordinationV1().Leases(cuNamespace).Create(t.Context(), &coordinationv1.Lease{
					ObjectMeta: metav1.ObjectMeta{Name: upgradeLeaseName, Namespace: cuNamespace},
					Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &secs, RenewTime: &now},
				}, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				g := &leaseGuard{lastRenew: time.Now(), holder: "me"}
				g.renew = func(ctx context.Context) error { return renewUpgradeLease(ctx, cs, "me") }
				start, taken := time.Now(), false
				cs.PrependReactor("update", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
					if time.Since(start) <= leaseDuration+leaseRenew {
						g.observe(errAPIDown, time.Now())
						return true, nil, errAPIDown
					}
					if !taken {
						taken = true
						leaseGVR := coordinationv1.SchemeGroupVersion.WithResource("leases")
						obj, err := cs.Tracker().Get(leaseGVR, cuNamespace, upgradeLeaseName)
						if err != nil {
							t.Fatal(err)
						}
						l := obj.(*coordinationv1.Lease)
						other, renewed := "successor", metav1.NewMicroTime(time.Now())
						l.Spec.HolderIdentity, l.Spec.RenewTime = &other, &renewed
						if err := cs.Tracker().Update(leaseGVR, l, cuNamespace); err != nil {
							t.Fatal(err)
						}
						changeNode(t, cs, func(n *corev1.Node) {
							n.Spec.Unschedulable = true
							n.Annotations = map[string]string{
								k8sutil.UpgradeCordonAnnotation:     cordonClaim(kind, "successor"),
								k8sutil.UpgradeInProgressAnnotation: "v1.37.1",
							}
						})
						g.observe(errLeaseTaken, time.Now()) // the loop has seen it, too
					}
					return false, nil, nil
				})
				n := Node{Name: "cp1", Version: "v1.36.5", Ready: true, IsCP: true}
				o := ClusterOptions{Target: "v1.37.1", From: "/var/lib/stratum/upgrade/stratum-next", DrainGraceSec: 1, lease: g}
				_, err := upgradeOneNode(io.Discard, t.Context(), cs, n, o)
				got := getNode(t, cs)
				if !taken || !errors.Is(err, errLeaseTaken) || got.Annotations[k8sutil.UpgradeCordonAnnotation] != cordonClaim(kind, "successor") {
					t.Fatalf("taken=%v err=%v annotations=%v, want the run stopped with the successor's claim intact", taken, err, got.Annotations)
				}
				if err := finishNode(t.Context(), cs, "cp1", "v1.37.1", "", "successor"); err != nil {
					t.Fatalf("the successor's own finish: %v", err)
				}
			})
		})
	}
}

// Recording the upgrade in progress is the same: refused on a node another run
// claimed.
func TestMarkUpgradeInProgressRefusesANodeClaimedByAnotherRun(t *testing.T) {
	for name, c := range map[string]struct {
		claim   string
		wantErr bool
	}{
		"this run's claim":    {cordonClaim("v1.37.1", "me"), false},
		"another run's claim": {cordonClaim("v1.37.1", "successor"), true},
		"no holder":           {"v1.37.1", false},
		"no claim":            {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			ann := map[string]string{}
			if c.claim != "" {
				ann[k8sutil.UpgradeCordonAnnotation] = c.claim
			}
			cs := fakeNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1", ResourceVersion: "1", Annotations: ann}})
			err := markUpgradeInProgress(context.Background(), cs, "cp1", "v1.37.1", "me")
			got := getNode(t, cs).Annotations[k8sutil.UpgradeInProgressAnnotation]
			if c.wantErr != errors.Is(err, errClaimTaken) || (got != "") == c.wantErr {
				t.Errorf("err=%v in-progress=%q", err, got)
			}
		})
	}
}

// A run that halted after cordoning, before it recorded the upgrade in
// progress, leaves a claim for the target it was taking the node to. The next
// run may take the node to another target; as the lease holder it takes that
// claim over, and so undoes the cordon at the end. Left as `preexisting`, the
// upgrade's own cordon would stay forever. The Job's backstop,
// which holds no lease and acts for its own target, leaves such a claim alone.
func TestEarlierTargetClaimIsTheTakingOverRunsToUndo(t *testing.T) {
	const earlier, target = "v1.36.9", "v1.37.1"
	cs := fakeNode(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cp1", ResourceVersion: "1", Annotations: map[string]string{k8sutil.UpgradeCordonAnnotation: cordonClaim(earlier, "dead")}},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	})
	if done, err := releaseUpgradeCordon(context.Background(), cs, "cp1", target); err != nil || done || !getNode(t, cs).Spec.Unschedulable {
		t.Fatalf("the backstop for %s released a claim for %s: done=%v err=%v", target, earlier, done, err)
	}
	if err := cordonForUpgrade(context.Background(), cs, "cp1", target, &leaseGuard{holder: "me"}); err != nil {
		t.Fatal(err)
	}
	if got := getNode(t, cs).Annotations[k8sutil.UpgradeCordonAnnotation]; got != cordonClaim(target, "me") {
		t.Fatalf("claim = %q, want it taken over as %q, not made preexisting", got, cordonClaim(target, "me"))
	}
	if err := finishNode(context.Background(), cs, "cp1", target, "", "me"); err != nil {
		t.Fatal(err)
	}
	if got := getNode(t, cs); got.Spec.Unschedulable || got.Annotations[k8sutil.VersionAnnotation] != target {
		t.Errorf("the upgrade's own cordon was kept: unschedulable=%v annotations=%v", got.Spec.Unschedulable, got.Annotations)
	}
}

// Only a claim for a valid version is the upgrade's own to take over and undo.
// Anything else the code did not write (other case, stray whitespace, a kind a
// newer build adds, an empty kind) fails closed: it is left cordoned and
// claimed as a cordon found, never uncordoned.
func TestUnknownCordonClaimsFailClosed(t *testing.T) {
	const target = "v1.37.1"
	for _, kind := range []string{
		"Preexisting", "preexisting ", " preexisting", "RELEASED", "released\n",
		" v1.36.9", "v1.36.9 ", "V1.36.9", "v1.36", "1.36.9", "v1.36.9-rc.1", "v01.36.9", "newer-kind", "",
		// Shaped like a version, but a component overflows an int: never a real
		// target (k8sver.ValidTarget accepts these, ParseTarget does not).
		"v99999999999999999999.1.1", "v1.99999999999999999999.1", "v1.36.99999999999999999999",
		"v9223372036854775808.0.0",
	} {
		t.Run(kind, func(t *testing.T) {
			// With an in-progress record naming the target: a claim that is present
			// but not a version must not fall back to the older-orchestrator rule.
			ann := map[string]string{k8sutil.UpgradeCordonAnnotation: kind + "@dead", k8sutil.UpgradeInProgressAnnotation: target}
			cs := fakeNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1", ResourceVersion: "1", Annotations: ann}, Spec: corev1.NodeSpec{Unschedulable: true}})
			if err := cordonForUpgrade(context.Background(), cs, "cp1", target, &leaseGuard{holder: "me"}); err != nil {
				t.Fatal(err)
			}
			if got := getNode(t, cs).Annotations[k8sutil.UpgradeCordonAnnotation]; got != cordonClaim(cordonPreexisting, "me") {
				t.Errorf("claim = %q, want the cordon found and left as %q", got, cordonClaim(cordonPreexisting, "me"))
			}
			if err := finishNode(context.Background(), cs, "cp1", target, "", "me"); err != nil {
				t.Fatal(err)
			}
			if !getNode(t, cs).Spec.Unschedulable {
				t.Error("the node was uncordoned on a claim the upgrade did not write")
			}
			// And the Job's backstop, which is stricter still.
			cs = fakeNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1", ResourceVersion: "1", Annotations: ann}, Spec: corev1.NodeSpec{Unschedulable: true}})
			if done, err := releaseUpgradeCordon(context.Background(), cs, "cp1", target); err != nil || done || !getNode(t, cs).Spec.Unschedulable {
				t.Errorf("backstop: done=%v err=%v", done, err)
			}
		})
	}
}

// The Job's backstop holds the same line for its own target: a claim equal to a
// target that is not a real version (an overflowing component) is not released.
func TestBackstopRequiresARealTarget(t *testing.T) {
	for _, target := range []string{"v99999999999999999999.1.1", "v1.36.99999999999999999999", "newer-kind", ""} {
		t.Run(target, func(t *testing.T) {
			ann := map[string]string{k8sutil.UpgradeCordonAnnotation: target + "@dead"}
			cs := fakeNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1", ResourceVersion: "1", Annotations: ann}, Spec: corev1.NodeSpec{Unschedulable: true}})
			if done, err := releaseUpgradeCordon(context.Background(), cs, "cp1", target); err != nil || done || !getNode(t, cs).Spec.Unschedulable {
				t.Errorf("done=%v err=%v: released a claim for %q", done, err, target)
			}
		})
	}
}

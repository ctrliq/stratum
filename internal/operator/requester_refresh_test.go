package operator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestReconcileRefreshesLaggingRequesterVerifier(t *testing.T) {
	for _, version := range []string{"", stampBindingV2} {
		for _, operation := range []string{"install", "overlay", "delete", "tampered", "refresh unavailable"} {
			t.Run(version+"/"+operation, func(t *testing.T) {
				ctx := context.Background()
				h := useFakeHelm(t, fakeHelmOptions{})
				cs := fake.NewSimpleClientset()
				old := keyringFromSecret(t, cs, "k0", map[string][]byte{"k0": testRotationKey(0)})
				fast, slow := newRequesterSignerHolder(), newRequesterSignerHolder()
				fast.publish(old)
				slow.publish(old)
				writeRequesterSecret(t, cs, "k1", map[string][]byte{"k0": testRotationKey(0), "k1": testRotationKey(1)})
				sec, err := cs.CoreV1().Secrets(webhookNamespace).Get(ctx, requesterKeySecret, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				sec.Data[requesterBindingVersionField] = []byte(version)
				if _, err := cs.CoreV1().Secrets(webhookNamespace).Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				publishRefreshedSigner(ctx, cs, fast, sec)
				hc := newFlowHCIn(t, tenantNS, "app", "app-uid", map[string]interface{}{
					"chartContent": flowChart(t, "1.0.0"),
				})
				hc = admitObject(t, hc, nil, "alice", fast.current())
				var overlays []*unstructured.Unstructured
				if operation == "overlay" {
					// The chart itself verifies under the captured snapshot, but
					// a later overlay was stamped by the already-refreshed webhook.
					hc = admitObject(t, hc, nil, "alice", old)
					hcc := newFlowHCC(tenantNS, "app", map[string]interface{}{"valuesContent": "replicas: 2"})
					overlays = append(overlays, admitObject(t, hcc, nil, "alice", fast.current()))
				}
				if operation == "tampered" {
					ann := hc.GetAnnotations()
					ann[annRequester] = "mallory"
					hc.SetAnnotations(ann)
				}
				if operation == "delete" {
					now := metav1.Now()
					hc.SetDeletionTimestamp(&now)
					hc.Object["status"] = map[string]interface{}{"releaseNamespace": tenantNS, "releaseName": "app"}
					h.seed(t, tenantNS, "app", helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
				}
				dyn := newFlowDynClient(t, hc, overlays...)
				if operation == "refresh unavailable" {
					cs.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewInternalError(errors.New("apiserver unavailable"))
					})
				}
				refreshes := 0
				pol := helmPolicy{signer: slow.current(), refreshSigner: func(ctx context.Context) *requesterSigner {
					refreshes++
					refreshRequesterSigner(ctx, cs, slow)
					return slow.current()
				}}
				reconcileAllCharts(t, dyn, pol)
				if refreshes != 1 {
					t.Fatalf("refreshes = %d, want one before refusing the lagging stamp", refreshes)
				}
				if _, known := old.keyByID("k1"); known {
					t.Fatal("refresh mutated a previously captured signer snapshot")
				}
				if operation == "delete" {
					if hasFinalizer(getHCIn(t, dyn, tenantNS, "app"), helmFinalizer) {
						t.Fatal("valid rotated stamp prevented cleanup")
					}
					if hist := h.history(t, tenantNS, "app"); hist != nil {
						t.Fatalf("cleanup retained release: %+v", hist)
					}
					return
				}
				status := readStatusIn(t, dyn, tenantNS, "app")
				if operation == "tampered" || operation == "refresh unavailable" {
					if status.phase != hcPhaseFailed || !strings.Contains(status.message, "signature") {
						t.Fatalf("invalid/unverifiable stamp did not fail closed: %+v", status)
					}
					if hist := h.history(t, tenantNS, "app"); hist != nil {
						t.Fatal("unverified requester installed a release")
					}
				} else if status.phase != hcPhaseDeployed {
					t.Fatalf("valid admission was refused by lagging verifier: %+v", status)
				}
			})
		}
	}
}

func TestRequesterRefreshCancellationWhileAnotherRefreshIsBlocked(t *testing.T) {
	cs := fake.NewSimpleClientset()
	h := newRequesterSignerHolder()
	h.publish(keyringFromSecret(t, cs, "k0", map[string][]byte{"k0": testRotationKey(0)}))
	started, release, firstDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cs.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		close(started)
		<-release
		return false, nil, nil
	})
	go func() {
		defer close(firstDone)
		refreshRequesterSigner(context.Background(), cs, h)
	}()
	t.Cleanup(func() {
		close(release)
		<-firstDone
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first refresh did not reach the Secret read")
	}
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		refreshRequesterSigner(ctx, cs, h)
	}()
	cancel()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled refresh waited for another owner's blocked Secret read")
	}
}

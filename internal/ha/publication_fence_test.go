package ha

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/maintlease"
)

func TestUploadRefusesLeaseLostDuringPublicationReads(t *testing.T) {
	// Generate PKI once; each scenario replaces only the Secret and API reactors.
	cs := uploadFixture(t)
	secrets := cs.CoreV1().Secrets("kube-system")
	gvr := corev1.SchemeGroupVersion.WithResource("secrets")
	for _, scenario := range []string{"create", "update", "grant pruning", "conflict retry"} {
		t.Run(scenario, func(t *testing.T) {
			beforeReactors := append([]k8stesting.Reactor(nil), cs.ReactionChain...)
			defer func() { cs.ReactionChain = beforeReactors }()
			if err := cs.Tracker().Delete(gvr, "kube-system", certsSecret); err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			}
			var successor *corev1.Secret
			if scenario != "create" {
				successor = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: certsSecret, Namespace: "kube-system", ResourceVersion: "successor-version"}, Data: map[string][]byte{"certs": []byte("successor ciphertext"), grantEntryName("abc123"): []byte("pending grant")}}
				if err := cs.Tracker().Create(gvr, successor.DeepCopy(), "kube-system"); err != nil {
					t.Fatal(err)
				}
			}
			lost := false
			reads, updates, creates := 0, 0, 0
			cs.PrependReactor("*", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
				switch action.GetVerb() {
				case "get":
					a := action.(k8stesting.GetAction)
					if a.GetName() == certsSecret {
						reads++
						// First GET chooses create/update; the second reads the object to
						// update. A conflict forces a third GET before another write.
						if scenario == "create" || scenario == "update" && reads == 2 || scenario == "conflict retry" && reads == 3 {
							lost = true
						}
					} else if scenario == "grant pruning" && a.GetName() == "bootstrap-token-abc123" {
						lost = true
					}
				case "create":
					a := action.(k8stesting.CreateAction)
					if a.GetObject().(metav1.Object).GetName() == certsSecret {
						creates++
					}
				case "update":
					a := action.(k8stesting.UpdateAction)
					if a.GetObject().(metav1.Object).GetName() == certsSecret {
						updates++
						if scenario == "conflict retry" && updates == 1 {
							return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, certsSecret, errors.New("successor changed trust"))
						}
					}
				}
				return false, nil, nil
			})
			confirm := func(context.Context) error {
				if lost {
					return maintlease.ErrTaken
				}
				return nil
			}
			err := UploadCerts(mustKey(t), tokenOwner("bootstrap-token-def456", "new-grant"), confirm)
			wantUpdates := 0
			if scenario == "conflict retry" {
				wantUpdates = 1
			}
			if !errors.Is(err, maintlease.ErrTaken) || creates != 0 || updates != wantUpdates {
				t.Fatalf("err=%v reads=%d creates=%d updates=%d; lost holder must refuse publication", err, reads, creates, updates)
			}
			after, getErr := secrets.Get(t.Context(), certsSecret, metav1.GetOptions{})
			if successor == nil {
				if !apierrors.IsNotFound(getErr) {
					t.Fatalf("refused create persisted a Secret: %v", getErr)
				}
			} else if getErr != nil || !reflect.DeepEqual(after, successor) {
				t.Fatalf("successor Secret changed after lease loss: err=%v", getErr)
			}
		})
	}
}

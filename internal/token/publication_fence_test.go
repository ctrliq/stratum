package token

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/maintlease"
)

// A stalled read can complete after a successor takes the lease and publishes
// new trust. Returning its current resourceVersion makes the following stale
// update valid, so the lease must be checked after the read as well.
type leaseLostDuringConfigMapRead struct {
	*fakeConfigMaps
	loseAt    int
	lost      *bool
	successor *corev1.ConfigMap
}

func (f leaseLostDuringConfigMapRead) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.ConfigMap, error) {
	losing := f.gets+1 == f.loseAt
	if losing && f.successor != nil {
		f.cm = f.successor.DeepCopy()
	}
	cm, err := f.fakeConfigMaps.Get(ctx, name, opts)
	if losing {
		*f.lost = true
	}
	return cm, err
}

func TestTrustPublicationRefusesLeaseLostDuringFinalRead(t *testing.T) {
	kubeletTrustHost(t, true)
	for _, kind := range []string{"cluster-info", "kubelet-client-ca"} {
		t.Run(kind, func(t *testing.T) {
			for _, scenario := range []string{"create", "update", "conflict retry"} {
				t.Run(scenario, func(t *testing.T) {
					f := &fakeConfigMaps{}
					name, key := "cluster-info", kcKey
					if kind == "kubelet-client-ca" {
						name, key = clusterinfo.KubeletClientCAConfigMap, clusterinfo.KubeletClientCAKey
					}
					var successor *corev1.ConfigMap
					if scenario != "create" {
						successor = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-public", ResourceVersion: "successor-version"}, Data: map[string]string{key: "successor trust", sigKey: "successor signature", "unrelated": "preserve"}}
						f.cm = successor.DeepCopy()
					}
					loseAt, wantUpdates := 1, 0
					if scenario == "conflict retry" {
						f.conflicts = 1
						loseAt = 2
						wantUpdates = 1
					}
					lost := false
					cms := leaseLostDuringConfigMapRead{fakeConfigMaps: f, loseAt: loseAt, lost: &lost, successor: successor}
					confirm := func(context.Context) error {
						if lost {
							return maintlease.ErrTaken
						}
						return nil
					}
					var err error
					if kind == "cluster-info" {
						err = putClusterInfoKubeconfig(t.Context(), cms, "stale holder trust", confirm)
					} else {
						err = publishKubeletClientCA(t.Context(), cms, confirm)
					}
					if !errors.Is(err, maintlease.ErrTaken) || f.creates != 0 || f.updates != wantUpdates {
						t.Fatalf("err=%v creates=%d updates=%d; lost holder must refuse publication", err, f.creates, f.updates)
					}
					if !reflect.DeepEqual(f.cm, successor) {
						t.Fatalf("successor trust changed after lease loss: got=%+v want=%+v", f.cm, successor)
					}
				})
			}
		})
	}
}

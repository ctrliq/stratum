package k8sutil

import (
	"context"
	"errors"
	"testing"

	"github.com/ctrliq/stratum/internal/maintlease"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestMaintenanceLeaseConfirmsBothFencesAfterTakeover(t *testing.T) {
	for _, name := range []string{"stratum-cluster-maintenance", "stratum-cluster-upgrade"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cs := fake.NewSimpleClientset()
			lease, err := MaintenanceFence(cs)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			leases := cs.CoordinationV1().Leases("kube-system")
			current, err := leases.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			successor := "successor-maintenance-run"
			current.Spec.HolderIdentity = &successor
			if _, err := leases.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := lease.Confirm(ctx); !errors.Is(err, maintlease.ErrTaken) {
				t.Fatalf("confirmation accepted successor's %s lease: %v", name, err)
			}
		})
	}
}

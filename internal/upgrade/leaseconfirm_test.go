package upgrade

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// Confirming ownership must renew each lease once. A second common-lease
// renewal can reject a valid confirmation if the API becomes unavailable after
// both leases have already been renewed successfully.
func TestLeaseConfirmRenewsEachFenceOnce(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	lease, releaseLease, err := acquireUpgradeLease(ctx, cs, "this-run")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLease()
	maint, releaseMaint, err := acquireMaintenanceLease(ctx, cs, "this-run")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseMaint()
	lease.maintenance = maint
	o := ClusterOptions{lease: lease, maint: maint}
	cs.ClearActions()
	if err := o.leaseConfirm(ctx); err != nil {
		t.Fatal(err)
	}
	var renewed []string
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "leases" && a.GetVerb() == "get" {
			renewed = append(renewed, a.(interface{ GetName() string }).GetName())
		}
	}
	if want := []string{maintenanceLeaseName, upgradeLeaseName}; !reflect.DeepEqual(renewed, want) {
		t.Fatalf("ownership confirmation queried %v; want one renewal per fence, %v", renewed, want)
	}
}

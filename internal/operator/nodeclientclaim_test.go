package operator

import (
	"context"
	"sync"
	"testing"

	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReserveNodeClientNameOnlyOneCSRWins(t *testing.T) {
	cs := fake.NewSimpleClientset()
	csrs := []*certv1.CertificateSigningRequest{
		{ObjectMeta: metav1.ObjectMeta{Name: "first", UID: types.UID("first-uid")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "second", UID: types.UID("second-uid")}},
	}
	var won [2]bool
	var errs [2]error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range csrs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			won[i], errs[i] = reserveNodeClientName(context.Background(), cs, csrs[i], "worker-1")
		}(i)
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || won[0] == won[1] {
		t.Fatalf("concurrent reservations = (%v, %v), (%v, %v); exactly one CSR must own the name", won[0], errs[0], won[1], errs[1])
	}
}

package certs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/nodestate"
)

const (
	noVIPSelf  = "192.168.99.12"
	noVIPPeer  = "192.168.99.13"
	noVIPThird = "192.168.99.14"
)

// noVIPControlPlane is kubeVIPNode's control plane as `up` leaves one built
// without a VIP: every kubeconfig names the local apiserver. The local admin
// client, which the rotation takes its lease on, publishes apiservers as the
// kubernetes Service's endpoints.
func noVIPControlPlane(t *testing.T, apiservers ...string) *fake.Clientset {
	t.Helper()
	kubeVIPNode(t)
	if err := nodestate.SaveState(&nodestate.NodeState{
		Role: "controller", NodeIP: noVIPSelf, KubeletNodeIP: noVIPSelf,
	}); err != nil {
		t.Fatal(err)
	}
	if err := kubeconfig.GenerateAll("cp1", "https://127.0.0.1:6443", false); err != nil {
		t.Fatal(err)
	}
	// A release through an apiserver that no longer answers would otherwise
	// retry for its whole budget before the test can finish.
	old := maintlease.ReleaseRetryBudget
	maintlease.ReleaseRetryBudget = 0
	t.Cleanup(func() { maintlease.ReleaseRetryBudget = old })
	return fakeAdminClientset(t, apiserverEndpoints(apiservers...))
}

// apiserverEndpoints is the EndpointSlice the apiservers keep for the
// kubernetes Service, one ready endpoint per address.
func apiserverEndpoints(ips ...string) *discoveryv1.EndpointSlice {
	name, port := "https", int32(6443)
	s := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubernetes", Namespace: metav1.NamespaceDefault,
			Labels: map[string]string{discoveryv1.LabelServiceName: "kubernetes"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: &name, Port: &port}},
	}
	for _, ip := range ips {
		s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{Addresses: []string{ip}})
	}
	return s
}

// sameCluster is another apiserver of cs's cluster: it serves the same
// objects, and cs's reactors do not apply to it.
func sameCluster(cs *fake.Clientset) *fake.Clientset {
	other := &fake.Clientset{}
	other.AddReactor("*", "*", clienttesting.ObjectReaction(cs.Tracker()))
	return other
}

// errNoAnswer is what a lease call gets from an apiserver whose etcd is
// restarting: it accepts the connection and never answers.
var errNoAnswer = fmt.Errorf("Get \"https://127.0.0.1:6443/apis/coordination.k8s.io/v1/namespaces/kube-system/leases\": %w",
	context.DeadlineExceeded)

// leasesStopAnsweringAfterTake makes cs stop answering lease calls once the
// rotation has taken the maintenance lease through it, as the local apiserver
// does once its control plane starts restarting.
func leasesStopAnsweringAfterTake(cs *fake.Clientset) {
	var down atomic.Bool
	cs.PrependReactor("*", "leases", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if down.Load() {
			return true, nil, errNoAnswer
		}
		if ca, ok := a.(clienttesting.CreateAction); ok {
			if l, ok := ca.GetObject().(*coordinationv1.Lease); ok && l.Name == fenceTestMaintenanceLease {
				down.Store(true)
			}
		}
		return false, nil, nil
	})
}

// silentAPIServer is an apiserver of cs's cluster that answers no lease call.
func silentAPIServer(cs *fake.Clientset) *fake.Clientset {
	s := sameCluster(cs)
	s.PrependReactor("*", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errNoAnswer
	})
	return s
}

// usePeers points the peer client seam at clients, keyed by apiserver URL, and
// returns the URLs the run asked for, in order.
func usePeers(t *testing.T, clients map[string]kubernetes.Interface) *[]string {
	t.Helper()
	var asked []string
	old := peerAPIServerClientset
	peerAPIServerClientset = func(server string) (kubernetes.Interface, error) {
		asked = append(asked, server)
		cs, ok := clients[server]
		if !ok {
			return nil, fmt.Errorf("no apiserver at %s", server)
		}
		return cs, nil
	}
	t.Cleanup(func() { peerAPIServerClientset = old })
	return &asked
}

func apiserverURL(ip string) string { return "https://" + ip + ":6443" }

// assertNoLeaseLeft checks that the run released both maintenance leases.
func assertNoLeaseLeft(t *testing.T, cs kubernetes.Interface) {
	t.Helper()
	leases, err := cs.CoordinationV1().Leases(fenceTestMaintenanceNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range leases.Items {
		t.Errorf("lease %s/%s left behind, held by %s", l.Namespace, l.Name, *l.Spec.HolderIdentity)
	}
}

// TestLeafRotationWithoutVIPRenewsTheLeaseThroughAnotherAPIServer: without a
// VIP the local apiserver is the only one a control plane's kubeconfigs name,
// and it stops answering while the rotation restarts it. The lease must ride
// another control plane's apiserver for the run, or it lapses mid-restart and
// the next control plane restarts beside this one.
func TestLeafRotationWithoutVIPRenewsTheLeaseThroughAnotherAPIServer(t *testing.T) {
	local := noVIPControlPlane(t, noVIPSelf, noVIPPeer)
	leasesStopAnsweringAfterTake(local)
	peer := sameCluster(local)
	asked := usePeers(t, map[string]kubernetes.Interface{apiserverURL(noVIPPeer): peer})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rotated, err := RotateContext(ctx, io.Discard, Options{Quiet: true})
	if err != nil || !rotated {
		t.Fatalf("rotation with the local apiserver down = (%v, %v), want it to renew the lease through %s and finish", rotated, err, noVIPPeer)
	}
	if want := []string{apiserverURL(noVIPPeer)}; !slices.Equal(*asked, want) {
		t.Errorf("apiservers asked = %v, want %v", *asked, want)
	}
	assertNoLeaseLeft(t, peer)
}

// TestLeaseRidesThePeerThroughALocalOutageLongerThanTheLease: once the lease
// rides another apiserver, the background renewal alone, with no Confirm,
// keeps both maintenance leases renewed through it while the local apiserver
// stays down for longer than the lease, and another run still cannot take
// them. The clock is fake and the renewal fast, so the outage is two lease
// durations long and the test takes milliseconds.
func TestLeaseRidesThePeerThroughALocalOutageLongerThanTheLease(t *testing.T) {
	local := fake.NewSimpleClientset(apiserverEndpoints(noVIPSelf, noVIPPeer))
	leasesStopAnsweringAfterTake(local)
	peer := sameCluster(local)
	usePeers(t, map[string]kubernetes.Interface{apiserverURL(noVIPPeer): peer})

	var now atomic.Int64
	now.Store(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC).UnixNano())
	oldNow, oldEvery, oldRelease := maintlease.Now, maintlease.RenewEvery, maintlease.ReleaseRetryBudget
	maintlease.Now = func() time.Time { return time.Unix(0, now.Load()) }
	maintlease.RenewEvery = 10 * time.Millisecond
	maintlease.ReleaseRetryBudget = 0
	t.Cleanup(func() {
		maintlease.Now, maintlease.RenewEvery, maintlease.ReleaseRetryBudget = oldNow, oldEvery, oldRelease
	})

	ctx := context.Background()
	rider, guard, release, err := acquireMaintenanceLease(ctx, local, "cp1/100/1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := rideLeaseOffLocalAPIServer(ctx, io.Discard, rider, guard, local, noVIPSelf); err != nil {
		t.Fatal(err)
	}

	later := time.Unix(0, now.Add(int64(2*maintlease.Duration)))
	names := []string{"stratum-cluster-upgrade", fenceTestMaintenanceLease}
	deadline := time.Now().Add(5 * time.Second)
	for _, name := range names {
		for {
			l, err := peer.CoordinationV1().Leases(fenceTestMaintenanceNS).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !l.Spec.RenewTime.Time.Before(later) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("lease %s last renewed at %s, want a background renewal through %s at %s or later",
					name, l.Spec.RenewTime.Time, noVIPPeer, later)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if err := guard.Err(); err != nil {
		t.Errorf("guard.Err() = %v after renewals through %s, want nil", err, noVIPPeer)
	}
	for _, name := range names {
		_, releaseOther, err := maintlease.Acquire(ctx, peer, fenceTestMaintenanceNS, name, "cp2/200/1", "maintenance run")
		if releaseOther != nil {
			releaseOther()
		}
		if !errors.Is(err, maintlease.ErrTaken) {
			t.Errorf("another run's take of %s = %v, want %v", name, err, maintlease.ErrTaken)
		}
	}
}

// TestLeafRotationOnTheOnlyControlPlaneKeepsItsOwnAPIServer: the only control
// plane has no other apiserver, and no other control plane can take the lease
// while its own is down.
func TestLeafRotationOnTheOnlyControlPlaneKeepsItsOwnAPIServer(t *testing.T) {
	local := noVIPControlPlane(t, noVIPSelf)
	asked := usePeers(t, nil)

	var out bytes.Buffer
	if rotated, err := RotateContext(context.Background(), &out, Options{Quiet: true}); err != nil || !rotated {
		t.Fatalf("Rotate = (%v, %v), want a finished rotation", rotated, err)
	}
	if len(*asked) != 0 {
		t.Errorf("the only control plane asked for apiservers %v", *asked)
	}
	if strings.Contains(out.String(), "WARN") {
		t.Errorf("the only control plane warned:\n%s", out.String())
	}
	assertNoLeaseLeft(t, local)
}

// TestLeafRotationWithoutVIPSkipsAnAPIServerThatDoesNotRenew: an apiserver
// that does not renew the lease is passed over for the next one listed.
func TestLeafRotationWithoutVIPSkipsAnAPIServerThatDoesNotRenew(t *testing.T) {
	local := noVIPControlPlane(t, noVIPSelf, noVIPPeer, noVIPThird)
	leasesStopAnsweringAfterTake(local)
	third := sameCluster(local)
	asked := usePeers(t, map[string]kubernetes.Interface{
		apiserverURL(noVIPPeer):  silentAPIServer(local),
		apiserverURL(noVIPThird): third,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if rotated, err := RotateContext(ctx, io.Discard, Options{Quiet: true}); err != nil || !rotated {
		t.Fatalf("RotateContext = (%v, %v), want the lease renewed through %s", rotated, err, noVIPThird)
	}
	if want := []string{apiserverURL(noVIPPeer), apiserverURL(noVIPThird)}; !slices.Equal(*asked, want) {
		t.Errorf("apiservers asked = %v, want %v", *asked, want)
	}
	assertNoLeaseLeft(t, third)
}

// TestLeafRotationWithoutVIPWarnsWhenNoOtherAPIServerRenews: with no other
// apiserver renewing, the run keeps its own, as it did before it looked, and
// says the lease may lapse while it restarts.
func TestLeafRotationWithoutVIPWarnsWhenNoOtherAPIServerRenews(t *testing.T) {
	local := noVIPControlPlane(t, noVIPSelf, noVIPPeer)
	usePeers(t, map[string]kubernetes.Interface{apiserverURL(noVIPPeer): silentAPIServer(local)})

	var out bytes.Buffer
	if rotated, err := RotateContext(context.Background(), &out, Options{Quiet: true}); err != nil || !rotated {
		t.Fatalf("Rotate = (%v, %v), want a finished rotation through the local apiserver", rotated, err)
	}
	if !strings.Contains(out.String(), "WARN") || !strings.Contains(out.String(), apiserverURL(noVIPPeer)) {
		t.Errorf("no warning naming the apiserver that did not renew:\n%s", out.String())
	}
	assertNoLeaseLeft(t, local)
}

// TestLeafRotationWithoutVIPWarnsWhenTheAPIServersCannotBeListed: apiservers
// that cannot be listed leave the lease on the local one, with the same single
// warning.
func TestLeafRotationWithoutVIPWarnsWhenTheAPIServersCannotBeListed(t *testing.T) {
	local := noVIPControlPlane(t, noVIPSelf, noVIPPeer)
	if err := local.Tracker().Delete(discoveryv1.SchemeGroupVersion.WithResource("endpointslices"), metav1.NamespaceDefault, "kubernetes"); err != nil {
		t.Fatal(err)
	}
	asked := usePeers(t, nil)

	var out bytes.Buffer
	if rotated, err := RotateContext(context.Background(), &out, Options{Quiet: true}); err != nil || !rotated {
		t.Fatalf("Rotate = (%v, %v), want a finished rotation through the local apiserver", rotated, err)
	}
	if n := strings.Count(out.String(), "WARN"); n != 1 || !strings.Contains(out.String(), "cannot list the other apiservers") {
		t.Errorf("want one warning naming the failed listing, got %d:\n%s", n, out.String())
	}
	if len(*asked) != 0 {
		t.Errorf("apiservers asked = %v with none listed", *asked)
	}
	assertNoLeaseLeft(t, local)
}

// TestPeerAPIServersExcludesThisNodeHoweverItsAddressIsWritten: node state may
// spell an IPv6 address differently from the apiserver's endpoint.
func TestPeerAPIServersExcludesThisNodeHoweverItsAddressIsWritten(t *testing.T) {
	cs := fake.NewSimpleClientset(apiserverEndpoints("fd00::12", "fd00::13"))
	got, err := peerAPIServers(context.Background(), cs, []string{"192.168.99.12", "fd00:0:0::12"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"https://[fd00::13]:6443"}; !slices.Equal(got, want) {
		t.Errorf("peerAPIServers = %v, want %v", got, want)
	}
}

// TestLeafRotationWithoutVIPStopsOnALeaseTakenMeanwhile: a lease another run
// took while this one chose an apiserver is lost, not a reason to fall back to
// the local one.
func TestLeafRotationWithoutVIPStopsOnALeaseTakenMeanwhile(t *testing.T) {
	local := noVIPControlPlane(t, noVIPSelf, noVIPPeer)
	peer := sameCluster(local)
	peer.PrependReactor("get", "leases", func(a clienttesting.Action) (bool, runtime.Object, error) {
		l := liveMaintenanceLease("sh-cp3/28111/1")
		l.Name = a.(clienttesting.GetAction).GetName()
		return true, l, nil
	})
	usePeers(t, map[string]kubernetes.Interface{apiserverURL(noVIPPeer): peer})

	before := readLeaf(t, "apiserver.crt")
	_, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true})
	if !errors.Is(err, maintlease.ErrTaken) || !strings.Contains(err.Error(), "sh-cp3/28111/1") ||
		!strings.Contains(err.Error(), "stopping before reissuing leaves") {
		t.Fatalf("Rotate = %v, want it stopped before reissuing leaves on the lease sh-cp3 took", err)
	}
	if !bytes.Equal(before, readLeaf(t, "apiserver.crt")) {
		t.Error("the rotation reissued leaves after losing the lease")
	}
}

// TestLeafRotationWithVIPDoesNotLookForAnotherAPIServer: a VIP renews the
// lease through whichever apiserver holds it, so a VIP node never looks for
// another.
func TestLeafRotationWithVIPDoesNotLookForAnotherAPIServer(t *testing.T) {
	kubeVIPNode(t)
	asked := usePeers(t, nil)
	if rotated, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true}); err != nil || !rotated {
		t.Fatalf("Rotate = (%v, %v), want a finished rotation", rotated, err)
	}
	if len(*asked) != 0 {
		t.Errorf("a VIP node asked for apiservers %v", *asked)
	}
}

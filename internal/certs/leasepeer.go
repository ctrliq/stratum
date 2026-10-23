package certs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
)

// peerAPIServerClientset is this node's admin client pointed at the apiserver
// at server instead of the one its kubeconfig names; a test replaces it.
var peerAPIServerClientset = func(server string) (kubernetes.Interface, error) {
	cfg, err := k8sutil.AdminRESTConfig()
	if err != nil {
		return nil, err
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Host = server
	return kubernetes.NewForConfig(cfg)
}

// rideLeaseOffLocalAPIServer moves the maintenance lease's API calls to
// another control plane's apiserver before a rotation restarts this one.
// Without a VIP every kubeconfig on a control plane names its own apiserver,
// which answers nothing while its etcd and apiserver restart, so a restart
// that outlasts the lease lets the next control plane take it and restart
// beside this one. The candidates are the apiservers the kubernetes Service
// publishes, other than one at this node's nodeIP ("v4" or "v4,v6"), tried in
// turn until one renews the lease.
//
// The lease stays on local, the client it was taken on, when there is no
// other apiserver: no other control plane can take it while this one's is
// down. It stays there too, with a warning, when none of the others renews it
// or they cannot be listed. A lease found taken is lost, and stops the run.
func rideLeaseOffLocalAPIServer(ctx context.Context, w io.Writer, rider *leaseRider, guard *maintlease.Guard,
	local kubernetes.Interface, nodeIP string) error {
	peers, err := peerAPIServers(ctx, local, strings.Split(nodeIP, ","))
	var failed []string
	if err != nil {
		failed = append(failed, err.Error())
	}
	for _, server := range peers {
		cs, err := peerAPIServerClientset(server)
		if err == nil {
			rider.swap(cs)
			if err = guard.Confirm(ctx); err == nil {
				return nil
			}
			if errors.Is(err, maintlease.ErrTaken) {
				return fmt.Errorf("the maintenance lease could not be confirmed; stopping before reissuing leaves: %w", err)
			}
		}
		failed = append(failed, fmt.Sprintf("%s: %v", server, err))
	}
	rider.swap(local)
	if len(failed) > 0 {
		fmt.Fprintf(w, "  WARN: no other apiserver renewed the maintenance lease (%s); renewing it through this "+
			"node's own, so a restart longer than %s can let another control plane take it\n",
			strings.Join(failed, "; "), maintlease.Duration)
	}
	return nil
}

// peerAPIServers lists, as URLs, the ready apiservers the kubernetes Service
// publishes, other than one at any of nodeIPs.
func peerAPIServers(ctx context.Context, cs kubernetes.Interface, nodeIPs []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, maintlease.CallTimeout)
	defer cancel()
	slice, err := cs.DiscoveryV1().EndpointSlices(metav1.NamespaceDefault).Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("cannot list the other apiservers: %w", err)
	}
	var port int32
	for _, p := range slice.Ports {
		if p.Name != nil && *p.Name == "https" && p.Port != nil {
			port = *p.Port
		}
	}
	if port == 0 {
		return nil, errors.New("cannot list the other apiservers: the kubernetes Service publishes no https port")
	}
	var peers []string
	for _, e := range slice.Endpoints {
		if e.Conditions.Ready != nil && !*e.Conditions.Ready {
			continue
		}
		for _, ip := range e.Addresses {
			if !slices.ContainsFunc(nodeIPs, func(own string) bool { return net.ParseIP(own).Equal(net.ParseIP(ip)) }) {
				peers = append(peers, "https://"+net.JoinHostPort(ip, strconv.Itoa(int(port))))
			}
		}
	}
	return peers, nil
}

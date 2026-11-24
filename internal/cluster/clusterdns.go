package cluster

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/version"
)

// Indirected so a worker start is testable without a cluster.
var (
	fetchClusterDNSIP    = kubeDNSServiceIP
	saveState            = nodestate.SaveState
	writeKubeletConf     = runtime.WriteKubeletConfig
	writeCoreDNSUpstream = runtime.WriteCoreDNSUpstream
)

// A cluster the worker cannot reach is no failure: the next start tries again.
func learnClusterDNS(st *NodeState) (changed bool) {
	if st.Role != "worker" || st.ServiceCIDR != "" {
		return false
	}
	current := st.ClusterDNSIP
	if current == "" {
		// The address the kubelet renders with no range and nothing recorded, so no change.
		current = netcfg.Default().DNSIPv4
	}
	ip, err := fetchClusterDNSIP()
	if err != nil {
		fmt.Printf("stratum: could not read the cluster DNS address from the kube-dns Service, keeping %s: %v\n", current, err)
		return false
	}
	if ip == current {
		return false
	}
	fmt.Printf("stratum: cluster DNS is %s, read from the kube-dns Service (was %s)\n", ip, current)
	st.ClusterDNSIP = ip
	return true
}

func kubeDNSServiceIP() (string, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig"))
	if err != nil {
		return "", err
	}
	cfg.Timeout = 10 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	svc, err := cs.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if net.ParseIP(svc.Spec.ClusterIP) == nil {
		return "", fmt.Errorf("the kube-dns Service has no usable ClusterIP (%q)", svc.Spec.ClusterIP)
	}
	return svc.Spec.ClusterIP, nil
}

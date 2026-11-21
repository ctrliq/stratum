package staticpod

import (
	"fmt"
	"path/filepath"

	"github.com/ctrliq/stratum/internal/enginesettings"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	stratumpki "github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// rendered from the persisted node state on every write, preserving ArgsFn semantics.

func etcdArgs(st *nodestate.NodeState) []string {
	pki := func(f string) string { return filepath.Join(version.PKIDir, f) }
	clientURL := nodestate.EtcdClientURL(st.NodeIP)
	peerURL := nodestate.EtcdPeerURL(st.NodeIP)
	return []string{
		"--name", st.EtcdName,
		"--data-dir", version.EtcdDataDir,
		"--listen-client-urls", fmt.Sprintf("https://127.0.0.1:%d,%s", version.EtcdClientPort, clientURL),
		"--advertise-client-urls", clientURL,
		"--listen-peer-urls", peerURL,
		"--initial-advertise-peer-urls", peerURL,
		"--initial-cluster", st.EtcdInitialCluster,
		"--initial-cluster-state", st.EtcdState,
		"--initial-cluster-token", version.EtcdClusterToken,
		// plain-HTTP health endpoint for the liveness probe; the client port needs a cert.
		"--listen-metrics-urls", fmt.Sprintf("http://127.0.0.1:%d", version.EtcdMetricsPort),
		"--cert-file", pki("etcd.crt"), "--key-file", pki("etcd.key"),
		"--trusted-ca-file", pki("etcd-ca-bundle.crt"), "--client-cert-auth",
		"--peer-cert-file", pki("etcd.crt"), "--peer-key-file", pki("etcd.key"),
		"--peer-trusted-ca-file", pki("etcd-ca-bundle.crt"), "--peer-client-cert-auth",
		"--peer-cert-allowed-cn", stratumpki.EtcdCN,
	}
}

func apiserverArgs(st *nodestate.NodeState, p profile.Profile, pki func(string) string) []string {
	args := []string{
		"--advertise-address=" + st.NodeIP,
		fmt.Sprintf("--secure-port=%d", version.APIServerPort),
		fmt.Sprintf("--etcd-servers=https://127.0.0.1:%d", version.EtcdClientPort),
		"--etcd-cafile=" + pki("etcd-ca-bundle.crt"),
		"--etcd-certfile=" + pki("apiserver-etcd-client.crt"),
		"--etcd-keyfile=" + pki("apiserver-etcd-client.key"),
		"--client-ca-file=" + pki("ca-bundle.crt"),
		"--enable-bootstrap-token-auth=true",
		"--tls-cert-file=" + pki("apiserver.crt"), "--tls-private-key-file=" + pki("apiserver.key"),
		"--kubelet-client-certificate=" + pki("apiserver-kubelet-client.crt"),
		"--kubelet-client-key=" + pki("apiserver-kubelet-client.key"),
		"--kubelet-preferred-address-types=InternalIP,Hostname",
		"--service-account-key-file=" + pki("sa-bundle.pub"),
		"--service-account-signing-key-file=" + pki(signer("sa")+".key"),
		"--service-account-issuer=https://kubernetes.default.svc." + st.Domain(),
		"--api-audiences=https://kubernetes.default.svc." + st.Domain(),
		"--admission-control-config-file=" + version.AdmissionWebhookConfig(),
		"--authorization-mode=Node,RBAC", "--enable-admission-plugins=NodeRestriction",
		"--service-cluster-ip-range=" + st.ServiceCIDR,
		"--allow-privileged=true",
		"--profiling=false", "--request-timeout=300s", "--service-account-lookup=true",
		"--tls-cipher-suites=" + version.TLSCipherSuites,
		"--requestheader-client-ca-file=" + pki("front-proxy-ca-bundle.crt"),
		"--requestheader-allowed-names=front-proxy-client",
		"--requestheader-extra-headers-prefix=X-Remote-Extra-",
		"--requestheader-group-headers=X-Remote-Group",
		"--requestheader-username-headers=X-Remote-User",
		"--proxy-client-cert-file=" + pki("front-proxy-client.crt"),
		"--proxy-client-key-file=" + pki("front-proxy-client.key"),
	}
	// profile flags in their order, then the flags every render carries.
	args = append(args, p.APIServerArgs(version.ProfileLocations(pki))...)
	args = append(args, enginesettings.APIServerFlags(pki)...)
	return append(args, enginesettings.ArgvAnchor)
}

func cmArgs(st *nodestate.NodeState, pki, kc func(string) string) []string {
	args := []string{
		"--kubeconfig=" + kc("controller-manager.kubeconfig"),
		"--authentication-kubeconfig=" + kc("controller-manager.kubeconfig"),
		"--authorization-kubeconfig=" + kc("controller-manager.kubeconfig"),
		"--client-ca-file=" + pki("ca-bundle.crt"),
		"--cluster-signing-cert-file=" + pki(signer("ca")+".crt"), "--cluster-signing-key-file=" + pki(signer("ca")+".key"),
		fmt.Sprintf("--cluster-signing-duration=%dh", int(stratumpki.CAValidity.Hours())),
		"--root-ca-file=" + pki("ca-bundle.crt"),
		"--service-account-private-key-file=" + pki(signer("sa")+".key"),
		"--use-service-account-credentials=true",
		// bootstrapsigner is off by default; it signs cluster-info for joining nodes.
		version.ControllerManagerControllersFlag,
		"--profiling=false", "--bind-address=127.0.0.1",
		"--allocate-node-cidrs=true", "--cluster-cidr=" + st.PodCIDR,
	}
	if st.DualStack {
		args = append(args,
			fmt.Sprintf("--node-cidr-mask-size-ipv4=%d", netcfg.NodeCIDRMask),
			fmt.Sprintf("--node-cidr-mask-size-ipv6=%d", netcfg.NodeCIDRMaskV6))
	} else {
		args = append(args, fmt.Sprintf("--node-cidr-mask-size=%d", netcfg.NodeCIDRMask))
	}
	return args
}

func schedulerArgs() []string {
	kc := version.SchedulerKubeconfig()
	return []string{
		"--kubeconfig=" + kc,
		"--authentication-kubeconfig=" + kc,
		"--authorization-kubeconfig=" + kc,
		"--profiling=false", "--bind-address=127.0.0.1",
	}
}

// its kubeconfig points at the local apiserver: leader election through the VIP is circular.
func kubeVipArgs(st *nodestate.NodeState) []string {
	args := []string{
		"manager",
		"--address", st.VIP.Address,
		"--port", fmt.Sprint(version.APIServerPort),
		"--controlplane",
		"--leaderElection",
		"--prometheusHTTPServer", "",
		"--k8sConfigPath", version.KubeVipKubeconfig(),
	}
	// omitting --interface asks for default-gateway auto-detection; there is no flag for it.
	if st.VIP.Interface != "" {
		args = append(args, "--interface", st.VIP.Interface)
	}
	if st.VIP.BGP {
		args = append(args, "--bgp")
	} else {
		args = append(args, "--arp")
	}
	return args
}

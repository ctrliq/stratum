package cluster

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profilefiles"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/version"
)

// Host seams a test can stand in for a host it cannot prepare or detect on.
var (
	hostHostname      = host.Hostname
	hostDirs          = host.Dirs
	hostPrereqs       = host.Prereqs
	hostTrustPodCIDRs = host.TrustPodCIDRs
	hostDetectNodeIPs = host.DetectNodeIPs
)

// Everything a fresh controller needs before the supervisor starts; idempotent.
func PrepareController(cfg *ClusterConfig, nodeIPFlag string, releaseDataLock func()) (*NodeState, error) {
	return PrepareControllerContext(context.Background(), cfg, nodeIPFlag, releaseDataLock)
}

// Stops between steps when its caller cancels: a step in flight finishes.
func PrepareControllerContext(ctx context.Context, cfg *ClusterConfig, nodeIPFlag string, releaseDataLock func()) (*NodeState, error) {
	return PrepareControllerWithLease(ctx, cfg, nodeIPFlag, nil, releaseDataLock)
}

// PrepareControllerContext that carries the CLI's maintenance lease through.
func PrepareControllerWithLease(ctx context.Context, cfg *ClusterConfig, nodeIPFlag string, lease *k8sutil.MaintenanceLease, releaseDataLock func()) (*NodeState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := lease.Confirm(ctx); err != nil {
		return nil, err
	}
	hostname, err := hostHostname()
	if err != nil {
		return nil, err
	}
	prof, err := profile.ForNode(cfg.ProfileName())
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// HA members would lack the at-rest key the Secrets split needs.
	mint, err := sharedSecretGate(prof, version.ProfileSecretFile, readEtcdJoinState)
	if err != nil {
		return nil, err
	}

	// Only a missing node.yaml is a first boot; an unreadable one holds membership.
	prev, err := LoadState()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read node state %s: %w; refusing to start from a guessed address and membership "+
			"-- repair or remove the file", StatePath(), err)
	}
	savedIP, err := savedControllerIP(prev)
	if err != nil {
		return nil, err
	}

	st := &NodeState{
		Role: "controller", Hostname: hostname,
		EtcdName:        hostname,
		EtcdState:       "new",
		Profile:         cfg.ProfileName(),
		CIS:             cfg.Spec.CIS,
		VIP:             cfg.Spec.VIP,
		NodeLabels:      cfg.Spec.Node.Labels,
		NodeTaints:      cfg.Spec.Node.Taints,
		RuntimeHandlers: cfg.Spec.Node.RuntimeHandlers,
	}
	warnChangedTaints(os.Stderr, st, prev)
	if st.VIP.Address == "" && prev != nil {
		st.VIP.Address = prev.VIP.Address
	}
	// A corroborated record wins, else the VIP, else the local apiserver.
	st.Server = recordedServer(prev, st)
	st.Server = serverURL(st)

	pendingNetworkSaved := false
	a := ha.NodeAssembly{
		HostDirs: hostDirs,
		Prereqs:  hostPrereqs,
		ResolveNet: func() (netcfg.Config, error) {
			if err := ctx.Err(); err != nil {
				return netcfg.Config{}, err
			}
			established, err := networkEstablished(prev)
			if err != nil {
				return netcfg.Config{}, err
			}
			netc, err := resolveNetConfig(cfg.Spec.Network, prev)
			if err != nil {
				return netcfg.Config{}, err
			}
			pendingNetworkSaved = !established
			if err := cfg.Spec.Network.Cilium.CheckStack(netc.DualStack); err != nil {
				return netcfg.Config{}, fmt.Errorf("%s: %w", cfg.path, err)
			}
			st.DualStack, st.PodCIDR, st.ServiceCIDR, st.ClusterDomain =
				netc.DualStack, netc.PodCIDR, netc.ServiceCIDR, netc.ClusterDomain
			return netc, nil
		},
		TrustPodCIDRs: hostTrustPodCIDRs,
		ResolveNodeIP: host.ResolveNodeIP,
		DetectNodeIPs: hostDetectNodeIPs,
		BeforeCerts: func(choice host.NodeIPChoice) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Printf("stratum: node IP %s (%s)\n", choice.IP, choice.Source)
			nodeIP := choice.IP
			primary := host.Primary(nodeIP)
			if err := nodeIPMoveRefusal(prev, primary, choice.Source); err != nil {
				return err
			}
			st.NodeIP, st.KubeletNodeIP = primary, nodeIP
			st.EtcdInitialCluster = hostname + "=" + nodestate.EtcdPeerURL(primary)
			keepEtcdMembership(st, prev)
			return nil
		},
		IssueCerts: func(host, nodeIP string, sans *pki.APISANs) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			k8sutil.Step(os.Stdout, "PKI")
			return pki.GenerateAll(host, nodeIP, sans)
		},
		WriteKubeconfigs: func(host, server string, vip bool) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			k8sutil.Step(os.Stdout, "Kubeconfigs")
			return kubeconfig.GenerateAll(host, server, vip)
		},
		WriteProfileConfigs: func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			k8sutil.Step(os.Stdout, "Control-plane configs for the "+prof.Name+" profile")
			if err := mintSharedSecrets(prof, mint, version.ProfileSecretFile,
				func() error { return lease.Confirm(ctx) }, readEtcdJoinState); err != nil {
				return err
			}
			return profilefiles.Write(prof)
		},
		// The kubelet running the static pods needs its configs.
		WriteRuntimeConfigs: runtime.PrepareNodeConfigs,
	}
	req := ha.AssembleRequest{
		Hostname:      hostname,
		NodeIPFlag:    nodeIPFlag,
		ConfigNodeIP:  cfg.Spec.Network.NodeIP,
		SavedIP:       savedIP,
		VIPAddress:    st.VIP.Address,
		APIServer:     st.Server,
		PrereqOptions: prereqOptions(cfg, prof),
		State:         st,
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := assembleNode(ctx, a, req); err != nil {
		if pendingNetworkSaved {
			journalPartialPreparation(prev)
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		if pendingNetworkSaved {
			journalPartialPreparation(prev)
		}
		return nil, err
	}
	// The returned state must agree with the node.yaml SaveState just normalized.
	st.Profile, st.CIS = st.ProfileName(), st.ProfileName() == nodestate.CISProfile
	// The whole-struct copy saves future NodeState fields; nil release refuses.
	err = nodestate.MutateHeld(releaseDataLock, func(cur *NodeState) error {
		keep := *cur
		*cur = *st
		cur.ConfigPath, cur.ClusterDNSIP = keep.ConfigPath, keep.ClusterDNSIP
		st.ConfigPath, st.ClusterDNSIP = keep.ConfigPath, keep.ClusterDNSIP
		return nil
	})
	if err != nil {
		if pendingNetworkSaved {
			journalPartialPreparation(prev)
		}
		return nil, err
	}
	clearPartialPreparationJournal()
	return st, nil
}

// An established node keeps its recorded network; a fresh or pending one takes spec.network.
func resolveNetConfig(n Network, prev *NodeState) (netcfg.Config, error) {
	established, err := networkEstablished(prev)
	if err != nil {
		return netcfg.Config{}, err
	}
	if !established {
		netc, err := netcfg.WithDefaults(n.PodCIDR, n.ServiceCIDR)
		if err != nil {
			return netcfg.Config{}, err
		}
		netc.ClusterDomain = netcfg.DomainOr(n.ClusterDomain)
		if err := netcfg.Save(netc.PodCIDR, netc.ServiceCIDR, netc.ClusterDomain); err != nil {
			return netcfg.Config{}, err
		}
		return netc, nil
	}
	domain, err := netcfg.ResolveDomain(n.ClusterDomain)
	if err != nil {
		return netcfg.Config{}, err
	}
	var recorded netcfg.Config
	_, err = os.Stat(version.NetConfigPath)
	switch {
	case err == nil:
		if recorded, err = netcfg.Load(); err != nil {
			return netcfg.Config{}, err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return netcfg.Config{}, fmt.Errorf("stat %s: %w", version.NetConfigPath, err)
	default:
		if prev != nil && prev.PodCIDR != "" && prev.ServiceCIDR != "" {
			if recorded, err = netcfg.Parse(prev.PodCIDR, prev.ServiceCIDR); err != nil {
				return netcfg.Config{}, fmt.Errorf("node state %s: %w", StatePath(), err)
			}
		} else if recorded, err = netcfg.Load(); err != nil {
			return netcfg.Config{}, err
		}
		recorded.ClusterDomain = domain
		if err := netcfg.Save(recorded.PodCIDR, recorded.ServiceCIDR, domain); err != nil {
			return netcfg.Config{}, err
		}
	}
	if (n.PodCIDR != "" && n.PodCIDR != recorded.PodCIDR) || (n.ServiceCIDR != "" && n.ServiceCIDR != recorded.ServiceCIDR) {
		fmt.Fprintf(os.Stderr, "stratum: WARNING: spec.network (pod %q, service %q) differs from this cluster's "+
			"recorded CIDRs (pod %s, service %s); ignoring it -- changing them needs a cluster rebuild; "+
			"if this node's recorded CIDRs are the wrong ones, restore %s from another control plane\n",
			n.PodCIDR, n.ServiceCIDR, recorded.PodCIDR, recorded.ServiceCIDR, version.NetConfigPath)
	}
	return recorded, nil
}

// Absence of node.yaml alone is not a fresh node: CA or etcd data establishes it.
func networkEstablished(prev *NodeState) (bool, error) {
	dependent, err := holdsRecordedNetworkDependents(prev)
	if err != nil || dependent {
		return dependent, err
	}
	network, err := networkEvidenceExists(version.NetConfigPath)
	if err != nil || !network {
		return false, err
	}
	pending, err := networkEvidenceExists(partialPreparationPath())
	return !pending, err
}

// Unknown evidence is not absence: an error preserves the recorded network.
func networkEvidenceExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect network evidence %s: %w", path, err)
	}
	return true, nil
}

func holdsRecordedNetworkDependents(prev *NodeState) (bool, error) {
	if prev != nil && prev.PodCIDR != "" && prev.ServiceCIDR != "" {
		return true, nil
	}
	// WAL-only etcd member data counts: unreadable history is not a fresh node.
	for _, path := range []string{filepath.Join(version.PKIDir, "ca.crt")} {
		present, err := networkEvidenceExists(path)
		if err != nil || present {
			return present, err
		}
	}
	return netcfg.RetainedMemberData(version.EtcdDataDir)
}

// Marks a start that persisted netconfig but never finished preparation.
func partialPreparationPath() string {
	return filepath.Join(version.K8sConfDir, "prepare-incomplete")
}

func partialPreparationJournaled() bool {
	_, err := os.Stat(partialPreparationPath())
	return err == nil
}

// Best-effort: a failed write reported here would bury the start's own error.
func journalPartialPreparation(prev *NodeState) {
	if _, err := os.Stat(version.NetConfigPath); err != nil {
		return
	}
	if dependent, err := holdsRecordedNetworkDependents(prev); err != nil || dependent {
		return
	}
	_ = atomicfile.WriteFile(partialPreparationPath(),
		[]byte("a first start stopped part-way after saving netconfig; nothing established depends on it yet\n"), 0o644)
}

// Once node.yaml persists the journal is inert; removal is best-effort like the write.
func clearPartialPreparationJournal() {
	_ = os.Remove(partialPreparationPath())
}

// The kubelet applies taints only when it creates the Node, so a change never reaches it.
func warnChangedTaints(w io.Writer, st, prev *NodeState) {
	if prev == nil || sameTaints(st.NodeTaints, prev.NodeTaints) {
		return
	}
	fmt.Fprintf(w, "stratum: WARN: spec.node.taints changed from %s to %s, and the kubelet applies taints only when it "+
		"registers the Node: node %s keeps its taints until you change them with `stratum kubectl taint node %s`. "+
		"A Node that registers again takes the new list\n",
		cmp.Or(strings.Join(prev.NodeTaints, ","), "none"), cmp.Or(strings.Join(st.NodeTaints, ","), "none"), st.Hostname, st.Hostname)
}

// A state file with no address is damaged, not a first boot.
func savedControllerIP(prev *NodeState) (string, error) {
	if prev == nil {
		return "", nil
	}
	// Prefer the kubelet form, but older state may carry only the primary.
	if prev.KubeletNodeIP != "" {
		return prev.KubeletNodeIP, nil
	}
	if prev.NodeIP != "" {
		return prev.NodeIP, nil
	}
	return "", fmt.Errorf("node state %s records no node address; refusing to re-detect one for a node "+
		"that already has state -- repair the file", StatePath())
}

// An HA member's recorded membership must never be reset to state=new.
func keepEtcdMembership(st, prev *NodeState) {
	if prev != nil && prev.EtcdInitialCluster != "" {
		st.EtcdName, st.EtcdInitialCluster, st.EtcdState = prev.EtcdName, prev.EtcdInitialCluster, prev.EtcdState
		st.EtcdHasPeers, st.EtcdPromotePending = prev.EtcdHasPeers, prev.EtcdPromotePending
	}
}

// The recorded endpoint wins, else the VIP, else the local apiserver.
func serverURL(st *NodeState) string {
	return kubeconfig.NodeServerURL(st.Server, st.VIP.Address)
}

// Keeps the Server record only when the kubeconfig files corroborate it.
func recordedServer(prev, st *NodeState) string {
	// Interface and BGP control announcement, not the clients' endpoint.
	if prev == nil || prev.Server == "" || prev.VIP.Address != st.VIP.Address {
		return ""
	}
	if !kubeconfigsTarget(prev.Server) {
		return ""
	}
	return prev.Server
}

// Deliberately without kube-vip's kubeconfig and the join's bootstrap.conf.
func recordKubeconfigs() []string {
	files := []string{version.AdminConf}
	for _, f := range []string{"admin.kubeconfig", "controller-manager.kubeconfig", "scheduler.kubeconfig", "kubelet.kubeconfig"} {
		files = append(files, filepath.Join(version.KubeconfigDir, f))
	}
	return files
}

func kubeconfigsTarget(server string) bool {
	found := false
	for _, f := range recordKubeconfigs() {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		cur, err := kubeconfig.CurrentServer(f)
		if err != nil || cur != server {
			return false
		}
		found = true
	}
	return found
}

// An etcd member's address is baked into its membership and certs; refuse half of a move.
func nodeIPMoveRefusal(prev *NodeState, primary, source string) error {
	if prev == nil || prev.EtcdInitialCluster == "" {
		return nil
	}
	recorded := prev.NodeIP
	if recorded == "" {
		recorded = host.Primary(prev.KubeletNodeIP)
	}
	if recorded == primary {
		return nil
	}
	return fmt.Errorf("node IP resolved to %s (%s), but this node is an etcd member at %s: its membership and "+
		"certificates name %s, so starting on %s would leave it advertising an address its cluster does not know. "+
		"Remove the override to keep %s. Changing a control plane's IP in place is not supported: for an HA "+
		"control plane, first remove this node's etcd member from a remaining control plane "+
		"(`stratum etcdctl member remove <member-id>`, see `stratum etcdctl member list`), then remove this node "+
		"(`stratum uninstall`) and rejoin it with the new IP",
		primary, source, recorded, recorded, primary, recorded)
}

// Any node.yaml, even one that cannot be read, means an existing node.
func prereqOptions(cfg *ClusterConfig, prof profile.Profile) host.PrereqOptions {
	_, err := LoadState()
	return host.PrereqOptions{Profile: prof, SELinuxOptOut: cfg.SELinuxOptOut(), Existing: !errors.Is(err, fs.ErrNotExist)}
}

// workerSELinuxPath is a var so a test can redirect it.
var workerSELinuxPath = DefaultConfigPath

// Reads the node's SELinux settings the way its own next start would resolve them.
func NodeSELinuxOptions() (selinux.Options, error) {
	st, err := LoadState()
	if err != nil {
		return selinux.Options{}, err
	}
	if st.Role == "worker" {
		p, err := profile.Select(st.ProfileName())
		if err != nil {
			return selinux.Options{}, err
		}
		o := selinux.Options{RequiresSELinux: p.RequiresSELinux, Profile: p.Name, OptOut: st.SELinuxOptOut}
		optOut, set, err := WorkerSELinux(workerSELinuxPath)
		if err != nil {
			return selinux.Options{}, err
		}
		if set {
			o.OptOut = optOut
		}
		return o, nil
	}
	path, _ := activeConfigPath(LoadState, io.Discard)
	cfg, err := LoadConfig(path)
	if err != nil {
		return selinux.Options{}, err
	}
	// A node brought up with --profile keeps the profile in its state only.
	name := cfg.ProfileName()
	if name == "" {
		name = st.ProfileName()
	}
	p, err := profile.ForNode(name)
	if err != nil {
		return selinux.Options{}, err
	}
	return selinux.Options{RequiresSELinux: p.RequiresSELinux, Profile: p.Name, OptOut: cfg.SELinuxOptOut()}, nil
}

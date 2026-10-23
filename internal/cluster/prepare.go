package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

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

// The host-touching calls of PrepareController sit behind seams so a test can
// stand in for a host it cannot prepare (dirs, sysctls, modules, firewalld) or
// detect addresses on. Behavior-neutral indirections over internal/host.
var (
	hostHostname      = host.Hostname
	hostDirs          = host.Dirs
	hostPrereqs       = host.Prereqs
	hostTrustPodCIDRs = host.TrustPodCIDRs
	hostDetectNodeIPs = host.DetectNodeIPs
)

// PrepareController does everything a fresh first controller needs before the
// supervisor starts: dirs, prereqs, PKI, kubeconfigs, component configs, node
// state. Idempotent — a restarted controller re-runs it cheaply.
//
// The common node steps run through the ONE shared assembly the HA join runs
// (assembleNode, internal/ha/join.go): the controller-specific steps hang off
// it at the points the two paths' pinned orders fix — the netconfig resolution
// as ResolveNet, the IP-move refusal and node-state completion as BeforeCerts.
//
// nodeIPFlag is --node-ip from the command line ("" when not given); it outranks
// spec.network.nodeIP (host.ResolveNodeIP has the whole precedence).
// releaseDataLock is the release of the etcd data lock the caller holds across
// preparation (`stratum up` takes it before it calls this and releases it
// after); the node-state save writes under that held lock. A caller not
// holding the lock passes nil, and the save refuses rather than write unlocked.
func PrepareController(cfg *ClusterConfig, nodeIPFlag string, releaseDataLock func()) (*NodeState, error) {
	return PrepareControllerContext(context.Background(), cfg, nodeIPFlag, releaseDataLock)
}

// PrepareControllerContext stops preparation between steps when its caller
// cancels: the entry check refuses a run that arrived already stopped, and
// the shared assembly checks the context before each step, so a
// step in flight finishes before the next mutation is considered.
// releaseDataLock is as PrepareController's: the release of the held etcd data
// lock; nil makes the final node-state save refuse.
func PrepareControllerContext(ctx context.Context, cfg *ClusterConfig, nodeIPFlag string, releaseDataLock func()) (*NodeState, error) {
	return PrepareControllerWithLease(ctx, cfg, nodeIPFlag, nil, releaseDataLock)
}

// PrepareControllerWithLease carries the CLI's CIS-key maintenance fence through
// host preparation so the eventual key write can confirm ownership again.
// releaseDataLock is as PrepareController's: the release of the held etcd data
// lock; nil makes the final node-state save refuse.
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
	// The profile this control plane renders, before anything on the host
	// changes: a node that names one this binary did not register is refused
	// as it is, never rendered without it.
	prof, err := profile.ForNode(cfg.ProfileName())
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Only a sole control plane may mint the profile's shared secrets: the
	// cis profile's at-rest key, which the cluster's other apiservers would
	// lack, splits its Secrets. Before anything on
	// the host changes, so a refusal leaves the node as it was.
	mint, err := sharedSecretGate(prof, version.ProfileSecretFile, readEtcdJoinState)
	if err != nil {
		return nil, err
	}

	// A restarted node keeps the address it was brought up with unless something
	// explicit says otherwise: its apiserver cert and etcd peer URL name it, so
	// re-detecting on a multi-NIC host could quietly move the node.
	//
	// Only a missing node.yaml is a first boot. An unreadable or malformed one
	// belongs to a node that has state we cannot see: guessing an address and
	// writing a fresh state=new membership over it would lose the HA membership
	// it records. The state also stands in for a missing
	// netconfig (resolveNetConfig), so it is read before the CIDRs are chosen.
	prev, err := LoadState()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read node state %s: %w; refusing to start from a guessed address and membership "+
			"-- repair or remove the file", StatePath(), err)
	}
	savedIP, err := savedControllerIP(prev)
	if err != nil {
		return nil, err
	}

	// The node state is built before the assembly runs, so the assembly's
	// runtime-config write renders from it; its not-yet-known fields — the
	// netconfig's CIDRs, the resolved node IP, the carried membership — are
	// filled in by the caller-owned hooks as the assembly resolves them.
	st := &NodeState{
		Role: "controller", Hostname: hostname,
		EtcdName:  hostname,
		EtcdState: "new",
		Profile:   cfg.ProfileName(),
		CIS:       cfg.Spec.CIS,
		VIP:       cfg.Spec.VIP,
	}
	// A join can inherit its VIP from authenticated bundle metadata without
	// changing the config file. An unspecified address keeps that recorded
	// address on restart; explicit addresses and local interface/BGP settings
	// still come from the current config.
	if st.VIP.Address == "" && prev != nil {
		st.VIP.Address = prev.VIP.Address
	}
	// The endpoint record rides across a restart: the writers that chose an
	// endpoint — the HA join and ha-repoint among them — record it in the
	// state and write the kubeconfigs against it in the same run, and the
	// next start keeps a record the kubeconfigs on disk corroborate
	// (recordedServer). A record the files do not back is a leftover of the
	// previous release's join, which recorded the join URL its clients never
	// targeted (old bring-up re-derived at every start): that restart
	// re-derives by the rule, and the derived endpoint rewrites both the
	// kubeconfigs and the record. A changed spec.vip.address is the operator moving
	// the cluster's API address, so that restart re-derives the way a first
	// boot does; a fresh boot has no record and derives too. serverURL reads
	// only what is known here: the kubeconfigs the assembly writes target
	// this endpoint, on a fresh boot and on a restart alike.
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
			// A fresh boot seeds a single-member cluster; keepEtcdMembership lets
			// a pre-existing state win, so a restarted member keeps its recorded
			// membership and is never reset to state=new.
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
		// Every node runs kubelet+containerd: the control plane is static
		// pods, and the kubelet that runs them needs its configs.
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
	// SaveState normalizes only the state it writes, so the returned state must
	// carry the same spelling the file just got or the run phase disagrees with
	// the saved node.yaml.
	st.Profile, st.CIS = st.ProfileName(), st.ProfileName() == nodestate.CISProfile
	// The save goes through the mutation owner's under-lock variant: `stratum up`
	// holds the etcd data lock across preparation, so the owner re-reads node.yaml
	// and this write lands on what it read. Copy the whole struct, as the
	// whole-file save did, so a field added to NodeState later is saved without
	// anyone remembering to extend a list; only the two fields other owners
	// record (recordConfigPath, learnClusterDNS) are kept from disk. A nil
	// release means the caller holds no lock, and the owner refuses rather
	// than write unlocked.
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

// resolveNetConfig picks the cluster's CIDRs and domain for this start and
// persists them. What wins is the node's establishment, read
// from node state, the CA and network files and retained etcd data together:
//
//   - An established node — one holding anything that depends on the network a
//     start recorded (networkEstablished) — keeps the recorded network. Once
//     netconfig exists it wins: the CIDRs and the domain are fixed at first
//     boot (joins and rotations inherit them), and spec.network, or its
//     absence, must not move them. A node with state but no netconfig predates
//     it (or lost it), and its state records the cluster's CIDRs and domain,
//     so those stand in; a node with neither recovers them through
//     netcfg.Load from the CA or etcd data it holds. A configured domain that
//     differs from the recorded one is refused (netcfg.ResolveDomain), and
//     corrected CIDRs are ignored with a warning: changing them needs a
//     cluster rebuild.
//   - A fresh node — holding none of that — is a first boot: it takes
//     spec.network, and fills the half it leaves unset from the built-in
//     defaults.
//   - A pending node — a first start that stopped part-way after the network
//     resolution persisted netconfig, which journalPartialPreparation journaled
//     — takes spec.network too: nothing live or retained depends on the
//     earlier values yet, so a corrected spec.network converges onto the
//     correction instead of being ignored or refused.
//
// Absence of node.yaml alone is none of these: it does not by itself make a
// node fresh.
//
// The domain is resolved first, by netcfg.ResolveDomain, so a configured domain
// that differs from the recorded one is refused before anything is written.
func resolveNetConfig(n Network, prev *NodeState) (netcfg.Config, error) {
	established, err := networkEstablished(prev)
	if err != nil {
		return netcfg.Config{}, err
	}
	if !established {
		// A first boot, or a retry after one stopped part-way: nothing on this
		// node depends on the network a start recorded yet, so spec.network
		// wins wholesale, and the persisted netconfig is written (rewritten)
		// from it.
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
		// The node is established with no netconfig: its state records the
		// cluster's CIDRs and domain when it has the ranges, and otherwise the
		// CA or retained etcd data it holds stands in through netcfg.Load.
		// Either way the recorded network is what gets persisted, so joins and
		// rotations inherit it.
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

// networkEstablished reports whether anything on this node already depends on
// the network a start recorded, so the recorded network must win over
// spec.network. The evidence is read together: readable node
// state with the cluster's ranges, the cluster CA, retained etcd member data,
// or a netconfig that no partial-preparation journal marks as belonging to a
// first start that never finished. Absence of node.yaml alone is not a first
// boot: a node that lost node.yaml but holds the CA or etcd data is
// established all the same.
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

// Unknown evidence is not absence: refusing preserves the recorded network
// until the operator can inspect or repair the unreadable path.
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

// holdsRecordedNetworkDependents reports whether this node holds, apart from
// netconfig itself, anything the network resolution fed: node state with the
// cluster's ranges (state without them predates the resolution), the cluster
// CA, or retained etcd member data. A readable state exists only after a whole
// preparation succeeded, so any of these says the recorded network is in force
// whatever a journal of a stopped start says.
func holdsRecordedNetworkDependents(prev *NodeState) (bool, error) {
	if prev != nil && prev.PodCIDR != "" && prev.ServiceCIDR != "" {
		return true, nil
	}
	// The cluster CA, and etcd member data — database or WAL-only, which the
	// shared check treats the same: a WAL without its database is a member
	// whose history cannot be read, not a fresh node (netcfg.RetainedMemberData).
	for _, path := range []string{filepath.Join(version.PKIDir, "ca.crt")} {
		present, err := networkEvidenceExists(path)
		if err != nil || present {
			return present, err
		}
	}
	return netcfg.RetainedMemberData(version.EtcdDataDir)
}

// partialPreparationPath is the journal a first start leaves when it stops
// part-way after the network resolution persisted netconfig. node.yaml is
// written only after the whole assembly succeeds, so beside it the journal
// says the netconfig was saved by a preparation that never finished: a retry
// with a corrected spec.network may then converge onto the correction,
// because nothing live or retained depends on the earlier values.
// Once the node holds anything that does — the CA, etcd data, node state —
// the journal is disregarded and the recorded network wins as before.
func partialPreparationPath() string {
	return filepath.Join(version.K8sConfDir, "prepare-incomplete")
}

// partialPreparationJournaled reports whether the journal is on disk.
func partialPreparationJournaled() bool {
	_, err := os.Stat(partialPreparationPath())
	return err == nil
}

// journalPartialPreparation records a first start that stopped part-way after
// the network resolution persisted netconfig, so a retry may adopt a corrected
// spec.network. It runs on a start that is already failing, and it is
// best-effort on purpose: a journal that cannot be written leaves the retry on
// the conservative reading (the recorded network wins), while a failed write
// reported here would bury the start's own error, which is the one the
// operator has to see.
func journalPartialPreparation(prev *NodeState) {
	if _, err := os.Stat(version.NetConfigPath); err != nil {
		return // the start stopped before the network resolution persisted anything
	}
	if dependent, err := holdsRecordedNetworkDependents(prev); err != nil || dependent {
		return // unknown evidence, or a dependent already holds the recorded network
	}
	_ = atomicfile.WriteFile(partialPreparationPath(),
		[]byte("a first start stopped part-way after saving netconfig; nothing established depends on it yet\n"), 0o644)
}

// clearPartialPreparationJournal removes the journal once a start has
// persisted node.yaml: the node is then established whatever the journal said,
// so a journal left behind is inert. Removal is best-effort for the same
// reason the write is.
func clearPartialPreparationJournal() {
	_ = os.Remove(partialPreparationPath())
}

// savedControllerIP only treats a missing state file as first boot. A state
// with no address is damaged even if it has no etcd membership yet.
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

// keepEtcdMembership lets a pre-existing state (restart, or a join that prepared
// it) win for etcd membership: PrepareController must never reset an HA member
// to state=new, nor forget that the cluster has had peers, which is
// what makes `stratum up` demand the member's WAL.
func keepEtcdMembership(st, prev *NodeState) {
	if prev != nil && prev.EtcdInitialCluster != "" {
		st.EtcdName, st.EtcdInitialCluster, st.EtcdState = prev.EtcdName, prev.EtcdInitialCluster, prev.EtcdState
		st.EtcdHasPeers = prev.EtcdHasPeers
	}
}

// serverURL is the apiserver endpoint every kubeconfig on this node targets,
// from the node-state record's facts: the recorded endpoint wins, else the
// VIP when configured, else the local apiserver. The derivation is the one
// rule kubeconfig.NodeServerURL owns, which the HA join records its endpoint
// by too, so a joined control plane keeps its endpoint across its first
// restart.
func serverURL(st *NodeState) string {
	return kubeconfig.NodeServerURL(st.Server, st.VIP.Address)
}

// recordedServer returns the endpoint the previous state's Server record
// contributes to this start, or "" when this start re-derives by serverURL's
// rule. The record is kept only when it was written under the
// recorded-endpoint contract: the kubeconfig files on disk already target it,
// which is how the writers that record an endpoint write it — the HA join and
// ha-repoint write the kubeconfigs and the record in the same run. A record
// the files do not back is a leftover of the previous release's join (it
// recorded the join URL the clients never targeted); re-deriving drops it
// once, and the derived endpoint rewrites the kubeconfigs and is saved back
// as the record.
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

// recordKubeconfigs are the kubeconfig files whose endpoint the Server record
// governs: the leaf kubeconfigs kubeconfig.GenerateAll writes against the
// recorded endpoint, plus the admin.conf copy — the same files ha-repoint
// rewrites, so a deliberate repoint record is always corroborated by the
// files it wrote. kube-vip's kubeconfig is deliberately absent (GenerateAll
// targets it at the local apiserver by its own rule) and so is the join's
// bootstrap.conf (it targets the grant's discovery endpoint, not the one
// clients serve through).
func recordKubeconfigs() []string {
	files := []string{version.AdminConf}
	for _, f := range []string{"admin.kubeconfig", "controller-manager.kubeconfig", "scheduler.kubeconfig", "kubelet.kubeconfig"} {
		files = append(files, filepath.Join(version.KubeconfigDir, f))
	}
	return files
}

// kubeconfigsTarget reports whether the kubeconfigs on disk already target
// server: every one of recordKubeconfigs that exists must name it
// (kubeconfig.CurrentServer). A file that cannot be read or carries another
// endpoint leaves the record uncorroborated, and with none of the files
// present there is nothing to corroborate it either; either way the caller
// re-derives and the derived endpoint rewrites the files.
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

// nodeIPMoveRefusal refuses to restart an etcd member on a different address.
//
// The saved membership is kept below, and it names this member by its old peer
// URL, while etcd's argv would be rendered from the new node IP: the member
// then advertises and listens on an address its own cluster does not know, and
// cannot reconnect. The apiserver and etcd certificates name
// the old address too. Moving a member is an etcd membership change and has to
// be done as one, so an explicit new address is refused rather than half-applied.
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

// prereqOptions are the host-preparation settings for a controller start. A
// node with any node.yaml, even one that cannot be read, was brought up
// before: only a missing one is a first start.
func prereqOptions(cfg *ClusterConfig, prof profile.Profile) host.PrereqOptions {
	_, err := LoadState()
	return host.PrereqOptions{Profile: prof, SELinuxOptOut: cfg.SELinuxOptOut(), Existing: !errors.Is(err, fs.ErrNotExist)}
}

// workerSELinuxPath is the config file a worker's spec.selinux is read from
// when no --config path is at hand, as in the upgrade preflight. A var so a
// test can redirect it.
var workerSELinuxPath = DefaultConfigPath

// NodeSELinuxOptions reads this node's SELinux settings from disk, for the
// upgrade preflight: whether its profile requires SELinux and whether the
// node opted out of SELinux labelling. Each is resolved the way the node's own
// next start resolves it: a control plane reads the config file its start
// recorded and the profile that config names; a worker keeps the profile its
// join recorded, and the opt-out it saved unless its config file sets
// spec.selinux now. An unreadable node state or config, or a profile this
// binary does not register, is an error: the caller cannot tell what the node
// runs with and warns instead of deciding.
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
	// The saved profile stands in when the config names none: a node brought
	// up with --profile keeps it in its state, not in its config file.
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

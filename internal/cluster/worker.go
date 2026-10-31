package cluster

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/bootstrap"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// The host and bootstrap calls PrepareWorker makes beyond the ones prepare.go
// already puts behind seams, behind seams in the same shape: a test can drive
// the join's own assembly — the node state it saves, the CA files and
// bootstrap kubeconfig it writes — without a host to detect addresses on or
// an apiserver to discover a CA from. A worker detects its addresses through
// hostDetectNodeIPs, as a controller does, not through host.NodeIPs:
// host.ResolveNodeIP, on a join, and host.ResolveNodeIPWithoutConfig, on a
// restart, have already weighed $NODE_IP by the time they fall back to
// detection.
var (
	discoverJoin             = bootstrap.DiscoverJoin
	fetchKubeletClientCA     = bootstrap.FetchKubeletClientCA
	writeBootstrapKubeconfig = bootstrap.WriteBootstrapKubeconfig
	writeTrustFile           = atomicfile.WriteFile
)

// WorkerOptions are the inputs of a worker start, as `stratum up --worker`
// resolves them from its flags and config file. A restart passes no join
// credentials: it resumes from the saved node state.
type WorkerOptions struct {
	// Server is the control-plane or VIP URL to join (--server); empty on a
	// restart.
	Server string
	// Token is the bootstrap token the join authenticates with.
	Token string
	// CAHash is the "sha256:<hex>" pin of the cluster CA
	// (--discovery-token-ca-cert-hash).
	CAHash string
	// NodeIP is --node-ip; empty leaves spec.network.nodeIP, $NODE_IP and
	// detection to the join, and the saved address, else $NODE_IP and
	// detection, to a restart. A restart given one that differs moves the
	// worker to it and saves it, once it is an address the worker can
	// register: an IPv4 address on a single-stack worker, its v4,v6 pair on a
	// dual-stack one.
	NodeIP string
	// ConfigNodeIP is spec.network.nodeIP from the config file, which only a
	// join reads. It is kept apart from NodeIP, which outranks it, so the join
	// names the source its address came from.
	ConfigNodeIP string
	// ServiceCIDR, PodCIDR and ClusterDomain describe a cluster that does not
	// use the defaults. A restart reads only PodCIDR, to record the range a
	// legacy worker, or one joined without --pod-cidr, never saved, or to
	// replace the default an earlier release saved for such a join.
	ServiceCIDR   string
	PodCIDR       string
	ClusterDomain string
	// Profile selects the named node profile on a join.
	Profile string
	// SELinuxOptOut is spec.selinux: false from the config file, nil when a
	// restart's file does not set it; a restart saves a changed setting.
	SELinuxOptOut *bool
	// DualStack registers a v4,v6 node IP pair on a join (--dual-stack).
	DualStack bool
	// Node is --node-label, --node-taint and --runtime-handler. A nil field
	// was not given. A join lays it over ConfigNode; a restart given a field
	// replaces the one the worker saved with it.
	Node NodeSpec
	// ConfigNode is spec.node from the config file, which only a join reads.
	ConfigNode NodeSpec
}

// PrepareWorker runs the worker join flow: discover + pin the CA,
// write the bootstrap kubeconfig, persist the node state. RunWorker, which
// follows, writes the kubelet and containerd configs. The kubelet does TLS
// bootstrap; the operator's CSR approver signs its serving cert. A worker
// that already has worker state and is given no Server takes the restart
// fast path instead, which reuses that state.
//
// ctx is the run's interrupt context, created by the caller before its
// preflight: a signal received before the join began stops it here, before
// any host write, and the caller stops the run at its boundary once the join
// returns.
func PrepareWorker(ctx context.Context, o WorkerOptions) (*NodeState, error) {
	profileName := o.Profile
	// A join entered with the run already stopped refuses before anything on
	// this host is touched, surfacing the signal's own stop.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("worker join: %w before preparing this node (nothing was written)", err)
	}
	// A restarted worker reuses its state — no token needed after first join.
	// A state file that is there but cannot be read is reported as such on a
	// restart: asking for join credentials would send the operator to a new
	// registration when what needs repairing is the file.
	if st, err := LoadState(); err != nil && !errors.Is(err, fs.ErrNotExist) && o.Server == "" {
		return nil, fmt.Errorf("worker restart: read this worker's saved node state: %w; repair the file, which a restart resumes from", err)
	} else if err == nil && st.Role == "worker" && o.Server == "" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A restart must retain the profile recorded by the join.
		p, err := profile.Select(st.ProfileName())
		if err != nil {
			return nil, fmt.Errorf("this worker's saved state names a %w; run it with a binary that registers the profile", err)
		}
		// Every refusal comes before the restart saves a setting or prepares
		// the host, so a refused start leaves node.yaml and the host as they
		// were: it does not save one setting it was given and then refuse
		// another.
		if o.SELinuxOptOut != nil && p.RequiresSELinux && *o.SELinuxOptOut && !st.SELinuxOptOut {
			return nil, fmt.Errorf("the %s profile cannot be combined with spec.selinux: false; remove it from this worker's config file", p.Name)
		}
		// The interface-scoped binding needs no pod CIDR. Legacy workers, and
		// workers joined without --pod-cidr, can start without one; persist an
		// explicit range if supplied. Earlier releases saved the default range
		// for a join given no --pod-cidr, so a saved default is replaced by a
		// range given here rather than refused as a conflict.
		savedDefault := st.PodCIDR == netcfg.Default().PodCIDR
		recordPodCIDR := o.PodCIDR != "" && o.PodCIDR != st.PodCIDR && (st.PodCIDR == "" || savedDefault)
		if recordPodCIDR {
			if err := netcfg.ValidatePodCIDR(o.PodCIDR); err != nil {
				return nil, fmt.Errorf("--pod-cidr: %w", err)
			}
		} else if o.PodCIDR != "" && o.PodCIDR != st.PodCIDR {
			return nil, fmt.Errorf("--pod-cidr %s conflicts with this worker's saved pod CIDR %s", o.PodCIDR, st.PodCIDR)
		}
		if err := o.Node.Validate(); err != nil {
			return nil, fmt.Errorf("worker restart: %w; nothing was saved", err)
		}
		if o.Node.Taints != nil && !sameTaints(o.Node.Taints, st.NodeTaints) {
			return nil, fmt.Errorf("worker restart: --node-taint %s differs from the taints this worker joined with (%s), "+
				"and a node registers its taints only when it joins; change them from a control plane with "+
				"`stratum kubectl taint node %s`; nothing was saved",
				strings.Join(o.Node.Taints, ","), cmp.Or(strings.Join(st.NodeTaints, ","), "none"), st.Hostname)
		}
		labels, handlers, replaced := restartNodeFacts(st, o.Node)
		// A restart keeps the address this worker registered with unless
		// --node-ip moves it, by the rule every controller start uses: the
		// saved address outranks $NODE_IP, which says only what the
		// environment running this start says now. The reboot unit reruns a
		// bare `stratum up --worker`, so a moved address is saved below, or
		// the next start would put the kubelet back on the old one. Older
		// state records only the primary address. Because it is saved, a
		// changed address is checked first: one that is not an IP address, or
		// one that drops or adds an address family this worker registered
		// with, would otherwise start the kubelet on it at this start and at
		// every boot after it. A restart reads no spec.network.nodeIP, so it
		// resolves without one, and the hint it adds to a failed detection
		// names only the sources it does read.
		saved := cmp.Or(st.KubeletNodeIP, st.NodeIP)
		// A worker is dual-stack when it joined with --dual-stack or with a
		// v4,v6 pair: one joined with a pair but without --dual-stack
		// registered both families.
		dualStack := st.DualStack || strings.Contains(saved, ",")
		choice, err := host.ResolveNodeIPWithoutConfig(o.NodeIP, saved, func() (string, error) { return hostDetectNodeIPs(dualStack) })
		if err != nil {
			return nil, err
		}
		// An explicit flag also pins the kubelet address missing from older
		// state, even when it matches the primary address recorded there.
		updateNodeIP := choice.IP != saved || (o.NodeIP != "" && choice.IP != st.KubeletNodeIP)
		// Always check an explicit flag, even when it repeats an invalid
		// address from damaged or older state.
		if updateNodeIP || o.NodeIP != "" {
			checkDualStack := dualStack
			if !updateNodeIP {
				// A join accepts one explicit IPv4 with --dual-stack too.
				// Repeating it keeps the address families it registered.
				checkDualStack = strings.Contains(saved, ",")
			}
			if err := host.CheckNodeIP(choice.IP, checkDualStack); err != nil {
				return nil, fmt.Errorf("worker restart: %s %q cannot be this worker's node IP: %w; it keeps its saved node IP %s",
					choice.Source, choice.IP, err, cmp.Or(saved, "(none)"))
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Every start, as on a control plane (PrepareController): a reboot
		// can bring swap back, and an upgrade may carry host settings the
		// join never applied -- the inotify limits among them.
		if o.SELinuxOptOut != nil && *o.SELinuxOptOut != st.SELinuxOptOut {
			if err := nodestate.Mutate(func(cur *NodeState) error { cur.SELinuxOptOut = *o.SELinuxOptOut; return nil }); err != nil {
				return nil, fmt.Errorf("save spec.selinux: %w", err)
			}
			st.SELinuxOptOut = *o.SELinuxOptOut
		} else if o.SELinuxOptOut == nil && st.SELinuxOptOut {
			// A config file that omits spec.selinux, or cannot be read, leaves
			// nil here, so the saved opt-out stays; only spec.selinux: true
			// clears it.
			fmt.Printf("  NOTE: keeping the spec.selinux: false this worker saved. Set spec.selinux: true in its config file to confine this worker\n")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := hostPrereqs(host.PrereqOptions{Profile: p, SELinuxOptOut: st.SELinuxOptOut, Existing: true}); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(replaced) > 0 {
			if err := nodestate.Mutate(func(cur *NodeState) error {
				cur.NodeLabels, cur.RuntimeHandlers = labels, handlers
				return nil
			}); err != nil {
				return nil, fmt.Errorf("save worker node facts: %w", err)
			}
			st.NodeLabels, st.RuntimeHandlers = labels, handlers
			for _, line := range replaced {
				fmt.Printf("stratum: %s\n", line)
			}
		}
		if recordPodCIDR {
			if err := nodestate.Mutate(func(cur *NodeState) error { cur.PodCIDR = o.PodCIDR; return nil }); err != nil {
				return nil, fmt.Errorf("save worker pod CIDR: %w", err)
			}
			if savedDefault {
				fmt.Printf("stratum: pod CIDR %s replaces the default %s an earlier join saved\n", o.PodCIDR, st.PodCIDR)
			}
			st.PodCIDR = o.PodCIDR
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Every start says which address it runs on and where that came from,
		// as a controller start does.
		fmt.Printf("stratum: node IP %s (%s)\n", choice.IP, choice.Source)
		if updateNodeIP {
			// Merge only the address under the same lock as every state
			// mutation, keeping fields recorded since this start loaded it.
			if err := nodestate.Mutate(func(cur *NodeState) error {
				cur.NodeIP, cur.KubeletNodeIP = host.Primary(choice.IP), choice.IP
				*st = *cur
				return nil
			}); err != nil {
				return nil, fmt.Errorf("save worker node IP: %w", err)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hostTrustPodCIDRs(st.PodCIDR)
		return st, nil
	}
	if o.Server == "" || o.Token == "" || o.CAHash == "" {
		return nil, errors.New("worker join needs --server, --token and --discovery-token-ca-cert-hash")
	}
	// A join selects its profile before any host effects.
	p, err := profile.Select(profileName)
	if err != nil {
		return nil, fmt.Errorf("worker join: %w; every node of a cluster that runs a profile must run a binary that registers it", err)
	}
	if p.RequiresSELinux && o.SELinuxOptOut != nil && *o.SELinuxOptOut {
		return nil, fmt.Errorf("the %s profile cannot be combined with spec.selinux: false; remove it from this worker's config file", p.Name)
	}
	if o.PodCIDR != "" {
		if err := netcfg.ValidatePodCIDR(o.PodCIDR); err != nil {
			return nil, fmt.Errorf("--pod-cidr: %w", err)
		}
	}
	if o.ServiceCIDR != "" {
		// Refuse a range the kubelet could not derive its cluster DNS IP from
		// before anything on the host is touched.
		if _, err := netcfg.DNSIPFor(o.ServiceCIDR); err != nil {
			return nil, fmt.Errorf("--service-cidr: %w", err)
		}
	}
	if o.ClusterDomain != "" {
		if err := netcfg.ValidateDomain(o.ClusterDomain); err != nil {
			return nil, fmt.Errorf("--cluster-domain: %w", err)
		}
	}
	node, err := JoinNodeSpec(o.ConfigNode, o.Node)
	if err != nil {
		return nil, fmt.Errorf("worker join: %w", err)
	}
	hostname, err := hostHostname()
	if err != nil {
		return nil, err
	}
	// A join is a new registration, so an address an earlier join saved on
	// this host is not a source: --node-ip, then spec.network.nodeIP, then
	// $NODE_IP, then detection.
	choice, err := host.ResolveNodeIP(o.NodeIP, o.ConfigNodeIP, "", func() (string, error) { return hostDetectNodeIPs(o.DualStack) })
	if err != nil {
		return nil, err
	}
	// The join saves this address and every later start reuses it, so it is
	// checked before anything on the host or the cluster is touched. The
	// stack is read from the value, as the restart reads it from the saved
	// one: a pair given without --dual-stack registers both families, and one
	// address given with it registers only that one, as before the check.
	if err := host.CheckNodeIP(choice.IP, strings.Contains(choice.IP, ",")); err != nil {
		return nil, fmt.Errorf("worker join: %s %q cannot be this worker's node IP: %w", choice.Source, choice.IP, err)
	}
	fmt.Printf("stratum: node IP %s (%s)\n", choice.IP, choice.Source)
	nodeIP := choice.IP
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := hostDirs(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	optOut := o.SELinuxOptOut != nil && *o.SELinuxOptOut
	if err := hostPrereqs(host.PrereqOptions{Profile: p, SELinuxOptOut: optOut}); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Bind Cilium's pod-facing interfaces on workers as on controllers. The
	// interface-scoped binding does not depend on the pod range, so a join
	// given none still converges.
	hostTrustPodCIDRs(o.PodCIDR)

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ca, split, err := discoverJoin(o.Server, o.Token, o.CAHash)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The kubelet's clientCAFile is the kubelet-client CA a split cluster
	// publishes, or the discovered CA on a cluster that predates the split.
	kubeletCA, found, err := fetchKubeletClientCA(o.Server, ca)
	if err != nil {
		return nil, err
	}
	if found != split {
		return nil, fmt.Errorf("kubelet client-CA discovery disagrees with token-authenticated cluster-info: split=%t, bundle published=%t", split, found)
	}
	if !split {
		kubeletCA = ca
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for f, b := range map[string][]byte{
		filepath.Join(version.PKIDir, pki.RootCAName+".crt"): ca,
		pki.RootTrustBundlePath():                            ca,
		pki.KubeletClientBundlePath():                        kubeletCA,
	} {
		if err := writeTrustFile(f, b, 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", filepath.Base(f), err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := writeBootstrapKubeconfig(o.Server, o.Token, ca); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// A join learns no pod range from the cluster, so the state records only
	// the --pod-cidr the user gave. A default recorded here would read on a
	// restart as a range the user gave, and refuse the cluster's real one.
	st := &NodeState{
		Role: "worker", Hostname: hostname,
		NodeIP: host.Primary(nodeIP), KubeletNodeIP: nodeIP,
		Server: o.Server, Profile: profileName, CIS: profileName == nodestate.CISProfile, SELinuxOptOut: optOut, DualStack: o.DualStack,
		ServiceCIDR: o.ServiceCIDR, PodCIDR: o.PodCIDR,
		ClusterDomain: o.ClusterDomain,
		NodeLabels:    node.Labels, NodeTaints: node.Taints, RuntimeHandlers: node.RuntimeHandlers,
	}
	// containerd and kubelet configs are RunWorker's, on this start as on
	// every other.
	if err := nodestate.Mutate(func(cur *NodeState) error {
		st.ClusterDNSIP = cur.ClusterDNSIP
		*cur = *st
		return nil
	}); err != nil {
		return nil, err
	}
	return st, nil
}

// restartNodeFacts is the saved labels and runtime handlers, each replaced by
// the restart's flag when one was given, and a line for every field that
// changes what the worker saved.
func restartNodeFacts(st *NodeState, flags NodeSpec) (labels, handlers map[string]string, replaced []string) {
	labels, handlers = st.NodeLabels, st.RuntimeHandlers
	if flags.Labels != nil && !maps.Equal(flags.Labels, labels) {
		replaced = append(replaced, fmt.Sprintf("--node-label replaces the labels this worker saved (%s -> %s)", formatKeyValues(labels), formatKeyValues(flags.Labels)))
		labels = flags.Labels
	}
	if flags.RuntimeHandlers != nil && !maps.Equal(flags.RuntimeHandlers, handlers) {
		replaced = append(replaced, fmt.Sprintf("--runtime-handler replaces the runtime handlers this worker saved (%s -> %s)", formatKeyValues(handlers), formatKeyValues(flags.RuntimeHandlers)))
		handlers = flags.RuntimeHandlers
	}
	return labels, handlers, replaced
}

// formatKeyValues renders a map as sorted name=value pairs, "none" when empty.
func formatKeyValues(m map[string]string) string {
	if len(m) == 0 {
		return "none"
	}
	out := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, k+"="+m[k])
	}
	return strings.Join(out, ",")
}

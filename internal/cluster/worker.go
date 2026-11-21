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

// Seams let tests drive the join's own assembly without a host or an apiserver.
var (
	discoverJoin             = bootstrap.DiscoverJoin
	fetchKubeletClientCA     = bootstrap.FetchKubeletClientCA
	writeBootstrapKubeconfig = bootstrap.WriteBootstrapKubeconfig
	writeTrustFile           = atomicfile.WriteFile
)

// One worker start's inputs; a restart passes no join credentials and resumes saved state.
type WorkerOptions struct {
	// Server is the control-plane or VIP URL to join; empty on a restart.
	Server string
	// Token is the bootstrap token the join authenticates with.
	Token string
	// CAHash is the "sha256:<hex>" pin of the cluster CA.
	CAHash string
	// NodeIP is --node-ip; a restart given a registrable one moves the worker to it.
	NodeIP string
	// ConfigNodeIP is spec.network.nodeIP, which only a join reads.
	ConfigNodeIP string
	// A restart reads only PodCIDR, to record or replace a range older state never saved.
	ServiceCIDR   string
	PodCIDR       string
	ClusterDomain string
	// Profile selects the node profile on a join; a restart given another is refused.
	Profile string
	// SELinuxOptOut is spec.selinux: false from the config file; nil when unset there.
	SELinuxOptOut *bool
	// DualStack registers a v4,v6 node IP pair on a join (--dual-stack).
	DualStack bool
	// Node is --node-label, --node-taint and --runtime-handler; nil fields were not given.
	Node NodeSpec
	// ConfigNode is spec.node from the config file, which only a join reads.
	ConfigNode NodeSpec
}

// ResolveJoinNodeIP checks a join's node IP before anything is written, so a refusal writes none.
func ResolveJoinNodeIP(nodeIP, configNodeIP string, dual bool) (host.NodeIPChoice, error) {
	choice, err := host.ResolveNodeIP(nodeIP, configNodeIP, "", func() (string, error) { return hostDetectNodeIPs(dual) })
	if err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := host.CheckNodeIP(choice.IP, strings.Contains(choice.IP, ",")); err != nil {
		return host.NodeIPChoice{}, fmt.Errorf("worker join: %s %q cannot be this worker's node IP: %w", choice.Source, choice.IP, err)
	}
	return choice, nil
}

// PrepareWorker runs the worker join, or the restart fast path when saved worker state is present.
func PrepareWorker(ctx context.Context, o WorkerOptions) (*NodeState, error) {
	profileName := o.Profile
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("worker join: %w before preparing this node (nothing was written)", err)
	}
	if st, err := LoadState(); err != nil && !errors.Is(err, fs.ErrNotExist) && o.Server == "" {
		return nil, fmt.Errorf("worker restart: read this worker's saved node state: %w; repair the file, which a restart resumes from", err)
	} else if err == nil && st.Role == "worker" && o.Server == "" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := profile.Select(st.ProfileName())
		if err != nil {
			return nil, fmt.Errorf("this worker's saved state names a %w; run it with a binary that registers the profile", err)
		}
		if err := RefuseWorkerProfileChange(st, profileName); err != nil {
			return nil, err
		}
		if o.SELinuxOptOut != nil && p.RequiresSELinux && *o.SELinuxOptOut && !st.SELinuxOptOut {
			return nil, fmt.Errorf("the %s profile cannot be combined with spec.selinux: false; remove it from this worker's config file", p.Name)
		}
		// A saved default came from an older release's join, so it is replaced rather than refused.
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
		// The reboot unit reruns a bare `stratum up --worker`, so a moved address must be saved.
		saved := cmp.Or(st.KubeletNodeIP, st.NodeIP)
		// A pair joined without --dual-stack registered both families.
		dualStack := st.DualStack || strings.Contains(saved, ",")
		choice, err := host.ResolveNodeIPWithoutConfig(o.NodeIP, saved, func() (string, error) { return hostDetectNodeIPs(dualStack) })
		if err != nil {
			return nil, err
		}
		// An explicit flag also pins the kubelet address missing from older state.
		updateNodeIP := choice.IP != saved || (o.NodeIP != "" && choice.IP != st.KubeletNodeIP)
		// Check an explicit flag even when it repeats an invalid address from damaged state.
		if updateNodeIP || o.NodeIP != "" {
			checkDualStack := dualStack
			if !updateNodeIP {
				// Repeating a join's single IPv4 keeps the families it registered.
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
		// A reboot can bring swap back; an upgrade may carry settings the join never applied.
		if o.SELinuxOptOut != nil && *o.SELinuxOptOut != st.SELinuxOptOut {
			if err := nodestate.Mutate(func(cur *NodeState) error { cur.SELinuxOptOut = *o.SELinuxOptOut; return nil }); err != nil {
				return nil, fmt.Errorf("save spec.selinux: %w", err)
			}
			st.SELinuxOptOut = *o.SELinuxOptOut
		} else if o.SELinuxOptOut == nil && st.SELinuxOptOut {
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
				if o.Node.Labels != nil {
					cur.NodeLabels = labels
				}
				if o.Node.RuntimeHandlers != nil {
					cur.RuntimeHandlers = handlers
				}
				st.NodeLabels, st.RuntimeHandlers = cur.NodeLabels, cur.RuntimeHandlers
				return nil
			}); err != nil {
				return nil, fmt.Errorf("save worker node facts: %w", err)
			}
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
		fmt.Printf("stratum: node IP %s (%s)\n", choice.IP, choice.Source)
		if updateNodeIP {
			// Save under the data lock so fields another writer changed survive.
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
		// The kubelet derives its cluster DNS IP from this range.
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
	choice, err := ResolveJoinNodeIP(o.NodeIP, o.ConfigNodeIP, o.DualStack)
	if err != nil {
		return nil, err
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
	// The interface-scoped binding does not depend on the pod range, so an empty one converges.
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
	// A split cluster publishes a separate kubelet-client CA; a legacy one does not.
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

	// A recorded default would read on a restart as a range the user gave and refuse the real one.
	st := &NodeState{
		Role: "worker", Hostname: hostname,
		NodeIP: host.Primary(nodeIP), KubeletNodeIP: nodeIP,
		Server: o.Server, Profile: profileName, CIS: profileName == nodestate.CISProfile, SELinuxOptOut: optOut, DualStack: o.DualStack,
		ServiceCIDR: o.ServiceCIDR, PodCIDR: o.PodCIDR,
		ClusterDomain: o.ClusterDomain,
		NodeLabels:    node.Labels, NodeTaints: node.Taints, RuntimeHandlers: node.RuntimeHandlers,
		NodeTaintsCheckPending: len(node.Taints) > 0,
	}
	if err := nodestate.Mutate(func(cur *NodeState) error {
		st.ClusterDNSIP = cur.ClusterDNSIP
		*cur = *st
		return nil
	}); err != nil {
		return nil, err
	}
	return st, nil
}

func restartNodeFacts(st *NodeState, flags NodeSpec) (labels, handlers map[string]string, replaced []string) {
	labels, handlers = st.NodeLabels, st.RuntimeHandlers
	if flags.Labels != nil && !maps.Equal(flags.Labels, labels) {
		replaced = append(replaced, fmt.Sprintf("--node-label replaces the labels this worker saved (%s -> %s)", FormatKeyValues(labels), FormatKeyValues(flags.Labels)))
		labels = flags.Labels
	}
	if flags.RuntimeHandlers != nil && !maps.Equal(flags.RuntimeHandlers, handlers) {
		replaced = append(replaced, fmt.Sprintf("--runtime-handler replaces the runtime handlers this worker saved (%s -> %s)", FormatKeyValues(handlers), FormatKeyValues(flags.RuntimeHandlers)))
		handlers = flags.RuntimeHandlers
	}
	return labels, handlers, replaced
}

// FormatKeyValues renders a map as sorted name=value pairs, "none" when empty.
func FormatKeyValues(m map[string]string) string {
	if len(m) == 0 {
		return "none"
	}
	out := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, k+"="+m[k])
	}
	return strings.Join(out, ",")
}

func RefuseWorkerProfileChange(st *NodeState, want string) error {
	if want == "" || want == st.ProfileName() {
		return nil
	}
	return fmt.Errorf("worker restart: this worker joined with %s, and a restart cannot change it to profile %q: "+
		"a worker takes its profile from its join. To move it, remove spec.profile and spec.cis from this worker's "+
		"config file, which a worker join refuses, then run `stratum token create` on a control plane and run the "+
		"worker join line it prints on this node with its --profile set to %s, adding one if the line has none. "+
		"The join replaces this worker's saved state with what that line and the config file give, so also add "+
		"the --node-ip, --dual-stack, --node-label, --node-taint and --runtime-handler settings that %s records "+
		"and the config file does not set. Then restart the worker as before. Nothing was changed",
		describeProfile(st.ProfileName()), want, want, nodestate.StatePath())
}

func describeProfile(name string) string {
	if name == "" {
		return "no profile"
	}
	return fmt.Sprintf("profile %q", name)
}

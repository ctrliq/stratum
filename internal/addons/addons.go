// Package addons carries the cluster add-on manifests (CoreDNS, local-path
// storage, Cilium CNI) embedded in the stratum binary, and renders each one for
// the cluster it is about to be installed into — substituting the mirrored image
// registry, the pinned versions, and the per-cluster apiserver endpoint.
//
// Rendering is all this package does. The controller writes the rendered
// bundles into /var/lib/stratum/manifests (internal/cluster.writeBundles) and
// the operator's deployer server-side-applies and prunes them, so there is one
// apply path rather than two.
package addons

import (
	"bytes"
	_ "embed"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/ctrliq/stratum/internal/version"
)

//go:embed manifests/coredns.yaml
var corednsYAML []byte

//go:embed manifests/local-path.yaml
var localPathYAML []byte

// Cilium rendered to a static manifest at the image-list-pinned version (the
// Makefile's CILIUM_VERSION, from build/images.txt) by
// tools/gen-cilium-manifest.sh (helm template, committed — install needs no
// network or cilium CLI). Only the kube-proxy-replacement rendering exists:
// Stratum ships exactly one service dataplane (Cilium's kube-proxy
// replacement; kube-proxy is not built), so there is no kpr=false variant to
// drift. The two apiserver sentinels are filled in at
// apply time by FillAPIServer (they can't be baked: they're per-cluster).
//
//go:embed manifests/cilium-kpr-true.yaml
var ciliumKPRTrueYAML []byte

const (
	apiserverHostSentinel = "__STRATUM_APISERVER_HOST__"
	apiserverURLsSentinel = "__STRATUM_APISERVER_URLS__"
)

// NeedsAPIServer reports whether a manifest still carries an apiserver sentinel
// that FillAPIServer must resolve before it is applied.
func NeedsAPIServer(m []byte) bool {
	return bytes.Contains(m, []byte(apiserverHostSentinel)) || bytes.Contains(m, []byte(apiserverURLsSentinel))
}

// APIServer is how Cilium reaches the apiserver before Kubernetes networking
// exists: kube-proxy-free agents cannot use the in-cluster Service until they
// are themselves up.
type APIServer struct {
	// Host is KUBERNETES_SERVICE_HOST in every Cilium pod.
	Host string
	// URLs is k8s-api-server-urls: every control plane, which the agents rotate
	// through until they reach the kubernetes Service. Empty means Host alone.
	URLs []string
}

// APIServerFor derives the Cilium apiserver settings from facts every control
// plane sees identically, so the manifest is the same whichever of them the
// operator leader runs on. A render that bakes in the rendering
// node's own IP would differ per control plane: a leader change
// re-points every agent at the new leader and rolls the DaemonSet during the
// failover it was reacting to, and an agent pinned to one control plane loses
// the API whenever that one does.
//
//   - With a VIP: the VIP alone. kube-vip already moves it.
//   - Without one: every control plane's address, sorted, and the first of them
//     as Host. Losing a node does not remove its Node object, so failover
//     changes neither value; a join or a removal does, and rolls the agents
//     once, as a planned change.
//
// selfIP is the fallback while no Node carries the control-plane label yet (the
// first apply on a fresh cluster). An empty result means there is nothing to
// render with, and the caller must not apply.
func APIServerFor(vip, selfIP string, controlPlaneIPs []string) APIServer {
	if vip != "" {
		return APIServer{Host: vip}
	}
	ips := slices.Clone(controlPlaneIPs)
	if len(ips) == 0 && selfIP != "" {
		ips = []string{selfIP}
	}
	if len(ips) == 0 {
		return APIServer{}
	}
	slices.SortFunc(ips, compareIPs)
	ips = slices.Compact(ips)
	a := APIServer{Host: ips[0]}
	if len(ips) > 1 {
		for _, ip := range ips {
			a.URLs = append(a.URLs, "https://"+net.JoinHostPort(ip, strconv.Itoa(version.APIServerPort)))
		}
	}
	return a
}

// compareIPs orders addresses numerically (10.0.0.9 before 10.0.0.10), falling
// back to string order for anything that does not parse.
func compareIPs(a, b string) int {
	pa, pb := net.ParseIP(a), net.ParseIP(b)
	if pa == nil || pb == nil {
		return strings.Compare(a, b)
	}
	return bytes.Compare(pa.To16(), pb.To16())
}

// FillAPIServer resolves the apiserver sentinels. The URL list is space
// separated, which is the form Cilium's chart renders for k8s.apiServerURLs in
// both places it appears.
func FillAPIServer(m []byte, a APIServer) []byte {
	m = bytes.ReplaceAll(m, []byte(apiserverHostSentinel), []byte(a.Host))
	return bytes.ReplaceAll(m, []byte(apiserverURLsSentinel), []byte(strings.Join(a.URLs, " ")))
}

// Every image reference in the bundled manifests is written as
// __STRATUM_REGISTRY__/<name>:<tag>[@<digest>] and resolved here. The mirror
// namespace is a build-time decision (build/versions.mk image_registry), and
// this sentinel is what keeps it from being written down in a dozen YAML files
// — which is also why a manifest cannot accidentally name registry.k8s.io or
// quay.io: there is nowhere in the rendering path for such a name to come from.
// TestRenderedManifestsUseOnlyTheMirror asserts exactly that.
const registrySentinel = "__STRATUM_REGISTRY__"

// busyboxImageSentinel is local-path's helper image and CoreDNS's init
// container. It resolves to the same
// pinned, mirrored busybox the node Jobs use, so the tag cannot drift away from
// what the airgap payload actually carries when the pin moves.
const busyboxImageSentinel = "__STRATUM_BUSYBOX_IMAGE__"

// The four per-cluster inputs the staged bundles carry as named sentinels
// instead of the staging node's local values. The staging path leaves them
// unfilled, so every control plane stages the same bytes; the deployer fills
// each at apply time from the cluster-inputs record the staging writes beside
// the prune inventory (operator.manifestSentinels is the consuming table). They
// resolve to what their name says:
//
//   - DNSAddressSentinel: the kube-dns Service's clusterIP, the tenth address
//     of the cluster's service CIDR (netcfg.DNSIPFor).
//   - ClusterDomainSentinel: the cluster domain the Corefile serves.
//   - EnableIPv6Sentinel: Cilium's dual-stack toggle, true or false.
//   - StorageDefaultSentinel: whether local-path claims the default
//     StorageClass, true when spec.storage.default is local-path.
const (
	DNSAddressSentinel     = "__STRATUM_DNS_IP__"
	ClusterDomainSentinel  = "__STRATUM_CLUSTER_DOMAIN__"
	EnableIPv6Sentinel     = "__STRATUM_ENABLE_IPV6__"
	StorageDefaultSentinel = "__STRATUM_STORAGE_DEFAULT__"
)

// Storage backends selectable in the cluster config (spec.storage.default).
// Only local-path and none are selectable here.
const (
	StorageLocalPath = "local-path"
	StorageNone      = "none"
)

// RenderCilium substitutes the per-node-independent values into the embedded
// manifest: the registry, the operator replica count, and the cluster's
// spec.network.cilium settings. The apiserver sentinels are left for
// FillAPIServer, because the right values depend on the cluster, not on the
// node rendering the file — and so is the dual-stack toggle: what enable-ipv6
// should say is the cluster's, not the staging node's, so the staging path
// calls RenderCiliumStaged, which leaves it as EnableIPv6Sentinel for the
// deployer to fill at apply time. The dualStack argument only decides what a
// direct RenderCilium caller's bytes say.
func RenderCilium(dualStack bool, s CiliumSettings) []byte {
	m := bytes.ReplaceAll(ciliumKPRTrueYAML, []byte(registrySentinel), []byte(ImageRegistry))
	m = pinCiliumOperatorToOneReplica(m)
	if dualStack {
		// Pod IPv6 rides the existing vxlan tunnel over the IPv4 underlay.
		m = bytes.ReplaceAll(m, []byte(`enable-ipv6: "false"`), []byte(`enable-ipv6: "true"`))
	}
	return appendCiliumBGPSecretsRBAC(applyCiliumSettings(m, s), s.BGPControlPlane)
}

// RenderCiliumStaged renders the Cilium bundle the staging path stages:
// RenderCilium's render with the dual-stack toggle left as the
// EnableIPv6Sentinel instead of the staging node's own DualStack answer.
// Every control plane therefore stages the same bytes, and the deployer fills
// the toggle at apply time from the cluster-inputs record, the way it fills
// the apiserver sentinels from cluster state.
func RenderCiliumStaged(s CiliumSettings) []byte {
	m := RenderCilium(false, s)
	return bytes.ReplaceAll(m, []byte(`enable-ipv6: "false"`), []byte(`enable-ipv6: "`+EnableIPv6Sentinel+`"`))
}

// pinCiliumOperatorToOneReplica drops the cilium-operator Deployment to a single
// replica.
//
// The chart default is 2, and the pod spec it renders admits at most one of them
// per node twice over: the pod is hostNetwork with fixed hostPorts (9234, 9963),
// and it carries a *required* podAntiAffinity on kubernetes.io/hostname. So on a
// single node — the topology stratum bootstraps with, and a supported end state
// — the second replica is Pending forever. Relaxing the anti-affinity alone
// would not help: the hostPorts still fit only one operator per node.
//
// One replica is the right count rather than a concession. The operator is
// leader-elected, so only ever one of the two does anything; the standby buys a
// faster failover, not availability, and losing it costs a reschedule on node
// failure rather than correctness — the agents' dataplane keeps forwarding
// without an operator, and IPAM/CiliumNode work resumes when it comes back.
//
// maxUnavailable goes with it. At 2 replicas the chart renders 50%, so one pod
// can be torn down before its replacement is scheduled. At 1 replica 50% rounds
// down to zero, which would make the rollout create the new pod first — onto the
// same node, where it cannot bind the hostPorts the running pod holds, and the
// upgrade wedges. The upstream comment two lines above the strategy block says
// the same thing and names 100% as the answer for a single replica.
//
// Both edits are made here rather than in tools/gen-cilium-manifest.sh so the
// committed asset stays byte-identical to `helm template` at the pin: the
// DO-NOT-EDIT banner holds and a version bump rebases without conflict. Each
// anchor below is unique in the manifest today (one replicas: key, one
// percentage maxUnavailable — the DaemonSets use counts). If a regeneration
// moves them, the substitution no-ops and TestCiliumOperatorFitsASingleNode
// fails rather than passing silently.
func pinCiliumOperatorToOneReplica(m []byte) []byte {
	m = bytes.ReplaceAll(m, []byte("\n  replicas: 2\n"), []byte("\n  replicas: 1\n"))
	return bytes.ReplaceAll(m, []byte("maxUnavailable: 50%"), []byte("maxUnavailable: 100%"))
}

// Stamped from build/versions.mk at link time (see the repo Makefile) so the
// deployed images always match the pin manifest and the airgap payload.
var (
	CorednsVersion   = "unknown"
	LocalPathVersion = "unknown"

	// ImageRegistry is the namespace every bundled manifest pulls from. A
	// running cluster reaches no upstream registry: the images are mirrored
	// here by digest (tools/mirror-images.sh) and carried in the airgap payload
	// under these exact names, so the manifest and the payload agree by
	// construction. Unstamped builds get a name that resolves nowhere, on
	// purpose — same reason as CorednsVersion.
	ImageRegistry = "unknown"
)

// RenderCoreDNS renders the CoreDNS stack the staging path stages: the
// registry, the pinned busybox helper image and the CoreDNS version resolve
// here, while the two per-cluster inputs stay as the DNSAddressSentinel (the
// kube-dns Service's clusterIP) and the ClusterDomainSentinel (the Corefile's
// zone) — the deployer fills both at apply time from the cluster-inputs
// record, so every control plane stages the same bytes whatever its own
// service CIDR and domain are. Both inputs live in cluster state, so the
// render takes no dnsIP or domain argument: an argument that the bytes never
// read states an intent the manifest does not carry.
func RenderCoreDNS() []byte {
	m := bytes.ReplaceAll(corednsYAML, []byte("__STRATUM_COREDNS_VERSION__"), []byte(CorednsVersion))
	m = bytes.ReplaceAll(m, []byte(busyboxImageSentinel), []byte(version.NsenterImage))
	return bytes.ReplaceAll(m, []byte(registrySentinel), []byte(ImageRegistry))
}

// RenderLocalPath renders the local-path stack the staging path stages: the
// registry, the pinned busybox helper image and the provisioner's version
// resolve here, while whether the StorageClass claims the cluster default
// stays as the StorageDefaultSentinel — the cluster's storage decision, which
// the deployer fills at apply time from the cluster-inputs record.
func RenderLocalPath() []byte {
	m := bytes.ReplaceAll(localPathYAML, []byte("__STRATUM_LOCALPATH_VERSION__"), []byte(LocalPathVersion))
	m = bytes.ReplaceAll(m, []byte(busyboxImageSentinel), []byte(version.NsenterImage))
	m = bytes.ReplaceAll(m,
		[]byte(`storageclass.kubernetes.io/is-default-class: "true"`),
		[]byte(`storageclass.kubernetes.io/is-default-class: "`+StorageDefaultSentinel+`"`))
	return bytes.ReplaceAll(m, []byte(registrySentinel), []byte(ImageRegistry))
}
func RenderHelmCRDs() []byte { return helmChartCRDsYAML }

// Package addons carries the cluster add-on manifests embedded in the stratum binary.
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

// Rendered from the Cilium chart by tools/gen-cilium-manifest.sh at the pinned version.
//
//go:embed manifests/cilium-kpr-true.yaml
var ciliumKPRTrueYAML []byte

const (
	apiserverHostSentinel = "__STRATUM_APISERVER_HOST__"
	apiserverURLsSentinel = "__STRATUM_APISERVER_URLS__"
)

func NeedsAPIServer(m []byte) bool {
	return bytes.Contains(m, []byte(apiserverHostSentinel)) || bytes.Contains(m, []byte(apiserverURLsSentinel))
}

// APIServer is how Cilium reaches the apiserver before cluster networking exists.
type APIServer struct {
	// Host is KUBERNETES_SERVICE_HOST in every Cilium pod.
	Host string
	// URLs is k8s-api-server-urls; empty means Host alone.
	URLs []string
}

// APIServerFor derives settings identical on every control plane; empty result: nothing to render.
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

func compareIPs(a, b string) int {
	pa, pb := net.ParseIP(a), net.ParseIP(b)
	if pa == nil || pb == nil {
		return strings.Compare(a, b)
	}
	return bytes.Compare(pa.To16(), pb.To16())
}

// FillAPIServer resolves the sentinels; URLs are space separated, Cilium's chart form.
func FillAPIServer(m []byte, a APIServer) []byte {
	m = bytes.ReplaceAll(m, []byte(apiserverHostSentinel), []byte(a.Host))
	return bytes.ReplaceAll(m, []byte(apiserverURLsSentinel), []byte(strings.Join(a.URLs, " ")))
}

const registrySentinel = "__STRATUM_REGISTRY__"

// busyboxImageSentinel resolves to the same pinned, mirrored busybox the node Jobs carry.
const busyboxImageSentinel = "__STRATUM_BUSYBOX_IMAGE__"

const (
	DNSAddressSentinel     = "__STRATUM_DNS_IP__"
	ClusterDomainSentinel  = "__STRATUM_CLUSTER_DOMAIN__"
	EnableIPv6Sentinel     = "__STRATUM_ENABLE_IPV6__"
	StorageDefaultSentinel = "__STRATUM_STORAGE_DEFAULT__"
)

// Storage backends selectable in spec.storage.default.
const (
	StorageLocalPath = "local-path"
	StorageNone      = "none"
)

// RenderCilium fills node-independent values; per-cluster inputs stay as sentinels.
func RenderCilium(dualStack bool, s CiliumSettings) []byte {
	m := bytes.ReplaceAll(ciliumKPRTrueYAML, []byte(registrySentinel), []byte(ImageRegistry))
	m = pinCiliumOperatorToOneReplica(m)
	if dualStack {
		// Pod IPv6 rides the existing vxlan tunnel over the IPv4 underlay.
		m = bytes.ReplaceAll(m, []byte(`enable-ipv6: "false"`), []byte(`enable-ipv6: "true"`))
	}
	return appendCiliumBGPSecretsRBAC(applyCiliumSettings(m, s), s.BGPControlPlane)
}

// RenderCiliumStaged leaves the dual-stack toggle as the EnableIPv6Sentinel.
func RenderCiliumStaged(s CiliumSettings) []byte {
	m := RenderCilium(false, s)
	return bytes.ReplaceAll(m, []byte(`enable-ipv6: "false"`), []byte(`enable-ipv6: "`+EnableIPv6Sentinel+`"`))
}

// The operator pod's hostPorts and anti-affinity admit one pod per node.
func pinCiliumOperatorToOneReplica(m []byte) []byte {
	m = bytes.ReplaceAll(m, []byte("\n  replicas: 2\n"), []byte("\n  replicas: 1\n"))
	return bytes.ReplaceAll(m, []byte("maxUnavailable: 50%"), []byte("maxUnavailable: 100%"))
}

// Stamped at link time by the Makefile so the deployed images match the pin.
var (
	CorednsVersion   = "unknown"
	LocalPathVersion = "unknown"

	// ImageRegistry is the namespace every bundled manifest pulls from; no upstream registry.
	ImageRegistry = "unknown"
)

func RenderCoreDNS() []byte {
	m := bytes.ReplaceAll(corednsYAML, []byte("__STRATUM_COREDNS_VERSION__"), []byte(CorednsVersion))
	m = bytes.ReplaceAll(m, []byte(busyboxImageSentinel), []byte(version.NsenterImage))
	return bytes.ReplaceAll(m, []byte(registrySentinel), []byte(ImageRegistry))
}

func RenderLocalPath() []byte {
	m := bytes.ReplaceAll(localPathYAML, []byte("__STRATUM_LOCALPATH_VERSION__"), []byte(LocalPathVersion))
	m = bytes.ReplaceAll(m, []byte(busyboxImageSentinel), []byte(version.NsenterImage))
	m = bytes.ReplaceAll(m,
		[]byte(`storageclass.kubernetes.io/is-default-class: "true"`),
		[]byte(`storageclass.kubernetes.io/is-default-class: "`+StorageDefaultSentinel+`"`))
	return bytes.ReplaceAll(m, []byte(registrySentinel), []byte(ImageRegistry))
}
func RenderHelmCRDs() []byte { return helmChartCRDsYAML }

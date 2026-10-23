package uninstall

import (
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/selinux"
)

// The layout paths the preview names as fixed items. They are the same paths
// a production run wires into its hostEffects value (netnsDir, ciliumRunDir,
// cgroupRoot, legacyKubeletDir in Run), declared once here so the preview and
// the teardown cannot name different paths. Constants, not variables: a test
// redirects a run's layout through its own hostEffects value, never through
// these.
const (
	netnsDir         = "/run/netns"
	ciliumRunDir     = "/run/cilium"
	cgroupRoot       = "/sys/fs/cgroup"
	legacyKubeletDir = "/var/lib/kubelet"
)

// The one explanation per conditional effect, shared by both surfaces: the
// CLI dry run renders it as the line's parenthetical, the MCP tool returns it
// as the item's detail.
const (
	legacyContainerdConfigDetail = "containerd config a previous stratum wrote over the host's; removed so the host's containerd and Docker can start"
	firewalldSourceDetail        = "firewalld trusted-zone source stratum added so pods can reach this node"
	firewalldInterfaceDetail     = "firewalld interface binding stratum added"
	selinuxContextsDetail        = "the SELinux file contexts stratum registered (listed by `semanage fcontext -l -C`)"
)

// PreviewItem is one effect an uninstall would have beyond deleting the
// targets: what it is (Kind), what path, CIDR or interface it touches (Path),
// the one explanation both surfaces render (Detail) and, when the probe that
// should have answered could not, that error (Unknown) — an effect this
// preview cannot vouch for is one the run could leave in place, so it is
// reported, never dropped.
type PreviewItem struct {
	Kind    string `json:"kind" jsonschema:"what the effect is: netns, cilium, cgroups, legacy-kubelet, local-path-dir, legacy-containerd-config, firewalld-source, firewalld-interface or selinux-contexts"`
	Path    string `json:"path,omitempty" jsonschema:"the path, CIDR or interface the effect touches; empty for an effect with none, such as the registered SELinux contexts"`
	Detail  string `json:"detail" jsonschema:"what the effect is and why an uninstall touches it, rendered as-is by both the CLI dry run and this tool"`
	Unknown string `json:"unknown,omitempty" jsonschema:"the probe error, when the probe that should have answered could not; the effect would be left in place"`
}

// PreviewReport is what an uninstall would do: Targets, every path the delete
// loop visits, and Items, the effects the delete loop alone does not cover.
type PreviewReport struct {
	Targets []string      `json:"targets" jsonschema:"the paths uninstall deletes, exactly Targets() returns"`
	Items   []PreviewItem `json:"items" jsonschema:"the conditional effects, fixed layout items first"`
}

// Probes is the set of read-only host probes the shared preview consults: the
// legacy containerd config, the firewalld bindings and the registered SELinux
// contexts. The MCP handler builds no hostEffects value, so these are a
// package variable rather than per-run effects — read-only probes, safe to
// share, and the seam this package's and internal/mcp's tests stub.
type Probes struct {
	// LegacyHostConfig reports the host containerd config path a previous
	// stratum wrote over, and whether such a file is present.
	LegacyHostConfig func() (string, bool)
	// TrustedPodCIDRs reports the firewalld trusted-zone sources stratum
	// added, or the error that leaves them unknown.
	TrustedPodCIDRs func() ([]string, error)
	// OwnedFirewalldInterfaces reports the interface bindings stratum added.
	OwnedFirewalldInterfaces func() ([]string, error)
	// SELinuxRegistered reports whether stratum registered SELinux file
	// contexts on this node.
	SELinuxRegistered func() bool
}

// PreviewProbes is the preview's one host seam, with the production probes
// wired as its default.
var PreviewProbes = Probes{
	LegacyHostConfig:         runtime.LegacyHostConfig,
	TrustedPodCIDRs:          host.TrustedPodCIDRs,
	OwnedFirewalldInterfaces: host.OwnedFirewalldInterfaces,
	SELinuxRegistered:        selinux.Registered,
}

// Preview returns what an uninstall would do as data: Targets() plus the
// conditional effects, so the CLI dry run's prose and the MCP
// uninstall_preview's structured output are rendered from one value and
// cannot enumerate different effects. It prints nothing and reaches the host
// only through PreviewProbes, all read-only.
func Preview() PreviewReport {
	report := PreviewReport{Targets: Targets()}
	add := func(kind, path, detail, unknown string) {
		report.Items = append(report.Items, PreviewItem{Kind: kind, Path: path, Detail: detail, Unknown: unknown})
	}

	// The five fixed layout items: the datapath teardown always runs, so the
	// preview always says so.
	add("netns", netnsDir, "the CNI network namespace mounts under it, detached by the datapath teardown", "")
	add("cilium", ciliumRunDir, "Cilium's run directory, with its links, iptables chains and BPF pins", "")
	add("cgroups", cgroupRoot, "the kubepods cgroups under it", "")
	add("legacy-kubelet", legacyKubeletDir, "the legacy kubelet directory, stripped of Stratum's kubelet's stale sockets and its CA-signed certificate pairs, and removed only once nothing else is left in it; another kubelet's state there is kept", "")
	add("local-path-dir", selinux.LocalPathDir, localPathPreviewDetail, "")

	// The host-dependent conditionals, exactly as the probes answered.
	if PreviewProbes.LegacyHostConfig == nil {
		add("legacy-containerd-config", "", legacyContainerdConfigDetail, "the probe is not wired")
	} else if p, present := PreviewProbes.LegacyHostConfig(); present {
		add("legacy-containerd-config", p, legacyContainerdConfigDetail, "")
	}
	if PreviewProbes.TrustedPodCIDRs == nil {
		add("firewalld-source", "", firewalldSourceDetail, "the probe is not wired")
	} else if cidrs, err := PreviewProbes.TrustedPodCIDRs(); err != nil {
		add("firewalld-source", "", firewalldSourceDetail, err.Error())
	} else {
		for _, cidr := range cidrs {
			add("firewalld-source", cidr, firewalldSourceDetail, "")
		}
	}
	if PreviewProbes.OwnedFirewalldInterfaces == nil {
		add("firewalld-interface", "", firewalldInterfaceDetail, "the probe is not wired")
	} else if ifaces, err := PreviewProbes.OwnedFirewalldInterfaces(); err != nil {
		add("firewalld-interface", "", firewalldInterfaceDetail, err.Error())
	} else {
		for _, iface := range ifaces {
			add("firewalld-interface", iface, firewalldInterfaceDetail, "")
		}
	}
	switch {
	case PreviewProbes.SELinuxRegistered == nil:
		add("selinux-contexts", "", selinuxContextsDetail, "the probe is not wired")
	case PreviewProbes.SELinuxRegistered():
		add("selinux-contexts", "", selinuxContextsDetail, "")
	}
	return report
}

// renderPreviewItem renders one item as a dry-run line: the target-style
// "- path (detail)" when the item names a path, the bare explanation when it
// has none, and the unknown form when the probe could not answer.
func renderPreviewItem(it PreviewItem) string {
	switch {
	case it.Unknown != "":
		return "  - " + it.Detail + ": UNKNOWN (" + it.Unknown + "); they would be left in place"
	case it.Path != "":
		return "  - " + it.Path + "  (" + it.Detail + ")"
	default:
		return "  - " + it.Detail
	}
}

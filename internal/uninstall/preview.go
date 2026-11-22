package uninstall

import (
	"github.com/ctrliq/stratum/internal/apparmor"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/selinux"
)

// declared once so the preview and the teardown cannot name different paths.
const (
	netnsDir         = "/run/netns"
	ciliumRunDir     = "/run/cilium"
	cgroupRoot       = "/sys/fs/cgroup"
	legacyKubeletDir = "/var/lib/kubelet"
)

// one explanation per conditional effect, shared by the CLI dry run and the MCP tool.
const (
	legacyContainerdConfigDetail = "containerd config a previous stratum wrote over the host's; removed so the host's containerd and Docker can start"
	firewalldSourceDetail        = "firewalld trusted-zone source stratum added so pods can reach this node"
	firewalldInterfaceDetail     = "firewalld interface binding stratum added"
	selinuxContextsDetail        = "the SELinux file contexts stratum registered (listed by `semanage fcontext -l -C`)"
	apparmorRuncDetail           = "the kernel's runc AppArmor profile, which stratum extended to its own runc: reloaded from the host's own profile, or removed when the host loads none"
)

// an effect the preview cannot vouch for is reported, never dropped.
type PreviewItem struct {
	Kind    string `json:"kind" jsonschema:"what the effect is: netns, cilium, cgroups, legacy-kubelet, local-path-dir, legacy-containerd-config, firewalld-source, firewalld-interface, selinux-contexts or apparmor-runc-profile"`
	Path    string `json:"path,omitempty" jsonschema:"the path, CIDR or interface the effect touches; empty for an effect with none, such as the registered SELinux contexts"`
	Detail  string `json:"detail" jsonschema:"what the effect is and why an uninstall touches it, rendered as-is by both the CLI dry run and this tool"`
	Unknown string `json:"unknown,omitempty" jsonschema:"the probe error, when the probe that should have answered could not; the effect would be left in place"`
}

type PreviewReport struct {
	Targets []string      `json:"targets" jsonschema:"the paths uninstall deletes, exactly Targets() returns"`
	Items   []PreviewItem `json:"items" jsonschema:"the conditional effects, fixed layout items first"`
}

// read-only probes, safe to share as a package variable: the MCP handler builds no hostEffects.
type Probes struct {
	LegacyHostConfig         func() (string, bool)
	TrustedPodCIDRs          func() ([]string, error)
	OwnedFirewalldInterfaces func() ([]string, error)
	SELinuxRegistered        func() bool
	// whether the kernel's runc AppArmor profile covers stratum's own runc.
	AppArmorWidened func() bool
}

var PreviewProbes = Probes{
	LegacyHostConfig:         runtime.LegacyHostConfig,
	TrustedPodCIDRs:          host.TrustedPodCIDRs,
	OwnedFirewalldInterfaces: host.OwnedFirewalldInterfaces,
	SELinuxRegistered:        selinux.Registered,
	AppArmorWidened:          apparmor.Widened,
}

// one value for both surfaces; reaches the host only through PreviewProbes.
func Preview() PreviewReport {
	report := PreviewReport{Targets: Targets()}
	add := func(kind, path, detail, unknown string) {
		report.Items = append(report.Items, PreviewItem{Kind: kind, Path: path, Detail: detail, Unknown: unknown})
	}

	// the datapath teardown always runs, so the preview always says so.
	add("netns", netnsDir, "the CNI network namespace mounts under it, detached by the datapath teardown", "")
	add("cilium", ciliumRunDir, "Cilium's run directory, with its links, iptables chains and BPF pins", "")
	add("cgroups", cgroupRoot, "the kubepods cgroups under it", "")
	add("legacy-kubelet", legacyKubeletDir, "the legacy kubelet directory, stripped of Stratum's kubelet's stale sockets and its CA-signed certificate pairs, and removed only once nothing else is left in it; another kubelet's state there is kept", "")
	add("local-path-dir", selinux.LocalPathDir, localPathPreviewDetail, "")

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
	switch {
	case PreviewProbes.AppArmorWidened == nil:
		add("apparmor-runc-profile", "", apparmorRuncDetail, "the probe is not wired")
	case PreviewProbes.AppArmorWidened():
		add("apparmor-runc-profile", "", apparmorRuncDetail, "")
	}
	return report
}

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

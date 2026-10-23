package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

var (
	hostResolvConf    = "/etc/resolv.conf"
	systemdResolvConf = "/run/systemd/resolve/resolv.conf"
)

var fallbackNameservers = []string{"8.8.8.8", "1.1.1.1"}

// CoreDNSUpstream is the file in CoreDNSUpstreamDir CoreDNS forwards through
// when it exists. Without it, CoreDNS forwards through its pod's resolv.conf,
// which is the node's.
func CoreDNSUpstream() string { return filepath.Join(version.CoreDNSUpstreamDir, "resolv.conf") }

// WriteCoreDNSUpstream chooses where this node's CoreDNS pods forward names
// outside the cluster, writes or removes CoreDNSUpstream to match,
// and reports the choice unless it is the node's own resolv.conf. Only CoreDNS
// reads it: the kubelet and every other pod keep the node's resolvers.
func WriteCoreDNSUpstream(st *nodestate.NodeState) error {
	// Written by an earlier release's kubelet resolvConf override, which nothing reads now.
	if err := os.Remove(filepath.Join(version.K8sConfDir, "resolv.conf")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	c, err := chooseCoreDNSUpstream(podIPv6(st))
	if err != nil {
		return err
	}
	if line := c.report(); line != "" {
		fmt.Print(line)
	}
	return nil
}

// upstreamChoice is what CoreDNS forwards through, and why.
type upstreamChoice struct {
	Written     bool   // CoreDNSUpstream holds Nameservers; otherwise it is absent
	Source      string // the node file the nameservers came from; "" for the fallback
	Nameservers []string
	LeftOut     []string // "<addr> from <file>: <reason>", or "<file>: <read error>"
}

func (c upstreamChoice) report() string {
	left := ""
	if len(c.LeftOut) > 0 {
		left = "; left out " + strings.Join(c.LeftOut, "; ")
	}
	switch {
	case c.Source == "":
		return fmt.Sprintf("  WARN: neither %s nor %s names a nameserver a pod can reach%s. CoreDNS forwards names outside the cluster through %s, which cannot see private or site DNS: give the node a nameserver pods can reach and restart stratum\n",
			hostResolvConf, systemdResolvConf, left, strings.Join(c.Nameservers, " and "))
	case !c.Written:
		return ""
	}
	return fmt.Sprintf("stratum: CoreDNS forwards through %s (%s, from %s)%s\n",
		CoreDNSUpstream(), strings.Join(c.Nameservers, " "), c.Source, left)
}

func chooseCoreDNSUpstream(ipv6 bool, noIPv6 string) (upstreamChoice, error) {
	var left []string
	for _, src := range []string{hostResolvConf, systemdResolvConf} {
		reachable, unreachable, err := podReachableNameservers(src, ipv6, noIPv6)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			left = append(left, src+": not present")
		case err != nil:
			left = append(left, fmt.Sprintf("%s: %v", src, err))
		}
		left = append(left, unreachable...)
		if len(reachable) == 0 {
			continue
		}
		c := upstreamChoice{Source: src, Nameservers: reachable, LeftOut: left}
		if src == hostResolvConf && len(unreachable) == 0 {
			err := os.Remove(CoreDNSUpstream())
			if errors.Is(err, fs.ErrNotExist) {
				err = nil
			}
			return c, err
		}
		c.Written = true
		return c, writeUpstream(reachable, src)
	}
	c := upstreamChoice{Written: true, Nameservers: fallbackNameservers, LeftOut: left}
	return c, writeUpstream(fallbackNameservers, hostResolvConf)
}

// writeUpstream writes CoreDNSUpstream, keeping the search, domain and options
// lines of the file at keep. It goes through the shared atomic writer
// (internal/atomicfile): staged under a hidden unique temp name, fsynced at
// its final mode, renamed into place, and the directory fsynced so the rename
// survives a power loss — the file is replaced whole, never truncated, so a
// pod starting mid-write reads the old file or the new one.
func writeUpstream(nameservers []string, keep string) error {
	var b strings.Builder
	for _, ns := range nameservers {
		fmt.Fprintf(&b, "nameserver %s\n", ns)
	}
	if src, err := os.ReadFile(keep); err == nil {
		for _, line := range strings.Split(string(src), "\n") {
			if f := strings.Fields(line); len(f) > 0 && (f[0] == "search" || f[0] == "domain" || f[0] == "options") {
				b.WriteString(line + "\n")
			}
		}
	}
	if err := os.MkdirAll(version.CoreDNSUpstreamDir, 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(CoreDNSUpstream(), []byte(b.String()), 0o644)
}

// podIPv6 reports whether this node's pods have IPv6, from the cluster ranges
// it recorded. When they have none, noIPv6 says why an IPv6 nameserver was
// left out.
func podIPv6(st *nodestate.NodeState) (ok bool, noIPv6 string) {
	for _, cidrs := range []string{st.PodCIDR, st.ServiceCIDR} {
		for _, c := range strings.Split(cidrs, ",") {
			if p, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil && p.Addr().Is6() {
				return true, ""
			}
		}
	}
	if st.PodCIDR == "" && st.ServiceCIDR == "" {
		return false, "IPv6, and this node recorded no cluster ranges, so its pods count as IPv4-only"
	}
	return false, "IPv6, and pods here have no IPv6"
}

// podReachableNameservers splits the nameservers in path into those a pod
// reaches and the rest, each as "<addr> from <path>: <reason>".
func podReachableNameservers(path string, ipv6 bool, noIPv6 string) (reachable, unreachable []string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(f[1])
		if err != nil {
			continue
		}
		if why := podUnreachable(a, ipv6, noIPv6); why != "" {
			unreachable = append(unreachable, fmt.Sprintf("%s from %s: %s", f[1], path, why))
		} else {
			reachable = append(reachable, f[1])
		}
	}
	return reachable, unreachable, nil
}

// podUnreachable says why a pod cannot reach a nameserver at a, or "".
func podUnreachable(a netip.Addr, ipv6 bool, noIPv6 string) string {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return "loopback"
	case a.IsUnspecified():
		return "unspecified"
	case a.Is6() && a.IsLinkLocalUnicast():
		return "IPv6 link-local"
	case a.Is6() && !ipv6:
		return noIPv6
	}
	return ""
}

// Package netcfg understands the cluster's pod and service CIDRs and families.
package netcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// Config is the parsed, validated cluster network configuration.
type Config struct {
	DualStack bool

	// Verbatim comma-joined strings passed straight to the control-plane flags.
	PodCIDR     string
	ServiceCIDR string

	// Derived: Service IP is the range's first host, DNS its tenth, or legacy 10.96.0.10 when inside.
	ServiceIPv4 string
	DNSIPv4     string
	ServiceIPv6 string

	// Parse leaves it at the default; Load returns the persisted one.
	ClusterDomain string
}

// The tenth address of the service CIDR, the conventional cluster DNS one.
const dnsHostIndex = 10

// Each argument is one or two CIDRs, IPv4 first when two, matching family counts.
func Parse(podCIDR, serviceCIDR string) (Config, error) {
	pods, err := splitFamilies(podCIDR, "pod-cidr")
	if err != nil {
		return Config{}, err
	}
	svcs, err := splitFamilies(serviceCIDR, "service-cidr")
	if err != nil {
		return Config{}, err
	}
	if len(pods) != len(svcs) {
		return Config{}, fmt.Errorf("pod-cidr and service-cidr must have the same number of families (got %d pod, %d service) — supply both single-stack or both dual-stack", len(pods), len(svcs))
	}
	c := Config{PodCIDR: podCIDR, ServiceCIDR: serviceCIDR, ClusterDomain: netdefault.ClusterDomain}
	if c.ServiceIPv4, c.DNSIPv4, err = serviceIPs(svcs[0]); err != nil {
		return Config{}, err
	}
	if len(svcs) == 2 {
		c.DualStack = true
		c.ServiceIPv6 = firstHost(svcs[1])
	}
	return c, nil
}

// The domain lands in kubelet.conf, apiserver flags and the Corefile: no injection.
func ValidateDomain(domain string) error {
	if errs := validation.IsDNS1123Subdomain(domain); len(errs) > 0 {
		return fmt.Errorf("cluster domain %q: %s", domain, strings.Join(errs, "; "))
	}
	return nil
}

// Authoritative once it exists: a crash mid-write must leave the old file or the new.
func Save(podCIDR, serviceCIDR, domain string) error {
	domain = DomainOr(domain)
	if err := ValidateDomain(domain); err != nil {
		return err
	}
	c := Config{PodCIDR: podCIDR, ServiceCIDR: serviceCIDR, ClusterDomain: domain}
	return atomicfile.WriteFile(version.NetConfigPath, c.Encode(), 0o644)
}

// A default domain keeps two lines for pre-domain binaries; a third line cannot roll back.
func (c Config) Encode() []byte {
	b := c.PodCIDR + "\n" + c.ServiceCIDR + "\n"
	if c.ClusterDomain != "" && c.ClusterDomain != netdefault.ClusterDomain {
		b += c.ClusterDomain + "\n"
	}
	return []byte(b)
}

// The file is authoritative: an absent one recovers state, a damaged one is an error.
func Load() (Config, error) {
	b, err := os.ReadFile(version.NetConfigPath)
	if os.IsNotExist(err) {
		r, err := recoverState()
		if err != nil {
			return Config{}, err
		}
		// Recovered values are validated through Parse like a persisted one.
		c, err := WithDefaults(r.podCIDR, r.serviceCIDR)
		if err != nil {
			return Config{}, fmt.Errorf("network ranges recovered from %s: %w", r.source, err)
		}
		if r.found {
			c.ClusterDomain = r.domain
		}
		return c, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", version.NetConfigPath, err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	// Two lines: a pre-domain cluster, or a default-domain one.
	if len(lines) != 2 && len(lines) != 3 {
		return Config{}, fmt.Errorf("%s: expected 2 or 3 lines (pod-cidr, service-cidr, cluster domain), got %d%s", version.NetConfigPath, len(lines), repairHint)
	}
	c, err := Parse(strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]))
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w%s", version.NetConfigPath, err, repairHint)
	}
	if len(lines) == 3 {
		c.ClusterDomain = strings.TrimSpace(lines[2])
		if err := ValidateDomain(c.ClusterDomain); err != nil {
			return Config{}, fmt.Errorf("%s: %w%s", version.NetConfigPath, err, repairHint)
		}
	}
	return c, nil
}

// Once recorded the domain wins; a differing configured one is refused, never half-applied.
func ResolveDomain(configured string) (string, error) {
	recorded, source, found, err := recordedDomain()
	if err != nil {
		return "", err
	}
	if !found {
		return DomainOr(configured), nil
	}
	if configured != "" && configured != recorded {
		return "", fmt.Errorf("spec.network.clusterDomain %q does not match this cluster's domain %q recorded in %s: "+
			"the cluster domain cannot be changed after init; remove the field or set it to %q",
			configured, recorded, source, recorded)
	}
	return recorded, nil
}

func recordedDomain() (domain, source string, found bool, err error) {
	if _, err := os.Stat(version.NetConfigPath); err == nil {
		c, err := Load()
		return c.ClusterDomain, version.NetConfigPath, true, err
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", "", false, fmt.Errorf("stat %s: %w", version.NetConfigPath, err)
	}
	r, err := recoverState()
	return r.domain, r.source, r.found, err
}

// An empty podCIDR or serviceCIDR means the state records none; use the default.
type recovered struct {
	domain, podCIDR, serviceCIDR string
	source                       string
	found                        bool // false for a node with neither state, nor CA, nor etcd data: a first start
}

func recoverState() (recovered, error) {
	st, err := nodestate.LoadState()
	switch {
	case err == nil:
		// Fail here rather than only after a joiner has downloaded the CA.
		if err := ValidateDomain(st.Domain()); err != nil {
			return recovered{}, fmt.Errorf("node state %s: %w", nodestate.StatePath(), err)
		}
		return recovered{domain: st.Domain(), podCIDR: st.PodCIDR, serviceCIDR: st.ServiceCIDR,
			source: nodestate.StatePath(), found: true}, nil
	case !errors.Is(err, fs.ErrNotExist):
		// An unreadable state is refused, not guessed at.
		return recovered{}, fmt.Errorf("read node state %s: %w", nodestate.StatePath(), err)
	}
	ca := filepath.Join(version.PKIDir, "ca.crt")
	if _, err := os.Stat(ca); err == nil {
		return recovered{domain: netdefault.ClusterDomain, source: ca, found: true}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return recovered{}, fmt.Errorf("stat %s: %w", ca, err)
	}
	// Only member-data presence is establishment; no network value is read from it.
	retained, err := RetainedMemberData(version.EtcdDataDir)
	if err != nil {
		return recovered{}, err
	}
	if retained {
		return recovered{domain: netdefault.ClusterDomain, source: version.EtcdDataDir, found: true}, nil
	}
	return recovered{}, nil
}

// A WAL without the database is still a member's history, so not a first start.
func RetainedMemberData(dir string) (bool, error) {
	etcdDB := filepath.Join(dir, "member", "snap", "db")
	if _, err := os.Stat(etcdDB); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat %s: %w", etcdDB, err)
	}
	wal, err := os.ReadDir(filepath.Join(dir, "member", "wal"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", filepath.Join(dir, "member", "wal"), err)
	}
	for _, e := range wal {
		// Any regular file in wal/ is history; a non-file is refused, not read.
		if e.Type().IsRegular() {
			return true, nil
		}
	}
	if len(wal) > 0 {
		return false, fmt.Errorf("inspect %s: holds entries that are not WAL segments, so whether it holds retained member history cannot be decided",
			filepath.Join(dir, "member", "wal"))
	}
	return false, nil
}

const repairHint = "; the file is damaged -- restore it with the cluster's pod and service CIDRs (and its " +
	"cluster domain, if it has a custom one), copied from another control plane's netconfig"

// Localhost and the node's own IPs are added by pki.LeafSpecs, not here.
func (c Config) APIServerExtraIPs() []string {
	if !c.DualStack {
		return []string{c.ServiceIPv4}
	}
	return []string{c.ServiceIPv4, c.ServiceIPv6, "::1"}
}

// For callers that know the service range but not the pod range Parse needs.
func DNSIPFor(serviceCIDR string) (string, error) {
	svcs, err := splitFamilies(serviceCIDR, "service-cidr")
	if err != nil {
		return "", err
	}
	_, dns, err := serviceIPs(svcs[0])
	return dns, err
}

// For a worker that trusts the range in firewalld and has no service range to pair.
func ValidatePodCIDR(podCIDR string) error {
	_, err := splitFamilies(podCIDR, "pod-cidr")
	return err
}

// An upgraded cluster's immutable Service still holds it; it equals the default by coincidence.
const legacyDNSIP = "10.96.0.10"

// A ClusterIP is immutable, so a range containing the legacy DNS address keeps it.
func serviceIPs(n *net.IPNet) (svc, dns string, err error) {
	if !isV4(n) {
		return "", "", fmt.Errorf("service-cidr: the first (primary) family must be IPv4, got %s", n)
	}
	ones, bits := n.Mask.Size()
	if bits-ones < 4 { // fewer than 16 addresses: no room for the tenth
		return "", "", fmt.Errorf("service-cidr %s is too small: it must hold at least 16 addresses (/28 or larger) "+
			"so the cluster DNS Service can take its tenth address", n)
	}
	dns = nthHost(n, dnsHostIndex)
	if n.Contains(net.ParseIP(legacyDNSIP)) {
		dns = legacyDNSIP
	}
	return nthHost(n, 1), dns, nil
}

// One function for install, join and renewal, so the three cannot drift apart.
func (c Config) APIServerSANs(vip string) []string {
	ips := c.APIServerExtraIPs()
	if vip != "" {
		ips = append(ips, vip)
	}
	return ips
}

// Invalid lists never compare equal, even to themselves.
func EqualCIDRs(a, b string) bool {
	left, err := splitFamilies(a, "CIDR list")
	if err != nil {
		return false
	}
	right, err := splitFamilies(b, "CIDR list")
	if err != nil || len(left) != len(right) {
		return false
	}
	for i, n := range left {
		if n.String() != right[i].String() {
			return false
		}
	}
	return true
}

func splitFamilies(s, flag string) ([]*net.IPNet, error) {
	parts := strings.Split(s, ",")
	if len(parts) == 0 || len(parts) > 2 {
		return nil, fmt.Errorf("%s: expected 1 or 2 comma-separated CIDRs, got %q", flag, s)
	}
	nets := make([]*net.IPNet, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid CIDR %q: %w", flag, p, err)
		}
		nets = append(nets, n)
	}
	if len(nets) == 2 {
		if !isV4(nets[0]) || isV4(nets[1]) {
			return nil, fmt.Errorf("%s: dual-stack must list the IPv4 CIDR first, then IPv6 (got %q)", flag, s)
		}
	}
	return nets, nil
}

func isV4(n *net.IPNet) bool { return n.IP.To4() != nil }

func firstHost(n *net.IPNet) string { return nthHost(n, 1) }

// Callers keep k inside the range; serviceIPs checks the size.
func nthHost(n *net.IPNet, k int) string {
	ip := make(net.IP, len(n.IP))
	copy(ip, n.IP)
	for ; k > 0; k-- {
		for i := len(ip) - 1; i >= 0; i-- {
			ip[i]++
			if ip[i] != 0 {
				break
			}
		}
	}
	return ip.String()
}

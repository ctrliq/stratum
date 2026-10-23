// Package netcfg is the single place that understands the cluster's pod/service
// CIDRs and their address families, so every consumer (systemd unit flags, PKI
// SANs, Cilium, node-IP detection) agrees on single- vs dual-stack.
//
// v1 supports single-stack IPv4 (the default) and IPv4-primary dual-stack. A
// dual-stack cluster is expressed as comma-separated CIDRs with IPv4 first, e.g.
//
//	--pod-cidr     10.245.0.0/16,fd00:245::/56
//	--service-cidr 10.96.0.0/12,fd00:96::/108
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
	DualStack bool // true when both a pod and service IPv6 CIDR are present

	// PodCIDR / ServiceCIDR are the verbatim comma-joined strings passed straight
	// to the control-plane flags (--cluster-cidr / --service-cluster-ip-range).
	PodCIDR     string
	ServiceCIDR string

	// Derived service IPs. ServiceIPv4 is the first host of the IPv4 service
	// CIDR — the `kubernetes` Service's ClusterIP, which the apiserver
	// allocates itself and which must be an apiserver cert SAN — and DNSIPv4 the
	// tenth, or the legacy 10.96.0.10 when the range contains it (serviceIPs),
	// the cluster DNS Service's ClusterIP (kubelet clusterDNS). Default()
	// derives 10.96.0.1 and 10.96.0.10 the same way, as the first and tenth
	// addresses of the default service range 10.96.0.0/12; deriving them is
	// what makes a custom service CIDR work at all. ServiceIPv6 is the first
	// host of the IPv6 service CIDR (the secondary kubernetes-service
	// ClusterIP); empty when single-stack.
	ServiceIPv4 string
	DNSIPv4     string
	ServiceIPv6 string

	// ClusterDomain is the cluster's DNS domain. Parse leaves it at the
	// default; Load and Resolve return the persisted one.
	ClusterDomain string
}

// dnsHostIndex is which address of the service CIDR the cluster DNS Service
// takes: the tenth, the conventional address for it.
const dnsHostIndex = 10

// Parse validates the pod and service CIDR strings and reports whether the
// cluster is dual-stack. Each argument is one or two comma-separated CIDRs; when
// two are given they must be IPv4 first then IPv6, and pod and service must have
// the same family count (both single or both dual).
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

// ValidateDomain rejects a cluster domain that is not a DNS subdomain. It
// lands in kubelet.conf, apiserver flags and the Corefile, so a value with a
// newline or a space must stop here.
func ValidateDomain(domain string) error {
	if errs := validation.IsDNS1123Subdomain(domain); len(errs) > 0 {
		return fmt.Errorf("cluster domain %q: %s", domain, strings.Join(errs, "; "))
	}
	return nil
}

// Save persists the effective CIDRs and cluster domain to version.NetConfigPath
// (pod-cidr, service-cidr and, for a non-default domain, the cluster domain, one
// per line) so HA joins and cert rotation inherit them. Always written at
// cluster-init, even single-stack, so custom IPv4 CIDRs propagate too. An empty
// domain is saved as the default (the two-line format, see Encode), and an
// invalid one is refused without writing anything.
//
// Written through the shared atomic writer (internal/atomicfile): a temp file
// in the same directory, fsynced at its final 0644 mode, renamed over the old
// one, and the rename made durable. The file is authoritative once it exists,
// so a crash mid-write must leave either the old file or the new, never a
// truncated one.
func Save(podCIDR, serviceCIDR, domain string) error {
	domain = DomainOr(domain)
	if err := ValidateDomain(domain); err != nil {
		return err
	}
	c := Config{PodCIDR: podCIDR, ServiceCIDR: serviceCIDR, ClusterDomain: domain}
	return atomicfile.WriteFile(version.NetConfigPath, c.Encode(), 0o644)
}

// Encode is the netconfig file format Save writes and Load reads: pod CIDR and
// service CIDR, one per line, plus the cluster domain as a third line only when
// it is not the default. The shared-cert bundle carries the same bytes, so a
// joiner's file is what Save would have written.
//
// A default-domain cluster keeps the two-line format on purpose: the binary
// before this change reads exactly two lines, so a cluster that is upgraded and
// then rolled back still starts. A custom domain needs the third line, and that
// cluster cannot be rolled back to a binary that never supported one.
func (c Config) Encode() []byte {
	b := c.PodCIDR + "\n" + c.ServiceCIDR + "\n"
	if c.ClusterDomain != "" && c.ClusterDomain != netdefault.ClusterDomain {
		b += c.ClusterDomain + "\n"
	}
	return []byte(b)
}

// Load returns the persisted cluster network config, or the built-in IPv4
// single-stack defaults when the file is absent (an IPv4-only or pre-existing node).
//
// An absent file is not simply the defaults for a node that is already
// established (node state, the cluster CA, or retained etcd data on disk) and
// has lost its netconfig: it is on the domain and the pod and service ranges
// its node state records, as in ResolveDomain. Every caller that reads the
// network config through Load (certificate rotation and the upgrade reissue,
// `token create`, the shared-cert bundle, joins) therefore gets the effective
// values, and none of them can reissue an apiserver certificate or print a
// join line for cluster.local and the default ranges on a cluster that runs
// others. A first start has none of these, so it is still the defaults.
// A file that is there but damaged is an error that names it: the file is
// authoritative, so nothing guesses past it.
func Load() (Config, error) {
	b, err := os.ReadFile(version.NetConfigPath)
	if os.IsNotExist(err) {
		r, err := recoverState()
		if err != nil {
			return Config{}, err
		}
		// Each field falls back to the built-in default only when the recovered
		// state has no value for it (legacy node state, or the CA alone), and
		// the pair is validated through Parse like a persisted one.
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
	// Two lines is a cluster initialised before the domain was recorded, or a
	// default-domain one (see Encode).
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

// ResolveDomain returns the cluster domain a control plane starts with. Once
// the cluster's domain is recorded the recorded one wins, so a control plane
// that joined a cluster keeps the joined domain across restarts whatever its
// config says. A configured domain that differs from it is refused: the domain
// cannot change after init, and half-applying it would move the kubelet,
// apiserver flags and CoreDNS but not the certificates or the workers.
//
// The domain is recorded in netconfig. A node without one that is already
// established (node state, the cluster CA, or retained etcd data on disk: a
// legacy cluster, or a lost file) is on the domain its node state records, else
// cluster.local, which is what every release before this one ran, exactly as
// for a legacy two-line netconfig. Only a node with none of these is on a
// first start: the configured domain, else the default.
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

// recordedDomain finds the domain this node is already on, and where it says so.
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

// recovered is what an established node with no netconfig can say about its
// cluster. An empty podCIDR or serviceCIDR means the state records none (legacy
// node state, or only the CA or etcd data is on disk), and the caller uses the
// default.
type recovered struct {
	domain, podCIDR, serviceCIDR string
	source                       string
	found                        bool // false for a node with neither state, nor CA, nor etcd data: a first start
}

// recoverState is the cluster domain and ranges of an established node whose
// netconfig is absent: the ones its node state records (the domain defaulting to
// cluster.local), or the defaults when only the cluster CA or retained etcd
// member data is on disk.
func recoverState() (recovered, error) {
	st, err := nodestate.LoadState()
	switch {
	case err == nil:
		// The domain flows into netconfig and, through the shared-cert bundle,
		// to every joiner, so a bad value in node state fails here, on the
		// donor, rather than only after a joiner has downloaded the CA.
		if err := ValidateDomain(st.Domain()); err != nil {
			return recovered{}, fmt.Errorf("node state %s: %w", nodestate.StatePath(), err)
		}
		return recovered{domain: st.Domain(), podCIDR: st.PodCIDR, serviceCIDR: st.ServiceCIDR,
			source: nodestate.StatePath(), found: true}, nil
	case !errors.Is(err, fs.ErrNotExist):
		// A state we cannot read belongs to a node we cannot see: do not guess.
		return recovered{}, fmt.Errorf("read node state %s: %w", nodestate.StatePath(), err)
	}
	ca := filepath.Join(version.PKIDir, "ca.crt")
	if _, err := os.Stat(ca); err == nil {
		return recovered{domain: netdefault.ClusterDomain, source: ca, found: true}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return recovered{}, fmt.Errorf("stat %s: %w", ca, err)
	}
	// Retained etcd member data is establishment too: a node
	// holding it has been an etcd member whatever node.yaml and the CA say
	// today, so it is on the domain the default records — the domain every
	// release before this one ran. Only its presence is establishment: the
	// database's layout belongs to etcd, and no network value is read from it.
	// WAL segments count as history the same as the database: a member whose
	// database is gone but whose WAL survives cannot have its membership read,
	// which is exactly why it is not a first start.
	retained, err := RetainedMemberData(version.EtcdDataDir)
	if err != nil {
		return recovered{}, err
	}
	if retained {
		return recovered{domain: netdefault.ClusterDomain, source: version.EtcdDataDir, found: true}, nil
	}
	return recovered{}, nil
}

// RetainedMemberData reports whether dir holds retained etcd member data:
// the snapshot database or any write-ahead log segment. Either is history a
// node has carried as an etcd member — the WAL without the database is a
// member whose history can no longer be read, which is all the more reason it
// is not a first start — so callers treat true as "the recorded network is in
// force" and an inspect error as refusal, never as a fresh node.
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
		// Etcd names WAL segments <seq>-<seq>.wal; any regular file there is
		// history. A directory or symlink is not proof either way, so it is
		// refused (below) rather than read through.
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

// Resolve returns the cluster network config for an HA join / cert rotation:
// explicit override when both CIDRs are supplied, else the persisted Load()
// (which itself falls back to the IPv4 defaults when no file is present). The
// cluster domain always comes from the persisted file: no flag overrides it. A
// netconfig that exists but cannot be read or validated is an error even with
// explicit CIDRs, since falling back to the default domain would put the wrong
// DNS suffix in certificates and manifests.
func Resolve(podCIDR, serviceCIDR string) (Config, error) {
	if podCIDR == "" || serviceCIDR == "" {
		return Load()
	}
	c, err := Parse(podCIDR, serviceCIDR)
	if err != nil {
		return Config{}, err
	}
	persisted, err := Load()
	if err != nil {
		return Config{}, err
	}
	c.ClusterDomain = persisted.ClusterDomain
	return c, nil
}

// APIServerExtraIPs returns the apiserver-cert IP SANs that come from the
// cluster network rather than the node: the `kubernetes` Service IP, plus the
// IPv6 service IP and IPv6 loopback under dual-stack. (Localhost and the node's
// own IPs are added by pki.LeafSpecs.)
func (c Config) APIServerExtraIPs() []string {
	if !c.DualStack {
		return []string{c.ServiceIPv4}
	}
	return []string{c.ServiceIPv4, c.ServiceIPv6, "::1"}
}

// DNSIPFor returns the cluster DNS IP for a service CIDR ("v4" or "v4,v6"; the
// IPv4 family decides) — for callers, like a worker's kubelet, that know the
// service range but not the pod range Parse also needs.
func DNSIPFor(serviceCIDR string) (string, error) {
	svcs, err := splitFamilies(serviceCIDR, "service-cidr")
	if err != nil {
		return "", err
	}
	_, dns, err := serviceIPs(svcs[0])
	return dns, err
}

// ValidatePodCIDR checks a pod CIDR list ("v4" or "v4,v6") the way Parse does,
// for a caller that is told only the pod range: a worker, which trusts it in
// firewalld and has no service range to pair it with.
func ValidatePodCIDR(podCIDR string) error {
	_, err := splitFamilies(podCIDR, "pod-cidr")
	return err
}

// legacyDNSIP is the cluster DNS address every earlier release used,
// whatever the configured service range.
const legacyDNSIP = "10.96.0.10"

// serviceIPs derives the kubernetes Service IP and the cluster DNS IP from the
// IPv4 service CIDR, refusing a range too small to hold the DNS address.
//
// The DNS IP is the tenth address, except when the range contains the legacy
// 10.96.0.10. Earlier releases always created the kube-dns Service at
// 10.96.0.10, and every kubelet pointed there. On a custom range
// that happened to contain it (10.64.0.0/10, say), that cluster worked, and its
// Service holds that ClusterIP. A ClusterIP is immutable, so deriving the tenth
// address instead would make the deployer's apply fail, and would point
// kubelets at an address nothing serves. On the default range the two rules
// agree, since 10.96.0.10 is its tenth address. So every cluster that worked
// keeps its DNS IP, and only ranges that never contained 10.96.0.10 get the
// derived address. No state is needed to decide it, so every node decides
// the same.
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

// APIServerSANs is APIServerExtraIPs plus the cluster VIP when there is one:
// the extra IP SANs every control plane's apiserver certificate carries. One
// function for install, HA join and renewal, because the three drifting apart
// is how a joined control plane ended up serving a certificate the VIP was not
// in, and how every renewal then removed it from the rest.
func (c Config) APIServerSANs(vip string) []string {
	ips := c.APIServerExtraIPs()
	if vip != "" {
		ips = append(ips, vip)
	}
	return ips
}

// EqualCIDRs reports whether two valid CIDR lists describe the same networks
// in the same family order. Whitespace, IPv6 spelling and host bits do not
// change a network. Invalid lists never compare equal, even to themselves.
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

// splitFamilies parses a "v4[,v6]" CIDR list, requiring 1 or 2 entries, IPv4
// first when two, and each a valid CIDR.
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

// isV4 reports whether an IPNet is IPv4 (as opposed to IPv6).
func isV4(n *net.IPNet) bool { return n.IP.To4() != nil }

// firstHost returns the first usable address of a CIDR (network+1), e.g.
// fd00:96::/108 -> "fd00:96::1". Used to derive the apiserver's IPv6 service IP.
func firstHost(n *net.IPNet) string { return nthHost(n, 1) }

// nthHost returns the network address plus k, e.g. (10.96.0.0/12, 10) ->
// "10.96.0.10". Callers keep k inside the range (serviceIPs checks the size).
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

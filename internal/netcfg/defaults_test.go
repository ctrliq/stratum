package netcfg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
)

// TestNetworkDefaultsKeepTheirValues: the defaults moved home, not value. A
// different one would move the pod or service range, the kubernetes Service IP,
// the DNS address or the domain under every cluster installed on the old one.
func TestNetworkDefaultsKeepTheirValues(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"PodCIDR", netdefault.PodCIDR, "10.245.0.0/16"},
		{"ServiceCIDR", netdefault.ServiceCIDR, "10.96.0.0/12"},
		{"APIServerSvcIP", netdefault.APIServerSvcIP, "10.96.0.1"},
		{"DNSClusterIP", netdefault.DNSClusterIP, "10.96.0.10"},
		{"ClusterDomain", netdefault.ClusterDomain, "cluster.local"},
	} {
		if tc.got != tc.want {
			t.Errorf("netdefault.%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestNodeCIDRMasksAreFixed: the per-node pod-CIDR masks are what the
// controller-manager carves each node's range at, the same on every release.
func TestNodeCIDRMasksAreFixed(t *testing.T) {
	if NodeCIDRMask != 24 || NodeCIDRMaskV6 != 64 {
		t.Errorf("NodeCIDRMask, NodeCIDRMaskV6 = %d, %d; want 24, 64", NodeCIDRMask, NodeCIDRMaskV6)
	}
}

// TestDefaultIsTheBuiltInNetwork: Default is the network a cluster gets when
// the user names none, and its Service and DNS addresses are the ones derived
// from the default service range, so the named defaults and the range they
// come from cannot drift apart.
func TestDefaultIsTheBuiltInNetwork(t *testing.T) {
	c := Default()
	if c.PodCIDR != netdefault.PodCIDR || c.ServiceCIDR != netdefault.ServiceCIDR {
		t.Errorf("Default() ranges = %q, %q; want %q, %q", c.PodCIDR, c.ServiceCIDR, netdefault.PodCIDR, netdefault.ServiceCIDR)
	}
	if c.ServiceIPv4 != netdefault.APIServerSvcIP {
		t.Errorf("Default().ServiceIPv4 = %q, want netdefault.APIServerSvcIP %q", c.ServiceIPv4, netdefault.APIServerSvcIP)
	}
	if c.DNSIPv4 != netdefault.DNSClusterIP {
		t.Errorf("Default().DNSIPv4 = %q, want netdefault.DNSClusterIP %q", c.DNSIPv4, netdefault.DNSClusterIP)
	}
	if c.ClusterDomain != netdefault.ClusterDomain {
		t.Errorf("Default().ClusterDomain = %q, want %q", c.ClusterDomain, netdefault.ClusterDomain)
	}
	if c.DualStack || c.ServiceIPv6 != "" {
		t.Errorf("Default() DualStack, ServiceIPv6 = %v, %q; want single-stack IPv4", c.DualStack, c.ServiceIPv6)
	}
	parsed, err := Parse(netdefault.PodCIDR, netdefault.ServiceCIDR)
	if err != nil {
		t.Fatal(err)
	}
	if c != parsed {
		t.Errorf("Default() = %+v, want what Parse derives from the default ranges: %+v", c, parsed)
	}
}

// TestDomainOrFillsOnlyAnEmptyDomain: a domain the user gives is never
// swapped for the default, even one that is not a valid domain — refusing it
// is the validator's job, and dropping it silently would hide the mistake.
func TestDomainOrFillsOnlyAnEmptyDomain(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", netdefault.ClusterDomain},
		{netdefault.ClusterDomain, netdefault.ClusterDomain},
		{"fuzz.internal", "fuzz.internal"},
		{"Bad Domain", "Bad Domain"},
	} {
		if got := DomainOr(tc.in); got != tc.want {
			t.Errorf("DomainOr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestWithDefaultsFillsOnlyAnEmptyCIDR: each range the user gives is kept
// verbatim and its addresses derived from it; only a range left empty takes
// the default. The domain is the default, as with Parse.
func TestWithDefaultsFillsOnlyAnEmptyCIDR(t *testing.T) {
	for _, tc := range []struct {
		name, pod, svc string
		want           Config
	}{
		{"neither given: the built-in network", "", "", Config{
			PodCIDR: netdefault.PodCIDR, ServiceCIDR: netdefault.ServiceCIDR,
			ServiceIPv4: netdefault.APIServerSvcIP, DNSIPv4: netdefault.DNSClusterIP, ClusterDomain: netdefault.ClusterDomain,
		}},
		{"pod range given", "10.42.0.0/16", "", Config{
			PodCIDR: "10.42.0.0/16", ServiceCIDR: netdefault.ServiceCIDR,
			ServiceIPv4: netdefault.APIServerSvcIP, DNSIPv4: netdefault.DNSClusterIP, ClusterDomain: netdefault.ClusterDomain,
		}},
		{"service range given", "", "10.43.0.0/16", Config{
			PodCIDR: netdefault.PodCIDR, ServiceCIDR: "10.43.0.0/16",
			ServiceIPv4: "10.43.0.1", DNSIPv4: "10.43.0.10", ClusterDomain: netdefault.ClusterDomain,
		}},
		{"both given", "10.42.0.0/16", "10.43.0.0/16", Config{
			PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16",
			ServiceIPv4: "10.43.0.1", DNSIPv4: "10.43.0.10", ClusterDomain: netdefault.ClusterDomain,
		}},
		{"dual-stack given", "10.42.0.0/16,fd00:42::/56", "10.43.0.0/16,fd00:43::/108", Config{
			DualStack: true, PodCIDR: "10.42.0.0/16,fd00:42::/56", ServiceCIDR: "10.43.0.0/16,fd00:43::/108",
			ServiceIPv4: "10.43.0.1", DNSIPv4: "10.43.0.10", ServiceIPv6: "fd00:43::1", ClusterDomain: netdefault.ClusterDomain,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WithDefaults(tc.pod, tc.svc)
			if err != nil {
				t.Fatalf("WithDefaults(%q, %q): %v", tc.pod, tc.svc, err)
			}
			if got != tc.want {
				t.Errorf("WithDefaults(%q, %q) = %+v, want %+v", tc.pod, tc.svc, got, tc.want)
			}
		})
	}
}

// TestWithDefaultsRefusesAGivenCIDRItCannotUse: a range the user gives that
// is not usable is an error naming why, never replaced by the default, which
// would start the cluster on a network the user did not ask for.
func TestWithDefaultsRefusesAGivenCIDRItCannotUse(t *testing.T) {
	for _, tc := range []struct{ name, pod, svc, wantErr string }{
		{"invalid pod range", "not-a-cidr", "", "invalid CIDR"},
		{"invalid service range", "", "not-a-cidr", "invalid CIDR"},
		{"service range too small", "", "10.43.0.0/29", "too small"},
		{"dual-stack pod range against the single-stack default", "10.42.0.0/16,fd00:42::/56", "", "same number of families"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := WithDefaults(tc.pod, tc.svc)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("WithDefaults(%q, %q) = %+v, %v; want an error containing %q", tc.pod, tc.svc, c, err, tc.wantErr)
			}
		})
	}
}

// TestNetdefaultImportsNothingFromThisModule: nodestate reads the default
// domain and netcfg imports nodestate, so the defaults can serve both only
// from a package that imports neither, nor anything of this module that could
// reach back to them.
func TestNetdefaultImportsNothingFromThisModule(t *testing.T) {
	const module = "github.com/ctrliq/stratum"
	files, err := filepath.Glob(filepath.Join("netdefault", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if path == module || strings.HasPrefix(path, module+"/") {
				t.Errorf("%s imports %s; netdefault must import nothing from %s", p, path, module)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test Go file found in netdefault: the check would pass without checking anything")
	}
}

// networkDefaultNames are the network defaults that left internal/version.
var networkDefaultNames = map[string]bool{
	"PodCIDR": true, "ServiceCIDR": true, "APIServerSvcIP": true, "DNSClusterIP": true,
	"ClusterDomain": true, "NodeCIDRMask": true, "NodeCIDRMaskV6": true,
}

// netcfgSources parses this package's non-test Go files.
func netcfgSources(t *testing.T) map[string]*ast.File {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		parsed[p] = f
	}
	if len(parsed) == 0 {
		t.Fatal("no non-test Go file found in netcfg: the check would pass without checking anything")
	}
	return parsed
}

// TestLegacyDNSIPStaysItsOwnAddress: legacyDNSIP is the kube-dns address
// every earlier release used, which an upgraded cluster's immutable Service
// still holds. It equals the default DNS IP only by coincidence, so it stays a
// literal of its own: aliased to the default, a change to the default would
// move every such cluster's DNS address with it.
func TestLegacyDNSIPStaysItsOwnAddress(t *testing.T) {
	found := false
	for p, f := range netcfgSources(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if name.Name != "legacyDNSIP" {
					continue
				}
				found = true
				if i >= len(spec.Values) {
					t.Errorf("%s: legacyDNSIP has no value of its own", p)
					continue
				}
				if lit, ok := spec.Values[i].(*ast.BasicLit); !ok || lit.Value != `"10.96.0.10"` {
					t.Errorf("%s: legacyDNSIP is not the literal \"10.96.0.10\"", p)
				}
			}
			return true
		})
	}
	if !found {
		t.Error("legacyDNSIP is not declared in netcfg")
	}
}

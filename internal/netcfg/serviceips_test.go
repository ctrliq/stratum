package netcfg

import (
	"slices"
	"strings"
	"testing"
)

// TestServiceIPsFollowTheServiceCIDR pins that the kubernetes Service IP
// and the cluster DNS IP are the first and tenth addresses of the cluster's
// actual service range. At the default they are the old constants, exactly.
func TestServiceIPsFollowTheServiceCIDR(t *testing.T) {
	for _, tc := range []struct {
		name, pod, svc   string
		wantSvc, wantDNS string
		wantExtra        []string
	}{
		{"default", "10.245.0.0/16", "10.96.0.0/12", "10.96.0.1", "10.96.0.10", []string{"10.96.0.1"}},
		{"custom /16 (the old docs sample)", "10.42.0.0/16", "10.43.0.0/16", "10.43.0.1", "10.43.0.10", []string{"10.43.0.1"}},
		{"smallest allowed /28", "10.42.0.0/16", "10.43.0.0/28", "10.43.0.1", "10.43.0.10", []string{"10.43.0.1"}},
		{"dual-stack: the IPv4 family decides", "10.42.0.0/16,fd00:42::/56", "10.43.0.0/16,fd00:43::/108",
			"10.43.0.1", "10.43.0.10", []string{"10.43.0.1", "fd00:43::1", "::1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse(tc.pod, tc.svc)
			if err != nil {
				t.Fatal(err)
			}
			if c.ServiceIPv4 != tc.wantSvc || c.DNSIPv4 != tc.wantDNS {
				t.Errorf("ServiceIPv4, DNSIPv4 = %s, %s; want %s, %s", c.ServiceIPv4, c.DNSIPv4, tc.wantSvc, tc.wantDNS)
			}
			if got := c.APIServerExtraIPs(); !slices.Equal(got, tc.wantExtra) {
				t.Errorf("APIServerExtraIPs = %v, want %v", got, tc.wantExtra)
			}
			if dns, err := DNSIPFor(tc.svc); err != nil || dns != tc.wantDNS {
				t.Errorf("DNSIPFor(%q) = %q, %v; want %q", tc.svc, dns, err, tc.wantDNS)
			}
		})
	}
}

// A range that cannot hold the tenth address is refused at parse time, with a
// message that says why — not accepted and then broken at DNS.
func TestServiceCIDRTooSmallIsRefused(t *testing.T) {
	for _, svc := range []string{"10.43.0.0/29", "10.43.0.0/32"} {
		if _, err := Parse("10.42.0.0/16", svc); err == nil || !strings.Contains(err.Error(), "too small") {
			t.Errorf("Parse(svc=%s) err = %v; want a too-small refusal", svc, err)
		}
		if _, err := DNSIPFor(svc); err == nil {
			t.Errorf("DNSIPFor(%s) accepted a range with no tenth address", svc)
		}
	}
}

// TestDNSIPKeepsTheLegacyAddressWhereItWorked: earlier releases always
// created the kube-dns Service at 10.96.0.10. A custom range containing it ran
// fine, and its Service holds that immutable ClusterIP, so it must not move.
// Only a range that never contained it, the broken case, gets the tenth address.
func TestDNSIPKeepsTheLegacyAddressWhereItWorked(t *testing.T) {
	for _, tc := range []struct{ cidr, want string }{
		{"10.96.0.0/12", "10.96.0.10"}, // the default: both rules agree
		{"10.64.0.0/10", "10.96.0.10"}, // contains the legacy address: keep it
		{"10.96.0.0/24", "10.96.0.10"}, // contains it at the tenth address anyway
		{"10.43.0.0/16", "10.43.0.10"}, // never contained it: the derived tenth
		{"172.20.0.0/28", "172.20.0.10"},
	} {
		got, err := DNSIPFor(tc.cidr)
		if err != nil || got != tc.want {
			t.Errorf("DNSIPFor(%s) = %q, %v; want %q", tc.cidr, got, err, tc.want)
		}
	}
}

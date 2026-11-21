package pki

import (
	"slices"
	"strings"
	"testing"
)

func TestLeafSpecsServiceIP(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra []string
		want  []string // exact apiserver IP SANs, in order
	}{
		{"no extras: the default service IP", nil,
			[]string{"10.96.0.1", "127.0.0.1", "192.168.1.21"}},
		{"default range, with a VIP", []string{"10.96.0.1", "192.168.1.10"},
			[]string{"10.96.0.1", "127.0.0.1", "192.168.1.21", "192.168.1.10"}},
		{"custom range replaces the default", []string{"10.43.0.1"},
			[]string{"10.43.0.1", "127.0.0.1", "192.168.1.21"}},
		{"custom range, with a VIP", []string{"10.43.0.1", "192.168.1.10"},
			[]string{"10.43.0.1", "127.0.0.1", "192.168.1.21", "192.168.1.10"}},
		{"dual-stack extras", []string{"10.43.0.1", "fd00:43::1", "::1"},
			[]string{"10.43.0.1", "127.0.0.1", "192.168.1.21", "fd00:43::1", "::1"}},
		{"IPv6-only extras keep the default IPv4 service IP", []string{"fd00:96::1", "::1"},
			[]string{"10.96.0.1", "127.0.0.1", "192.168.1.21", "fd00:96::1", "::1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, s := range LeafSpecs("cp1", "192.168.1.21", &APISANs{IPs: tc.extra}) {
				if s.Name != "apiserver" {
					continue
				}
				if !slices.Equal(s.IPs, tc.want) {
					t.Errorf("apiserver IP SANs = %s\nwant               %s", strings.Join(s.IPs, " "), strings.Join(tc.want, " "))
				}
				return
			}
			t.Fatal("no apiserver leaf")
		})
	}
}

func TestAPIServerCertCarriesTheClusterDomain(t *testing.T) {
	for _, tc := range []struct {
		name string
		sans *APISANs
		want string
	}{
		{"nil", nil, "kubernetes.default.svc.cluster.local"},
		{"empty domain", &APISANs{}, "kubernetes.default.svc.cluster.local"},
		{"custom", &APISANs{Domain: "fuzz.internal"}, "kubernetes.default.svc.fuzz.internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, s := range LeafSpecs("cp1", "192.168.1.21", tc.sans) {
				if s.Name != "apiserver" {
					continue
				}
				if !slices.Contains(s.DNS, tc.want) {
					t.Errorf("apiserver DNS SANs = %v, want %s", s.DNS, tc.want)
				}
				for _, d := range s.DNS {
					if d != tc.want && strings.HasPrefix(d, "kubernetes.default.svc.") {
						t.Errorf("apiserver DNS SANs carry a second domain %s", d)
					}
				}
				return
			}
			t.Fatal("no apiserver leaf")
		})
	}
}

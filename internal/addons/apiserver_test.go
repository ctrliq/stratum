package addons

import (
	"slices"
	"strings"
	"testing"
)

// TestAPIServerForIsLeaderIndependent pins leader-independent apiserver
// settings. The deployer runs on whichever control plane holds the operator
// lease, so a render that bakes in that node's own IP would re-point every
// agent and roll the DaemonSet in the middle of the failover that caused it.
// Given the same cluster, every leader must produce the same settings.
func TestAPIServerForIsLeaderIndependent(t *testing.T) {
	cps := []string{"192.168.99.13", "192.168.99.12", "192.168.99.14"}
	want := APIServerFor("", "192.168.99.12", cps)
	for i, self := range cps {
		// Each leader lists the Nodes in whatever order the apiserver returns.
		shuffled := append(slices.Clone(cps[i:]), cps[:i]...)
		if got := APIServerFor("", self, shuffled); got.Host != want.Host || !slices.Equal(got.URLs, want.URLs) {
			t.Errorf("leader %s renders %+v, leader 192.168.99.12 renders %+v", self, got, want)
		}
	}
	if want.Host != "192.168.99.12" {
		t.Errorf("Host = %q, want the lowest control plane", want.Host)
	}
	if !slices.Equal(want.URLs, []string{"https://192.168.99.12:6443", "https://192.168.99.13:6443", "https://192.168.99.14:6443"}) {
		t.Errorf("URLs = %q", want.URLs)
	}
}

func TestAPIServerFor(t *testing.T) {
	for _, tc := range []struct {
		name, vip, self string
		cps             []string
		want            APIServer
	}{
		{"a VIP wins, alone", "10.0.0.10", "10.0.0.11", []string{"10.0.0.11", "10.0.0.12"}, APIServer{Host: "10.0.0.10"}},
		{"one control plane: no list", "", "10.0.0.11", []string{"10.0.0.11"}, APIServer{Host: "10.0.0.11"}},
		{"nothing labelled yet: this node", "", "10.0.0.11", nil, APIServer{Host: "10.0.0.11"}},
		{"numeric order, not string order", "", "10.0.0.9", []string{"10.0.0.10", "10.0.0.9"},
			APIServer{Host: "10.0.0.9", URLs: []string{"https://10.0.0.9:6443", "https://10.0.0.10:6443"}}},
		{"duplicates collapse", "", "", []string{"10.0.0.11", "10.0.0.11"}, APIServer{Host: "10.0.0.11"}},
		{"IPv6 is bracketed", "", "", []string{"fd00::2", "fd00::1"},
			APIServer{Host: "fd00::1", URLs: []string{"https://[fd00::1]:6443", "https://[fd00::2]:6443"}}},
		{"nothing to render with", "", "", nil, APIServer{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := APIServerFor(tc.vip, tc.self, tc.cps)
			if got.Host != tc.want.Host || !slices.Equal(got.URLs, tc.want.URLs) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestFillAPIServerWithOneAddressRendersEmptyURLList: a VIP or single-node
// cluster renders the empty URL list the chart produces with
// k8s.apiServerURLs unset, so upgrading one rolls nothing.
func TestFillAPIServerWithOneAddressRendersEmptyURLList(t *testing.T) {
	out := string(FillAPIServer(RenderCilium(false, CiliumSettings{}), APIServer{Host: "10.0.0.10"}))
	for _, want := range []string{`k8s-api-server-urls: ""`, `- "--k8s-api-server-urls="`} {
		if !strings.Contains(out, want) {
			t.Errorf("single-address render lacks %s", want)
		}
	}
}

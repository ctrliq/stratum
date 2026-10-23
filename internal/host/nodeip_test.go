package host

import (
	"errors"
	"strings"
	"testing"
)

// TestResolveNodeIPPrecedence pins the precedence: flag, saved state, config,
// $NODE_IP, then detection — the first non-empty source wins, and detection
// is never consulted while an explicit source exists (a node with a pinned
// address on an air-gapped network must not need a route to 8.8.8.8).
//
// state outranks config and $NODE_IP: the reboot-persistence
// unit's ExecStart carries no --node-ip, so on restart, state is the only
// source that can still name the address this node's etcd membership and
// certificates actually recorded — config/$NODE_IP may have moved on for
// reasons that have nothing to do with this node.
func TestResolveNodeIPPrecedence(t *testing.T) {
	noDetect := func() (string, error) {
		t.Helper()
		t.Error("detect was called although an explicit source was set")
		return "", nil
	}
	for _, tc := range []struct {
		name, flag, config, env, state string
		want, source                   string
	}{
		{"flag beats everything", "10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.1", "--node-ip"},
		{"state beats config and env: a restart keeps its address", "", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.4", "saved node state"},
		{"config beats env when there is no saved state (first boot)", "", "10.0.0.2", "10.0.0.3", "", "10.0.0.2", "spec.network.nodeIP"},
		{"env when nothing else explicit", "", "", "10.0.0.3", "", "10.0.0.3", "$NODE_IP"},
		{"dual-stack values pass through verbatim", "", "10.0.0.2,fd00::2", "", "", "10.0.0.2,fd00::2", "spec.network.nodeIP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NODE_IP", tc.env)
			got, err := ResolveNodeIP(tc.flag, tc.config, tc.state, noDetect)
			if err != nil {
				t.Fatal(err)
			}
			if got.IP != tc.want || got.Source != tc.source {
				t.Errorf("got %+v, want {%s %s}", got, tc.want, tc.source)
			}
		})
	}
}

// Detection is the last resort, and a failure names the explicit fixes.
func TestResolveNodeIPFallsBackToDetection(t *testing.T) {
	t.Setenv("NODE_IP", "")
	got, err := ResolveNodeIP("", "", "", func() (string, error) { return "192.0.2.7", nil })
	if err != nil || got.IP != "192.0.2.7" || !strings.Contains(got.Source, "detected") {
		t.Fatalf("got %+v, %v", got, err)
	}
	_, err = ResolveNodeIP("", "", "", func() (string, error) { return "", errors.New("no route") })
	if err == nil {
		t.Fatal("a detection failure was swallowed")
	}
	for _, want := range []string{"no route", "--node-ip", "spec.network.nodeIP", "$NODE_IP"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("detection error missing %q: %v", want, err)
		}
	}
}

// A start that does not read spec.network.nodeIP keeps the precedence every
// other start uses, without its config step: flag, saved state, $NODE_IP, then
// detection, which is never consulted while an explicit source exists.
func TestResolveNodeIPWithoutConfigPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, flag, state, env string
		detected               string // what detection answers; "" fails the test if it is called
		want, source           string
	}{
		{name: "flag beats state and env", flag: "10.0.0.1", state: "10.0.0.4", env: "10.0.0.3", want: "10.0.0.1", source: "--node-ip"},
		{name: "state beats env: a restart keeps its address", state: "10.0.0.4", env: "10.0.0.3", want: "10.0.0.4", source: "saved node state"},
		{name: "env when nothing else explicit", env: "10.0.0.3", want: "10.0.0.3", source: "$NODE_IP"},
		{name: "dual-stack values pass through verbatim", state: "10.0.0.4,fd00::4", want: "10.0.0.4,fd00::4", source: "saved node state"},
		{name: "detection when nothing is explicit", detected: "192.0.2.7", want: "192.0.2.7", source: "autodetected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NODE_IP", tc.env)
			detect := func() (string, error) {
				if tc.detected == "" {
					t.Error("detect was called although an explicit source was set")
				}
				return tc.detected, nil
			}
			got, err := ResolveNodeIPWithoutConfig(tc.flag, tc.state, detect)
			if err != nil {
				t.Fatal(err)
			}
			if got.IP != tc.want || got.Source != tc.source {
				t.Errorf("got %+v, want {%s %s}", got, tc.want, tc.source)
			}
		})
	}
}

// A detection failure tells the operator how to pin the address, so it names
// exactly the explicit sources the start reads: a start that does not read
// spec.network.nodeIP must not send the operator to it. Saved node state is
// never named, since an operator cannot pin it. The hint of a start that reads
// every source stays word for word what it was, and a lookup offered no source
// an operator can set returns the detection failure alone.
func TestADetectionFailureNamesOnlyTheSourcesItsStartReads(t *testing.T) {
	t.Setenv("NODE_IP", "")
	noRoute := errors.New("no route")
	detect := func() (string, error) { return "", noRoute }
	for _, tc := range []struct {
		name    string
		resolve func() (NodeIPChoice, error)
		want    string // the whole error
	}{
		{
			name:    "a start that reads every source",
			resolve: func() (NodeIPChoice, error) { return ResolveNodeIP("", "", "", detect) },
			want:    "no route (pin the node IP with --node-ip, spec.network.nodeIP or $NODE_IP)",
		},
		{
			name:    "a start that does not read spec.network.nodeIP",
			resolve: func() (NodeIPChoice, error) { return ResolveNodeIPWithoutConfig("", "", detect) },
			want:    "no route (pin the node IP with --node-ip or $NODE_IP)",
		},
		{
			name:    "a lookup with no source an operator can set",
			resolve: func() (NodeIPChoice, error) { return resolveNodeIP([]NodeIPChoice{{"", savedNodeState}}, detect) },
			want:    "no route",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.resolve()
			if err == nil {
				t.Fatalf("a detection failure was swallowed: got %+v", got)
			}
			if !errors.Is(err, noRoute) {
				t.Errorf("the error %q does not wrap the detection failure", err)
			}
			if err.Error() != tc.want {
				t.Errorf("the error is %q, want %q", err, tc.want)
			}
		})
	}
}

// CheckNodeIP accepts what kubelet --node-ip registers for a node of the given
// stack, in the form every start path saves -- an IPv4 address on a
// single-stack node, since a cluster's primary family is IPv4, and the IPv4
// then the IPv6 on a dual-stack one -- and refuses everything else, naming the
// value it refused. It also refuses a loopback, all-zeros, multicast or
// link-local address, which the kubelet never registers as a node IP on any
// host, and the broadcast address, which names every host on the link rather
// than one node; an IPv4-mapped IPv6 is judged as the IPv4 it names.
func TestCheckNodeIPAcceptsOnlyAnAddressForTheNodesStack(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ip        string
		dualStack bool
		want      string // "" accepts; otherwise a substring of the refusal
	}{
		{name: "one IPv4 on a single-stack node", ip: "10.0.0.1"},
		{name: "one IPv6 on a single-stack node", ip: "fd00::1", want: "a single-stack node's IP is an IPv4 address"},
		{name: "an IPv4-mapped IPv6 on a single-stack node", ip: "::ffff:10.0.0.1", want: "a single-stack node's IP is an IPv4 address"},
		{name: "a loopback address", ip: "127.0.0.1", want: `"127.0.0.1" is a loopback address, which the kubelet does not register as a node IP`},
		{name: "the all-zeros address", ip: "0.0.0.0", want: `"0.0.0.0" is the all-zeros address, which the kubelet does not register as a node IP`},
		{name: "an IPv4-mapped all-zeros address", ip: "::ffff:0.0.0.0", want: `"::ffff:0.0.0.0" is the all-zeros address, which the kubelet does not register as a node IP`},
		{name: "a multicast address", ip: "224.0.0.1", want: `"224.0.0.1" is a multicast address, which the kubelet does not register as a node IP`},
		// 224.0.0.1 is link-local multicast. These two rows pin that a
		// multicast address of any scope is refused: it names a group of
		// hosts rather than one node, however far its scope reaches.
		{name: "an administratively scoped multicast address", ip: "239.1.1.1", want: `"239.1.1.1" is a multicast address, which the kubelet does not register as a node IP`},
		{name: "a pair with a global-scope multicast IPv6", ip: "10.0.0.1,ff0e::1", dualStack: true, want: `"ff0e::1" is a multicast address, which the kubelet does not register as a node IP`},
		{name: "a link-local address", ip: "169.254.1.1", want: `"169.254.1.1" is a link-local address, which the kubelet does not register as a node IP`},
		{name: "the broadcast address", ip: "255.255.255.255", want: `"255.255.255.255" is the broadcast address, which names every host on the link rather than one node`},
		{name: "a pair with a loopback IPv6", ip: "10.0.0.1,::1", dualStack: true, want: `"::1" is a loopback address, which the kubelet does not register as a node IP`},
		{name: "a pair with a link-local IPv6", ip: "10.0.0.1,fe80::1", dualStack: true, want: `"fe80::1" is a link-local address, which the kubelet does not register as a node IP`},
		{name: "a pair of all-zeros addresses", ip: "0.0.0.0,::", dualStack: true, want: `"0.0.0.0" is the all-zeros address, which the kubelet does not register as a node IP`},
		// The kind is judged on the unmapped address, before the pair's order,
		// so a mapped broadcast is refused as the broadcast it names.
		{name: "a pair with an IPv4-mapped broadcast IPv6", ip: "10.0.0.1,::ffff:255.255.255.255", dualStack: true, want: `"::ffff:255.255.255.255" is the broadcast address, which names every host on the link rather than one node`},
		{name: "the pair on a dual-stack node", ip: "10.0.0.1,fd00::1", dualStack: true},
		{name: "an address that does not parse", ip: "10.0.0.300", want: `"10.0.0.300" is not an IP address`},
		{name: "an address padded with a space", ip: " 10.0.0.1", want: `" 10.0.0.1" is not an IP address`},
		{name: "no address", ip: "", want: `"" is not an IP address`},
		{name: "an address with an interface zone", ip: "fe80::1%eth0", want: `"fe80::1%eth0" carries an interface zone`},
		{name: "a pair whose IPv6 carries an interface zone", ip: "10.0.0.1,fd00::1%eth0", dualStack: true, want: `"fd00::1%eth0" carries an interface zone`},
		{name: "a pair on a single-stack node", ip: "10.0.0.1,fd00::1", want: "a single-stack node takes one address"},
		{name: "only the IPv4 on a dual-stack node", ip: "10.0.0.1", dualStack: true, want: `"v4,v6" pair`},
		{name: "three addresses on a dual-stack node", ip: "10.0.0.1,fd00::1,fd00::2", dualStack: true, want: `"v4,v6" pair`},
		{name: "a pair missing its IPv6", ip: "10.0.0.1,", dualStack: true, want: `"" is not an IP address`},
		{name: "a pair whose IPv6 does not parse", ip: "10.0.0.1,fd00::zz", dualStack: true, want: `"fd00::zz" is not an IP address`},
		{name: "the IPv6 first on a dual-stack node", ip: "fd00::1,10.0.0.1", dualStack: true, want: "IPv4 first"},
		{name: "two IPv4 on a dual-stack node", ip: "10.0.0.1,10.0.0.2", dualStack: true, want: "IPv4 first"},
		{name: "two IPv6 on a dual-stack node", ip: "fd00::1,fd00::2", dualStack: true, want: "IPv4 first"},
		{name: "an IPv4-mapped IPv6 as the pair's IPv6", ip: "10.0.0.1,::ffff:10.0.0.2", dualStack: true, want: "IPv4 first"},
		{name: "an IPv4-mapped IPv6 as the pair's IPv4", ip: "::ffff:10.0.0.1,fd00::1", dualStack: true, want: "IPv4 first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckNodeIP(tc.ip, tc.dualStack)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("CheckNodeIP(%q, dual-stack %t) = %v, want it accepted", tc.ip, tc.dualStack, err)
			case tc.want != "" && err == nil:
				t.Errorf("CheckNodeIP(%q, dual-stack %t) accepted it, want a refusal naming %s", tc.ip, tc.dualStack, tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Errorf("CheckNodeIP(%q, dual-stack %t) = %q, want it to name %s", tc.ip, tc.dualStack, err, tc.want)
			}
		})
	}
}

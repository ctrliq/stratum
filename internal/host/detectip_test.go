package host

import (
	"errors"
	"net"
	"regexp"
	"strings"
	"testing"
)

func cidr(t *testing.T, s string) net.Addr {
	t.Helper()
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	n.IP = ip
	return n
}

func TestDetectIPv4NoDefaultRoute(t *testing.T) {
	for _, tc := range []struct {
		name    string
		addrs   []string
		want    string // address, or "" when an error is expected
		errHas  []string
		listErr error
	}{
		{name: "single NIC, static address", addrs: []string{"127.0.0.1/8", "192.168.1.229/24", "fe80::1/64"}, want: "192.168.1.229"},
		{name: "ignores IPv6 and link-local", addrs: []string{"169.254.1.1/16", "fd00::2/64", "10.0.0.5/24"}, want: "10.0.0.5"},
		{name: "two candidates is ambiguous", addrs: []string{"10.0.0.5/24", "172.17.0.1/16"},
			errHas: []string{"default route", "10.0.0.5", "172.17.0.1"}},
		{name: "no candidates", addrs: []string{"127.0.0.1/8", "::1/128"},
			errHas: []string{"default route", "no global unicast"}},
		{name: "interface listing fails", listErr: errors.New("boom"),
			errHas: []string{"default route", "boom"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialUDP = func(string, string) (net.Conn, error) {
				return nil, errors.New("connect: network is unreachable")
			}
			interfaceAddrs = func() ([]net.Addr, error) {
				var out []net.Addr
				for _, a := range tc.addrs {
					out = append(out, cidr(t, a))
				}
				return out, tc.listErr
			}
			t.Cleanup(func() { dialUDP, interfaceAddrs = net.Dial, net.InterfaceAddrs })

			got, err := detectIPv4()
			if tc.want != "" {
				if err != nil || got != tc.want {
					t.Fatalf("got %q, %v; want %q", got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("got %q, want an error", got)
			}
			for _, w := range tc.errHas {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
			for _, source := range []string{"--node-ip", "spec.network.nodeIP", "NODE_IP"} {
				if strings.Contains(err.Error(), source) {
					t.Errorf("error %q names %s, a source only its caller knows it reads", err, source)
				}
			}
		})
	}
}

func TestAFailedDetectionNamesEachSourceOnlyInItsCallersHint(t *testing.T) {
	t.Setenv("NODE_IP", "")
	detect := func() (string, error) { return DetectNodeIPs(false) }
	detectDualStack := func() (string, error) { return DetectNodeIPs(true) }
	for _, caller := range []struct {
		name    string
		resolve func() (string, error)
		hint    string // the whole hint the caller appends
		dual    bool   // whether it detects a node IPv6 as well
	}{
		{
			name: "a start that does not read spec.network.nodeIP",
			resolve: func() (string, error) {
				c, err := ResolveNodeIPWithoutConfig("", "", detect)
				return c.IP, err
			},
			hint: "(pin the node IP with --node-ip or $NODE_IP)",
		},
		{
			name: "a start that reads every source",
			resolve: func() (string, error) {
				c, err := ResolveNodeIP("", "", "", detect)
				return c.IP, err
			},
			hint: "(pin the node IP with --node-ip, spec.network.nodeIP or $NODE_IP)",
		},
		{name: "NodeIP, which reads only $NODE_IP", resolve: NodeIP, hint: "(pin the node IP with $NODE_IP)"},
		{
			name:    "NodeIPs, which reads only $NODE_IP",
			resolve: func() (string, error) { return NodeIPs(false) },
			hint:    "(pin the node IP with $NODE_IP)",
		},
		{
			name: "a dual-stack start that does not read spec.network.nodeIP",
			resolve: func() (string, error) {
				c, err := ResolveNodeIPWithoutConfig("", "", detectDualStack)
				return c.IP, err
			},
			hint: "(pin the node IP with --node-ip or $NODE_IP)",
			dual: true,
		},
		{
			name: "a dual-stack start that reads every source",
			resolve: func() (string, error) {
				c, err := ResolveNodeIP("", "", "", detectDualStack)
				return c.IP, err
			},
			hint: "(pin the node IP with --node-ip, spec.network.nodeIP or $NODE_IP)",
			dual: true,
		},
		{
			name:    "NodeIPs under dual-stack, which reads only $NODE_IP",
			resolve: func() (string, error) { return NodeIPs(true) },
			hint:    "(pin the node IP with $NODE_IP)",
			dual:    true,
		},
	} {
		t.Run(caller.name, func(t *testing.T) {
			for _, failure := range []struct {
				name    string
				addrs   []string
				listErr error
				dual    bool     // only a caller that detects a node IPv6 as well fails
				says    []string // patterns the detector's own reason still matches
			}{
				{name: "no address to fall back on", addrs: []string{"127.0.0.1/8", "::1/128"}},
				{name: "interface listing fails", listErr: errors.New("boom")},
				{
					name:  "dual stack with no routable IPv6 beside the fallback IPv4",
					addrs: []string{"10.0.0.5/24", "fe80::1/64", "::1/128"},
					dual:  true,
					says:  []string{`no routable node IPv6`, `<?v4>?,<?v6>?`},
				},
			} {
				if failure.dual && !caller.dual {
					continue
				}
				t.Run(failure.name, func(t *testing.T) {
					dialUDP = func(string, string) (net.Conn, error) {
						return nil, errors.New("connect: network is unreachable")
					}
					interfaceAddrs = func() ([]net.Addr, error) {
						var out []net.Addr
						for _, a := range failure.addrs {
							out = append(out, cidr(t, a))
						}
						return out, failure.listErr
					}
					t.Cleanup(func() { dialUDP, interfaceAddrs = net.Dial, net.InterfaceAddrs })

					got, err := caller.resolve()
					if err == nil {
						t.Fatalf("got %q, want an error", got)
					}
					if !strings.HasSuffix(err.Error(), " "+caller.hint) {
						t.Errorf("the error %q does not end with the hint %q", err, caller.hint)
					}
					for _, source := range []string{"--node-ip", "spec.network.nodeIP", "NODE_IP"} {
						if n, want := strings.Count(err.Error(), source), strings.Count(caller.hint, source); n != want {
							t.Errorf("the error %q names %s %d times, want %d, as often as its caller's hint %q does", err, source, n, want, caller.hint)
						}
					}
					for _, pattern := range failure.says {
						if !regexp.MustCompile(pattern).MatchString(err.Error()) {
							t.Errorf("the error %q does not match %q", err, pattern)
						}
					}
				})
			}
		})
	}
}

func TestNodeIPv6IsTheFirstRoutableIPv6OfTheListedInterfaces(t *testing.T) {
	for _, tc := range []struct {
		name  string
		addrs []string
		want  string
	}{
		{name: "a ULA beside loopback and link-local", addrs: []string{"::1/128", "fe80::1/64", "10.0.0.5/24", "fd00::5/64"}, want: "fd00::5"},
		{name: "the first of a global address and a ULA", addrs: []string{"10.0.0.5/24", "2001:db8::5/64", "fd00::5/64"}, want: "2001:db8::5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			interfaceAddrs = func() ([]net.Addr, error) {
				var out []net.Addr
				for _, a := range tc.addrs {
					out = append(out, cidr(t, a))
				}
				return out, nil
			}
			t.Cleanup(func() { interfaceAddrs = net.InterfaceAddrs })

			if got, err := nodeIPv6(); err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

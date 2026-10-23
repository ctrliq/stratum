package runtime

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// upstreamFixture points the resolver paths and version dirs at temp dirs and
// returns a writer for the two node files ("" leaves one absent).
func upstreamFixture(t *testing.T) func(host, systemd string) {
	t.Helper()
	oldHost, oldSystemd := hostResolvConf, systemdResolvConf
	oldPKI, oldConf, oldDNS := version.PKIDir, version.K8sConfDir, version.CoreDNSUpstreamDir
	t.Cleanup(func() {
		hostResolvConf, systemdResolvConf = oldHost, oldSystemd
		version.PKIDir, version.K8sConfDir, version.CoreDNSUpstreamDir = oldPKI, oldConf, oldDNS
	})
	return func(host, systemd string) {
		t.Helper()
		dir := t.TempDir()
		version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()
		version.CoreDNSUpstreamDir = filepath.Join(t.TempDir(), "dns")
		hostResolvConf = filepath.Join(dir, "etc-resolv.conf")
		systemdResolvConf = filepath.Join(dir, "systemd-resolv.conf")
		for p, body := range map[string]string{hostResolvConf: host, systemdResolvConf: systemd} {
			if body == "" {
				continue
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func readUpstream(t *testing.T) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(CoreDNSUpstream())
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

func TestChooseCoreDNSUpstream(t *testing.T) {
	setup := upstreamFixture(t)
	choose := func(t *testing.T, host, systemd string, st ...nodestate.NodeState) string {
		t.Helper()
		setup(host, systemd)
		node := nodestate.NodeState{Role: "worker", Hostname: "w1"}
		if len(st) > 0 {
			node = st[0]
		}
		c, err := chooseCoreDNSUpstream(podIPv6(&node))
		if err != nil {
			t.Fatal(err)
		}
		return c.report()
	}
	sub := func(s string) string {
		return strings.NewReplacer("HOST", hostResolvConf, "SYSTEMD", systemdResolvConf, "UPSTREAM", CoreDNSUpstream()).Replace(s)
	}
	const stub = "# This is /run/systemd/resolve/stub-resolv.conf\nnameserver 127.0.0.53\noptions edns0 trust-ad\nsearch ec2.internal corp.example\n"

	t.Run("a node whose nameservers a pod reaches writes nothing", func(t *testing.T) {
		setup("", "")
		if err := os.MkdirAll(version.CoreDNSUpstreamDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(CoreDNSUpstream(), []byte("nameserver 8.8.8.8\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(hostResolvConf, []byte("search ec2.internal\nnameserver 10.60.0.2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := chooseCoreDNSUpstream(true, "")
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := readUpstream(t); ok {
			t.Errorf("a stale upstream survived, %q: CoreDNS would forward through it rather than the node's own resolv.conf", got)
		}
		if r := c.report(); r != "" {
			t.Errorf("the node's own resolv.conf reported %q, want nothing", r)
		}
	})

	t.Run("the systemd-resolved stub defers to resolved's upstream file", func(t *testing.T) {
		r := choose(t, stub, "nameserver 10.60.0.2\nsearch ec2.internal\n")
		if got, _ := readUpstream(t); got != "nameserver 10.60.0.2\nsearch ec2.internal\n" {
			t.Errorf("upstream = %q", got)
		}
		if want := sub("stratum: CoreDNS forwards through UPSTREAM (10.60.0.2, from SYSTEMD); left out 127.0.0.53 from HOST: loopback\n"); r != want {
			t.Errorf("report = %q, want %q", r, want)
		}
	})

	for _, tc := range []struct {
		name, host, systemd, want, report string
	}{
		{"a mixed /etc/resolv.conf", "nameserver 127.0.0.53\nnameserver 192.168.104.2\nnameserver fe80::1%eth0\nsearch corp.example\n", "",
			"nameserver 192.168.104.2\nsearch corp.example\n",
			"stratum: CoreDNS forwards through UPSTREAM (192.168.104.2, from HOST); left out 127.0.0.53 from HOST: loopback; fe80::1%eth0 from HOST: IPv6 link-local\n"},
		{"a mixed resolved file", stub, "nameserver 10.60.0.2\nnameserver ::1\nnameserver 10.60.0.3\nsearch ec2.internal\noptions ndots:2\n",
			"nameserver 10.60.0.2\nnameserver 10.60.0.3\nsearch ec2.internal\noptions ndots:2\n",
			"stratum: CoreDNS forwards through UPSTREAM (10.60.0.2 10.60.0.3, from SYSTEMD); left out 127.0.0.53 from HOST: loopback; ::1 from SYSTEMD: loopback\n"},
	} {
		t.Run(tc.name+" keeps only the reachable servers", func(t *testing.T) {
			r := choose(t, tc.host, tc.systemd)
			if got, _ := readUpstream(t); got != tc.want {
				t.Errorf("upstream = %q, want %q", got, tc.want)
			}
			if want := sub(tc.report); r != want {
				t.Errorf("report = %q, want %q", r, want)
			}
		})
	}

	for name, tc := range map[string]struct{ systemd, left string }{
		"no resolved file":         {"", "SYSTEMD: not present"},
		"a loopback resolved file": {"nameserver 127.0.0.1\nnameserver ::1\n", "127.0.0.1 from SYSTEMD: loopback; ::1 from SYSTEMD: loopback"},
		"an empty-of-servers file": {"search corp.example\n", ""},
	} {
		t.Run("a loopback-only node with "+name+" falls back to the public pair", func(t *testing.T) {
			r := choose(t, stub, tc.systemd)
			if got, _ := readUpstream(t); got != "nameserver 8.8.8.8\nnameserver 1.1.1.1\noptions edns0 trust-ad\nsearch ec2.internal corp.example\n" {
				t.Errorf("upstream = %q: the public pair, and the node's search and options but not its loopback nameserver", got)
			}
			left := "; left out 127.0.0.53 from HOST: loopback"
			if tc.left != "" {
				left += "; " + tc.left
			}
			want := sub("  WARN: neither HOST nor SYSTEMD names a nameserver a pod can reach" + left +
				". CoreDNS forwards names outside the cluster through 8.8.8.8 and 1.1.1.1, which cannot see private or site DNS: give the node a nameserver pods can reach and restart stratum\n")
			if r != want {
				t.Errorf("report = %q, want %q", r, want)
			}
		})
	}

	t.Run("an IPv6-only resolver is used only where pods have IPv6", func(t *testing.T) {
		const v6 = "nameserver 2001:db8::53\nsearch corp.example\n"
		for _, st := range []nodestate.NodeState{
			{Role: "controller", Hostname: "cp1", DualStack: true, PodCIDR: "10.245.0.0/16,fd00:10:245::/56", ServiceCIDR: "10.96.0.0/12,fd00:10:96::/112"},
			{Role: "worker", Hostname: "w1", PodCIDR: "10.245.0.0/16,fd00:10:245::/56", ServiceCIDR: "10.96.0.0/12,fd00:10:96::/112"},
		} {
			choose(t, v6, "", st)
			if got, ok := readUpstream(t); ok {
				t.Errorf("%+v: a dual-stack pod reaches 2001:db8::53, but an upstream was written: %q", st, got)
			}
		}
		r := choose(t, v6, "", nodestate.NodeState{Role: "worker", Hostname: "w1", DualStack: true, PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12"})
		if want := "2001:db8::53 from " + hostResolvConf + ": IPv6, and pods here have no IPv6"; !strings.Contains(r, want) {
			t.Errorf("report = %q, want it to contain %q", r, want)
		}
		r = choose(t, v6, "")
		if want := "2001:db8::53 from " + hostResolvConf + ": IPv6, and this node recorded no cluster ranges, so its pods count as IPv4-only"; !strings.Contains(r, want) {
			t.Errorf("report = %q, want it to contain %q", r, want)
		}
	})

	t.Run("an unreadable file is named with its error", func(t *testing.T) {
		setup(stub, "")
		if err := os.Chmod(hostResolvConf, 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := os.ReadFile(hostResolvConf); err == nil {
			t.Skip("running as root: an unreadable file cannot be made")
		}
		c, err := chooseCoreDNSUpstream(false, "")
		if err != nil {
			t.Fatal(err)
		}
		if want := hostResolvConf + ": open " + hostResolvConf + ": permission denied"; !strings.Contains(c.report(), want) {
			t.Errorf("report = %q, want it to contain %q", c.report(), want)
		}
	})
}

// TestKubeletKeepsTheNodesResolvConf: the kubelet hands every pod
// with dnsPolicy Default, host-network ones included, the node's own
// resolv.conf. Only CoreDNS forwards through a chosen upstream, so kubelet.conf
// names no resolvConf, and one written by an earlier release is dropped on the next render.
func TestKubeletKeepsTheNodesResolvConf(t *testing.T) {
	setup := upstreamFixture(t)
	setup("nameserver 127.0.0.53\n", "")
	p := filepath.Join(version.K8sConfDir, "kubelet.conf")
	if err := os.WriteFile(p, []byte("resolvConf: /var/lib/stratum/config/resolv.conf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeKubeletConfig(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), "resolvConf") {
		t.Errorf("kubelet.conf names a resolvConf:\n%s", b)
	}
}

func TestWriteCoreDNSUpstream(t *testing.T) {
	setup := upstreamFixture(t)
	run := func(host string) string {
		t.Helper()
		setup(host, "")
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout := os.Stdout
		os.Stdout = w
		err = WriteCoreDNSUpstream(&nodestate.NodeState{Role: "worker", Hostname: "w1"})
		os.Stdout = stdout
		w.Close()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r)
		return string(b)
	}

	if out := run("nameserver 10.60.0.2\n"); out != "" {
		t.Errorf("the node's own resolv.conf printed %q", out)
	}
	setup("nameserver 127.0.0.53\n", "")
	legacy := filepath.Join(version.K8sConfDir, "resolv.conf")
	if err := os.WriteFile(legacy, []byte("nameserver 8.8.8.8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	err := WriteCoreDNSUpstream(&nodestate.NodeState{Role: "worker", Hostname: "w1"})
	os.Stdout = stdout
	w.Close()
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(r)
	if strings.Count(string(out), "WARN:") != 1 || !strings.HasPrefix(string(out), "  WARN: neither "+hostResolvConf) {
		t.Errorf("a loopback-only node printed %q, want the fallback WARN once", out)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the legacy kubelet resolv.conf at %s survived: stat err = %v", legacy, err)
	}
}

func TestPodReachableNameservers(t *testing.T) {
	const noV6 = "IPv6, and pods here have no IPv6"
	for _, tc := range []struct {
		body        string
		ipv6        bool
		reachable   []string
		unreachable [][2]string // address, reason
	}{
		{"nameserver 10.60.0.2\n", false, []string{"10.60.0.2"}, nil},
		{"nameserver 127.0.0.53\nnameserver 192.168.104.2\n", false, []string{"192.168.104.2"}, [][2]string{{"127.0.0.53", "loopback"}}},
		{"nameserver 2001:db8::53\n", true, []string{"2001:db8::53"}, nil},
		{"nameserver 2001:db8::53\n", false, nil, [][2]string{{"2001:db8::53", noV6}}},
		{"nameserver 169.254.169.253\n", false, []string{"169.254.169.253"}, nil},
		{"nameserver ::ffff:169.254.169.253\n", false, []string{"::ffff:169.254.169.253"}, nil},
		{"nameserver fe80::1%eth0\nnameserver fe80::2\n", true, nil, [][2]string{{"fe80::1%eth0", "IPv6 link-local"}, {"fe80::2", "IPv6 link-local"}}},
		{"nameserver 127.0.0.11\n", true, nil, [][2]string{{"127.0.0.11", "loopback"}}},
		{"nameserver ::1\n", true, nil, [][2]string{{"::1", "loopback"}}},
		{"nameserver ::ffff:127.0.0.1\n", true, nil, [][2]string{{"::ffff:127.0.0.1", "loopback"}}},
		{"nameserver 0.0.0.0\nnameserver ::\nnameserver ::ffff:0.0.0.0\n", true, nil, [][2]string{{"0.0.0.0", "unspecified"}, {"::", "unspecified"}, {"::ffff:0.0.0.0", "unspecified"}}},
		{"# nameserver 10.0.0.1\n;nameserver 10.0.0.2\n", true, nil, nil},
		{"nameserver\nsearch 10.0.0.3\n", true, nil, nil},
		{"nameserver not-an-address\n", true, nil, nil},
		{"", true, nil, nil},
	} {
		p := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, u := range tc.unreachable {
			want = append(want, u[0]+" from "+p+": "+u[1])
		}
		r, u, err := podReachableNameservers(p, tc.ipv6, noV6)
		if err != nil || !slices.Equal(r, tc.reachable) || !slices.Equal(u, want) {
			t.Errorf("podReachableNameservers(%q, ipv6=%v) = %q, %q, %v; want %q, %q", tc.body, tc.ipv6, r, u, err, tc.reachable, want)
		}
	}
	if r, u, err := podReachableNameservers(filepath.Join(t.TempDir(), "missing"), true, noV6); r != nil || u != nil || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing resolv.conf = %q, %q, %v; want nothing and a not-exist error", r, u, err)
	}
}

// TestWriteUpstreamGoesThroughTheSharedAtomicWriter: resolv.conf is a node
// file the owning-package record (internal/version) keys to internal/runtime,
// and the record's rule is that its owner writes it through the shared atomic
// writer. writeUpstream's own private CreateTemp-and-rename writer replaced
// the file whole but never fsynced it or the directory, so a power loss could
// still undo the rename or drop the contents after a "successful" write —
// and the raw-write guard (tools/nodefilewrites) cannot see it, because it
// looks for os.WriteFile. This test parses writeUpstream's body, so a revert
// to a private writer fails here instead of only in the record's claim.
func TestWriteUpstreamGoesThroughTheSharedAtomicWriter(t *testing.T) {
	src, err := os.ReadFile("corednsupstream.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	start := strings.Index(s, "func writeUpstream")
	if start < 0 {
		t.Fatal("corednsupstream.go no longer declares writeUpstream; update this test")
	}
	end := strings.Index(s[start:], "\nfunc ")
	body := s[start:]
	if end >= 0 {
		body = s[start : start+end]
	}
	for _, forbidden := range []string{"os.CreateTemp", "os.Rename"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("writeUpstream still writes %s by hand (%s): the node file goes through the shared atomic writer (internal/atomicfile), whose temp, fsync and directory sync this parse cannot see from a private writer", CoreDNSUpstream(), forbidden)
		}
	}
	if !strings.Contains(body, "atomicfile.WriteFile(") {
		t.Errorf("writeUpstream no longer calls atomicfile.WriteFile: resolv.conf must go through the shared atomic writer the owning-package record names for it")
	}
}

func TestPodIPv6(t *testing.T) {
	for _, tc := range []struct {
		st     nodestate.NodeState
		want   bool
		noIPv6 string
	}{
		{nodestate.NodeState{}, false, "IPv6, and this node recorded no cluster ranges, so its pods count as IPv4-only"},
		{nodestate.NodeState{Role: "worker", DualStack: true}, false, "IPv6, and this node recorded no cluster ranges, so its pods count as IPv4-only"},
		{nodestate.NodeState{PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12"}, false, "IPv6, and pods here have no IPv6"},
		{nodestate.NodeState{Role: "worker", DualStack: true, PodCIDR: "10.245.0.0/16"}, false, "IPv6, and pods here have no IPv6"},
		{nodestate.NodeState{Role: "controller", DualStack: true, PodCIDR: "10.245.0.0/16,fd00:10:245::/56", ServiceCIDR: "10.96.0.0/12,fd00:10:96::/112"}, true, ""},
		{nodestate.NodeState{Role: "worker", PodCIDR: "10.245.0.0/16,fd00:10:245::/56"}, true, ""},
		{nodestate.NodeState{ServiceCIDR: "10.96.0.0/12, fd00:10:96::/112"}, true, ""},
	} {
		if got, why := podIPv6(&tc.st); got != tc.want || why != tc.noIPv6 {
			t.Errorf("podIPv6(%+v) = %v, %q; want %v, %q", tc.st, got, why, tc.want, tc.noIPv6)
		}
	}
}

func TestWriteUpstreamReplacesRatherThanTruncates(t *testing.T) {
	oldDNS := version.CoreDNSUpstreamDir
	t.Cleanup(func() { version.CoreDNSUpstreamDir = oldDNS })
	version.CoreDNSUpstreamDir = filepath.Join(t.TempDir(), "dns")

	if err := writeUpstream([]string{"10.0.0.1"}, filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(CoreDNSUpstream())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeUpstream([]string{"10.0.0.2"}, filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(CoreDNSUpstream())
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the rewrite reused the file in place: a pod starting mid-write would read it empty or partial")
	}
	if b, _ := os.ReadFile(CoreDNSUpstream()); string(b) != "nameserver 10.0.0.2\n" {
		t.Errorf("upstream = %q after the rewrite", b)
	}
	if after.Mode().Perm() != 0o644 {
		t.Errorf("upstream mode = %o, want 644: CoreDNS's init container reads it as nobody", after.Mode().Perm())
	}
	entries, err := os.ReadDir(version.CoreDNSUpstreamDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("%s holds %d entries after two writes, want only resolv.conf: a temporary file was left behind", version.CoreDNSUpstreamDir, len(entries))
	}
}

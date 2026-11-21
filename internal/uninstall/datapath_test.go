package uninstall

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

// every path is in a temp dir, so nothing touches the machine the tests run on.
type datapathHost struct {
	root  string
	calls []string
}

func (h *datapathHost) path(p string) string { return filepath.Join(h.root, p) }

func (h *datapathHost) mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(h.path(d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *datapathHost) touch(t *testing.T, files ...string) {
	t.Helper()
	for _, f := range files {
		h.mkdirs(t, filepath.Dir(f))
		if err := os.WriteFile(h.path(f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *datapathHost) exists(p string) bool {
	_, err := os.Lstat(h.path(p))
	return err == nil
}

func newDatapathHost(t *testing.T, fx *hostEffects) *datapathHost {
	t.Helper()
	// sequential by swap's exception: it shortens the shared cgroupRemoveWait and cgroupProgramWait.
	h := &datapathHost{root: t.TempDir()}
	fx.netnsDir = h.path("run/netns")
	fx.ciliumRunDir = h.path("run/cilium")
	fx.bpfRoot = h.path("sys/fs/bpf")
	fx.netClassDir = h.path("sys/class/net")
	fx.cgroupRoot = h.path("sys/fs/cgroup")
	fx.legacyKubeletDir = h.path("var/lib/kubelet")
	swap(t, &cgroupRemoveWait, 0)
	swap(t, &cgroupProgramWait, 0) // a failed detach is listed again once
	fx.mountInfoPath = h.path("mountinfo")
	// any other cni-* is a host containerd's, and must be left alone.
	fx.stratumNetns = map[string]bool{h.path("run/netns/cni-aaa"): true, h.path("run/netns/cni-bbb"): true}
	fx.stratumNetnsErr = nil
	fx.netnsRecordPath = h.path("netns-record.json")
	fx.netnsEnumerated = false
	fx.containerdStateDir = h.path("var/lib/stratum/containerd") // absent unless a test makes it
	h.mkdirs(t, "proc")
	fx.procRoot = h.path("proc") // no other kubelet or runtime unless a test adds one
	// tests that need a different answer between two looks re-set fx.foreignRuntimesFn.
	fx.foreignRuntimesFn = func() ([]string, []string, error) { return foreignRuntimes(fx) }
	h.setMounts(t)
	fx.unmountFn = func(mp string) error { h.calls = append(h.calls, "umount "+mp); return nil }
	fx.runCmd = func(stdin, name string, args ...string) ([]byte, error) {
		h.calls = append(h.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		return nil, nil
	}
	return h
}

func (h *datapathHost) setMounts(t *testing.T, mounts ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("36 1 0:30 / / rw - ext4 /dev/sda1 rw\n")
	for i, m := range mounts {
		fmt.Fprintf(&b, "%d 36 0:%d / %s rw - tmpfs tmpfs rw\n", 40+i, 40+i, m)
	}
	if err := os.WriteFile(h.path("mountinfo"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func index(t *testing.T, calls []string, want string) int {
	t.Helper()
	i := slices.Index(calls, want)
	if i < 0 {
		t.Fatalf("calls = %q, missing %q", calls, want)
	}
	return i
}

func TestCiliumChainCleanup(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	dump := strings.Join([]string{
		"*nat",
		":PREROUTING ACCEPT [0:0]",
		":CILIUM_PRE_nat - [0:0]",
		":CILIUM_POST_nat - [0:0]",
		":KUBE-KUBELET-CANARY - [0:0]",
		"-A PREROUTING -m comment --comment \"cilium-feeder: CILIUM_PRE_nat\" -j CILIUM_PRE_nat",
		"-A POSTROUTING -m comment --comment \"cilium-feeder: CILIUM_POST_nat\" -j CILIUM_POST_nat",
		"-A CILIUM_POST_nat -s 10.0.0.0/24 -j ACCEPT",
		"-A CILIUM_PRE_nat -j CILIUM_POST_nat",
		"-A POSTROUTING -s 10.1.0.0/16 -j MASQUERADE",
		"COMMIT",
		"*filter",
		":INPUT ACCEPT [0:0]",
		":CILIUM_INPUT - [0:0]",
		"-A INPUT -m comment --comment \"cilium-feeder: CILIUM_INPUT\" -j CILIUM_INPUT",
		"-A INPUT -p tcp --dport 22 -j ACCEPT",
		"COMMIT",
		"",
	}, "\n")
	want := [][]string{
		{"-w", "5", "-t", "nat", "-D", "PREROUTING", "-m", "comment", "--comment", "cilium-feeder: CILIUM_PRE_nat", "-j", "CILIUM_PRE_nat"},
		{"-w", "5", "-t", "nat", "-D", "POSTROUTING", "-m", "comment", "--comment", "cilium-feeder: CILIUM_POST_nat", "-j", "CILIUM_POST_nat"},
		{"-w", "5", "-t", "filter", "-D", "INPUT", "-m", "comment", "--comment", "cilium-feeder: CILIUM_INPUT", "-j", "CILIUM_INPUT"},
		{"-w", "5", "-t", "nat", "-F", "CILIUM_PRE_nat"},
		{"-w", "5", "-t", "nat", "-F", "CILIUM_POST_nat"},
		{"-w", "5", "-t", "filter", "-F", "CILIUM_INPUT"},
		{"-w", "5", "-t", "nat", "-X", "CILIUM_PRE_nat"},
		{"-w", "5", "-t", "nat", "-X", "CILIUM_POST_nat"},
		{"-w", "5", "-t", "filter", "-X", "CILIUM_INPUT"},
	}
	got := ciliumChainCleanup(dump)
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("ciliumChainCleanup:\n%q\nwant:\n%q", got, want)
	}
	// What is left is none of Cilium's, so there is nothing more to do.
	clean := "*nat\n:PREROUTING ACCEPT [0:0]\n:KUBE-KUBELET-CANARY - [0:0]\n-A POSTROUTING -s 10.1.0.0/16 -j MASQUERADE\nCOMMIT\n"
	if got := ciliumChainCleanup(clean); len(got) != 0 {
		t.Errorf("a clean dump gave %q", got)
	}
}

func TestSplitRule(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	for rule, want := range map[string][]string{
		`INPUT -j CILIUM_INPUT`: {"INPUT", "-j", "CILIUM_INPUT"},
		`PREROUTING -m comment --comment "a b: CILIUM_X" -j CILIUM_X`: {"PREROUTING", "-m", "comment", "--comment", "a b: CILIUM_X", "-j", "CILIUM_X"},
		`INPUT -m comment --comment "say \"hi\" \\ ok" -j CILIUM_X`:   {"INPUT", "-m", "comment", "--comment", `say "hi" \ ok`, "-j", "CILIUM_X"},
		`INPUT --comment "" -j CILIUM_X`:                              {"INPUT", "--comment", "", "-j", "CILIUM_X"},
	} {
		if got := splitRule(rule); !slices.Equal(got, want) {
			t.Errorf("splitRule(%q) = %q, want %q", rule, got, want)
		}
	}
}

func TestNetworkMountsAreCNINetnsAndCiliumRunMounts(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.setMounts(t,
		h.path("run/netns"), // the directory itself is left alone
		h.path("run/netns/cni-aaa"),
		h.path("run/netns/cni-bbb"),
		h.path("run/netns/ip-netns-user"),      // an operator's namespace
		h.path("run/netns/cni-hostcontainerd"), // another containerd's cni- namespace
		h.path("run/cilium/cgroupv2"),
		h.path("run/cilium"),
		h.path("run/other/cni-ccc"),
	)
	got, err := networkMounts(fx, fx.stratumNetns, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		h.path("run/cilium/cgroupv2"),
		h.path("run/netns/cni-aaa"),
		h.path("run/netns/cni-bbb"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("networkMounts() = %q, want %q", got, want)
	}
}

func TestMissingCiliumRunDirectoryUnmountsNothing(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.setMounts(t, h.path("unrelated"))
	fx.ciliumRunDir = ""
	fx.stratumNetns = map[string]bool{}
	err := detachNetns(fx, true)
	if !errors.Is(err, errUnsetEffect) || !strings.Contains(err.Error(), "ciliumRunDir") {
		t.Errorf("detach = %v; want missing ciliumRunDir", err)
	}
	if len(h.calls) != 0 {
		t.Fatalf("empty Cilium layout selected unrelated mounts: %v", h.calls)
	}
	// an absent layout must be refused before the mount table is even read.
	fx.mountInfoPath = h.path("absent-mountinfo")
	if _, err := networkMounts(fx, nil, true); !errors.Is(err, errUnsetEffect) {
		t.Fatalf("mount selection with empty Cilium layout = %v", err)
	}
	fx.mountInfoPath = h.path("mountinfo")
	owned := h.path("run/netns/cni-owned")
	h.setMounts(t, owned, h.path("unrelated"))
	if got, err := networkMounts(fx, map[string]bool{owned: true}, false); err != nil || !slices.Equal(got, []string{owned}) {
		t.Errorf("disabled Cilium cleanup selected %v, %v; want only the owned namespace", got, err)
	}
}

func TestDetachNetworkMountsUnmountsBeforeRemovingTheFiles(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "run/netns/cni-aaa", "run/netns/cni-bbb", "run/netns/keep-me")
	h.setMounts(t, h.path("run/netns/cni-aaa"), h.path("run/netns/cni-bbb"), h.path("run/cilium/cgroupv2"))
	// an unmount takes its mount out of mountinfo, as the kernel does.
	mounted := []string{h.path("run/netns/cni-aaa"), h.path("run/netns/cni-bbb"), h.path("run/cilium/cgroupv2")}
	fx.unmountFn = func(mp string) error {
		if strings.Contains(mp, "/cni-") {
			if _, err := os.Lstat(mp); err != nil {
				t.Errorf("%s was removed before it was unmounted", mp)
			}
		}
		h.calls = append(h.calls, "umount "+mp)
		mounted = slices.DeleteFunc(mounted, func(m string) bool { return m == mp })
		h.setMounts(t, mounted...)
		return nil
	}

	if err := detachNetns(fx, true); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 3 {
		t.Errorf("calls = %q, want three unmounts", h.calls)
	}
	if h.exists("run/netns/cni-aaa") || h.exists("run/netns/cni-bbb") || !h.exists("run/netns/keep-me") {
		t.Error("want the cni-* files removed and keep-me left")
	}

	h.calls = nil
	if err := detachNetns(fx, true); err != nil || len(h.calls) != 0 {
		t.Errorf("second run = %v, calls %q; want a no-op", err, h.calls)
	}
}

func TestDetachNetworkMountsKeepsTheFileOfAMountItCouldNotDetach(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "run/netns/cni-aaa")
	h.setMounts(t, h.path("run/netns/cni-aaa"))
	fx.unmountFn = func(string) error { return errors.New("busy") }
	err := detachNetns(fx, true)
	if err == nil || !strings.Contains(err.Error(), "cni-aaa") || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("err = %v, want the stuck mount named with its reason", err)
	}
	if !h.exists("run/netns/cni-aaa") {
		t.Error("the file of a still-mounted netns was removed")
	}
}

func TestDetachNetworkMountsWithNoNetnsDir(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	if err := detachNetns(fx, true); err != nil {
		t.Fatalf("err = %v, want a no-op on a node with no %s", err, fx.netnsDir)
	}
}

func TestRemoveCiliumLinks(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	links := []string{"cilium_host", "cilium_net", "cilium_vxlan", "lxc1a2b3", "lxc1a2b3c4d5e6f", "lxc_health", "lxcbr0", "lxc1a2b3c4d5e6f7", "lxcnothex1", "eth0", "lo"}
	for _, l := range links {
		h.touch(t, "sys/class/net/"+l)
	}
	// deleting one end of a pair takes the other; the link itself goes.
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		h.calls = append(h.calls, name+" "+strings.Join(args, " "))
		peer := map[string]string{"cilium_host": "cilium_net", "cilium_net": "cilium_host"}[args[2]]
		for _, l := range []string{args[2], peer} {
			if l == "" {
				continue
			}
			if err := os.Remove(h.path("sys/class/net/" + l)); err != nil {
				return nil, errors.New("Cannot find device " + l)
			}
		}
		return nil, nil
	}

	if err := removeCiliumLinks(fx); err != nil {
		t.Fatal(err)
	}
	want := []string{"ip link delete cilium_host", "ip link delete cilium_net", "ip link delete cilium_vxlan", "ip link delete lxc1a2b3", "ip link delete lxc1a2b3c4d5e6f", "ip link delete lxc_health"}
	if !slices.Equal(h.calls, want) {
		t.Errorf("calls = %q, want %q", h.calls, want)
	}
	for _, kept := range []string{"lxcbr0", "lxc1a2b3c4d5e6f7", "lxcnothex1", "eth0", "lo"} {
		if !h.exists("sys/class/net/" + kept) {
			t.Errorf("link %s was deleted", kept)
		}
	}

	h.calls = nil
	if err := removeCiliumLinks(fx); err != nil || len(h.calls) != 0 {
		t.Errorf("second run = %v, calls %q; want a no-op", err, h.calls)
	}
}

func TestRemoveCiliumLinksReportsALinkThatStays(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/class/net/cilium_host")
	fx.runCmd = func(string, string, ...string) ([]byte, error) { return nil, errors.New("Operation not permitted") }
	if err := removeCiliumLinks(fx); err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("err = %v, want the failed delete", err)
	}
}

func TestRemoveCiliumChains(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dumps := map[string]string{
		"iptables-save":  "*filter\n:INPUT ACCEPT [0:0]\n:CILIUM_INPUT - [0:0]\n-A INPUT -j CILIUM_INPUT\n-A INPUT -p tcp --dport 22 -j ACCEPT\nCOMMIT\n",
		"ip6tables-save": "*filter\n:INPUT ACCEPT [0:0]\nCOMMIT\n",
	}
	var calls []string
	fx.runCmd = func(stdin, name string, args ...string) ([]byte, error) {
		if strings.HasSuffix(name, "-save") {
			return []byte(dumps[name]), nil
		}
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}

	if err := removeCiliumChains(fx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"iptables -w 5 -t filter -D INPUT -j CILIUM_INPUT",
		"iptables -w 5 -t filter -F CILIUM_INPUT",
		"iptables -w 5 -t filter -X CILIUM_INPUT",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}

	dumps["iptables-save"] = "*filter\n:INPUT ACCEPT [0:0]\n-A INPUT -p tcp --dport 22 -j ACCEPT\nCOMMIT\n"
	calls = nil
	if err := removeCiliumChains(fx); err != nil || len(calls) != 0 {
		t.Errorf("second run = %v, calls %q; want a no-op", err, calls)
	}
}

func TestRemoveCiliumChainsWithoutIptables(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	fx.runCmd = func(_, name string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("%s: %w", name, exec.ErrNotFound)
	}
	err := removeCiliumChains(fx)
	for _, want := range []string{"iptables-save was not found", "ip6tables-save was not found", "install iptables", "re-run"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to say %q", err, want)
		}
	}
}

func TestRemoveCiliumChainsReportsAFailedDelete(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		if name == "iptables-save" {
			return []byte("*filter\n:CILIUM_INPUT - [0:0]\nCOMMIT\n"), nil
		}
		if name == "iptables" && slices.Contains(args, "-X") {
			return nil, errors.New("iptables -X: Chain already in use")
		}
		return nil, nil
	}
	if err := removeCiliumChains(fx); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v, want the failed chain delete", err)
	}
}

func TestRemoveCiliumChainsRemovesOldCiliumChains(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dumps := map[string]string{
		"iptables-save": strings.Join([]string{
			"*filter",
			":INPUT ACCEPT [0:0]",
			":OLD_CILIUM_INPUT - [0:0]",
			`-A INPUT -m comment --comment "cilium-feeder: OLD_CILIUM_INPUT" -j OLD_CILIUM_INPUT`,
			`-A INPUT -m comment --comment "renamed to OLD_CILIUM_INPUT once" -j ACCEPT`,
			`-A OLD_CILIUM_INPUT -m comment --comment "cilium-feeder: CILIUM_INPUT" -j CILIUM_INPUT`,
			"COMMIT",
			"",
		}, "\n"),
		"ip6tables-save": strings.Join([]string{
			"*nat",
			":PREROUTING ACCEPT [0:0]",
			":OLD_CILIUM_PRE_nat - [0:0]",
			"-A PREROUTING -g OLD_CILIUM_PRE_nat",
			"COMMIT",
			"",
		}, "\n"),
	}
	var calls []string
	fx.runCmd = func(stdin, name string, args ...string) ([]byte, error) {
		if strings.HasSuffix(name, "-save") {
			return []byte(dumps[name]), nil
		}
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}

	if err := removeCiliumChains(fx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"iptables -w 5 -t filter -D INPUT -m comment --comment cilium-feeder: OLD_CILIUM_INPUT -j OLD_CILIUM_INPUT",
		"iptables -w 5 -t filter -F OLD_CILIUM_INPUT",
		"iptables -w 5 -t filter -X OLD_CILIUM_INPUT",
		"ip6tables -w 5 -t nat -D PREROUTING -g OLD_CILIUM_PRE_nat",
		"ip6tables -w 5 -t nat -F OLD_CILIUM_PRE_nat",
		"ip6tables -w 5 -t nat -X OLD_CILIUM_PRE_nat",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}

	// an empty dump, or one with none of Cilium's, runs nothing.
	dumps["iptables-save"] = ""
	dumps["ip6tables-save"] = "*filter\n:INPUT ACCEPT [0:0]\nCOMMIT\n"
	calls = nil
	if err := removeCiliumChains(fx); err != nil || len(calls) != 0 {
		t.Errorf("empty input: run = %v, calls %q; want a no-op", err, calls)
	}
	if got := ciliumChainCleanup(""); got != nil {
		t.Errorf("ciliumChainCleanup(\"\") = %q, want nil", got)
	}
}

func TestRemoveCiliumState(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/fs/bpf/tc/globals/cilium_ct4_global", "sys/fs/bpf/tc/globals/cilium_policy",
		"sys/fs/bpf/cilium/socketlb/links/cgroup/connect4", "run/cilium/cilium.sock")

	if err := removeCiliumState(fx); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"sys/fs/bpf/tc", "sys/fs/bpf/cilium", "run/cilium"} {
		if h.exists(gone) {
			t.Errorf("%s was not removed", gone)
		}
	}
	if err := removeCiliumState(fx); err != nil {
		t.Errorf("second run = %v, want a no-op", err)
	}
}

func TestRemoveCiliumStateKeepsForeignPinsInTheSharedTrees(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/fs/bpf/tc/globals/cilium_ct4_global", "sys/fs/bpf/tc/globals/foreign_map",
		"sys/fs/bpf/tc/other-app/prog", "sys/fs/bpf/xdp/foreign_xdp", "sys/fs/bpf/xdp/cilium_looking_but_in_xdp",
		"sys/fs/bpf/someone-elses-pin", "sys/fs/bpf/cilium/x")
	if err := removeCiliumState(fx); err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"sys/fs/bpf/tc/globals/foreign_map", "sys/fs/bpf/tc/other-app/prog",
		"sys/fs/bpf/xdp/foreign_xdp", "sys/fs/bpf/xdp/cilium_looking_but_in_xdp", "sys/fs/bpf/someone-elses-pin"} {
		if !h.exists(kept) {
			t.Errorf("%s was removed", kept)
		}
	}
	for _, gone := range []string{"sys/fs/bpf/tc/globals/cilium_ct4_global", "sys/fs/bpf/cilium"} {
		if h.exists(gone) {
			t.Errorf("%s was not removed", gone)
		}
	}
}

func TestRemoveCiliumStateDoesNotRemoveThroughAMount(t *testing.T) {
	for _, tc := range []struct {
		name           string
		setup          func(h *datapathHost, t *testing.T)
		bpfSubtreeGone bool // a mount elsewhere does not stop the subtree's removal; an unreadable table stops both
	}{
		{"a mount that stayed", func(h *datapathHost, t *testing.T) { h.setMounts(t, h.path("run/cilium/cgroupv2")) }, true},
		{"an unreadable table", func(h *datapathHost, t *testing.T) {
			if err := os.Remove(h.path("mountinfo")); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		setup := tc.setup
		t.Run(tc.name, func(t *testing.T) {
			fx := &hostEffects{}
			h := newDatapathHost(t, fx)
			h.touch(t, "run/cilium/cgroupv2/cgroup.controllers", "run/cilium/cilium.sock", "sys/fs/bpf/cilium/x")
			setup(h, t)
			err := removeCiliumState(fx)
			if err == nil || !strings.Contains(err.Error(), "not removing") {
				t.Fatalf("err = %v, want the refusal to remove through the mount", err)
			}
			if !h.exists("run/cilium/cgroupv2/cgroup.controllers") || !h.exists("run/cilium/cilium.sock") {
				t.Error("what is under the mount was removed")
			}
			if h.exists("sys/fs/bpf/cilium") == tc.bpfSubtreeGone {
				t.Errorf("bpffs subtree exists = %v, want %v", !tc.bpfSubtreeGone, !tc.bpfSubtreeGone)
			}
		})
	}
}

func TestCiliumTCFilters(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	dump := strings.Join([]string{
		"filter protocol all pref 1 bpf chain 0 ",
		"filter protocol all pref 1 bpf chain 0 handle 0x1 cil_from_netdev-eth0 direct-action not_in_hw id 7 tag abc jited ",
		"filter protocol ip pref 2 bpf chain 0 handle 0x2 cilium-old-style direct-action not_in_hw id 8 tag def",
		"filter protocol all pref 3 bpf chain 0 handle 0x1 other_prog direct-action not_in_hw id 9 tag 123",
		"filter parent ffff: protocol all pref 4 bpf chain 0 handle 0x3 cil_to_host direct-action",
		// a name that only contains cil_ or cilium is some other program's.
		"filter protocol all pref 5 bpf chain 0 handle 0x1 vendor_cil_probe direct-action",
		"filter protocol all pref 6 bpf chain 0 handle 0x1 acme-cilium-shim direct-action",
		"filter protocol all pref 8 bpf chain 7 handle 0x1 cil_from_netdev-eth0 direct-action",
	}, "\n")
	want := [][4]string{{"all", "1", "0x1", "0"}, {"ip", "2", "0x2", "0"}, {"all", "4", "0x3", "0"}, {"all", "8", "0x1", "7"}}
	if got := ciliumTCFilters(dump); !slices.Equal(got, want) {
		t.Errorf("ciliumTCFilters = %q, want %q", got, want)
	}
}

func TestRemoveCiliumTCFilters(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/class/net/eth0", "sys/class/net/lo")
	filters := map[string]string{
		"eth0 ingress": "filter protocol all pref 1 bpf chain 0 handle 0x1 cil_from_netdev-eth0 direct-action\nfilter protocol all pref 9 bpf chain 0 handle 0x1 foreign direct-action\n",
		"eth0 egress":  "filter protocol all pref 1 bpf chain 0 handle 0x1 cil_to_netdev-eth0 direct-action\n",
	}
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		h.calls = append(h.calls, name+" "+strings.Join(args, " "))
		switch args[1] {
		case "show":
			return []byte(filters[args[3]+" "+args[4]]), nil
		case "del":
			delete(filters, args[3]+" "+args[4])
		}
		return nil, nil
	}
	if err := removeCiliumTCFilters(fx); err != nil {
		t.Fatal(err)
	}
	var dels []string
	for _, c := range h.calls {
		if strings.HasPrefix(c, "tc filter del") {
			dels = append(dels, c)
		}
	}
	want := []string{
		"tc filter del dev eth0 ingress protocol all chain 0 pref 1 handle 0x1 bpf",
		"tc filter del dev eth0 egress protocol all chain 0 pref 1 handle 0x1 bpf",
	}
	if !slices.Equal(dels, want) {
		t.Errorf("deletes = %q, want only Cilium's filters: %q", dels, want)
	}

	h.calls = nil
	if err := removeCiliumTCFilters(fx); err != nil {
		t.Fatal(err)
	}
	for _, c := range h.calls {
		if strings.HasPrefix(c, "tc filter del") {
			t.Errorf("second run ran %q; want a no-op", c)
		}
	}
}

func TestRemoveCiliumTCFiltersTargetsChain(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/class/net/eth0")
	dumps := map[string]string{
		"eth0 ingress": "filter protocol all pref 1 bpf chain 0 handle 0x1 foreign direct-action\n" +
			"filter protocol all pref 1 bpf chain 7 handle 0x1 cil_from_netdev-eth0 direct-action\n" +
			"filter protocol ip pref 2 bpf handle 0x2 cil_to_host direct-action\n",
	}
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		h.calls = append(h.calls, name+" "+strings.Join(args, " "))
		if args[1] == "show" {
			return []byte(dumps[args[3]+" "+args[4]]), nil
		}
		return nil, nil
	}
	if err := removeCiliumTCFilters(fx); err != nil {
		t.Fatal(err)
	}
	var dels []string
	for _, c := range h.calls {
		if strings.HasPrefix(c, "tc filter del") {
			dels = append(dels, c)
		}
	}
	want := []string{
		"tc filter del dev eth0 ingress protocol all chain 7 pref 1 handle 0x1 bpf",
		// no chain in the line means chain 0.
		"tc filter del dev eth0 ingress protocol ip chain 0 pref 2 handle 0x2 bpf",
	}
	if !slices.Equal(dels, want) {
		t.Errorf("deletes = %q, want %q", dels, want)
	}
}

func TestRemoveCiliumTCFiltersWithoutTC(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/class/net/eth0")
	fx.runCmd = func(_, name string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("%s: %w", name, exec.ErrNotFound)
	}
	err := removeCiliumTCFilters(fx)
	if err == nil || !strings.Contains(err.Error(), "iproute-tc") || !strings.Contains(err.Error(), "re-run") {
		t.Fatalf("err = %v, want an actionable error: without tc the filters cannot be checked, so the node is not known to be clean", err)
	}
}

func TestRemoveCiliumTCFiltersErrors(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/class/net/eth0")
	mk := func(show, del error) func(string, string, ...string) ([]byte, error) {
		return func(_, _ string, args ...string) ([]byte, error) {
			if args[1] == "show" {
				return []byte("filter protocol all pref 1 bpf chain 0 handle 0x1 cil_from_netdev-eth0 direct-action\n"), show
			}
			return nil, del
		}
	}
	fx.runCmd = mk(nil, errors.New("RTNETLINK answers: Operation not permitted"))
	if err := removeCiliumTCFilters(fx); err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("err = %v, want the failed delete reported", err)
	}
	fx.runCmd = mk(errors.New("tc: boom"), nil)
	if err := removeCiliumTCFilters(fx); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want the failed listing on a device that exists reported", err)
	}
	fx.runCmd = mk(errors.New("Error: Cannot find specified qdisc on specified device."), nil)
	if err := removeCiliumTCFilters(fx); err != nil {
		t.Errorf("err = %v, want a device with no qdisc to count as having no filters", err)
	}
}

func TestRemoveLegacyKubeletDir(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatalf("absent: %v", err)
	}
	h.mkdirs(t, "var/lib/kubelet")
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil || h.exists("var/lib/kubelet") {
		t.Fatalf("empty: err = %v, exists = %v; want it removed", err, h.exists("var/lib/kubelet"))
	}
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatalf("second run: %v", err)
	}
	// Another kubelet's state: kept, not an error, and nothing in it is touched.
	h.touch(t, "var/lib/kubelet/pods/p1/volumes/x", "var/lib/kubelet/config.yaml")
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatalf("non-empty: %v", err)
	}
	if !h.exists("var/lib/kubelet/pods/p1/volumes/x") || !h.exists("var/lib/kubelet/config.yaml") {
		t.Error("another kubelet's state was removed")
	}
}

// a dir short enough that a socket below it fits sun_path (about 104 bytes).
func shortKubeletDir(t *testing.T, fx *hostEffects) string {
	t.Helper()
	d, err := os.MkdirTemp("", "kb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	fx.legacyKubeletDir = d
	return d
}

// a socket file nothing listens on, as a killed kubelet leaves.
func staleSocket(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
}

func TestRemoveLegacyKubeletDirRemovesStaleKubeletSockets(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dir := shortKubeletDir(t, fx)
	staleSocket(t, filepath.Join(dir, "device-plugins/kubelet.sock"))
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists (%v), want it gone with its stale sockets", dir, err)
	}
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Errorf("second run = %v, want a no-op", err)
	}
}

func TestRemoveLegacyKubeletDirKeepsPodResources(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, dir string)
		kept string
	}{
		{"stale socket", func(t *testing.T, dir string) {
			staleSocket(t, filepath.Join(dir, "pod-resources/kubelet.sock"))
		}, "pod-resources/kubelet.sock"},
		{"empty directory", func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "pod-resources"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "pod-resources"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &hostEffects{}
			newDatapathHost(t, fx)
			dir := shortKubeletDir(t, fx)
			staleSocket(t, filepath.Join(dir, "device-plugins/kubelet.sock"))
			tc.make(t, dir)
			var out bytes.Buffer
			if err := removeLegacyKubeletDir(&out, fx); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(dir, tc.kept)); err != nil {
				t.Errorf("%s was removed: %v", tc.kept, err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "device-plugins")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("device-plugins still exists (%v), want it gone with the stale kubelet.sock", err)
			}
			// and says the directory is the kubelet's default, not Stratum's kubelet root.
			if !strings.Contains(out.String(), "left "+dir+" in place") || !strings.Contains(out.String(), "the kubelet's default directory") {
				t.Errorf("output = %q, want it to say %s, the kubelet's default directory, was left in place", out.String(), dir)
			}
		})
	}
}

func TestRemoveLegacyKubeletDirKeepsWhatIsNotStale(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dir := shortKubeletDir(t, fx)

	live, err := net.Listen("unix", filepath.Join(dir, "short.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if err := os.MkdirAll(filepath.Join(dir, "device-plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a live socket at the kubelet's path: someone answers, so it is not stale.
	liveAtPath, err := net.Listen("unix", filepath.Join(dir, "device-plugins/kubelet.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer liveAtPath.Close()
	staleSocket(t, filepath.Join(dir, "device-plugins/vendor.sock")) // another plugin's

	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"device-plugins/kubelet.sock", "device-plugins/vendor.sock", "short.sock"} {
		if _, err := os.Lstat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}

	dir = shortKubeletDir(t, fx)
	if err := os.MkdirAll(filepath.Join(dir, "device-plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "device-plugins/kubelet.sock"), []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "device-plugins/kubelet.sock")); err != nil {
		t.Errorf("a regular file with the kubelet's socket name was removed: %v", err)
	}
}

func TestRemoveLegacyKubeletDirKeepsAnotherPluginsSocket(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dir := shortKubeletDir(t, fx)
	staleSocket(t, filepath.Join(dir, "device-plugins/kubelet.sock"))
	staleSocket(t, filepath.Join(dir, "device-plugins/vendor.sock"))
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "device-plugins/kubelet.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale kubelet.sock was kept: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "device-plugins/vendor.sock")); err != nil {
		t.Errorf("another plugin's socket was removed: %v", err)
	}
}

type fakeSystemd struct {
	active    bool
	stopErr   error
	queryErr  error  // what is-active answers instead, when set
	onQuery   func() // what the query does while it runs
	onStop    func() // what stopping does to the cgroup directory
	stops     int
	dirAtStop *bool // set by stop: whether the slice's directory was still there
}

type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

// answers systemctl from f and records it, passing everything else through.
func (h *datapathHost) systemd(t *testing.T, fx *hostEffects, f *fakeSystemd) {
	t.Helper()
	prev := fx.runCmd
	fx.runCmd = func(stdin, name string, args ...string) ([]byte, error) {
		if name != "systemctl" {
			return prev(stdin, name, args...)
		}
		h.calls = append(h.calls, "systemctl "+strings.Join(args, " "))
		switch args[0] {
		case "is-active":
			if f.onQuery != nil {
				f.onQuery()
			}
			if f.queryErr != nil {
				return nil, f.queryErr
			}
			if f.active {
				return nil, nil
			}
			return nil, fakeExit(3) // inactive
		case "stop":
			dir := h.exists("sys/fs/cgroup/kubepods.slice")
			f.dirAtStop = &dir
			if f.stopErr != nil {
				return nil, f.stopErr
			}
			f.stops++
			f.active = false
			if f.onStop != nil {
				f.onStop()
			}
		}
		return nil, nil
	}
}

func TestRemoveKubepodsCgroups(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	sd := &fakeSystemd{active: true}
	h.systemd(t, fx, sd)
	h.mkdirs(t,
		"sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podabc.slice/cri-containerd-1.scope",
		"sys/fs/cgroup/kubepods.slice/kubepods-besteffort.slice",
		"sys/fs/cgroup/kubepods/burstable/pod1",
		"sys/fs/cgroup/system.slice/kubepods-impostor",
	)
	if err := removeKubepodsCgroups(fx); err != nil {
		t.Fatal(err)
	}
	if h.exists("sys/fs/cgroup/kubepods.slice") || h.exists("sys/fs/cgroup/kubepods") {
		t.Error("a kubepods cgroup tree is still there")
	}
	if !h.exists("sys/fs/cgroup/system.slice/kubepods-impostor") {
		t.Error("a cgroup outside kubepods was removed")
	}
	// the slice is stopped once the tree is gone and so known to be empty.
	if sd.stops != 1 || sd.dirAtStop == nil || *sd.dirAtStop {
		t.Errorf("stops = %d, directory present at stop = %v; want one stop of a tree already removed", sd.stops, sd.dirAtStop)
	}
	if want := []string{"systemctl stop kubepods.slice"}; !slices.Equal(h.calls, want) {
		t.Errorf("calls = %q, want %q", h.calls, want)
	}

	h.calls = nil
	if err := removeKubepodsCgroups(fx); err != nil || len(h.calls) != 1 || h.calls[0] != "systemctl is-active --quiet kubepods.slice" {
		t.Errorf("second run = %v, calls %q; want only the read-only check that the unit is inactive", err, h.calls)
	}
}

func TestRemoveKubepodsCgroupsStopsAnActiveSliceWhoseDirectoryIsGone(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	sd := &fakeSystemd{active: true}
	h.systemd(t, fx, sd)
	if err := removeKubepodsCgroups(fx); err != nil {
		t.Fatal(err)
	}
	if sd.stops != 1 {
		t.Errorf("stops = %d, want the stale active unit stopped", sd.stops)
	}
}

// systemd made the directory again at the daemon-reload on a real node.
func TestRemoveKubepodsCgroupsRemovesADirectorySystemdMakesAgain(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice")
	sd := &fakeSystemd{active: true, onStop: func() { _ = os.MkdirAll(h.path("sys/fs/cgroup/kubepods.slice"), 0o755) }}
	h.systemd(t, fx, sd)
	if err := removeKubepodsCgroups(fx); err != nil {
		t.Fatal(err)
	}
	if h.exists("sys/fs/cgroup/kubepods.slice") {
		t.Error("the directory systemd made again was left")
	}
}

func TestRemoveKubepodsCgroupsDoesNotStopASliceAnotherRuntimeMayShare(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice")
	addProc(t, h, "4242", "kubelet")
	sd := &fakeSystemd{active: true}
	h.systemd(t, fx, sd)
	if err := removeKubepodsCgroups(fx); err == nil || !strings.Contains(err.Error(), "kubelet (pid 4242)") {
		t.Fatalf("err = %v, want the shared tree reported", err)
	}
	if sd.stops != 0 || !h.exists("sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice") {
		t.Errorf("stops = %d: the shared slice was stopped or its tree removed", sd.stops)
	}

	// Nor an active unit whose directory is gone.
	h = newDatapathHost(t, fx)
	addProc(t, h, "4242", "kubelet")
	sd = &fakeSystemd{active: true}
	h.systemd(t, fx, sd)
	if err := removeKubepodsCgroups(fx); err != nil || sd.stops != 0 {
		t.Errorf("err = %v, stops = %d; want a running foreign kubelet's slice left alone", err, sd.stops)
	}
}

func TestRemoveKubepodsCgroupsDoesNotStopASliceThatIsStillInUse(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/busy")
	h.touch(t, "sys/fs/cgroup/kubepods.slice/busy/stand-in-for-a-process")
	sd := &fakeSystemd{active: true}
	h.systemd(t, fx, sd)
	if err := removeKubepodsCgroups(fx); err == nil || !strings.Contains(err.Error(), "still in use") {
		t.Fatalf("err = %v, want what is still in use reported", err)
	}
	if sd.stops != 0 {
		t.Error("the slice was stopped with a cgroup still in use under it")
	}
}

func TestStopSliceErrors(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice")
	for _, e := range []error{errors.New("systemctl stop kubepods.slice: exit status 5: Unit kubepods.slice not loaded."), fmt.Errorf("systemctl: %w", exec.ErrNotFound)} {
		h.mkdirs(t, "sys/fs/cgroup/kubepods.slice")
		h.systemd(t, fx, &fakeSystemd{active: true, stopErr: e})
		if err := removeKubepodsCgroups(fx); err != nil {
			t.Errorf("err = %v for %v, want none", err, e)
		}
	}
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice")
	h.systemd(t, fx, &fakeSystemd{active: true, stopErr: errors.New("systemctl stop kubepods.slice: exit status 1: Failed to stop")})
	if err := removeKubepodsCgroups(fx); err == nil || !strings.Contains(err.Error(), "still owns the cgroups") {
		t.Errorf("err = %v, want the failed stop reported", err)
	}
}

func TestRemoveKubepodsCgroupsRunsNoSystemctlForCgroupfs(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods/burstable/pod1")
	var systemctl int
	prev := fx.runCmd
	fx.runCmd = func(stdin, name string, args ...string) ([]byte, error) {
		if name == "systemctl" && !slices.Contains(args, "kubepods.slice") {
			systemctl++
		}
		return prev(stdin, name, args...)
	}
	h.systemd(t, fx, &fakeSystemd{}) // kubepods.slice is inactive and absent
	if err := removeKubepodsCgroups(fx); err != nil || h.exists("sys/fs/cgroup/kubepods") || systemctl != 0 {
		t.Errorf("err = %v, systemctl calls for another unit = %d", err, systemctl)
	}
}

func TestRemoveKubepodsCgroupsReportsACgroupThatStays(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")
	// a regular file stands in for a busy cgroup: rmdir of its parent fails.
	h.touch(t, "sys/fs/cgroup/kubepods/pod1/busy")
	err := removeKubepodsCgroups(fx)
	if err == nil {
		t.Fatal("err = nil, want the cgroup that could not be removed")
	}
	if !h.exists("sys/fs/cgroup/kubepods/pod1") {
		t.Error("the busy cgroup was removed")
	}
}

func TestRemoveKubepodsCgroupsReportsAnUninspectableRoot(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup")
	if err := os.Symlink(h.path("sys/fs/cgroup/kubepods.slice"), h.path("sys/fs/cgroup/kubepods.slice")); err != nil {
		t.Fatal(err)
	}
	if err := removeKubepodsCgroups(fx); err == nil {
		t.Fatal("err = nil, want the stat failure: a root that cannot be inspected is not gone")
	}
}

func TestRemoveKubepodsCgroupsReportsAnUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1/container")
	if err := os.Chmod(h.path("sys/fs/cgroup/kubepods/pod1"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.path("sys/fs/cgroup/kubepods/pod1"), 0o755) })
	err := removeKubepodsCgroups(fx)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the unreadable directory reported", err)
	}
}

func TestTeardownDatapathOrder(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "run/netns/cni-aaa", "sys/class/net/cilium_host", "sys/class/net/eth0", "sys/fs/bpf/tc/globals/cilium_m", "run/cilium/x")
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/pod")
	h.setMounts(t, h.path("run/netns/cni-aaa"))
	fx.unmountFn = func(mp string) error {
		h.calls = append(h.calls, "umount "+mp)
		h.setMounts(t)
		return nil
	}
	dumps := map[string]string{"iptables-save": "*filter\n:CILIUM_INPUT - [0:0]\nCOMMIT\n", "ip6tables-save": ""}
	tcFilter := "filter protocol all pref 1 bpf chain 0 handle 0x1 cil_from_netdev-eth0 direct-action\n"
	cgroupProgs := "ID       AttachType      AttachFlags     Name\n77       connect4        multi           cil_sock4_conne\n"
	sliceUp := true
	fx.runCmd = func(stdin, name string, args ...string) ([]byte, error) {
		call := strings.TrimSpace(name + " " + strings.Join(args, " "))
		h.calls = append(h.calls, call)
		switch name {
		case "iptables-save", "ip6tables-save":
			return []byte(dumps[name]), nil
		case "iptables":
			if slices.Contains(args, "-X") {
				dumps["iptables-save"] = ""
			}
		case "ip":
			_ = os.Remove(h.path("sys/class/net/" + args[2]))
		case "systemctl":
			if args[0] == "is-active" {
				if sliceUp {
					return nil, nil
				}
				return nil, fakeExit(3)
			}
			sliceUp = false
		case "bpftool":
			if args[1] == "show" {
				return []byte(cgroupProgs), nil
			}
			cgroupProgs = ""
		case "tc":
			if args[1] == "show" && args[2] == "dev" && args[3] == "eth0" && args[4] == "ingress" {
				return []byte(tcFilter), nil
			}
			if args[1] == "del" {
				tcFilter = ""
			}
		}
		return nil, nil
	}

	if err := teardownDatapath(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	order := []string{
		"umount " + h.path("run/netns/cni-aaa"),
		"ip link delete cilium_host",
		"tc filter del dev eth0 ingress protocol all chain 0 pref 1 handle 0x1 bpf",
		"iptables -w 5 -t filter -F CILIUM_INPUT",
		"bpftool cgroup detach " + h.path("sys/fs/cgroup") + " connect4 id 77",
		"systemctl stop kubepods.slice", // after the cgroups are gone, before the daemon-reload Run does later
	}
	last := -1
	for _, want := range order {
		i := index(t, h.calls, want)
		if i <= last {
			t.Fatalf("calls = %q: %q is out of order", h.calls, want)
		}
		last = i
	}
	for _, gone := range []string{"run/netns/cni-aaa", "sys/class/net/cilium_host", "sys/fs/bpf/tc", "run/cilium", "sys/fs/cgroup/kubepods.slice"} {
		if h.exists(gone) {
			t.Errorf("%s is still there", gone)
		}
	}

	h.calls = nil
	if err := teardownDatapath(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	// the only commands a clean node sees are the read-only dumps.
	for _, c := range h.calls {
		if !strings.HasSuffix(strings.Fields(c)[0], "-save") && !strings.HasPrefix(c, "tc filter show") && !strings.HasPrefix(c, "bpftool cgroup show") && !strings.HasPrefix(c, "systemctl is-active") {
			t.Errorf("second run ran %q; want a no-op", c)
		}
	}
}

func TestTeardownDatapathRunsEveryStepAndJoinsTheErrors(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "run/netns/cni-aaa", "sys/class/net/cilium_host")
	h.mkdirs(t, "sys/fs/cgroup/kubepods")
	h.touch(t, "sys/fs/cgroup/kubepods/busy")
	h.setMounts(t, h.path("run/netns/cni-aaa"))
	fx.unmountFn = func(string) error { return errors.New("busy") }
	fx.runCmd = func(string, string, ...string) ([]byte, error) { return nil, errors.New("boom") }

	err := teardownDatapath(io.Discard, fx)
	if err == nil {
		t.Fatal("err = nil")
	}
	for _, what := range []string{"detach CNI netns", "remove Cilium links", "remove Cilium tc filters", "remove Cilium netfilter chains", "remove kubepods cgroups"} {
		if !strings.Contains(err.Error(), what) {
			t.Errorf("err = %v, want it to name %q", err, what)
		}
	}
}

func TestRunReportsADatapathFailureAndStillRemovesState(t *testing.T) {
	_, fx := runNode(t, "controller")
	var removed []string
	fx.removeAll = func(path string) error { removed = append(removed, path); return nil }
	fx.teardownDatapathFn = func() error { return errors.New("1 mounts under /run/netns could not be unmounted") }

	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "could not be unmounted") {
		t.Fatalf("runUninstall = %v, want the datapath failure", err)
	}
	for _, want := range []string{version.DataDir} {
		if !slices.Contains(removed, want) {
			t.Errorf("removed = %q, missing %q", removed, want)
		}
	}
}

func TestRunFailsOnAMissingTCAfterTheRestOfTheCleanup(t *testing.T) {
	_, fx := runNode(t, "controller")
	h := newDatapathHost(t, fx) // after runNode, so its mount table and paths win
	fx.teardownDatapathFn = func() error { return teardownDatapath(io.Discard, fx) }
	var removed []string
	fx.removeAll = func(path string) error { removed = append(removed, path); return nil }
	h.touch(t, "sys/class/net/eth0", "sys/fs/bpf/tc/globals/cilium_ct4_global", "run/cilium/cilium.sock")
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")
	iptables := "*filter\n:CILIUM_INPUT - [0:0]\nCOMMIT\n"
	fx.runCmd = func(stdin, name string, args ...string) ([]byte, error) {
		h.calls = append(h.calls, name+" "+strings.Join(args, " "))
		switch name {
		case "tc":
			return nil, fmt.Errorf("tc: %w", exec.ErrNotFound)
		case "iptables-save":
			return []byte(iptables), nil
		}
		return nil, nil
	}

	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "iproute-tc") {
		t.Fatalf("runUninstall = %v, want the missing tc reported", err)
	}
	if !slices.Contains(h.calls, "iptables -w 5 -t filter -X CILIUM_INPUT") || h.exists("sys/fs/bpf/tc") || h.exists("run/cilium") || h.exists("sys/fs/cgroup/kubepods") {
		t.Errorf("a step after the tc filters did not run: calls %q", h.calls)
	}
	if !slices.Contains(removed, version.DataDir) {
		t.Errorf("removed = %q, the delete loop did not run", removed)
	}
}

func TestRemoveCiliumStateDoesNotDeleteSharedPinsThroughAMount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *datapathHost, t *testing.T)
	}{
		{"an unreadable table", func(h *datapathHost, t *testing.T) {
			if err := os.Remove(h.path("mountinfo")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a bind mount over tc/globals", func(h *datapathHost, t *testing.T) {
			h.setMounts(t, h.path("sys/fs/bpf"), h.path("sys/fs/bpf/tc/globals"))
		}},
		{"a bind mount over tc", func(h *datapathHost, t *testing.T) { h.setMounts(t, h.path("sys/fs/bpf/tc")) }},
		{"a mount below tc", func(h *datapathHost, t *testing.T) { h.setMounts(t, h.path("sys/fs/bpf/tc/globals/deeper")) }},
	} {
		setup := tc.setup
		t.Run(tc.name, func(t *testing.T) {
			fx := &hostEffects{}
			h := newDatapathHost(t, fx)
			h.touch(t, "sys/fs/bpf/tc/globals/cilium_ct4_global", "sys/fs/bpf/tc/globals/foreign_map", "sys/fs/bpf/cilium/x")
			setup(h, t)
			err := removeCiliumState(fx)
			if err == nil || !strings.Contains(err.Error(), "not removing") {
				t.Fatalf("err = %v, want the refusal to delete through the mount", err)
			}
			if !h.exists("sys/fs/bpf/tc/globals/cilium_ct4_global") || !h.exists("sys/fs/bpf/tc/globals/foreign_map") {
				t.Error("a shared pin was removed through a mount, or without being able to check")
			}
		})
	}
}

// the host's own bpffs mount, at the root of the tree, is not in the way.
func TestRemoveCiliumStateAllowsTheBPFFSRootMount(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/fs/bpf/tc/globals/cilium_ct4_global", "sys/fs/bpf/tc/globals/foreign_map")
	h.setMounts(t, h.path("sys/fs/bpf"))
	if err := removeCiliumState(fx); err != nil {
		t.Fatal(err)
	}
	if h.exists("sys/fs/bpf/tc/globals/cilium_ct4_global") || !h.exists("sys/fs/bpf/tc/globals/foreign_map") {
		t.Error("want Cilium's pin removed and the foreign one kept")
	}
}

// a datapath failure must not cost the worker its mark: a retry could not make one.
func TestRunMarksAWorkerWhoseDatapathTeardownFailed(t *testing.T) {
	events, fx := runNode(t, "worker")
	var removed []string
	fx.removeAll = func(path string) error { removed = append(removed, path); return nil }
	fx.teardownDatapathFn = func() error { return errors.New("1 mounts under /run/netns could not be unmounted") }

	out := captureWriter(func(w io.Writer) {
		err := runUninstall(w, true, fx)
		if err == nil || !strings.Contains(err.Error(), "could not be unmounted") || !strings.Contains(err.Error(), "re-run") {
			t.Errorf("Run = %v, want the datapath failure and how to finish", err)
		}
	})
	if !slices.Contains(*events, "mark node-a") {
		t.Errorf("events = %q, want the worker's Node marked", *events)
	}
	if !slices.Contains(removed, version.DataDir) {
		t.Errorf("removed = %q, the worker's state was not removed", removed)
	}
	if !strings.Contains(out, "marked Node node-a") || !strings.Contains(out, "removed by the control plane") {
		t.Errorf("output lacks the mark and the notice:\n%s", out)
	}
	if strings.Contains(out, "stratum uninstalled.") {
		t.Errorf("output says uninstalled after a failed datapath teardown:\n%s", out)
	}
}

func TestRunReportsBothAStateAndADatapathFailureWithoutMarking(t *testing.T) {
	events, fx := runNode(t, "worker")
	fx.removeAll = func(string) error { return errors.New("device busy") }
	fx.teardownDatapathFn = func() error { return errors.New("datapath boom") }

	err := runUninstall(io.Discard, true, fx)
	if err == nil || !strings.Contains(err.Error(), "device busy") || !strings.Contains(err.Error(), "datapath boom") {
		t.Fatalf("runUninstall = %v, want both failures", err)
	}
	if slices.Contains(*events, "mark node-a") {
		t.Errorf("events = %q, want no mark when state removal failed", *events)
	}
}

func TestCiliumChainCleanupLeavesRulesThatOnlyMentionACiliumJump(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	dump := strings.Join([]string{
		"*filter",
		":INPUT ACCEPT [0:0]",
		":FORWARD ACCEPT [0:0]",
		":CILIUM_INPUT - [0:0]",
		":CILIUM_FORWARD - [0:0]",
		":MYCHAIN - [0:0]",
		`-A INPUT -m comment --comment "reference -j CILIUM_INPUT" -j ACCEPT`,
		`-A INPUT -m comment --comment "see -g CILIUM_FORWARD" -g MYCHAIN`,
		`-A FORWARD -m comment --comment "quoted \"-j CILIUM_FORWARD\" text" -j MYCHAIN`,
		`-A FORWARD -s 10.0.0.1/32 -m comment --comment "-j CILIUM_FORWARD" -j DROP`,
		`-A INPUT -m comment --comment "cilium-feeder: CILIUM_INPUT" -j CILIUM_INPUT`,
		`-A FORWARD -m comment --comment "cilium-feeder: CILIUM_FORWARD" -g CILIUM_FORWARD`,
		"-A MYCHAIN -j CILIUM_INPUT",
		"-A CILIUM_INPUT -j CILIUM_FORWARD",
		"COMMIT",
		"",
	}, "\n")
	var deletes []string
	for _, args := range ciliumChainCleanup(dump) {
		if slices.Contains(args, "-D") {
			deletes = append(deletes, strings.Join(args, " "))
		}
	}
	want := []string{
		"-w 5 -t filter -D INPUT -m comment --comment cilium-feeder: CILIUM_INPUT -j CILIUM_INPUT",
		"-w 5 -t filter -D FORWARD -m comment --comment cilium-feeder: CILIUM_FORWARD -g CILIUM_FORWARD",
		"-w 5 -t filter -D MYCHAIN -j CILIUM_INPUT",
	}
	if !slices.Equal(deletes, want) {
		t.Errorf("deletes:\n%q\nwant only the rules whose target is a Cilium chain:\n%q", deletes, want)
	}
}

func TestJumpsToCiliumChain(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	for rule, want := range map[string]bool{
		`-A INPUT -j CILIUM_INPUT`:                                          true,
		`-A INPUT -g CILIUM_INPUT`:                                          true,
		`-A INPUT -j OLD_CILIUM_INPUT`:                                      true,
		`-A INPUT -g OLD_CILIUM_PRE_nat`:                                    true,
		`-A INPUT -m comment --comment "x" -j CILIUM_INPUT`:                 true,
		`-A INPUT -j ACCEPT`:                                                false,
		`-A INPUT -m comment --comment "-j CILIUM_INPUT" -j ACCEPT`:         false,
		`-A INPUT -m comment --comment "a -g CILIUM_INPUT" -g OTHER`:        false,
		`-A INPUT -m comment --comment "see -j OLD_CILIUM_INPUT" -j ACCEPT`: false,
		`-A INPUT -j CILIUM_INPUT -m comment --comment "trailing"`:          false, // not how iptables-save prints a jump
		`-A CILIUM_INPUT -j CILIUM_FORWARD`:                                 false, // flushed with its chain, not deleted
		`-A OLD_CILIUM_INPUT -j CILIUM_FORWARD`:                             false, // flushed with its chain, not deleted
		`-A CILIUM_INPUT -j OLD_CILIUM_FORWARD`:                             false,
		`-A INPUT -p tcp --dport 22 -j ACCEPT`:                              false,
		`-A INPUT -m comment --comment "x -j CILIUM_INPUT"`:                 false,
		`-A INPUT`: false,
		``:         false,
	} {
		if got := jumpsToCiliumChain(splitRule(rule)); got != want {
			t.Errorf("jumpsToCiliumChain(%q) = %v, want %v", rule, got, want)
		}
	}
}

// runNode for a worker, with removeAll failing only for the paths in failing.
func workerRun(t *testing.T, failing ...string) (*[]string, *hostEffects, *[]string) {
	t.Helper()
	events, fx := runNode(t, "worker")
	removed := &[]string{}
	fx.removeAll = func(path string) error {
		if slices.Contains(failing, path) {
			return errors.New("cannot remove " + path)
		}
		*removed = append(*removed, path)
		return nil
	}
	return events, fx, removed
}

// once DataDir is gone a retry cannot mark, so any other failure must not skip it.
func TestRunMarksAWorkerWhoseStateWasRemovedWhateverElseFailed(t *testing.T) {
	var events *[]string
	var fx *hostEffects
	sysctl := "/etc/sysctl.d/99-stratum.conf"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  string // a fragment of the returned error
	}{
		{"a later target cannot be removed", func(t *testing.T) { removeAllFailing(t, fx, sysctl) }, sysctl},
		{"reloading systemd fails", func(t *testing.T) { fx.reloadSystemd = func() error { return errors.New("reload boom") } }, "reload boom"},
		{"the datapath teardown fails", func(t *testing.T) {
			fx.teardownDatapathFn = func() error { return errors.New("datapath boom") }
		}, "datapath boom"},
		{"a later target, systemd and the datapath all fail", func(t *testing.T) {
			removeAllFailing(t, fx, sysctl)
			fx.reloadSystemd = func() error { return errors.New("reload boom") }
			fx.teardownDatapathFn = func() error { return errors.New("datapath boom") }
		}, "datapath boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, fx, _ = workerRun(t)
			tc.setup(t)
			out := captureWriter(func(w io.Writer) {
				err := runUninstall(w, true, fx)
				if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "re-run") {
					t.Errorf("Run = %v, want the failure (%q) and how to finish", err, tc.want)
				}
			})
			if !slices.Contains(*events, "mark node-a") {
				t.Errorf("events = %q, want the worker's Node marked: its state was removed", *events)
			}
			if !strings.Contains(out, "marked Node node-a") || !strings.Contains(out, "removed by the control plane") {
				t.Errorf("output lacks the mark and the notice:\n%s", out)
			}
			if strings.Contains(out, "stratum uninstalled.") {
				t.Errorf("output says uninstalled after a failure:\n%s", out)
			}
		})
	}
}

// fails only for the given paths, delegating the rest to runNode's stub.
func removeAllFailing(t *testing.T, fx *hostEffects, failing ...string) {
	t.Helper()
	ok := fx.removeAll
	fx.removeAll = func(path string) error {
		if slices.Contains(failing, path) {
			return errors.New("cannot remove " + path)
		}
		return ok(path)
	}
}

// a mark with the state kept could delete the Node under a node that is still there.
func TestRunDoesNotMarkAWorkerWhoseStateWasKept(t *testing.T) {
	var events *[]string
	var fx *hostEffects
	var removed *[]string
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  string
	}{
		{"removing DataDir fails", func(t *testing.T) { removeAllFailing(t, fx, version.DataDir) }, version.DataDir},
		{"DataDir is kept for the firewalld record", func(t *testing.T) {
			fx.untrustPodCIDRs = func() error { return errors.New("firewalld boom") }
		}, "firewalld boom"},
		{"DataDir fails and so does the datapath", func(t *testing.T) {
			removeAllFailing(t, fx, version.DataDir)
			fx.teardownDatapathFn = func() error { return errors.New("datapath boom") }
		}, "datapath boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, fx, removed = workerRun(t)
			tc.setup(t)
			if tc.name == "DataDir is kept for the firewalld record" {
				// runNode's removeAll stub, via workerRun's, records every path.
				defer func() {
					if slices.Contains(*removed, version.DataDir) {
						t.Errorf("removed = %q, DataDir must be kept", *removed)
					}
				}()
			}
			out := captureWriter(func(w io.Writer) {
				err := runUninstall(w, true, fx)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("Run = %v, want %q", err, tc.want)
				}
				if err != nil && strings.Contains(err.Error(), "state was removed") {
					t.Errorf("Run = %v, must not say the state was removed", err)
				}
			})
			if slices.Contains(*events, "mark node-a") {
				t.Errorf("events = %q, want no mark: the state was kept or its removal failed", *events)
			}
			if strings.Contains(out, "marked Node") || strings.Contains(out, "stratum uninstalled.") {
				t.Errorf("unexpected output:\n%s", out)
			}
		})
	}
}

func TestCiliumCgroupPrograms(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	show := strings.Join([]string{
		"ID       AttachType      AttachFlags     Name",
		"77       connect4        multi           cil_sock4_conne",
		"78       sendmsg6        multi           cil_sock6_sendm",
		"79       post_bind4                      cil_sock4_post_",
		"80       egress          multi           other_prog",
		"81       connect4        multi           notcil_sock4",
		"x1       connect4        multi           cil_bad_id",
		"82       device",
		"",
	}, "\n")
	// 79 has an empty flags column, so its name is the third field.
	want := [][2]string{{"connect4", "77"}, {"sendmsg6", "78"}, {"post_bind4", "79"}}
	if got := ciliumCgroupPrograms(show); !slices.Equal(got, want) {
		t.Errorf("ciliumCgroupPrograms = %q, want %q", got, want)
	}
}

// agents on kernels below 5.7 attach without a bpf link, so the pins detach nothing.
func TestRemoveCiliumCgroupPrograms(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	show := "ID       AttachType      AttachFlags     Name\n77       connect4        multi           cil_sock4_conne\n80       egress          multi           other_prog\n"
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		h.calls = append(h.calls, name+" "+strings.Join(args, " "))
		if args[1] == "show" {
			return []byte(show), nil
		}
		show = "ID       AttachType      AttachFlags     Name\n80       egress          multi           other_prog\n"
		return nil, nil
	}
	if err := removeCiliumCgroupPrograms(fx); err != nil {
		t.Fatal(err)
	}
	root := h.path("sys/fs/cgroup")
	want := []string{"bpftool cgroup show " + root, "bpftool cgroup detach " + root + " connect4 id 77"}
	if !slices.Equal(h.calls, want) {
		t.Errorf("calls = %q, want %q: only Cilium's program detached", h.calls, want)
	}
	h.calls = nil
	if err := removeCiliumCgroupPrograms(fx); err != nil || len(h.calls) != 1 {
		t.Errorf("second run = %v, calls %q; want only the listing", err, h.calls)
	}
}

func TestRemoveCiliumCgroupProgramsWithoutBpftool(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	fx.runCmd = func(_, name string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("%s: %w", name, exec.ErrNotFound)
	}
	err := removeCiliumCgroupPrograms(fx)
	if err == nil || !strings.Contains(err.Error(), "install bpftool") || !strings.Contains(err.Error(), "re-run") {
		t.Fatalf("err = %v, want an actionable error: without bpftool the programs cannot be checked", err)
	}
}

func TestRemoveCiliumCgroupProgramsReportsAFailedDetach(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	fx.runCmd = func(_, _ string, args ...string) ([]byte, error) {
		if args[1] == "show" {
			return []byte("77 connect4 multi cil_sock4_conne\n"), nil
		}
		return nil, errors.New("bpftool: Operation not permitted")
	}
	if err := removeCiliumCgroupPrograms(fx); err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("err = %v, want the failed detach", err)
	}
}

func cgroupShow(progs ...string) string {
	var b strings.Builder
	b.WriteString("ID       AttachType      AttachFlags     Name\n")
	for _, p := range progs {
		id, typ, _ := strings.Cut(p, " ")
		fmt.Fprintf(&b, "%s       %s        multi           cil_sock_%s\n", id, typ, id)
	}
	return b.String()
}

// seen on every worker uninstall on kernel 6.12: links are released from a work queue.
func TestRemoveCiliumCgroupProgramsAcceptsProgramsThatLeaveOnTheirOwn(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	listed := cgroupShow("77 connect4", "78 sendmsg6")
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		h.calls = append(h.calls, name+" "+strings.Join(args, " "))
		if args[1] == "show" {
			return []byte(listed), nil
		}
		listed = cgroupShow() // both links released meanwhile
		if args[5] == "77" {
			return nil, errors.New("bpftool cgroup detach: exit status 255: Error: failed to detach program")
		}
		return nil, errors.New("bpftool cgroup detach: exit status 255: Error: get by id (78): No such file or directory")
	}
	if err := removeCiliumCgroupPrograms(fx); err != nil {
		t.Fatalf("err = %v, want none: both programs left on their own", err)
	}
	root := h.path("sys/fs/cgroup")
	want := []string{
		"bpftool cgroup show " + root,
		"bpftool cgroup detach " + root + " connect4 id 77",
		"bpftool cgroup detach " + root + " sendmsg6 id 78",
		"bpftool cgroup show " + root,
	}
	if !slices.Equal(h.calls, want) {
		t.Errorf("calls = %q, want %q: one listing after the failed detaches", h.calls, want)
	}
}

func TestRemoveCiliumCgroupProgramsListsAgainUntilTheProgramIsGone(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	swap(t, &cgroupProgramWait, time.Minute)
	swap(t, &pollInterval, time.Millisecond)
	shows := 0
	fx.runCmd = func(_, _ string, args ...string) ([]byte, error) {
		if args[1] != "show" {
			return nil, errors.New("Error: failed to detach program")
		}
		shows++
		if shows <= 3 { // the first listing and two more still show it
			return []byte(cgroupShow("77 connect4")), nil
		}
		return []byte(cgroupShow()), nil
	}
	if err := removeCiliumCgroupPrograms(fx); err != nil || shows != 4 {
		t.Fatalf("err = %v after %d listings, want none after 4", err, shows)
	}
}

func TestRemoveCiliumCgroupProgramsReportsOnlyWhatStaysAttached(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	listed := cgroupShow("77 connect4", "78 sendmsg6")
	fx.runCmd = func(_, _ string, args ...string) ([]byte, error) {
		if args[1] == "show" {
			return []byte(listed), nil
		}
		listed = cgroupShow("78 sendmsg6") // 77's link was released; 78 has none
		return nil, fmt.Errorf("bpftool %s: Error: failed to detach program", strings.Join(args, " "))
	}
	err := removeCiliumCgroupPrograms(fx)
	if err == nil || !strings.Contains(err.Error(), "id 78") {
		t.Fatalf("err = %v, want the detach of 78, which is still attached", err)
	}
	if strings.Contains(err.Error(), "id 77") {
		t.Errorf("err = %v, reports 77, which is no longer attached", err)
	}
}

func TestRemoveCiliumCgroupProgramsReportsAFailedRelisting(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	shows := 0
	fx.runCmd = func(_, _ string, args ...string) ([]byte, error) {
		if args[1] != "show" {
			return nil, errors.New("Error: failed to detach program")
		}
		if shows++; shows > 1 {
			return nil, errors.New("bpftool: wedged")
		}
		return []byte(cgroupShow("77 connect4")), nil
	}
	err := removeCiliumCgroupPrograms(fx)
	if err == nil || !strings.Contains(err.Error(), "failed to detach program") || !strings.Contains(err.Error(), "wedged") {
		t.Fatalf("err = %v, want the failed detach and the failed listing", err)
	}
}

// the listing after a timed-out detach would wait out the same timeout.
func TestRemoveCiliumCgroupProgramsStopsOnATimeout(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		h.calls = append(h.calls, name+" "+strings.Join(args, " "))
		if args[1] == "show" {
			return []byte(cgroupShow("77 connect4", "78 sendmsg6")), nil
		}
		return nil, fmt.Errorf("x: %w", errCommandTimeout)
	}
	if err := removeCiliumCgroupPrograms(fx); !errors.Is(err, errCommandTimeout) || len(h.calls) != 2 {
		t.Fatalf("err = %v after %q, want the timeout after the listing and one detach", err, h.calls)
	}
}

// the cni- prefix is containerd's, so a host containerd's namespaces carry it too.
func TestDetachNetworkMountsLeavesAHostContainerdsNamespaces(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "run/netns/cni-aaa", "run/netns/cni-hostcontainerd", "run/netns/cni-alsohost")
	h.setMounts(t, h.path("run/netns/cni-aaa"), h.path("run/netns/cni-hostcontainerd"), h.path("run/netns/cni-alsohost"))
	mounted := []string{h.path("run/netns/cni-aaa"), h.path("run/netns/cni-hostcontainerd"), h.path("run/netns/cni-alsohost")}
	fx.unmountFn = func(mp string) error {
		h.calls = append(h.calls, "umount "+mp)
		mounted = slices.DeleteFunc(mounted, func(m string) bool { return m == mp })
		h.setMounts(t, mounted...)
		return nil
	}
	// the remaining mounts are the host's: the second look must not count them.
	if err := detachNetns(fx, true); err != nil {
		t.Fatal(err)
	}
	if want := []string{"umount " + h.path("run/netns/cni-aaa")}; !slices.Equal(h.calls, want) {
		t.Errorf("calls = %q, want only this node's namespace unmounted: %q", h.calls, want)
	}
	if h.exists("run/netns/cni-aaa") || !h.exists("run/netns/cni-hostcontainerd") || !h.exists("run/netns/cni-alsohost") {
		t.Error("want this node's namespace file removed and the host containerd's kept")
	}
}

func TestDetachNetworkMountsWithoutTheRecordedNamespaces(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	fx.stratumNetns = map[string]bool{}
	fx.stratumNetnsErr = errors.New("containerd: no answer")
	h.touch(t, "run/netns/cni-aaa")
	h.setMounts(t, h.path("run/netns/cni-aaa"), h.path("run/cilium/cgroupv2"))
	unmounts(t, fx, h, h.path("run/netns/cni-aaa"), h.path("run/cilium/cgroupv2"))
	err := detachNetns(fx, true)
	if err == nil || !strings.Contains(err.Error(), "no answer") || !strings.Contains(err.Error(), "could not identify every namespace") {
		t.Fatalf("err = %v, want the unreadable record reported", err)
	}
	if want := []string{"umount " + h.path("run/cilium/cgroupv2")}; !slices.Equal(h.calls, want) || !h.exists("run/netns/cni-aaa") {
		t.Errorf("calls = %q, want only Cilium's mount detached and the netns file kept", h.calls)
	}
}

// unmountFn drops each mount from mountinfo, as the kernel does.
func unmounts(t *testing.T, fx *hostEffects, h *datapathHost, mounted ...string) {
	t.Helper()
	fx.unmountFn = func(mp string) error {
		h.calls = append(h.calls, "umount "+mp)
		mounted = slices.DeleteFunc(mounted, func(m string) bool { return m == mp })
		h.setMounts(t, mounted...)
		return nil
	}
}

func TestRecordStratumNetns(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	spec := func(path string) string {
		return `{"ID":"x","Spec":{"linux":{"namespaces":[{"type":"pid"},{"type":"network","path":"` + path + `"},{"type":"ipc"}]}}}`
	}
	fx.ctrRun = func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "-n k8s.io containers ls -q":
			return "pause1\ncont1\nnonet\n", nil
		case "-n k8s.io containers info pause1", "-n k8s.io containers info cont1":
			return spec("/var/run/netns/cni-aaa"), nil
		case "-n k8s.io containers info nonet":
			return `{"Spec":{"linux":{"namespaces":[{"type":"pid"}]}}}`, nil
		case "-n moby containers ls -q":
			return "m1\n", nil
		case "-n moby containers info m1":
			return spec("/run/netns/cni-bbb"), nil
		case "-n k8s.io sandboxes ls":
			return "ID CREATED RUNTIME\npause1 2026-10-12 io.containerd.runc.v2\nhalfway 2026-10-12 io.containerd.runc.v2\nhostnet 2026-10-12 io.containerd.runc.v2\n", nil
		case "-n k8s.io sandboxes info pause1":
			return criSandboxInfo("/var/run/netns/cni-aaa"), nil
		case "-n k8s.io sandboxes info halfway":
			return criSandboxInfo("/var/run/netns/cni-ccc"), nil
		case "-n k8s.io sandboxes info hostnet":
			return criSandboxInfo(""), nil
		case "-n moby sandboxes ls":
			return "ID CREATED RUNTIME\n", nil
		}
		return "", fmt.Errorf("unexpected ctr %s", strings.Join(args, " "))
	}
	if err := recordStratumNetns(fx, []string{"k8s.io", "moby"}); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{h.path("run/netns/cni-aaa"): true, h.path("run/netns/cni-bbb"): true, h.path("run/netns/cni-ccc"): true}
	if fx.stratumNetnsErr != nil || !maps.Equal(fx.stratumNetns, want) {
		t.Errorf("stratumNetns = %v, err %v; want the containers' two paths and the one only a pod sandbox names", fx.stratumNetns, fx.stratumNetnsErr)
	}
	rec, err := loadNetnsRecord(fx)
	if err != nil || rec == nil || !rec.Complete || len(rec.Paths) != 3 {
		t.Errorf("saved record = %+v, %v; want the three paths, complete, saved before any container is stopped", rec, err)
	}

	// a container that cannot be read leaves a gap: the record is saved incomplete.
	fx.ctrRun = func(args ...string) (string, error) {
		if args[2] == "containers" && args[3] == "ls" {
			return "c1\n", nil
		}
		return "", errors.New("no answer from containerd")
	}
	if err := recordStratumNetns(fx, []string{"k8s.io"}); err != nil {
		t.Fatal(err)
	}
	if fx.stratumNetnsErr == nil || !strings.Contains(fx.stratumNetnsErr.Error(), "c1") {
		t.Errorf("stratumNetnsErr = %v, want the unreadable container reported", fx.stratumNetnsErr)
	}
	rec, err = loadNetnsRecord(fx)
	if err != nil || rec == nil || rec.Complete || len(rec.Paths) != 3 || len(rec.Unread) != 2 || !strings.Contains(rec.Unread[0], "c1") || !strings.Contains(rec.Unread[1], "pod sandboxes") {
		t.Errorf("saved record = %+v, %v; want it incomplete, still naming the three paths, the unread container and the unlisted sandboxes", rec, err)
	}
}

func TestRecordStratumNetnsSandboxFailures(t *testing.T) {
	failFirst := func(first func() (string, error)) func(string) (string, error) {
		return func(id string) (string, error) {
			if id == "sb1" {
				return first()
			}
			return criSandboxInfo("/var/run/netns/cni-bbb"), nil
		}
	}
	for _, tc := range []struct {
		name         string
		info         func(id string) (string, error)
		lsTimeout    bool
		containersLs error
		wantUnread   string
		wantPaths    []string
		wantNotAsked []string
	}{
		{
			name:       "info fails",
			info:       failFirst(func() (string, error) { return "", errors.New("no answer for sb1") }),
			wantUnread: "read pod sandbox k8s.io/sb1: no answer for sb1",
			wantPaths:  []string{"run/netns/cni-aaa", "run/netns/cni-bbb"},
		},
		{
			name:       "info does not decode",
			info:       failFirst(func() (string, error) { return "not json", nil }),
			wantUnread: "read pod sandbox k8s.io/sb1: decode its info",
			wantPaths:  []string{"run/netns/cni-aaa", "run/netns/cni-bbb"},
		},
		{
			name:         "info times out",
			info:         failFirst(func() (string, error) { return "", fmt.Errorf("ctr: %w", errCtrTimeout) }),
			wantUnread:   "read pod sandbox k8s.io/sb1",
			wantPaths:    []string{"run/netns/cni-aaa"},
			wantNotAsked: []string{"-n k8s.io sandboxes info sb2", "-n moby containers ls -q"},
		},
		{
			name:         "listing times out",
			lsTimeout:    true,
			wantUnread:   "list pod sandboxes in containerd namespace k8s.io",
			wantPaths:    []string{"run/netns/cni-aaa"},
			wantNotAsked: []string{"-n k8s.io sandboxes info sb1", "-n moby containers ls -q"},
		},
		{
			name:         "container listing fails, sandboxes still read",
			containersLs: errors.New("no answer"),
			info: func(id string) (string, error) {
				if id == "sb1" {
					return criSandboxInfo("/var/run/netns/cni-ccc"), nil
				}
				return criSandboxInfo("/var/run/netns/cni-bbb"), nil
			},
			wantUnread: "list containers in containerd namespace k8s.io: no answer",
			wantPaths:  []string{"run/netns/cni-bbb", "run/netns/cni-ccc"},
		},
		{
			name: "path outside the netns directory",
			info: func(id string) (string, error) {
				if id == "sb1" {
					return criSandboxInfo("/tmp/elsewhere/netns/cni-zzz"), nil
				}
				return criSandboxInfo("/var/run/netns/cni-bbb"), nil
			},
			wantPaths: []string{"run/netns/cni-aaa", "run/netns/cni-bbb"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &hostEffects{}
			h := newDatapathHost(t, fx)
			fx.stratumNetns = map[string]bool{}
			asked := map[string]bool{}
			fx.ctrRun = func(args ...string) (string, error) {
				call := strings.Join(args, " ")
				asked[call] = true
				switch call {
				case "-n k8s.io containers ls -q":
					if tc.containersLs != nil {
						return "", tc.containersLs
					}
					return "c1\n", nil
				case "-n k8s.io containers info c1":
					return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"/var/run/netns/cni-aaa"}]}}}`, nil
				case "-n k8s.io sandboxes ls":
					if tc.lsTimeout {
						return "", fmt.Errorf("ctr: %w", errCtrTimeout)
					}
					return "ID CREATED RUNTIME\nsb1 2026-10-12 io.containerd.runc.v2\nsb2 2026-10-12 io.containerd.runc.v2\n", nil
				case "-n k8s.io sandboxes info sb1":
					return tc.info("sb1")
				case "-n k8s.io sandboxes info sb2":
					return tc.info("sb2")
				case "-n moby containers ls -q", "-n moby sandboxes ls":
					return "", nil
				}
				return "", fmt.Errorf("unexpected ctr %s", call)
			}
			if err := recordStratumNetns(fx, []string{"k8s.io", "moby"}); err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, p := range tc.wantPaths {
				want = append(want, h.path(p))
			}
			rec, err := loadNetnsRecord(fx)
			if err != nil || rec == nil {
				t.Fatalf("saved record = %+v, %v", rec, err)
			}
			if !slices.Equal(rec.Paths, want) {
				t.Errorf("record paths = %q, want %q", rec.Paths, want)
			}
			if tc.wantUnread == "" {
				if !rec.Complete || fx.stratumNetnsErr != nil {
					t.Errorf("record complete=%v, err %v; want complete", rec.Complete, fx.stratumNetnsErr)
				}
			} else if rec.Complete || len(rec.Unread) != 1 || !strings.Contains(rec.Unread[0], tc.wantUnread) {
				t.Errorf("record = %+v; want it incomplete with one unread entry naming %q", rec, tc.wantUnread)
			}
			for _, call := range tc.wantNotAsked {
				if asked[call] {
					t.Errorf("asked %q after a timeout; the enumeration should have stopped", call)
				}
			}
		})
	}
}

// ctr answers a removed sandbox with only "failed to load sandbox", a container with not found.
func TestRecordStratumNetnsContainerRemovedMidRun(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gone        string // which object's info read fails: the container, or the sandbox
		infoErr     error  // what that read answers with
		relist      string // the second listing's output; "" with relistErr
		relistErr   error  // the second listing's error
		wantRelists int    // second listings asked
		wantUnread  []string
		// a timeout ends the enumeration, so the moby namespace is never asked.
		wantStop        bool
		noSandboxesRead bool
	}{
		{
			name:        "container removed after it was listed",
			gone:        "container",
			infoErr:     errors.New(`ctr: container "gone" in namespace "k8s.io": not found`),
			relist:      "kept\n",
			wantRelists: 1,
		},
		{
			name:        "pod sandbox removed after it was listed",
			gone:        "sandbox",
			infoErr:     errors.New("ctr: failed to load sandbox sb1"),
			relist:      "ID CREATED RUNTIME\n",
			wantRelists: 1,
		},
		{
			name:        "a container still listed is unread, whatever its error says",
			gone:        "container",
			infoErr:     errors.New("type with url types.example/Spec: not found"),
			relist:      "kept\ngone\n",
			wantRelists: 1,
			wantUnread:  []string{"read container k8s.io/gone: type with url types.example/Spec: not found"},
		},
		{
			name:        "a sandbox still listed is unread",
			gone:        "sandbox",
			infoErr:     errors.New("ctr: failed to load sandbox sb1"),
			relist:      "ID CREATED RUNTIME\nsb1 2026-10-15 io.containerd.runc.v2\n",
			wantRelists: 1,
			wantUnread:  []string{"read pod sandbox k8s.io/sb1: ctr: failed to load sandbox sb1"},
		},
		{
			name:        "a second listing that fails proves nothing",
			gone:        "container",
			infoErr:     errors.New(`ctr: container "gone" in namespace "k8s.io": not found`),
			relistErr:   errors.New("ctr: no such file or directory"),
			wantRelists: 1,
			wantUnread:  []string{`read container k8s.io/gone: ctr: container "gone" in namespace "k8s.io": not found`},
		},
		{
			name:            "a timeout asks no second listing",
			gone:            "container",
			infoErr:         errCtrTimeout,
			relist:          "kept\n",
			wantRelists:     0,
			wantUnread:      []string{"read container k8s.io/gone: " + errCtrTimeout.Error()},
			wantStop:        true,
			noSandboxesRead: true,
		},
		{
			name:        "a container's second listing that times out ends the enumeration",
			gone:        "container",
			infoErr:     errors.New("ctr: read failed"),
			relistErr:   errCtrTimeout,
			wantRelists: 1,
			wantUnread: []string{
				"read container k8s.io/gone: ctr: read failed",
				"list containers in containerd namespace k8s.io: " + errCtrTimeout.Error(),
			},
			wantStop:        true,
			noSandboxesRead: true,
		},
		{
			name:        "a sandbox's second listing that times out ends the enumeration",
			gone:        "sandbox",
			infoErr:     errors.New("ctr: failed to load sandbox sb1"),
			relistErr:   errCtrTimeout,
			wantRelists: 1,
			wantUnread: []string{
				"read pod sandbox k8s.io/sb1: ctr: failed to load sandbox sb1",
				"list pod sandboxes in containerd namespace k8s.io: " + errCtrTimeout.Error(),
			},
			wantStop: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &hostEffects{}
			h := newDatapathHost(t, fx)
			fx.stratumNetns = map[string]bool{}
			lists := map[string]int{}
			relists := 0
			mobyAsked := false
			listed := func(call, first string) (string, error) {
				lists[call]++
				if lists[call] == 1 {
					return first, nil
				}
				relists++
				return tc.relist, tc.relistErr
			}
			fx.ctrRun = func(args ...string) (string, error) {
				call := strings.Join(args, " ")
				switch call {
				case "-n k8s.io containers ls -q":
					return listed(call, "kept\ngone\n")
				case "-n k8s.io containers info kept":
					return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"/run/netns/cni-aaa"}]}}}`, nil
				case "-n k8s.io containers info gone":
					if tc.gone == "container" {
						return "", tc.infoErr
					}
					return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"/run/netns/cni-bbb"}]}}}`, nil
				case "-n k8s.io sandboxes ls":
					return listed(call, "ID CREATED RUNTIME\nsb1 2026-10-15 io.containerd.runc.v2\n")
				case "-n k8s.io sandboxes info sb1":
					if tc.gone == "sandbox" {
						return "", tc.infoErr
					}
					return criSandboxInfo("/run/netns/cni-bbb"), nil
				case "-n moby containers ls -q", "-n moby sandboxes ls":
					mobyAsked = true
					return "", nil
				}
				return "", fmt.Errorf("unexpected ctr %s", call)
			}
			if err := recordStratumNetns(fx, []string{"k8s.io", "moby"}); err != nil {
				t.Fatal(err)
			}
			if relists != tc.wantRelists {
				t.Errorf("second listings = %d, want %d", relists, tc.wantRelists)
			}
			if mobyAsked == tc.wantStop {
				t.Errorf("moby namespace asked = %v, want %v: a timeout ends the enumeration", mobyAsked, !tc.wantStop)
			}
			// stopping before the sandboxes loses cni-bbb, named only there.
			wantPaths := []string{h.path("run/netns/cni-aaa"), h.path("run/netns/cni-bbb")}
			if tc.noSandboxesRead {
				wantPaths = wantPaths[:1]
			}
			wantNetns := map[string]bool{}
			for _, p := range wantPaths {
				wantNetns[p] = true
			}
			rec, err := loadNetnsRecord(fx)
			if err != nil || rec == nil {
				t.Fatalf("saved record = %+v, %v", rec, err)
			}
			if len(tc.wantUnread) == 0 {
				if !maps.Equal(fx.stratumNetns, wantNetns) ||
					fx.stratumNetnsErr != nil || !rec.Complete || !slices.Equal(rec.Paths, wantPaths) || len(rec.Unread) != 0 {
					t.Errorf("stratumNetns = %v, err %v, record = %+v; want both namespaces, complete, nothing unread", fx.stratumNetns, fx.stratumNetnsErr, rec)
				}
				return
			}
			if !maps.Equal(fx.stratumNetns, wantNetns) {
				t.Errorf("stratumNetns = %v; want %v, the namespaces the readable objects name", fx.stratumNetns, wantNetns)
			}
			if rec.Complete || !slices.Equal(rec.Unread, tc.wantUnread) || !slices.Equal(rec.Paths, wantPaths) {
				t.Errorf("record = %+v; want it incomplete, paths %q, unread %q", rec, wantPaths, tc.wantUnread)
			}
			if want := strings.Join(tc.wantUnread, "\n"); fx.stratumNetnsErr == nil || fx.stratumNetnsErr.Error() != want {
				t.Errorf("stratumNetnsErr = %v, want %q", fx.stratumNetnsErr, want)
			}
		})
	}
}

// the shape containerd 2.3 prints: CRI metadata as a protobuf Any's base64 JSON value.
func criSandboxInfo(netns string) string {
	md := `{"Version":"v1","Metadata":{"ID":"x","Name":"pod","NetNSPath":"` + netns + `","IP":""}}`
	return `{"ID":"x","Sandboxer":"podsandbox","Extensions":{"metadata":{"type_url":"github.com/containerd/cri/pkg/store/sandbox/Metadata","value":"` +
		base64.StdEncoding.EncodeToString([]byte(md)) + `"}}}`
}

func TestSandboxNetnsPath(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	for _, tc := range []struct {
		name, info, want string
		wantErr          bool
	}{
		{name: "pod network", info: criSandboxInfo("/var/run/netns/cni-aaa"), want: "/var/run/netns/cni-aaa"},
		{name: "host network", info: criSandboxInfo("")},
		{name: "no CRI metadata", info: `{"ID":"x","Extensions":{}}`, wantErr: true},
		{name: "unversioned metadata", info: `{"Extensions":{"metadata":{"value":"` + base64.StdEncoding.EncodeToString([]byte(`{"NetNSPath":"/run/netns/cni-b"}`)) + `"}}}`, wantErr: true},
		{name: "another version", info: `{"Extensions":{"metadata":{"value":"` + base64.StdEncoding.EncodeToString([]byte(`{"Version":"v2","Metadata":{"NetNSPath":"/run/netns/cni-b"}}`)) + `"}}}`, wantErr: true},
		{name: "empty metadata", info: `{"Extensions":{"metadata":{"value":"` + base64.StdEncoding.EncodeToString([]byte(`{}`)) + `"}}}`, wantErr: true},
		{name: "info does not decode", info: "not json", wantErr: true},
		{name: "metadata does not decode", info: `{"Extensions":{"metadata":{"value":"` + base64.StdEncoding.EncodeToString([]byte("{")) + `"}}}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sandboxNetnsPath(tc.info)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("sandboxNetnsPath = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestExecCmdTimesOut(t *testing.T) {
	swap(t, &cmdTimeout, 100*time.Millisecond)
	start := time.Now()
	_, err := execCmd("", "sleep", "30")
	if !errors.Is(err, errCommandTimeout) {
		t.Fatalf("err = %v, want errCommandTimeout", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %s, want the 100ms limit enforced", took)
	}
	if out, err := execCmd("hello", "cat"); err != nil || string(out) != "hello" {
		t.Errorf("a command within the limit = %q, %v", out, err)
	}
}

func TestTimeoutStopsTheLoops(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/class/net/cilium_host", "sys/class/net/lxc1a2b3", "sys/class/net/eth0", "sys/class/net/eth1")
	timedOut := fmt.Errorf("x: %w", errCommandTimeout)

	var links []string
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		links = append(links, name)
		return nil, timedOut
	}
	if err := removeCiliumLinks(fx); !errors.Is(err, errCommandTimeout) || len(links) != 1 {
		t.Errorf("links: err = %v, %d commands; want one command and the timeout", err, len(links))
	}

	var tcs []string
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		tcs = append(tcs, name)
		return nil, timedOut
	}
	if err := removeCiliumTCFilters(fx); !errors.Is(err, errCommandTimeout) || len(tcs) != 1 {
		t.Errorf("tc: err = %v, %d commands; want one command and the timeout", err, len(tcs))
	}

	var ipt []string
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		ipt = append(ipt, name)
		if name == "iptables-save" {
			return []byte("*filter\n:CILIUM_A - [0:0]\n:CILIUM_B - [0:0]\nCOMMIT\n"), nil
		}
		return nil, timedOut
	}
	if err := removeCiliumChains(fx); !errors.Is(err, errCommandTimeout) {
		t.Errorf("iptables: err = %v, want the timeout", err)
	}
	// not every -F and -X, and not ip6tables either, which shares the lock.
	if want := []string{"iptables-save", "iptables"}; !slices.Equal(ipt, want) {
		t.Errorf("iptables commands = %q, want %q: the first timeout ends the step", ipt, want)
	}
}

func TestRemoveLegacyKubeletDirKeepsSocketsBelowAMount(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	dir := shortKubeletDir(t, fx)
	p := filepath.Join(dir, "device-plugins/kubelet.sock")
	staleSocket(t, p)
	h.setMounts(t, filepath.Dir(p))
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); err != nil {
		t.Fatalf("a socket below a mount was removed: %v", err)
	}

	if err := os.Remove(h.path("mountinfo")); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyKubeletDir(io.Discard, fx); err == nil || !strings.Contains(err.Error(), "not removing") {
		t.Fatalf("err = %v, want the unreadable mount table reported", err)
	}
	if _, err := os.Lstat(p); err != nil {
		t.Fatalf("a socket was removed without being able to read the mount table: %v", err)
	}
}

func TestRemoveLegacyKubeletDirDoesNotFollowSymlinks(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	foreign, err := os.MkdirTemp("", "foreign")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(foreign) })
	staleSocket(t, filepath.Join(foreign, "kubelet.sock"))

	dir := shortKubeletDir(t, fx)
	if err := os.Symlink(foreign, filepath.Join(dir, "device-plugins")); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(foreign, "kubelet.sock")); err != nil {
		t.Fatalf("a socket outside /var/lib/kubelet was removed through a symlink: %v", err)
	}

	link := filepath.Join(t.TempDir(), "kubelet")
	if err := os.Symlink(foreign, link); err != nil {
		t.Fatal(err)
	}
	fx.legacyKubeletDir = link
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(foreign, "kubelet.sock")); err != nil {
		t.Fatalf("a socket was removed through a symlinked /var/lib/kubelet: %v", err)
	}
}

// a datagram socket answers a stream connect with a wrong-type error, which proves nothing.
func TestRemoveLegacyKubeletDirKeepsASocketItCannotShowStale(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dir := shortKubeletDir(t, fx)
	p := filepath.Join(dir, "device-plugins/kubelet.sock")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: p, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p); err != nil {
		t.Fatalf("a live datagram socket was removed: %v", err)
	}
}

// a namespace is Stratum's only at a path in the netns directory a spec names.
func TestCanonNetns(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	for in, want := range map[string]string{
		"/run/netns/cni-aaa":               h.path("run/netns/cni-aaa"),
		"/var/run/netns/cni-aaa":           h.path("run/netns/cni-aaa"),
		"/var/run/netns/../netns/cni-aaa":  h.path("run/netns/cni-aaa"),
		h.path("run/netns/cni-aaa"):        h.path("run/netns/cni-aaa"),
		"/operator/netns/cni-aaa":          "",
		"/run/netns/sub/cni-aaa":           "",
		"/run/cni-aaa":                     "",
		"/proc/1/ns/net":                   "",
		"/run/netns":                       "",
		"/run/netns/":                      "",
		h.path("run/netns/x") + "/../../y": "",
	} {
		got, ok := canonNetns(fx, in)
		if got != want || ok != (want != "") {
			t.Errorf("canonNetns(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestRecordStratumNetnsIgnoresTheSameNameElsewhere(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "run/netns/cni-foreign")
	h.setMounts(t, h.path("run/netns/cni-foreign"))
	fx.stratumNetns = map[string]bool{}
	fx.ctrRun = func(args ...string) (string, error) {
		if args[2] == "sandboxes" {
			return "", nil
		}
		if args[3] == "ls" {
			return "c1", nil
		}
		return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"/operator/netns/cni-foreign"}]}}}`, nil
	}
	recordStratumNetns(fx, []string{"k8s.io"})
	if err := detachNetns(fx, true); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 0 || !h.exists("run/netns/cni-foreign") {
		t.Fatalf("an unrelated /run/netns file was selected by its name: calls %v", h.calls)
	}
}

func TestRecordStratumNetnsReportsWhatItCannotRead(t *testing.T) {
	good := func(h *datapathHost) string {
		return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"` + h.path("run/netns/cni-aaa") + `"}]}}}`
	}
	for name, bad := range map[string]func() (string, error){
		"malformed JSON": func() (string, error) { return "{malformed", nil },
		"a failed read":  func() (string, error) { return "", errors.New("cannot read bad spec") },
	} {
		t.Run(name, func(t *testing.T) {
			fx := &hostEffects{}
			h := newDatapathHost(t, fx)
			h.touch(t, "run/netns/cni-aaa", "run/netns/cni-bbb")
			h.setMounts(t, h.path("run/netns/cni-aaa"), h.path("run/netns/cni-bbb"))
			unmounts(t, fx, h, h.path("run/netns/cni-aaa"), h.path("run/netns/cni-bbb"))
			fx.stratumNetns = map[string]bool{}
			fx.ctrRun = func(args ...string) (string, error) {
				switch {
				case args[3] == "ls":
					return "good bad", nil
				case args[4] == "good":
					return good(h), nil
				}
				return bad()
			}
			if err := recordStratumNetns(fx, []string{"k8s.io"}); err != nil {
				t.Fatal(err)
			}
			if fx.stratumNetnsErr == nil || !strings.Contains(fx.stratumNetnsErr.Error(), "k8s.io/bad") {
				t.Fatalf("stratumNetnsErr = %v, want the unreadable container named", fx.stratumNetnsErr)
			}
			err := detachNetns(fx, true)
			if err == nil || !strings.Contains(err.Error(), "could not identify every namespace") || !strings.Contains(err.Error(), "k8s.io/bad") {
				t.Fatalf("err = %v, want it to say the record is incomplete, naming the container", err)
			}
			// The namespace that was identified is detached; the other is not touched.
			if want := []string{"umount " + h.path("run/netns/cni-aaa")}; !slices.Equal(h.calls, want) || !h.exists("run/netns/cni-bbb") {
				t.Errorf("calls = %q, want only the identified namespace detached", h.calls)
			}
			if rec, err := loadNetnsRecord(fx); err != nil || rec == nil || rec.Complete {
				t.Errorf("saved record = %+v, %v; want it kept, incomplete", rec, err)
			}
		})
	}
}

func TestRecordStratumNetnsStopsAfterATimeout(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	var calls int
	fx.ctrRun = func(args ...string) (string, error) {
		calls++
		if args[3] == "ls" {
			return "c1 c2 c3", nil
		}
		return "", fmt.Errorf("ctr: %w within 2m0s", errCtrTimeout)
	}
	if err := recordStratumNetns(fx, []string{"k8s.io", "moby"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || fx.stratumNetnsErr == nil {
		t.Errorf("%d ctr calls, err %v; want the list and one read, then the timeout reported", calls, fx.stratumNetnsErr)
	}
	// a failure before any path is found must not read as an empty, complete one.
	if rec, err := loadNetnsRecord(fx); err != nil || rec == nil || rec.Complete || len(rec.Paths) != 0 {
		t.Errorf("saved record = %+v, %v; want it incomplete, with no paths", rec, err)
	}
}

func TestNetnsDetachCanBeFinishedByALaterRun(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "run/netns/cni-aaa", "run/netns/cni-bbb")
	h.setMounts(t, h.path("run/netns/cni-aaa"), h.path("run/netns/cni-bbb"))
	fx.stratumNetns = map[string]bool{}
	fx.ctrRun = func(args ...string) (string, error) {
		if args[2] == "sandboxes" {
			return "", nil
		}
		if args[3] == "ls" {
			return "c1 c2", nil
		}
		return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"` + h.path("run/netns/cni-"+map[string]string{"c1": "aaa", "c2": "bbb"}[args[4]]) + `"}]}}}`, nil
	}
	if err := recordStratumNetns(fx, []string{"k8s.io"}); err != nil {
		t.Fatal(err)
	}
	if rec, err := loadNetnsRecord(fx); err != nil || rec == nil || !rec.Complete || len(rec.Paths) != 2 {
		t.Fatalf("saved record = %+v, %v; want both paths, complete", rec, err)
	}

	fx.unmountFn = func(string) error { return errors.New("busy") }
	if err := detachNetns(fx, true); err == nil {
		t.Fatal("the first attempt should fail")
	}
	if _, err := os.Stat(fx.netnsRecordPath); err != nil {
		t.Fatalf("the record was not kept for a retry: %v", err)
	}

	// A new process: no container specs, no in-memory record. Still busy.
	fx.stratumNetns, fx.stratumNetnsErr = map[string]bool{}, nil
	if err := detachNetns(fx, true); err == nil || !strings.Contains(err.Error(), "cni-") {
		t.Fatalf("the retry reported %v while the owned mounts remain", err)
	}
	if _, err := os.Stat(fx.netnsRecordPath); err != nil {
		t.Fatalf("the record was removed while a mount remains: %v", err)
	}

	// The mount goes this time.
	h.calls = nil
	unmounts(t, fx, h, h.path("run/netns/cni-aaa"), h.path("run/netns/cni-bbb"))
	if err := detachNetns(fx, true); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 2 || h.exists("run/netns/cni-aaa") || h.exists("run/netns/cni-bbb") {
		t.Errorf("calls %v: want both owned mounts detached and their files removed", h.calls)
	}
	if _, err := os.Stat(fx.netnsRecordPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record is still there after a complete detach: %v", err)
	}

	// And with nothing left, a further run is a no-op.
	h.calls = nil
	if err := detachNetns(fx, true); err != nil || len(h.calls) != 0 {
		t.Errorf("a run with nothing left = %v, calls %v", err, h.calls)
	}
}

func TestNetnsDetachWithAnUnreadableSavedRecord(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	fx.stratumNetns = map[string]bool{}
	h.touch(t, "run/netns/cni-aaa")
	h.setMounts(t, h.path("run/netns/cni-aaa"))
	if err := os.WriteFile(fx.netnsRecordPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := detachNetns(fx, true)
	if err == nil || !errors.Is(err, errKeepState) || !strings.Contains(err.Error(), "is not known") {
		t.Fatalf("err = %v, want the unreadable record reported, asking to keep the state", err)
	}
	if len(h.calls) != 0 || !h.exists("run/netns/cni-aaa") {
		t.Errorf("calls %v: an owned namespace was touched without a readable record", h.calls)
	}
	if _, err := os.Stat(fx.netnsRecordPath); err != nil {
		t.Errorf("the unreadable record was deleted: %v", err)
	}
}

// iptables reports an exhausted -w wait as an ordinary failure (exit status 4).
func TestRemoveCiliumChainsStopsWhenTheXtablesLockWaitRunsOut(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	var calls []string
	fx.runCmd = func(stdin, name string, args ...string) ([]byte, error) {
		calls = append(calls, name)
		if strings.HasSuffix(name, "-save") {
			return []byte("*filter\n:CILIUM_A - [0:0]\n:CILIUM_B - [0:0]\nCOMMIT\n"), nil
		}
		return execCmd("", "sh", "-c", "echo 'Another app is currently holding the xtables lock. Stopped waiting after 5s.' >&2; exit 4")
	}
	err := removeCiliumChains(fx)
	if err == nil || !strings.Contains(err.Error(), "holding the xtables lock") {
		t.Fatalf("err = %v, want the lock failure reported", err)
	}
	if want := []string{"iptables-save", "iptables"}; !slices.Equal(calls, want) {
		t.Errorf("commands = %q, want %q: the first exhausted wait ends the step", calls, want)
	}
}

func TestWaitExhausted(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	for err, want := range map[error]bool{
		fmt.Errorf("x: %w", errCommandTimeout): true,
		errors.New("iptables -w 5 -t nat -F X: exit status 4: Another app is currently holding the xtables lock. Stopped waiting after 5s."): true,
		errors.New("iptables: No chain/target/match by that name."):                                                                          false,
		errors.New("iptables: Bad rule (does a matching rule exist in that chain?)."):                                                        false,
	} {
		if got := waitExhausted(err); got != want {
			t.Errorf("waitExhausted(%q) = %v, want %v", err, got, want)
		}
	}
}

func TestPartialRecordNeverCleanOnAFreshRetry(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	a, b := h.path("run/netns/cni-aaa"), h.path("run/netns/cni-bbb")
	h.touch(t, "run/netns/cni-aaa", "run/netns/cni-bbb")
	h.setMounts(t, a, b)
	unmounts(t, fx, h, a, b)
	fx.stratumNetns = map[string]bool{}
	fx.ctrRun = func(args ...string) (string, error) {
		if args[3] == "ls" {
			return "good unreadable", nil
		}
		if args[4] == "good" {
			return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"` + a + `"}]}}}`, nil
		}
		return "", errors.New("cannot read second container")
	}
	if err := recordStratumNetns(fx, []string{"k8s.io"}); err != nil {
		t.Fatal(err)
	}
	if err := detachNetns(fx, true); err == nil {
		t.Fatal("the first attempt should report the incomplete record")
	}

	// the next process has no specs and no in-memory error, only the saved record.
	fx.stratumNetns, fx.stratumNetnsErr = map[string]bool{}, nil
	err := detachNetns(fx, true)
	if err == nil || !strings.Contains(err.Error(), "k8s.io/unreadable") {
		t.Fatalf("fresh retry = %v, want the incomplete record reported, naming the unread container", err)
	}
	if !h.exists("run/netns/cni-bbb") {
		t.Error("the unread container's namespace was removed")
	}
	if rec, err := loadNetnsRecord(fx); err != nil || rec == nil || rec.Complete {
		t.Errorf("the record = %+v, %v; want it kept, and still incomplete", rec, err)
	}
}

// sticky: a container unread then may be gone now, so a clean read does not complete it.
func TestIncompleteRecordStaysIncomplete(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	fx.stratumNetns = map[string]bool{}
	if err := saveNetnsRecord(fx, netnsRecord{Complete: false, Paths: []string{h.path("run/netns/cni-aaa")}, Unread: []string{"k8s.io/gone: no answer"}}); err != nil {
		t.Fatal(err)
	}
	fx.ctrRun = func(args ...string) (string, error) { return "", nil }
	if err := recordStratumNetns(fx, []string{"k8s.io"}); err != nil {
		t.Fatal(err)
	}
	rec, err := loadNetnsRecord(fx)
	if err != nil || rec == nil || rec.Complete || len(rec.Paths) != 1 || len(rec.Unread) != 1 {
		t.Errorf("record = %+v, %v; want it incomplete, with its path and its unread entry kept", rec, err)
	}
}

func TestRecordStratumNetnsLeavesAnUnreadableSavedRecordAlone(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	fx.stratumNetns = map[string]bool{}
	original := []byte(`{"complete":true,"paths":["/run/netns/cni-earlier"`)
	if err := os.WriteFile(fx.netnsRecordPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	fx.ctrRun = func(args ...string) (string, error) {
		if args[3] == "ls" {
			return "current", nil
		}
		return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"` + h.path("run/netns/cni-current") + `"}]}}}`, nil
	}
	err := recordStratumNetns(fx, []string{"k8s.io"})
	if err == nil || !strings.Contains(err.Error(), "read the saved record") || !strings.Contains(err.Error(), "left as it is") {
		t.Fatalf("recordStratumNetns = %v, want the unreadable saved record returned", err)
	}
	after, readErr := os.ReadFile(fx.netnsRecordPath)
	if readErr != nil || !bytes.Equal(after, original) {
		t.Errorf("the unreadable record was changed: %q, %v", after, readErr)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(fx.netnsRecordPath), ".*.tmp-*")); len(left) != 0 {
		t.Errorf("a replacement was written beside it: %v", left)
	}
}

func TestRunAbortsWhenTheSavedNetnsRecordCannotBeRead(t *testing.T) {
	events, fx := runNode(t, "worker")
	h := newDatapathHost(t, fx) // after runNode, so its mount table and paths win
	fx.stratumNetns = map[string]bool{}
	fx.stopContainersFn = func() error { return stopContainers(io.Discard, fx) }
	var removed, torn []string
	fx.removeAll = func(path string) error { removed = append(removed, path); return nil }
	original := []byte(`{"complete":true,"paths":["/run/netns/cni-earlier"`)
	if err := os.WriteFile(fx.netnsRecordPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	fx.ctrRun = func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "namespaces ls -q":
			return "k8s.io\n", nil
		case "-n k8s.io containers ls -q":
			return "current\n", nil
		case "-n k8s.io containers info current":
			return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"` + h.path("run/netns/cni-current") + `"}]}}}`, nil
		case "-n k8s.io sandboxes ls":
			return "ID CREATED RUNTIME\n", nil
		}
		torn = append(torn, strings.Join(args, " "))
		return "", nil
	}

	var err error
	out := captureWriter(func(w io.Writer) { err = runUninstall(w, true, fx) })
	if err == nil || !strings.Contains(err.Error(), "read the saved record") || !strings.Contains(err.Error(), "Nothing was deleted") {
		t.Fatalf("runUninstall = %v, want the abort naming the unreadable record, with nothing deleted", err)
	}
	if len(torn) != 0 {
		t.Errorf("workload containers were touched before the record could be trusted: %q", torn)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %q, want nothing removed", removed)
	}
	if slices.Contains(*events, "mark node-a") || strings.Contains(out, "marked Node") || strings.Contains(out, "stratum uninstalled.") {
		t.Errorf("events = %q, output:\n%s; want no mark and no \"uninstalled\"", *events, out)
	}
	if after, readErr := os.ReadFile(fx.netnsRecordPath); readErr != nil || !bytes.Equal(after, original) {
		t.Errorf("the unreadable record was changed: %q, %v", after, readErr)
	}
}

func TestNoRecordIsSavedWhenThereIsNothingToRemember(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	fx.stratumNetns = map[string]bool{}
	fx.ctrRun = func(args ...string) (string, error) { return "", nil }
	if err := recordStratumNetns(fx, []string{"k8s.io"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fx.netnsRecordPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a record was saved with nothing in it: %v", err)
	}
}

// ownership must be durable before it can be lost: save before anything is torn down.
func TestStopContainersStopsBeforeTeardownWhenTheRecordCannotBeSaved(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	fx.stratumNetns = map[string]bool{}
	// a record path in a missing directory makes the atomic write fail safely.
	fx.netnsRecordPath = filepath.Join(filepath.Dir(fx.netnsRecordPath), "missing", "netns-record.json")
	var torn []string
	fx.ctrRun = func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "namespaces ls -q":
			return "k8s.io\n", nil
		case "-n k8s.io containers ls -q":
			return "c1\n", nil
		case "-n k8s.io containers info c1":
			return `{"Spec":{"linux":{"namespaces":[{"type":"network","path":"` + h.path("run/netns/cni-aaa") + `"}]}}}`, nil
		case "-n k8s.io sandboxes ls":
			return "ID CREATED RUNTIME\n", nil
		}
		torn = append(torn, strings.Join(args, " "))
		return "", nil
	}
	err := stopContainers(io.Discard, fx)
	if err == nil || !strings.Contains(err.Error(), "save the record") {
		t.Fatalf("stopContainers = %v, want the failed save", err)
	}
	if len(torn) != 0 {
		t.Errorf("containers were touched before the record was durable: %q", torn)
	}
}

func TestRunKeepsDataDirWhenTheNetnsRecordCannotBeRead(t *testing.T) {
	events, fx := runNode(t, "worker")
	var removed []string
	fx.removeAll = func(path string) error { removed = append(removed, path); return nil }
	fx.teardownDatapathFn = func() error {
		return fmt.Errorf("detach CNI netns and Cilium mounts: could not read the saved record: %w", errKeepState)
	}
	var err error
	out := captureWriter(func(w io.Writer) { err = runUninstall(w, true, fx) })
	if err == nil || !strings.Contains(err.Error(), "could not read the saved record") {
		t.Fatalf("runUninstall = %v, want the unreadable record reported", err)
	}
	if slices.Contains(removed, version.DataDir) {
		t.Errorf("removed = %q, DataDir must be kept", removed)
	}
	if !slices.Contains(removed, version.ConfDir) {
		t.Errorf("removed = %q, the other targets must still be removed", removed)
	}
	if slices.Contains(*events, "mark node-a") || strings.Contains(out, "marked Node") || strings.Contains(out, "stratum uninstalled.") {
		t.Errorf("events = %q, output:\n%s; want no mark and no \"uninstalled\" with the state kept", *events, out)
	}
}

func TestRemoveCiliumChainsStopsWhenReadingTheRulesTimesOut(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	var calls []string
	fx.runCmd = func(_, name string, _ ...string) ([]byte, error) {
		calls = append(calls, name)
		return nil, fmt.Errorf("%s: %w", name, errCommandTimeout)
	}
	err := removeCiliumChains(fx)
	if !errors.Is(err, errCommandTimeout) {
		t.Fatalf("err = %v, want the timeout", err)
	}
	if want := []string{"iptables-save"}; !slices.Equal(calls, want) {
		t.Errorf("commands = %q, want %q: a timeout reading the rules ends the step", calls, want)
	}
}

func addProc(t *testing.T, h *datapathHost, pid, comm string) {
	t.Helper()
	h.touch(t, "proc/"+pid+"/comm")
	if err := os.WriteFile(h.path("proc/"+pid+"/comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a live process: a stat saying not a zombie, and a cgroup outside kubepods.
	if err := os.WriteFile(h.path("proc/"+pid+"/stat"), []byte(pid+" ("+comm+") S 1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setProcCgroup(t, h, pid, "0::/system.slice/"+comm+".service")
}

func setProcCgroup(t *testing.T, h *datapathHost, pid string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(h.path("proc/"+pid+"/cgroup"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestForeignRuntimes(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	for pid, comm := range map[string]string{
		"11": "kubelet", "12": "containerd", "13": "containerd-shim", "14": "crio", "15": "dockerd",
		"16": "bash", "17": "kubelet-helper-x", "18": "stratum", "19": "kubeletx",
	} {
		addProc(t, h, pid, comm)
	}
	h.mkdirs(t, "proc/self", "proc/sys") // not pids
	// a zombie keeps its /proc name but runs nothing; other states do not.
	for pid, state := range map[string]string{"11": "S", "12": "R", "13": "Z", "14": "S"} {
		if err := os.WriteFile(h.path("proc/"+pid+"/stat"), []byte(pid+" (a) weird) "+state+" 1 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, bystanders, err := foreignRuntimes(fx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bystanders) != 0 {
		t.Errorf("bystanders = %q, want none: a runtime with a kubelet running may serve it", bystanders)
	}
	slices.Sort(got)
	want := []string{"containerd (pid 12)", "crio (pid 14)", "dockerd (pid 15)", "kubelet (pid 11)", "kubelet-helper-x (pid 17)"} // 13 is a zombie
	if !slices.Equal(got, want) {
		t.Errorf("foreignRuntimes = %q, want %q", got, want)
	}
}

func TestRemoveKubepodsCgroupsKeepsATreeAnotherRuntimeMayShare(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice", "sys/fs/cgroup/kubepods/pod1")
	addProc(t, h, "4242", "kubelet")
	err := removeKubepodsCgroups(fx)
	if err == nil || !strings.Contains(err.Error(), "kubelet (pid 4242)") || !strings.Contains(err.Error(), "left") {
		t.Fatalf("err = %v, want the shared tree reported with the process named", err)
	}
	if !h.exists("sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice") || !h.exists("sys/fs/cgroup/kubepods/pod1") {
		t.Error("a cgroup that another runtime may own was removed")
	}
	if len(h.calls) != 0 {
		t.Errorf("calls = %q, want none: a slice is never stopped", h.calls)
	}
}

func TestRemoveKubepodsCgroupsKeepsTheTreeWhenProcCannotBeRead(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")
	fx.procRoot = h.path("no-proc")
	err := removeKubepodsCgroups(fx)
	if err == nil || !strings.Contains(err.Error(), "could not check for another kubelet") {
		t.Fatalf("err = %v, want the failed check reported", err)
	}
	if !h.exists("sys/fs/cgroup/kubepods/pod1") {
		t.Error("the tree was removed without being able to check")
	}
}

// the kernel's refusal is what makes rmdir safe where stopping a slice is not.
func TestRemoveKubepodsCgroupsKeepsWhatIsStillInUse(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods/empty", "sys/fs/cgroup/kubepods/busy")
	// a regular file stands in for what makes a populated cgroup's rmdir fail.
	h.touch(t, "sys/fs/cgroup/kubepods/busy/cgroup.procs-stand-in")
	err := removeKubepodsCgroups(fx)
	if err == nil || !strings.Contains(err.Error(), "still in use") {
		t.Fatalf("err = %v, want what is still in use reported", err)
	}
	if h.exists("sys/fs/cgroup/kubepods/empty") || !h.exists("sys/fs/cgroup/kubepods/busy") || !h.exists("sys/fs/cgroup/kubepods") {
		t.Error("want the empty cgroup removed and the busy one, with its parent, kept")
	}
}

func TestRemoveLegacyKubeletDirRemovesTheDirectoryOfASocketAlreadyGone(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dir := shortKubeletDir(t, fx)
	if err := os.MkdirAll(filepath.Join(dir, "device-plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists (%v): an empty directory of a socket already gone was left", dir, err)
	}
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Errorf("second run = %v, want a no-op", err)
	}

	dir = shortKubeletDir(t, fx)
	staleSocket(t, filepath.Join(dir, "device-plugins/vendor.sock"))
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "device-plugins/vendor.sock")); err != nil {
		t.Errorf("another plugin's socket was removed: %v", err)
	}
}

func TestRemoveLegacyKubeletDirDoesNotRemoveASymlinkedSocketDirectory(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	dir := shortKubeletDir(t, fx)
	foreign, err := os.MkdirTemp("", "foreign")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(foreign) })
	if err := os.Symlink(foreign, filepath.Join(dir, "device-plugins")); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("the directory a symlink points to was removed: %v", err)
	}
}

func TestUnattributedNetns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(fx *hostEffects, h *datapathHost, t *testing.T)
		want  bool // an error asking to keep the state
	}{
		{"containerd metadata kept, a cni namespace mounted, no record", func(_ *hostEffects, h *datapathHost, t *testing.T) {
			h.mkdirs(t, "var/lib/stratum/containerd")
			h.setMounts(t, h.path("run/netns/cni-aaa"))
		}, true},
		{"no containerd metadata: nothing was run to attribute", func(_ *hostEffects, h *datapathHost, t *testing.T) {
			h.setMounts(t, h.path("run/netns/cni-aaa"))
		}, false},
		{"no cni namespace mounted", func(_ *hostEffects, h *datapathHost, t *testing.T) {
			h.mkdirs(t, "var/lib/stratum/containerd")
			h.setMounts(t, h.path("run/netns/ip-netns-user"), h.path("run/elsewhere/cni-x"))
		}, false},
		{"the containers were read this run", func(fx *hostEffects, h *datapathHost, t *testing.T) {
			h.mkdirs(t, "var/lib/stratum/containerd")
			h.setMounts(t, h.path("run/netns/cni-aaa"))
			fx.netnsEnumerated = true
		}, false},
		{"a saved record exists", func(fx *hostEffects, h *datapathHost, t *testing.T) {
			h.mkdirs(t, "var/lib/stratum/containerd")
			h.setMounts(t, h.path("run/netns/cni-aaa"))
			if err := saveNetnsRecord(fx, netnsRecord{Complete: true, Paths: []string{h.path("run/netns/cni-aaa")}}); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &hostEffects{}
			h := newDatapathHost(t, fx)
			fx.stratumNetns = map[string]bool{}
			unmounts(t, fx, h)
			tc.setup(fx, h, t)
			err := detachNetns(fx, true)
			if tc.want {
				if err == nil || !errors.Is(err, errKeepState) || !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), "cni-aaa") {
					t.Fatalf("err = %v, want it incomplete, naming the namespace, keeping the state", err)
				}
				if len(h.calls) != 0 {
					t.Errorf("calls %v: a namespace that could not be shown to be Stratum's was detached", h.calls)
				}
			} else if err != nil && strings.Contains(err.Error(), "could not be shown to be Stratum's") {
				t.Errorf("err = %v, want no unattributed-namespace error", err)
			}
		})
	}
}

func TestRunKeepsDataDirWhenNetnsOwnershipCannotBeEstablished(t *testing.T) {
	events, fx := runNode(t, "worker")
	h := newDatapathHost(t, fx) // after runNode, so its mount table and paths win
	fx.teardownDatapathFn = func() error { return teardownDatapath(io.Discard, fx) }
	fx.stratumNetns = map[string]bool{}
	h.mkdirs(t, "var/lib/stratum/containerd")
	h.touch(t, "run/netns/cni-aaa")
	h.setMounts(t, h.path("run/netns/cni-aaa"))
	var removed []string
	fx.removeAll = func(path string) error { removed = append(removed, path); return nil }
	var err error
	out := captureWriter(func(w io.Writer) { err = runUninstall(w, true, fx) })
	if err == nil || !strings.Contains(err.Error(), "could not be shown to be Stratum's") {
		t.Fatalf("runUninstall = %v, want the unattributed namespace reported", err)
	}
	if slices.Contains(removed, version.DataDir) || !slices.Contains(removed, version.ConfDir) {
		t.Errorf("removed = %q, want DataDir kept and the other targets removed", removed)
	}
	if !h.exists("run/netns/cni-aaa") || slices.ContainsFunc(h.calls, func(c string) bool { return strings.HasPrefix(c, "umount ") }) {
		t.Errorf("the namespace was touched: calls %v", h.calls)
	}
	if slices.Contains(*events, "mark node-a") || strings.Contains(out, "marked Node") || strings.Contains(out, "stratum uninstalled.") {
		t.Errorf("events = %q, output:\n%s; want no mark and no \"uninstalled\"", *events, out)
	}
}

// run can be called again in a persistent process (the MCP server).
func TestRepeatedRunDoesNotReuseNetnsEnumeration(t *testing.T) {
	events, fx := runNode(t, "worker")
	h := newDatapathHost(t, fx)
	fx.stopContainersFn = func() error { return stopContainers(io.Discard, fx) }
	fx.teardownDatapathFn = func() error { return teardownDatapath(io.Discard, fx) }
	fx.ctrRun = func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "namespaces ls -q":
			return "k8s.io", nil
		case "-n k8s.io containers ls -q":
			return "", nil
		case "-n k8s.io tasks ls":
			return "TASK PID STATUS\n", nil
		case "-n k8s.io sandboxes ls":
			return "ID CREATED RUNTIME\n", nil
		}
		return "", errors.New("unexpected ctr call")
	}
	var firstErr error
	captureWriter(func(w io.Writer) { firstErr = runUninstall(w, true, fx) })
	if firstErr != nil {
		t.Fatalf("first uninstall: %v", firstErr)
	}
	if !fx.netnsEnumerated {
		t.Fatal("the first call should have enumerated the containers")
	}
	if _, err := os.Stat(fx.netnsRecordPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no record: %v", err)
	}

	// later, in the same process: retained metadata and a mounted namespace.
	h.mkdirs(t, "var/lib/stratum/containerd")
	h.touch(t, "run/netns/cni-later")
	h.setMounts(t, h.path("run/netns/cni-later"))
	fx.unitActive = func(string) bool { return false }
	unitWanted = func(string) bool { return false }
	*events = nil
	var removed []string
	fx.removeAll = func(p string) error { removed = append(removed, p); return nil }
	var secondErr error
	captureWriter(func(w io.Writer) { secondErr = runUninstall(w, true, fx) })
	if !errors.Is(secondErr, errKeepState) {
		t.Errorf("the later call = %v, want the unattributed namespace reported, keeping the state", secondErr)
	}
	if slices.Contains(removed, version.DataDir) {
		t.Error("the later call deleted DataDir despite an unattributed mount")
	}
	if slices.Contains(*events, "mark node-a") {
		t.Error("the later call marked the worker despite an unattributed mount")
	}
	if !h.exists("run/netns/cni-later") {
		t.Error("the unattributed namespace was removed")
	}
}

func TestRunSeedsFreshDiscoveryAndKeepsTheSavedRecord(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	fx.stratumNetns = map[string]bool{h.path("run/netns/cni-aaa"): true}
	fx.stratumNetnsErr = errors.New("an earlier call's read error")
	fx.netnsEnumerated = true
	if err := saveNetnsRecord(fx, netnsRecord{Complete: true, Paths: []string{h.path("run/netns/cni-aaa")}}); err != nil {
		t.Fatal(err)
	}
	fx.acquireNodeLock = freeNodeLock
	fx.acquireEtcdDataLock = func() (func(), error) {
		if len(fx.stratumNetns) != 0 || fx.stratumNetnsErr != nil || fx.netnsEnumerated {
			t.Errorf("the run reached the host lock with a previous call's discovery: %v, %v, %v", fx.stratumNetns, fx.stratumNetnsErr, fx.netnsEnumerated)
		}
		return nil, errors.New("stop at the host lock")
	}
	if err := runUninstall(io.Discard, true, fx); err == nil || !strings.Contains(err.Error(), "stop at the host lock") {
		t.Fatalf("runUninstall = %v, want the host-lock stop", err)
	}
	if rec, err := loadNetnsRecord(fx); err != nil || rec == nil || len(rec.Paths) != 1 {
		t.Errorf("the saved record = %+v, %v; want it kept", rec, err)
	}
}

func TestForeignRuntimesDistinguishesAVanishedProcessFromAnUnreadableOne(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")

	// vanished: the entry has no comm (ENOENT), as after the process exited.
	h.mkdirs(t, "proc/4001")
	if err := removeKubepodsCgroups(fx); err != nil {
		t.Fatalf("a vanished process stopped the removal: %v", err)
	}
	if h.exists("sys/fs/cgroup/kubepods") {
		t.Error("the empty tree was kept because of a vanished process")
	}

	// unreadable: comm cannot be read as a file (EISDIR), while /proc is readable.
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1", "proc/4242/comm")
	err := removeKubepodsCgroups(fx)
	if err == nil || !strings.Contains(err.Error(), "could not check for another kubelet") || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("err = %v, want the unreadable process reported", err)
	}
	if !h.exists("sys/fs/cgroup/kubepods/pod1") {
		t.Error("the tree was removed without being able to inspect every process")
	}
}

func TestVanished(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	for err, want := range map[error]bool{
		os.ErrNotExist: true,
		&os.PathError{Op: "open", Err: syscall.ENOENT}: true,
		&os.PathError{Op: "read", Err: syscall.ESRCH}:  true,
		&os.PathError{Op: "open", Err: syscall.EACCES}: false,
		&os.PathError{Op: "read", Err: syscall.EIO}:    false,
		errors.New("other"):                            false,
	} {
		if got := vanished(err); got != want {
			t.Errorf("vanished(%v) = %v, want %v", err, got, want)
		}
	}
}

func TestStaleActiveSliceIsLookedAtAgainBeforeAnyStop(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	sd := &fakeSystemd{active: true, onQuery: func() {
		h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/busy")
		h.touch(t, "sys/fs/cgroup/kubepods.slice/busy/process") // stands in for a process
	}}
	h.systemd(t, fx, sd)
	err := removeKubepodsCgroups(fx)
	if err == nil || !strings.Contains(err.Error(), "still in use") {
		t.Fatalf("err = %v, want the in-use tree reported", err)
	}
	if sd.stops != 0 {
		t.Error("the slice was stopped after its cgroup had become populated")
	}

	h = newDatapathHost(t, fx)
	sd = &fakeSystemd{active: true, onQuery: func() { h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice") }}
	h.systemd(t, fx, sd)
	if err := removeKubepodsCgroups(fx); err != nil || sd.stops != 1 || h.exists("sys/fs/cgroup/kubepods.slice") {
		t.Errorf("err = %v, stops = %d, directory there = %v; want the empty tree removed and the unit stopped", err, sd.stops, h.exists("sys/fs/cgroup/kubepods.slice"))
	}
}

func TestARuntimeThatStartsDuringTheRemovalStopsTheStop(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice")
	sd := &fakeSystemd{active: true}
	h.systemd(t, fx, sd)
	var looks int
	fx.foreignRuntimesFn = func() ([]string, []string, error) {
		looks++
		if looks == 1 {
			return nil, nil, nil
		}
		return []string{"kubelet (pid 4242)"}, nil, nil
	}
	err := removeKubepodsCgroups(fx)
	if err == nil || !strings.Contains(err.Error(), "not stopping kubepods.slice") || !strings.Contains(err.Error(), "kubelet (pid 4242)") {
		t.Fatalf("err = %v, want the stop refused, naming the runtime", err)
	}
	if sd.stops != 0 || looks != 2 {
		t.Errorf("stops = %d after %d looks; want no stop, and the check made again before it", sd.stops, looks)
	}

	h = newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice")
	sd = &fakeSystemd{active: true}
	h.systemd(t, fx, sd)
	looks = 0
	fx.foreignRuntimesFn = func() ([]string, []string, error) {
		looks++
		if looks == 1 {
			return nil, nil, nil
		}
		return nil, nil, errors.New("proc: i/o error")
	}
	if err := removeKubepodsCgroups(fx); err == nil || sd.stops != 0 {
		t.Errorf("err = %v, stops = %d; want the stop refused", err, sd.stops)
	}
}

func TestSliceActivityIsNotAssumedInactive(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		active bool
		fail   bool
	}{
		"active":           {nil, true, false},
		"inactive (3)":     {fakeExit(3), false, false},
		"no such unit (4)": {fakeExit(4), false, false},
		"no systemctl":     {fmt.Errorf("systemctl: %w", exec.ErrNotFound), false, false},
		"a failed query":   {fakeExit(1), false, true},
		"another status":   {fakeExit(255), false, true},
		"a timeout":        {fmt.Errorf("systemctl is-active: %w", errCommandTimeout), false, true},
		"an unknown error": {errors.New("Failed to connect to bus"), false, true},
	} {
		t.Run(name, func(t *testing.T) {
			fx := &hostEffects{}
			newDatapathHost(t, fx)
			fx.runCmd = func(string, string, ...string) ([]byte, error) { return nil, tc.err }
			active, err := sliceActive(fx, "kubepods.slice")
			if active != tc.active || (err != nil) != tc.fail {
				t.Errorf("sliceActive = %v, %v; want active %v, failure %v", active, err, tc.active, tc.fail)
			}
		})
	}
}

func TestRemoveKubepodsCgroupsReportsAFailedActivityQuery(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	sd := &fakeSystemd{queryErr: fmt.Errorf("systemctl is-active: %w", errCommandTimeout)}
	h.systemd(t, fx, sd)
	err := removeKubepodsCgroups(fx)
	if !errors.Is(err, errCommandTimeout) || !strings.Contains(err.Error(), "could not tell whether") {
		t.Fatalf("err = %v, want the timeout reported", err)
	}
	if sd.stops != 0 {
		t.Error("a unit whose state was not known was stopped")
	}
}

func TestRemoveKubepodsCgroupsReportsAnErrorInspectingAfterTheStop(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice")
	h.systemd(t, fx, &fakeSystemd{active: true, onStop: func() {
		if err := os.Symlink("kubepods.slice", h.path("sys/fs/cgroup/kubepods.slice")); err != nil {
			t.Fatal(err)
		}
	}})
	err := removeKubepodsCgroups(fx)
	if err == nil || !strings.Contains(err.Error(), "after stopping kubepods.slice") {
		t.Errorf("err = %v, want the failed inspection reported", err)
	}
}

func TestTeardownDatapathSkipsCiliumWhileAnotherRuntimeRuns(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "run/netns/cni-aaa", "sys/class/net/cilium_host", "sys/class/net/eth0", "sys/fs/bpf/tc/globals/cilium_m", "run/cilium/x", "sys/fs/bpf/cilium/y")
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")
	h.setMounts(t, h.path("run/netns/cni-aaa"), h.path("run/cilium/cgroupv2"))
	addProc(t, h, "4242", "rke2-server")
	var cmds []string
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		cmds = append(cmds, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	unmounts(t, fx, h, h.path("run/netns/cni-aaa"), h.path("run/cilium/cgroupv2"))

	err := teardownDatapath(io.Discard, fx)
	if err == nil || !errors.Is(err, errRuntimeRunning) || !strings.Contains(err.Error(), "rke2-server (pid 4242)") || !strings.Contains(err.Error(), "left Cilium's datapath") {
		t.Fatalf("err = %v, want the skipped Cilium teardown reported, naming the runtime", err)
	}
	if c := ciliumCommands(cmds); len(c) != 0 {
		t.Errorf("Cilium commands run with another runtime present: %q", c)
	}
	for _, kept := range []string{"sys/class/net/cilium_host", "sys/fs/bpf/tc/globals/cilium_m", "run/cilium/x", "sys/fs/bpf/cilium/y"} {
		if !h.exists(kept) {
			t.Errorf("%s was removed", kept)
		}
	}
	// what is proved to be Stratum's still goes: its netns; Cilium's mount stays.
	if want := []string{"umount " + h.path("run/netns/cni-aaa")}; !slices.Equal(h.calls, want) {
		t.Errorf("unmounts = %q, want only the proved namespace: %q", h.calls, want)
	}
	if h.exists("run/netns/cni-aaa") {
		t.Error("the proved namespace file was not removed")
	}
	if !h.exists("sys/fs/cgroup/kubepods/pod1") {
		t.Error("the kubepods tree was removed with another runtime present")
	}
}

func TestTeardownDatapathSkipsCiliumWhenTheRuntimeCheckFails(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/class/net/cilium_host", "run/cilium/x")
	fx.procRoot = h.path("no-proc")
	var cmds []string
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		cmds = append(cmds, name)
		return nil, nil
	}
	err := teardownDatapath(io.Discard, fx)
	if err == nil || !strings.Contains(err.Error(), "could not check for another kubelet or container runtime") {
		t.Fatalf("err = %v, want the failed check reported", err)
	}
	if c := ciliumCommands(cmds); len(c) != 0 || !h.exists("sys/class/net/cilium_host") || !h.exists("run/cilium/x") {
		t.Errorf("Cilium was touched without being able to check: commands %q", c)
	}
}

func TestForeignRuntimesFindsEmbeddedKubeletsAndOtherRuntimes(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	want := []string{"k3s", "k3s-server", "k3s-agent", "rke2", "rke2-server", "rke2-agent", "kubelite", "k0s", "cilium-agent", "cilium-envoy", "cri-dockerd", "podman", "conmon", "crun", "kubelet", "containerd", "containerd-shim", "crio", "dockerd"}
	not := []string{"bash", "sshd", "kubectl", "kube-proxy", "stratum", "systemd", "sleep", "conmonx", "podmanager", "crunch", "k0sx", "kubeliteX", "cilium", "cilium-agentx"}
	pid := 100
	pids := map[string]string{}
	for _, c := range append(slices.Clone(want), not...) {
		pid++
		pids[fmt.Sprint(pid)] = c
		addProc(t, h, fmt.Sprint(pid), c)
	}
	got, _, err := foreignRuntimes(fx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, g := range got {
		found[g[:strings.Index(g, " (")]] = true
	}
	for _, w := range want {
		if !found[w] {
			t.Errorf("%q was not recognized as a runtime", w)
		}
	}
	for _, n := range not {
		if found[n] {
			t.Errorf("%q was recognized as a runtime", n)
		}
	}
}

// Docker as on a developer's machine: components under system.slice, plus a zombie shim.
func dockerHost(t *testing.T, h *datapathHost) {
	t.Helper()
	addProc(t, h, "2484", "containerd")
	addProc(t, h, "3241", "dockerd")
	addProc(t, h, "4149", "containerd-shim")
	setProcCgroup(t, h, "4149", "0::/system.slice/docker-0123abcd.scope")
	addProc(t, h, "4150", "nginx") // the container's own process
	setProcCgroup(t, h, "4150", "0::/system.slice/docker-0123abcd.scope")
	addProc(t, h, "4151", "containerd-shim")
	if err := os.WriteFile(h.path("proc/4151/stat"), []byte("4151 (containerd-shim) Z 1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestForeignRuntimesTakesARuntimeWithoutPodsForABystander(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	dockerHost(t, h)
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice") // left empty
	h.touch(t, "sys/fs/cgroup/kubepods.slice/cgroup.procs")
	foreign, bystanders, err := foreignRuntimes(fx)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(bystanders)
	want := []string{"containerd (pid 2484)", "containerd-shim (pid 4149)", "dockerd (pid 3241)"} // 4151 is a zombie
	if len(foreign) != 0 || !slices.Equal(bystanders, want) {
		t.Errorf("foreign = %q, bystanders = %q; want none foreign and %q", foreign, bystanders, want)
	}

	// Each runtime alone, Podman's included, is one too.
	for _, comm := range []string{"containerd", "crio", "dockerd", "cri-dockerd", "podman", "conmon", "crun"} {
		h := newDatapathHost(t, fx)
		addProc(t, h, "900", comm)
		foreign, bystanders, err := foreignRuntimes(fx)
		if err != nil || len(foreign) != 0 || !slices.Equal(bystanders, []string{comm + " (pid 900)"}) {
			t.Errorf("%s: foreign = %q, bystanders = %q, err = %v; want it a bystander", comm, foreign, bystanders, err)
		}
	}
}

// a running kubelet or Cilium agent under any name, or any pod, makes every runtime foreign.
func TestForeignRuntimesTakesARuntimeForForeignWithAKubeletOrAPod(t *testing.T) {
	cases := map[string]func(t *testing.T, h *datapathHost){
		"cri-dockerd": func(t *testing.T, h *datapathHost) {
			addProc(t, h, "899", "cri-dockerd")
			addProc(t, h, "900", "kubelet")
		},
		"systemd pod": func(t *testing.T, h *datapathHost) {
			addProc(t, h, "900", "pause")
			setProcCgroup(t, h, "900", "0::/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod1.slice/cri-containerd-ab.scope")
		},
		"cgroupfs v1 pod": func(t *testing.T, h *datapathHost) {
			addProc(t, h, "900", "pause")
			setProcCgroup(t, h, "900", "12:pids:/system.slice/x.service", "4:memory:/kubepods/burstable/pod1/ab", "0::/")
		},
		"nested pod": func(t *testing.T, h *datapathHost) {
			addProc(t, h, "900", "pause")
			setProcCgroup(t, h, "900", "0::/system.slice/docker-kind.scope/kubelet.slice/kubelet-kubepods.slice/kubelet-kubepods-pod1.slice")
		},
		"pod not in /proc": func(t *testing.T, h *datapathHost) {
			h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")
			if err := os.WriteFile(h.path("sys/fs/cgroup/kubepods/pod1/cgroup.procs"), []byte("77\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	// each name alone, under system.slice and with no pod: an agent run by Docker.
	for _, comm := range []string{"kubelet", "k3s", "k3s-server", "rke2-agent", "kubelite", "k0s", "cilium-agent", "cilium-envoy"} {
		cases[comm] = func(t *testing.T, h *datapathHost) { addProc(t, h, "900", comm) }
	}
	for name, add := range cases {
		t.Run(name, func(t *testing.T) {
			fx := &hostEffects{}
			h := newDatapathHost(t, fx)
			dockerHost(t, h)
			add(t, h)
			foreign, bystanders, err := foreignRuntimes(fx)
			if err != nil {
				t.Fatal(err)
			}
			if len(bystanders) != 0 || !slices.Contains(foreign, "dockerd (pid 3241)") || !slices.Contains(foreign, "containerd (pid 2484)") {
				t.Errorf("foreign = %q, bystanders = %q; want Docker foreign", foreign, bystanders)
			}
			// the pod's process is named, so an orphan of Stratum's own is not Docker's.
			if name == "systemd pod" && !slices.Contains(foreign, "pause under kubepods (pid 900)") {
				t.Errorf("foreign = %q, want the pod's process named", foreign)
			}
		})
	}
}

func TestForeignRuntimesFailsWhenAProcessCgroupCannotBeRead(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	addProc(t, h, "900", "sleep")
	_ = os.Remove(h.path("proc/900/cgroup"))
	h.mkdirs(t, "proc/900/cgroup") // EISDIR: there, and unreadable
	if foreign, bystanders, err := foreignRuntimes(fx); err != nil || len(foreign)+len(bystanders) != 0 {
		t.Fatalf("foreign = %q, bystanders = %q, err = %v; want nothing with no runtime running", foreign, bystanders, err)
	}
	dockerHost(t, h)
	_, bystanders, err := foreignRuntimes(fx)
	if err == nil || !strings.Contains(err.Error(), "process 900") || len(bystanders) != 0 {
		t.Fatalf("bystanders = %q, err = %v; want the unreadable cgroup reported", bystanders, err)
	}
	h.touch(t, "sys/class/net/cilium_host")
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")
	if err := teardownDatapath(io.Discard, fx); err == nil || !h.exists("sys/class/net/cilium_host") || !h.exists("sys/fs/cgroup/kubepods/pod1") {
		t.Errorf("err = %v; want the Cilium link and kubepods kept", err)
	}
}

// the kubepods tree is read for the pods /proc does not show under cgroup v2.
func TestForeignRuntimesFailsWhenTheKubepodsTreeCannotBeRead(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T, h *datapathHost){
		"unreadable cgroup.procs": func(t *testing.T, h *datapathHost) {
			h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/pod1", "elsewhere")
			// a symlink to a directory: not a directory entry, and EISDIR to read.
			if err := os.Symlink(h.path("elsewhere"), h.path("sys/fs/cgroup/kubepods.slice/pod1/cgroup.procs")); err != nil {
				t.Fatal(err)
			}
		},
		"unlistable cgroup": func(t *testing.T, h *datapathHost) {
			if os.Geteuid() == 0 {
				t.Skip("root reads a directory whatever its mode")
			}
			h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")
			if err := os.Chmod(h.path("sys/fs/cgroup/kubepods/pod1"), 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(h.path("sys/fs/cgroup/kubepods/pod1"), 0o755) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx := &hostEffects{}
			h := newDatapathHost(t, fx)
			dockerHost(t, h)
			mk(t, h)
			_, bystanders, err := foreignRuntimes(fx)
			if err == nil || !strings.Contains(err.Error(), "could not check whether anything runs under kubepods") || len(bystanders) != 0 {
				t.Fatalf("bystanders = %q, err = %v; want the unreadable tree reported", bystanders, err)
			}
		})
	}
}

func TestTeardownDatapathRemovesCiliumBesideDocker(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	dockerHost(t, h)
	h.touch(t, "sys/class/net/cilium_host", "sys/class/net/docker0", "run/cilium/x")
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice")
	sliceUp := true
	fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
		h.calls = append(h.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		switch name {
		case "ip":
			_ = os.Remove(h.path("sys/class/net/" + args[2]))
		case "systemctl":
			if args[0] == "is-active" {
				if sliceUp {
					return nil, nil
				}
				return nil, fakeExit(3)
			}
			sliceUp = false
		}
		return nil, nil
	}
	var out strings.Builder
	if err := teardownDatapath(&out, fx); err != nil {
		t.Fatalf("err = %v, want none: Docker uses neither", err)
	}
	for _, gone := range []string{"sys/class/net/cilium_host", "run/cilium", "sys/fs/cgroup/kubepods.slice"} {
		if h.exists(gone) {
			t.Errorf("%s is still there", gone)
		}
	}
	if !h.exists("sys/class/net/docker0") {
		t.Error("Docker's bridge was removed")
	}
	if !slices.Contains(h.calls, "systemctl stop kubepods.slice") {
		t.Errorf("calls = %q, want kubepods.slice stopped", h.calls)
	}
	if s := out.String(); !strings.Contains(s, "dockerd (pid 3241)") || !strings.Contains(s, "not touched") {
		t.Errorf("output = %q, want Docker named and said to be left alone", s)
	}
}

// the Cilium teardown commands among cmds (the kubepods step runs systemctl).
func ciliumCommands(cmds []string) []string {
	var out []string
	for _, c := range cmds {
		switch strings.Fields(c)[0] {
		case "ip", "tc", "iptables", "ip6tables", "iptables-save", "ip6tables-save", "bpftool":
			out = append(out, c)
		}
	}
	return out
}

func TestRemoveCiliumStateReportsARmdirThatFailsForAnotherReason(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes from a read-only directory")
	}
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.touch(t, "sys/fs/bpf/tc/globals/cilium_ct4_global")
	tc := h.path("sys/fs/bpf/tc")
	if err := os.Chmod(tc, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tc, 0o755) })
	if err := removeCiliumState(fx); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the failed rmdir of globals reported", err)
	}

	// a foreign pin keeping the directory is still not an error.
	h = newDatapathHost(t, fx)
	h.touch(t, "sys/fs/bpf/tc/globals/cilium_ct4_global", "sys/fs/bpf/tc/globals/foreign_map")
	if err := removeCiliumState(fx); err != nil {
		t.Errorf("err = %v, want a directory that holds another program's pin kept silently", err)
	}
}

// A command that fails with no output must not end in a dangling ": ".
func TestExecCmdErrorsCarryOutputOnlyWhenThereIsSome(t *testing.T) {
	t.Parallel() // host-free: touches only its own values
	_, err := execCmd("", "stratum-no-such-tool")
	if !errors.Is(err, exec.ErrNotFound) || strings.HasSuffix(err.Error(), ": ") {
		t.Errorf("err = %q, want ErrNotFound with nothing after it", err)
	}
	_, err = execCmd("", "sh", "-c", "exit 3")
	if err == nil || strings.HasSuffix(err.Error(), ": ") {
		t.Errorf("err = %q, want the exit status last", err)
	}
	_, err = execCmd("", "sh", "-c", "echo boom; exit 3")
	if err == nil || !strings.HasSuffix(err.Error(), ": boom") {
		t.Errorf("err = %q, want the output last", err)
	}
}

func writeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestExecCmdUsesTheFixedToolPath(t *testing.T) {
	callers, fixed := filepath.Join(t.TempDir(), "callers"), filepath.Join(t.TempDir(), "fixed")
	writeTool(t, callers, "shadowed", "echo caller")
	writeTool(t, fixed, "shadowed", `echo "fixed $PATH"`)
	writeTool(t, callers, "only-callers", "echo caller")
	t.Setenv("PATH", callers+":"+os.Getenv("PATH"))
	swap(t, &toolPath, fixed+":/usr/bin:/bin")

	out, err := execCmd("", "shadowed")
	if err != nil || strings.TrimSpace(string(out)) != "fixed "+toolPath {
		t.Errorf("out = %q, err = %v; want the fixed directory's tool, run with the fixed PATH", out, err)
	}
	if _, err := execCmd("", "only-callers"); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("err = %v, want a tool only on the caller's PATH not found", err)
	}
}

func TestUnmountUsesTheFixedToolPath(t *testing.T) {
	callers, fixed := filepath.Join(t.TempDir(), "callers"), filepath.Join(t.TempDir(), "fixed")
	marker := filepath.Join(t.TempDir(), "args")
	writeTool(t, callers, "umount", "exit 1")
	writeTool(t, fixed, "umount", `echo "$@" > `+marker)
	t.Setenv("PATH", callers+":"+os.Getenv("PATH"))
	swap(t, &toolPath, fixed)

	if err := umountMount("/some/mount"); err != nil {
		t.Fatalf("err = %v, want the fixed directory's umount run", err)
	}
	if b, _ := os.ReadFile(marker); strings.TrimSpace(string(b)) != "-l /some/mount" {
		t.Errorf("umount args = %q", b)
	}
}

func TestRemoveKubepodsCgroupsReportsARuntimeWhenTheQueryMadeTheTree(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	addProc(t, h, "4242", "kubelet")
	h.systemd(t, fx, &fakeSystemd{active: true, onQuery: func() { h.mkdirs(t, "sys/fs/cgroup/kubepods.slice") }})
	err := removeKubepodsCgroups(fx)
	if err == nil || !strings.Contains(err.Error(), "kubelet (pid 4242)") {
		t.Fatalf("err = %v, want the running runtime reported", err)
	}
	if !h.exists("sys/fs/cgroup/kubepods.slice") {
		t.Error("the tree was removed with another runtime running")
	}

	h = newDatapathHost(t, fx)
	addProc(t, h, "4242", "kubelet")
	h.systemd(t, fx, &fakeSystemd{active: true})
	if err := removeKubepodsCgroups(fx); err != nil {
		t.Errorf("err = %v, want no report when there is no tree to keep", err)
	}
}

// a bare name is looked up on the caller's PATH, which a slow entry can stall.
func TestToolCmdIsBuiltFromTheResolvedPath(t *testing.T) {
	fixed := t.TempDir()
	writeTool(t, fixed, "tool", "true")
	t.Setenv("PATH", filepath.Join(t.TempDir(), "absent"))
	swap(t, &toolPath, fixed)

	cmd := toolCmd(context.Background(), "tool", "a")
	if want := filepath.Join(fixed, "tool"); cmd.Err != nil || cmd.Path != want || cmd.Args[0] != want {
		t.Errorf("Path = %q, Args[0] = %q, Err = %v; want the command built from %q", cmd.Path, cmd.Args[0], cmd.Err, want)
	}
	if cmd := toolCmd(context.Background(), "missing"); !errors.Is(cmd.Err, exec.ErrNotFound) {
		t.Errorf("Err = %v, want ErrNotFound for a tool that is not in toolPath", cmd.Err)
	}
}

// the tools run as root: a variable that loads code must not reach them.
func TestToolCmdEnvIsExplicit(t *testing.T) {
	t.Setenv("XTABLES_LIBDIR", "/evil")
	t.Setenv("TC_LIB_DIR", "/evil")
	swap(t, &toolPath, "/tp")

	got := toolCmd(context.Background(), "tool").Env
	if want := []string{"PATH=/tp", "LC_ALL=C"}; !slices.Equal(got, want) {
		t.Errorf("Env = %q, want exactly %q", got, want)
	}
}

func TestUmountMountErrorHasNoTrailingSeparator(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "umount", "exit 1")
	swap(t, &toolPath, dir)

	err := umountMount("/mnt")
	if err == nil || strings.HasSuffix(err.Error(), ": ") {
		t.Errorf("err = %v, want an error without a trailing \": \"", err)
	}
}

const cgroupShowENODEV = "bpftool cgroup show /sys/fs/cgroup: exit status 255: Error: can't query bpf programs attached to /sys/fs/cgroup: No such device"

func progShow(progs ...[3]string) []byte {
	var parts []string
	for _, p := range progs {
		parts = append(parts, fmt.Sprintf(`{"id":%s,"type":%q,"name":%q,"tag":"0011223344556677"}`, p[0], p[1], p[2]))
	}
	return []byte("[" + strings.Join(parts, ",") + "]")
}

func TestRemoveCiliumCgroupProgramsFallsBackWhenBpftoolCannotListTheCgroup(t *testing.T) {
	for _, showErr := range []string{
		cgroupShowENODEV,
		"bpftool cgroup show /sys/fs/cgroup: exit status 255: Error: can't query bpf programs attached to /sys/fs/cgroup: No such device or address",
		"bpftool cgroup show /sys/fs/cgroup: exit status 255: Error: can't query bpf programs attached to /sys/fs/cgroup: Invalid argument",
	} {
		t.Run(showErr[strings.LastIndex(showErr, ": ")+2:], func(t *testing.T) {
			fx := &hostEffects{}
			h := newDatapathHost(t, fx)
			fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
				h.calls = append(h.calls, name+" "+strings.Join(args, " "))
				if args[0] == "cgroup" {
					return nil, errors.New(showErr)
				}
				return progShow(
					[3]string{"12", "sched_cls", "cil_from_contai"},
					[3]string{"13", "cgroup_sock_addr", "other_connect4"},
					[3]string{"14", "cgroup_device", "sd_devices"},
				), nil
			}
			if err := removeCiliumCgroupPrograms(fx); err != nil {
				t.Fatalf("err = %v, want none: no Cilium cgroup program is loaded, so none is attached", err)
			}
			want := []string{"bpftool cgroup show " + h.path("sys/fs/cgroup"), "bpftool -j prog show"}
			if !slices.Equal(h.calls, want) {
				t.Errorf("calls = %q, want %q: no detach without a listing", h.calls, want)
			}
		})
	}
}

func TestRemoveCiliumCgroupProgramsFallbackWaitsForProgramsTheKernelIsReleasing(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	swap(t, &cgroupProgramWait, time.Minute)
	swap(t, &pollInterval, time.Millisecond)
	lists := 0
	fx.runCmd = func(_, _ string, args ...string) ([]byte, error) {
		if args[0] == "cgroup" {
			return nil, errors.New(cgroupShowENODEV)
		}
		lists++
		if lists <= 2 {
			return progShow([3]string{"77", "cgroup_sock_addr", "cil_sock4_conne"}), nil
		}
		return progShow(), nil
	}
	if err := removeCiliumCgroupPrograms(fx); err != nil || lists != 3 {
		t.Fatalf("err = %v after %d listings, want none after 3", err, lists)
	}
}

func TestRemoveCiliumCgroupProgramsFallbackReportsProgramsStillLoaded(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	swap(t, &cgroupProgramWait, 0)
	fx.runCmd = func(_, _ string, args ...string) ([]byte, error) {
		if args[0] == "cgroup" {
			return nil, errors.New(cgroupShowENODEV)
		}
		return progShow([3]string{"77", "cgroup_sock_addr", "cil_sock4_conne"}, [3]string{"78", "cgroup_sock", "cil_sock4_post_"}), nil
	}
	err := removeCiliumCgroupPrograms(fx)
	if !errors.Is(err, errCgroupShowBug) {
		t.Fatalf("err = %v, want the listing failure reported", err)
	}
	for _, want := range []string{"cil_sock4_conne (id 77, cgroup_sock_addr)", "cil_sock4_post_ (id 78, cgroup_sock)", "bpftool to 7.6", "No such device"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, missing %q", err, want)
		}
	}
}

func TestRemoveCiliumCgroupProgramsFallbackReportsAFailedProgramListing(t *testing.T) {
	fx := &hostEffects{}
	newDatapathHost(t, fx)
	fx.runCmd = func(_, _ string, args ...string) ([]byte, error) {
		if args[0] == "cgroup" {
			return nil, errors.New(cgroupShowENODEV)
		}
		return nil, errors.New("bpftool -j prog show: exit status 255: Error: can't get next program: Operation not permitted")
	}
	err := removeCiliumCgroupPrograms(fx)
	if err == nil || !strings.Contains(err.Error(), "No such device") || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("err = %v, want both failures", err)
	}
	fx.runCmd = func(_, _ string, args ...string) ([]byte, error) {
		if args[0] == "cgroup" {
			return nil, errors.New(cgroupShowENODEV)
		}
		return []byte("not json"), nil
	}
	if err := removeCiliumCgroupPrograms(fx); err == nil || !strings.Contains(err.Error(), "bpftool -j prog show") {
		t.Fatalf("err = %v, want an unreadable listing reported", err)
	}
}

func TestRemoveCiliumCgroupProgramsDoesNotFallBackOnOtherListingErrors(t *testing.T) {
	for _, showErr := range []string{
		"bpftool cgroup show /sys/fs/cgroup: exit status 255: Error: can't open cgroup /sys/fs/cgroup: Permission denied",
		"bpftool cgroup show /sys/fs/cgroup: exit status 255: Error: can't query bpf programs attached to /sys/fs/cgroup: Operation not permitted",
	} {
		t.Run(showErr[strings.LastIndex(showErr, ": ")+2:], func(t *testing.T) {
			fx := &hostEffects{}
			h := newDatapathHost(t, fx)
			fx.runCmd = func(_, name string, args ...string) ([]byte, error) {
				h.calls = append(h.calls, name+" "+strings.Join(args, " "))
				if args[0] == "cgroup" {
					return nil, errors.New(showErr)
				}
				return progShow(), nil
			}
			if err := removeCiliumCgroupPrograms(fx); err == nil || errors.Is(err, errCgroupShowBug) || len(h.calls) != 1 {
				t.Fatalf("err = %v, calls %q; want the listing error alone, with no fallback", err, h.calls)
			}
		})
	}
}

func TestLoadedCiliumCgroupProgramsCountsATypeBpftoolCannotName(t *testing.T) {
	out := []byte(`[{"id":77,"type":31,"name":"cil_sock4_conne"},{"id":12,"type":"sched_cls","name":"cil_from_contai"},{"id":13,"type":31,"name":"other"}]`)
	got, err := loadedCiliumCgroupPrograms(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cil_sock4_conne (id 77, type 31)"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q: a Cilium program of a type bpftool cannot name may be a cgroup one", got, want)
	}
}

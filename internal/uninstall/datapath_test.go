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

// datapathHost points every datapath path at a temp dir, so nothing here reads
// or changes the machine the tests run on, and records each command and unmount
// into calls.
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
	// Sequential by swap's stated exception: it shortens the shared
	// cgroupRemoveWait and cgroupProgramWait.
	h := &datapathHost{root: t.TempDir()}
	// The filesystem layout and the discovery state are the run's own values:
	// each test points its run's fields at this temp tree, so nothing here
	// reads or changes the machine the tests run on, and two runs share
	// nothing.
	fx.netnsDir = h.path("run/netns")
	fx.ciliumRunDir = h.path("run/cilium")
	fx.bpfRoot = h.path("sys/fs/bpf")
	fx.netClassDir = h.path("sys/class/net")
	fx.cgroupRoot = h.path("sys/fs/cgroup")
	fx.legacyKubeletDir = h.path("var/lib/kubelet")
	swap(t, &cgroupRemoveWait, 0)
	swap(t, &cgroupProgramWait, 0) // a failed detach is listed again once
	fx.mountInfoPath = h.path("mountinfo")
	// This node's containers' netns files; any other cni-* is a host
	// containerd's, and must be left alone.
	fx.stratumNetns = map[string]bool{h.path("run/netns/cni-aaa"): true, h.path("run/netns/cni-bbb"): true}
	fx.stratumNetnsErr = nil
	fx.netnsRecordPath = h.path("netns-record.json")
	fx.netnsEnumerated = false
	fx.containerdStateDir = h.path("var/lib/stratum/containerd") // absent unless a test makes it
	h.mkdirs(t, "proc")
	fx.procRoot = h.path("proc") // no other kubelet or runtime unless a test adds one
	// The real runtime check, reading this run's /proc: tests that need a
	// different answer between two looks re-set fx.foreignRuntimesFn.
	fx.foreignRuntimesFn = func() ([]string, []string, error) { return foreignRuntimes(fx) }
	h.setMounts(t)
	fx.unmountFn = func(mp string) error { h.calls = append(h.calls, "umount "+mp); return nil }
	fx.runCmd = func(stdin, name string, args ...string) ([]byte, error) {
		h.calls = append(h.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		return nil, nil
	}
	return h
}

// setMounts writes mountinfo with one mount point per path.
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
		// The jumps first, each as the rule is printed, so only it matches.
		{"-w", "5", "-t", "nat", "-D", "PREROUTING", "-m", "comment", "--comment", "cilium-feeder: CILIUM_PRE_nat", "-j", "CILIUM_PRE_nat"},
		{"-w", "5", "-t", "nat", "-D", "POSTROUTING", "-m", "comment", "--comment", "cilium-feeder: CILIUM_POST_nat", "-j", "CILIUM_POST_nat"},
		{"-w", "5", "-t", "filter", "-D", "INPUT", "-m", "comment", "--comment", "cilium-feeder: CILIUM_INPUT", "-j", "CILIUM_INPUT"},
		// Then every Cilium chain flushed, then deleted: one cannot be deleted
		// while another still jumps to it.
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
	// Refuse before even reading mountinfo; an absent layout must not depend
	// on whether the host's mount table is accessible.
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
	// An unmount takes its mount out of mountinfo, as the kernel does; the file
	// must still be there when it happens.
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
	// Deleting one end of a pair takes the other; the link itself goes.
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
	// Only Cilium's rule and chain are touched, one at a time, and the ruleset
	// is never replayed, so a rule another program adds meanwhile is not lost.
	want := []string{
		"iptables -w 5 -t filter -D INPUT -j CILIUM_INPUT",
		"iptables -w 5 -t filter -F CILIUM_INPUT",
		"iptables -w 5 -t filter -X CILIUM_INPUT",
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}

	// After it, the dump has none of Cilium's, and a second run runs nothing.
	dumps["iptables-save"] = "*filter\n:INPUT ACCEPT [0:0]\n-A INPUT -p tcp --dport 22 -j ACCEPT\nCOMMIT\n"
	calls = nil
	if err := removeCiliumChains(fx); err != nil || len(calls) != 0 {
		t.Errorf("second run = %v, calls %q; want a no-op", err, calls)
	}
}

// A host without iptables-save can still have Cilium's rules, which the agent
// adds with the tools in its own container: not knowing is an error.
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

// The agent renames its chains to OLD_CILIUM_* while it reinstalls them at
// every start; an agent that stops between the rename and the cleanup leaves
// them behind, and they can keep their rules. They are removed exactly like
// the CILIUM_ ones, the jumps to them first, and an OLD_CILIUM_ name inside
// a comment selects nothing.
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

	// An empty dump, or one with none of Cilium's, runs nothing.
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

// Another program's pins in the trees Cilium shares are not Cilium's, however
// they are named, and neither are the directories that hold them.
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

// RemoveAll does not stop at a mount boundary: /run/cilium holds Cilium's
// cgroup2 mount, so a mount that is still there, or a mount table that cannot
// be read, leaves the directory and what is mounted in it alone.
func TestRemoveCiliumStateDoesNotRemoveThroughAMount(t *testing.T) {
	for _, tc := range []struct {
		name           string
		setup          func(h *datapathHost, t *testing.T)
		bpfSubtreeGone bool // a mount elsewhere does not stop the unmounted subtree's removal; an unreadable table stops both
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
		// A name that only contains cil_ or cilium is some other program's.
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

// A Cilium filter in chain 7 and a foreign filter in chain 0 can share
// protocol, pref and handle; a delete without the chain hits chain 0.
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
		// No chain in the line means chain 0.
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

// shortKubeletDir points the run's legacyKubeletDir at a directory whose
// sockets fit sun_path (about 104 bytes), which a t.TempDir() path with a
// socket below it does not.
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

// staleSocket creates a unix socket file nothing listens on, as a killed
// kubelet leaves.
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

// Stratum's kubelet leaves its device-plugin socket at a path fixed in kubelet,
// outside --root-dir: found by the integration check. It is stale once
// the kubelet is gone, so it, and the directories it empties, are removed.
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

// Stratum's kubelet serves pod-resources under --root-dir, so a pod-resources
// socket under /var/lib/kubelet is another kubelet's, stale or not, and an
// empty pod-resources directory there was made by something else, such as a
// pod's hostPath on the upstream default path. Both are kept, with
// /var/lib/kubelet; the device-plugin socket beside them still goes.
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
			// And says the directory is the kubelet's default, not Stratum's
			// kubelet root, which an operator reads it as otherwise.
			if !strings.Contains(out.String(), "left "+dir+" in place") || !strings.Contains(out.String(), "the kubelet's default directory") {
				t.Errorf("output = %q, want it to say %s, the kubelet's default directory, was left in place", out.String(), dir)
			}
		})
	}
}

// Anything that is not Stratum's kubelet's stale socket is kept, and so is the
// directory it is in: a live listener, another plugin's socket, a regular file
// with the kubelet's name.
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
	// A live socket at the kubelet's path: someone answers, so it is not left behind.
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

// Only the stale socket goes when another plugin's is beside it.
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

// fakeSystemd stands for the node's systemd as far as the kubepods slice goes.
type fakeSystemd struct {
	active    bool
	stopErr   error
	queryErr  error  // what is-active answers instead, when set
	onQuery   func() // what the query does while it runs
	onStop    func() // what stopping does to the cgroup directory
	stops     int
	dirAtStop *bool // set by stop: whether the slice's directory was still there
}

// fakeExit is a command's exit status, as exec.ExitError reports it.
type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

// systemd swaps runCmd for one that answers systemctl from f and records it in
// h.calls, passing everything else to the runCmd it replaces.
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
	// The slice is stopped, once the tree is gone and so known to be empty:
	// systemd owns these cgroups, and left active it makes the top one again.
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

// systemd's unit outlives the directory: after an earlier run removed it, or
// systemd made it again empty, an active unit with no directory is still stopped.
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

// systemd makes the directory again while the unit is stopped (it did, on a
// real node, at the daemon-reload): the empty directory is removed again.
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

// With another kubelet or runtime running, the shared slice is neither removed
// nor stopped: stopping it would stop their pods.
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

// A cgroup that is still in use is not emptied by stopping its slice.
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
	// Not loaded, or no systemctl: nothing to stop, not a failure.
	for _, e := range []error{errors.New("systemctl stop kubepods.slice: exit status 5: Unit kubepods.slice not loaded."), fmt.Errorf("systemctl: %w", exec.ErrNotFound)} {
		h.mkdirs(t, "sys/fs/cgroup/kubepods.slice")
		h.systemd(t, fx, &fakeSystemd{active: true, stopErr: e})
		if err := removeKubepodsCgroups(fx); err != nil {
			t.Errorf("err = %v for %v, want none", err, e)
		}
	}
	// Anything else is reported.
	h.mkdirs(t, "sys/fs/cgroup/kubepods.slice")
	h.systemd(t, fx, &fakeSystemd{active: true, stopErr: errors.New("systemctl stop kubepods.slice: exit status 1: Failed to stop")})
	if err := removeKubepodsCgroups(fx); err == nil || !strings.Contains(err.Error(), "still owns the cgroups") {
		t.Errorf("err = %v, want the failed stop reported", err)
	}
}

// Under the cgroupfs driver there is no unit: systemctl is not run at all.
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

// A cgroup that still has a process in it cannot be removed; the leftover must
// be reported, not mistaken for gone.
func TestRemoveKubepodsCgroupsReportsACgroupThatStays(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")
	// A regular file stands in for a busy cgroup: rmdir of its parent fails.
	h.touch(t, "sys/fs/cgroup/kubepods/pod1/busy")
	err := removeKubepodsCgroups(fx)
	if err == nil {
		t.Fatal("err = nil, want the cgroup that could not be removed")
	}
	if !h.exists("sys/fs/cgroup/kubepods/pod1") {
		t.Error("the busy cgroup was removed")
	}
}

// An error other than "does not exist" is not absence.
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

// A directory that cannot be read is not known to be empty.
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

// TestTeardownDatapathOrder pins the order the node needs: netns
// mounts detached, then Cilium's links, chains and pins, then the cgroups. And
// a second run, with everything gone, issues no command at all.
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
		// The slice is stopped but the cgroup directory stays until removed.
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
	// The only commands a clean node sees are the read-only dumps.
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

// A datapath failure is reported at the end, and the rest of the teardown, the
// delete loop included, still runs.
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

// A missing tc fails the uninstall, after everything that does not need it has
// run: the other datapath steps and the delete loop.
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

// The shared tc/globals tree gets the same mount check as the directories
// Cilium owns: an unreadable table, or a mount at or below tc/, leaves the
// cilium_* pins alone and is reported, while the independent cleanup goes on.
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

// The host's own bpffs mount, at the root of the tree, is not in the way.
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

// On a worker the datapath failure must not cost it the Node mark: the loop
// removes the credential the mark needs, and a retry could not mark.
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

// A failure removing state still skips the mark, as before: the state, and so
// the credential, may still be there for a retry to mark with. And a datapath
// failure no longer hides it.
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

// What a rule jumps to is read from its parsed -j/-g option, never from text
// anywhere in the line: a --comment that says "-j CILIUM_INPUT" does not make
// its rule a Cilium feeder, and deleting it (-D with the exact rule) would
// delete a foreign rule. Real feeders, jump and goto, are still found.
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

// workerRun is runNode for a worker with the datapath teardown stubbed, and
// removeAll failing only for the paths in failing, recording the rest.
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

// Marking follows whether the node's state (DataDir, which holds the credential
// the mark uses) was deleted, apart from the rest of the cleanup: once it is
// gone a retry cannot mark, so any other failure must not skip it.
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

// removeAllFailing makes removeAll fail for the given paths and succeed (and
// record "remove state") for the rest, as runNode's stub does.
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

// With the state kept, or its removal failing, there is no mark: a retry still
// has the credential, and a mark now could have the Node deleted under a node
// that is still there.
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

// Agents on kernels below 5.7 attach their socket-LB programs without a bpf
// link, so deleting the pins detaches nothing; they are detached by id, and a
// second run finds none.
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

// cgroupShow is a `bpftool cgroup show` listing of the given Cilium programs.
func cgroupShow(progs ...string) string {
	var b strings.Builder
	b.WriteString("ID       AttachType      AttachFlags     Name\n")
	for _, p := range progs {
		id, typ, _ := strings.Cut(p, " ")
		fmt.Fprintf(&b, "%s       %s        multi           cil_sock_%s\n", id, typ, id)
	}
	return b.String()
}

// A program whose bpf link was pinned leaves once removeCiliumState drops the
// pin, but the kernel releases the link from a work queue, so the listing can
// still show it and its detach by id fails: "failed to detach program" while
// the link holds it, "get by id" once it is freed. Seen on every worker
// uninstall on kernel 6.12. A fresh listing that no longer shows it means it is
// detached, not a failure.
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

// A link the kernel has not released yet is listed again until it is gone,
// within cgroupProgramWait.
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

// What is still attached once the wait is over is reported, and only that.
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

// A listing that fails after a failed detach leaves nothing shown detached:
// both errors are reported.
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

// A detach that times out ends the step at once, with no listing after it: the
// listing would wait out the same timeout.
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

// Only the namespaces this node's containers run in are ever unmounted or
// removed: the cni- prefix is containerd's, so a separate host containerd's live
// namespaces carry it too.
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
	// The remaining mounts are the host's, so detachMounts' second look must
	// not count them as left over.
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

// When the node's namespaces could not be read at all, none is guessed at; the
// Cilium mounts are still detached, and the gap is an error.
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

// unmounts makes unmountFn drop each mount from mountinfo, as the kernel does,
// starting from mounted.
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

	// A container that cannot be read leaves a gap: kept as an error, and the
	// record saved incomplete (with what was read, and what the earlier one named).
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

// A pod sandbox that cannot be read leaves a gap exactly as a container does:
// the record is incomplete, and what was read is still kept. A timeout ends the
// enumeration, since every further call would wait as long. A sandbox path
// outside the netns directory is never taken.
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

// A container or pod sandbox removed between the listing and the info read is
// not an unread one: when a second listing answers and no longer names it, it
// is gone, with no namespace to detach, so the record stays complete. The
// error text decides nothing: `ctr sandboxes info` answers a removed sandbox
// with only "failed to load sandbox", and a container still listed stays
// unread even when its error says not found. A second listing that fails
// proves nothing, and a timeout asks no second listing.
func TestRecordStratumNetnsContainerRemovedMidRun(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gone        string // which object's info read fails: the container, or the sandbox
		infoErr     error  // what that read answers with
		relist      string // the second listing's output; "" with relistErr
		relistErr   error  // the second listing's error
		wantRelists int    // second listings asked
		wantUnread  []string
		// wantStop: a timeout ends the enumeration, so the moby namespace is
		// never asked; before the sandboxes too when a container read ends it.
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
			// Every namespace a read named; stopping before the sandboxes
			// loses cni-bbb, which only the sandbox and the gone container name.
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

// criSandboxInfo is `ctr sandboxes info` for a CRI pod sandbox, in the shape
// containerd 2.3 prints: the CRI's metadata, versioned, as the JSON value of a
// protobuf Any, which encoding/json renders in base64.
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

// runCmd is bounded: a command that does not finish returns a timeout error
// instead of hanging the teardown.
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

// After one command times out, the loops over many stop waiting out each.
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
	// iptables-save, then the first command that timed out: not every -F and -X,
	// and not ip6tables either, which shares the lock.
	if want := []string{"iptables-save", "iptables"}; !slices.Equal(ipt, want) {
		t.Errorf("iptables commands = %q, want %q: the first timeout ends the step", ipt, want)
	}
}

// --- additional cases ---------------------------------------

// A mount below the directory means what is under it is not Stratum's kubelet's
// to delete: the socket is kept, and an unreadable mount table is an error.
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

// Nothing is removed through a symlink: the socket's directory, or
// /var/lib/kubelet itself, being one points somewhere that is not kubelet's.
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

// Only a refused connection shows a socket stale. A datagram socket answers a
// stream connect with a wrong-type error, which proves nothing, so it is kept.
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

// A namespace is Stratum's only at a path in the netns directory that a Stratum
// container's spec names; the same name elsewhere is not.
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

// A spec that does not decode is an error naming the container, never a
// container with nothing to detach. Whatever was identified is detached, but the
// record is incomplete, so the result is an error, whatever was detached.
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

// After a timeout every further call would wait as long, so the enumeration ends.
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
	// No path was discovered, and the record is still saved, as incomplete: a
	// failure before any path is found must not read as an empty, complete one.
	if rec, err := loadNetnsRecord(fx); err != nil || rec == nil || rec.Complete || len(rec.Paths) != 0 {
		t.Errorf("saved record = %+v, %v; want it incomplete, with no paths", rec, err)
	}
}

// The record is saved before any container is stopped and outlives a failed
// detach: a later run, with the containers and the state gone, finishes it, and
// does not report success while an owned mount is still there.
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
	// Saved already, before anything is torn down: a process that dies here
	// loses nothing.
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

// A saved record that cannot be read may name mounts that are still there, so
// nothing is guessed at, and Run is told to keep DataDir.
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

// iptables reports an exhausted -w wait as an ordinary failure (exit status 4),
// not as the deadline execCmd enforces. That error shape, produced by a real
// process through execCmd, ends the step too.
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

// --- the saved netns record's lifecycle -------------------------------------

// A record that was partial stays incomplete across a fresh process: it must
// never authorize a clean result, however many of its namespaces are detached,
// since a namespace of the unread container may still be mounted.
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

	// Run goes on to delete the state. The next process has no specs and no
	// in-memory error: only the saved record, which says it is incomplete.
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

// An incomplete record is sticky: a later enumeration that reads everything does
// not make it complete, since a container that was unread then may be gone now.
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

// A saved record that cannot be read is treated like a save that fails: the
// enumeration returns it, with the file left exactly as it was. It may hold
// ownership an earlier run saved that the containers' metadata cannot give back,
// so a record that reads is never written over it.
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
	if _, statErr := os.Stat(fx.netnsRecordPath + ".tmp"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a replacement was written beside it: %v", statErr)
	}
}

// Through Run: an unreadable saved record found while the containers are read
// aborts the uninstall before any workload container is touched, with the
// existing rollback, so nothing is deleted, DataDir stays, and the worker is not
// marked.
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

// Nothing to remember, nothing saved.
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

// Ownership has to be durable before it can be lost: a record that cannot be
// saved stops stopContainers before a task is signalled, so the uninstall aborts
// with nothing torn down and nothing to lose.
func TestStopContainersStopsBeforeTeardownWhenTheRecordCannotBeSaved(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	fx.stratumNetns = map[string]bool{}
	// A directory where the temp file goes makes the atomic write fail, without
	// touching any path of the machine the tests run on.
	if err := os.Mkdir(fx.netnsRecordPath+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
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

// With the saved record unreadable, DataDir is kept and the rest of the cleanup
// goes on; and the worker is not marked, since a retry still can.
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

// A timeout reading the rules, not only changing them, ends the step: the other
// tool shares the lock, and the wait.
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

// --- Runtime detection --------------------------------------------------------

// addProc makes a process in the fake /proc with the given command name.
func addProc(t *testing.T, h *datapathHost, pid, comm string) {
	t.Helper()
	h.touch(t, "proc/"+pid+"/comm")
	if err := os.WriteFile(h.path("proc/"+pid+"/comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A live process has a stat, whose state says it is not a zombie, and a
	// cgroup, outside kubepods.
	if err := os.WriteFile(h.path("proc/"+pid+"/stat"), []byte(pid+" ("+comm+") S 1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setProcCgroup(t, h, pid, "0::/system.slice/"+comm+".service")
}

// setProcCgroup writes a process's /proc/<pid>/cgroup.
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
	// A zombie keeps its name in /proc but runs nothing (a killed shim under a
	// parent that does not reap); a process in any other state does.
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

// The pod cgroup names are not Stratum's alone: with another kubelet or runtime
// running, the tree is kept whole and reported, even where it looks empty, and
// nothing is ever stopped.
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

// Without being able to look for another runtime, nothing is removed.
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

// A cgroup that still has a process in it is refused by the kernel, which is
// what makes rmdir safe where stopping a slice is not: it is kept and reported,
// and its empty siblings go.
func TestRemoveKubepodsCgroupsKeepsWhatIsStillInUse(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods/empty", "sys/fs/cgroup/kubepods/busy")
	// A regular file stands in for what makes a populated cgroup's rmdir fail.
	h.touch(t, "sys/fs/cgroup/kubepods/busy/cgroup.procs-stand-in")
	err := removeKubepodsCgroups(fx)
	if err == nil || !strings.Contains(err.Error(), "still in use") {
		t.Fatalf("err = %v, want what is still in use reported", err)
	}
	if h.exists("sys/fs/cgroup/kubepods/empty") || !h.exists("sys/fs/cgroup/kubepods/busy") || !h.exists("sys/fs/cgroup/kubepods") {
		t.Error("want the empty cgroup removed and the busy one, with its parent, kept")
	}
}

// A socket that is already gone, as after an earlier run, leaves its directory
// behind empty; the directory, validated like the socket's, is removed too, and
// then /var/lib/kubelet. One that holds anything else stays.
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

	// Another plugin's socket beside the gone one keeps the directory.
	dir = shortKubeletDir(t, fx)
	staleSocket(t, filepath.Join(dir, "device-plugins/vendor.sock"))
	if err := removeLegacyKubeletDir(io.Discard, fx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "device-plugins/vendor.sock")); err != nil {
		t.Errorf("another plugin's socket was removed: %v", err)
	}
}

// A symlinked socket directory is not removed through, with the socket gone.
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

// With containerd stopped and no shim left the containers are never read, so
// whose a mounted cni- namespace is has no answer when there is no saved record
// and Stratum's containerd kept the metadata of containers: left, reported
// incomplete, DataDir kept. Where there is nothing to attribute, it is clean.
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

// Through Run: the unattributed namespaces keep DataDir, the rest of the cleanup
// carries on, and the worker is not marked, since a retry still can.
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

// --- repeated runs and netns enumeration ------------------------------------

// Run can be called again in a persistent process (the MCP server), so what one
// call learned about the namespaces must not stand in for what the next has not
// read: an earlier call that read an empty set of containers, and saved no
// record, must not make a later one, with containerd and its shims stopped,
// retained metadata and a mounted cni- namespace, believe it enumerated.
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

	// Later, in the same process: retained metadata, a mounted namespace, and
	// containerd and its shims stopped.
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

// What an invocation learned is seeded fresh at the runner's entry; the saved
// record on disk is not cleared with it.
func TestRunSeedsFreshDiscoveryAndKeepsTheSavedRecord(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	// What a previous call left on this run's value — an ownership record, a
	// read error, the enumerated flag — must not stand in for what this run has
	// not read.
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

// Only a process that has gone is skipped. A /proc entry that cannot be read for
// any other reason may be another kubelet or runtime, so the shared tree is kept
// and the failure reported.
func TestForeignRuntimesDistinguishesAVanishedProcessFromAnUnreadableOne(t *testing.T) {
	fx := &hostEffects{}
	h := newDatapathHost(t, fx)
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")

	// Vanished: the entry has no comm (ENOENT), as after the process exited.
	h.mkdirs(t, "proc/4001")
	if err := removeKubepodsCgroups(fx); err != nil {
		t.Fatalf("a vanished process stopped the removal: %v", err)
	}
	if h.exists("sys/fs/cgroup/kubepods") {
		t.Error("the empty tree was kept because of a vanished process")
	}

	// Unreadable: comm cannot be read as a file (EISDIR), while /proc itself is
	// readable.
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

// --- cgroup activity checked again before a stop ----------------------------

// The query takes time, in which systemd or a runtime can make the cgroup again,
// populated: the tree is looked at again after it, and a cgroup in use is never
// stopped over.
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

	// And one made again empty is removed, then stopped.
	h = newDatapathHost(t, fx)
	sd = &fakeSystemd{active: true, onQuery: func() { h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice") }}
	h.systemd(t, fx, sd)
	if err := removeKubepodsCgroups(fx); err != nil || sd.stops != 1 || h.exists("sys/fs/cgroup/kubepods.slice") {
		t.Errorf("err = %v, stops = %d, directory there = %v; want the empty tree removed and the unit stopped", err, sd.stops, h.exists("sys/fs/cgroup/kubepods.slice"))
	}
}

// The other-runtime check is made again right before a stop: the removal retries
// for seconds in between, and a runtime can start in them.
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

	// A check that cannot be made refuses it too.
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

// Only a known inactive or missing unit is inactive. A timeout or a failed query
// says nothing about the unit, and is reported: an active one makes the directory
// again at the daemon-reload.
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

// With the directory gone, a query that fails is an error, not a clean node: the
// unit may be active, and make the directory again.
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

// An error inspecting the cgroup after the stop is reported, as before it.
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

// --- Cilium teardown while another runtime runs -----------------------------

// Cilium's datapath is found by name, which does not say whose it is: with
// another kubelet or container runtime running (RKE2 or k3s with Cilium of its
// own), every Cilium step is skipped, none of its commands runs, and the cleanup
// is reported as not clean. What is proved to be Stratum's still goes.
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
	// Stratum's own namespace, proved by the record, is still detached; Cilium's
	// mount is not; and the kubepods tree, with its own guard, is kept.
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

// With no way to tell whether another runtime is running, Cilium is left too.
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

// Embedded kubelets and other runtimes are found too: k3s runs its kubelet
// inside k3s-server, MicroK8s inside kubelite, and with no pod there is no shim
// to give them away.
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

// dockerHost adds Docker as it runs on a developer's machine: dockerd, the
// distribution's containerd and a shim, all under system.slice, and a shim
// that has exited but not been reaped.
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

// A container runtime shares Cilium's datapath and the kubepods cgroups only by
// serving a kubelet: with none running and nothing under kubepods, Docker is a
// bystander, and is named as one.
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

// Any sign of a kubelet or a Cilium agent makes every runtime foreign: one
// running, under any of its names, or a pod, which is a process in a kubepods
// cgroup however the kubelet that made it is named or hidden.
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
	// Each name alone, under system.slice and with no pod: Cilium's agent run
	// by Docker, or a kubelet that has none yet.
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
			// The pod's process is named, so one of Stratum's own left behind is
			// not read as Docker.
			if name == "systemd pod" && !slices.Contains(foreign, "pause under kubepods (pid 900)") {
				t.Errorf("foreign = %q, want the pod's process named", foreign)
			}
		})
	}
}

// A runtime is only a bystander when every process could be looked at: a
// cgroup that cannot be read may be a pod's, and is an error. With no runtime
// running, nothing more is read than before.
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
	// And the datapath and kubepods are kept for it.
	h.touch(t, "sys/class/net/cilium_host")
	h.mkdirs(t, "sys/fs/cgroup/kubepods/pod1")
	if err := teardownDatapath(io.Discard, fx); err == nil || !h.exists("sys/class/net/cilium_host") || !h.exists("sys/fs/cgroup/kubepods/pod1") {
		t.Errorf("err = %v; want the Cilium link and kubepods kept", err)
	}
}

// The kubepods tree is read for the pods /proc does not show: a part of it that
// cannot be read is not shown to be empty, and is an error too.
func TestForeignRuntimesFailsWhenTheKubepodsTreeCannotBeRead(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T, h *datapathHost){
		"unreadable cgroup.procs": func(t *testing.T, h *datapathHost) {
			h.mkdirs(t, "sys/fs/cgroup/kubepods.slice/pod1", "elsewhere")
			// A symlink to a directory: not a directory entry, and EISDIR to read.
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

// On a machine that also runs Docker, the uninstall removes Cilium's datapath
// and the kubepods cgroups, says Docker was left alone, and reports no failure.
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

// ciliumCommands is the commands among cmds that are Cilium's teardown: the
// network tools and bpftool. (The kubepods step runs systemctl, with its own guard.)
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

// --- teardown error reporting and fixed tool paths --------------------------

// Only a directory that is not empty (it holds another program's pins) or is
// already gone is expected to survive the rmdir; any other failure is reported.
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

	// A foreign pin keeping the directory is still not an error.
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

// writeTool puts an executable shell script named name in dir.
func writeTool(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The tools uninstall runs are found on a fixed PATH, not the caller's, and see
// that PATH: a caller's PATH may put a shim of `ip` or `iptables` first.
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

// With the activity query making the tree again, and another runtime running,
// the tree is kept and that is reported: it is there now, whatever it was when
// the loop began.
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

	// No tree, even after the query: a runtime's own unit is not an error.
	h = newDatapathHost(t, fx)
	addProc(t, h, "4242", "kubelet")
	h.systemd(t, fx, &fakeSystemd{active: true})
	if err := removeKubepodsCgroups(fx); err != nil {
		t.Errorf("err = %v, want no report when there is no tree to keep", err)
	}
}

// exec.Command looks a bare name up on the caller's PATH when it is called,
// before anything can be overridden, and a slow entry there stalls it outside
// cmdTimeout. So the command must be built from the path toolPath resolved,
// and a bare name must never reach exec.
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

// The tools run as root, so a variable that loads code (XTABLES_LIBDIR,
// TC_LIB_DIR) in the caller's environment must not reach them.
func TestToolCmdEnvIsExplicit(t *testing.T) {
	t.Setenv("XTABLES_LIBDIR", "/evil")
	t.Setenv("TC_LIB_DIR", "/evil")
	swap(t, &toolPath, "/tp")

	got := toolCmd(context.Background(), "tool").Env
	if want := []string{"PATH=/tp", "LC_ALL=C"}; !slices.Equal(got, want) {
		t.Errorf("Env = %q, want exactly %q", got, want)
	}
}

// umount's own message is the whole error when it printed nothing.
func TestUmountMountErrorHasNoTrailingSeparator(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "umount", "exit 1")
	swap(t, &toolPath, dir)

	err := umountMount("/mnt")
	if err == nil || strings.HasSuffix(err.Error(), ": ") {
		t.Errorf("err = %v, want an error without a trailing \": \"", err)
	}
}

// The weekly run's failure: bpftool cannot list the root cgroup.
const cgroupShowENODEV = "bpftool cgroup show /sys/fs/cgroup: exit status 255: Error: can't query bpf programs attached to /sys/fs/cgroup: No such device"

// progShow is a `bpftool -j prog show` listing: id, type and name per program.
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

package uninstall

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/toolenv"
)

// Never written once a Run starts, so two Runs cannot race over them; tests shorten them.
var (
	cgroupRemoveWait = 10 * time.Second

	cgroupProgramWait = 10 * time.Second

	// Not the caller's PATH, which can put a shim of any of these tools first.
	toolPath = toolenv.Path

	// One held lock or wedged tool must not hang the teardown, and the Node mark with it.
	cmdTimeout  = 30 * time.Second
	xtablesWait = "5"
)

var errCommandTimeout = errors.New("timed out")

func execCmd(stdin, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	cmd := toolCmd(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("%s %s: %w after %s (a lock held by another process, or a wedged tool?); whether it took effect is not known", name, strings.Join(args, " "), errCommandTimeout, cmdTimeout)
	}
	if err != nil {
		err = fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		if o := strings.TrimSpace(string(out)); o != "" {
			err = fmt.Errorf("%w: %s", err, o)
		}
		return out, err
	}
	return out, nil
}

// Built from the resolved path: exec resolves a bare name on the caller's PATH, beyond cmdTimeout.
func toolCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	path, err := lookTool(name)
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Err = err
	cmd.Env = []string{"PATH=" + toolPath, "LC_ALL=C"}
	return cmd
}

func lookTool(name string) (string, error) { return toolenv.Look(toolPath, name) }

var ciliumLink = regexp.MustCompile(`^(cilium_[a-z0-9_]+|lxc_health|lxc[0-9a-f]{5,12})$`)

func teardownDatapath(w io.Writer, fx *hostEffects) error {
	var errs []error
	shared := false
	if fx.foreignRuntimesFn == nil {
		shared = true
		errs = append(errs, unsetEffect("foreignRuntimesFn"))
	} else if others, bystanders, err := fx.foreignRuntimesFn(); err != nil {
		shared = true
		errs = append(errs, fmt.Errorf("left Cilium's datapath in place: could not check for another kubelet or container runtime: %w", err))
	} else if len(others) > 0 {
		shared = true
		errs = append(errs, fmt.Errorf("left Cilium's datapath (links, tc filters, netfilter rules, BPF pins, cgroup programs, %s) in place: it may be shared with another kubelet or container runtime that is running (%s): %w", fx.ciliumRunDir, strings.Join(others, ", "), errRuntimeRunning))
	} else if len(bystanders) > 0 {
		fmt.Fprintf(w, "  another container runtime is running (%s), with no kubelet or Cilium agent and nothing under kubepods: it does not use Cilium's datapath or the kubepods cgroups, so they are not kept for it, and it is not touched\n", strings.Join(bystanders, ", "))
	}
	for _, s := range []struct {
		what   string
		cilium bool
		fn     func() error
	}{
		{"detach CNI netns and Cilium mounts", false, func() error { return detachNetns(fx, !shared) }},
		{"remove Cilium links", true, func() error { return removeCiliumLinks(fx) }},
		{"remove Cilium tc filters", true, func() error { return removeCiliumTCFilters(fx) }},
		{"remove Cilium netfilter chains", true, func() error { return removeCiliumChains(fx) }},
		{"remove Cilium BPF pins and state", true, func() error { return removeCiliumState(fx) }},
		{"detach Cilium cgroup programs", true, func() error { return removeCiliumCgroupPrograms(fx) }},
		{"remove kubepods cgroups", false, func() error { return removeKubepodsCgroups(fx) }},
		{"remove " + fx.legacyKubeletDir, false, func() error { return removeLegacyKubeletDir(w, fx) }},
	} {
		if s.cilium && shared {
			continue
		}
		if err := s.fn(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.what, err))
		}
	}
	return errors.Join(errs...)
}

// marks a failure after which Run keeps DataDir (it may still say what to detach) and carries on.
var errKeepState = errors.New("keeping the node state")

type netnsRecord struct {
	Complete bool     `json:"complete"`
	Paths    []string `json:"paths"`
	Unread   []string `json:"unread,omitempty"`
}

func canonNetns(fx *hostEffects, p string) (string, bool) {
	dir, base := filepath.Split(filepath.Clean(p))
	switch filepath.Clean(dir) {
	case fx.netnsDir, "/run/netns", "/var/run/netns":
		if base != "" {
			return filepath.Join(fx.netnsDir, base), true
		}
	}
	return "", false
}

func goneFromListing(fx *hostEffects, ids func(string) []string, id string, list ...string) (bool, error) {
	out, err := fx.ctrRun(list...)
	return err == nil && !slices.Contains(ids(out), id), err
}

// Ownership saved before containers stop; a timeout ends the loop: later calls would wait as long.
func recordStratumNetns(fx *hostEffects, namespaces []string) error {
	if fx.ctrRun == nil {
		return unsetEffect("ctrRun")
	}
	paths := map[string]bool{}
	var errs []error
enumerate:
	for _, ns := range namespaces {
		out, err := fx.ctrRun("-n", ns, "containers", "ls", "-q")
		if err != nil {
			errs = append(errs, fmt.Errorf("list containers in containerd namespace %s: %w", ns, err))
			if errors.Is(err, errCtrTimeout) {
				break
			}
			out = ""
		}
		for _, id := range strings.Fields(out) {
			info, err := fx.ctrRun("-n", ns, "containers", "info", id)
			if err != nil {
				var gone bool
				var listErr error
				if !errors.Is(err, errCtrTimeout) {
					gone, listErr = goneFromListing(fx, strings.Fields, id, "-n", ns, "containers", "ls", "-q")
				}
				if gone {
					continue
				}
				errs = append(errs, fmt.Errorf("read container %s/%s: %w", ns, id, err))
				if errors.Is(listErr, errCtrTimeout) {
					errs = append(errs, fmt.Errorf("list containers in containerd namespace %s: %w", ns, listErr))
				}
				if errors.Is(err, errCtrTimeout) || errors.Is(listErr, errCtrTimeout) {
					break enumerate
				}
				continue
			}
			found, err := containerNetnsPaths(info)
			if err != nil {
				errs = append(errs, fmt.Errorf("read container %s/%s: %w", ns, id, err))
				continue
			}
			for _, p := range found {
				if c, ok := canonNetns(fx, p); ok {
					paths[c] = true
				}
			}
		}
		out, err = fx.ctrRun("-n", ns, "sandboxes", "ls")
		if err != nil {
			errs = append(errs, fmt.Errorf("list pod sandboxes in containerd namespace %s: %w", ns, err))
			if errors.Is(err, errCtrTimeout) {
				break
			}
			continue
		}
		for _, id := range sandboxIDs(out) {
			info, err := fx.ctrRun("-n", ns, "sandboxes", "info", id)
			if err != nil {
				var gone bool
				var listErr error
				if !errors.Is(err, errCtrTimeout) {
					gone, listErr = goneFromListing(fx, sandboxIDs, id, "-n", ns, "sandboxes", "ls")
				}
				if gone {
					continue
				}
				errs = append(errs, fmt.Errorf("read pod sandbox %s/%s: %w", ns, id, err))
				if errors.Is(listErr, errCtrTimeout) {
					errs = append(errs, fmt.Errorf("list pod sandboxes in containerd namespace %s: %w", ns, listErr))
				}
				if errors.Is(err, errCtrTimeout) || errors.Is(listErr, errCtrTimeout) {
					break enumerate
				}
				continue
			}
			p, err := sandboxNetnsPath(info)
			if err != nil {
				errs = append(errs, fmt.Errorf("read pod sandbox %s/%s: %w", ns, id, err))
				continue
			}
			if c, ok := canonNetns(fx, p); ok {
				paths[c] = true
			}
		}
	}
	rec := netnsRecord{Complete: len(errs) == 0}
	for _, err := range errs {
		rec.Unread = append(rec.Unread, err.Error())
	}
	saved, loadErr := loadNetnsRecord(fx)
	if loadErr != nil {
		// an earlier run's ownership may be in it; never replaced by one that reads.
		return fmt.Errorf("read the saved record of this node's network namespaces, left as it is: %w; check %s and %s, remove the record once what it names is detached, and re-run", loadErr, fx.netnsDir, fx.netnsRecordPath)
	}
	if saved != nil {
		for _, p := range saved.Paths {
			paths[p] = true
		}
		rec.Complete = rec.Complete && saved.Complete
		rec.Unread = append(saved.Unread, rec.Unread...)
	}
	rec.Paths = slices.Sorted(maps.Keys(paths))
	fx.stratumNetns, fx.stratumNetnsErr = paths, errors.Join(errs...)
	fx.netnsEnumerated = true
	if len(rec.Paths) == 0 && rec.Complete && saved == nil {
		return nil
	}
	if err := saveNetnsRecord(fx, rec); err != nil {
		return fmt.Errorf("save the record of this node's network namespaces to %s: %w", fx.netnsRecordPath, err)
	}
	return nil
}

// Output that does not decode is an error: an empty answer would read as no namespace.
func containerNetnsPaths(info string) ([]string, error) {
	var c struct {
		Spec struct {
			Linux struct {
				Namespaces []struct{ Type, Path string }
			}
		}
	}
	if err := json.Unmarshal([]byte(info), &c); err != nil {
		return nil, fmt.Errorf("decode its spec: %w", err)
	}
	var paths []string
	for _, n := range c.Spec.Linux.Namespaces {
		if n.Type == "network" && n.Path != "" {
			paths = append(paths, n.Path)
		}
	}
	return paths, nil
}

// containerd's own record, not an API: anything but v1 errors; "" means a host-network sandbox.
func sandboxNetnsPath(info string) (string, error) {
	var sb struct {
		Extensions map[string]struct {
			Value []byte `json:"value"`
		}
	}
	if err := json.Unmarshal([]byte(info), &sb); err != nil {
		return "", fmt.Errorf("decode its info: %w", err)
	}
	ext, ok := sb.Extensions["metadata"]
	if !ok {
		return "", fmt.Errorf("it carries no CRI metadata")
	}
	var md struct {
		Version  string
		Metadata *struct{ NetNSPath string }
	}
	if err := json.Unmarshal(ext.Value, &md); err != nil {
		return "", fmt.Errorf("decode its CRI metadata: %w", err)
	}
	if md.Version != "v1" || md.Metadata == nil {
		return "", fmt.Errorf("its CRI metadata is not the version 1 format this release reads (version %q)", md.Version)
	}
	return md.Metadata.NetNSPath, nil
}

// an unreadable one is an error: it may name namespaces still mounted.
func loadNetnsRecord(fx *hostEffects) (*netnsRecord, error) {
	b, err := os.ReadFile(fx.netnsRecordPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec netnsRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w", fx.netnsRecordPath, err)
	}
	var paths []string
	for _, p := range rec.Paths {
		if c, ok := canonNetns(fx, p); ok {
			paths = append(paths, c)
		}
	}
	rec.Paths = paths
	return &rec, nil
}

// a torn record after a power loss would make the next uninstall refuse.
func saveNetnsRecord(fx *hostEffects, rec netnsRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(fx.netnsRecordPath, b, 0o600)
}

func networkMounts(fx *hostEffects, owned map[string]bool, cilium bool) ([]string, error) {
	// an empty directory would give a "/" prefix and select every host mount.
	if cilium && fx.ciliumRunDir == "" {
		return nil, unsetEffect("ciliumRunDir")
	}
	all, err := mountPoints(fx)
	if err != nil {
		return nil, err
	}
	var mounts []string
	for _, mp := range all {
		if owned[mp] || (cilium && strings.HasPrefix(mp, fx.ciliumRunDir+"/")) {
			mounts = append(mounts, mp)
		}
	}
	slices.SortFunc(mounts, func(a, b string) int {
		return cmp.Or(cmp.Compare(strings.Count(b, "/"), strings.Count(a, "/")), strings.Compare(a, b))
	})
	return slices.Compact(mounts), nil
}

func unattributedNetns(fx *hostEffects) error {
	if _, err := os.Stat(fx.containerdStateDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("could not check for the containers this node ran: %w; %w", err, errKeepState)
	}
	mounts, err := mountPoints(fx)
	if err != nil {
		return fmt.Errorf("could not read the mount table to look for network namespaces: %w; %w", err, errKeepState)
	}
	var left []string
	for _, mp := range mounts {
		if filepath.Dir(mp) == fx.netnsDir && strings.HasPrefix(filepath.Base(mp), "cni-") {
			left = append(left, mp)
		}
	}
	if len(left) == 0 {
		return nil
	}
	return fmt.Errorf("incomplete: containerd was stopped with no shim left, so this node's containers were not read, and no record of their network namespaces was saved; %d mounted under %s (the first %s) could not be shown to be Stratum's and were left: detach any that are, or reboot; keeping the node state: %w", len(left), fx.netnsDir, left[0], errKeepState)
}

// removing a file that is still a mount point would fail with EBUSY.
func detachNetns(fx *hostEffects, cilium bool) error {
	var errs []error
	if fx.unmountFn == nil {
		return unsetEffect("unmountFn")
	}
	owned := maps.Clone(fx.stratumNetns)
	if owned == nil {
		owned = map[string]bool{}
	}
	incomplete := fx.stratumNetnsErr != nil
	var unread []string
	if fx.stratumNetnsErr != nil {
		unread = append(unread, fx.stratumNetnsErr.Error())
	}
	saved, loadErr := loadNetnsRecord(fx)
	switch {
	case loadErr != nil:
		errs = append(errs, fmt.Errorf("could not read the saved record of this node's network namespaces, so what is left to detach under %s is not known: %w; %w", fx.netnsDir, loadErr, errKeepState))
	case saved != nil:
		for _, p := range saved.Paths {
			owned[p] = true
		}
		if !saved.Complete {
			incomplete = true
			unread = append(unread, saved.Unread...)
		}
	}
	if loadErr == nil && saved == nil && !fx.netnsEnumerated && len(owned) == 0 {
		if err := unattributedNetns(fx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := detachMounts(fx.unmountFn, func() ([]string, error) { return networkMounts(fx, owned, cilium) }, "under "+fx.netnsDir+" or "+fx.ciliumRunDir); err != nil {
		errs = append(errs, err)
	} else {
		for _, p := range slices.Sorted(maps.Keys(owned)) {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	if incomplete {
		errs = append(errs, fmt.Errorf("could not identify every namespace of this node's containers (%s), so one under %s may still be mounted: the ones identified were detached; reboot the node, or detach the rest and delete %s", strings.Join(unread, "; "), fx.netnsDir, fx.netnsRecordPath))
	}
	if len(errs) == 0 {
		if err := os.Remove(fx.netnsRecordPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// a veth's peer goes with it, so only a link still there afterwards is an error.
func removeCiliumLinks(fx *hostEffects) error {
	if fx.runCmd == nil {
		return unsetEffect("runCmd")
	}
	entries, err := os.ReadDir(fx.netClassDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if !ciliumLink.MatchString(name) {
			continue
		}
		if _, err := fx.runCmd("", "ip", "link", "delete", name); err != nil {
			if errors.Is(err, errCommandTimeout) {
				return errors.Join(append(errs, err)...)
			}
			if _, statErr := os.Lstat(filepath.Join(fx.netClassDir, name)); statErr == nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

var tcFilterLine = regexp.MustCompile(`^filter (?:parent \S+ )?protocol (\S+) pref (\d+) bpf (?:chain (\d+) )?handle (\S+) (\S+)`)

// the chain is part of a filter's identity: a foreign one can share protocol, pref, handle.
func ciliumTCFilters(dump string) [][4]string {
	var out [][4]string
	for _, line := range strings.Split(dump, "\n") {
		m := tcFilterLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || !(strings.HasPrefix(m[5], "cil_") || strings.HasPrefix(m[5], "cilium")) {
			continue
		}
		chain := m[3]
		if chain == "" {
			chain = "0"
		}
		out = append(out, [4]string{m[1], m[2], m[4], chain})
	}
	return out
}

// without tcx, clsact filters survive pin deletion; a missing tc is an error, not proof none exist.
func removeCiliumTCFilters(fx *hostEffects) error {
	if fx.runCmd == nil {
		return unsetEffect("runCmd")
	}
	entries, err := os.ReadDir(fx.netClassDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		dev := e.Name()
		for _, parent := range []string{"ingress", "egress"} {
			dump, err := fx.runCmd("", "tc", "filter", "show", "dev", dev, parent)
			if err != nil {
				if errors.Is(err, exec.ErrNotFound) {
					return errors.Join(append(errs, errors.New("tc was not found, so Cilium's tc filters could not be checked or detached from the host's interfaces; install iproute-tc and re-run `stratum uninstall --yes`"))...)
				}
				if errors.Is(err, errCommandTimeout) {
					return errors.Join(append(errs, err)...)
				}
				_, statErr := os.Lstat(filepath.Join(fx.netClassDir, dev))
				// no qdisc means no filters, and a device that went away has none.
				if !strings.Contains(err.Error(), "Cannot find") && statErr == nil {
					errs = append(errs, err)
				}
				continue
			}
			for _, f := range ciliumTCFilters(string(dump)) {
				if _, err := fx.runCmd("", "tc", "filter", "del", "dev", dev, parent, "protocol", f[0], "chain", f[3], "pref", f[1], "handle", f[2], "bpf"); err != nil {
					if errors.Is(err, errCommandTimeout) {
						return errors.Join(append(errs, err)...)
					}
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// a missing iptables-save is an error: Cilium adds its rules with the tools in its container.
func removeCiliumChains(fx *hostEffects) error {
	if fx.runCmd == nil {
		return unsetEffect("runCmd")
	}
	var errs []error
	for _, tool := range []string{"iptables", "ip6tables"} {
		dump, err := fx.runCmd("", tool+"-save")
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				err = fmt.Errorf("%s-save was not found, so Cilium's netfilter rules could not be checked or removed; install iptables and re-run `stratum uninstall --yes`", tool)
			}
			errs = append(errs, err)
			if waitExhausted(err) {
				return errors.Join(errs...) // the other tool shares the lock and the wait
			}
			continue
		}
		for _, args := range ciliumChainCleanup(string(dump)) {
			if _, err := fx.runCmd("", tool, args...); err != nil {
				errs = append(errs, err)
				if waitExhausted(err) {
					return errors.Join(errs...)
				}
			}
		}
	}
	return errors.Join(errs...)
}

func waitExhausted(err error) bool {
	return errors.Is(err, errCommandTimeout) || strings.Contains(err.Error(), "holding the xtables lock")
}

// a chain cannot be deleted while another still jumps to it, so the jumps go first.
func ciliumChainCleanup(dump string) [][]string {
	var jumps, flush, del [][]string
	table := ""
	for _, line := range strings.Split(dump, "\n") {
		switch {
		case strings.HasPrefix(line, "*"):
			table = strings.TrimSpace(line[1:])
		case strings.HasPrefix(line, ":") && ciliumChainName(line[1:]):
			chain := strings.Fields(line[1:])[0]
			flush = append(flush, []string{"-w", xtablesWait, "-t", table, "-F", chain})
			del = append(del, []string{"-w", xtablesWait, "-t", table, "-X", chain})
		case strings.HasPrefix(line, "-A "):
			if args := splitRule(line); jumpsToCiliumChain(args) {
				jumps = append(jumps, append([]string{"-w", xtablesWait, "-t", table, "-D"}, args[1:]...))
			}
		}
	}
	return slices.Concat(jumps, flush, del)
}

// OLD_CILIUM_ is what the agent renames its chains to at every start, and can leave.
func ciliumChainName(s string) bool {
	return strings.HasPrefix(s, "CILIUM_") || strings.HasPrefix(s, "OLD_CILIUM_")
}

// read from the parsed arguments: a --comment naming "-j CILIUM_X" must not select.
func jumpsToCiliumChain(args []string) bool {
	n := len(args)
	return n >= 4 && args[0] == "-A" && !ciliumChainName(args[1]) &&
		(args[n-2] == "-j" || args[n-2] == "-g") && ciliumChainName(args[n-1])
}

func splitRule(rule string) []string {
	var args []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(rule); i++ {
		c := rule[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(rule):
			i++
			cur.WriteByte(rule[i])
		case c == '"':
			inQuote, have = !inQuote, true
		case c == ' ' && !inQuote:
			if have {
				args = append(args, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		args = append(args, cur.String())
	}
	return args
}

// deleting through the run directory's cgroup2 mount would delete the host's hierarchy.
func removeCiliumState(fx *hostEffects) error {
	var errs []error
	tcDir := filepath.Join(fx.bpfRoot, "tc")
	if err := refuseMounted(fx, tcDir); err != nil {
		errs = append(errs, err)
	} else {
		globals := filepath.Join(tcDir, "globals")
		maps, err := os.ReadDir(globals)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		for _, m := range maps {
			if strings.HasPrefix(m.Name(), "cilium_") {
				if err := os.Remove(filepath.Join(globals, m.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err)
				}
			}
		}
		// rmdir failing on a directory that holds another program's pins is the point.
		for _, d := range []string{globals, tcDir} {
			if err := os.Remove(d); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
				errs = append(errs, err)
			}
		}
	}
	for _, d := range []string{filepath.Join(fx.bpfRoot, "cilium"), fx.ciliumRunDir} {
		if err := removeAllUnlessMounted(fx, d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func refuseMounted(fx *hostEffects, dir string) error {
	mounts, err := mountPoints(fx)
	if err != nil {
		return fmt.Errorf("not removing from %s: %w", dir, err)
	}
	for _, m := range mounts {
		if m == dir || strings.HasPrefix(m, dir+"/") {
			return fmt.Errorf("not removing from %s: %s is still mounted", dir, m)
		}
	}
	return nil
}

// RemoveAll does not stop at a mount boundary, so the table is read again.
func removeAllUnlessMounted(fx *hostEffects, dir string) error {
	if err := refuseMounted(fx, dir); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// fixed in kubelet's source, whatever --root-dir is; pod-resources is served under --root-dir.
var kubeletFixedSockets = []string{"device-plugins/kubelet.sock"}

// kubelet's fixed --cert-dir default; its certificate pairs outlive the DataDir wipe.
const kubeletCertDirRel = "pki"

// only a positive proof removes; the caller checked nothing below the kubelet dir is mounted.
func removeStratumSignedKubeletCerts(w io.Writer, certDir string) error {
	roots, err := pki.KubeletServingRoots()
	if err != nil || len(roots) == 0 {
		fmt.Fprintf(w, "  left %s in place: stratum's CA bundle could not be read (%v), so no certificate there can be shown to be stratum's kubelet's\n", certDir, err)
		return nil
	}
	entries, err := os.ReadDir(certDir)
	if err != nil {
		return err
	}
	// links must be judged while their timestamped targets still exist.
	owned := make(map[string]bool)
	for _, e := range entries {
		p := filepath.Join(certDir, e.Name())
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		leaf, err := pki.ReadKubeletServingLeaf(p)
		switch {
		case err != nil:
			fmt.Fprintf(w, "  left %s in place: it could not be read as a certificate (%v), so it cannot be shown to be stratum's kubelet's\n", p, err)
		case !pki.SignedByAny(leaf, roots):
			fmt.Fprintf(w, "  left %s in place: no CA in stratum's bundle signed it, so it may be another kubelet's\n", p)
		default:
			owned[e.Name()] = true
		}
	}
	var errs []error
	remove := func(p string) {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	for _, name := range []string{"kubelet-client-current.pem", "kubelet-server-current.pem"} {
		p := filepath.Join(certDir, name)
		target, err := os.Readlink(p)
		if err != nil {
			continue
		}
		// client-go writes absolute targets; a relative basename is accepted, nothing deeper.
		if filepath.IsAbs(target) {
			if filepath.Dir(target) != filepath.Clean(certDir) {
				continue
			}
		} else if filepath.Dir(target) != "." {
			continue
		}
		if owned[filepath.Base(target)] {
			remove(p)
		}
	}
	for _, e := range entries {
		if owned[e.Name()] {
			remove(filepath.Join(certDir, e.Name()))
		}
	}

	return errors.Join(errs...)
}

func removeLegacyKubeletDir(w io.Writer, fx *hostEffects) error {
	keep := func(format string, a ...any) {
		fmt.Fprintf(w, "  left %s in place: %s\n", fx.legacyKubeletDir, fmt.Sprintf(format, a...))
	}
	fi, err := os.Lstat(fx.legacyKubeletDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		keep("it is not a plain directory")
		return nil
	}
	mounts, err := mountPoints(fx)
	if err != nil {
		return fmt.Errorf("not removing from %s: %w", fx.legacyKubeletDir, err)
	}
	for _, m := range mounts {
		if m == fx.legacyKubeletDir || strings.HasPrefix(m, fx.legacyKubeletDir+"/") {
			keep("%s is mounted", m)
			return nil
		}
	}
	var errs []error
	for _, rel := range kubeletFixedSockets {
		p := filepath.Join(fx.legacyKubeletDir, rel)
		pdir, err := os.Lstat(filepath.Dir(p))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !pdir.IsDir() {
			keep("%s is not a plain directory", filepath.Dir(p))
			continue
		}
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			// already gone: its directory is still removed if the socket left it empty.
			_ = os.Remove(filepath.Dir(p))
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if fi.Mode()&fs.ModeSocket == 0 {
			continue
		}
		c, err := net.DialTimeout("unix", p, time.Second)
		switch {
		case err == nil:
			_ = c.Close()
			continue
		case !errors.Is(err, syscall.ECONNREFUSED):
			keep("%s could not be shown stale: %v", p, err)
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		_ = os.Remove(filepath.Dir(p)) // fails when something else is in it, which is the point
	}
	certDir := filepath.Join(fx.legacyKubeletDir, kubeletCertDirRel)
	if cdi, err := os.Lstat(certDir); err == nil && cdi.IsDir() {
		if err := removeStratumSignedKubeletCerts(w, certDir); err != nil {
			// without the CA bundle a leftover pair could never be proven later.
			errs = append(errs, fmt.Errorf("remove Stratum's kubelet's verified certificate pairs: %w; %w", err, errKeepState))
		}
		_ = os.Remove(certDir) // fails while a foreign or unjudged file is in it, which is the point
	}
	switch err := os.Remove(fx.legacyKubeletDir); {
	case err == nil || errors.Is(err, fs.ErrNotExist):
	case errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EBUSY):
		// the kubelet's default dir, not its root: say so, or the line reads as claiming it.
		keep("it is the kubelet's default directory, where Stratum's kubelet leaves only its stale device-plugin socket and certificate pairs, and it holds something else, or is in use")
	default:
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// the kernel truncates a program name to 15 characters, so only the prefix is reliable.
func ciliumCgroupPrograms(show string) [][2]string {
	var out [][2]string
	for _, line := range strings.Split(show, "\n") {
		f := strings.Fields(line)
		// The flags column can be empty, so the name is the last field.
		if len(f) < 3 || strings.Trim(f[0], "0123456789") != "" || !strings.HasPrefix(f[len(f)-1], "cil_") {
			continue
		}
		out = append(out, [2]string{f[1], f[0]})
	}
	return out
}

// pre-5.7 agents attach without links (pin deletion detaches nothing); a work queue frees links
func removeCiliumCgroupPrograms(fx *hostEffects) error {
	if fx.runCmd == nil {
		return unsetEffect("runCmd")
	}
	progs, err := listCiliumCgroupPrograms(fx)
	if errors.Is(err, errCgroupShowBug) {
		return awaitNoLoadedCiliumCgroupPrograms(fx, err)
	}
	if err != nil {
		return err
	}
	failed := map[[2]string]error{}
	for _, p := range progs {
		if _, err := fx.runCmd("", "bpftool", "cgroup", "detach", fx.cgroupRoot, p[0], "id", p[1]); err != nil {
			failed[p] = err
			if errors.Is(err, errCommandTimeout) {
				return detachFailures(progs, failed)
			}
		}
	}
	for deadline := time.Now().Add(cgroupProgramWait); len(failed) > 0; time.Sleep(pollInterval) {
		left, err := listCiliumCgroupPrograms(fx)
		if err != nil {
			return errors.Join(detachFailures(progs, failed), err)
		}
		maps.DeleteFunc(failed, func(p [2]string, _ error) bool { return !slices.Contains(left, p) })
		if len(failed) == 0 || !time.Now().Before(deadline) {
			break
		}
	}
	return detachFailures(progs, failed)
}

func listCiliumCgroupPrograms(fx *hostEffects) ([][2]string, error) {
	out, err := fx.runCmd("", "bpftool", "cgroup", "show", fx.cgroupRoot)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("bpftool was not found, so Cilium's cgroup programs could not be checked or detached; install bpftool and re-run `stratum uninstall --yes`")
		}
		if cgroupShowBug(err) {
			return nil, fmt.Errorf("%w: %w", errCgroupShowBug, err)
		}
		return nil, err
	}
	return ciliumCgroupPrograms(string(out)), nil
}

// bpftool 7.4 fails against tcx/netkit kernels, 7.5.0 against older ones; both fixed in 7.6.
var errCgroupShowBug = errors.New("this bpftool cannot list the root cgroup's programs")

func cgroupShowBug(err error) bool {
	msg := strings.TrimSpace(err.Error())
	if !strings.Contains(msg, "can't query bpf programs attached to") {
		return false
	}
	for _, reason := range []string{": No such device", ": No such device or address", ": Invalid argument"} {
		if strings.HasSuffix(msg, reason) {
			return true
		}
	}
	return false
}

// attached implies loaded, so none of Cilium's loaded means none attached.
func awaitNoLoadedCiliumCgroupPrograms(fx *hostEffects, showErr error) error {
	for deadline := time.Now().Add(cgroupProgramWait); ; time.Sleep(pollInterval) {
		out, err := fx.runCmd("", "bpftool", "-j", "prog", "show")
		if err != nil {
			return errors.Join(showErr, fmt.Errorf("list the loaded BPF programs instead: %w", err))
		}
		loaded, err := loadedCiliumCgroupPrograms(out)
		if err != nil {
			return errors.Join(showErr, err)
		}
		if len(loaded) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w, and Cilium cgroup programs are still loaded (%s), so they may still be attached; upgrade bpftool to 7.6 or later and re-run `stratum uninstall --yes`: %v",
				errCgroupShowBug, strings.Join(loaded, ", "), showErr)
		}
	}
}

// a type bpftool cannot name prints as a number, and such a program counts.
func loadedCiliumCgroupPrograms(out []byte) ([]string, error) {
	var progs []struct {
		ID   int             `json:"id"`
		Type json.RawMessage `json:"type"`
		Name string          `json:"name"`
	}
	if err := json.Unmarshal(out, &progs); err != nil {
		return nil, fmt.Errorf("read `bpftool -j prog show`: %w", err)
	}
	var names []string
	for _, p := range progs {
		if !strings.HasPrefix(p.Name, "cil_") {
			continue
		}
		var typ string
		if json.Unmarshal(p.Type, &typ) != nil {
			typ = "type " + string(p.Type)
		} else if !strings.HasPrefix(typ, "cgroup_") && typ != "sock_ops" {
			continue
		}
		names = append(names, fmt.Sprintf("%s (id %d, %s)", p.Name, p.ID, typ))
	}
	return names, nil
}

func detachFailures(progs [][2]string, failed map[[2]string]error) error {
	var errs []error
	for _, p := range progs {
		if err, ok := failed[p]; ok {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// matching is deliberately broad: a match only keeps what is shared, never removes.
var (
	sharingComms = []string{"kubelet", "k3s*", "rke2*", "kubelite", "k0s", "cilium-agent", "cilium-envoy"}
	runtimeComms = []string{"containerd", "crio", "dockerd", "cri-dockerd", "podman", "conmon", "crun"}
)

func matchComm(comm string, names []string) bool {
	for _, r := range names {
		if prefix, ok := strings.CutSuffix(r, "*"); (ok && strings.HasPrefix(comm, prefix)) || (!ok && (comm == r || strings.HasPrefix(comm, r+"-"))) {
			return true
		}
	}
	return false
}

func foreignRuntimes(fx *hostEffects) (foreign, bystanders []string, err error) {
	entries, err := os.ReadDir(fx.procRoot)
	if err != nil {
		return nil, nil, err
	}
	var kubelets, runtimes []string
	live := map[string]string{}
	for _, e := range entries {
		if strings.Trim(e.Name(), "0123456789") != "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(fx.procRoot, e.Name(), "comm"))
		if err != nil {
			if vanished(err) {
				continue
			}
			// a process that cannot be inspected may be another kubelet or runtime.
			return nil, nil, fmt.Errorf("read the name of process %s: %w", e.Name(), err)
		}
		comm := strings.TrimSpace(string(b))
		if zombie(fx, e.Name()) {
			continue // exited and not yet reaped: nothing running, though its name remains
		}
		live[e.Name()] = comm
		p := fmt.Sprintf("%s (pid %s)", comm, e.Name())
		switch {
		case matchComm(comm, sharingComms):
			kubelets = append(kubelets, p)
		case matchComm(comm, runtimeComms):
			runtimes = append(runtimes, p)
		}
	}
	if len(kubelets) > 0 {
		return append(kubelets, runtimes...), nil, nil
	}
	if len(runtimes) == 0 {
		return nil, nil, nil
	}
	pods, err := podEvidence(fx, live)
	if err != nil {
		return nil, nil, fmt.Errorf("%s running: could not check whether anything runs under kubepods: %w", strings.Join(runtimes, ", "), err)
	}
	if len(pods) > 0 {
		return append(runtimes, pods...), nil, nil
	}
	return nil, runtimes, nil
}

// a process is named, so an orphaned runc init is not read as the runtime's.
func podEvidence(fx *hostEffects, live map[string]string) ([]string, error) {
	var found []string
	for _, pid := range slices.Sorted(maps.Keys(live)) {
		b, err := os.ReadFile(filepath.Join(fx.procRoot, pid, "cgroup"))
		if err != nil {
			if vanished(err) {
				continue
			}
			return nil, fmt.Errorf("read the cgroup of process %s: %w", pid, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			// "hierarchy-ID:controllers:path"; the path may hold anything.
			parts := strings.SplitN(line, ":", 3)
			if len(parts) == 3 && underKubepods(parts[2]) {
				found = append(found, fmt.Sprintf("%s under kubepods (pid %s)", live[pid], pid))
				break
			}
		}
	}
	for _, name := range []string{"kubepods.slice", "kubepods"} {
		root := filepath.Join(fx.cgroupRoot, name)
		var errs []error
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err)
				}
				return nil
			}
			if d.IsDir() || d.Name() != "cgroup.procs" {
				return nil
			}
			b, err := os.ReadFile(p)
			switch {
			case err != nil && !errors.Is(err, fs.ErrNotExist):
				errs = append(errs, err)
			case strings.TrimSpace(string(b)) != "":
				found = append(found, "a process in "+filepath.Dir(p))
			}
			return nil
		})
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
	}
	return found, nil
}

func underKubepods(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if strings.Contains(seg, "kubepods") {
			return true
		}
	}
	return false
}

func vanished(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// a zombie keeps its /proc name but runs nothing and holds no cgroup member.
func zombie(fx *hostEffects, pid string) bool {
	b, err := os.ReadFile(filepath.Join(fx.procRoot, pid, "stat"))
	if err != nil {
		return vanished(err) // gone since its name was read; anything else is not shown dead
	}
	// "pid (comm) S ...": the state follows the last ")", comm may hold anything.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 >= len(b) {
		return false
	}
	return b[i+2] == 'Z' || b[i+2] == 'X'
}

// systemd's units stay active after their cgroup directories are removed, so the slice is stopped.
func removeKubepodsCgroups(fx *hostEffects) error {
	var errs []error
	for _, name := range []string{"kubepods.slice", "kubepods"} {
		root := filepath.Join(fx.cgroupRoot, name)
		slice := strings.HasSuffix(name, ".slice")
		exists, err := cgroupExists(root)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !exists {
			// a directory that is gone is not a unit that is: ask systemd.
			if !slice {
				continue
			}
			active, err := sliceActive(fx, name)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !active {
				continue
			}
		}
		if err := noOtherRuntime(fx, root); err != nil {
			// the unit query can have made the tree since it was looked at.
			if !exists && errors.Is(err, errRuntimeRunning) {
				now, cerr := cgroupExists(root)
				exists = now || cerr != nil // not knowing it is gone is not it being gone
			}
			if exists || !errors.Is(err, errRuntimeRunning) {
				errs = append(errs, err)
			}
			continue
		}
		// the query and the runtime check take time, in which the tree can be made again.
		if exists, err = cgroupExists(root); err != nil {
			errs = append(errs, err)
			continue
		}
		if exists {
			if err := removeCgroupTree(root, cgroupRemoveWait); err != nil {
				errs = append(errs, fmt.Errorf("left what is still in use under %s: %w", root, err))
				continue
			}
		}
		if !slice {
			continue
		}
		// the stop is what could kill a new runtime's pod, so the check is made again.
		if err := noOtherRuntime(fx, root); err != nil {
			errs = append(errs, fmt.Errorf("not stopping %s: %w", name, err))
			continue
		}
		if err := stopSlice(fx, name); err != nil {
			errs = append(errs, err)
			continue
		}
		exists, err = cgroupExists(root)
		if err != nil {
			errs = append(errs, fmt.Errorf("after stopping %s: %w", name, err))
			continue
		}
		if exists {
			if err := removeCgroupTree(root, cgroupRemoveWait); err != nil {
				errs = append(errs, fmt.Errorf("left what is still in use under %s after stopping %s: %w", root, name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// the kernel's fixed PROC_PID_INIT_INO for the initial namespace.
const initPIDNamespace = "pid:[4026531836]"

func pidNamespace(fx *hostEffects) (string, error) {
	return os.Readlink(filepath.Join(fx.procRoot, "self", "ns", "pid"))
}

// in a private PID namespace a host kubelet is not there to find.
func hostPIDNamespaceGate(fx *hostEffects) error {
	if fx.assumeNoForeignRuntimes {
		return nil
	}
	if fx.pidNamespaceFn == nil {
		return unsetEffect("pidNamespaceFn")
	}
	const remedy = "Run it on the host, or in a container started with --pid=host. If you have checked yourself that no other kubelet or container runtime runs on this host, re-run with --assume-no-foreign-runtimes"
	ns, err := fx.pidNamespaceFn()
	if err != nil {
		return fmt.Errorf("uninstall: could not tell whether this process can see the host's processes (%v), so it cannot check for another kubelet or container runtime sharing what it would remove. %s", err, remedy)
	}
	if ns != initPIDNamespace {
		return fmt.Errorf("uninstall: this process runs in a private PID namespace (%s), so it cannot see whether another kubelet or container runtime on the host shares what it would remove. %s", ns, remedy)
	}
	return nil
}

var errRuntimeRunning = errors.New("another kubelet or container runtime is running")

func noOtherRuntime(fx *hostEffects, root string) error {
	if fx.foreignRuntimesFn == nil {
		return fmt.Errorf("not removing %s: %w", root, unsetEffect("foreignRuntimesFn"))
	}
	others, _, err := fx.foreignRuntimesFn()
	if err != nil {
		return fmt.Errorf("not removing %s: could not check for another kubelet or container runtime: %w", root, err)
	}
	if len(others) > 0 {
		return fmt.Errorf("left %s in place: it may be shared with another kubelet or container runtime that is running (%s): %w", root, strings.Join(others, ", "), errRuntimeRunning)
	}
	return nil
}

// not knowing it is gone is not it being gone.
func cgroupExists(root string) (bool, error) {
	_, err := os.Stat(root)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// `systemctl is-active` exits 0 active, 3 inactive (also failed, activating), 4 no such unit.
func sliceActive(fx *hostEffects, name string) (bool, error) {
	if fx.runCmd == nil {
		return false, unsetEffect("runCmd")
	}
	_, err := fx.runCmd("", "systemctl", "is-active", "--quiet", name)
	if err == nil {
		return true, nil
	}
	var ee interface{ ExitCode() int }
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return false, nil
	case errors.As(err, &ee) && (ee.ExitCode() == 3 || ee.ExitCode() == 4):
		return false, nil
	}
	return false, fmt.Errorf("could not tell whether the systemd unit %s is active, so it was not stopped: %w", name, err)
}

func stopSlice(fx *hostEffects, name string) error {
	if fx.runCmd == nil {
		return unsetEffect("runCmd")
	}
	if _, err := fx.runCmd("", "systemctl", "stop", name); err != nil {
		if errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "not loaded") {
			return nil
		}
		return fmt.Errorf("stop the systemd unit %s, which still owns the cgroups kubelet created: %w", name, err)
	}
	return nil
}

// a cgroup's interface files cannot be unlinked, so rmdir deepest first, retrying EBUSY.
func removeCgroupTree(root string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := rmdirDeepestFirst(root)
		if err == nil || !time.Now().Before(deadline) {
			return err
		}
		time.Sleep(pollInterval)
	}
}

func rmdirDeepestFirst(root string) error {
	var dirs []string
	var errs []error
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A tree that could not be read is not known to be empty.
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for _, d := range slices.Backward(dirs) {
		if err := os.Remove(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

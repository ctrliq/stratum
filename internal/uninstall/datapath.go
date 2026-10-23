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

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/toolenv"
)

// Node and Cilium datapath state that outlives the supervisor: the
// CNI network-namespace bind mounts, Cilium's links, netfilter chains and BPF
// pins, and the kubepods cgroups. None of it lives under version.DataDir, so
// the delete loop never reached it. Order matters, and Run only calls this once
// kubelet and every container are stopped: the netns mounts are detached first
// (they keep pod namespaces, and so their veth peers, alive), then Cilium's
// links, chains and pins go, then the cgroups, which cannot be removed while a
// process is still in one.
//
// Every step is a no-op when its state is already gone, so a second uninstall
// does nothing here.
//
// What is Cilium's is decided the way Cilium's own `cilium-dbg post-uninstall-cleanup`
// does (the pinned agent version's): links by name, maps in the shared bpffs tc/globals tree by the
// cilium_ prefix, its dedicated bpffs cilium/ subtree whole, tc filters whose
// name carries cil_/cilium. Anything else in those shared trees is another
// program's and is left. A directory is never removed through a mount that
// could not be detached.
//
// ponytail: found by name, not through the agent, whose image may not be
// pullable at this point. XDP programs attached outside a bpf link are not
// detached (a link pinned under bpffs cilium/ goes with the pins); add an
// `ip link set dev X xdp off` sweep if one ever has to be supported.

// The filesystem layout the datapath teardown reads is per-run: every path is
// a field on the run's hostEffects value (fx.netnsDir, fx.bpfRoot and the
// rest), set by Run, so two Runs share no state and a test points them at a
// temp tree by setting the fields. The wait and timeout bounds below are read,
// never written, once the package is initialized, so two Runs cannot race over
// them; tests shorten them, which is why they stay variables.
var (
	cgroupRemoveWait = 10 * time.Second

	// cgroupProgramWait bounds how long removeCiliumCgroupPrograms keeps listing
	// a program whose detach failed, for the kernel to release it.
	cgroupProgramWait = 10 * time.Second

	// toolPath is where the tools uninstall runs (ip, tc, iptables, bpftool,
	// systemctl, umount) are looked for and what they see as PATH, not the
	// caller's, which can put a shim of any of them first.
	toolPath = toolenv.Path

	// cmdTimeout bounds every command, and xtablesWait the wait for the
	// iptables lock inside it, so one held lock or wedged tool cannot hang the
	// teardown, and with it the worker's Node mark, with the node half torn
	// down.
	cmdTimeout  = 30 * time.Second
	xtablesWait = "5"
)

// errCommandTimeout is what a command that did not finish in cmdTimeout
// returns, so a loop over many commands can stop instead of waiting out each.
var errCommandTimeout = errors.New("timed out")

// execCmd is runCmd's real implementation, bounded like runCtr
// (containers.go): a context deadline, and a WaitDelay so a child that keeps
// the output pipe open cannot outlive it.
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

// toolCmd is exec.CommandContext with name looked up in toolPath, and toolPath
// as the PATH the command runs with. A name with a slash is used as given. The
// command is built from the resolved path: exec looks a bare name up on the
// caller's PATH as soon as it is given one, which a slow entry there can stall
// outside cmdTimeout. The environment is explicit, not the caller's: tools
// that load code from it (iptables from XTABLES_LIBDIR, tc from TC_LIB_DIR) run
// as root here, and callers match the C-locale text of what tools report.
func toolCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	path, err := lookTool(name)
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Err = err
	cmd.Env = []string{"PATH=" + toolPath, "LC_ALL=C"}
	return cmd
}

// lookTool is name's path in toolPath. When it is not there the path is still
// one with a slash, so exec does not look it up elsewhere, and the error is
// exec.ErrNotFound, which is also what the command then fails with.
func lookTool(name string) (string, error) { return toolenv.Look(toolPath, name) }

// ciliumLink matches the links Cilium creates: its host-side devices, the
// health endpoint and one veth per pod (lxc + 5 to 12 hex digits: the hash is
// 5 characters in current agents and longer in others). Anchored so an
// operator's lxcbr0 is left alone.
var ciliumLink = regexp.MustCompile(`^(cilium_[a-z0-9_]+|lxc_health|lxc[0-9a-f]{5,12})$`)

func teardownDatapath(w io.Writer, fx *hostEffects) error {
	// Cilium's links, filters, rules, pins and programs are found by name, which
	// does not say whose they are: a node that runs another kubelet or container
	// runtime with Cilium of its own has the same ones. Stratum's own are all
	// stopped by now, so anything that matches is not Stratum's; with one
	// running, or with no way to tell, every Cilium step is skipped and the
	// cleanup is not reported clean. The namespaces of Stratum's containers (proved
	// by the record) and the kubepods cgroups (their own guard) are not Cilium's.
	// A container runtime with no kubelet and nothing under kubepods, as Docker on
	// a developer's machine, uses neither, and is only said to be there.
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

// stratumNetns is the netns files (/run/netns/cni-<uuid>, as full paths) that
// this node's containers and pod sandboxes use, read from the containers' specs
// and the sandboxes' CRI metadata by recordStratumNetns while Stratum's
// containerd is still up, since both go with what they describe. The cni-
// prefix is containerd's, not Stratum's, so a separate host containerd's live
// namespaces carry it too, and a name alone does not say whose a namespace is:
// only a path a Stratum container or pod sandbox names, inside the run's netns
// directory, is ever unmounted or removed.
//
// The record is also saved, at the run's netnsRecordPath, before the first
// container is stopped, so that ownership is durable before it can be lost, and
// a later run, whose containers and state are gone, can finish a detach that
// failed. It says whether it is complete: when any container could not be read,
// the namespaces identified are still detached, but the record stays
// incomplete, and an incomplete record never ends in a clean result, since a
// namespace it does not name may still be mounted. It is deleted only when it
// is complete and everything in it is detached and removed (it is in /run,
// which a reboot clears along with the mounts it names).
//
// What one run learned lives on that run's hostEffects value (fx.stratumNetns,
// fx.stratumNetnsErr, fx.netnsEnumerated), seeded fresh at the runner's entry:
// Run can be called again in a persistent process, and a discovery answer left
// by an earlier call must never stand in for what the next has not read.

// errKeepState marks a teardown failure after which DataDir must be kept: the
// saved record of this node's namespaces could not be read, so what is left to
// detach is not known, and what is under DataDir is the last thing that could
// say. Run keeps it, and the rest of the cleanup carries on.
var errKeepState = errors.New("keeping the node state")

// netnsRecord is the saved record.
type netnsRecord struct {
	Complete bool     `json:"complete"`
	Paths    []string `json:"paths"`
	// Unread is what could not be read, one entry per container (or a
	// reason), so an incomplete record says why.
	Unread []string `json:"unread,omitempty"`
}

// canonNetns is the run's netnsDir path for a network namespace path from a
// spec, or false when the path is not a file in that directory: /var/run is an
// alias for /run, and any other directory, even one that ends in netns, is not
// the one uninstall detaches from.
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

// goneFromListing reports whether id is gone: the listing ctr runs with list
// answers, and ids of its output no longer names id. A listing that fails
// proves nothing, so the read error that led here stands; its error is
// returned, so the caller can stop after a timeout.
func goneFromListing(fx *hostEffects, ids func(string) []string, id string, list ...string) (bool, error) {
	out, err := fx.ctrRun(list...)
	return err == nil && !slices.Contains(ids(out), id), err
}

// recordStratumNetns reads the network namespace of every container and every
// pod sandbox in every containerd namespace of this node's containerd, and saves
// the record, before any container is stopped. A save that fails is returned: nothing has been torn
// down yet, so the caller can stop with the node as it is, rather than lose what
// says which namespaces are Stratum's. Anything that could not be read, a
// command, a timeout or a spec that does not decode, is not returned: it is
// kept with the container it concerns, makes the record incomplete (and
// saved so), and must not abort the teardown. A container or pod sandbox whose
// read fails and that a second listing, which answers, no longer names is the
// exception: it was removed between the listing and the read, so it has no
// namespace to detach and it is not unread. The error text cannot tell this:
// `ctr sandboxes info` drops containerd's not found for its own message, and
// `ctr containers info` passes on spec decoding errors that say not found too.
// After a timeout, of a read or of that second listing, the enumeration ends,
// since every further call would wait as long. An earlier record that is
// incomplete keeps this one incomplete. One that cannot be read is returned like
// a failed save, and left untouched.
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
					// The container was removed between the listing and this read:
					// it has no namespace to detach, so it is not unread.
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
					// Gone like a container the listing dropped above: removed
					// after the listing, with no namespace to detach.
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
		// Like a save that fails: stop with nothing torn down, and the file as it
		// is. It may hold ownership an earlier run saved that the containers'
		// metadata cannot give back, so it is never replaced by one that reads.
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
		return nil // nothing to remember
	}
	if err := saveNetnsRecord(fx, rec); err != nil {
		return fmt.Errorf("save the record of this node's network namespaces to %s: %w", fx.netnsRecordPath, err)
	}
	return nil
}

// containerNetnsPaths is the network namespace path in a container's OCI spec,
// from `ctr containers info`. Output that does not decode is an error: an empty
// answer would read as a container with no namespace to detach.
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

// sandboxNetnsPath is the network namespace path the CRI keeps in a pod
// sandbox's metadata extension, from `ctr sandboxes info`, or "" for a sandbox
// on the host network. The CRI saves it there as soon as it mounts the
// namespace, before the network is set up and before the pause container
// exists, so a sandbox whose setup was cut short names a namespace that no
// container spec does.
//
// The extension is containerd's own record, not an API: the "metadata" key and
// the {"Version":"v1","Metadata":{...}} wrapper are what containerd 2.3 writes
// for every sandbox it creates, and it drops a sandbox without one when it
// restarts. Anything else, a missing extension, another version, or output that
// does not decode, is an error rather than "", so a change in that format makes
// the record incomplete instead of reading as a sandbox with no namespace.
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

// loadNetnsRecord is the saved record, nil when there is none. One that cannot
// be read is an error: it may name namespaces that are still mounted.
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

// saveNetnsRecord writes the record whole and atomically.
func saveNetnsRecord(fx *hostEffects, rec netnsRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := fx.netnsRecordPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, fx.netnsRecordPath)
}

// networkMounts is every netns bind mount in owned (full paths in the run's
// netns directory) and every mount under Cilium's run directory (its cgroup2
// mount), deepest first.
func networkMounts(fx *hostEffects, owned map[string]bool, cilium bool) ([]string, error) {
	// An empty directory would produce a "/" prefix and select every host
	// mount. Require the layout before consulting the mount table.
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

// unattributedNetns is an error, asking Run to keep DataDir, when the containers
// were not read this run (containerd stopped and no shim left), there is no saved
// record, and yet Stratum's containerd kept the metadata of containers and a
// netns is mounted under the run's netns directory: nothing says whether it is
// Stratum's, so it is left, and the cleanup is not clean. With no containerd
// metadata, or no mount, there is nothing to attribute.
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

// detachNetns unmounts the netns files of this node's containers, and Cilium's
// mounts under /run/cilium when cilium is true (they are found by where they
// are, not by proof, and are left when another runtime may share them), then
// removes the now-empty netns files (removing a file that is still a mount
// point would fail with EBUSY). The unmount is an effect of the run, read
// from fx.
//
// The namespaces are those recorded this run and those in the saved record of an
// earlier one. Whatever is identified is detached, and the saved record is deleted
// only once it is complete and everything in it is detached and removed. A record
// that is incomplete, because some container could not be read, now or in an
// earlier run, is an error however much was detached: a namespace it does not
// name may still be mounted. A saved record that cannot be read is too, and also
// asks Run to keep DataDir (errKeepState).
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

// removeCiliumLinks deletes Cilium's links. A veth's peer goes with it, so a
// delete that fails is only an error when the link is still there afterwards.
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

// tcFilterLine matches the line `tc filter show` prints for one bpf filter:
//
//	filter protocol all pref 1 bpf chain 0 handle 0x1 cil_from_netdev-eth0 direct-action ...
var tcFilterLine = regexp.MustCompile(`^filter (?:parent \S+ )?protocol (\S+) pref (\d+) bpf (?:chain (\d+) )?handle (\S+) (\S+)`)

// ciliumTCFilters is the (protocol, pref, handle, chain) of every Cilium filter
// in a `tc filter show` dump (chain 0 when the line has none). The chain is part
// of the filter's identity: a foreign filter in chain 0 can share the other
// three. They are found by the name the loader gives the program
// (cil_from_netdev-eth0, cil_to_host...; cilium_* from agents before 1.13.1).
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

// removeCiliumTCFilters detaches Cilium's tc programs from the interfaces that
// outlive it (the host's). Kernels without tcx (below 6.6, and the platform
// floor is 4.19.57) attach them as clsact filters, which removing the pins does
// not detach: they keep their maps and keep steering host traffic. Only
// Cilium's filters go, and the qdisc stays. Without `tc` (iproute-tc) there is
// nothing to enumerate them with, and uninstall cannot tell a node with no such
// filters from one that still has them, so a missing tc is an error, not a
// clean result. Run reports it after the remaining cleanup has run.
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
				// No qdisc means no filters, and a device that went away has none.
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

// removeCiliumChains removes Cilium's rules and chains from iptables and
// ip6tables, one rule or chain at a time: the jumps to its chains are deleted,
// then the chains flushed and deleted. The ruleset is never replayed, so a rule
// another program adds meanwhile survives. Nothing is run for a node with none,
// so a second run changes nothing.
//
// A missing iptables-save is an error, not "no rules": Cilium runs in a
// container and adds its rules with the tools inside it, so a host without the
// iptables userland can still have them.
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
					// The lock is likely still held, and both tools share it: not
					// worth waiting out every remaining command.
					return errors.Join(errs...)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// waitExhausted is whether err says a command gave up waiting: the deadline in
// execCmd, or what iptables itself reports when its -w wait runs out ("Another
// app is currently holding the xtables lock", exit status 4).
func waitExhausted(err error) bool {
	return errors.Is(err, errCommandTimeout) || strings.Contains(err.Error(), "holding the xtables lock")
}

// ciliumChainCleanup is the iptables arguments that remove Cilium's rules and
// chains from an iptables-save dump, in the order they must run: the jumps to a
// Cilium chain from any other chain (-D, with the rule as the dump prints it, so
// it only matches that rule), then every Cilium chain flushed, then deleted. A
// chain cannot be deleted while another still jumps to it.
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

// ciliumChainName is whether s starts with the name of a chain Cilium owns:
// its own CILIUM_ prefix, or the OLD_CILIUM_ prefix the agent renames its
// chains to while it reinstalls them at every start, and can leave behind,
// with or without rules, when it stops between the rename and the cleanup.
// Both are Cilium's and both are removed.
func ciliumChainName(s string) bool {
	return strings.HasPrefix(s, "CILIUM_") || strings.HasPrefix(s, "OLD_CILIUM_")
}

// jumpsToCiliumChain is whether a parsed `-A <chain> ...` rule, in a chain that
// is not Cilium's own, has a Cilium chain as its target. The target is the
// -j/-g option and its argument that end the rule, as iptables-save prints them:
// read from the parsed arguments, never from the line, so a --comment that
// mentions "-j CILIUM_X" (one quoted argument) cannot select a rule that does
// not jump there.
func jumpsToCiliumChain(args []string) bool {
	n := len(args)
	return n >= 4 && args[0] == "-A" && !ciliumChainName(args[1]) &&
		(args[n-2] == "-j" || args[n-2] == "-g") && ciliumChainName(args[n-1])
}

// splitRule splits an iptables-save rule into arguments: on spaces, with a
// double-quoted argument (a --comment) kept whole and its \" and \\ unescaped.
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

// removeCiliumState removes Cilium's BPF pins and its run directory: its
// dedicated bpffs cilium/ subtree, and the cilium_* maps of the shared
// tc/globals tree (the shared tree, and anything else in it or in xdp/, is
// other programs'; the then-empty tc/globals and tc are removed only if empty).
// The bpffs mount itself is the host's and stays. Nothing is removed through a
// mount that is still there: the run directory holds Cilium's cgroup2 mount,
// and deleting through it would delete the host's cgroup hierarchy.
func removeCiliumState(fx *hostEffects) error {
	var errs []error
	tcDir := filepath.Join(fx.bpfRoot, "tc")
	// The shared tc tree is checked like the directories below: a mount at or
	// below it (the bpffs root mount itself is the host's, and is fine) is not
	// ours to delete through, and neither is a table that cannot be read.
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
		// rmdir fails on a directory that still holds another program's pins,
		// which is the point; any other failure is reported.
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

// refuseMounted is an error when dir, or anything below it, is a mount point,
// or when the mount table cannot be read.
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

// removeAllUnlessMounted removes dir recursively, after reading the mount table
// again: a mount at or below it, or a table that cannot be read, leaves it
// alone and is an error, since RemoveAll does not stop at a mount boundary.
func removeAllUnlessMounted(fx *hostEffects, dir string) error {
	if err := refuseMounted(fx, dir); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// kubeletFixedSockets are the sockets kubelet creates at paths fixed in the
// kubelet source (the device-plugin manager's), whatever --root-dir is,
// relative to the run's legacyKubeletDir. A kubelet that is killed leaves them
// behind. The pod-resources socket is not one: kubelet serves it under
// --root-dir, which the DataDir wipe removes, so one under legacyKubeletDir is
// another kubelet's.
var kubeletFixedSockets = []string{"device-plugins/kubelet.sock"}

// kubeletCertDirRel is the kubelet's fixed --cert-dir default (internal/pki's
// kubeletDefaultCertDir), relative to the run's legacyKubeletDir: where the
// kubelet's certificate manager keeps its client and serving pairs. Stratum's
// kubelet leaves them there although its --root-dir is under DataDir, so they
// outlive the DataDir wipe unless this removes them.
const kubeletCertDirRel = "pki"

// removeStratumSignedKubeletCerts removes the certificate files in certDir that
// stratum's CA bundle signed — stratum's kubelet's cert-manager pairs, whose
// proofs are positive or nothing: a file whose certificate is signed by
// stratum's roots is the kubelet's own state, one signed by another CA is a
// co-hosted kubelet's and is kept and said so, and a file that cannot be read
// or judged proves nothing either way, so it is kept too. Only regular files
// are candidates; nothing is removed through a symlink. The caller has already
// established that nothing at or below the kubelet dir is mounted.
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
	// Identify regular files before deleting any: the current links must be
	// checked while their timestamped targets still exist.
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
		// client-go uses absolute targets; accept a relative basename too,
		// but never a path that traverses another directory or another link.
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

// removeLegacyKubeletDir removes what Stratum's kubelet left in the run's
// legacy kubelet directory and then the directory when nothing else is in it:
// its stale fixed-path sockets, its cert-manager pairs under the fixed
// --cert-dir default that stratum's CA signed, and the then-empty directories
// that held them.
//
// Whose a file is, and whether a socket is stale, is only taken as established
// when it is positive, and otherwise the file is kept and said so: nothing is
// removed below a mount (a mount table that cannot be read is an error); the
// directory and a file's own directory must be real directories, not symlinks,
// so nothing is removed through one into somewhere else; a socket is stale only
// when a connection to it is refused (one that answers is live, and any other
// result, a datagram socket's wrong-type error, a permission error or a
// timeout, proves nothing); and a certificate is stratum's kubelet's only when
// stratum's CA bundle signed it. So another kubelet's state there is kept, the
// directory itself included (rmdir of a mount point is EBUSY).
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
			// Already gone, as after an earlier run that removed the socket:
			// its directory, which was just shown to be a real directory below
			// no mount, is still removed if that left it empty.
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
			_ = c.Close() // answered: a live kubelet's, not left behind
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
	// Stratum's kubelet's cert pairs, under the kubelet's fixed --cert-dir
	// default: removed when provably stratum's (above), the directory itself
	// only once nothing foreign is left in it.
	certDir := filepath.Join(fx.legacyKubeletDir, kubeletCertDirRel)
	if cdi, err := os.Lstat(certDir); err == nil && cdi.IsDir() {
		if err := removeStratumSignedKubeletCerts(w, certDir); err != nil {
			// A verified pair that could not be removed keeps DataDir: the
			// CA bundle under it is what decides whose a certificate is, so
			// deleting the state would leave the leftover unprovable — and
			// a later uninstall would keep it forever instead of retrying.
			errs = append(errs, fmt.Errorf("remove Stratum's kubelet's verified certificate pairs: %w; %w", err, errKeepState))
		}
		_ = os.Remove(certDir) // fails while a foreign or unjudged file is in it, which is the point
	}
	switch err := os.Remove(fx.legacyKubeletDir); {
	case err == nil || errors.Is(err, fs.ErrNotExist):
	case errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EBUSY):
		// Not Stratum's kubelet root (that is under its state directory): the
		// kubelet's default, where it still keeps a few fixed paths. Say so, or
		// the line reads as Stratum claiming the directory.
		keep("it is the kubelet's default directory, where Stratum's kubelet leaves only its stale device-plugin socket and certificate pairs, and it holds something else, or is in use")
	default:
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ciliumCgroupPrograms is the (attach type, id) of every Cilium program in a
// `bpftool cgroup show` listing:
//
//	ID       AttachType      AttachFlags     Name
//	1234     connect4        multi           cil_sock4_connect
//
// found by the cil_ name prefix the loader gives its programs (the kernel
// truncates a name to 15 characters, so only the prefix is reliable).
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

// removeCiliumCgroupPrograms detaches Cilium's socket-LB programs from the root
// cgroup. Agents on kernels below 5.7, and nodes upgraded from them, attach them
// with BPF_PROG_ATTACH rather than a bpf link, so no pin names them and deleting
// the pins detaches nothing: they would go on rewriting every socket call on the
// host, to backends that no longer exist. Run after the pins are gone, so what
// is left to detach is only what has no link. Without bpftool there is nothing
// to list them with, and uninstall cannot tell a node with none from one that
// has them, so that is an error, like a missing tc.
//
// A program whose bpf link was pinned is detached by the kernel once the pin is
// gone, but not at once: the kernel releases a link from a work queue, so the
// listing can still show the program. Its detach by id then fails, because a
// linked program cannot be detached by id, or because bpftool no longer finds
// it by the time it looks it up. So a failed detach is checked against fresh
// listings until cgroupProgramWait has passed, and only a program still
// attached then is reported.
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

// listCiliumCgroupPrograms lists the Cilium programs attached to the root
// cgroup (ciliumCgroupPrograms).
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

// errCgroupShowBug marks a `bpftool cgroup show` that failed in its check for
// any attached program, before listing: bpftool 7.4 and earlier also query
// non-cgroup attach types, which a kernel with tcx or netkit answers with
// ENODEV or ENXIO, and 7.5.0 fails on EINVAL from a kernel older than it. Both
// are fixed in bpftool 7.6.
var errCgroupShowBug = errors.New("this bpftool cannot list the root cgroup's programs")

// cgroupShowBug reports whether a `bpftool cgroup show` failure is that bug:
// the attached-program check failed with one of the errors it returns. The
// same check failing for any other reason, such as a permission error, is not
// the bug and stays a listing failure.
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

// awaitNoLoadedCiliumCgroupPrograms stands in for the listing when bpftool
// cannot produce it: a program attached to a cgroup is loaded, so none of
// Cilium's cgroup programs loaded means none attached. One the kernel is still
// releasing is waited for, as a failed detach is.
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

// loadedCiliumCgroupPrograms names, from `bpftool -j prog show`, every loaded
// program with Cilium's cil_ prefix and a cgroup program type. bpftool prints
// a type it has no name for as a number; such a program counts.
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

// detachFailures joins the errors of the failed detaches, in listing order.
func detachFailures(progs [][2]string, failed map[[2]string]error) error {
	var errs []error
	for _, p := range progs {
		if err, ok := failed[p]; ok {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sharingComms and runtimeComms are the names of the processes that run pods or
// containers, or Cilium. Stratum's own are all stopped by the time the datapath
// is torn down, so one still running is another's.
//
// sharingComms are a kubelet, alone or embedded (k3s runs its kubelet inside
// k3s-server or k3s-agent, MicroK8s inside kubelite), and a Cilium agent, which
// can run without one (in a Docker container, for a Cilium external workload):
// one running is always taken to share Cilium's datapath and the kubepods
// cgroups. runtimeComms are container runtimes, which share them only by
// serving one of those: one running is taken to share them when a kubelet or
// Cilium agent is running too, or when anything runs in a kubepods cgroup
// (podEvidence),
// and is otherwise a bystander, as Docker on a developer's machine is, whose
// containers are under system.slice.
//
// A name ending in * is a prefix; any other matches itself, or itself followed
// by a dash. Broad on purpose: a match is a reason to keep what is shared and
// say so, never to remove anything, and a bystander is only one while nothing
// shows there are pods.
var (
	sharingComms = []string{"kubelet", "k3s*", "rke2*", "kubelite", "k0s", "cilium-agent", "cilium-envoy"}
	runtimeComms = []string{"containerd", "crio", "dockerd", "cri-dockerd", "podman", "conmon", "crun"}
)

// matchComm is whether comm is one of names, by the rule above.
func matchComm(comm string, names []string) bool {
	for _, r := range names {
		if prefix, ok := strings.CutSuffix(r, "*"); (ok && strings.HasPrefix(comm, prefix)) || (!ok && (comm == r || strings.HasPrefix(comm, r+"-"))) {
			return true
		}
	}
	return false
}

// foreignRuntimes names, from the run's procRoot, every kubelet, Cilium agent
// or container runtime process still running that may share Cilium's datapath
// and the kubepods cgroups (foreign), and every container runtime that runs
// nothing under kubepods with no kubelet or Cilium agent running (bystanders).
// Comparing by name is deliberately broad, and anything that cannot be
// inspected is an error: not being able to look is not there being nothing to
// see.
func foreignRuntimes(fx *hostEffects) (foreign, bystanders []string, err error) {
	entries, err := os.ReadDir(fx.procRoot)
	if err != nil {
		return nil, nil, err
	}
	var kubelets, runtimes []string
	live := map[string]string{} // pid to name
	for _, e := range entries {
		if strings.Trim(e.Name(), "0123456789") != "" {
			continue // not a pid
		}
		b, err := os.ReadFile(filepath.Join(fx.procRoot, e.Name(), "comm"))
		if err != nil {
			if vanished(err) {
				continue // exited between the listing and this read
			}
			// A process that cannot be inspected may be another kubelet or runtime.
			return nil, nil, fmt.Errorf("read the name of process %s: %w", e.Name(), err)
		}
		comm := strings.TrimSpace(string(b))
		if zombie(fx, e.Name()) {
			continue // exited and not yet reaped: nothing is running, though its name remains
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

// podEvidence names what shows that pods run on the node: a live process whose
// cgroup, in any hierarchy, is under a kubepods one (/proc/<pid>/cgroup), and a
// kubepods cgroup under the run's cgroupRoot that has a process in it (its
// cgroup.procs), which under cgroup v2 also finds one /proc does not show.
// Stratum's own are stopped by now, so either is taken to be another kubelet's
// pod. A file that cannot be read, other than one whose process has gone, is an
// error. A process is named, so that one of Stratum's own left behind (a
// container's runc init orphaned mid-start) is not read as the runtime beside
// it.
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

// underKubepods is whether a cgroup path has a kubepods cgroup in it: kubepods
// under cgroupfs, kubepods.slice and its children under systemd, and a nested
// kubelet's (kind's kubelet-kubepods.slice).
func underKubepods(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if strings.Contains(seg, "kubepods") {
			return true
		}
	}
	return false
}

// vanished is whether err says the process (or its /proc entry) is gone.
func vanished(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// zombie is whether the process is one that has exited and is waiting to be
// reaped, which keeps its name in /proc but runs nothing and holds no cgroup
// member (a killed shim under a parent that does not reap, as in a container).
func zombie(fx *hostEffects, pid string) bool {
	b, err := os.ReadFile(filepath.Join(fx.procRoot, pid, "stat"))
	if err != nil {
		return vanished(err) // gone since its name was read; anything else is not shown to be dead
	}
	// "pid (comm) S ...": the state follows the last ")", comm may hold anything.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 >= len(b) {
		return false
	}
	return b[i+2] == 'Z' || b[i+2] == 'X'
}

// removeKubepodsCgroups removes kubelet's pod cgroup tree: kubepods.slice
// under the systemd driver, kubepods under cgroupfs. The tree is rmdir'd,
// deepest first, which the kernel refuses for a cgroup that still has a process
// in it, so nothing is killed to empty it.
//
// Under the systemd driver the cgroups are systemd's: kubelet and the runtime
// create each slice as a transient unit, and the units stay active after the
// directories are removed from under them. Removing the directories alone left
// the node as it was found on a real one: the units still loaded, active and
// owning cgroups that no longer exist, and the daemon-reload at the end of
// uninstall made systemd create kubepods.slice again, empty. So once the tree is
// gone, and so known to be empty, the slice is stopped, which stops the units
// beneath it too and has nothing left to kill; and it is also stopped when its
// directory is already gone but the unit is still active, as after such a run.
//
// The names are not Stratum's alone: a kubelet and container runtime of the
// distro share them, and a cgroup that is empty now is not shown to be
// Stratum's. So nothing is touched, removed or stopped, when another kubelet or
// runtime is running, and a tree that is there is kept, whole, and said so. A
// cgroup that still has a process in it after Stratum's are stopped is also kept
// and reported, and its slice is not stopped.
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
			// A directory that is gone is not a unit that is: ask systemd. Only a
			// known "inactive" or "no such unit" is no; a query that failed or timed
			// out is reported, since an active unit makes the directory again.
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
			// A runtime's own unit with no tree is nothing to report, but the
			// unit query can have made the tree since it was looked at.
			if !exists && errors.Is(err, errRuntimeRunning) {
				now, cerr := cgroupExists(root)
				exists = now || cerr != nil // not knowing it is gone is not it being gone
			}
			if exists || !errors.Is(err, errRuntimeRunning) {
				errs = append(errs, err)
			}
			continue
		}
		// The tree as it is now, not as it was found: the query above, and the
		// runtime check, take time in which something can make it again.
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
		// The removal retries for seconds, and a runtime can start in them: the
		// check is made again right before the stop, which is what could kill.
		if err := noOtherRuntime(fx, root); err != nil {
			errs = append(errs, fmt.Errorf("not stopping %s: %w", name, err))
			continue
		}
		if err := stopSlice(fx, name); err != nil {
			errs = append(errs, err)
			continue
		}
		// Stopping removes what is left of the cgroup; if systemd made it again
		// meanwhile it is empty, since nothing runs in it, and goes too.
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

// initPIDNamespace is the host's PID namespace as /proc/<pid>/ns/pid names it:
// the kernel gives the initial namespace the fixed inode PROC_PID_INIT_INO.
const initPIDNamespace = "pid:[4026531836]"

// pidNamespace is this process's PID namespace, read from the run's procRoot.
func pidNamespace(fx *hostEffects) (string, error) {
	return os.Readlink(filepath.Join(fx.procRoot, "self", "ns", "pid"))
}

// hostPIDNamespaceGate refuses a run that cannot see the host's processes, or
// cannot tell whether it can: the runtime guard reads /proc, and in a private
// PID namespace a host kubelet or runtime is not there to find.
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

// errRuntimeRunning marks the report that another kubelet or runtime is running.
var errRuntimeRunning = errors.New("another kubelet or container runtime is running")

// noOtherRuntime is nil when no other kubelet or Cilium agent, or container
// runtime that may serve one, is running (a bystander runtime, which
// foreignRuntimes names apart, is not one). A match is an error wrapping
// errRuntimeRunning, that says what; a /proc, or with a runtime running a
// process's cgroup or the kubepods tree, that cannot be read is an error too,
// since nothing is then known.
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

// cgroupExists is whether the cgroup directory is there. Not knowing is an
// error: not knowing it is gone is not it being gone.
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

// sliceActive is whether systemd has the slice unit active. `systemctl
// is-active` exits 0 for active, and 3 for inactive (or failed, activating...)
// and 4 for no such unit, which are the only other answers that are an answer: no
// systemctl is no systemd. Anything else, a timeout or a failed query to systemd,
// says nothing about the unit and is an error.
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

// stopSlice stops the systemd slice unit and, with it, the units beneath it. A
// unit that is not loaded, or no systemctl, is nothing to stop.
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

// removeCgroupTree rmdirs root and every cgroup below it, deepest first: a
// cgroup directory's interface files cannot be unlinked, so RemoveAll does not
// work here. A cgroup whose processes are still exiting is EBUSY for a moment,
// so it retries until wait elapses.
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

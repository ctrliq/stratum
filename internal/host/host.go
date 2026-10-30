// Package host performs node prerequisites: directories, kernel modules,
// sysctls, swapoff, and node-IP/hostname detection.
package host

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/version"
)

// Hostname returns the short hostname (hostname -s equivalent).
func Hostname() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	h, _, _ = strings.Cut(h, ".")
	return h, nil
}

// NodeIP returns $NODE_IP if set, else the IP of the default outbound route.
// A detection failure names $NODE_IP, the one source it reads.
//
// It knows nothing about the address a node was actually brought up with. Code
// acting on a node that already runs wants nodestate.NodeIP, which prefers the
// recorded address; this is the bare fallback beneath it.
func NodeIP() (string, error) {
	c, err := resolveNodeIP([]NodeIPChoice{{os.Getenv("NODE_IP"), "$NODE_IP"}}, detectIPv4)
	return c.IP, err
}

// dialUDP and interfaceAddrs are the system calls node-IP detection rests on:
// detectIPv4 uses both, nodeIPv6 the interface listing. They are vars so a test
// can stand in for a host without a default route and decide its addresses.
var (
	dialUDP        = net.Dial
	interfaceAddrs = net.InterfaceAddrs
)

// detectIPv4 returns the source address of the default outbound route: dial a
// public IP over UDP (no packet is sent) and read the local address chosen. On a
// host with more than one NIC this is often not the cluster network, which is
// why every explicit source outranks it (ResolveNodeIP).
//
// With no default route the dial fails, so it falls back to the
// host's global unicast IPv4 when exactly one exists. With none or several it
// errors rather than guess a cluster address. The error says why detection
// failed but names no source to pin the address with: its callers read
// different sources, and each names its own (resolveNodeIP).
func detectIPv4() (string, error) {
	conn, err := dialUDP("udp", "8.8.8.8:80")
	if err == nil {
		defer conn.Close()
		return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
	}
	addrs, aerr := interfaceAddrs()
	if aerr != nil {
		return "", fmt.Errorf("detect node IP: %w (and listing interface addresses failed: %v); autodetection uses the default route", err, aerr)
	}
	var found []string
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && ipn.IP.IsGlobalUnicast() {
			found = append(found, ipn.IP.String())
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	return "", fmt.Errorf("detect node IP: %w; autodetection uses the default route and this host has none, and it has %s", err, describeIPv4s(found))
}

func describeIPv4s(ips []string) string {
	if len(ips) == 0 {
		return "no global unicast IPv4 address to fall back on"
	}
	return fmt.Sprintf("%d global unicast IPv4 addresses (%s) to choose from", len(ips), strings.Join(ips, ", "))
}

// Primary returns the first (IPv4-primary) address of a possibly comma-joined
// node IP ("v4,v6" -> "v4"). Use it where a single IP is required — etcd peer/
// client URLs, the Cilium k8sServiceHost — while kubelet/PKI take the full pair.
func Primary(nodeIP string) string {
	if i := strings.IndexByte(nodeIP, ','); i >= 0 {
		return nodeIP[:i]
	}
	return nodeIP
}

// NodeIPs returns the value for kubelet --node-ip: the IPv4 from NodeIP, plus a
// node IPv6 when dualStack is requested (kubelet wants "<v4>,<v6>"). If $NODE_IP
// is set it is honored verbatim (it may already carry both, comma-separated).
// dualStack with no usable node IPv6 is a hard error rather than a half-configured
// node that would fail to register its IPv6. A detection failure names
// $NODE_IP, the one source it reads.
func NodeIPs(dualStack bool) (string, error) {
	c, err := resolveNodeIP([]NodeIPChoice{{os.Getenv("NODE_IP"), "$NODE_IP"}}, func() (string, error) { return DetectNodeIPs(dualStack) })
	return c.IP, err
}

// DetectNodeIPs is NodeIPs without the $NODE_IP override: the default-route
// IPv4, plus a node IPv6 under dual-stack. ResolveNodeIP uses it as the last
// resort, after every explicit source has been considered.
func DetectNodeIPs(dualStack bool) (string, error) {
	v4, err := detectIPv4()
	if err != nil {
		return "", err
	}
	if !dualStack {
		return v4, nil
	}
	v6, err := nodeIPv6()
	if err != nil {
		return "", err
	}
	return v4 + "," + v6, nil
}

// nodeIPv6 picks a node-global IPv6: the first non-loopback, non-link-local
// (fe80::/10) IPv6 on any interface — a ULA (fd00::/8) or global address.
// Like detectIPv4's, its error says why the lookup failed but names no source
// to pin the address with: each caller names the ones it reads (resolveNodeIP).
func nodeIPv6() (string, error) {
	addrs, err := interfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("enumerate interfaces for IPv6: %w", err)
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP
		if ip.To4() != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		return ip.String(), nil
	}
	return "", fmt.Errorf("dual-stack requested but no routable node IPv6 found; assign a ULA/global IPv6 to the node, or pin both addresses as a v4,v6 pair")
}

// The host paths bring-up writes outside stratum's three roots
// (version.DataDir, version.ConfDir, version.RunDir) are declared beside
// version.SystemdUnitPath — the one home, which the owning-package record
// keys and which this package reads: Prereqs writes, applySysctls loads and
// Footprint returns them through the version variables. ModulesLoadFile,
// SysctlConf and SysctlCISConf stay here as read-through aliases because
// internal/uninstall's delete list names them here; the literals live once,
// in version, so no second spelling of the paths exists for the writer, the
// sysctl loader and uninstall's delete list to drift between.
var (
	// ModulesLoadFile, SysctlConf and SysctlCISConf alias
	// version.ModulesLoadFile, version.SysctlConf and version.SysctlCISConf.
	ModulesLoadFile = version.ModulesLoadFile
	SysctlConf      = version.SysctlConf
	SysctlCISConf   = version.SysctlCISConf
	// CNIDir is where the CNI plugin's network configuration lives.
	CNIDir = "/etc/cni/net.d"
	// KubeletDir is the kubelet's default root directory. Stratum's kubelet
	// keeps its own state under version.DataDir, but its fixed-path serving
	// cert directory (internal/pki) stays here, and Dirs creates it.
	KubeletDir = "/var/lib/kubelet"
)

// Footprint returns the paths bring-up writes outside stratum's three roots:
// the modules-load file, both sysctl files (the CIS one under --cis) and the
// drop-in of every other registered profile that declares sysctls, the CNI
// dir, the kubelet dir, the audit log dir, the systemd unit, and containerd's
// certs.d and its registry and runtime-handler conf.d files. The paths
// version declares — the sysctl and modules-load files among them — are
// named through their variables and
// the /etc and /var drop-ins through the declarations above, so each path has
// one home. Uninstall covers every path of this set —
// the delete targets named from it, except the kubelet dir, which the
// teardown's removeLegacyKubeletDir removes under its keep-foreign-state
// contract instead of the plain delete loop — which is why the set is exactly
// bring-up's: a missing entry leaves a written path undeclared (and
// unremoved); an extra one makes uninstall delete a path bring-up does not
// own. /opt/local-path-provisioner is deliberately absent: SELinux only
// relabels that directory (spec.storage.localPathProvisioner), stratum never
// writes it.
func Footprint() []string {
	return append([]string{
		version.ModulesLoadFile,
		version.SysctlConf,
		version.SysctlCISConf,
		CNIDir,
		KubeletDir,
		version.AuditLogDir,
		version.SystemdUnitPath,
		version.CertsDir,
		version.ContainerdConfD,
		version.ContainerdRuntimeConfD,
	}, ProfileSysctlFiles()...)
}

// ProfileSysctlFiles are the sysctl drop-ins Prereqs writes for the binary's
// registered profiles that declare sysctls, other than the cis profile's,
// which is version.SysctlCISConf. A node runs only a profile its binary
// registers, so these cover every profile drop-in that binary's nodes carry.
func ProfileSysctlFiles() []string {
	var out []string
	for _, p := range profile.All() {
		if len(p.Sysctls) == 0 {
			continue
		}
		if f := profileSysctlFile(p); f != version.SysctlCISConf {
			out = append(out, f)
		}
	}
	return out
}

// publicDirs and privateDirs are the on-disk layout created by Dirs. The
// private dirs hold etcd data, private keys and credentialed kubeconfigs, so
// they are 0700; the rest are 0755.
func publicDirs() []string {
	return []string{
		version.ConfDir, version.K8sConfDir,
		version.K8sConfDir + "/manifests", KubeletDir,
		CNIDir, version.ManifestsDir,
	}
}

func privateDirs() []string {
	return []string{version.EtcdDataDir, version.PKIDir, version.KubeconfigDir, version.AuditLogDir}
}

// Dirs creates the on-disk layout used by the control plane. Note MkdirAll
// does not tighten a dir that already exists, so private modes are enforced
// with an explicit Chmod.
func Dirs() error {
	for _, d := range publicDirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	for _, d := range privateDirs() {
		if err := ensurePrivateDir(d); err != nil {
			return err
		}
	}
	return tightenEtcdRestoreCopies(version.EtcdDataDir)
}

// tightenEtcdRestoreCopies sets 0700 on the copies of the etcd data dir a
// snapshot restore keeps beside it (<dir>.pre-restore-*, <dir>.failed-restore-*).
// A restore renames the live dir, so a copy made while it was 0755 stays 0755
// with the whole cluster state in it. Symlinks are left alone, and a copy
// removed while this runs is not an error. The copies are found by name
// prefix, not a glob, so the data dir's path is never read as a pattern. A
// parent that cannot be listed is warned about, not an error: the sweep is
// best-effort, and Dirs has already made the live data dir private.
func tightenEtcdRestoreCopies(dataDir string) error {
	dir, base := filepath.Dir(dataDir), filepath.Base(dataDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		fmt.Printf("  WARN: could not list %s to make the etcd restore copies in it 0700: %v\n", dir, err)
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || (!strings.HasPrefix(name, base+".pre-restore-") && !strings.HasPrefix(name, base+".failed-restore-")) {
			continue
		}
		if err := os.Chmod(filepath.Join(dir, name), 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// profileSysctlFile is where a profile other than cis has its sysctl set
// written: the sibling of version.SysctlCISConf named for the profile. The
// cis profile's set goes to version.SysctlCISConf itself, which Prereqs
// names so the path's one home stays beside the other boot-critical files
// and uninstall's delete list still covers it.
func profileSysctlFile(p profile.Profile) string {
	return filepath.Join(filepath.Dir(version.SysctlCISConf), "99-stratum-"+p.Name+".conf")
}

// ensurePrivateDir creates d at 0700 and tightens an existing dir that is
// looser (e.g. one created 0755 by an older release).
func ensurePrivateDir(d string) error {
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	return os.Chmod(d, 0o700)
}

// EnsureBPFFS mounts the BPF filesystem at /sys/fs/bpf with shared propagation
// when it is not already there. Cilium's mount-bpf-fs init container requires
// it (systemd hosts mount it at boot; containers and minimal hosts do not).
func EnsureBPFFS() {
	const bpf = "/sys/fs/bpf"
	if b, err := os.ReadFile("/proc/mounts"); err == nil && strings.Contains(string(b), " "+bpf+" bpf ") {
		_ = exec.Command("mount", "--make-shared", bpf).Run()
		return
	}
	_ = os.MkdirAll(bpf, 0o755)
	if err := exec.Command("mount", "-t", "bpf", "bpf", bpf).Run(); err != nil {
		fmt.Printf("  WARN: could not mount bpffs at %s: %v (Cilium needs it)\n", bpf, err)
		return
	}
	_ = exec.Command("mount", "--make-shared", bpf).Run()
}

// procMountinfo is where the kernel describes the mounts and their
// propagation. A var so a test can stand in for the file.
var procMountinfo = "/proc/self/mountinfo"

// runMount runs mount and returns its output with the error, so a failure
// carries mount's own words rather than a bare "exit status 1". A var so a
// test can stand in for the binary.
var runMount = func(args ...string) (string, error) {
	out, err := exec.Command("mount", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// rootPrivate reports whether the / mount is private, as mountinfo describes
// it: a / line is private unless it carries a shared:N or master:N tag (the
// kernel writes each tag as its own optional field before the "-" separator
// that ends them). ok is false when no line describes the / mount, so the
// state cannot be told.
// When several mounts are stacked on /, as in some containers and chroots,
// the one in effect is the one no other / mount is stacked on, so it decides.
// It is told by the mount and parent IDs, the first two fields, because the
// kernel does not promise to list a stacked mount after the one beneath it.
// The root of the mount tree, such as the rootfs of a host booted from its
// initramfs alone, names itself as its parent, which is not a mount on it.
func rootPrivate(mountinfo string) (private, ok bool) {
	type rootMount struct {
		id, parent string
		private    bool
	}
	var roots []rootMount
	for line := range strings.SplitSeq(mountinfo, "\n") {
		fields := strings.Fields(line)
		// fields[4] is the mount point; the optional fields after it end at
		// the "-" separator that starts the filesystem fields.
		if len(fields) < 6 || fields[4] != "/" {
			continue
		}
		r := rootMount{id: fields[0], parent: fields[1], private: true}
		for _, f := range fields[5:] {
			if f == "-" {
				break
			}
			if strings.HasPrefix(f, "shared:") || strings.HasPrefix(f, "master:") {
				r.private = false
			}
		}
		roots = append(roots, r)
	}
	for _, r := range roots {
		if !slices.ContainsFunc(roots, func(o rootMount) bool { return o.parent == r.id && o.id != r.id }) {
			return r.private, true
		}
	}
	return false, false
}

// privateRootNote is why a private / is made shared: containerd refuses the
// host-path mounts Cilium's pods make when / is not a shared or slave mount.
const privateRootNote = "containerd refuses host-path mounts with propagation, such as Cilium's, on a private /"

// EnsureRootShared makes / a recursively shared mount when it is private.
// systemd makes / shared at boot; other inits (BusyBox, OpenRC) do not, and
// on those hosts every mount stays private, so containerd refuses the
// host-to-container bind mounts Cilium's pods make ("path ... is mounted on
// ... but it is not a shared or slave mount") and the node never becomes
// Ready. containerd accepts a shared or a slave mount for those, so a root
// that is already either is left alone; making a slave root shared would also
// cut it off from the mounts it follows. Cilium's one bidirectional mount, on
// /sys/fs/bpf, needs a shared mount, which EnsureBPFFS provides whatever / is.
// The change is recursive, as systemd's is: every mount under / becomes
// shared too, which on these hosts are all private. Nothing here is fatal: if
// the mount fails, Cilium's own error will say why.
func EnsureRootShared() {
	mi, err := os.ReadFile(procMountinfo)
	if err != nil {
		fmt.Printf("  WARN: could not read %s to check whether / is a shared mount: %v\n", procMountinfo, err)
		return
	}
	private, ok := rootPrivate(string(mi))
	if !ok {
		fmt.Printf("  WARN: %s has no / mount; cannot tell whether / is a shared mount\n", procMountinfo)
		return
	}
	if !private {
		return
	}
	out, err := runMount("--make-rshared", "/")
	if err != nil {
		fmt.Printf("  WARN: mount --make-rshared / failed: %v: %s (%s)\n", err, out, privateRootNote)
		return
	}
	fmt.Printf("  made / a shared mount (mount --make-rshared /): %s\n", privateRootNote)
}

// CheckFloor enforces the platform floor before anything starts: cgroup v2 (the
// runtime is cgroup-v2-only) and a kernel new enough for Cilium's kube-proxy
// replacement (>=4.19.57 hard floor, >=5.10 target). Failing early with the
// requirement beats a kubelet and a CNI failing later in ways that do not name
// it.
func CheckFloor() error {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return fmt.Errorf("cgroup v2 is required (unified hierarchy not mounted at /sys/fs/cgroup) — SKE is cgroup-v2-only")
	}
	if rel := kernelRelease(); rel != "" {
		var maj, min int
		if _, err := fmt.Sscanf(rel, "%d.%d", &maj, &min); err == nil {
			if maj < 4 || (maj == 4 && min < 19) {
				return fmt.Errorf("kernel %s is below Cilium's kube-proxy-replacement floor (>=4.19.57; >=5.10 recommended)", rel)
			}
			if maj < 5 || (maj == 5 && min < 10) {
				fmt.Printf("  WARN: kernel %s is below the recommended >=5.10 floor\n", rel)
			}
		}
	}
	return nil
}

// baseSysctls are what pod networking needs, set unconditionally.
const baseSysctls = "net.bridge.bridge-nf-call-iptables=1\nnet.bridge.bridge-nf-call-ip6tables=1\nnet.ipv4.ip_forward=1\nnet.ipv6.conf.all.forwarding=1\n"

// inotifyFloors raise the per-user inotify limits. The kubelet, the control
// plane, every containerd shim and every root-running container share uid 0's
// budget, and the EL default of 128 instances runs out on one hyperconverged
// node with a moderate workload: the pod that needs the 129th fails with "too
// many open files" (136 in use with about 30 pods). The values are the
// ones commonly set for Kubernetes nodes (kind sets the same).
//
// Floors, not settings: 99-stratum.conf sorts late in /etc/sysctl.d, so a fixed
// value would lower a host an operator already tuned higher. The rendered value
// is whichever is larger, the floor or what the kernel has now.
var inotifyFloors = []struct {
	key   string
	floor int
}{
	{"fs.inotify.max_user_instances", 8192},
	{"fs.inotify.max_user_watches", 524288},
}

// procSys is where the live sysctl values are read. A var so a test can point
// it at a fixture.
var procSys = "/proc/sys"

// renderSysctls is 99-stratum.conf: the networking sysctls plus the inotify
// floors, each at the higher of the floor and the running value.
func renderSysctls() string {
	var b strings.Builder
	b.WriteString(baseSysctls)
	for _, f := range inotifyFloors {
		v := f.floor
		if cur, ok := readSysctlInt(f.key); ok && cur > v {
			v = cur
		}
		fmt.Fprintf(&b, "%s=%d\n", f.key, v)
	}
	return b.String()
}

// readSysctlInt reads one integer sysctl from procSys.
func readSysctlInt(key string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(procSys, filepath.FromSlash(strings.ReplaceAll(key, ".", "/"))))
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	return v, err == nil
}

// runSysctl runs sysctl and returns its output with the error, so a failure
// carries sysctl's own words rather than a bare "exit status 1". A var so a
// test can stand in for the binary.
var runSysctl = func(args ...string) (string, error) {
	out, err := exec.Command("sysctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func sysctlError(args []string, out string, err error) error {
	if out == "" {
		return fmt.Errorf("sysctl %s: %w", strings.Join(args, " "), err)
	}
	return fmt.Errorf("sysctl %s: %w: %s", strings.Join(args, " "), err, out)
}

// applySysctls loads the sysctl configuration. `sysctl --system` is the right
// call, since it applies every file in order and an operator's later file
// still wins. BusyBox's sysctl has no --system, though -- the integration
// harness has it, and so do some minimal hosts -- and there nothing stratum
// wrote ever reached the kernel, with only a warning to show for it.
// So when --system is not supported, stratum's own files are loaded
// directly, which both implementations support.
//
// Only then. procps exits non-zero when one key fails (a bridge key before
// br_netfilter loads) having still applied every file in order; loading ours
// again on top would beat an operator's later file.
func applySysctls(profileFile string) error {
	out, err := runSysctl("--system")
	if err == nil {
		return nil
	}
	if !strings.Contains(out, "unrecognized option") {
		return sysctlError([]string{"--system"}, out, err)
	}
	files := []string{version.SysctlConf}
	if profileFile != "" {
		files = append(files, profileFile)
	}
	for _, f := range files {
		if pout, perr := runSysctl("-p", f); perr != nil {
			return fmt.Errorf("this sysctl has no --system, and loading %s directly failed: %w", f, sysctlError([]string{"-p", f}, pout, perr))
		}
	}
	return nil
}

// PrereqOptions are the node settings host preparation depends on.
type PrereqOptions struct {
	// Profile is the node's profile: its sysctl set is written and applied,
	// and its SELinux requirement holds.
	Profile profile.Profile
	// SELinuxOptOut is spec.selinux: false.
	SELinuxOptOut bool
	// Existing is a node that was brought up before, which a missing SELinux
	// policy does not stop (selinux.Options.Existing).
	Existing bool
}

// selinuxOptions are the SELinux settings host preparation runs with: the
// node profile's requirement, under its name, the opt-out, and whether the
// node was brought up before.
func selinuxOptions(o PrereqOptions) selinux.Options {
	return selinux.Options{RequiresSELinux: o.Profile.RequiresSELinux, Profile: o.Profile.Name, OptOut: o.SELinuxOptOut, Existing: o.Existing}
}

// Prereqs registers Stratum's SELinux file contexts and relabels its files,
// loads kernel modules, applies sysctls, and disables swap. It also writes the
// node profile's sysctl set; a profile that declares one depends on it (the
// cis profile's kubelet protectKernelDefaults is a hard boot gate), so a
// sysctl-apply failure is then FATAL rather than an opaque kubelet crash-loop
// later.
func Prereqs(o PrereqOptions) error {
	if err := CheckFloor(); err != nil {
		return err
	}
	if err := selinux.Ensure(selinuxOptions(o)); err != nil {
		return err
	}
	EnsureBPFFS()
	EnsureRootShared()
	for _, m := range []string{"overlay", "br_netfilter"} {
		_ = exec.Command("modprobe", m).Run()
	}
	// Persist the module list: without it br_netfilter/overlay are gone after a
	// reboot and pod networking silently breaks. All three writes here are
	// boot-critical — a torn one breaks pod networking or the kubelet's
	// protectKernelDefaults boot gate after a crash — so each goes through the
	// shared atomic writer (temp file, file fsync, rename, directory fsync),
	// named through the version path variables.
	if err := WriteFileAtomically(version.ModulesLoadFile, []byte("overlay\nbr_netfilter\n"), 0o644); err != nil {
		return err
	}

	if err := WriteFileAtomically(version.SysctlConf, []byte(renderSysctls()), 0o644); err != nil {
		return err
	}
	profileFile := ""
	if len(o.Profile.Sysctls) > 0 {
		profileFile = version.SysctlCISConf
		if o.Profile.Name != profile.CISName {
			profileFile = profileSysctlFile(o.Profile)
		}
		if err := WriteFileAtomically(profileFile, []byte(strings.Join(o.Profile.Sysctls, "\n")+"\n"), 0o644); err != nil {
			return err
		}
	}
	if err := applySysctls(profileFile); err != nil {
		if profileFile != "" {
			return fmt.Errorf("sysctl --system failed under the %s profile: %w (the kubelet may refuse to start until the profile's sysctls apply)", o.Profile.Name, err)
		}
		fmt.Printf("  WARN: sysctl --system failed: %v (ip_forward/bridge-nf may be off — pod networking can break)\n", err)
	}
	_ = exec.Command("swapoff", "-a").Run()
	return nil
}

// Package host performs node prerequisites: dirs, modules, sysctls, swapoff, node IPs.
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

	"github.com/ctrliq/stratum/internal/apparmor"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/version"
)

// Hostname returns the short hostname.
func Hostname() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	h, _, _ = strings.Cut(h, ".")
	return h, nil
}

// NodeIP returns $NODE_IP if set, else the default outbound route's source address.
func NodeIP() (string, error) {
	c, err := resolveNodeIP([]NodeIPChoice{{os.Getenv("NODE_IP"), "$NODE_IP"}}, detectIPv4)
	return c.IP, err
}

var (
	dialUDP        = net.Dial
	interfaceAddrs = net.InterfaceAddrs
)

// The error names no source to pin: each caller's hint names the sources it reads.
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

// Primary returns the first (IPv4-primary) address of a possibly comma-joined node IP.
func Primary(nodeIP string) string {
	if i := strings.IndexByte(nodeIP, ','); i >= 0 {
		return nodeIP[:i]
	}
	return nodeIP
}

// NodeIPs returns kubelet --node-ip's value; dualStack without an IPv6 is an error.
func NodeIPs(dualStack bool) (string, error) {
	c, err := resolveNodeIP([]NodeIPChoice{{os.Getenv("NODE_IP"), "$NODE_IP"}}, func() (string, error) { return DetectNodeIPs(dualStack) })
	return c.IP, err
}

// DetectNodeIPs is NodeIPs without the $NODE_IP override; ResolveNodeIP's last resort.
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

// Like detectIPv4's, its error names no source to pin the address with.
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

// The literals live once in version; uninstall's delete list names them from here.
var (
	ModulesLoadFile = version.ModulesLoadFile
	SysctlConf      = version.SysctlConf
	SysctlCISConf   = version.SysctlCISConf
	CNIDir          = "/etc/cni/net.d"
	// Stratum's kubelet state lives under DataDir, but its serving-cert dir stays fixed.
	KubeletDir = "/var/lib/kubelet"
)

// The paths bring-up writes outside stratum's three roots; uninstall deletes each one.
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
	}, ProfileSysctlFiles()...)
}

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

// The private dirs hold etcd data, private keys and credentialed kubeconfigs.
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

// MkdirAll does not tighten an existing dir, so private modes get an explicit Chmod.
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

// A restore renames the live dir, so a copy made while it was 0755 stays loose.
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

func profileSysctlFile(p profile.Profile) string {
	return filepath.Join(filepath.Dir(version.SysctlCISConf), "99-stratum-"+p.Name+".conf")
}

func ensurePrivateDir(d string) error {
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	return os.Chmod(d, 0o700)
}

// Cilium's mount-bpf-fs init container needs it; systemd hosts only mount it at boot.
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

var procMountinfo = "/proc/self/mountinfo"

var runMount = func(args ...string) (string, error) {
	out, err := exec.Command("mount", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// With stacked mounts on /, the one no other / mount is stacked on decides, by parent ID.
func rootPrivate(mountinfo string) (private, ok bool) {
	type rootMount struct {
		id, parent string
		private    bool
	}
	var roots []rootMount
	for line := range strings.SplitSeq(mountinfo, "\n") {
		fields := strings.Fields(line)
		// fields[4] is the mount point; the optional fields end at the "-" separator.
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

const privateRootNote = "containerd refuses host-path mounts with propagation, such as Cilium's, on a private /"

// Non-systemd inits leave / private; containerd refuses Cilium's mounts on it.
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

// Failing early names the requirement; a failing kubelet or CNI does not.
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

const baseSysctls = "net.bridge.bridge-nf-call-iptables=1\nnet.bridge.bridge-nf-call-ip6tables=1\nnet.ipv4.ip_forward=1\nnet.ipv6.conf.all.forwarding=1\n"

// Floors, not fixed values: 99-stratum.conf sorts late, so a tuned host keeps its value.
var inotifyFloors = []struct {
	key   string
	floor int
}{
	{"fs.inotify.max_user_instances", 8192},
	{"fs.inotify.max_user_watches", 524288},
}

var procSys = "/proc/sys"

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

func readSysctlInt(key string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(procSys, filepath.FromSlash(strings.ReplaceAll(key, ".", "/"))))
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	return v, err == nil
}

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

// BusyBox's sysctl lacks --system; procps failing one key already applied every file.
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
	Profile profile.Profile
	// SELinuxOptOut is spec.selinux: false.
	SELinuxOptOut bool
	// A node brought up before; a missing SELinux policy does not stop it.
	Existing bool
}

func selinuxOptions(o PrereqOptions) selinux.Options {
	return selinux.Options{RequiresSELinux: o.Profile.RequiresSELinux, Profile: o.Profile.Name, OptOut: o.SELinuxOptOut, Existing: o.Existing}
}

// A profile sysctl-apply failure is fatal: the cis kubelet boot gate needs it.
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
	// Without the module list overlay/br_netfilter vanish at reboot, breaking pod networking.
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
	// sysctl --system sets the restriction sysctl that decides runc's profile need.
	if err := apparmor.Ensure(os.Stdout); err != nil {
		return err
	}
	_ = exec.Command("swapoff", "-a").Run()
	return nil
}

// Package staticpod renders the control-plane static pod manifests, and is their sole writer.
package staticpod

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/procid"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// kubelet recreates a static pod whenever its manifest content changes.
const RestartedAtAnnotation = "stratum.ciq.com/restarted-at"

// Dir is the kubelet staticPodPath (prepare.go points kubelet.conf here).
func Dir() string { return filepath.Join(version.K8sConfDir, "manifests") }

// Path is the manifest file for one component.
func Path(name string) string { return filepath.Join(Dir(), name+".yaml") }

// Names is every component rendered as a static pod, in start order.
var Names = []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-vip"}

// the one component rendered only with a VIP, and with no localhost listener.
const kubeVip = "kube-vip"

// a fresh slice per call: callers append to and reorder the roster they are handed.
func NamesFor(st *nodestate.NodeState) []string {
	names := make([]string, 0, len(Names))
	for _, n := range Names {
		if n == kubeVip && st.VIP.Address == "" {
			continue
		}
		names = append(names, n)
	}
	return names
}

// every roster component but kube-vip, whose metrics server is disabled.
func HasListener(name string) bool {
	_, ok := localPort[name]
	return ok
}

// how Active tells a running component from a stopped one without a CRI client.
var localPort = map[string]int{
	"etcd":                    version.EtcdClientPort,
	"kube-apiserver":          version.APIServerPort,
	"kube-controller-manager": 10257,
	"kube-scheduler":          10259,
}

// renders every manifest; an unchanged render is not rewritten, or kubelet restarts the pod.
func WriteAll(st *nodestate.NodeState) error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	roster := NamesFor(st)
	for _, n := range roster {
		if err := write(n, st, currentRestartedAt(n)); err != nil {
			return fmt.Errorf("static pod %s: %w", n, err)
		}
	}
	// a VIP removed from the config takes its manifest away too.
	if !slices.Contains(roster, kubeVip) {
		if err := os.Remove(Path(kubeVip)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// re-renders from the persisted node state, so patched state renders patched flags.
func Start(name string) error {
	st, err := nodestate.LoadState()
	if err != nil {
		return fmt.Errorf("static pod %s: node state: %w", name, err)
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	return write(name, st, currentRestartedAt(name))
}

// returns name's manifest as it is on disk, or nil when there is none.
func Read(name string) ([]byte, error) {
	b, err := os.ReadFile(Path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// writes the manifest as given: a rollback restores manifests without re-rendering them.
func Put(name string, b []byte) error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	return writeFile(name, b)
}

// kubelet parses every visible file here as a pod; only these does a restore replace.
func ManifestName(name string) (string, bool) {
	if !strings.HasSuffix(name, ".yaml") {
		return "", false
	}
	component := strings.TrimSuffix(name, ".yaml")
	return component, slices.Contains(Names, component)
}

// a restore never re-renders the manifests it puts back, so these ride the manifest set.
func derivedFiles() map[string]string {
	return map[string]string{
		filepath.Base(version.AdmissionWebhookConfig()):  version.AdmissionWebhookConfig(),
		filepath.Base(version.WebhookClientKubeconfig()): version.WebhookClientKubeconfig(),
	}
}

// the set lands complete or not at all, staged beside dir and renamed once complete.
func Snapshot(dir string) error {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return err
	}
	// an empty dir is not a snapshot anyone kept, so it moves aside for the rename.
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		held, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(held) == 0 {
			if err := os.Remove(dir); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(filepath.Dir(dir), "."+filepath.Base(dir)+".staging-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging) // no-op once the rename below succeeded
	var firstErr error
	keep := func(src string) {
		if firstErr != nil {
			return
		}
		if err := copyFileTo(staging, src); err != nil {
			firstErr = err
		}
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if _, ok := ManifestName(e.Name()); ok {
			keep(filepath.Join(Dir(), e.Name()))
		}
	}
	for _, live := range derivedFiles() {
		if _, err := os.Stat(live); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		keep(live)
	}
	if firstErr != nil {
		return firstErr
	}
	if err := os.Rename(staging, dir); err != nil {
		return err
	}
	// the rename lives in the parent's entry: until it is synced, a power loss can undo it.
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return fmt.Errorf("%s: %w: %w", dir, atomicfile.ErrNotDurable, err)
	}
	return nil
}

// puts a snapshot's manifest set back where the render keeps it.
func RestoreSnapshot(w io.Writer, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if err := sweepVisibleStaging(w); err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	derived := derivedFiles()
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		if component, ok := ManifestName(e.Name()); ok {
			if err := writeFile(component, data); err != nil {
				return err
			}
			continue
		}
		if live, ok := derived[e.Name()]; ok {
			fi, err := e.Info()
			if err != nil {
				return err
			}
			if err := writeFileAt(live, data, fi.Mode().Perm()); err != nil {
				return err
			}
		}
	}
	return nil
}

// a visible "<name>.yaml.tmp" is parsed by kubelet as a pod; a hidden one is a live write.
func sweepVisibleStaging(w io.Writer) error {
	entries, err := os.ReadDir(Dir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || !strings.HasSuffix(n, ".yaml.tmp") {
			continue
		}
		stale := filepath.Join(Dir(), n)
		// already gone is just something else winning the race.
		if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale staging file %s (kubelet parses it as a pod): %w", stale, err)
		}
		fmt.Fprintf(w, "  removed stale staging file %s (kubelet was parsing it as a pod)\n", stale)
	}
	return nil
}

// copied through the atomic writer, so each file is fsynced at its mode before it lands.
func copyFileTo(dstDir, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	return atomicfile.WriteFileWith(filepath.Join(dstDir, filepath.Base(src)), data, fi.Mode().Perm(), atomicfileSeams())
}

// re-renders with a bumped restarted-at annotation; kubelet recreates the pod.
func Restart(name string) error {
	st, err := nodestate.LoadState()
	if err != nil {
		return fmt.Errorf("static pod %s: node state: %w", name, err)
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	return write(name, st, time.Now().UTC().Format(time.RFC3339Nano))
}

// restarts by bumping the annotation on the manifest as it is on disk, not re-rendering.
func RestartAsIs(name string) error {
	b, err := os.ReadFile(Path(name))
	if err != nil {
		return err
	}
	// Untyped, so a field this binary's Pod type does not know survives.
	var pod map[string]any
	if err := yaml.Unmarshal(b, &pod); err != nil {
		return fmt.Errorf("static pod %s: parse manifest: %w", name, err)
	}
	if pod == nil { // an empty or null document unmarshals without error
		return fmt.Errorf("static pod %s: manifest %s is empty", name, Path(name))
	}
	meta, _ := pod["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		pod["metadata"] = meta
	}
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		meta["annotations"] = ann
	}
	ann[RestartedAtAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	if b, err = yaml.Marshal(pod); err != nil {
		return err
	}
	return writeFile(name, b)
}

// waits so destructive callers, as etcd's data-dir rename, never race a live process.
func Stop(name string, timeout time.Duration) error {
	if err := os.Remove(Path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		gone, err := processGone(name)
		if err != nil {
			return fmt.Errorf("static pod %s process liveness: %w", name, err)
		}
		if !Active(name) && gone {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	if Active(name) {
		return fmt.Errorf("static pod %s still serving on :%d after %v", name, localPort[name], timeout)
	}
	return fmt.Errorf("static pod %s process still present after %v", name, timeout)
}

// unlike Active, it covers a listener already closed during shutdown, and kube-vip.
func Running(name string) (bool, error) {
	gone, err := processGone(name)
	return !gone, err
}

// port-closed alone is shutdown begun, not reaped: the etcd-restore caller renames the data dir.
func processGone(name string) (bool, error) {
	pids, err := PIDs(name)
	return len(pids) == 0, err
}

// a loose argv[0]-basename rule, so an image's entrypoint without a host path is found.
func PIDs(name string) ([]int, error) {
	var pids []int
	err := procid.Walk("/proc", func(p procid.Process) (bool, error) {
		if procid.HasArgv0Basename(name, p) {
			pids = append(pids, p.PID)
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return pids, nil
}

// a point in time on the node's boot clock, the clock process start times use.
type Mark struct{ ticks uint64 }

// the kernel's USER_HZ, the unit of process start times.
const clockTicks = 100

// Now marks the current time on the boot clock.
func Now() (Mark, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return Mark{}, fmt.Errorf("read /proc/uptime: %w", err)
	}
	up, _, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	secs, err := strconv.ParseFloat(up, 64)
	if err != nil {
		return Mark{}, fmt.Errorf("parse /proc/uptime %q: %w", up, err)
	}
	return Mark{ticks: uint64(secs * clockTicks)}, nil
}

// StartTicks is when process pid started, in clock ticks since boot.
func StartTicks(pid int) (uint64, bool, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	// the command name is in parentheses and may hold spaces; the start time is the 22nd field.
	end := strings.LastIndexByte(string(b), ')')
	fields := strings.Fields(string(b)[end+1:])
	if end < 0 || len(fields) < 20 {
		return 0, false, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	t, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse /proc/%d/stat start time: %w", pid, err)
	}
	return t, true, nil
}

// a process started in mark's own tick counts as before it: start times have tick resolution.
func WaitReplaced(name string, mark Mark, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		pids, err := PIDs(name)
		if err != nil {
			return err
		}
		var older []int
		for _, pid := range pids {
			start, alive, err := StartTicks(pid)
			if err != nil {
				return err
			}
			if alive && start <= mark.ticks {
				older = append(older, pid)
			}
		}
		if len(older) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("static pod %s: a process started before its manifest changed (pid %v) still runs after %v",
				name, older, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// the static-pod replacement for comps.UnitExists.
func Exists(name string) bool {
	_, err := os.Stat(Path(name))
	return err == nil
}

// Active reports whether the component's localhost listener is up.
func Active(name string) bool {
	port, ok := localPort[name]
	if !ok {
		return false
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// WaitActive polls until Active or timeout.
func WaitActive(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if Active(name) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("static pod %s not serving on :%d within %v", name, localPort[name], timeout)
}

// an idempotent re-render must not erase the annotation, which would restart the pod.
func currentRestartedAt(name string) string {
	b, err := os.ReadFile(Path(name))
	if err != nil {
		return ""
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal(b, &pod); err != nil {
		return ""
	}
	return pod.Annotations[RestartedAtAnnotation]
}

// the apiserver reads the file only at start, so a changed one must change the manifest.
const AuditPolicyAnnotation = "stratum.ciq.com/audit-policy-sha256"

// the sha256 of p's plain config files, in order; "" for a profile with none.
func configFilesSum(p profile.Profile) (string, error) {
	h := sha256.New()
	n := 0
	for _, f := range p.ConfigFiles {
		if f.Kind != profile.PlainFile {
			continue
		}
		b, err := os.ReadFile(version.ProfileConfigFile(f.Name))
		if err != nil {
			return "", fmt.Errorf("read the %s profile's %s: %w", p.Name, f.Name, err)
		}
		h.Write(b)
		n++
	}
	if n == 0 {
		return "", nil
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// a separate annotation, so AuditPolicyAnnotation keeps the value it had.
const AdmissionConfigAnnotation = "stratum.ciq.com/admission-config-sha256"

// the sha256 of p's AdmissionConfiguration file; "" for a profile with none.
func admissionConfigSum(p profile.Profile) (string, error) {
	f, ok := p.AdmissionFile()
	if !ok {
		return "", nil
	}
	b, err := os.ReadFile(version.ProfileConfigFile(f.Name))
	if err != nil {
		return "", fmt.Errorf("read the %s profile's %s: %w", p.Name, f.Name, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func write(name string, st *nodestate.NodeState, restartedAt string) error {
	var p profile.Profile
	if name == "kube-apiserver" {
		var err error
		if p, err = profile.ForNode(st.ProfileName()); err != nil {
			return fmt.Errorf("render kube-apiserver: %w", err)
		}
		if err := writeWebhookAuth(p); err != nil {
			return err
		}
	}
	pod, err := renderWith(name, st, p)
	if err != nil {
		return err
	}
	if name == "kube-apiserver" {
		for _, a := range []struct {
			name string
			sum  func(profile.Profile) (string, error)
		}{
			{AuditPolicyAnnotation, configFilesSum},
			{AdmissionConfigAnnotation, admissionConfigSum},
		} {
			v, err := a.sum(p)
			if err != nil {
				return err
			}
			if v != "" {
				if pod.Annotations == nil {
					pod.Annotations = map[string]string{}
				}
				pod.Annotations[a.name] = v
			}
		}
	}
	if restartedAt != "" {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[RestartedAtAnnotation] = restartedAt
	}
	b, err := yaml.Marshal(pod)
	if err != nil {
		return err
	}
	return writeFile(name, b)
}

// writeFile replaces name's manifest with b, unless it already holds b.
func writeFile(name string, b []byte) error {
	return writeFileAt(Path(name), b, 0o600)
}

// kubelet parses every non-hidden file in the manifest directory.
func writeFileAt(path string, b []byte, perm os.FileMode) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm() == perm.Perm() {
		if cur, err := os.ReadFile(path); err == nil && string(cur) == string(b) {
			return nil
		}
	}
	return atomicfile.WriteFile(path, b, perm)
}

func render(name string, st *nodestate.NodeState) (*corev1.Pod, error) {
	var p profile.Profile
	if name == "kube-apiserver" {
		var err error
		if p, err = profile.ForNode(st.ProfileName()); err != nil {
			return nil, fmt.Errorf("render kube-apiserver: %w", err)
		}
	}
	return renderWith(name, st, p)
}

// renderWith is render with the node's profile already resolved.
func renderWith(name string, st *nodestate.NodeState, p profile.Profile) (*corev1.Pod, error) {
	pki := func(f string) string { return filepath.Join(version.PKIDir, f) }
	kc := func(f string) string { return filepath.Join(version.KubeconfigDir, f) }

	var c corev1.Container
	var volumes []corev1.Volume

	mount := func(vname, path string, readOnly bool) {
		volumes = append(volumes, corev1.Volume{Name: vname, VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: path, Type: hostPathType(corev1.HostPathDirectoryOrCreate)},
		}})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: vname, MountPath: path, ReadOnly: readOnly})
	}

	switch name {
	case "etcd":
		c = corev1.Container{
			Name:          "etcd",
			Image:         version.EtcdImage,
			Command:       append([]string{"etcd"}, etcdArgs(st)...),
			LivenessProbe: probe(corev1.URISchemeHTTP, version.EtcdMetricsPort, "/health"),
		}
		mount("pki", version.PKIDir, true)
		mount("data", version.EtcdDataDir, false)

	case "kube-apiserver":
		c = corev1.Container{
			Name:           "kube-apiserver",
			Image:          version.KubeAPIServerImage,
			Command:        append([]string{"kube-apiserver"}, apiserverArgs(st, p, pki)...),
			LivenessProbe:  probe(corev1.URISchemeHTTPS, version.APIServerPort, "/livez"),
			ReadinessProbe: probe(corev1.URISchemeHTTPS, version.APIServerPort, "/readyz"),
		}
		mount("pki", version.PKIDir, true)
		mount("config", version.K8sConfDir, true) // audit/admission configs under --cis
		mount("audit-log", version.AuditLogDir, false)
		mount("etc-ssl", "/etc/ssl/certs", true) // webhook/OIDC egress trust

	case "kube-controller-manager":
		c = corev1.Container{
			Name:          "kube-controller-manager",
			Image:         version.KubeControllerManagerImage,
			Command:       append([]string{"kube-controller-manager"}, cmArgs(st, pki, kc)...),
			LivenessProbe: probe(corev1.URISchemeHTTPS, localPort["kube-controller-manager"], "/healthz"),
		}
		mount("pki", version.PKIDir, true)
		mount("kubeconfig", version.KubeconfigDir, true)
		mount("etc-ssl", "/etc/ssl/certs", true)

	case "kube-scheduler":
		c = corev1.Container{
			Name:          "kube-scheduler",
			Image:         version.KubeSchedulerImage,
			Command:       append([]string{"kube-scheduler"}, schedulerArgs()...),
			LivenessProbe: probe(corev1.URISchemeHTTPS, localPort["kube-scheduler"], "/healthz"),
		}
		mount("kubeconfig", filepath.Dir(version.SchedulerKubeconfig()), true)

	case "kube-vip":
		if st.VIP.Address == "" {
			return nil, fmt.Errorf("kube-vip: node state has no VIP")
		}
		c = corev1.Container{
			Name:  "kube-vip",
			Image: version.KubeVipImage,
			// The image's entrypoint is the kube-vip binary; args only.
			Args: kubeVipArgs(st),
			SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"},
			}},
		}
		mount("kubeconfig", filepath.Dir(version.KubeVipKubeconfig()), true)

	default:
		return nil, fmt.Errorf("unknown static pod component %q", name)
	}

	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kube-system",
			Labels:    map[string]string{"component": name, "tier": "control-plane"},
		},
		Spec: corev1.PodSpec{
			HostNetwork:       true,
			PriorityClassName: "system-node-critical",
			SecurityContext:   &corev1.PodSecurityContext{SELinuxOptions: &corev1.SELinuxOptions{Type: "spc_t"}},
			Containers:        []corev1.Container{c},
			Volumes:           volumes,
		},
	}, nil
}

func hostPathType(t corev1.HostPathType) *corev1.HostPathType { return &t }

func probe(scheme corev1.URIScheme, port int, path string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Host: "127.0.0.1", Scheme: scheme, Port: intstr.FromInt32(int32(port)), Path: path,
		}},
		InitialDelaySeconds: 10, TimeoutSeconds: 15, PeriodSeconds: 10, FailureThreshold: 8,
	}
}

// a component's exec definition read back from the manifest kubelet actually reads.
type ContainerSpec struct {
	// empty means the image's entrypoint is unknown to this reader.
	Command []string
	// Command followed by Args; without Command it starts with Args.
	Argv []string
	// the declared environment, without the image's own ENV layer.
	Env map[string]string
}

// reads the file, not a re-render: an audit reports what kubelet was handed.
func ReadContainerSpec(name string) (ContainerSpec, error) {
	b, err := os.ReadFile(Path(name))
	if err != nil {
		return ContainerSpec{}, err
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal(b, &pod); err != nil {
		return ContainerSpec{}, fmt.Errorf("static pod %s: parse %s: %w", name, Path(name), err)
	}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != name {
			continue
		}
		spec := ContainerSpec{
			Command: append([]string{}, c.Command...),
			Argv:    append(append([]string{}, c.Command...), c.Args...),
			Env:     make(map[string]string, len(c.Env)),
		}
		for _, e := range c.Env {
			// valueFrom defers the value elsewhere, so the manifest is not the whole environment.
			if e.ValueFrom != nil {
				return ContainerSpec{}, fmt.Errorf("static pod %s: environment variable %s takes its value from elsewhere (valueFrom), "+
					"so this manifest is not the container's full environment", name, e.Name)
			}
			spec.Env[e.Name] = e.Value
		}
		// envFrom would be a second, invisible environment source.
		if len(c.EnvFrom) > 0 {
			return ContainerSpec{}, fmt.Errorf("static pod %s: container declares envFrom, so its environment cannot be read from this manifest alone", name)
		}
		return spec, nil
	}
	return ContainerSpec{}, fmt.Errorf("static pod %s: manifest declares no container named %q", name, name)
}

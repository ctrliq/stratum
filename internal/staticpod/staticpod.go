// Package staticpod renders and manages the control-plane static pod
// manifests (the control plane runs as kubelet-managed static pods).
//
// stratum is the SOLE writer of the manifest directory: every manifest is
// re-rendered idempotently from the persisted node state on controller start,
// so hand-edits are overwritten by design and the rendered files stand as
// the on-disk truth.
//
// The systemctl-shaped verbs the host-process model needed are replaced by
// three file operations kubelet reacts to:
//
//	Restart — re-render with a bumped restarted-at annotation (content change
//	          makes kubelet recreate the pod)
//	Stop    — remove the manifest and wait for the component to go down
//	Start   — re-render the manifest from the current node state
package staticpod

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// RestartedAtAnnotation is bumped by Restart; kubelet recreates a static pod
// whenever its manifest content changes, and the annotation is the one field
// whose only job is to change.
const RestartedAtAnnotation = "stratum.ciq.com/restarted-at"

// Dir is the kubelet staticPodPath (prepare.go points kubelet.conf here).
func Dir() string { return filepath.Join(version.K8sConfDir, "manifests") }

// Path is the manifest file for one component.
func Path(name string) string { return filepath.Join(Dir(), name+".yaml") }

// Names is every component rendered as a static pod, in start order.
var Names = []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-vip"}

// kubeVip is the one roster component that renders only where the node state
// carries a VIP, and the one with no localhost listener.
const kubeVip = "kube-vip"

// NamesFor is the roster this node state renders: Names in start order, with
// kube-vip in it exactly when the state carries a VIP. It is the one place the
// kube-vip-only-with-VIP rule lives, so every roster derived from it — the
// upgrade's re-render order, the restore's restart set — cannot restate the
// rule beside it. A fresh slice per call: callers append to and reorder the
// roster they are handed, and one that aliased Names would corrupt it for
// every other reader.
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

// HasListener reports whether the component owns a localhost listener a caller
// can wait on (the checks Active polls): every roster component but kube-vip,
// whose metrics server is disabled, and no name outside the roster. It is the
// listenerless rule localPort already encodes, so a second static pod without
// a listener needs only its localPort entry left out.
func HasListener(name string) bool {
	_, ok := localPort[name]
	return ok
}

// localPort is the localhost listener each component owns — how Active tells a
// running component from a stopped one without a CRI client. kube-vip has no
// listener (its metrics server is disabled), so it has no Active check; nothing
// needs one.
var localPort = map[string]int{
	"etcd":                    version.EtcdClientPort,
	"kube-apiserver":          version.APIServerPort,
	"kube-controller-manager": 10257,
	"kube-scheduler":          10259,
}

// WriteAll renders every control-plane manifest this node needs. Idempotent:
// an unchanged render is not rewritten (kubelet would restart the pod), and a
// restarted-at annotation left by a previous Restart is preserved so a
// controller restart does not bounce every pod.
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
	// Sole-writer discipline cuts both ways: a VIP removed from the config must
	// take its manifest (and so its pod) away too.
	if !slices.Contains(roster, kubeVip) {
		if err := os.Remove(Path(kubeVip)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Start (re)renders one component's manifest from the persisted node state —
// the static-pod equivalent of the supervisor's ArgsFn semantics: `stratum
// snapshot rejoin/restore` patches the state, and Start renders the patched
// membership flags.
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

// Read returns name's manifest exactly as it is on disk, or nil when there is
// none.
func Read(name string) ([]byte, error) {
	b, err := os.ReadFile(Path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// Put writes b as name's manifest, exactly as given. It is for putting back a
// manifest read earlier, where re-rendering would take its images from this
// binary rather than from whatever was running (an upgrade rollback runs the
// newer binary against the older manifests).
func Put(name string, b []byte) error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	return writeFile(name, b)
}

// ManifestName reports whether a file name in the manifest directory — or in a
// snapshot of it — is one of the rendered component manifests ("<name>.yaml"),
// and which component. It is false for a render-derived file, a staging
// leftover, and anything else: kubelet parses every visible file in the
// directory as a pod, but only these are ones this package renders, and only
// these does a manifest-set restore replace. Exported for the upgrade's
// readers of a manifest snapshot, which must count exactly the components a
// restore replaces — a webhook config file is not a pod kubelet runs.
func ManifestName(name string) (string, bool) {
	if !strings.HasSuffix(name, ".yaml") {
		return "", false
	}
	component := strings.TrimSuffix(name, ".yaml")
	return component, slices.Contains(Names, component)
}

// derivedFiles are the files the render writes OUTSIDE the manifest directory
// whenever it writes the apiserver manifest: the admission configuration the
// apiserver's argv names, and the webhook client kubeconfig that
// configuration's plugin names. The manifest set carries them because a
// restore never re-renders the manifests it puts back — a rollback runs its
// restored apiserver as it was, so nothing else puts its own admission chain
// back. Keyed by file name, with the live path the render writes each at.
func derivedFiles() map[string]string {
	return map[string]string{
		filepath.Base(version.AdmissionWebhookConfig()):  version.AdmissionWebhookConfig(),
		filepath.Base(version.WebhookClientKubeconfig()): version.WebhookClientKubeconfig(),
	}
}

// Snapshot copies the manifest set into dir, which it creates: every component
// manifest in the manifest directory, plus the render-derived files. Staging
// leftovers and anything else in the directory stay out — a file that joined a
// snapshot would be restored as a pod on every later restore. The set lands
// complete or not at all: it is staged under a hidden name beside dir and
// renamed into place only once every copy succeeded, so a snapshot that exists
// is one a restore can put back whole. A caller that keeps an existing
// snapshot for its own resume semantics (the upgrade's) must check before
// calling: a snapshot already at dir is not retaken.
func Snapshot(dir string) error {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return err
	}
	// An empty directory at dir is not a snapshot anyone kept — a snapshot
	// always holds entries — so it moves aside for the rename below, which
	// would otherwise fail with EEXIST on a fresh (empty) target. A non-empty
	// one is left alone and fails the rename: a caller that keeps an existing
	// snapshot for its own resume semantics must not have it retaken under it.
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
				continue // the render has not written this one yet
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
	// The rename that turns the staging copy into the snapshot lives in the
	// parent directory's entry: until that directory is synced, a power loss
	// can undo the rename after this function has returned, and the upgrade
	// would go on believing it holds a backup it no longer has.
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return fmt.Errorf("%s: %w: %w", dir, atomicfile.ErrNotDurable, err)
	}
	return nil
}

// RestoreSnapshot puts a snapshot's manifest set back where the render keeps
// it: every manifest through the package's one staging path into the manifest
// directory, every render-derived file at the live path the render writes it,
// with the mode the snapshot holds. Anything else in the snapshot — a stray
// file, a leftover — lands nowhere: kubelet parses every non-hidden file in
// the manifest directory as a pod, and the derived files belong in the
// directory the apiserver's argv names. Before anything moves, the visible
// staging files the pre-atomic writer left in the live manifest directory are
// swept: kubelet parses them as pods, and they sort AFTER the real manifest in
// the file source's glob, so they hold that pod's store key permanently.
func RestoreSnapshot(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if err := sweepVisibleStaging(); err != nil {
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

// sweepVisibleStaging removes the visible "<name>.yaml.tmp" files the
// pre-atomic writer could leave in the manifest directory, where kubelet
// parses them as pods. A hidden temp is a live write by a concurrent Start —
// removing it would fail that writer's rename with ENOENT — and is never ours
// to delete.
func sweepVisibleStaging() error {
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
		// A failure here is a failed restore, not a warning: leaving the ghost
		// in place while reporting success is how the honesty contract breaks
		// (the caller reports the restore failed). Already gone is fine — that
		// is just something else winning the race.
		if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale staging file %s (kubelet parses it as a pod): %w", stale, err)
		}
		fmt.Printf("  removed stale staging file %s (kubelet was parsing it as a pod)\n", stale)
	}
	return nil
}

// copyFileTo copies src into dstDir under its base name, with the mode the
// node had: a restored file keeps its mode, not the writer's default. A
// missing source is the caller's decision to make, not this one's. The copy
// goes through the shared atomic writer with this package's seams, so each
// staged file is fsynced at its final mode before it lands and a test can
// fail the sync.
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

// Restart re-renders with a bumped restarted-at annotation; kubelet recreates
// the pod on the content change.
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

// RestartAsIs restarts a component whose manifest must not change: it bumps the
// restarted-at annotation on the manifest as it is on disk, where Restart
// re-renders it and so takes its images from this binary. An upgrade rollback
// runs the newer binary against the older manifests it has just restored, and
// re-rendering those would put the newer images back. It reports an
// error for a component with no manifest.
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

// Stop removes the manifest and waits until the component's process is gone,
// so callers that are about to do something destructive (move etcd's data dir
// aside) know the old process is no longer serving or retaining host state.
// kube-vip has no listener, but its process must still exit before teardown.
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

// Running reports whether a static-pod component process is still present on
// this node. Unlike Active, it covers a process that has already closed its
// listener while it finishes shutting down, and kube-vip which has no listener.
// An error means the process state is unknown.
func Running(name string) (bool, error) {
	gone, err := processGone(name)
	return !gone, err
}

// processGone reports that no process on this node has the component as its
// argv[0]. Port-closed alone is "shutdown begun", not "process reaped" — and
// the etcd-restore caller renames the data dir the moment Stop returns, which
// must never happen under a still-live etcd. /proc is the cheapest authority
// that does not need a CRI client. An unreadable process table is unknown, not
// proof that the process is gone.
func processGone(name string) (bool, error) {
	pids, err := PIDs(name)
	return len(pids) == 0, err
}

// PIDs lists the processes on this node running the component: procid's walk
// with the static-pod scanners' deliberately loose argv[0]-basename rule, so
// an image's entrypoint with no staged host path is still found. An unreadable
// process table is unknown, not proof that the process is gone.
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

// Mark is a point in time on this node's boot clock, the clock process start
// times are kept in.
type Mark struct{ ticks uint64 }

// clockTicks is the kernel's USER_HZ, the unit of process start times, which
// Linux fixes at 100 for user space on every architecture.
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
	// The command name is in parentheses and may hold spaces; the fields
	// after it start at the third, and the start time is the 22nd.
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

// WaitReplaced waits until every process running the component started after
// mark: once a changed manifest is written, until kubelet has taken down
// whatever ran before it. A process that started in mark's own tick counts as
// started before it: kubelet takes far longer than a tick to start a pod from
// a manifest written after the mark. A check that the component answers, made before
// then, can be answered by a process from the manifest it replaced, however
// many times that one was restarted in between.
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

// Exists reports whether this node renders the component at all — the
// static-pod replacement for comps.UnitExists ("is this a control plane?").
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

// currentRestartedAt reads the annotation off the existing manifest so an
// idempotent re-render does not erase the last Restart (erasing it would be a
// content change, i.e. an unintended pod restart on every controller start).
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

// AuditPolicyAnnotation carries the sha256 of the node profile's plain
// config files, in order, on the kube-apiserver pod: for the cis profile,
// its audit policy. The apiserver reads such a file only when it starts, and
// its manifest names the file without its contents, so a changed file would
// otherwise leave the running apiserver on the old one.
const AuditPolicyAnnotation = "stratum.ciq.com/audit-policy-sha256"

// configFilesSum is the sha256 of p's plain config files, read from where the
// engine wrote them, in order; "" for a profile that declares none.
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

// AdmissionConfigAnnotation carries the sha256 of the node profile's
// AdmissionConfiguration file on the kube-apiserver pod. The apiserver reads
// the admission-webhook.yaml merged from it only when it starts, and its
// manifest names that file without its contents, so a changed admission body
// would otherwise leave the running apiserver on the old policy. It is an
// annotation of its own so AuditPolicyAnnotation keeps the value it had.
const AdmissionConfigAnnotation = "stratum.ciq.com/admission-config-sha256"

// admissionConfigSum is the sha256 of p's AdmissionConfiguration file, read
// from where the engine wrote it; "" for a profile that declares none.
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

// writeFileAt replaces the file at path with b through the package's one
// staging path — atomicfile.WriteFile — unless it is a regular file that
// already holds b with the requested permissions. The staging
// name is dot-prefixed because kubelet's file source skips hidden files but
// parses EVERYTHING else in the manifest directory regardless of extension: a
// visible temp would be read mid-write, and a torn manifest is unparseable,
// silently rejected on every re-read, and the pod keeps running the old spec
// (found by check-upgrade in CI: a process killed mid-write left exactly
// that). It is unique per write because the fixed ".<name>.yaml.tmp" it
// replaced was one path shared by every writer of the file — a rollback's Put
// and a concurrent Start both staging there tear one manifest, one truncating
// the file the other is about to rename into place.
func writeFileAt(path string, b []byte, perm os.FileMode) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm() == perm.Perm() {
		if cur, err := os.ReadFile(path); err == nil && string(cur) == string(b) {
			return nil
		}
	}
	return atomicfile.WriteFile(path, b, perm)
}

// render builds the typed pod spec for one component. Shape is the conventional
// static-pod layout: hostNetwork, system-node-critical, hostPath mounts, a
// liveness probe on the component's local health endpoint.
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

// renderWith is render with the node's profile already resolved; only the
// apiserver reads it.
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

// ContainerSpec is the rendered exec definition of a component, read back from
// the manifest kubelet actually reads.
//
// It exists so an auditor can see a component's ARGV and its ENVIRONMENT
// together, from one artifact. For a kubelet-managed static pod those really
// are the whole story: a container's environment is the image's ENV plus the
// pod spec's env, and the kubelet does not leak its own environ into it. So for
// any component rendered here, this file is the environment — which is what
// makes an audit of etcd's boolean TLS flags decidable rather than an
// inference about etcd's defaults.
type ContainerSpec struct {
	// Command is the explicit command in the manifest. If empty, the image's
	// entrypoint is unknown to this reader.
	Command []string
	// Argv is Command followed by Args. Without Command it starts with Args,
	// not the image entrypoint.
	Argv []string
	// Env is the container's declared environment. It does NOT include the
	// image's own ENV layer, which is not visible from the manifest; that is
	// pinned by digest in build/images.txt and audited at build time.
	Env map[string]string
}

// ReadContainerSpec parses the rendered manifest for a component and returns its
// explicit command, combined argv and declared environment.
//
// Deliberately reads the FILE rather than re-rendering from node state: a
// re-render would report what stratum intends, and the point of an audit is to
// report what kubelet was handed. An out-of-band edit is therefore visible here,
// which is exactly what a CIS check over these flags is for.
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
			// ValueFrom defers the value to a ConfigMap key, a Secret key or a
			// fieldRef, so the manifest names the variable without stating what
			// it resolves to. This reader's whole contract — the one the CIS
			// etcd controls rest on — is that the rendered manifest IS the
			// container's environment, and an entry like this breaks it.
			//
			// Refusing rather than recording it as empty: the empty form happens
			// to fail those controls today, because "" is not a boolean, but
			// that is an accident of ParseBool rather than a decision, and it
			// reports "ETCD_AUTO_TLS=\"\" is not a boolean" when the truth is
			// that the audit cannot see the value at all.
			if e.ValueFrom != nil {
				return ContainerSpec{}, fmt.Errorf("static pod %s: environment variable %s takes its value from elsewhere (valueFrom), "+
					"so this manifest is not the container's full environment", name, e.Name)
			}
			spec.Env[e.Name] = e.Value
		}
		// EnvFrom would be a second, invisible source of environment. Nothing
		// renders one today; report rather than silently audit a partial view.
		if len(c.EnvFrom) > 0 {
			return ContainerSpec{}, fmt.Errorf("static pod %s: container declares envFrom, so its environment cannot be read from this manifest alone", name)
		}
		return spec, nil
	}
	return ContainerSpec{}, fmt.Errorf("static pod %s: manifest declares no container named %q", name, name)
}

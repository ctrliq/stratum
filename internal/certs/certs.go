// Package certs orchestrates control-plane certificate rotation on a node:
// inspect expiry (check), and force-reissue the leaf certs from the existing CA
// + rewrite the kubeconfigs + restart the control-plane units with health gates
// (rotate). Leaf-only — it never touches the root/front-proxy CAs or the SA
// signing keypair (those are the heavier cluster-wide "CA rotation" flow).
package certs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/certinputs"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
	"sigs.k8s.io/yaml"
)

// The restart sequence after a rotation: static pods first (etcd and apiserver,
// then controller-manager and scheduler, health-gated), then host processes that
// authenticate with material rotation just rewrote, derived from comps.Units
// beside this line. operator is last because it uses super-admin.conf.
// Components absent on this node are skipped.
var (
	podRestartOrder  = staticpod.Names // already in start order
	hostRestartOrder = hostRestartOrderFrom(comps.Units)
)

// hostRestartOrderFrom filters a component roster down to the rotation's host
// processes: every unit that authenticates with the material the rotation
// rewrites — every unit but containerd, which holds no certificates — in
// roster order, so kubelet restarts first and the operator, which reads
// super-admin.conf, last.
func hostRestartOrderFrom(units []string) []string {
	var out []string
	for _, u := range units {
		if u == "containerd" {
			continue
		}
		out = append(out, u)
	}
	return out
}

// RenewalRecord is the scheduled renewal loop's LAST attempt on this node,
// persisted under the node's state area so `stratum certs check` can show a
// dead or failing loop before the leaves expire — nothing else on the node
// says when renewal last ran or how it went.
type RenewalRecord struct {
	// AttemptedAt is when the loop started its last rotation attempt.
	AttemptedAt time.Time `json:"attemptedAt"`
	// Result is that attempt's rotation error, empty when it succeeded.
	Result string `json:"result,omitempty"`
}

// RenewalRecordPath is where the renewal record persists, beside node.yaml in
// the node's state area.
func RenewalRecordPath() string { return filepath.Join(version.K8sConfDir, "renewal.yaml") }

// incompleteRotationPath marks a leaf rotation that reissued leaves but has not
// yet rewritten every kubeconfig and restarted every component onto them. The
// fresh leaves on disk then say nothing about what the running control plane
// uses, so while the marker exists the timer rotates again.
func incompleteRotationPath() string {
	return filepath.Join(version.K8sConfDir, "rotation-incomplete")
}

// incompleteRotationExpired is the marker content of a rotation that began from
// expired leaves: its control plane may still run on them after the leaves on
// disk are fresh, so its retry keeps the expired-leaf path to the node lock.
const incompleteRotationExpired = "expired"

// IncompleteRotation reports whether a leaf rotation reissued leaves but has
// not yet restarted every component onto them, and whether that rotation began
// from expired leaves. A marker that cannot be read still counts as present.
func IncompleteRotation() (present, expired bool) {
	if _, err := os.Lstat(incompleteRotationPath()); errors.Is(err, fs.ErrNotExist) {
		return false, false
	}
	b, _ := os.ReadFile(incompleteRotationPath())
	return true, strings.TrimSpace(string(b)) == incompleteRotationExpired
}

// SaveRenewalRecord persists the renewal record atomically through the shared
// atomic writer (internal/atomicfile): a failed or crashed save leaves either
// the old record or the new one, never a truncated one and never a stray temp
// copy beside it.
func SaveRenewalRecord(r RenewalRecord) error {
	b, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(RenewalRecordPath()), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(RenewalRecordPath(), b, 0o600)
}

// LoadRenewalRecord reads the persisted renewal record. A record that does not
// exist is reported as an error wrapping fs.ErrNotExist, which callers read as
// no attempt recorded yet.
func LoadRenewalRecord() (RenewalRecord, error) {
	b, err := os.ReadFile(RenewalRecordPath())
	if err != nil {
		return RenewalRecord{}, err
	}
	var r RenewalRecord
	if err := yaml.Unmarshal(b, &r); err != nil {
		return RenewalRecord{}, fmt.Errorf("%s: %w", RenewalRecordPath(), err)
	}
	return r, nil
}

// CertStatus is one row of `stratum certs check`.
type CertStatus struct {
	Name           string
	NotAfter       time.Time
	DaysLeft       int
	IsCA           bool
	Expiring       bool // within the warn window
	Rotatable      bool // a leaf we can rotate (false for CAs)
	Kubelet        bool // the kubelet's CSR-issued client cert, which the kubelet renews itself
	RenewalOverdue bool // the kubelet client cert is past its latest expected renewal point
}

// KubeletClientName is the `certs check` row for a worker's CSR-issued kubelet
// client cert.
const KubeletClientName = "kubelet-client"

// Options for RotateNode.
type Options struct {
	IfExpiringDays int  // >0: no-op unless a leaf is within this many days of expiry (timer path). 0 = force.
	WarnDays       int  // window for the "expiring" flag in Check (default version.CertRenewWindowDays)
	Quiet          bool // suppress the per-step step logs (timer path)

	// NodeLockHeld is the held-lock capability: the caller already holds the
	// node maintenance lock (internal/nodelock) for the whole call and Rotate
	// must not reacquire it — a second Acquire from the same process refuses
	// against its own fence, because flock conflicts between open file
	// descriptions. The caller releases the lock on its own exits and must
	// also hold the cluster fence when a renewal is needed.
	NodeLockHeld bool

	// KubeVipRequired fails the rotation when kube-vip cannot be restarted,
	// or when kubelet has not replaced the process that ran before the
	// restart within kubeVipReplaceWait, where otherwise that restart is
	// best-effort: a caller moving kube-vip off a credential it must no
	// longer hold cannot count a rotation that left the running kube-vip on
	// it as done.
	KubeVipRequired bool
}

// warnDays is the window behind Check's "expiring" flag. A caller's positive
// WarnDays wins; the default is version.CertRenewWindowDays, the window the
// auto-renew timer rotates on, so check warns inside the same window the
// timer renews in.
func warnDays(o Options) int {
	if o.WarnDays > 0 {
		return o.WarnDays
	}
	return version.CertRenewWindowDays
}

// Check reports expiry for managed certificates and the kubelet client cert.
// Any present managed or kubelet cert that cannot be read or parsed is an error
// for operators to address; absent ones are skipped.
func Check(o Options) ([]CertStatus, error) {
	return check(o, true)
}

// Rotation only needs managed leaves and CAs. An unrelated kubelet client
// certificate must not prevent control-plane leaf renewal.
func check(o Options, includeKubelet bool) ([]CertStatus, error) {
	hostname, nodeIP, _, _, err := hostNodeIP()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	status := func(name string, na time.Time) CertStatus {
		// Floor negative durations too: a certificate that expired an hour
		// ago must report -1, not zero days left.
		days := int(math.Floor(na.Sub(now).Hours() / 24))
		return CertStatus{Name: name, NotAfter: na, DaysLeft: days, Expiring: days <= warnDays(o)}
	}
	mk := func(name string, isCA, rotatable, reparable bool) (CertStatus, bool, error) {
		na, err := pki.CertNotAfter(name)
		if err != nil {
			// Only an absent file is skipped (e.g. a worker has no managed
			// leaves). One that is present but unreadable or unparsable is disk
			// damage or tampering, so it is an error a human sees first, not a
			// cert to overwrite or skip silently.
			if !errors.Is(err, fs.ErrNotExist) {
				p := filepath.Join(version.PKIDir, name+".crt")
				err = fmt.Errorf("certificate %s is present but cannot be read or parsed: %w", p, err)
				if rotatable && reparable {
					// A leaf is repairable by forced rotation only where the
					// signer reissues it without reading the damaged file; a CA
					// is never repairable this way. Offering the remedy where
					// rotate fails anyway would send the operator in a circle.
					const rotate = "`sudo /usr/local/bin/stratum certs rotate --yes`"
					if st, serr := os.Lstat(p); serr == nil && st.IsDir() {
						// The reissued file replaces the old one by rename, which
						// cannot replace a directory, so it has to move first.
						err = fmt.Errorf("%w; it is a directory: move it aside with `sudo mv %s %s`, then reissue it with %s",
							err, shellQuote(p), shellQuote(p+".damaged"), rotate)
					} else {
						err = fmt.Errorf("%w; reissue it with %s", err, rotate)
					}
				}
				return CertStatus{}, false, err
			}
			return CertStatus{}, false, nil
		}
		s := status(name, na)
		s.IsCA, s.Rotatable = isCA, rotatable
		return s, true, nil
	}
	var out []CertStatus
	// The cluster root, front-proxy, etcd, and kubelet-client CAs are long-lived
	// anchors that do not auto-renew. Include each in the expiry table; mk skips
	// only an absent CA (pre-migration nodes and workers) and fails on a
	// present-but-damaged one.
	for _, ca := range []string{"ca", "front-proxy-ca", pki.EtcdCAName, pki.KubeletClientCAName} {
		s, ok, err := mk(ca, true, false, false)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, s)
		}
	}
	_, etcdSpecs := pki.SplitLeafSpecs(hostname, nodeIP, nil)
	for _, name := range pki.LeafNames(hostname, nodeIP) {
		s, ok, err := mk(name, false, true, rotationRepairs(etcdSpecs, name))
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, s)
		}
	}
	if !includeKubelet {
		return out, nil
	}
	p := kubeletClientCertPath()
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	leaf, err := readKubeletClientLeaf(p)
	if err != nil {
		return nil, err
	}
	s := status(KubeletClientName, leaf.NotAfter)
	s.Kubelet = true
	s.RenewalOverdue = leaf.NotAfter.Sub(now) <= leaf.NotAfter.Sub(leaf.NotBefore)/10
	return append(out, s), nil
}

// rotationRepairs reports whether a forced rotation can reissue a damaged
// leaf in the node's current state, under the same conditions its signer
// reissues it. RotateLeaves signs each leaf with the CA that signs it today,
// and that signer reads the live leaf whenever the etcd CA exists or the
// kubelet client trust is not split — a leaf that cannot be read fails there
// with no reissue, so the rotate remedy would be false advice.
func rotationRepairs(etcdSpecs []pki.CertSpec, name string) bool {
	for _, sp := range etcdSpecs {
		if sp.Name == name {
			// etcdLeafSigner reads the live leaf to keep its trust anchor once the
			// etcd CA exists; only a pre-migration node (no etcd CA) re-signs the
			// etcd leaves with the root without reading them.
			return !pki.EtcdCAExists()
		}
	}
	if name == "apiserver-kubelet-client" {
		// kubeletClientReissuer reads the live leaf to tell which CA signed it
		// unless the kubelet client trust is split; a split node reissues it
		// under kubelet-client-ca without reading it.
		split, err := pki.KubeletClientSplit()
		return err == nil && split
	}
	return true
}

// acquireNodeLock takes the node maintenance lock (internal/nodelock, the
// /run/stratum-upgrade.lock flock the upgrade already fences at) for the life
// of one certs operation, and refuses rather than waits when another operation
// holds it. The refusal names the holder's operation and pid, and reaches the
// CLI and the MCP tool unchanged: they call exactly these functions, so
// pkg/cli adds nothing. Taking it here moves the fence to where the damage
// would happen, so a cluster Job and an operator's by-hand run meet it too.
//
// held is the held-lock capability (Options.NodeLockHeld): a caller that took
// the lock before calling passes it instead of
// reacquiring, because a second Acquire from the same process refuses against
// its own fence. The caller releases the lock on its exits.
func acquireNodeLock(op string, held bool) (release func(), err error) {
	if held {
		return func() {}, nil
	}
	return nodelock.Acquire(op)
}

// subjectDrift is pki.SubjectDrift; a test replaces it.
var subjectDrift = pki.SubjectDrift

// SubjectDrift is pki.SubjectDrift for this node: the leaves on disk whose
// subject this release no longer issues, as leaves an earlier release issued
// in system:masters.
func SubjectDrift() ([]string, error) {
	hostname, nodeIP, _, _, err := hostNodeIP()
	if err != nil {
		return nil, err
	}
	return subjectDrift(hostname, nodeIP)
}

// Rotate reissues the leaf certs from the existing CA, rewrites the kubeconfigs
// preserving the current endpoint, and restarts the control-plane units. It
// reports whether the run reissued anything: (false, nil) when the window
// check found nothing to rotate, (true, nil) after a completed rotation, and
// false beside the error on every refusal or failure.
//
// The cluster fence and host lock come before any change, so a run
// beside a live upgrade or other maintenance operation refuses having reissued
// nothing, and the defer releases it on every exit.
func Rotate(w io.Writer, o Options) (bool, error) { return RotateContext(context.Background(), w, o) }

// RotateContext reissues the leaf certs from the existing CA, rewrites the
// kubeconfigs preserving the current endpoint, and restarts the
// control-plane units, bounded by ctx: a rotation whose context ends mid-run
// stops at the next step boundary — the step in flight (usually the RSA
// reissue) finishes, and no later step starts. The run group joins the
// renewal loop at shutdown, so this is what keeps that join from waiting out
// a whole rotation and what keeps systemd's KillMode=process from cutting one
// down mid-step instead. The cluster fence and host lock come before any
// change, so a run beside a live upgrade or other maintenance operation
// refuses having reissued nothing, and the defers release them on every exit.
// Like Rotate, it reports whether the run reissued anything: false when the
// window check found nothing to rotate or on any error, true only after the
// restart sequence completed.
func RotateContext(ctx context.Context, w io.Writer, o Options) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("certs: rotation not started: %w", err)
	}
	if err := RequireNodeProfile("rotate"); err != nil {
		return false, err
	}
	// A forced run reissues every leaf regardless of what is on disk, so a
	// damaged leaf must not stop the run that repairs it; a check error only
	// stops the timer path, where a damaged leaf is something a human should
	// see first. A forced run whose check failed has no statuses, so the
	// expired-leaf fallback below does not apply to it.
	needed := o.IfExpiringDays <= 0
	statuses, err := check(o, false)
	if err != nil && !needed {
		return false, err
	}
	if !needed {
		needed = anyLeafExpiringWithin(statuses, o.IfExpiringDays)
	}
	incomplete, startedExpired := IncompleteRotation()
	expired := anyLeafExpiringWithin(statuses, -1) || startedExpired
	if !needed && incomplete {
		needed = true
		fmt.Fprintf(w, "  WARN: a previous leaf rotation reissued leaves but did not restart every component onto them; rotating again\n")
	}
	var confirm func(string) error
	var rideOffLocal func(nodeIP string) error
	if needed && !o.NodeLockHeld {
		cs, err := adminClientset()
		if err != nil {
			err = fmt.Errorf("certificate renewal cannot establish cluster maintenance ownership: %w", err)
		}
		var rider *leaseRider
		var guard *maintlease.Guard
		var releaseMaintenance func()
		if err == nil {
			rider, guard, releaseMaintenance, err = acquireMaintenanceLease(ctx, cs, maintenanceHolder())
		}
		switch {
		case err == nil:
			defer releaseMaintenance()
			confirm = func(what string) error { return confirmMaintenanceLease(ctx, guard, what) }
			rideOffLocal = func(nodeIP string) error {
				return rideLeaseOffLocalAPIServer(ctx, w, rider, guard, cs, nodeIP)
			}
		case ctx.Err() == nil && !errors.Is(err, maintlease.ErrTaken) && expired:
			// An expired leaf is what keeps the API from answering (admin.conf
			// rejected, or the apiserver's etcd client refused), so no fence
			// can be taken through it, and refusing would leave the node with
			// no way to renew. A lease another run holds still refuses, and so
			// does a run whose context ended. Without the fence, each step
			// boundary still stops on the context.
			fmt.Fprintf(w, "  WARN: leaves have expired and the cluster maintenance lease cannot be taken (%v); rotating under the node lock alone\n", err)
			confirm = func(string) error { return stepBoundary(ctx) }
		default:
			return false, err
		}
	}
	release, err := acquireNodeLock("certs rotate", o.NodeLockHeld)
	if err != nil {
		return false, err
	}
	defer release()

	if !needed {
		if !o.Quiet {
			fmt.Fprintf(w, "certs: no leaf within %d days of expiry — nothing to rotate\n", o.IfExpiringDays)
		}
		return false, nil
	}

	hostname, nodeIP, apiExtra, vip, err := hostNodeIP()
	if err != nil {
		return false, err
	}
	// Through a VIP the lease reaches another apiserver while this one restarts.
	if rideOffLocal != nil && vip == "" {
		if err := rideOffLocal(nodeIP); err != nil {
			return false, err
		}
	}
	if confirm != nil {
		if err := confirm("reissuing leaves"); err != nil {
			return false, err
		}
	}
	var mark []byte
	if expired {
		mark = []byte(incompleteRotationExpired + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(incompleteRotationPath()), 0o755); err != nil {
		return false, fmt.Errorf("certs: marking the rotation incomplete before reissuing leaves: %w", err)
	}
	if err := atomicfile.WriteFile(incompleteRotationPath(), mark, 0o600); err != nil {
		return false, fmt.Errorf("certs: marking the rotation incomplete before reissuing leaves: %w", err)
	}
	step(w, o, "Reissue control-plane leaf certificates")
	if err := pki.RotateLeaves(hostname, nodeIP, apiExtra); err != nil {
		return false, err
	}
	if err := stepBoundary(ctx); err != nil {
		return false, err
	}

	if confirm != nil {
		if err := confirm("rewriting kubeconfigs"); err != nil {
			return false, err
		}
	}
	step(w, o, "Rewrite kubeconfigs (endpoint preserved)")
	server, err := kubeconfig.CurrentServer(version.AdminConf)
	if err != nil {
		// fall back to the local apiserver if admin.conf is unreadable
		server = fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)
		fmt.Fprintf(w, "  WARN: could not read current server endpoint (%v); using %s\n", err, server)
	}
	if err := writeKubeconfigs(hostname, server, vip); err != nil {
		return false, err
	}
	if err := stepBoundary(ctx); err != nil {
		return false, err
	}

	step(w, o, "Restart control-plane units")
	allRestarted, err := restartControlPlaneGuarded(w, staticpod.Restart, waitBounced, confirm, o.KubeVipRequired)
	if err != nil {
		return false, err
	}
	if !allRestarted {
		fmt.Fprintf(w, "  WARN: not every component restarted onto the reissued leaves; the next scheduled renewal rotates again\n")
		return true, nil
	}
	if err := os.Remove(incompleteRotationPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("certs: rotation completed but its incomplete marker could not be removed: %w", err)
	}
	if !o.Quiet {
		fmt.Fprintf(w, "certs: rotated — leaves valid for ~%d days\n", int(pki.LeafValidity.Hours()/24))
	}
	return true, nil
}

// stepBoundary reports a rotation whose context ended between two steps: the
// caller stops there instead of starting the next step, the safe point.
func stepBoundary(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("certs: rotation stopped at a step boundary: %w", err)
	}
	return nil
}

// restartControlPlane bounces the control-plane static pods in order
// (re-render with a bumped annotation; kubelet recreates the pod),
// health-gating etcd + apiserver (bounce observed → active → apiserver
// /healthz). A failed gate or component restart is collected while the
// remaining restarts are attempted, then returned. kube-vip and supervised
// host processes remain best-effort. Absent components are skipped. Shared by
// leaf rotation and CA rotation.
func restartControlPlane(w io.Writer) error {
	_, err := restartControlPlaneGuarded(w, staticpod.Restart, waitBounced, nil, false)
	return err
}

// Pass the pod operations explicitly so restart and readiness failures can be
// exercised without restarting a live control plane or waiting for timeouts.
func restartControlPlaneWith(w io.Writer, restartPod, waitPod func(string) error) (bool, error) {
	return restartControlPlaneGuarded(w, restartPod, waitPod, nil, false)
}

// restartControlPlaneGuarded takes its pod operations explicitly so restart and
// readiness failures can be exercised without restarting a live control plane
// or waiting for timeouts. allRestarted is false when a best-effort restart
// failed without failing the sequence.
func restartControlPlaneGuarded(w io.Writer, restartPod, waitPod func(string) error, confirm func(string) error, kubeVipRequired bool) (allRestarted bool, err error) {
	var failures []error
	allRestarted = true
	for _, name := range podRestartOrder {
		if !staticpod.Exists(name) {
			continue
		}
		if confirm != nil {
			if err := confirm("restarting " + name); err != nil {
				return false, errors.Join(append(failures, err)...)
			}
		}
		switch name {
		case "etcd", "kube-apiserver":
			// A restart that fails does not end the sequence either, and there is
			// nothing to gate: record it and go on to the later pods.
			if err := restartPod(name); err != nil {
				fmt.Fprintf(w, "  WARN: restart %s: %v (still restarting the rest)\n", name, err)
				failures = append(failures, fmt.Errorf("restart %s: %w", name, err))
				continue
			}
			// A gate that fails does not end the sequence: the leaves
			// are already on disk, and a slow kubelet is the usual cause. Skipping
			// the later restarts would leave those components on the old
			// certificates until they crash-looped onto the new files by chance.
			gate := waitPod(name)
			if gate == nil && name == "kube-apiserver" {
				if err := waitHealthz(healthzAttempts); err != nil {
					gate = fmt.Errorf("apiserver /healthz after restart: %w", err)
				}
			}
			if gate != nil {
				fmt.Fprintf(w, "  WARN: %s did not come back: %v (still restarting the rest)\n", name, gate)
				failures = append(failures, fmt.Errorf("wait for %s after restart: %w", name, gate))
			}
		case "kube-controller-manager", "kube-scheduler":
			// The wait must block until the component is RUNNING again, and
			// callers depend on it: `stratum token create` right
			// after a rotation needs the controller-manager (bootstrapsigner)
			// up to sign cluster-info. Attempt the remaining restarts on failure,
			// but do not report a successful rotation with either component down.
			if err := restartPod(name); err != nil {
				failures = append(failures, fmt.Errorf("restart %s: %w", name, err))
				continue
			}
			if err := waitPod(name); err != nil {
				failures = append(failures, fmt.Errorf("wait for %s after restart: %w", name, err))
			}
		default: // kube-vip: no local health listener to wait on
			if !kubeVipRequired {
				if err := restartPod(name); err != nil {
					fmt.Fprintf(w, "  WARN: restart %s: %v (continuing)\n", name, err)
					allRestarted = false
				}
				continue
			}
			if err := restartReplaced(name, restartPod); err != nil {
				fmt.Fprintf(w, "  WARN: restart %s: %v (continuing)\n", name, err)
				failures = append(failures, err)
			}
		}
	}
	for _, u := range hostRestartOrder {
		if !comps.UnitExists(u) {
			continue
		}
		if confirm != nil {
			if err := confirm("restarting " + u); err != nil {
				return false, errors.Join(append(failures, err)...)
			}
		}
		if err := comps.Restart(u); err != nil {
			fmt.Fprintf(w, "  WARN: restart %s: %v (continuing)\n", u, err)
			allRestarted = false
		}
	}
	if len(failures) > 0 {
		return false, fmt.Errorf("control-plane restart incomplete (every remaining restart was still attempted): %w",
			errors.Join(failures...))
	}
	return allRestarted, nil
}

// waitHealthz is a variable so the apiserver gate can be exercised without a
// 60s wait.
var waitHealthz = k8sutil.WaitHealthz

// markNow and waitReplaced are staticpod.Now and staticpod.WaitReplaced,
// variables so a required restart can be exercised without /proc.
var (
	markNow      = staticpod.Now
	waitReplaced = staticpod.WaitReplaced
)

// kubeVipReplaceWait is how long a required kube-vip restart waits for kubelet
// to replace the process that ran before it.
const kubeVipReplaceWait = 2 * time.Minute

// restartReplaced restarts the static pod name and waits until no process
// that started before the restart still runs it. Restarting rewrites the
// manifest only; a caller that needs the old process gone, as one moving it
// off a credential does, must see kubelet replace it.
func restartReplaced(name string, restartPod func(string) error) error {
	mark, err := markNow()
	if err != nil {
		return fmt.Errorf("restart %s: %w", name, err)
	}
	if err := restartPod(name); err != nil {
		return fmt.Errorf("restart %s: %w", name, err)
	}
	if err := waitReplaced(name, mark, kubeVipReplaceWait); err != nil {
		return fmt.Errorf("wait for %s to be replaced after restart: %w", name, err)
	}
	return nil
}

// healthzAttempts and downWindowWait are part of the worst-case time one control
// plane restart sequence can take, which the remote CA Job limits must cover
// (caJobDeadline): the apiserver /healthz wait ends after healthzAttempts ×
// k8sutil.HealthzPollInterval, and each pod is given downWindowWait to drop its
// listener before its serve gate starts.
const (
	healthzAttempts = 60
	downWindowWait  = 30 * time.Second
)

// serveBudget is how long a restarted static pod gets to serve again. etcd's is
// longer because the wait covers kubelet noticing the manifest, tearing down the
// old pod and mounting its volumes before etcd even starts: 163s on an idle node
// with ~40 pods, over the 90s every other pod gets. It is a
// ceiling, not a delay; a healthy restart returns as soon as the listener is up.
// ponytail: flat ceiling. A wait keyed to etcd's applied raft index needs
// a member to query; a bounce has none until etcd is serving.
func serveBudget(name string) time.Duration {
	if name == "etcd" {
		return 5 * time.Minute
	}
	return 90 * time.Second
}

// waitBounced watches a restarted static pod go down and come back. kubelet
// takes a beat to notice the manifest change, so "still serving" right after
// Restart may be the OLD process; wait for the listener to drop (bounded —
// missing the down-window is fine) and then for it to serve again.
func waitBounced(name string) error {
	down := time.Now().Add(downWindowWait)
	for time.Now().Before(down) && staticpod.Active(name) {
		time.Sleep(500 * time.Millisecond)
	}
	return staticpod.WaitActive(name, serveBudget(name))
}

func anyLeafExpiringWithin(ss []CertStatus, days int) bool {
	return slices.ContainsFunc(ss, func(s CertStatus) bool {
		return s.Rotatable && s.DaysLeft <= days
	})
}

// hostNodeIP returns this node's hostname, its kubelet node IP ("v4" or "v4,v6"
// under dual-stack), the apiserver-cert SANs that come from the cluster (the
// extra IPs and the cluster domain, from netconfig), and the VIP this node was
// installed with, or "" for none — so leaf rotation reproduces the same SANs
// the cluster was installed with (incl. IPv6). A caller passes this one VIP to
// writeKubeconfigs too, so the apiserver's VIP SAN and the kube-vip.kubeconfig
// rewrite come from the same read and cannot disagree.
//
// The inputs are the one constructor's (internal/certinputs), the path
// bring-up, the join and the upgrade issue through: a node with state answers
// by the constructor's recorded-address rule — renewal's refusal of a
// dual-stack node whose state lost the recorded pair included — and a node
// with no state file yet is detected, the way its first boot resolved it.
func hostNodeIP() (string, string, *pki.APISANs, string, error) {
	h, err := host.Hostname()
	if err != nil {
		return "", "", nil, "", err
	}
	netc, err := netcfg.Load()
	if err != nil {
		return "", "", nil, "", err
	}
	vip, err := nodeVIP()
	if err != nil {
		return "", "", nil, "", err
	}
	in, err := nodeInputs(h, netc, vip)
	if err != nil {
		return "", "", nil, "", err
	}
	return in.Host, in.NodeIP, in.SANs, vip, nil
}

// nodeInputs builds this node's certificate inputs through the constructor. A
// node with state answers by certinputs.ForNode, whose SANs and VIP come from
// that one state read; a node with no state file yet has no recorded address
// and no VIP, so its inputs are built from the address detection its first
// boot used.
func nodeInputs(h string, netc netcfg.Config, vip string) (certinputs.Inputs, error) {
	st, err := nodestate.LoadState()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		ip, err := host.NodeIPs(netc.DualStack)
		if err != nil {
			return certinputs.Inputs{}, err
		}
		return certinputs.New(h, ip, netc, vip), nil
	case err != nil:
		return certinputs.Inputs{}, fmt.Errorf("read node state: %w", err)
	}
	return certinputs.ForNode(st, netc, h)
}

// writeKubeconfigs is the one place cert rotation rewrites the control-plane
// kubeconfigs, so leaf rotation and every CA-rotation phase include
// kube-vip.kubeconfig on a VIP node alike. vip is what hostNodeIP read.
func writeKubeconfigs(hostname, server, vip string) error {
	return kubeconfig.GenerateAll(hostname, server, vip != "")
}

// nodeVIP is the VIP this node was installed with, or "" for none. A node
// with no node state has no VIP; node state that cannot be read is an error,
// never "no VIP": rotating a VIP node as if it had none drops the VIP SAN
// from the apiserver certificate and leaves kube-vip on the superseded leaf.
func nodeVIP() (string, error) {
	st, err := nodestate.LoadState()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("cannot tell whether this node serves a VIP: %w", err)
	}
	return st.VIP.Address, nil
}

func step(w io.Writer, o Options, msg string) {
	if !o.Quiet {
		k8sutil.Step(w, msg)
	}
}

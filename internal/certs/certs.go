// Package certs checks expiry and rotates node leaf certificates and the cluster CAs.
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

// Static pods first, then host units; operator last because it reads super-admin.conf.
var (
	podRestartOrder  = staticpod.Names // already in start order
	hostRestartOrder = hostRestartOrderFrom(comps.Units)
)

// hostRestartOrderFrom drops containerd, which holds no certificates to adopt.
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

// RenewalRecord is the renewal loop's last attempt, shown by `stratum certs check`.
type RenewalRecord struct {
	AttemptedAt time.Time `json:"attemptedAt"`
	// Result is that attempt's rotation error, empty when it succeeded.
	Result string `json:"result,omitempty"`
}

func RenewalRecordPath() string { return filepath.Join(version.K8sConfDir, "renewal.yaml") }

// Present while a rotation reissued leaves without restarting every component.
func incompleteRotationPath() string {
	return filepath.Join(version.K8sConfDir, "rotation-incomplete")
}

// Marker for a rotation begun from expired leaves; its retry keeps the expired-leaf path.
const incompleteRotationExpired = "expired"

// IncompleteRotation reports incomplete and expired; an unreadable marker still counts as present.
func IncompleteRotation() (present, expired bool) {
	if _, err := os.Lstat(incompleteRotationPath()); errors.Is(err, fs.ErrNotExist) {
		return false, false
	}
	b, _ := os.ReadFile(incompleteRotationPath())
	return true, strings.TrimSpace(string(b)) == incompleteRotationExpired
}

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

// LoadRenewalRecord reports a missing record as an error wrapping fs.ErrNotExist.
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
	Rotatable      bool // false for CAs
	Kubelet        bool // the kubelet's CSR-issued client cert, which the kubelet renews itself
	RenewalOverdue bool // the kubelet client cert is past its latest expected renewal point
}

// KubeletClientName is the `certs check` row for the kubelet's CSR-issued client cert.
const KubeletClientName = "kubelet-client"

// Options for RotateNode.
type Options struct {
	IfExpiringDays int  // >0: no-op unless a leaf is within this many days of expiry (timer path). 0 = force.
	WarnDays       int  // window for the "expiring" flag in Check (default version.CertRenewWindowDays)
	Quiet          bool // suppress the per-step step logs (timer path)

	// Caller holds the node lock and the cluster fence for the whole call and releases them itself.
	NodeLockHeld bool

	// Fails the rotation when the kube-vip restart is unconfirmed, not best-effort.
	KubeVipRequired bool
}

// The window behind "expiring"; a caller's positive WarnDays wins, else the timer's renewal window.
func warnDays(o Options) int {
	if o.WarnDays > 0 {
		return o.WarnDays
	}
	return version.CertRenewWindowDays
}

// Check reports expiry for managed certificates and the kubelet client cert.
func Check(o Options) ([]CertStatus, error) {
	return check(o, true)
}

// Rotation skips the kubelet client cert so an unrelated expired kubelet cert cannot block it.
func check(o Options, includeKubelet bool) ([]CertStatus, error) {
	hostname, nodeIP, _, _, err := hostNodeIP()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	status := func(name string, na time.Time) CertStatus {
		// Floor negative durations too: a cert expired an hour ago reports -1, not zero days.
		days := int(math.Floor(na.Sub(now).Hours() / 24))
		return CertStatus{Name: name, NotAfter: na, DaysLeft: days, Expiring: days <= warnDays(o)}
	}
	mk := func(name string, isCA, rotatable, reparable bool) (CertStatus, bool, error) {
		na, err := pki.CertNotAfter(name)
		if err != nil {
			// Only an absent file is skipped; present but unreadable is damage a human sees first.
			if !errors.Is(err, fs.ErrNotExist) {
				p := filepath.Join(version.PKIDir, name+".crt")
				err = fmt.Errorf("certificate %s is present but cannot be read or parsed: %w", p, err)
				if rotatable && reparable {
					// Offer the rotate remedy only where the signer reissues without reading the damaged file.
					const rotate = "`sudo /usr/local/bin/stratum certs rotate --yes`"
					if st, serr := os.Lstat(p); serr == nil && st.IsDir() {
						// Rename cannot replace a directory, so it must be moved aside first.
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
	// Long-lived CAs that never auto-renew still belong in the expiry table.
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

// False where the signer would read the damaged leaf and fail, making the remedy false advice.
func rotationRepairs(etcdSpecs []pki.CertSpec, name string) bool {
	for _, sp := range etcdSpecs {
		if sp.Name == name {
			// etcdLeafSigner reads the live leaf once the etcd CA exists; a pre-migration node does not.
			return !pki.EtcdCAExists()
		}
	}
	if name == "apiserver-kubelet-client" {
		// kubeletClientReissuer reads the live leaf unless the kubelet client trust is split.
		split, err := pki.KubeletClientSplit()
		return err == nil && split
	}
	return true
}

// held: a same-process second Acquire would refuse against the caller's own fence.
func acquireNodeLock(op string, held bool) (release func(), err error) {
	if held {
		return func() {}, nil
	}
	return nodelock.Acquire(op)
}

// subjectDrift is pki.SubjectDrift; a test replaces it.
var subjectDrift = pki.SubjectDrift

// SubjectDrift lists this node's leaves whose subject this release no longer issues.
func SubjectDrift() ([]string, error) {
	hostname, nodeIP, _, _, err := hostNodeIP()
	if err != nil {
		return nil, err
	}
	return subjectDrift(hostname, nodeIP)
}

// Reissues leaves, rewrites kubeconfigs, restarts units; the bool says if anything was reissued.
func RotateContext(ctx context.Context, w io.Writer, o Options) (bool, error) {
	// A negative window would read as a forced run below and reissue every leaf without consent.
	if o.IfExpiringDays < 0 {
		return false, fmt.Errorf("certs: rotation not started: the if-expiring window must be 0 (rotate now) or a number of days above 0, not %d", o.IfExpiringDays)
	}
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("certs: rotation not started: %w", err)
	}
	if err := RequireNodeProfile("rotate"); err != nil {
		return false, err
	}
	// A forced run repairs a damaged leaf, so a check error stops only the timer path.
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
			// Expired leaves can leave the API unanswerable, so the run proceeds under the node lock alone.
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

// stepBoundary stops a rotation whose context ended between two steps.
func stepBoundary(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("certs: rotation stopped at a step boundary: %w", err)
	}
	return nil
}

func restartControlPlane(w io.Writer) error {
	_, err := restartControlPlaneGuarded(w, staticpod.Restart, waitBounced, nil, false)
	return err
}

// Takes pod ops for tests; allRestarted is false with nil error when a best-effort restart failed.
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
			if err := restartPod(name); err != nil {
				fmt.Fprintf(w, "  WARN: restart %s: %v (still restarting the rest)\n", name, err)
				failures = append(failures, fmt.Errorf("restart %s: %w", name, err))
				continue
			}
			// The leaves are already on disk; later restarts must not be skipped on a failed gate.
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
			// Callers depend on the component running again; a failed restart or wait fails the rotation.
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

// waitHealthz is a variable so the apiserver gate can be tested without a 60s wait.
var waitHealthz = k8sutil.WaitHealthz

// Variables so a required restart can be tested without /proc.
var (
	markNow      = staticpod.Now
	waitReplaced = staticpod.WaitReplaced
)

// How long a required kube-vip restart waits for kubelet to replace the prior process.
const kubeVipReplaceWait = 2 * time.Minute

// A restart rewrites the manifest only; kubelet must replace the old process too.
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

// Part of the worst-case restart sequence time the remote CA Job limits must cover.
const (
	healthzAttempts = 60
	downWindowWait  = 30 * time.Second
)

// A ceiling, not a delay; etcd's covers kubelet noticing the manifest and mounting volumes.
func serveBudget(name string) time.Duration {
	if name == "etcd" {
		return 5 * time.Minute
	}
	return 90 * time.Second
}

// "Still serving" just after Restart may be the old process; wait for it to drop, then serve.
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

// hostNodeIP returns the hostname, node IP, apiserver SANs and installed VIP for leaf rotation.
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

// A node with no state file has no recorded address; its inputs come from first-boot detection.
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

// The one rewrite site, shared by leaf and CA rotation; vip is what hostNodeIP read.
func writeKubeconfigs(hostname, server, vip string) error {
	return kubeconfig.GenerateAll(hostname, server, vip != "")
}

// Unreadable state is an error, not "no VIP": that would drop the VIP SAN from the apiserver cert.
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

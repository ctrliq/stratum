package upgrade

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/rollout"
	"github.com/ctrliq/stratum/internal/version"
)

// Shared by the cluster preflight's refusal and a node's own --check refusal.
const selinuxPrereqDetail = "(container-selinux, policycoreutils, policycoreutils-python-utils or libselinux-utils missing on an enforcing node or one whose profile requires SELinux, or spec.selinux: false combined with such a profile)"

const apparmorPrereqDetail = "or the AppArmor one: where kernel.apparmor_restrict_unprivileged_unconfined is set, Stratum's runc needs the AppArmor profile runc, and it could not be built"

const cuNamespace = "kube-system"

// Held beside upgradeLeaseName so the --cluster maintenance operations exclude one another.
const maintenanceLeaseName = "stratum-cluster-maintenance"

// The Job's deadline adds a best-effort slice for the stamp and backstop uncordon.
const (
	upgradeWorkBudget  = 15 * time.Minute
	upgradeJobDeadline = upgradeWorkBudget + 5*time.Minute
	upgradeJobPolls    = int(upgradeJobDeadline / (5 * time.Second))
)

// The import itself waits up to three minutes for containerd, then reads the payload's archives.
const (
	imageImportDeadline    = 10 * time.Minute
	imageImportPolls       = int(imageImportDeadline / (5 * time.Second))
	imageImportConcurrency = 4
)

const maxJobNameLen = 63

// An unsatisfiable PDB is a deterministic failure, not a transient one.
const drainTimeout = 15 * time.Minute

const orchestratorHealthzWindow = 2 * time.Minute

// k8sutil.WaitHealthzClient, indirected for tests.
var waitAPIHealthz = k8sutil.WaitHealthzClient

// ClusterOptions configures `stratum upgrade --cluster`.
type ClusterOptions struct {
	Target                string
	From                  string // upgrade binary staged at this same path on EVERY node (required)
	WorkersMaxUnavailable int
	DrainGraceSec         int

	// Checked before every destructive step; nil in dry-run and tests.
	lease *leaseGuard
	// Guard of the shared maintenance lease, taken alongside lease; nil in dry-run and tests.
	maint        *maintlease.Guard
	SoakSec      int
	SkipSnapshot bool
	DryRun       bool

	// Each node verifies the staged binary (fixed host script); MCP sets it, the CLI trusts --from.
	VerifyStagedOnNode bool

	// Nil means this node's admin.conf; MCP passes its own credential, bounding the rollout by RBAC.
	Client kubernetes.Interface

	// Pins every node's Job to the --from binary: same build everywhere, exact bytes on this arch.
	expectBuild, expectSHA256, arch string
	// The staged binary's release; this orchestrator may be an older build than it installs.
	stratumVersion string
}

type nodeOutcome int

const (
	nodeUpgraded  nodeOutcome = iota
	nodeCurrent               // already on this build: not drained
	nodeUnchecked             // at target but NotReady: its build could not be asked
)

// Takes the shared maintenance lease under the same holder identity as the released upgrade lease.
func acquireMaintenanceLease(ctx context.Context, cs kubernetes.Interface, holder string) (*maintlease.Guard, func(), error) {
	return maintlease.AcquireWithClock(ctx, cs, cuNamespace, maintenanceLeaseName, holder, "maintenance run", leaseNow)
}

// Either lease gone stops the run before it touches another node.
func (o *ClusterOptions) leaseErr() error {
	if err := o.lease.Err(); err != nil {
		return err
	}
	return o.maint.Err()
}

// Lost for good, not the recoverable stale state, which still rides the renewals in leaseConfirm.
func (g *leaseGuard) lostErr() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lost
}

// A same-holder renewal is a compare-and-swap; Err alone only reflects the renewals made so far.
func (o *ClusterOptions) leaseConfirm(ctx context.Context) error {
	if lost := o.lease.lostErr(); lost != nil {
		return fmt.Errorf("%w: %w", errLeaseTaken, lost)
	}
	return o.lease.confirm(ctx)
}

// UpgradeCluster rolls the cluster: serial control planes, then WorkersMaxUnavailable workers.
func UpgradeCluster(w io.Writer, o ClusterOptions) error {
	if o.From == "" {
		return fmt.Errorf("--cluster requires --from <upgrade-binary>: the new stratum binary staged at the same " +
			"path on every node (its embedded payload has no in-cluster transport); each per-node Job execs it directly")
	}
	if o.WorkersMaxUnavailable < 1 {
		o.WorkersMaxUnavailable = 1
	}
	ctx := context.Background()
	cs := o.Client
	if cs == nil {
		admin, err := k8sutil.AdminClientset()
		if err != nil {
			return err
		}
		cs = admin
	}
	plan, err := Plan(ctx, cs, o.Target, o.WorkersMaxUnavailable)
	if err != nil {
		return err
	}
	if !plan.Allowed {
		return fmt.Errorf("preflight: %s", plan.Blocker)
	}
	ordered := orderedNodes(plan.Nodes)

	// The same classification and ordering the MCP upgrade_plan tool reports, so they cannot disagree.
	byName := make(map[string]Node, len(plan.Nodes))
	for _, n := range plan.Nodes {
		byName[n.Name] = n
	}
	k8sutil.Step(w, fmt.Sprintf("Upgrade plan -> %s", o.Target))
	for _, s := range plan.Steps {
		for _, name := range s.Nodes {
			n := byName[name]
			fmt.Fprintf(w, "  %-20s %-14s current=%-10s %s\n", n.Name, s.Role, n.Version, planState(n, o.Target))
		}
	}
	fmt.Fprintln(w, "  first, import the upgrade binary's images on every Ready node: the add-ons roll out to every node as soon as the first control plane is upgraded")
	warnUntrustedKubeletServing(w, ctx, ordered)
	if o.DryRun {
		fmt.Fprintln(w, "\n(dry-run; no changes) — ensure the upgrade binary is staged at", o.From, "on every node.")
		return nil
	}

	// The released lease name goes first: an orchestrator on the previous release reads it.
	holder := upgradeHolder()
	lease, releaseLease, err := acquireUpgradeLease(ctx, cs, holder)
	if err != nil {
		return err
	}
	defer releaseLease()
	maint, releaseMaint, err := acquireMaintenanceLease(ctx, cs, holder)
	if err != nil {
		return err
	}
	defer releaseMaint()
	lease.maintenance = maint
	o.lease = lease
	o.maint = maint
	present := make([]string, 0, len(plan.Nodes))
	for _, n := range plan.Nodes {
		present = append(present, n.Name)
	}
	if err := sweepUpgradeJobs(w, ctx, cs, present); err != nil {
		return err
	}

	// Swept up front: a skipped node would keep a legacy Job whose name may block another node's own.
	for _, n := range ordered {
		if err := clearStaleUpgradeJobs(w, ctx, cs, n.Name); err != nil {
			return err
		}
	}

	// Unpinned, nodes answer for whatever is staged there and the run can report mixed builds complete.
	if err := pinStagedBinary(&o); err != nil {
		return err
	}

	// Before anything is cordoned or drained; the run refuses naming every failing node at once.
	if err := preflightNodePrereqs(ctx, w, cs, ordered, o); err != nil {
		return err
	}
	// After the prerequisite pass, so a run it refuses imports nothing.
	if err := importImagesOnEveryNode(ctx, w, cs, ordered, o); err != nil {
		return err
	}

	var (
		mu                sync.Mutex
		upgraded, current int
		unchecked         []string
	)
	tally := func(n Node, out nodeOutcome) {
		mu.Lock()
		defer mu.Unlock()
		switch out {
		case nodeUpgraded:
			upgraded++
		case nodeCurrent:
			current++
		case nodeUnchecked:
			unchecked = append(unchecked, n.Name)
		}
	}

	// Also after a halted run: a re-run finds those nodes current and would never print the steps.
	defer func() {
		if upgraded > 0 {
			fmt.Fprint(w, confinementRestartSteps(stratumInstallPath))
		}
	}()

	cps, workers := nodesToUpgrade(ordered)
	// Control planes serially, never two etcd members down at once.
	for _, n := range cps {
		out, err := upgradeOneNode(w, ctx, cs, n, o)
		if err != nil {
			return fmt.Errorf("control-plane %s: %w (halting; later nodes untouched)", n.Name, err)
		}
		tally(n, out)
	}
	// Not etcd members, and the eviction API enforces PDBs regardless of drain concurrency.
	if o.WorkersMaxUnavailable > 1 {
		k8sutil.Step(w, fmt.Sprintf("Upgrading %d worker(s), up to %d at a time", len(workers), o.WorkersMaxUnavailable))
	}
	ww := newLockedWriter(w)
	if err := runBounded(workers, o.WorkersMaxUnavailable, func(n Node) error {
		out, err := upgradeOneNode(ww, ctx, cs, n, o)
		if err != nil {
			return fmt.Errorf("worker %s: %w (halting; in-flight upgrades allowed to finish)", n.Name, err)
		}
		tally(n, out)
		return nil
	}); err != nil {
		return err
	}
	// A node that could not be asked is neither current nor upgraded.
	if len(unchecked) > 0 {
		sort.Strings(unchecked)
		return fmt.Errorf("upgraded %d node(s), %d already on this build; could not check %s, NotReady at %s -- bring them back and re-run to finish the upgrade",
			upgraded, current, strings.Join(unchecked, ", "), o.Target)
	}
	k8sutil.Step(w, fmt.Sprintf("Cluster upgrade complete: %d upgraded, %d already on this build", upgraded, current))
	return nil
}

func confinementRestartSteps(stratumPath string) string {
	stratum := shellQuote(stratumPath)
	kubectl := "sudo " + stratum + " kubectl"
	etcdctl := "sudo " + stratum + " etcdctl"
	localAPIServer := fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)
	restart := append(rollout.RestartCommands(kubectl), kubectl+" -n kube-system rollout restart ds/cilium-envoy")
	var status []string
	for _, wl := range rollout.Workloads {
		for _, t := range wl.Targets {
			status = append(status, kubectl+" -n "+wl.Namespace+" rollout status "+t)
		}
	}
	status = append(status, kubectl+" -n kube-system rollout status ds/cilium-envoy")
	var b strings.Builder
	b.WriteString(`
On SELinux nodes where this upgrade turned on container confinement, pods the drain does not evict (DaemonSet and static pods) keep running unconfined until they are recreated. The upgrade does not recreate them: run these steps yourself. Restart the ones Stratum owns, skipping the local-path-storage and stratum-dashboard lines on a cluster that does not run that add-on (they answer NotFound there):

  ` + strings.Join(restart, "\n  ") + `

Restart your own DaemonSets the same way:

  ` + kubectl + ` -n <namespace> rollout restart ds/<name>

A DaemonSet with the OnDelete update strategy, Stratum's or yours (` + kubectl + ` -n <namespace> get ds <name> -o jsonpath='{.spec.updateStrategy.type}' prints OnDelete), is not recreated by a restart, and rollout status refuses it with an error: skip its restart and status lines and instead delete its pods one at a time (` + kubectl + ` -n <namespace> delete pod <pod>), waiting for each replacement to be Running and Ready (` + kubectl + ` -n <namespace> get pods -o wide) before deleting the next.

Wait for every restart to finish, yours included (` + kubectl + ` -n <namespace> rollout status ds/<name>), and resolve any that fails or times out before going on (a NotFound for an add-on the cluster does not run is not a failure):

  ` + strings.Join(status, "\n  ") + `

No restart reaches the control-plane static pods; a reboot of each control-plane node does. A reboot takes that node's etcd member down, so start only when every control-plane node is upgraded and Ready and every etcd member is healthy (recover a control plane this upgrade halted on first). On a control-plane node, every member must report healthy:

  ` + etcdctl + ` endpoint health --cluster

Then, on each control-plane node, one at a time: before you drain it, see where CoreDNS runs (` + kubectl + ` -n kube-system get pods -l ` + dnsSelector + ` -o wide). CoreDNS has no PodDisruptionBudget, so a drain evicts every replica on the node. If the node runs one, drain it only once another replica, not Terminating, is Running and Ready on another node that is Ready (` + kubectl + ` get nodes); if none is, stop and get one serving there first. On a single-node cluster there is no other node: cluster DNS is down from the drain until you uncordon the node. Then drain it (` + kubectl + ` drain <node> --ignore-daemonsets --delete-emptydir-data), reboot it, and wait for it to be Ready. Ready is not enough to go on: its kubelet can report through another apiserver while its own etcd and apiserver are still down. On that node, run both of these until every etcd member reports healthy and its own apiserver answers ok:

  ` + etcdctl + ` endpoint health --cluster
  ` + kubectl + ` --server ` + localAPIServer + ` get --raw /readyz

Then uncordon it (` + kubectl + ` uncordon <node>) unless you had cordoned it yourself before the drain, and only then go on to the next control-plane node.
`)
	return b.String()
}

// Build ID via `version --build`; "" from a binary predating the flag. A var for tests.
var readStagedBuild = func(path string) string {
	out, err := exec.Command(path, "version", "--build").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Release version of the upgrade binary at path. A var for tests.
var readStagedVersion = k8sutil.BinaryVersion

// Pins o's build ID, sha256 (this architecture) and staged release from the --from binary.
func pinStagedBinary(o *ClusterOptions) error {
	refuse := func(why string) error {
		return fmt.Errorf("the upgrade binary %s %s; the rollout is not started, since without it each node would answer for whatever is staged there. Stage a build that reports its build ID, or upgrade each node with `stratum upgrade --from %s`", o.From, why, o.From)
	}
	o.expectBuild = readStagedBuild(o.From)
	if o.expectBuild == "" || o.expectBuild == "unknown" {
		return refuse("does not report a build ID")
	}
	// The version is read before the hash, so the stamped release belongs to the hashed bytes.
	var err error
	if o.stratumVersion, err = readStagedVersion(o.From); err != nil {
		return refuse("does not report its version (" + err.Error() + ")")
	}
	sum, err := fileSHA256(o.From)
	if err != nil {
		return refuse("cannot be hashed here (" + err.Error() + ")")
	}
	o.expectSHA256 = hex.EncodeToString(sum)
	o.arch = goruntime.GOARCH
	return nil
}

// The first control plane's add-on manifests roll out to every node at once, Cilium included.
func importImagesOnEveryNode(ctx context.Context, w io.Writer, cs kubernetes.Interface, nodes []Node, o ClusterOptions) error {
	k8sutil.Step(w, "Importing the upgrade binary's images on every node")
	ok, err := stagedImportsImages(ctx, o.From)
	if err != nil {
		return fmt.Errorf("%w (no node was cordoned or upgraded)", err)
	}
	if !ok {
		fmt.Fprintf(w, "  WARN: the upgrade binary %s has no image import step, so each node gets the new images only in its own upgrade, and the add-on pods the first control plane rolls out may not start on the others until then\n", o.From)
		return nil
	}
	ww := newLockedWriter(w)
	return runBounded(nodes, imageImportConcurrency, func(n Node) error {
		if !n.Ready {
			fmt.Fprintf(ww, "  WARN: %s is NotReady, so its images cannot be imported now; the new add-on pods may not start there, and if it is Ready again by its turn its drain can stall. Re-run the cluster upgrade once it is Ready\n", n.Name)
			return nil
		}
		// Confirm, not Err alone: a batch can start minutes after the leases were taken.
		if err := retryAPI(ctx, o.leaseConfirm); err != nil {
			return err
		}
		err := runImageImport(ctx, cs, nodejob.Spec{
			Output:         ww,
			Namespace:      cuNamespace,
			Name:           jobName(n.Name),
			Node:           n.Name,
			LabelKey:       upgradeJobLabel,
			Container:      "upgrade",
			Argv:           nodeImportImagesArgv(o, n),
			ActiveDeadline: imageImportDeadline,
			PollAttempts:   imageImportPolls,
			Kind:           "image import",
		})
		if err != nil {
			return fmt.Errorf("import the upgrade binary's images on %[1]s: %[2]w (no node was cordoned or upgraded; fix the import on %[1]s, then re-run the cluster upgrade)", n.Name, err)
		}
		return nil
	})
}

func planState(n Node, target string) string {
	switch {
	case n.Interrupted() == target:
		return "finish (interrupted after its kubelet swap)"
	// Before the build check: a NotReady node cannot run the check's pod.
	case n.NeedsBuildCheck(target) && !n.Ready:
		return "skip (NotReady: its build cannot be checked; the run ends with an error naming it)"
	case n.NeedsBuildCheck(target):
		return "check build (not drained if it already runs this build)"
	}
	return "upgrade"
}

// Every node is visited: one at target by a finished upgrade may still run another stratum build.
func nodesToUpgrade(ordered []Node) (cps, workers []Node) {
	for _, n := range ordered {
		if n.IsCP {
			cps = append(cps, n)
		} else {
			workers = append(workers, n)
		}
	}
	return cps, workers
}

// The CLI's os.Stdout tolerates concurrent writers; the MCP handler's buffer does not.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newLockedWriter(w io.Writer) *lockedWriter { return &lockedWriter{w: w} }

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

// On the first error it stops launching new work but lets already-started fns finish.
func runBounded[T any](items []T, limit int, fn func(T) error) error {
	if limit < 1 {
		limit = 1
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, limit)
	for _, item := range items {
		// Checked after the acquire, not before, or a just-finished failure could race a new launch.
		sem <- struct{}{}
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			<-sem
			break
		}
		wg.Add(1)
		go func(item T) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(item); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(item)
	}
	wg.Wait()
	return firstErr
}

func upgradeOneNode(w io.Writer, ctx context.Context, cs kubernetes.Interface, n Node, o ClusterOptions) (nodeOutcome, error) {
	role := "worker"
	if n.IsCP {
		role = "control-plane"
	}
	jn := jobName(n.Name)

	// Confirm, not Err alone: the gap since the last confirm can hold a takeover neither loop has seen.
	if err := retryAPI(ctx, o.leaseConfirm); err != nil {
		return nodeUpgraded, err
	}

	// The gap since the cluster-wide pass is the window an unfenced older run could use.
	if err := clearStaleUpgradeJobs(w, ctx, cs, n.Name); err != nil {
		return nodeUpgraded, err
	}
	// Asked before the cordon, so a node on this build is neither drained nor reported upgraded.
	if n.NeedsBuildCheck(o.Target) {
		// A NotReady node cannot start the check's pod; it is left alone and named at the end.
		if !n.Ready {
			fmt.Fprintf(w, "  WARN: %s is at %s but NotReady; its stratum build cannot be checked, so it is left alone\n", n.Name, o.Target)
			return nodeUnchecked, nil
		}
		k8sutil.Step(w, fmt.Sprintf("Checking %s (%s): at %s; comparing its stratum build with the upgrade binary", n.Name, role, o.Target))
	} else {
		k8sutil.Step(w, fmt.Sprintf("Upgrading %s (%s) -> %s", n.Name, role, o.Target))
	}
	// The check also holds the staged binary to the pin; found later, the node is already cordoned.
	if n.Ready {
		current, err := nodeRunsThisBuild(ctx, w, cs, n, o)
		if err != nil {
			return nodeUpgraded, fmt.Errorf("check %s's stratum build: %w (not drained)", n.Name, err)
		}
		// The annotation alone decides; the Node status can still show the old kubelet version.
		switch {
		case current && n.InProgress != o.Target:
			// Not the full path, which would drain a node already on the target build.
			if err := retryAPI(ctx, func(ctx context.Context) error {
				return releaseCurrentCordon(ctx, cs, n.Name, o.lease)
			}); err != nil {
				return nodeUpgraded, fmt.Errorf("release %s's cordon: %w", n.Name, err)
			}
			fmt.Fprintf(w, "  %s already runs this build of %s; not drained\n", n.Name, o.Target)
			return nodeCurrent, nil
		case current:
			fmt.Fprintf(w, "  %s already runs this build of %s, but its upgrade was never recorded as finished; finishing it\n", n.Name, o.Target)
		}
		if n.NeedsBuildCheck(o.Target) {
			fmt.Fprintf(w, "  %s runs another stratum build of %s; upgrading it\n", n.Name, o.Target)
		}
	}
	if err := cordonForUpgrade(ctx, cs, n.Name, o.Target, o.lease); err != nil {
		return nodeUpgraded, fmt.Errorf("cordon: %w", err)
	}
	// After the cordon, so no replica can land here while this decides. Released when the drain ends.
	releaseDNS, err := gateCoreDNSDrain(w, ctx, cs, n.Name)
	if err != nil {
		return nodeUpgraded, fmt.Errorf("drain: %w", err)
	}
	var releaseOnce sync.Once
	releaseDNSGate := func() { releaseOnce.Do(releaseDNS) }
	defer releaseDNSGate()
	// Again before the first eviction: the DNS gate can wait minutes.
	if err := o.leaseErr(); err != nil {
		return nodeUpgraded, fmt.Errorf("%w (%s left cordoned, nothing evicted)", err, n.Name)
	}
	grace := int64(o.DrainGraceSec)
	if grace == 0 {
		grace = 60
	}
	// Bounded: an unsatisfiable PDB would otherwise retry forever with the node already cordoned.
	drainDeadline := time.Now().Add(drainTimeout)
	for {
		// Every eviction attempt, not just the first: a drain can take minutes.
		if err := o.leaseErr(); err != nil {
			return nodeUpgraded, fmt.Errorf("%w (%s left cordoned mid-drain)", err, n.Name)
		}
		rem, err := Drain(ctx, cs, n.Name, jn, grace)
		if err != nil {
			return nodeUpgraded, fmt.Errorf("drain: %w", err)
		}
		if rem == 0 {
			releaseDNSGate()
			break
		}
		if time.Now().After(drainDeadline) {
			return nodeUpgraded, fmt.Errorf("drain of %s did not complete within %s: %d pod(s) still blocking "+
				"(check PodDisruptionBudgets: kubectl get pdb -A); node left cordoned", n.Name, drainTimeout, rem)
		}
		fmt.Fprintf(w, "  draining %s: %d pod(s) remaining (PDB-aware)\n", n.Name, rem)
		time.Sleep(5 * time.Second)
	}

	// And before the Job, the step that changes the host.
	if err := o.leaseErr(); err != nil {
		return nodeUpgraded, fmt.Errorf("%w (%s drained and left cordoned, not upgraded)", err, n.Name)
	}
	// Recorded before the Job: a Job dying after the kubelet swap must leave the node unfinished.
	if err := retryAPI(ctx, func(ctx context.Context) error {
		if err := o.leaseConfirm(ctx); err != nil {
			return err
		}
		return markUpgradeInProgress(ctx, cs, n.Name, o.Target, o.lease.id())
	}); err != nil {
		return nodeUpgraded, fmt.Errorf("record the upgrade on %s: %w (drained and left cordoned, not upgraded)", n.Name, err)
	}
	// Its log, not the Job's status, says whether the node rolled back.
	err = runNodeJob(ctx, cs, nodejob.Spec{
		Output:         w,
		Namespace:      cuNamespace,
		Name:           jn,
		Node:           n.Name,
		LabelKey:       upgradeJobLabel,
		Container:      "upgrade",
		Argv:           nodeUpgradeArgv(o, n),
		ActiveDeadline: upgradeJobDeadline,
		PollAttempts:   upgradeJobPolls,
		Kind:           "per-node upgrade",
		// Drained by now, so a critical pod may evict only DaemonSets; evicted, the check fails closed.
		Critical: true,
	})
	if err == nil {
		err = nodejob.WaitReady(ctx, cs, n.Name, 60)
	}
	if err == nil && n.IsCP {
		hctx, cancel := context.WithTimeout(ctx, orchestratorHealthzWindow)
		err = waitAPIHealthz(hctx, cs)
		cancel()
	}
	// The Job, Ready wait and gate can span an API outage long enough for a lease to expire.
	if err == nil {
		err = retryAPI(ctx, func(ctx context.Context) error {
			if err := o.leaseConfirm(ctx); err != nil {
				return err
			}
			return finishNode(ctx, cs, n.Name, o.Target, o.stratumVersion, o.lease.id())
		})
		if err != nil {
			err = fmt.Errorf("stamp version and uncordon %s: %w", n.Name, err)
		}
	}
	if err != nil {
		return nodeUpgraded, fmt.Errorf("%w\n%s", err, recoverySteps(n, jn, o))
	}
	fmt.Fprintf(w, "  %s upgraded to %s\n", n.Name, o.Target)
	return nodeUpgraded, nil
}

// The Job outlives this process; only a worker is said to be left cordoned.
func recoverySteps(n Node, job string, o ClusterOptions) string {
	// The node has no kubectl on its PATH, and a bare `stratum` may be an older release.
	from := shellQuote(o.From)
	state := "is left cordoned, and its upgrade Job"
	if n.IsCP {
		state = "may be left cordoned (its upgrade Job uncordons it once upgraded), and the Job"
	}
	return fmt.Sprintf("  %s %s may still be running. To recover:\n"+
		"    1. %s kubectl -n %s get job/%s       (wait for it to succeed or fail; \"not found\" means it\n"+
		"       already finished and was cleaned up)\n"+
		"    2. %s upgrade --cluster --target %s --from %s --yes    (finishes the node)\n"+
		"       or, if the node is at the target and healthy, and this run cordoned it (you did not cordon it yourself\n"+
		"       before the upgrade): %s kubectl uncordon %s",
		n.Name, state, from, cuNamespace, job, from, o.Target, from, from, n.Name)
}

// s as one shell word: unchanged when a shell leaves it alone, else single-quoted.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// The pre-hashing name: a plain truncation two long node names sharing a prefix could collide on.
func legacyJobName(node string) string {
	n := "stratum-upgrade-" + node
	if len(n) > maxJobNameLen {
		n = n[:maxJobNameLen]
	}
	return n
}

// By label, not name: an older binary left its Job under the previous truncated name.
func clearStaleUpgradeJobs(w io.Writer, ctx context.Context, cs kubernetes.Interface, node string) error {
	mine := jobName(node)
	list, err := cs.BatchV1().Jobs(cuNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: upgradeJobLabel + "=" + nodejob.NodeLabelValue(node),
	})
	if err != nil {
		return fmt.Errorf("look for earlier upgrade Jobs for %s: %w", node, err)
	}
	for i := range list.Items {
		j := &list.Items[i]
		// The label can be a hash, so confirm the node before acting.
		if nodejob.JobNode(j, upgradeJobLabel) != node {
			continue
		}
		if !nodejob.Finished(j) {
			return fmt.Errorf("a per-node upgrade Job %s is still running on %s (from an earlier or "+
				"concurrent run); refusing to start another on the same node. Wait for it to finish or "+
				"hit its deadline, then retry", j.Name, node)
		}
		if j.Name == mine {
			continue // nodejob.Run replaces this one itself.
		}
		fmt.Fprintf(w, "  removing the earlier upgrade Job %s left for %s\n", j.Name, node)
		if err := nodejob.DeleteExactAndWait(ctx, cs, j); err != nil {
			return fmt.Errorf("remove the earlier upgrade Job %s for %s: %w", j.Name, node, err)
		}
	}
	return nil
}

// Deliberately shell-free: every value is one argv element no interpreter ever re-parses.
func nodeUpgradeArgv(o ClusterOptions, n Node) []string {
	argv := append(stagedUpgradeArgv(o, n), "--soak", strconv.Itoa(o.SoakSec))
	if o.SkipSnapshot {
		argv = append(argv, "--skip-snapshot")
	}
	return argv
}

// Both the upgrade and its --check start here, so the check cannot answer for another binary.
func stagedUpgradeArgv(o ClusterOptions, n Node) []string {
	argv := []string{"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--"}
	if o.VerifyStagedOnNode {
		sum := ""
		if n.Arch == o.arch {
			sum = o.expectSHA256
		}
		argv = append(argv, "/bin/sh", "-c", verifyStagedScript, "stratum-verify-staged", o.From, "0", sum)
	}
	argv = append(argv,
		o.From, "upgrade",
		"--from", o.From,
		"--target", o.Target,
	)
	if o.expectBuild != "" {
		argv = append(argv, "--expect-build", o.expectBuild)
	}
	// Exact bytes only where they can match: another arch runs another binary of the same build.
	if o.expectSHA256 != "" && n.Arch == o.arch {
		argv = append(argv, "--expect-sha256", o.expectSHA256)
	}
	return argv
}

// Positional parameters only, so no path or value is ever parsed as shell.
const verifyStagedScript = `set -eu
f=$1 owner=$2 want=$3
shift 3
fail() { echo "stratum: refusing the staged upgrade binary on this node: $*" >&2; exit 1; }
check() {
	[ ! -L "$1" ] || fail "$1 is a symlink"
	[ "$(stat -c %u -- "$1")" = "$owner" ] || fail "$1 is not owned by uid $owner"
	case "$(stat -c %A -- "$1")" in
	?????w* | ????????w*) fail "$1 is writable by group or others" ;;
	esac
}
case "$f" in */*) d=${f%/*} ;; *) fail "$f is not an absolute path" ;; esac
check "${d:-/}"
[ -d "${d:-/}" ] || fail "${d:-/} is not a directory"
check "$f"
[ -f "$f" ] || fail "$f is not a regular file"
if [ -n "$want" ]; then
	got=$(sha256sum -- "$f")
	[ "${got%% *}" = "$want" ] || fail "$f has sha256 ${got%% *}, the rollout's is $want"
fi
exec "$@"
`

// Serially: the point is to name every refusing node at once, before anything is cordoned.
func preflightNodePrereqs(ctx context.Context, w io.Writer, cs kubernetes.Interface, ordered []Node, o ClusterOptions) error {
	var missing []string
	for _, n := range ordered {
		if err := o.leaseErr(); err != nil {
			return err
		}
		if !n.Ready {
			from := shellQuote(o.From)
			fmt.Fprintf(w, "  WARN: %s is NotReady, so the SELinux and AppArmor prerequisite checks cannot run on it; check it with sudo %s upgrade --from %s --check\n", n.Name, from, from)
			continue
		}
		code, log, err := runExitCodeLogs(ctx, cs, nodejob.Spec{
			Output:         w,
			Namespace:      cuNamespace,
			Name:           jobName(n.Name),
			Node:           n.Name,
			LabelKey:       upgradeJobLabel,
			Container:      "upgrade",
			Argv:           nodeCheckArgv(o, n),
			ActiveDeadline: 120 * time.Second,
			PollAttempts:   24, // ~2 min
			Kind:           "prerequisite check",
		})
		if errors.Is(err, nodejob.ErrLogUnread) {
			// The exit status still answers; an untrusted kubelet serving certificate stops only the log.
			fmt.Fprintf(w, "  WARN: %s: %v; any warnings its check printed are not shown\n", n.Name, err)
		} else if err != nil {
			return fmt.Errorf("prerequisite check on %s: %w (logs: kubectl -n %s logs job/%s)", n.Name, err, cuNamespace, jobName(n.Name))
		}
		for _, line := range strings.Split(log, "\n") {
			if strings.Contains(line, "WARN:") {
				fmt.Fprintf(w, "  %s: %s\n", n.Name, strings.TrimSpace(line))
			}
		}
		switch code {
		case 0, ExitUpgradeNeeded:
		case ExitPrereqMissing:
			missing = append(missing, n.Name)
		default:
			// Other answers are build-check refusals, worded as upgradeOneNode words them.
			return fmt.Errorf("check %s's stratum build: the check exited %d (logs: kubectl -n %s logs job/%s); nothing was cordoned or drained",
				n.Name, code, cuNamespace, jobName(n.Name))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("preflight: node(s) %s: each failed a prerequisite `stratum up` enforces: the SELinux one "+selinuxPrereqDetail+", "+apparmorPrereqDetail+". "+
			"Nothing was cordoned or drained. Run sudo %s upgrade --from %s --check on each node for its exact reason",
			strings.Join(missing, ", "), shellQuote(o.From), shellQuote(o.From))
	}
	return nil
}

// Asks via a Job running the upgrade binary's --check; it changes nothing on the host.
func nodeRunsThisBuild(ctx context.Context, w io.Writer, cs kubernetes.Interface, n Node, o ClusterOptions) (bool, error) {
	if err := o.leaseErr(); err != nil {
		return false, err
	}
	code, err := runExitCode(ctx, cs, nodejob.Spec{
		Output:         w,
		Namespace:      cuNamespace,
		Name:           jobName(n.Name),
		Node:           n.Name,
		LabelKey:       upgradeJobLabel,
		Container:      "upgrade",
		Argv:           nodeCheckArgv(o, n),
		ActiveDeadline: 120 * time.Second,
		PollAttempts:   24, // ~2 min
		Kind:           "build check",
	})
	if err != nil {
		return false, err
	}
	// Again after the Job: it can run for two minutes, and a lease can be lost meanwhile.
	if err := o.leaseErr(); err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case ExitUpgradeNeeded:
		return false, nil
	case ExitPrereqMissing:
		return false, fmt.Errorf("%w: the node failed a prerequisite `stratum up` enforces: the SELinux one "+selinuxPrereqDetail+", "+apparmorPrereqDetail+"; the node was not drained. Run sudo %s upgrade --from %s --check on it for its exact reason (logs: kubectl -n %s logs job/%s)",
			ErrPrereqMissing, shellQuote(o.From), shellQuote(o.From), cuNamespace, jobName(n.Name))
	default:
		return false, fmt.Errorf("build check exited %d (logs: kubectl -n %s logs job/%s)", code, cuNamespace, jobName(n.Name))
	}
}

// nodejob.Run; a var so a test can stop a node's upgrade at the host-changing step.
var runNodeJob = nodejob.Run

// nodejob.RunExitCode; a var so a test can answer for a node.
var runExitCode = nodejob.RunExitCode

// nodejob.RunExitCodeLogs; a var so a test can answer for a node.
var runExitCodeLogs = nodejob.RunExitCodeLogs

// nodejob.Run; a var so a test can answer for a node.
var runImageImport = nodejob.Run

// Only "unknown flag" means no; a probe failing otherwise would skip an import the binary can do.
var stagedImportsImages = func(ctx context.Context, path string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "upgrade", "--import-images", "--help").CombinedOutput()
	if err == nil {
		return true, nil
	}
	if strings.Contains(string(out), "unknown flag: --import-images") {
		return false, nil
	}
	return false, fmt.Errorf("ask the upgrade binary %s whether it can import images: %w", path, err)
}

// nodeUpgradeArgv's read-only twin: --check instead of upgrading.
func nodeCheckArgv(o ClusterOptions, n Node) []string {
	return append(stagedUpgradeArgv(o, n), "--check")
}

func nodeImportImagesArgv(o ClusterOptions, n Node) []string {
	return append(stagedUpgradeArgv(o, n), "--import-images")
}

func jobName(node string) string {
	return nodejob.TruncName("stratum-upgrade-" + node)
}

// Hashing alone cannot prevent a long node's hashed Job name equaling a short node's own name.
func validateUpgradeJobNames(nodes []Node) error {
	owners := make(map[string]string, len(nodes))
	for _, node := range nodes {
		name := jobName(node.Name)
		if other, exists := owners[name]; exists && other != node.Name {
			return fmt.Errorf("upgrade Job name %q collides for nodes %q and %q", name, other, node.Name)
		}
		owners[name] = node.Name
	}
	return nil
}

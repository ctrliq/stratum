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
)

// selinuxPrereqDetail is what the SELinux prerequisite refuses, shared by
// the cluster preflight's refusal and a node's own --check refusal.
const selinuxPrereqDetail = "(container-selinux, policycoreutils, policycoreutils-python-utils or libselinux-utils missing on an enforcing node or one whose profile requires SELinux, or spec.selinux: false combined with such a profile)"

const cuNamespace = "kube-system"

// maintenanceLeaseName is the shared cluster-wide maintenance Lease, held by
// this orchestrator alongside its own released one so an upgrade, a
// rotate-ca --cluster and a migrate-etcd-ca --cluster exclude one another
// whatever node each runs on. The upgrade's own lease name
// (upgradeLeaseName, lease.go) is a cluster-wide contract an orchestrator on
// the previous release still reads; the shared name is one too, pinned as a
// literal by the fence tests, so it is spelled out here and in
// internal/certs/cacluster.go rather than behind one package's constant.
const maintenanceLeaseName = "stratum-cluster-maintenance"

// The per-node upgrade Job's budget. upgradeWorkBudget is what the node's own
// upgrade is expected to take, as the Job's deadline always assumed. It is an
// assumption, not enforced at runtime: the Job's deadline adds a slice for what
// runs after the upgrade inside the Job, best effort, the version stamp
// (stampWindow) and the backstop uncordon (backstopWindow), so that an upgrade
// within upgradeWorkBudget does not reach the deadline during that tail. One
// that runs past it still can, and the Job is then reported failed though the
// upgrade may have landed. upgradeJobPolls is the orchestrator's poll count for
// the same span, at nodejob's 5 s interval.
const (
	upgradeWorkBudget  = 15 * time.Minute
	upgradeJobDeadline = upgradeWorkBudget + 5*time.Minute
	upgradeJobPolls    = int(upgradeJobDeadline / (5 * time.Second))
)

// The image import Job's budget: the import itself waits up to three minutes
// for containerd, then reads the payload's image archives. Imports run on up to
// imageImportConcurrency nodes at once; each one loads only its own node, so
// the bound only keeps a large cluster from creating every Job at once.
const (
	imageImportDeadline    = 10 * time.Minute
	imageImportPolls       = int(imageImportDeadline / (5 * time.Second))
	imageImportConcurrency = 4
)

// maxJobNameLen is the Kubernetes object-name limit.
const maxJobNameLen = 63

// drainTimeout bounds the PDB-aware drain wait per node. An unsatisfiable PDB is a
// deterministic failure, not a transient one, so the wait is bounded.
const drainTimeout = 15 * time.Minute

// orchestratorHealthzWindow bounds the apiserver /healthz wait after a
// control-plane node's upgrade Job (the 60 attempts, 2s apart, it replaced).
const orchestratorHealthzWindow = 2 * time.Minute

// waitAPIHealthz is k8sutil.WaitHealthzClient, indirected for tests.
var waitAPIHealthz = k8sutil.WaitHealthzClient

// ClusterOptions configures `stratum upgrade --cluster`.
type ClusterOptions struct {
	Target                string
	From                  string // upgrade binary staged at this same path on EVERY node (required)
	WorkersMaxUnavailable int
	DrainGraceSec         int

	// lease is checked before every destructive step (lease.go). Unset in
	// dry-run and in tests, where Err is nil.
	lease *leaseGuard
	// maint is the guard of the shared maintenance lease taken alongside
	// lease (kube-system/stratum-cluster-maintenance): the fence the upgrade,
	// rotate-ca --cluster and migrate-etcd-ca --cluster hold against one
	// another. Nil in dry-run and in tests, where Err is nil.
	maint        *maintlease.Guard
	SoakSec      int
	SkipSnapshot bool
	DryRun       bool

	// VerifyStagedOnNode has each node check the staged binary before it runs
	// it: neither the file nor its directory may be a symlink, both must be
	// owned by root and writable by no one else, and on a node of this
	// orchestrator's architecture the file must hash to expectSHA256. The
	// check is a fixed host shell script, so it does not depend on the release
	// a node is running. MCP sets it; the CLI trusts its operator's --from.
	VerifyStagedOnNode bool

	// Client is the API credential the rollout runs on. Nil means this node's
	// admin.conf, which is what the CLI uses; the MCP server passes the
	// credential it was started with, so its RBAC bounds the rollout.
	Client kubernetes.Interface

	// expectBuild and expectSHA256 pin every node's Job to the --from binary
	// on the node running this orchestrator, read once. Each node
	// runs its own copy at that path and refuses if it is another build; nodes
	// of this orchestrator's architecture must also match its exact bytes.
	expectBuild, expectSHA256, arch string
	// stratumVersion is the staged binary's release, stamped on every node:
	// this orchestrator may be an older build than the one it installs.
	stratumVersion string
}

// nodeOutcome is what upgradeOneNode did with a node.
type nodeOutcome int

const (
	nodeUpgraded  nodeOutcome = iota
	nodeCurrent               // already on this build: not drained
	nodeUnchecked             // at target but NotReady: its build could not be asked
)

// acquireMaintenanceLease takes the shared maintenance lease, kube-system/
// stratum-cluster-maintenance, under the same holder identity the released
// upgrade lease carries, or refuses naming the run that holds it. The lease
// mechanics live in internal/maintlease; its renewal rides this run's clock.
func acquireMaintenanceLease(ctx context.Context, cs kubernetes.Interface, holder string) (*maintlease.Guard, func(), error) {
	return maintlease.AcquireWithClock(ctx, cs, cuNamespace, maintenanceLeaseName, holder, "maintenance run", leaseNow)
}

// leaseErr reports whether this run still holds BOTH its leases, the released
// cluster-upgrade one and the shared maintenance one. Either being gone stops
// the run before it touches another node: a successor that took the shared
// lease may already be running another maintenance operation.
func (o *ClusterOptions) leaseErr() error {
	if err := o.lease.Err(); err != nil {
		return err
	}
	return o.maint.Err()
}

// lostErr is the lease this guard has already lost for good, if any — not the
// recoverable stale state, which may still be ours and must ride the renewals
// in leaseConfirm. It lives beside the confirm policy rather than in lease.go
// because that policy is what needs it; the guard's lock still guards it.
func (g *leaseGuard) lostErr() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lost
}

// leaseConfirm renews both leases now and returns nil only once both
// renewals succeeded: a same-holder renewal is a compare-and-swap, so it
// proves nobody held either lease since the last one. Err alone cannot say
// that — it only reflects the renewals the background loops have made so far,
// and after a long step a successor may already hold the shared lease before
// the next tick sees it.
//
// A guard that has already lost its lease for good refuses on the first
// attempt, wrapped in errLeaseTaken so retryAPI treats it as terminal: a lost
// lease stops the run at once instead of waiting out the API retry window,
// whatever the lost error's own shape. A stale guard is not lost — its lease
// may still be ours — and rides the renewals below, where one success clears
// it.
func (o *ClusterOptions) leaseConfirm(ctx context.Context) error {
	if lost := o.lease.lostErr(); lost != nil {
		return fmt.Errorf("%w: %w", errLeaseTaken, lost)
	}
	return o.lease.confirm(ctx)
}

// UpgradeCluster orchestrates a quorum-safe rolling HA upgrade from one CP node:
// preflight -> serial control planes -> workers (up to --workers-max-unavailable at
// a time); per node it does cordon -> drain (PDB-aware) -> per-node Job
// (`stratum upgrade --from`) -> node Ready + health gate -> stamp version ->
// uncordon. Halts on any node failure.
//
// Control planes are always serial (never two etcd members down at once). Workers
// run up to --workers-max-unavailable concurrently (default 1); they are not etcd
// members and the eviction API enforces PDBs regardless of drain concurrency.
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

	// The dry run renders the plan — the same classification and ordering the
	// MCP upgrade_plan tool reports, so the two cannot disagree about which
	// nodes are control planes or the order they roll in.
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

	// Exclusive cluster-wide from here until the run ends (lease.go). The
	// released lease name is taken FIRST — an orchestrator on the previous
	// release reads kube-system/stratum-cluster-upgrade before it upgrades —
	// then the shared maintenance lease, which fences this run against
	// rotate-ca --cluster and migrate-etcd-ca --cluster as well. Both are
	// held before the sweep below, the run's first change, and released on
	// every exit (defers unwind in reverse: the shared lease first, the
	// released name last).
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

	// Every node, up front, not only as each is reached: a node the run halts
	// before, or skips as current or NotReady, would otherwise keep a legacy
	// Job it left behind forever -- and if its legacy name happens to be
	// another node's current name, that node could never create its own Job.
	//
	// The collision resolves by ownership rather than by refusing the pair:
	// clearStaleUpgradeJobs acts only on Jobs whose own label says they target
	// that node, and leaves another node's current Job alone.
	for _, n := range ordered {
		if err := clearStaleUpgradeJobs(w, ctx, cs, n.Name); err != nil {
			return err
		}
	}

	// Pinned, or not run: unpinned, a node with another binary staged at that
	// path answers for its own, and the run can report complete with mixed
	// builds -- so a --from that cannot say what it is stops here, before
	// anything is drained.
	if err := pinStagedBinary(&o); err != nil {
		return err
	}

	// Before anything is cordoned or drained: every Ready node answers for its
	// prerequisites, and the run refuses, naming them all, if any would take
	// the upgrade unconfined.
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

	cps, workers := nodesToUpgrade(ordered)
	// Control planes serially (preserve etcd quorum: never two down at once).
	for _, n := range cps {
		out, err := upgradeOneNode(w, ctx, cs, n, o)
		if err != nil {
			return fmt.Errorf("control-plane %s: %w (halting; later nodes untouched)", n.Name, err)
		}
		tally(n, out)
	}
	// Workers, up to --workers-max-unavailable at a time. Control planes
	// stayed strictly serial above (etcd quorum); workers are NOT etcd members, and
	// the eviction API enforces PodDisruptionBudgets no matter how many drains run
	// concurrently — so bounded parallelism cannot over-evict. On the first failure
	// runBounded stops LAUNCHING new node upgrades but lets the in-flight ones
	// finish: a half-upgraded node left behind by a cancelled drain/Job is worse
	// than a slightly longer halt.
	//
	// The workers run concurrently, so their progress writes into w must be
	// serialized: the CLI hands os.Stdout, which is safe for concurrent
	// writers, but the MCP handler hands a plain buffer, which is not —
	// concurrent fmt.Fprintf calls on it race and can corrupt the progress
	// text. The locked writer makes the fan-out safe for either.
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
	// A node that could not be asked is neither current nor upgraded; the
	// run does not call itself complete with one left.
	if len(unchecked) > 0 {
		sort.Strings(unchecked)
		return fmt.Errorf("upgraded %d node(s), %d already on this build; could not check %s, NotReady at %s -- bring them back and re-run to finish the upgrade",
			upgraded, current, strings.Join(unchecked, ", "), o.Target)
	}
	k8sutil.Step(w, fmt.Sprintf("Cluster upgrade complete: %d upgraded, %d already on this build", upgraded, current))
	return nil
}

// readStagedBuild is the build ID of the upgrade binary at path, read by
// running it with `version --build`; "" when it cannot be read, as from a
// binary that predates the flag. A var for tests.
var readStagedBuild = func(path string) string {
	out, err := exec.Command(path, "version", "--build").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readStagedVersion is the release version of the upgrade binary at path. A var
// for tests.
var readStagedVersion = k8sutil.BinaryVersion

// pinStagedBinary fills o's pin from the --from binary on this node: its build
// ID for every node, its sha256 for the nodes of this architecture. Either
// unreadable is an error, before any node is touched.
func pinStagedBinary(o *ClusterOptions) error {
	refuse := func(why string) error {
		return fmt.Errorf("the upgrade binary %s %s; the rollout is not started, since without it each node would answer for whatever is staged there. Stage a build that reports its build ID, or upgrade each node with `stratum upgrade --from %s`", o.From, why, o.From)
	}
	o.expectBuild = readStagedBuild(o.From)
	if o.expectBuild == "" || o.expectBuild == "unknown" {
		return refuse("does not report a build ID")
	}
	// Both reads run the binary, so the version is read before the hash: the
	// stamped release then belongs to the bytes that were hashed.
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

// importImagesOnEveryNode has every Ready node import the upgrade binary's
// images before any node is upgraded. Once the first control plane runs the
// new release, its add-on manifests roll out to every node at once, Cilium's
// DaemonSet included, onto nodes still on the previous release. Those images
// come from the payload, not a registry: the mirror the manifests name serves
// no anonymous pulls and an airgapped node reaches no registry at all, so a
// node without them cannot start the new pods, and a node whose Cilium cannot
// start cannot finish its drain. A node that cannot import stops the run here,
// before anything is cordoned. An upgrade binary from before this step has no
// --import-images and would refuse it, so the run warns and goes on without
// the step, as it did before there was one.
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
		// Confirm, not Err alone: the import changes the host, and a batch can
		// start minutes after the leases were taken, as upgradeOneNode's can.
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

// planState is what the rollout will do with n, as the plan prints it.
func planState(n Node, target string) string {
	switch {
	case n.Interrupted() == target:
		return "finish (interrupted after its kubelet swap)"
	// Before the build check: a NotReady node cannot run the check's pod, and
	// upgradeOneNode leaves it alone. Preflight has already
	// refused this for a control plane without margin.
	case n.NeedsBuildCheck(target) && !n.Ready:
		return "skip (NotReady: its build cannot be checked; the run ends with an error naming it)"
	case n.NeedsBuildCheck(target):
		return "check build (not drained if it already runs this build)"
	}
	return "upgrade"
}

// nodesToUpgrade splits the nodes a cluster upgrade visits into
// control planes and workers, keeping their order. That is every node: one
// behind target, or at it by an upgrade that never finished, is
// upgraded; one at target by a finished upgrade may still run another stratum
// build of the same Kubernetes release, and upgradeOneNode asks it before
// draining anything.
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

// lockedWriter serializes Write calls on a writer that concurrent goroutines
// share. The cluster upgrade's worker fan-out hands one writer to up to
// WorkersMaxUnavailable upgradeOneNode goroutines at once; the CLI's os.Stdout
// tolerates concurrent writers, the MCP handler's buffer does not, so the
// operation makes the fan-out safe for either by locking at this boundary.
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

// runBounded runs fn for each item, at most limit concurrently. On the first error
// it stops LAUNCHING new work but lets already-started fns finish (a half-done node
// upgrade must never be abandoned mid-flight), then returns the first error seen.
// limit < 1 is treated as 1 (fully serial) — so --workers-max-unavailable=1
// behaves exactly like a serial loop.
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
		// Acquire a slot FIRST, then check for a prior failure. A freed slot means a
		// previous fn has returned (its error already recorded), so checking after the
		// acquire — not before — is what guarantees no new work launches once anything
		// has failed; checking before would race a just-finished failure.
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

	// The leases first: a run that has lost either must touch nothing, not
	// even a read, so this stays the first thing upgradeOneNode does. Confirm,
	// not Err alone: Err only reflects the renewals the background loops have
	// made so far, and the gap since the previous node's confirm can hold a
	// takeover neither loop has ticked over yet. Retried, so a renewal riding
	// out a bounded API-server gap does not stop a run that still holds both
	// leases; a takeover fails terminally on the first attempt.
	if err := retryAPI(ctx, o.leaseConfirm); err != nil {
		return nodeUpgraded, err
	}

	// Again here, not only in the cluster-wide pass above: the gap between
	// that pass and this node's Job being created is the window an unfenced
	// older run could use. lockNode is what fences a Job created in it.
	if err := clearStaleUpgradeJobs(w, ctx, cs, n.Name); err != nil {
		return nodeUpgraded, err
	}
	// At target by a finished upgrade: only the stratum build can differ, and
	// only the node can tell. Ask before the cordon, so a node already on this
	// build is neither drained nor reported upgraded.
	if n.NeedsBuildCheck(o.Target) {
		// A NotReady node cannot start the check's pod; waiting out its
		// deadline would only halt the whole run over a node that may need
		// nothing. It is left alone and named at the end.
		// A control plane gets here only when Preflight has found margin for
		// it to be down while another is drained.
		if !n.Ready {
			fmt.Fprintf(w, "  WARN: %s is at %s but NotReady; its stratum build cannot be checked, so it is left alone\n", n.Name, o.Target)
			return nodeUnchecked, nil
		}
		k8sutil.Step(w, fmt.Sprintf("Checking %s (%s): at %s; comparing its stratum build with the upgrade binary", n.Name, role, o.Target))
	} else {
		k8sutil.Step(w, fmt.Sprintf("Upgrading %s (%s) -> %s", n.Name, role, o.Target))
	}
	// Every Ready node is asked before its cordon, not only one at target: the
	// check is also where the node's staged binary is held to the rollout's
	// pin, and a mismatch found by the upgrade Job instead would come after the
	// drain, leaving the node cordoned. A node behind target
	// or mid-upgrade answers "needs the upgrade". A NotReady one behind target
	// keeps the old path: its check pod could not start.
	if n.Ready {
		current, err := nodeRunsThisBuild(ctx, w, cs, n, o)
		if err != nil {
			return nodeUpgraded, fmt.Errorf("check %s's stratum build: %w (not drained)", n.Name, err)
		}
		// Not a node with an upgrade to this target recorded and never
		// stamped: its node-side Job may have finished while this orchestrator
		// died before stampVersion, so the node answers "current" -- and
		// returning here would leave the in-progress annotation, which refuses
		// every later upgrade to another target, and the cordon. It takes the
		// full path instead, where the Job finds nothing to do and the gates,
		// stamp and uncordon finish what the earlier run started.
		//
		// The annotation alone decides, not Interrupted(): that also needs the
		// API's kubelet version at target, and the Node status can still show
		// the old one after the kubelet swap.
		switch {
		case current && n.InProgress != o.Target:
			// Leaving it alone must not leave the upgrade's own cordon behind;
			// not the full path, which would drain a node already
			// on the target build.
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
	// After the cordon, so no replica can land here while this decides
	// (corednsdrain.go). Released once the drain ends.
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
	// Bounded: an unsatisfiable PDB (e.g. minAvailable == replicas) would
	// otherwise retry forever with the node already cordoned.
	drainDeadline := time.Now().Add(drainTimeout)
	for {
		// Every eviction attempt, not just the first: a drain waiting out a
		// PDB can take minutes, and a lease can be lost meanwhile.
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
	// Recorded before the Job and cleared only by finishNode below, so a
	// Job that dies after the kubelet swap leaves the node planned as
	// unfinished rather than "already-at-target".
	// Every attempt confirms both leases and is refused a node another run claimed.
	if err := retryAPI(ctx, func(ctx context.Context) error {
		if err := o.leaseConfirm(ctx); err != nil {
			return err
		}
		return markUpgradeInProgress(ctx, cs, n.Name, o.Target, o.lease.id())
	}); err != nil {
		return nodeUpgraded, fmt.Errorf("record the upgrade on %s: %w (drained and left cordoned, not upgraded)", n.Name, err)
	}
	// A failed Job leaves the node cordoned for inspection, unless a
	// controller's Job uncordoned it before failing. Its log, not the Job's
	// status, says whether the node rolled back.
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
		// Admitted under node pressure: the node is drained by now, so what
		// a critical pod may evict is only what the drain left (DaemonSets).
		// The pre-drain check is not critical; evicted, it fails closed.
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
	// The leases again, and by renewing them, on every attempt of the write that
	// stamps the node and undoes its cordon: the Job, the Ready wait and the gate
	// can span an API outage long enough for a lease to expire and a successor
	// to take over, and so can the retries themselves. Err alone only reflects
	// the renewals made so far. The write refuses a node whose claim names
	// another run, which closes what is left between the renewal and the write.
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

// recoverySteps is what to do when a node's run halts after the drain. The
// per-node Job runs on the host and does not stop with this process, so it may
// still be upgrading the node. A controller's Job also uncordons the node once
// its upgrade passes, which can be before this run's own gates fail or after
// it halts, so only a worker is said to be left cordoned.
func recoverySteps(n Node, job string, o ClusterOptions) string {
	// The --from binary runs every command, kubectl included: the node has no
	// kubectl on its PATH, and a bare `stratum` may be an older release.
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

// shellQuote is s as one shell word: unchanged when it is made only of
// characters a shell leaves alone, else single-quoted.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// legacyJobName is the name this orchestrator used before jobName began
// hashing: a plain truncation, which two long node names sharing a prefix
// could collide on.
func legacyJobName(node string) string {
	n := "stratum-upgrade-" + node
	if len(n) > maxJobNameLen {
		n = n[:maxJobNameLen]
	}
	return n
}

// clearStaleUpgradeJobs removes finished upgrade Jobs for one node that are
// not the Job this run is about to create, and refuses if any is still
// running.
//
// Selected by label rather than by name: nodejob.Run only ever sees the name
// it is about to create, so a Job an older binary left under the previous
// truncated name is invisible to it. Selecting on the node's label value finds
// every Job targeting this node whatever named it, which also means this does
// not depend on legacyJobName still matching whatever that older binary used.
//
// This narrows a race it cannot close. A binary old enough to use the previous
// naming is also old enough to predate the cluster-upgrade lease, so nothing
// here fences it: it can create its Job after this returns. The fence is on the
// node instead (lockNode): two upgrade Jobs that land on one node meet
// at its lock whatever created them, and the second refuses before changing
// anything. That binds every node binary that takes the lock; one that
// predates the lock takes nothing, so running an older Stratum's
// `upgrade --cluster` beside this one is still not supported.
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

// nodeUpgradeArgv builds the exact argv the per-node upgrade Job runs (via
// nodejob.Run): nsenter PID1, then the per-node primitive
// `stratum upgrade --from <upgrade-binary>`. The upgrade binary must be host-staged.
//
// It is deliberately shell-free. nsenter execs the stratum binary directly, so
// every value below is a discrete argv element that no interpreter ever
// re-parses — o.Target and o.From cannot break out of their own argument no
// matter what they contain. The previous form built one string and handed it to
// `/bin/bash -c`, which made both values executable text on EVERY node in the
// cluster, as root, in the host namespaces.
//
// Keep it that way: this payload has no pipes, redirections, globs or variable
// expansions, so it has never needed a shell. If a future change genuinely
// needs shell features, pass the values in as environment variables and read
// them quoted, rather than re-introducing interpolation here.
//
// The binary exec'd is o.From itself — the staged upgrade binary, not the
// installed one: UpgradeCluster requires --from precisely because that staged
// path is the one location guaranteed to hold the new binary (and, via its
// payload, everything else) on every node, worker included.
func nodeUpgradeArgv(o ClusterOptions, n Node) []string {
	argv := append(stagedUpgradeArgv(o, n), "--soak", strconv.Itoa(o.SoakSec))
	if o.SkipSnapshot {
		argv = append(argv, "--skip-snapshot")
	}
	return argv
}

// stagedUpgradeArgv is the exec of the staged upgrade binary that both the
// upgrade and its --check start from, so the check can never answer for a
// different binary or target than the upgrade acts on.
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
	// Exact bytes only where they can match: a node of another architecture
	// runs another binary of the same build.
	if o.expectSHA256 != "" && n.Arch == o.arch {
		argv = append(argv, "--expect-sha256", o.expectSHA256)
	}
	return argv
}

// verifyStagedScript checks the staged binary on the node that is about to
// run it, then execs it. Its arguments are the binary's path, the uid that
// must own it and its directory, the sha256 it must have (empty to skip), and
// then the command to exec. They are positional parameters, so no path or
// value is ever parsed as shell.
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

// preflightNodePrereqs runs the staged binary's --check on every Ready node
// before the rollout cordons or drains anything, and refuses, naming every
// failing node at once, when any would take the upgrade without
// container-selinux. The Job changes nothing on the host, so a refusal here
// leaves the cluster exactly as it was. Serially, not runBounded: the point
// is to collect every refusing node before the first failure stops the run,
// and the pass is one Job per node, not a drain worth overlapping. A NotReady
// node cannot start the Job's pod, so it is not checked up front; the pass
// warns about it and goes on. A check that could not ask its question (the
// node's settings unreadable, or a policy tool that failed) still lets the
// node through, but only with a WARN line in the Job's log, which the Job's
// deletion would lose: the pass relays each one, naming the node, before
// anything is drained. So does a node at the target whose supervisor an
// earlier handoff could not replace: its check passes it as current, with a
// WARN saying how to restart that supervisor. A log that cannot be read loses
// only those lines: the Job's exit status still decides, and the pass warns
// that they are missing.
func preflightNodePrereqs(ctx context.Context, w io.Writer, cs kubernetes.Interface, ordered []Node, o ClusterOptions) error {
	var missing []string
	for _, n := range ordered {
		if err := o.leaseErr(); err != nil {
			return err
		}
		if !n.Ready {
			from := shellQuote(o.From)
			fmt.Fprintf(w, "  WARN: %s is NotReady, so the SELinux prerequisite check cannot run on it; check it with sudo %s upgrade --from %s --check\n", n.Name, from, from)
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
			// The exit status still answers: an untrusted kubelet serving
			// certificate, which the upgrade itself repairs, stops only the log.
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
			// The pass runs the build check's own --check, so its other
			// answers -- a staged binary that is not the pinned one, another
			// upgrade on the node -- are build-check refusals, worded as
			// upgradeOneNode words them.
			return fmt.Errorf("check %s's stratum build: the check exited %d (logs: kubectl -n %s logs job/%s); nothing was cordoned or drained",
				n.Name, code, cuNamespace, jobName(n.Name))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("preflight: node(s) %s: each failed the SELinux prerequisite `stratum up` enforces "+selinuxPrereqDetail+". "+
			"Nothing was cordoned or drained. Run sudo %s upgrade --from %s --check on each node for its exact reason",
			strings.Join(missing, ", "), shellQuote(o.From), shellQuote(o.From))
	}
	return nil
}

// nodeRunsThisBuild asks node, with a Job running the upgrade binary's
// --check, whether it already runs this version and build. The Job changes
// nothing on the host, so it needs neither a drain nor a cordon.
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
	// Again after the Job: it can run for two minutes, and a lease can be
	// lost meanwhile. A cordon and a drain come next.
	if err := o.leaseErr(); err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case ExitUpgradeNeeded:
		return false, nil
	case ExitPrereqMissing:
		return false, fmt.Errorf("%w: the node failed the SELinux prerequisite `stratum up` enforces "+selinuxPrereqDetail+"; the node was not drained. Run sudo %s upgrade --from %s --check on it for its exact reason (logs: kubectl -n %s logs job/%s)",
			ErrPrereqMissing, shellQuote(o.From), shellQuote(o.From), cuNamespace, jobName(n.Name))
	default:
		return false, fmt.Errorf("build check exited %d (logs: kubectl -n %s logs job/%s)", code, cuNamespace, jobName(n.Name))
	}
}

// runNodeJob is nodejob.Run for the upgrade Job; a var so a test can stop a
// node's upgrade at the host-changing step and see what led up to it.
var runNodeJob = nodejob.Run

// runExitCode is nodejob.RunExitCode; a var so a test can answer for a node.
var runExitCode = nodejob.RunExitCode

// runExitCodeLogs is nodejob.RunExitCodeLogs, for the prerequisite pass that
// relays the check's warnings; a var so a test can answer for a node.
var runExitCodeLogs = nodejob.RunExitCodeLogs

// runImageImport is nodejob.Run for the image import Job; a var so a test can
// answer for a node.
var runImageImport = nodejob.Run

// stagedImportsImages reports whether the upgrade binary at path has the image
// import step. With it, `upgrade --import-images --help` prints the usage and
// changes nothing; a build without it rejects the flag as unknown before it
// reads --help. Only that answer means "no": a probe that fails any other way
// (a timeout, a signal, a binary that cannot run) is an error, since skipping
// the import on a binary that has it would leave nodes without their images.
// Every node runs the same build (the rollout pins it), so this node's copy
// answers for all of them. A var for tests.
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

// nodeCheckArgv is nodeUpgradeArgv's read-only twin: the same exec of the
// staged binary, asking --check instead of upgrading.
func nodeCheckArgv(o ClusterOptions, n Node) []string {
	return append(stagedUpgradeArgv(o, n), "--check")
}

// nodeImportImagesArgv is the same exec of the staged binary, asking it to import
// its images and nothing else.
func nodeImportImagesArgv(o ClusterOptions, n Node) []string {
	return append(stagedUpgradeArgv(o, n), "--import-images")
}

func jobName(node string) string {
	return nodejob.TruncName("stratum-upgrade-" + node)
}

// validateUpgradeJobNames catches collisions that hashing alone cannot prevent:
// a long node's hashed Job name can also be a valid short node name. Check the
// full cluster before any node is cordoned, including nodes already at target,
// whose earlier upgrade Jobs may still exist.
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

package cluster

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/operator"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// RunController prepares (unless already prepared by a join) and supervises a
// controller until ctx is cancelled.
// processes is the roster seam: a var like pkg/cli's prepareControllerFn, so
// a test can substitute the supervised children (whose real gates dial this
// host's containerd socket) while RunController's own flow — the supervisor,
// its OnStart hook, the run group and the first-start consume/restore — runs
// for real.
var processes = Processes

func RunController(ctx context.Context, cfg *ClusterConfig, st *NodeState) error {
	if err := recordConfigPath(cfg, st, SaveState); err != nil {
		// Best-effort: it only lets the CLI and MCP find a non-default config.
		fmt.Fprintf(os.Stderr, "stratum: WARN: could not record the cluster config path: %v\n", err)
	}
	if err := writeBundles(cfg, st); err != nil {
		return err
	}
	if err := writeCoreDNSUpstream(st); err != nil {
		return fmt.Errorf("write the CoreDNS upstream: %w", err)
	}
	// The control plane is static pod manifests; kubelet launches
	// them once the supervisor brings it up. Rendered before the supervisor
	// starts so a kubelet that is already running picks changes up immediately.
	// Keep it before supervisor.Run: upgrade and rollback treat an answering
	// supervisor as proof this write is done (ensureSupervisor).
	if err := staticpod.WriteAll(st); err != nil {
		return err
	}
	procs := processes(st, cfg.OperatorEnabled())
	if err := writeComponentArgs(procs); err != nil {
		return err
	}

	// Post-start housekeeping: label/stamp the node, then the long-lived
	// cert-renew, snapshot and etcd-growth-recorder loops. The run group owns
	// every task for the controller's whole run — each one is panic-recovered
	// and backoff-restarted, and none is abandoned mid-flight at shutdown. The
	// tasks park on the group's context, so the group gets one derived from the
	// controller's that RunController cancels itself: supervisor.Run can also
	// return early on a failure (a log dir it cannot create, a control socket
	// it cannot bind) while the controller context is still live, and nothing
	// else would ever end the parked tasks then.
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tasks := newRunGroup(gctx)
	postStart(tasks, cfg, st)

	configPath := cfg.path
	if configPath == "" {
		configPath = DefaultConfigPath
	}
	onStart := managedKubeletOnStart(tasks, st.Hostname, func(ctx context.Context, hostname string) {
		restampWhenRegistered(ctx, hostname)
		if nodeRegistered(ctx, hostname) {
			labelOwnNode(ctx, st)
		}
	})
	err := runSupervisor(ctx, procs, supervisor.Options{
		LogDir:        version.LogDir,
		ControlSocket: version.ControlSocket,
		OnStart:       onStart,
		ReExec:        UpArgv(version.StratumInstallPath, false, configPath, cfg.ProfileName()),
	})
	// End the tasks' context now that supervisor.Run has returned: a no-op on
	// the normal shutdown path, where the parent context is already done by
	// the time Run does, and on the early-failure path the thing that frees
	// the tasks parked on it.
	cancel()
	// This wait extends the supervisor's join to the in-process tasks, so
	// RunController returns with nothing it started still in flight.
	tasks.wait()
	return err
}

// UpArgv is the steady-state `stratum up` command line run from exe: what the
// reboot-persistence unit runs, and what a supervisor re-execs into after an
// upgrade. It never carries --join/--token/--certificate-key: those are
// one-shot join credentials, valid only for the join that consumed them. A
// worker's restart needs nothing but --worker, because PrepareWorker's restart path
// resumes from the persisted NodeState, its profile included. A control
// plane's carries its profile, so a restart keeps the one the node was
// brought up with.
//
// The cis profile is spelled --cis, not --profile cis: an upgrade rollback
// restores the previous binary and re-execs it with this argv, and a binary
// that predates --profile rejects the flag it does not know, which would leave
// a rolled-back CIS control plane unable to start. Every binary reads --cis.
// Any other profile is spelled --profile <name>; no earlier binary could have
// run it.
func UpArgv(exe string, worker bool, configPath string, profileName string) []string {
	argv := []string{exe, "up"}
	if worker {
		return append(argv, "--worker")
	}
	argv = append(argv, "--config", configPath)
	switch profileName {
	case "":
	case nodestate.CISProfile:
		argv = append(argv, "--cis")
	default:
		argv = append(argv, "--profile", profileName)
	}
	return argv
}

// prepareWorkerKubelet is a worker start's only kubelet.conf render, and its
// CoreDNS upstream. kubelet.conf before the state: a state saved ahead of a
// kubelet.conf that failed to write would read as already learned on the next
// start, which would never rewrite it.
func prepareWorkerKubelet(st *NodeState) error {
	if err := writeCoreDNSUpstream(st); err != nil {
		return fmt.Errorf("write the CoreDNS upstream: %w", err)
	}
	learned := learnClusterDNS(st)
	if err := writeKubeletConf(st); err != nil {
		return fmt.Errorf("write kubelet.conf: %w", err)
	}
	if learned {
		// The DNS address is this save's one field, recorded through the
		// mutation owner against the state on disk: the kubelet.conf render
		// above can take long enough for another writer to land a field the
		// whole-file save of the state this start loaded would drop.
		if err := nodestate.MutateSave(func(cur *NodeState) error { cur.ClusterDNSIP = st.ClusterDNSIP; return nil }, saveState); err != nil {
			return fmt.Errorf("record the cluster DNS address %s: %w", st.ClusterDNSIP, err)
		}
	}
	return nil
}

// RunWorker supervises a worker node (containerd + kubelet).
func RunWorker(ctx context.Context, st *NodeState) error {
	// Ensure the containerd config exists (and reap a legacy host config)
	// unconditionally, before the supervisor launches containerd against
	// version.ContainerdConfig. A worker's first start is its join, and a
	// restarted worker reuses persisted state, so this is the one place both
	// get it. The config lives at a stratum-owned path, so a
	// worker upgraded from a build that wrote /etc/containerd/config.toml would
	// otherwise start containerd against a path that was never written — and its
	// legacy host config would never be healed. EnsureConfig is idempotent.
	//
	// The render reports whether it changed the config and restarts nothing;
	// this start owns no restart: preflight has already refused a node with a
	// running containerd, so the supervisor below brings containerd up on the
	// config just written. A changed config reaches a running containerd only
	// through whoever owns it — `stratum registry apply` (runtime.ApplyRegistry)
	// or the in-place upgrade's own host reload.
	if _, err := runtime.EnsureContainerdConfigs(os.Stdout, st); err != nil {
		return fmt.Errorf("prepare containerd: %w", err)
	}
	if err := prepareWorkerKubelet(st); err != nil {
		return err
	}
	// A worker never runs the operator, so the switch is off.
	procs := Processes(st, false)
	if err := writeComponentArgs(procs); err != nil {
		return err
	}
	return runSupervisor(ctx, procs, supervisor.Options{
		LogDir:        version.LogDir,
		ControlSocket: version.ControlSocket,
		// No admin.conf here: the kubelet stamps its own Node.
		OnStart: kubeletOnStart(st.Hostname, func(hostname string) {
			if restampOwnNodeWhenRegistered(hostname) {
				labelOwnNode(context.Background(), st)
			}
		}),
		ReExec: UpArgv(version.StratumInstallPath, true, "", ""),
	})
}

// writeBundles stages the versioned add-on manifests into the deployer's
// directory: the operator server-side-applies each stack and prunes what a
// release removes. Files are only rewritten on content change so the
// deployer's change detection stays quiet across restarts.
func writeBundles(cfg *ClusterConfig, st *NodeState) error {
	if err := refuseOrphaningTheDashboard(cfg); err != nil {
		return err
	}
	// Cilium's apiserver address is left as a sentinel: the operator fills it
	// in at apply time from the cluster's control planes, so every control
	// plane stages the same bytes and a leader change re-applies nothing
	// (addons.APIServerFor has the why). The four per-cluster inputs — the
	// CoreDNS pair, Cilium's dual-stack toggle and the local-path default
	// claim — are staged as sentinels too (addons.RenderCoreDNS,
	// RenderCiliumStaged, RenderLocalPath), for the deployer to fill at apply
	// time from the cluster-inputs record this staging writes: the applied
	// bytes are the cluster's, not the staging node's.
	stacks := map[string][]byte{
		"crds/stratum-crds.yaml":          addons.RenderHelmCRDs(),
		"node-mark/node-mark-policy.yaml": addons.RenderNodeMarkPolicy(),
		"cilium/cilium.yaml":              addons.RenderCiliumStaged(cfg.Spec.Network.Cilium.Settings()),
		"coredns/coredns.yaml":            addons.RenderCoreDNS(),
	}
	if cfg.Spec.Storage.Default == addons.StorageLocalPath {
		stacks[localPathFile] = addons.RenderLocalPath()
	}
	if cfg.DashboardEnabled() {
		stacks[dashboardFile] = addons.RenderDashboard(cfg.DashboardSettings())
	}
	if len(cfg.Spec.RuntimeClasses) > 0 {
		stacks[runtimeClassFile] = addons.RenderRuntimeClasses(cfg.Spec.RuntimeClasses)
	}
	imported, err := renderImporterStacks(st)
	if err != nil {
		return err
	}
	for rel, content := range imported {
		stacks[rel] = content
	}

	for rel, content := range stacks {
		dst := filepath.Join(version.ManifestsDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if cur, err := os.ReadFile(dst); err == nil && string(cur) == string(content) {
			continue
		}
		// The shared atomic writer, not os.WriteFile: the deployer
		// server-side-applies these manifests, so a crash mid-write must
		// never leave a torn one where a working one was, and a replaced
		// manifest must land at the declared mode (os.WriteFile applies perm
		// on create only, the atomic writer on every write).
		if err := host.WriteFileAtomically(dst, content, 0o644); err != nil {
			return err
		}
	}
	if err := removeStaleBundledFiles(stacks); err != nil {
		return err
	}
	if err := removeStaleImporterFiles(imported); err != nil {
		return err
	}

	// Publish the staged generation and sentinel inputs together. Cold first
	// boot cannot reach the API yet; postStart retries the persisted request.
	recordDesiredBundles(cfg, st, stacks)

	return saveCiliumSettings(cfg.Spec.Network.Cilium.Settings())
}

const (
	localPathFile    = "local-path/local-path.yaml"
	dashboardFile    = addons.DashboardStack + "/dashboard.yaml"
	runtimeClassFile = addons.RuntimeClassStack + "/runtime-classes.yaml"
)

// refuseOrphaningTheDashboard refuses spec.operator: false while the dashboard
// stack is still staged: with no operator left, nothing would prune it or renew
// its certificate.
func refuseOrphaningTheDashboard(cfg *ClusterConfig) error {
	if cfg.OperatorEnabled() {
		return nil
	}
	if _, err := os.Stat(filepath.Join(version.ManifestsDir, dashboardFile)); err != nil {
		return nil
	}
	return errors.New("spec.operator: false would leave the dashboard running with nothing to prune it or renew its certificate. " +
		"Set spec.dashboard.enabled: false and restart Stratum with the operator still on, wait until " +
		"`stratum kubectl get ns " + addons.DashboardNamespace + "` reports NotFound, then turn the operator off")
}

// removeStaleBundledFiles deletes files an earlier start staged under the
// bundled stacks and this one did not render. writeBundles only rewrites, so
// without this a bundle the config no longer asks for — local-path after
// spec.storage.default moves off it — would sit in the manifests directory
// forever: nothing applies it, its recorded objects never prune, and the
// deployer's digest gate would refuse the whole tree against a record that no
// longer renders it. Only the bundled stacks are cleaned: they are the
// staging's own namespace (an importer may not name one, and a loose file or
// a hand-dropped stack directory is the operator's drop-a-file contract,
// never the staging's to delete). An emptied stack directory goes with its
// files, like removeStaleImporterFiles's.
func removeStaleBundledFiles(stacks map[string][]byte) error {
	for _, stack := range bundledStacks {
		var files []string
		for _, pat := range []string{"*.yaml", "*.yml"} {
			matches, err := filepath.Glob(filepath.Join(version.ManifestsDir, stack, pat))
			if err != nil {
				return err
			}
			files = append(files, matches...)
		}
		for _, file := range files {
			rel, err := filepath.Rel(version.ManifestsDir, file)
			if err != nil {
				return err
			}
			if _, keep := stacks[rel]; keep {
				continue
			}
			if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("remove the dropped bundle file %s: %w", file, err)
			}
			_ = os.Remove(filepath.Dir(file))
		}
	}
	return nil
}

// desiredRecordBudget bounds one desired-generation recording. The recording
// is best-effort (see recordDesiredBundles), so `stratum up` never waits long
// on an apiserver that is not there yet.
const desiredRecordBudget = 15 * time.Second

// recordDesiredBundles publishes the rendered bytes and sentinel inputs as one
// generation. A pending request survives a cold first boot until postStart can
// reach the apiserver; an unchanged restart preserves existing cluster intent.
func recordDesiredBundles(cfg *ClusterConfig, st *NodeState, stacks map[string][]byte) {
	if err := stageDesiredBundles(cfg, st, stacks); err != nil {
		fmt.Fprintf(os.Stderr, "stratum: WARN: record the staged bundle generation: %v (the post-start task retries; existing cluster intent is preserved)\n", err)
	}
}

// renderedStacks names the stack directories a rendered bundle set covers: the
// first path component of every bundle — "cilium" from "cilium/cilium.yaml".
// The desired record carries them beside the digest, so the deployer's gate
// digests only the stacks the staging owns and leaves a hand-dropped stack
// directory out of it.
func renderedStacks(bundles map[string][]byte) []string {
	seen := make(map[string]bool, len(bundles))
	var out []string
	for rel := range bundles {
		stack, _, ok := strings.Cut(rel, "/")
		if !ok || seen[stack] {
			continue
		}
		seen[stack] = true
		out = append(out, stack)
	}
	return out
}

// clusterInputs derives the cluster-inputs document from the cluster config
// and the node state: the four values every control plane's deployer resolves
// the apply-time sentinels from.
func clusterInputs(cfg *ClusterConfig, st *NodeState) (operator.ClusterInputs, error) {
	dnsIP, err := runtime.ClusterDNSIP(st)
	if err != nil {
		return operator.ClusterInputs{}, err
	}
	return operator.ClusterInputs{
		DNSAddress:     dnsIP,
		ClusterDomain:  st.Domain(),
		DualStack:      st.DualStack,
		StorageDefault: cfg.Spec.Storage.Default,
	}, nil
}

// writeComponentArgs records each component's rendered argv under
// /var/lib/stratum/config/components/ — the on-disk truth of what each
// component runs (the supervisor has no readable unit files to audit).
func writeComponentArgs(procs []supervisor.Process) error {
	dir := filepath.Join(version.K8sConfDir, "components")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, p := range procs {
		args := p.Args
		if p.ArgsFn != nil {
			args = p.ArgsFn()
		}
		body := p.Path + " \\\n  " + strings.Join(args, " \\\n  ") + "\n"
		// The shared atomic writer: these files are the on-disk truth of each
		// component's rendered argv, so a torn .args file would record an argv
		// the component does not run.
		if err := host.WriteFileAtomically(filepath.Join(dir, p.Name+".args"), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// startEtcdGrowthRecorder, the four loop seams and the node-registered
// task's seams below are behind seams so a test can run postStart without
// an apiserver or a host, see what it starts, and drive a loop's ticks by
// sleeping instead of waiting out a day of renewal or a whole snapshot
// interval: certRenewInterval and snapshotInterval carry each loop's
// cadence, certRenewRotate and snapshotSave its per-tick work. There is
// deliberately no wait-API seam here: every task checks its own
// precondition per tick, and run_test.go's source pin holds that no gate
// comes back in front of them.
// runSupervisor is behind one so a test can stand where the supervisor
// stands — after every render, before any process starts — and see what a
// start path has written without running processes.
var (
	// startEtcdGrowthRecorder runs recordEtcdGrowth and then holds until
	// shutdown: the run group restarts a task that returns before the
	// context ends, right for a loop but wrong for the recorder, whose job
	// is one pass — returning would re-run a finished job every backoff.
	startEtcdGrowthRecorder = func(ctx context.Context) {
		recordEtcdGrowth(ctx)
		<-ctx.Done()
	}

	runSupervisor = supervisor.Run
	// Keep the daemon's warning delivery testable without rotating host PKI.
	renewCertificates = certs.RotateContext

	certRenewInterval = 24 * time.Hour
	certRenewRotate   = func(ctx context.Context) error {
		// Quiet suppresses routine progress, but restart and endpoint warnings
		// still belong in the daemon's log, including when rotation succeeds.
		_, err := renewCertificates(ctx, os.Stdout, certs.Options{IfExpiringDays: version.CertRenewWindowDays, Quiet: true})
		return err
	}
	snapshotInterval = func(c *ClusterConfig) time.Duration {
		return time.Duration(c.Spec.Snapshot.IntervalHours) * time.Hour
	}
	snapshotSave = func(ctx context.Context, c *ClusterConfig) error {
		// The save's prune warnings are the daemon's log: the controller's
		// stdout is that log, so the writer is os.Stdout, passed explicitly.
		opts := scheduledSaveOptions(c, etcdsnap.LoadConfigOr, os.Stdout)
		_, err := etcdsnap.SaveContext(ctx, os.Stdout, opts)
		return err
	}

	// The node-registered task's seams: nodeRegistered is ONE registration
	// check — a single attempt of k8sutil.WaitNodeRegistered, whose poll is
	// the one kubectl get node in the tree — no retry loop inside, because
	// the tick is the poll — labelControlPlane and stampNode are the two
	// writes the tick that finds the node registered makes, both ctx-bounded
	// so shutdown cuts them off instead of waiting a hung apiserver out,
	// nodeRegistrationInterval is the tick cadence and nodeRegistrationGiveUp
	// the ONE give-up budget: the documented policy for every
	// node-registration wait in the tree. Nothing beside the controller —
	// the kubelet-start restamp, postStart's label-and-stamp, pkg/cli's
	// joined-member labelling — may run its own registration wait or carry
	// its own give-up point; the node-registered task holds both.
	nodeRegistrationInterval = 3 * time.Second
	nodeRegistrationGiveUp   = 10 * time.Minute
	nodeRegistered           = func(ctx context.Context, hostname string) bool {
		return k8sutil.WaitNodeRegistered(ctx, hostname, 1) == nil
	}
	labelControlPlane = k8sutil.LabelControlPlaneContext

	// reserveNodeClientBinding empties the upstream node-client auto-approval
	// binding; nodeClientBindingRetry paces its retries until the apiserver
	// answers.
	reserveNodeClientBinding = func(ctx context.Context) error {
		cs, err := k8sutil.AdminClientset()
		if err != nil {
			return err
		}
		return k8sutil.DisableNodeClientAutoApprove(ctx, cs.RbacV1().ClusterRoleBindings())
	}
	nodeClientBindingRetry = 30 * time.Second
	// bindingWarnAfter is how long a binding task fails before it warns.
	bindingWarnAfter = 5 * time.Minute
	// clusterAdminsRetry paces the cluster-admins binding's retries: admin.conf
	// answers 403 until it lands.
	clusterAdminsRetry = 2 * time.Second
	// clusterAdminsResync re-runs the cluster-admins binding after a success:
	// a snapshot restore replaces the marker under a task that does not restart.
	clusterAdminsResync = time.Minute

	// ensureKubeletAPIAdminBinding binds apiserver-kubelet-client's user to
	// system:kubelet-api-admin.
	ensureKubeletAPIAdminBinding = func(ctx context.Context) error {
		cs, err := k8sutil.AdminClientset()
		if err != nil {
			return err
		}
		return k8sutil.EnsureKubeletAPIAdminBinding(ctx, cs.RbacV1().ClusterRoleBindings())
	}

	// ensureClusterAdminsBinding gives admin.conf's group cluster-admin, once
	// per cluster, holder being this controller's node name.
	ensureClusterAdminsBinding = func(ctx context.Context, holder string) error {
		cs, err := k8sutil.AdminClientset()
		if err != nil {
			return err
		}
		return k8sutil.EnsureClusterAdminsBinding(ctx, cs, holder)
	}

	// ensureKubeVipRBAC lets kube-vip's user hold its election lease, through
	// the local apiserver because admin.conf may name the VIP kube-vip holds.
	ensureKubeVipRBAC = func(ctx context.Context) error {
		cs, err := k8sutil.LocalAdminClientset()
		if err != nil {
			return err
		}
		return k8sutil.EnsureKubeVipRBAC(ctx, cs)
	}
	kubeVipRBACResync = 5 * time.Minute

	// leafSubjectDrift lists this node's leaves whose subject this release no
	// longer issues; rotateLeaves reissues every leaf and restarts the control
	// plane onto them.
	leafSubjectDrift = certs.SubjectDrift
	rotateLeaves     = func(ctx context.Context) error {
		// Quiet suppresses routine progress, but restart and endpoint warnings
		// still belong in the daemon's log. Nothing-to-rotate is a fine
		// outcome here, so the rotated flag goes unread.
		_, err := certs.RotateContext(ctx, os.Stdout, certs.Options{Quiet: true, KubeVipRequired: true})
		return err
	}
	// leafReissueBackoff is the first wait after a leaf reissue fails for a
	// reason other than the maintenance lease being held, doubling on each
	// failure up to leafReissueBackoffMax: every attempt restarts the
	// control plane, so a component that will not come back must not bounce
	// etcd and the apiserver every nodeClientBindingRetry.
	leafReissueBackoff    = time.Minute
	leafReissueBackoffMax = 30 * time.Minute

	// stampNode re-asserts the node's version annotations; non-fatal, but
	// logged. Both controller registration and restamping use the run group's
	// context so shutdown cancels and joins their API calls.
	stampNode = func(ctx context.Context, hostname string) {
		if err := k8sutil.StampNodeContext(ctx, hostname); err != nil {
			fmt.Fprintf(os.Stderr, "stratum: stamp version on %s: %v (non-fatal)\n", hostname, err)
		}
	}

	// labelOwnNode gives this node's Node its spec.node labels and the label
	// of each runtime handler it declares, as its kubelet, on every kubelet
	// start of both roles; non-fatal, but logged.
	labelOwnNode = func(ctx context.Context, st *NodeState) {
		handlers := slices.Sorted(maps.Keys(st.RuntimeHandlers))
		if err := k8sutil.ReconcileOwnNodeLabels(ctx, st.Hostname, st.NodeLabels, handlers); err != nil {
			fmt.Fprintf(os.Stderr, "stratum: label %s: %v (non-fatal)\n", st.Hostname, err)
		}
	}
)

// kubeletOnStart is the supervisor's OnStart hook, for a controller and a
// worker alike: every kubelet start, including the restart `stratum upgrade`
// triggers in a still-running supervisor, calls restamp, which is the role's
// own (a controller's through admin.conf, a worker's through its kubelet
// credential). It runs in the background, and the role's restamp decides what
// it needs before stamping: a worker's waits out its TLS bootstrap and
// registration, a controller's checks registration once — the wait for a
// first registration belongs to the node-registered task. At most one
// restamp runs at a time: a crash-looping kubelet would otherwise stack up
// overlapping restamps, so a start during one drops out and the one in flight
// stamps.
func kubeletOnStart(hostname string, restamp func(hostname string)) func(string, int) {
	var running atomic.Bool
	return func(name string, pid int) {
		fmt.Printf("stratum: started %s (pid %d)\n", name, pid)
		if name == "kubelet" && running.CompareAndSwap(false, true) {
			go func() {
				defer running.Store(false)
				restamp(hostname)
			}()
		}
	}
}

// managedKubeletOnStart keeps controller restamps under the run group's panic
// recovery and shutdown join. Starts while a restamp is queued or in progress
// are coalesced; the supervisor hook itself never waits for the API.
func managedKubeletOnStart(g *runGroup, hostname string, restamp func(context.Context, string)) func(string, int) {
	starts := make(chan struct{}, 1)
	var running atomic.Bool
	g.add("kubelet restamp", func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-starts:
				func() {
					defer running.Store(false)
					if ctx.Err() == nil {
						restamp(ctx, hostname)
					}
				}()
			}
		}
	})
	return func(name string, pid int) {
		fmt.Printf("stratum: started %s (pid %d)\n", name, pid)
		if name == "kubelet" && g.ctx.Err() == nil && running.CompareAndSwap(false, true) {
			starts <- struct{}{}
		}
	}
}

// restampWhenRegistered is the controller's kubelet-start restamp: it
// re-asserts the node's version annotations when the node is already
// registered, with one check and no waiting. The supervisor outlives
// `stratum upgrade`, so a kubelet restart is the moment a new kubelet's
// version must reach the Node object, and on a running node that object is
// already there. The wait for a first registration — with the control-plane
// label that rides with it — belongs to the node-registered task on the run
// group. A first-boot kubelet that has not registered yet therefore leaves
// the stamp to the task and stamps nothing itself.
func restampWhenRegistered(ctx context.Context, hostname string) {
	if nodeRegistered(ctx, hostname) {
		stampNode(ctx, hostname)
	}
}

// restampOwnNodeWhenRegistered stamps a worker's own Node and reports
// whether the Node is registered, so its labels can follow.
func restampOwnNodeWhenRegistered(hostname string) bool {
	// A first start waits out TLS bootstrap, which writes the kubeconfig this
	// reads, and registration.
	if err := k8sutil.WaitOwnNodeRegistered(hostname, 5*time.Minute); err != nil {
		fmt.Fprintf(os.Stderr, "stratum: stamp version on %s: %v (non-fatal)\n", hostname, err)
		return false
	}
	if err := k8sutil.StampOwnNode(hostname); err != nil {
		fmt.Fprintf(os.Stderr, "stratum: stamp version on %s: %v (non-fatal)\n", hostname, err)
	}
	return true
}

// postStart starts the controller's post-start housekeeping: the etcd growth
// recorder for an ungrown seed, the node-registered task, and the long-lived
// cert-renew and snapshot loops the config enables, each a run-group task
// with the controller's context. No task waits on the apiserver to start —
// and none needs it: renewal reads
// and reissues local certificates only, a snapshot needs only the local
// etcd, the recorder only this node's own etcd, and the node-registered
// task's per-tick registration check is itself the apiserver wait.
func postStart(g *runGroup, cfg *ClusterConfig, st *NodeState) {
	// Only this node's own etcd, no apiserver: started first, so a seed whose
	// apiserver never becomes ready still records HA growth.
	if st.EtcdState == "new" && !st.EtcdHasPeers {
		g.add("etcd growth recorder", startEtcdGrowthRecorder)
	}
	// The node-registered task is the one cluster-facing piece: it waits out
	// the node's registration itself, per tick, so a slow apiserver delays
	// its ticks, never the loops beside it.
	g.add("node registration", func(ctx context.Context) { nodeRegisteredTask(ctx, st.Hostname, dashboardHint(cfg, st.NodeIP)) })
	g.add("node labels", func(ctx context.Context) { labelOwnNodeOnceRegistered(ctx, st) })
	if cfg.CertRenewEnabled() {
		g.add("cert renewal", func(ctx context.Context) { certRenewLoop(ctx, st.Hostname) })
	}
	if !cfg.Spec.Snapshot.Disabled {
		warnRelativeSnapshotDir(cfg, "stratum: WARN: ")
		g.add("etcd snapshots", func(ctx context.Context) { snapshotLoop(ctx, cfg) })
	}
	g.add("cluster-admins binding", reconcileTask("bind admin.conf's group to cluster-admin", clusterAdminsRetry, clusterAdminsResync,
		func(ctx context.Context) error { return ensureClusterAdminsBinding(ctx, st.Hostname) }))
	g.add("kubelet API admin binding", bindingTask("bind the apiserver's kubelet client to system:kubelet-api-admin",
		func(ctx context.Context) error { return ensureKubeletAPIAdminBinding(ctx) }))
	if st.VIP.Address != "" {
		g.add("kube-vip lease role", reconcileTask("let kube-vip hold its election lease", nodeClientBindingRetry, kubeVipRBACResync,
			func(ctx context.Context) error { return ensureKubeVipRBAC(ctx) }))
	}
	// Publishing can first succeed only after this node starts its apiserver.
	// Retry the exact persisted staging request, including its previous local
	// fingerprint, so transient failures never authorize a stale restart.
	g.add("staged bundle generation", bindingTask("record the staged bundle generation", func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, desiredRecordBudget)
		defer cancel()
		return publishPendingStaging(ctx)
	}))
	if !cfg.OperatorEnabled() {
		g.add("node-client approval binding", bindingTask("reserve the node-client auto-approval binding",
			func(ctx context.Context) error { return reserveNodeClientBinding(ctx) }))
	}
	g.add("leaf subjects", bindingTask("move this node's leaves off the subjects an earlier release issued", leafSubjectMigration(st)))
}

// leafSubjectMigrationMarker records a leaf reissue that has started and
// not finished, holding the drift that started it.
func leafSubjectMigrationMarker() string {
	return filepath.Join(version.K8sConfDir, "leaf-subject-migration")
}

// leafSubjectMigration returns the apply step of the task that moves a node
// upgraded from an earlier release onto the subjects this release issues: it
// finds leaves with an old subject, records that in
// leafSubjectMigrationMarker, puts in place the bindings their new subjects
// need, rotates every leaf, then removes the marker. While the marker exists
// the rotation is retried, across restarts too, because one cut short can
// leave the leaves reissued, and so no longer drifting, while a running
// component still holds the old certificate.
//
// It reports every failed reissue as it happens, rather than through the
// task's one delayed warning, and waits out a held maintenance lease, which
// an upgrade holds for its whole run, without counting it as a failure.
func leafSubjectMigration(st *NodeState) func(context.Context) error {
	resumed := false
	var retryAt time.Time
	var backoff time.Duration
	waitingForLease := false
	return func(ctx context.Context) error {
		if wait := time.Until(retryAt); wait > 0 {
			return fmt.Errorf("the last leaf reissue failed; retrying in %v", wait.Round(time.Second))
		}
		marker := leafSubjectMigrationMarker()
		recorded, err := os.ReadFile(marker)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			drifted, err := leafSubjectDrift()
			if err != nil {
				return fmt.Errorf("find leaves with an old subject: %w", err)
			}
			if len(drifted) == 0 {
				return nil
			}
			list := strings.Join(drifted, ", ")
			// The marker is a later start's resume record: a later start
			// reads it to say what the migration it is resuming was for. A
			// raw write here can leave a truncated record, so it goes
			// through the atomic single-file writer like the renewal record
			// it works alongside.
			if err := atomicfile.WriteFile(marker, []byte(list+"\n"), 0o600); err != nil {
				return fmt.Errorf("record the leaf reissue: %w", err)
			}
			resumed = true
			fmt.Printf("stratum: %s carry a subject this release no longer issues; reissuing every leaf once their bindings exist\n", list)
		case err != nil:
			return fmt.Errorf("read the leaf reissue record: %w", err)
		case !resumed:
			resumed = true
			fmt.Printf("stratum: resuming the reissue of every leaf an earlier start began for %s\n", strings.TrimSpace(string(recorded)))
		}
		ensure := []func(context.Context) error{
			func(ctx context.Context) error { return ensureClusterAdminsBinding(ctx, st.Hostname) },
			ensureKubeletAPIAdminBinding,
		}
		if st.VIP.Address != "" {
			ensure = append(ensure, ensureKubeVipRBAC)
		}
		for _, e := range ensure {
			if err := e(ctx); err != nil {
				return fmt.Errorf("ensure the bindings the new subjects need: %w", err)
			}
		}
		if err := rotateLeaves(ctx); err != nil {
			if errors.Is(err, maintlease.ErrTaken) {
				if !waitingForLease {
					waitingForLease = true
					fmt.Fprintf(os.Stderr, "stratum: leaf reissue waiting for the maintenance lease (%v)\n", err)
				}
				return err
			}
			waitingForLease = false
			backoff = min(max(2*backoff, leafReissueBackoff), leafReissueBackoffMax)
			retryAt = time.Now().Add(backoff)
			fmt.Fprintf(os.Stderr, "stratum: WARN: leaf reissue failed: %v; retrying in %v\n", err, backoff)
			return err
		}
		if err := os.Remove(marker); err != nil {
			return fmt.Errorf("clear the finished leaf reissue: %w", err)
		}
		fmt.Println("stratum: reissued every leaf with the subjects this release issues")
		return nil
	}
}

// bindingTask returns a run-group task that applies one cluster RBAC binding
// once the apiserver answers, retrying every nodeClientBindingRetry, then holds
// until shutdown.
func bindingTask(what string, apply func(context.Context) error) func(context.Context) {
	return reconcileTask(what, nodeClientBindingRetry, 0, apply)
}

// reconcileTask is bindingTask retrying every retry that, with a non-zero
// resync, applies again every resync after a success instead of holding. It
// warns once when failures have lasted bindingWarnAfter.
func reconcileTask(what string, retry, resync time.Duration, apply func(context.Context) error) func(context.Context) {
	return func(ctx context.Context) {
		var failingSince time.Time
		warned := false
		for {
			wait := retry
			if err := apply(ctx); err == nil {
				if resync == 0 {
					<-ctx.Done()
					return
				}
				failingSince, warned, wait = time.Time{}, false, resync
			} else if failingSince.IsZero() {
				failingSince = time.Now()
			} else if !warned && time.Since(failingSince) >= bindingWarnAfter {
				warned = true
				fmt.Fprintf(os.Stderr, "stratum: WARN: cannot %s yet (%v); still retrying\n", what, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

// nodeRegisteredTask is the controller's ONE node-registered task: it owns
// the wait for this node's kubelet to register, and the control-plane label
// and version stamp that ride with it. It is the single waiter: a wait of
// its own in the kubelet-start restamp, in postStart, or in a joined-member
// labelling pkg/cli fires beside the controller (ha.LabelJoinedControlPlane)
// would each give up on its own schedule. This one waits on one cadence
// under the one give-up budget. Per tick it checks registration once; the tick that
// finds the node registered labels it control-plane and stamps its version
// annotations exactly once, then holds until shutdown: re-asserting every
// tick would churn the API, and returning would have the run group restart
// it into a second label-and-stamp. A label that fails is retried until it
// lands (labelUntilApplied): the label is the role every cluster-wide
// operation reads, and a join registers while the VIP may still be moving,
// so one refused connection must not leave a controller classified a
// worker. The kubelet-restart re-stamp stays with
// the OnStart hook's single flight. When the give-up budget runs out with
// the node still unregistered, the task logs the hand-labelling instruction
// — an unlabeled control plane is classified a worker by `stratum upgrade
// --cluster` — and holds until shutdown too: giving up is final for this
// run, and a late auto-label would contradict the instruction it printed.
func nodeRegisteredTask(ctx context.Context, hostname string, hint []string) {
	fmt.Fprintf(os.Stderr, "stratum: waiting up to %v for node %s to register\n", nodeRegistrationGiveUp, hostname)
	start := time.Now()
	// The budget covers each API request as well as the waits between them:
	// a stalled registration probe must not outlive the give-up deadline.
	registrationCtx, cancelRegistration := context.WithTimeout(ctx, nodeRegistrationGiveUp)
	defer cancelRegistration()
	t := time.NewTicker(nodeRegistrationInterval)
	defer t.Stop()
	// nodeRegistrationInterval ticks a minute every progressEveryTicks ticks;
	// the progress line is what keeps a slow first boot readable in the
	// journal instead of silent until give-up.
	const progressEveryTicks = 20
waitForRegistration:
	for ticks := 0; ; ticks++ {
		select {
		case <-registrationCtx.Done():
			break waitForRegistration
		case <-t.C:
		}
		registered := nodeRegistered(registrationCtx, hostname)
		if registrationCtx.Err() != nil {
			break
		}
		if registered {
			cancelRegistration()
			fmt.Fprintf(os.Stderr, "stratum: node %s registered after %v; labeling it control-plane and stamping its version\n", hostname, time.Since(start))
			labelErr := labelControlPlane(ctx, hostname)
			stampNode(ctx, hostname)
			for _, line := range hint {
				fmt.Fprintf(os.Stderr, "stratum: %s\n", line)
			}
			if labelErr != nil {
				labelUntilApplied(ctx, hostname, labelErr)
			}
			<-ctx.Done()
			return
		}
		if ticks%progressEveryTicks == 0 {
			fmt.Fprintf(os.Stderr, "stratum: node %s not registered yet, %v of %v elapsed\n", hostname, time.Since(start), nodeRegistrationGiveUp)
		}
	}
	if ctx.Err() != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "stratum: node %s did not register within %v — could not label it control-plane; "+
		"run `stratum kubectl label node %s %s=true` or `stratum upgrade --cluster` will treat it as a worker\n",
		hostname, nodeRegistrationGiveUp, hostname, k8sutil.ControlPlaneLabel)
	<-ctx.Done()
}

// labelOwnNodeOnceRegistered labels a control plane's Node once it first
// registers. A first boot's kubelet starts before its Node exists, so the
// kubelet-start hook finds nothing to label; this waits out that first
// registration, within the node-registered task's budget, and labels once.
// It then holds until shutdown, labelled or not, as the node-registered task
// does: the run group restarts a task that returns, which would relabel the
// Node on every backoff and give a never-registered node a fresh budget.
func labelOwnNodeOnceRegistered(ctx context.Context, st *NodeState) {
	labelOwnNodeWhenRegistered(ctx, st)
	<-ctx.Done()
}

// labelOwnNodeWhenRegistered is labelOwnNodeOnceRegistered's wait and label,
// returning once the Node is labelled or the budget is spent. Its first check
// waits a tick, as the node-registered task's does, so its kubectl is not
// forked while the supervisor scans for a legacy supervisor.
func labelOwnNodeWhenRegistered(ctx context.Context, st *NodeState) {
	ctx, cancel := context.WithTimeout(ctx, nodeRegistrationGiveUp)
	defer cancel()
	t := time.NewTicker(nodeRegistrationInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if nodeRegistered(ctx, st.Hostname) {
			labelOwnNode(ctx, st)
			return
		}
	}
}

// labelUntilApplied retries the control-plane label that first failed with
// err, every nodeRegistrationInterval until it lands or ctx ends. It says so
// on the first failure, and once the label has failed for bindingWarnAfter
// it prints the hand-labelling instruction, still retrying: an operator who
// labels the node by hand meets an idempotent retry, not a contradiction.
func labelUntilApplied(ctx context.Context, hostname string, err error) {
	fmt.Fprintf(os.Stderr, "stratum: label control-plane %s: %v; retrying\n", hostname, err)
	failingSince := time.Now()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(nodeRegistrationInterval):
		}
		if err = labelControlPlane(ctx, hostname); err == nil {
			fmt.Fprintf(os.Stderr, "stratum: labeled %s control-plane after %v\n", hostname, time.Since(failingSince).Round(time.Second))
			return
		}
		if ctx.Err() != nil {
			return
		}
		if !warned && time.Since(failingSince) >= bindingWarnAfter {
			warned = true
			fmt.Fprintf(os.Stderr, "stratum: WARN: cannot label %s control-plane yet (%v); still retrying — until it lands, "+
				"`stratum upgrade --cluster` and `stratum certs rotate-ca --cluster` treat it as a worker; label it by hand with %s=true\n",
				hostname, err, k8sutil.ControlPlaneLabel)
		}
	}
}

// certRenewLoop rotates expiring leaves daily — the supervisor-native
// equivalent of a systemd renewal timer. The rotation itself
// (internal/certs) restarts components through the control socket.
//
// RotateContext takes the cluster fence before the host lock and holds both
// through readiness. A refused renewal — fence contention included — is
// skipped and retried on the next tick.
//
// The loop's first fire comes at this node's jitter offset (renewalJitter),
// not a whole interval later: a node restarted more often than once a day
// would otherwise never renew. Every attempt is recorded
// (certs.SaveRenewalRecord) so `stratum certs check` can show when renewal
// last ran and how it went — a dead or failing loop is invisible otherwise.
func certRenewLoop(ctx context.Context, hostname string) {
	// The offset waits out before the ticker starts, so the ticker's own
	// fires sit one plain interval apart for the loop's whole life: the
	// spread the first fire bought holds without drifting.
	select {
	case <-ctx.Done():
		return
	case <-time.After(renewalJitter(hostname, certRenewInterval)):
	}
	t := time.NewTicker(certRenewInterval)
	defer t.Stop()
	for {
		// A ready tick can win the select beside a cancelled context. Stop
		// before starting a new renewal or changing its last-attempt record.
		if ctx.Err() != nil {
			return
		}
		if !certRenewAttempt(ctx) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// certRenewAttempt records failures even when rotation panics. Re-panicking
// preserves the run group's recovery, stack logging and restart policy.
// Expected shutdown cancellation leaves the previous attempt record alone.
func certRenewAttempt(ctx context.Context) (keepRunning bool) {
	at := time.Now()
	result := ""
	record := true
	defer func() {
		p := recover()
		if p != nil {
			result = fmt.Sprintf("panic: %v", p)
		}
		if record {
			if err := certs.SaveRenewalRecord(certs.RenewalRecord{AttemptedAt: at, Result: result}); err != nil {
				fmt.Fprintf(os.Stderr, "stratum: record the cert renewal attempt: %v (non-fatal)\n", err)
			}
		}
		if p != nil {
			panic(p)
		}
	}()
	err := certRenewRotate(ctx)
	if ctx.Err() != nil && interrupt.CancellationOnly(err) {
		record = false
		return false
	}
	if err != nil {
		result = err.Error()
		fmt.Fprintf(os.Stderr, "stratum: scheduled cert renew: skipping this run — %v\n", err)
	}
	return true
}

// renewalJitter is the per-node offset on the renewal loop's first fire,
// seeded by the hostname: deterministic, so a node keeps its slot across
// restarts and a test can predict it, and different for different nodes, so
// control planes installed together — same leaf lifetimes — renew at spread
// times instead of all at the same minute every day. The offset lands inside
// (0, interval): 0 would be no jitter at all, and a whole interval would only
// swap which nodes collide. This is the renewal loop's only contention
// reduction; RotateContext coordinates disruptive renewals through the
// shared maintenance lease and the node lock.
func renewalJitter(hostname string, interval time.Duration) time.Duration {
	if interval <= 1 {
		return 0
	}
	seed := fnv.New64a()
	seed.Write([]byte(hostname))
	// The FNV-1a sum of the hostname is uniform over the interval, so the
	// offsets of a same-day install spread out; % (interval-1) + 1 lands
	// in (0, interval).
	return time.Duration(seed.Sum64()%uint64(interval-1) + 1)
}

// SnapshotBase is the snapshot config this cluster config asks for: what
// applies when no snapshot.conf exists. Credentials never live in config.yaml;
// they come from the environment (stratum.env) via etcdsnap.
func (c *ClusterConfig) SnapshotBase() etcdsnap.Config {
	base := etcdsnap.Config{Retain: c.Spec.Snapshot.Retain}
	if s3 := c.Spec.Snapshot.S3; s3 != nil {
		base.S3 = objstore.Target{Bucket: s3.Bucket, Prefix: s3.Prefix, Region: s3.Region, Endpoint: s3.Endpoint, PathStyle: s3.PathStyle}
	}
	return base
}

// defaultSnapshotDir is where snapshots go when no directory is resolved: the
// value the resolver owns and hands out as part of the resolved settings. It
// must stay identical to etcdsnap's own unexported default, which the save
// path falls back to when a caller passes no directory at all, so the loop and
// every reader keep naming the same place.
const defaultSnapshotDir = "/var/backups/etcd"

// SnapshotDir is the local directory this cluster config writes scheduled
// snapshots to: spec.snapshots.dir, else defaultSnapshotDir. A relative
// dir is ignored, not resolved: it would name a different place for every
// process with a different working directory, and one starting with "-" would
// reach etcdutl as a flag. A dir with a ".." element is ignored too: the kernel
// resolves ".." after symlinks and filepath.Clean before them, so the callers
// would not agree on which directory it names. Other unclean forms ("/a//b",
// "/a/./b", "/a/") mean the same to both and are cleaned.
func (c *ClusterConfig) SnapshotDir() string {
	if d := c.Spec.Snapshot.Dir; filepath.IsAbs(d) && !hasParentElem(d) {
		return filepath.Clean(d)
	}
	return defaultSnapshotDir
}

func hasParentElem(d string) bool {
	return slices.Contains(strings.Split(d, string(filepath.Separator)), "..")
}

// LoadSnapshotDir is the directory the scheduled loop writes to, for the
// commands that read or prune what it wrote. It reads the same
// cluster config as LoadSnapshotConfig; an unreadable one yields the default
// dir, and LoadSnapshotConfig has already warned about it.
func LoadSnapshotDir() string {
	path, _ := activeConfigPath(LoadState, io.Discard)
	return snapshotDirFrom(path)
}

func snapshotDirFrom(path string) string {
	cc, err := LoadConfig(path)
	if err != nil {
		return defaultSnapshotDir
	}
	warnRelativeSnapshotDir(cc, "warning: ")
	return cc.SnapshotDir()
}

// warnRelativeSnapshotDir says that SnapshotDir ignored a relative dir or one with "..".
func warnRelativeSnapshotDir(c *ClusterConfig, prefix string) {
	d := c.Spec.Snapshot.Dir
	switch {
	case d == "":
	case !filepath.IsAbs(d):
		fmt.Fprintf(os.Stderr, "%sspec.snapshots.dir %q is not absolute; using %s\n", prefix, d, defaultSnapshotDir)
	case hasParentElem(d):
		fmt.Fprintf(os.Stderr, "%sspec.snapshots.dir %q contains a \"..\" element; using %s\n", prefix, d, defaultSnapshotDir)
	}
}

// recordConfigPath persists the config file this controller was started with
// in the node state, for activeConfigPath. The default path is recorded as
// empty: it is where readers look anyway, and a missing default config is the
// ordinary no-config case, not a deleted one.
//
// The save goes through the mutation owner's standalone lock contract: it
// re-reads the state under the etcd data lock and applies the path to what it
// read. The state this start prepared is stale the moment a restore or a
// rejoin of this node lands while the start runs — `stratum up` released the
// data lock before RunController — and a save of the prepared copy would drop
// that writer's record. While another operation holds the lock the save
// refuses: it is best-effort (RunController only warns), and the next start
// records the path again.
func recordConfigPath(cfg *ClusterConfig, st *NodeState, save func(*NodeState) error) error {
	if cfg.path == "" {
		return nil
	}
	want := cfg.path
	if want == DefaultConfigPath {
		want = ""
	}
	if st.ConfigPath == want {
		return nil
	}
	st.ConfigPath = want
	return nodestate.MutateSave(func(cur *NodeState) error { cur.ConfigPath = want; return nil }, save)
}

// activeConfigPath is the cluster config file the running controller reads:
// the one `stratum up --config` recorded, else DefaultConfigPath. recorded
// reports whether it came from the node state; such a file is expected to
// exist. A node state that does not exist yet is the ordinary default case. Any
// other read or parse error is warned about, since it can leave a node started
// with a custom --config reading the default path (and its retention) instead.
func activeConfigPath(loadState func() (*NodeState, error), warn io.Writer) (path string, recorded bool) {
	st, err := loadState()
	switch {
	case err == nil && st.ConfigPath != "":
		return st.ConfigPath, true
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		fmt.Fprintf(warn, "stratum: WARN: could not read the node state (%v); reading %s for the snapshot config, which may not be the config this controller was started with\n", err, DefaultConfigPath)
	}
	return DefaultConfigPath, false
}

// LoadSnapshotConfig is the effective snapshot config for the CLI and the MCP
// server, resolved the way the scheduled loop resolves it: from the cluster
// config the controller was started with (`stratum up --config`), with
// snapshot.conf winning when it exists. The resolved settings carry the
// directory too (Config.Dir): spec.snapshots.dir, else the default, so both
// front ends act in the directory the scheduled loop owns. An unreadable
// cluster config, or a recorded one that has since been deleted, falls back to
// the built-in defaults, as these callers did before the cluster config was
// consulted at all; that is warned about on stderr, since it can make a manual
// save prune to the default retention or `schedule` persist a local-only
// config.
func LoadSnapshotConfig() (etcdsnap.Config, error) {
	path, recorded := activeConfigPath(LoadState, os.Stderr)
	return loadSnapshotConfig(path, recorded, func(base etcdsnap.Config) (etcdsnap.Config, bool, error) {
		c, err := etcdsnap.LoadConfigOr(base)
		return c, c.FromFile(), err
	}, os.Stderr)
}

// loadSnapshotConfig resolves snapshot.conf over the cluster config base and
// reports whether the file supplied the result. An absent config.yaml is not
// an error (LoadConfig yields defaults), unless mustExist says the controller
// recorded it, in which case its disappearance is treated like an unreadable
// file. The warning fires only when that failure actually left the defaults in
// effect.
func loadSnapshotConfig(clusterPath string, mustExist bool, load func(etcdsnap.Config) (etcdsnap.Config, bool, error), warn io.Writer) (etcdsnap.Config, error) {
	base := etcdsnap.DefaultConfig()
	cc, cerr := LoadConfig(clusterPath)
	if cerr == nil && mustExist {
		if _, err := os.Stat(clusterPath); errors.Is(err, fs.ErrNotExist) {
			cerr = fmt.Errorf("%s: the cluster config this controller was started with no longer exists", clusterPath)
		}
	}
	if cerr == nil {
		base = cc.SnapshotBase()
	}
	c, fromFile, err := load(base)
	if cerr != nil && !fromFile && err == nil {
		fmt.Fprintf(warn, "stratum: WARN: spec.snapshots ignored, using built-in snapshot defaults (retain %d, no S3): %v\n", base.Retain, cerr)
	}
	// The directory is part of the resolved settings: every caller that reads
	// them acts in the directory the scheduled loop writes — spec.snapshots.dir,
	// else the default. An unreadable cluster config yields the default dir,
	// exactly as LoadSnapshotDir does for the same case, and the warning about
	// a dir the resolver ignored (relative, or with a ".." element) moves here
	// with the resolution.
	if cerr == nil {
		warnRelativeSnapshotDir(cc, "warning: ")
		c.Dir = cc.SnapshotDir()
	} else {
		c.Dir = defaultSnapshotDir
	}
	return c, err
}

// scheduledSaveOptions resolves one scheduled snapshot's options. The snapshot
// config file wins when it exists; when it is absent the cluster config's
// retain and s3 apply, so a config.yaml-only controller still offloads and
// prunes as configured.
//
// An unreadable snapshot.conf is not fatal — the backup itself is still worth
// taking — but its retention settings cannot be trusted, so the returned
// options prune nothing and offload nothing (Retain 0 keeps every snapshot,
// a nil S3 target skips the upload), and a WARN names the read error. That
// fires on every tick, since each tick re-resolves the config.
func scheduledSaveOptions(cfg *ClusterConfig, load func(etcdsnap.Config) (etcdsnap.Config, error), warn io.Writer) etcdsnap.SaveOptions {
	base := cfg.SnapshotBase()
	opts := etcdsnap.SaveOptions{Retain: base.Retain, S3: base.S3Target(), Dir: cfg.SnapshotDir()}
	sc, err := load(base)
	if err == nil {
		opts.Retain, opts.S3 = sc.Retain, sc.S3Target()
		return opts
	}
	fmt.Fprintf(warn, "stratum: WARN: cannot read snapshot config, pruning skipped this tick (snapshot still taken and kept, no S3 upload): %v\n", err)
	opts.Retain, opts.S3 = 0, nil
	return opts
}

// snapshotLoop saves etcd snapshots on the configured interval — the
// supervisor-native stratum-etcd-snapshot.timer. Config precedence: the
// operator-managed snapshot config file (written by `stratum snapshot
// schedule` and the MCP `snapshot_schedule` tool) wins
// over the cluster config's inline settings.
func snapshotLoop(ctx context.Context, cfg *ClusterConfig) {
	t := time.NewTicker(snapshotInterval(cfg))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := snapshotSave(ctx, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "stratum: scheduled etcd snapshot: %v\n", err)
		}
	}
}

// importPayloadImages loads the staged OCI archive into containerd's k8s.io
// namespace once the socket is up — the airgap contract: the
// artifact carries its images, so no node ever needs a registry to reach
// Ready. Idempotent by containerd's own import semantics (matching digests
// no-op), so restarts re-run it harmlessly.
func importPayloadImages(ctx context.Context) error {
	return ImportImages(ctx, filepath.Join(version.DataDir, assets.ImagesDir))
}

// ImportImages is importPayloadImages pointed at an explicit archive dir —
// the per-node upgrade (internal/upgrade) imports the upgrade binary's staged
// images from it before re-rendering the static pod manifests.
func ImportImages(ctx context.Context, dir string) error {
	return ImportImagesWithWriter(ctx, os.Stdout, dir)
}

// ImportImagesWithWriter imports payload images and reports progress to w.
func ImportImagesWithWriter(ctx context.Context, w io.Writer, dir string) error {
	if err := importWaitContainerd(ctx, 3*time.Minute); err != nil {
		return fmt.Errorf("image import: %w", err)
	}
	tars, _ := filepath.Glob(filepath.Join(dir, "*.tar"))
	if len(tars) == 0 {
		return nil // EMBEDDED_BINS=none build: images come from a registry/mirror
	}
	sock := strings.TrimPrefix(version.ContainerdSock, "unix://")
	ctr := filepath.Join(version.BinDir, "ctr")
	for _, tar := range tars {
		// This runs in kubelet's WaitFor, i.e. on EVERY kubelet (re)start.
		// Re-reading a ~1G archive each time would turn every kubelet bounce
		// into a minute of import; a size+mtime stamp makes the re-check as
		// cheap as assets.Stage's equivalent skip.
		if stampCurrent(tar) {
			continue
		}
		cmd := exec.Command(ctr, "--address", sock, "-n", "k8s.io", "images", "import", tar)
		if out, err := cmd.CombinedOutput(); err != nil {
			// Failing here fails kubelet's WaitFor, which the supervisor
			// retries with backoff — a visible crash-loop instead of a node
			// that comes up and ImagePullBackOffs forever on airgap.
			return fmt.Errorf("import %s: %w\n%s", tar, err, out)
		}
		fmt.Fprintf(w, "stratum: imported payload images from %s\n", filepath.Base(tar))
		writeStamp(tar)
	}
	names, err := assets.ImageNames(tars)
	if err != nil {
		return err
	}
	// Pinning is a handful of ctr calls — cheap enough to re-assert every
	// start, and it also covers images imported before a stratum upgrade
	// introduced pinning.
	return pinPayloadImages(ctr, sock, names)
}

// importWaitContainerd is waitContainerd behind a seam, so a test can run ImportImages against a fake ctr.
var importWaitContainerd = waitContainerd

// stampCurrent reports whether tar was already imported by this exact archive
// (size+mtime match, the same two-field comparison assets.Stage uses). The
// stamp lives under the CONFIG dir, not next to the tar: assets.Stage stages
// the images dir read-only, so a sibling stamp file fails to write silently
// and every kubelet restart would re-read the whole archive again.
func stampCurrent(tar string) bool {
	fi, err := os.Stat(tar)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(stampPath(tar))
	return err == nil && string(b) == stampValue(fi)
}

func writeStamp(tar string) {
	if fi, err := os.Stat(tar); err == nil {
		_ = os.WriteFile(stampPath(tar), []byte(stampValue(fi)), 0o644)
	}
}

func stampPath(tar string) string {
	return filepath.Join(version.K8sConfDir, filepath.Base(tar)+".imported")
}

func stampValue(fi os.FileInfo) string {
	return fmt.Sprintf("%d %d", fi.Size(), fi.ModTime().UnixNano())
}

// payloadImageRows picks the (ref, digest) rows of `ctr images ls` to pin:
// every name a payload archive carries, and every image under the engine's
// registry, which also catches the repo@digest aliases an earlier start made.
func payloadImageRows(ls string, archived map[string]bool) [][2]string {
	var rows [][2]string
	for _, line := range strings.Split(ls, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.HasPrefix(f[2], "sha256:") {
			continue // header or unexpected row
		}
		if strings.HasPrefix(f[0], addons.ImageRegistry+"/") || archived[f[0]] {
			rows = append(rows, [2]string{f[0], f[2]})
		}
	}
	return rows
}

// pinPayloadImages does two post-import fixups on every payload image (see
// payloadImageRows):
//
//  1. Registers a digest-named alias (repo@sha256:…). The bundled manifests pin
//     images as name:tag@digest, and CRI resolves such a reference by DIGEST —
//     but `ctr images import` registers only the name:tag from the archive, so
//     without the alias every digest-pinned pod tries the registry and an
//     airgapped node ImagePullBackOffs forever with the image already on disk.
//  2. Labels it io.cri-containerd.pinned=pinned, excluding it from kubelet's
//     image GC and disk-pressure reclaim. A GC'd payload image cannot be
//     re-pulled on an airgapped node, so "unused" images (an add-on whose pod
//     has not scheduled yet, kube-vip with no VIP) must survive tight disks.
//
// Both were found by CI, not invented: the digest miss is why add-ons pulled
// from a cold mirror, and a tight runner disk GC'd them minutes after import.
func pinPayloadImages(ctr, sock string, archived map[string]bool) error {
	out, err := exec.Command(ctr, "--address", sock, "-n", "k8s.io", "images", "ls").Output()
	if err != nil {
		return fmt.Errorf("pin payload images: list: %w", err)
	}
	for _, row := range payloadImageRows(string(out), archived) {
		ref, digest := row[0], row[1]
		names := []string{ref}
		// A ref that already carries "@" IS a digest name — either an alias
		// this loop created on a previous start or a digest-pulled image.
		// Deriving repo by last-colon on such a ref would split inside
		// "@sha256:" and build an invalid double-digest name that ctr rejects
		// — which, inside kubelet's WaitFor, is a kubelet that never starts.
		if !strings.Contains(ref, "@") {
			repo := ref
			if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
				repo = ref[:i]
			}
			alias := repo + "@" + digest
			cmd := exec.Command(ctr, "--address", sock, "-n", "k8s.io",
				"images", "tag", "--force", ref, alias)
			if tout, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("alias %s: %w\n%s", alias, err, tout)
			}
			names = append(names, alias)
		}
		for _, name := range names {
			cmd := exec.Command(ctr, "--address", sock, "-n", "k8s.io",
				"images", "label", name, "io.cri-containerd.pinned=pinned")
			if lout, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("pin %s: %w\n%s", name, err, lout)
			}
		}
	}
	return nil
}

// saveCiliumSettings records the Cilium settings this control plane renders,
// for the shared certs to carry to a joining one. The shipped settings leave no
// file, so a cluster that never set any shares no entry an older binary would
// refuse.
func saveCiliumSettings(s addons.CiliumSettings) error {
	if s.IsDefault() {
		if err := os.Remove(version.CiliumSettingsPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", version.CiliumSettingsPath, err)
		}
		return nil
	}
	if err := host.WriteFileAtomically(version.CiliumSettingsPath, s.Encode(), 0o644); err != nil {
		return fmt.Errorf("save the Cilium settings: %w", err)
	}
	return nil
}

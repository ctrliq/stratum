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

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

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

// a var so a test can substitute the supervised children.
var processes = Processes

// RunController supervises a controller until ctx is cancelled.
func RunController(ctx context.Context, cfg *ClusterConfig, st *NodeState, sampleEvidence ...EvidenceSampler) error {
	// reset every call: a later start must not publish through an earlier call's sampler
	evidenceSampler = noopEvidenceSampler
	if len(sampleEvidence) > 0 {
		evidenceSampler = sampleEvidence[0]
	}
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
	// before supervisor.Run: upgrade and rollback treat a live supervisor as proof this write is done
	if err := staticpod.WriteAll(st); err != nil {
		return err
	}
	procs := processes(st, cfg.OperatorEnabled())
	if err := writeComponentArgs(procs); err != nil {
		return err
	}

	// a derived context: supervisor.Run can return early while ctx is still live
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tasks := newRunGroup(gctx)
	postStart(tasks, cfg, st)

	configPath := cfg.path
	if configPath == "" {
		configPath = DefaultConfigPath
	}
	onStart := managedKubeletOnStart(tasks, st.Hostname, controllerKubeletStart(st), func(ctx context.Context, hostname string) {
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
	// a no-op on normal shutdown; frees the parked tasks on the early-failure path
	cancel()
	tasks.wait()
	return err
}

// restart argv; --cis, not --profile cis, for a rollback binary predating --profile.
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

// prepareWorkerKubelet renders kubelet.conf before the state: a state saved first reads as learned.
func prepareWorkerKubelet(st *NodeState) error {
	if err := writeCoreDNSUpstream(st); err != nil {
		return fmt.Errorf("write the CoreDNS upstream: %w", err)
	}
	learned := learnClusterDNS(st)
	if err := writeKubeletConf(st); err != nil {
		return fmt.Errorf("write kubelet.conf: %w", err)
	}
	if learned {
		// a whole-file save of st could drop a field another writer landed during the render above
		if err := nodestate.MutateSave(func(cur *NodeState) error { cur.ClusterDNSIP = st.ClusterDNSIP; return nil }, saveState); err != nil {
			return fmt.Errorf("record the cluster DNS address %s: %w", st.ClusterDNSIP, err)
		}
	}
	return nil
}

// RunWorker supervises a worker node (containerd + kubelet).
func RunWorker(ctx context.Context, st *NodeState) error {
	// preflight has refused a running containerd, so this start writes the config but restarts nothing
	if _, err := runtime.EnsureContainerdConfigs(os.Stdout, st); err != nil {
		return fmt.Errorf("prepare containerd: %w", err)
	}
	if err := prepareWorkerKubelet(st); err != nil {
		return err
	}
	procs := Processes(st, false)
	if err := writeComponentArgs(procs); err != nil {
		return err
	}
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tasks := newRunGroup(gctx)
	tasks.add("AppArmor runc profile", appArmorProfileLoop)
	err := runSupervisor(ctx, procs, supervisor.Options{
		LogDir:        version.LogDir,
		ControlSocket: version.ControlSocket,
		// No admin.conf here: the kubelet stamps its own Node.
		OnStart: kubeletOnStart(st.Hostname, workerKubeletStart(ctx, st), func(hostname string) {
			if ownNodeRegistered(hostname) {
				labelOwnNode(ctx, st)
			}
		}),
		ReExec: UpArgv(version.StratumInstallPath, true, "", ""),
	})
	cancel()
	tasks.wait()
	return err
}

// writeBundles stages the add-on manifests the operator server-side-applies and prunes.
func writeBundles(cfg *ClusterConfig, st *NodeState) error {
	if err := refuseOrphaningTheDashboard(cfg); err != nil {
		return err
	}
	if err := refuseRuntimeClassesWithoutTheOperator(cfg); err != nil {
		return err
	}
	if err := refuseForeignRuntimeClassFiles(); err != nil {
		return err
	}
	// sentinels: the operator fills them at apply time, so every control plane stages the same bytes
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
		// atomic writer: a torn manifest must never stand where a working one was
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

	// best-effort: a cold first boot cannot reach the API; postStart retries the persisted request
	recordDesiredBundles(cfg, st, stacks)

	return saveCiliumSettings(cfg.Spec.Network.Cilium.Settings())
}

const (
	localPathFile    = "local-path/local-path.yaml"
	dashboardFile    = addons.DashboardStack + "/dashboard.yaml"
	runtimeClassFile = addons.RuntimeClassStack + "/runtime-classes.yaml"
)

// refuseOrphaningTheDashboard refuses spec.operator: false while the dashboard is still staged.
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

func refuseRuntimeClassesWithoutTheOperator(cfg *ClusterConfig) error {
	if cfg.OperatorEnabled() {
		return nil
	}
	if len(cfg.Spec.RuntimeClasses) > 0 {
		return errors.New("spec.runtimeClasses needs the operator, which publishes them: with spec.operator: false nothing would. " +
			"Remove spec.runtimeClasses, or leave the operator on")
	}
	if _, err := os.Stat(filepath.Join(version.ManifestsDir, runtimeClassFile)); err != nil {
		return nil
	}
	return errors.New("spec.operator: false would leave the RuntimeClasses spec.runtimeClasses published with nothing to remove them. " +
		"Restart Stratum with spec.runtimeClasses removed and the operator still on, wait until " +
		"`stratum kubectl get runtimeclass` no longer lists them, then turn the operator off")
}

func refuseForeignRuntimeClassFiles() error {
	dir := filepath.Join(version.ManifestsDir, addons.RuntimeClassStack)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var foreign []string
	for _, e := range entries {
		if e.IsDir() || (filepath.Ext(e.Name()) != ".yaml" && filepath.Ext(e.Name()) != ".yml") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if path == filepath.Join(version.ManifestsDir, runtimeClassFile) {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.HasPrefix(string(b), addons.RuntimeClassHeader) {
				continue
			}
		}
		foreign = append(foreign, path)
	}
	if len(foreign) == 0 {
		return nil
	}
	return fmt.Errorf("%s holds %s, which Stratum did not stage: Stratum now stages spec.runtimeClasses in that directory, "+
		"and would overwrite or remove what is there, and the operator would then delete the objects it declares. "+
		"Move them to a directory of their own under %s, which the operator applies as a stack of that name, and start again",
		dir, strings.Join(foreign, ", "), version.ManifestsDir)
}

// deletes dropped bundles only under the bundled stacks, never hand-dropped or loose files
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

// desiredRecordBudget bounds one desired-generation recording, which is best-effort.
const desiredRecordBudget = 15 * time.Second

// recordDesiredBundles publishes the stacks as one generation; postStart retries it.
func recordDesiredBundles(cfg *ClusterConfig, st *NodeState, stacks map[string][]byte) {
	if err := stageDesiredBundles(cfg, st, stacks); err != nil {
		fmt.Fprintf(os.Stderr, "stratum: WARN: record the staged bundle generation: %v (the post-start task retries; existing cluster intent is preserved)\n", err)
	}
}

// renderedStacks names the stack directories a bundle set covers, for the deployer's digest gate.
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

// writeComponentArgs records each component's argv; the supervisor has no readable unit files.
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
		// a torn .args file would record an argv the component does not run
		if err := host.WriteFileAtomically(filepath.Join(dir, p.Name+".args"), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// seams, so a test can run postStart and drive the loops without an apiserver or a host.
var (
	// one job, then holds: an early return would be re-run by the run group every backoff
	startEtcdGrowthRecorder = func(ctx context.Context) {
		recordEtcdGrowth(ctx)
		<-ctx.Done()
	}

	runSupervisor = supervisor.Run
	// a seam, so a test can pin warning delivery without rotating host PKI
	renewCertificates = certs.RotateContext

	certRenewInterval = 24 * time.Hour
	certRenewRotate   = func(ctx context.Context) error {
		// quiet: routine progress off; restart and endpoint warnings still belong in the log
		_, err := renewCertificates(ctx, os.Stdout, certs.Options{IfExpiringDays: version.CertRenewWindowDays, Quiet: true})
		return err
	}
	snapshotInterval = func(c *ClusterConfig) time.Duration {
		return time.Duration(c.Spec.Snapshot.IntervalHours) * time.Hour
	}
	// a seam, so a test can supply or break snapshot.conf without touching /etc
	snapshotLoadConfig = etcdsnap.LoadConfigOr
	snapshotStartRetry = time.Minute
	// a path beside an error is a local save whose upload failed, not etcd being down
	snapshotSave = func(ctx context.Context, c *ClusterConfig) (string, error) {
		// the daemon's log is stdout; MultiWriter also records tick warnings for the evidence record
		var warns tickWarnings
		opts := scheduledSaveOptions(c, snapshotLoadConfig, io.MultiWriter(os.Stdout, &warns))
		opts.EvidenceWarnings = warns.recorded()
		return etcdsnap.SaveContext(ctx, os.Stdout, opts)
	}

	// the controller's one registration wait and give-up budget; no second controller wait or give-up
	nodeRegistrationInterval = 3 * time.Second
	nodeRegistrationGiveUp   = 10 * time.Minute
	nodeRegistered           = func(ctx context.Context, hostname string) bool {
		return k8sutil.WaitNodeRegistered(ctx, hostname, 1) == nil
	}
	labelControlPlane = k8sutil.LabelControlPlaneContext

	reserveNodeClientBinding = func(ctx context.Context) error {
		cs, err := k8sutil.AdminClientset()
		if err != nil {
			return err
		}
		return k8sutil.DisableNodeClientAutoApprove(ctx, cs.RbacV1().ClusterRoleBindings())
	}
	nodeClientBindingRetry = 30 * time.Second
	bindingWarnAfter       = 5 * time.Minute
	// admin.conf answers 403 until the binding lands
	clusterAdminsRetry = 2 * time.Second
	// a snapshot restore drops the binding marker under a task that does not restart
	clusterAdminsResync = time.Minute

	ensureKubeletAPIAdminBinding = func(ctx context.Context) error {
		cs, err := k8sutil.AdminClientset()
		if err != nil {
			return err
		}
		return k8sutil.EnsureKubeletAPIAdminBinding(ctx, cs.RbacV1().ClusterRoleBindings())
	}

	ensureClusterAdminsBinding = func(ctx context.Context, holder string) error {
		cs, err := k8sutil.AdminClientset()
		if err != nil {
			return err
		}
		return k8sutil.EnsureClusterAdminsBinding(ctx, cs, holder)
	}

	// through the local apiserver: admin.conf may name the VIP kube-vip holds
	ensureKubeVipRBAC = func(ctx context.Context) error {
		cs, err := k8sutil.LocalAdminClientset()
		if err != nil {
			return err
		}
		return k8sutil.EnsureKubeVipRBAC(ctx, cs)
	}
	kubeVipRBACResync = 5 * time.Minute

	leafSubjectDrift = certs.SubjectDrift
	rotateLeaves     = func(ctx context.Context) error {
		// nothing-to-rotate is a fine outcome here, so the rotated flag goes unread
		_, err := certs.RotateContext(ctx, os.Stdout, certs.Options{Quiet: true, KubeVipRequired: true})
		return err
	}
	// every attempt restarts the control plane, so failures must not bounce etcd on a fast retry
	leafReissueBackoff    = time.Minute
	leafReissueBackoffMax = 30 * time.Minute

	// both registration and restamp use the group's context so shutdown cancels and joins them
	stampNode = func(ctx context.Context, hostname string) {
		if err := k8sutil.StampNodeContext(ctx, hostname); err != nil {
			fmt.Fprintf(os.Stderr, "stratum: stamp version on %s: %v (non-fatal)\n", hostname, err)
		}
	}

	reconcileOwnNodeLabels = k8sutil.ReconcileOwnNodeLabels

	// labels from node.yaml as it is then: a running apply changes the handlers under this st
	labelOwnNode = func(ctx context.Context, st *NodeState) {
		label := func(ctx context.Context) error {
			cur := st
			if saved, err := LoadState(); err == nil && saved.Hostname == st.Hostname {
				cur = saved
			}
			return reconcileNodeLabels(ctx, cur)
		}
		err := label(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		fmt.Fprintf(os.Stderr, "stratum: label %s: %v; retrying\n", st.Hostname, err)
		took, ok := retryUntilApplied(ctx, label, func(err error) {
			fmt.Fprintf(os.Stderr, "stratum: WARN: cannot label %s yet (%v); still retrying — until it lands, "+
				"pods whose RuntimeClass selects this node's runtime handlers are not scheduled onto it\n", st.Hostname, err)
		})
		if ok {
			fmt.Fprintf(os.Stderr, "stratum: labelled %s after %v\n", st.Hostname, took)
		}
	}

	loadedHandlers = runtime.LoadedHandlers

	waitOwnNodeRegistered        = k8sutil.WaitOwnNodeRegistered
	stampOwnNode                 = k8sutil.StampOwnNode
	ownNodeTaints                = k8sutil.OwnNodeTaints
	ownNodeRegistrationWarnAfter = 5 * time.Minute

	checkHandlerBinary = runtime.CheckHandlerBinary

	// brief wait: a first join's containerd starts before the kubelet has a credential
	ownNodeRegistered = func(hostname string) bool {
		return k8sutil.WaitOwnNodeRegistered(hostname, 10*time.Second) == nil
	}
)

// the one labelling rule for a kubelet or containerd start, apply and restart.
func reconcileNodeLabels(ctx context.Context, st *NodeState) error {
	runnable := slices.DeleteFunc(slices.Sorted(maps.Keys(st.RuntimeHandlers)), func(n string) bool {
		return checkHandlerBinary(st.RuntimeHandlers[n]) != nil
	})
	return reconcileOwnNodeLabels(ctx, st.Hostname, st.NodeLabels, loadedHandlers(runnable))
}

// a containerd restart may load a handler an apply wrote without a kubelet start following.
func kubeletOnStart(hostname string, restamp, relabel func(hostname string)) func(string, int) {
	onKubelet, onContainerd := singleFlight(hostname, restamp), singleFlight(hostname, relabel)
	return func(name string, pid int) {
		fmt.Printf("stratum: started %s (pid %d)\n", name, pid)
		switch name {
		case "kubelet":
			onKubelet()
		case "containerd":
			onContainerd()
		}
	}
}

func singleFlight(hostname string, fn func(string)) func() {
	if fn == nil {
		return func() {}
	}
	var running atomic.Bool
	return func() {
		if running.CompareAndSwap(false, true) {
			go func() {
				defer running.Store(false)
				fn(hostname)
			}()
		}
	}
}

// managedKubeletOnStart is kubeletOnStart under the run group's recovery and shutdown join.
func managedKubeletOnStart(g *runGroup, hostname string, restamp, relabel func(context.Context, string)) func(string, int) {
	onKubelet := managedSingleFlight(g, "kubelet restamp", hostname, restamp)
	onContainerd := managedSingleFlight(g, "containerd relabel", hostname, relabel)
	return func(name string, pid int) {
		fmt.Printf("stratum: started %s (pid %d)\n", name, pid)
		switch name {
		case "kubelet":
			onKubelet()
		case "containerd":
			onContainerd()
		}
	}
}

// managedSingleFlight is singleFlight as the run-group task named task.
func managedSingleFlight(g *runGroup, task, hostname string, fn func(context.Context, string)) func() {
	if fn == nil {
		return func() {}
	}
	starts := make(chan struct{}, 1)
	var running atomic.Bool
	g.add(task, func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				return
			case <-starts:
				func() {
					defer running.Store(false)
					if ctx.Err() == nil {
						fn(ctx, hostname)
					}
				}()
			}
		}
	})
	return func() {
		if g.ctx.Err() == nil && running.CompareAndSwap(false, true) {
			starts <- struct{}{}
		}
	}
}

func controllerKubeletStart(st *NodeState) func(context.Context, string) {
	return func(ctx context.Context, hostname string) {
		if restampWhenRegistered(ctx, hostname) {
			labelOwnNode(ctx, st)
		}
	}
}

func workerKubeletStart(ctx context.Context, st *NodeState) func(string) {
	return func(hostname string) {
		if restampOwnNodeWhenRegistered(ctx, hostname) {
			checkJoinedTaints(ctx, st)
			labelOwnNode(ctx, st)
		}
	}
}

// the kubelet taints only a Node it creates, so a rejoin onto an old Node lands untainted.
func checkJoinedTaints(ctx context.Context, st *NodeState) {
	if !st.NodeTaintsCheckPending {
		return
	}
	have, err := ownNodeTaints(ctx, st.Hostname)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stratum: check %s carries its declared taints: %v; checking again on the next start\n", st.Hostname, err)
		return
	}
	var missing []string
	for _, t := range st.NodeTaints {
		key, value, effect, _ := runtime.SplitTaint(t)
		want := corev1.Taint{Key: key, Value: value, Effect: corev1.TaintEffect(effect)}
		if !slices.ContainsFunc(have, func(h corev1.Taint) bool { return want.MatchTaint(&h) && h.Value == want.Value }) {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		fix := "taint it with `stratum kubectl taint node %[1]s <taint>`"
		if slices.ContainsFunc(missing, func(t string) bool {
			k, v, e, _ := runtime.SplitTaint(t)
			return k == k8sutil.UnverifiedTaint && v == "true" && e == string(corev1.TaintEffectNoSchedule)
		}) {
			fix = "hold it with `stratum node hold %[1]s --reason <why>`, which adds " + k8sutil.UnverifiedTaint + "=true:NoSchedule, " + fix + " for any other"
		}
		fmt.Fprintf(os.Stderr, "stratum: WARN: node %[1]s registered without its declared taints %[2]s: the kubelet applies them only when it creates "+
			"the Node, and this join registered onto a Node that was already there, so nothing holds workloads off it. From a control plane, "+
			fix+", or delete the Node with `stratum kubectl delete node %[1]s` and restart "+
			"this worker so it registers again with them\n", st.Hostname, strings.Join(missing, ", "))
	}
	if err := nodestate.MutateSave(func(cur *NodeState) error { cur.NodeTaintsCheckPending = false; return nil }, saveState); err != nil {
		fmt.Fprintf(os.Stderr, "stratum: record that %s checked its taints: %v (non-fatal)\n", st.Hostname, err)
		return
	}
	st.NodeTaintsCheckPending = false
}

// the first-registration wait belongs to the node-registered task; a first boot stamps nothing.
func restampWhenRegistered(ctx context.Context, hostname string) bool {
	if !nodeRegistered(ctx, hostname) {
		return false
	}
	stampNode(ctx, hostname)
	return true
}

// a first start waits out TLS bootstrap, which writes the kubeconfig stampOwnNode reads.
func restampOwnNodeWhenRegistered(ctx context.Context, hostname string) bool {
	for warned := false; ; {
		err := waitOwnNodeRegistered(hostname, ownNodeRegistrationWarnAfter)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return false
		}
		if !warned {
			warned = true
			fmt.Fprintf(os.Stderr, "stratum: stamp version on %s: %v; still waiting\n", hostname, err)
		}
	}
	if err := stampOwnNode(hostname); err != nil {
		fmt.Fprintf(os.Stderr, "stratum: stamp version on %s: %v (non-fatal)\n", hostname, err)
	}
	return true
}

// no task waits on the apiserver: each checks its own precondition per tick.
func postStart(g *runGroup, cfg *ClusterConfig, st *NodeState) {
	// started first: a seed whose apiserver never comes up still records HA growth
	if st.EtcdState == "new" && !st.EtcdHasPeers {
		g.add("etcd growth recorder", startEtcdGrowthRecorder)
	}
	if st.EtcdPromotePending {
		g.add("etcd learner promotion", promoteLearner)
	}
	g.add("node registration", func(ctx context.Context) { nodeRegisteredTask(ctx, st, dashboardHint(cfg, st.NodeIP)) })
	g.add("AppArmor runc profile", appArmorProfileLoop)
	if cfg.CertRenewEnabled() {
		g.add("cert renewal", func(ctx context.Context) { certRenewLoop(ctx, st.Hostname) })
	}
	if !cfg.Spec.Snapshot.Disabled {
		warnRelativeSnapshotDir(cfg, "stratum: WARN: ")
		g.add("etcd snapshots", func(ctx context.Context) { snapshotLoop(ctx, cfg) })
		// the publisher shares the snapshot loop's gate: no snapshots, no evidence
		g.add("backup evidence publisher", func(ctx context.Context) { publishEvidenceLoop(ctx, evidenceSampler) })
	}
	g.add("cluster-admins binding", reconcileTask("bind admin.conf's group to cluster-admin", clusterAdminsRetry, clusterAdminsResync,
		func(ctx context.Context) error { return ensureClusterAdminsBinding(ctx, st.Hostname) }))
	g.add("kubelet API admin binding", bindingTask("bind the apiserver's kubelet client to system:kubelet-api-admin",
		func(ctx context.Context) error { return ensureKubeletAPIAdminBinding(ctx) }))
	if st.VIP.Address != "" {
		g.add("kube-vip lease role", reconcileTask("let kube-vip hold its election lease", nodeClientBindingRetry, kubeVipRBACResync,
			func(ctx context.Context) error { return ensureKubeVipRBAC(ctx) }))
	}
	// retry the exact persisted request, so a transient failure never authorizes a stale restart
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

// leafSubjectMigrationMarker records a started-but-unfinished reissue and its drift.
func leafSubjectMigrationMarker() string {
	return filepath.Join(version.K8sConfDir, "leaf-subject-migration")
}

// retried across restarts: a cut-short reissue leaves no drift, but components still hold old certs
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
			// atomic write: a raw one could leave a truncated record a later start reads
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

// bindingTask is a run-group task applying one binding once the apiserver answers, then holding.
func bindingTask(what string, apply func(context.Context) error) func(context.Context) {
	return reconcileTask(what, nodeClientBindingRetry, 0, apply)
}

// reconcileTask is bindingTask applying again every resync after a success instead of holding.
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

// nodeRegisteredTask is the one registration wait; after give-up it holds rather than relabelling.
func nodeRegisteredTask(ctx context.Context, st *NodeState, hint []string) {
	hostname := st.Hostname
	fmt.Fprintf(os.Stderr, "stratum: waiting up to %v for node %s to register\n", nodeRegistrationGiveUp, hostname)
	start := time.Now()
	// the budget covers each API request too: a stalled probe must not outlive the deadline
	registrationCtx, cancelRegistration := context.WithTimeout(ctx, nodeRegistrationGiveUp)
	defer cancelRegistration()
	t := time.NewTicker(nodeRegistrationInterval)
	defer t.Stop()
	// the progress line keeps a slow first boot readable instead of silent until give-up
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
			labelOwnNode(ctx, st)
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

// labelUntilApplied keeps retrying: a hand-label meets an idempotent retry.
func labelUntilApplied(ctx context.Context, hostname string, err error) {
	fmt.Fprintf(os.Stderr, "stratum: label control-plane %s: %v; retrying\n", hostname, err)
	took, ok := retryUntilApplied(ctx, func(ctx context.Context) error { return labelControlPlane(ctx, hostname) }, func(err error) {
		fmt.Fprintf(os.Stderr, "stratum: WARN: cannot label %s control-plane yet (%v); still retrying — until it lands, "+
			"`stratum upgrade --cluster` and `stratum certs rotate-ca --cluster` treat it as a worker; label it by hand with %s=true\n",
			hostname, err, k8sutil.ControlPlaneLabel)
	})
	if ok {
		fmt.Fprintf(os.Stderr, "stratum: labeled %s control-plane after %v\n", hostname, took)
	}
}

func retryUntilApplied(ctx context.Context, apply func(context.Context) error, warn func(error)) (time.Duration, bool) {
	failingSince := time.Now()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return 0, false
		case <-time.After(nodeRegistrationInterval):
		}
		err := apply(ctx)
		if err == nil {
			return time.Since(failingSince).Round(time.Second), true
		}
		if ctx.Err() != nil {
			return 0, false
		}
		if !warned && time.Since(failingSince) >= bindingWarnAfter {
			warned = true
			warn(err)
		}
	}
}

// first fire at the jitter offset: a node restarted more often would never renew.
func certRenewLoop(ctx context.Context, hostname string) {
	// the offset waits before the ticker starts, so later fires sit one plain interval apart
	select {
	case <-ctx.Done():
		return
	case <-time.After(renewalJitter(hostname, certRenewInterval)):
	}
	t := time.NewTicker(certRenewInterval)
	defer t.Stop()
	for {
		// a ready tick can win the select beside a cancelled context
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

// certRenewAttempt records even a panic, then re-panics into the run group's recovery.
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

// renewalJitter spreads same-day installs; deterministic per node, so each keeps its slot.
func renewalJitter(hostname string, interval time.Duration) time.Duration {
	if interval <= 1 {
		return 0
	}
	seed := fnv.New64a()
	seed.Write([]byte(hostname))
	// % (interval-1) + 1 lands in (0, interval): 0 would be no jitter at all
	return time.Duration(seed.Sum64()%uint64(interval-1) + 1)
}

// SnapshotBase is what applies when no snapshot.conf exists; credentials come from the environment.
func (c *ClusterConfig) SnapshotBase() etcdsnap.Config {
	base := etcdsnap.Config{Retain: c.Spec.Snapshot.Retain}
	if s3 := c.Spec.Snapshot.S3; s3 != nil {
		base.S3 = objstore.Target{Bucket: s3.Bucket, Prefix: s3.Prefix, Region: s3.Region, Endpoint: s3.Endpoint, PathStyle: s3.PathStyle}
	}
	return base
}

// defaultSnapshotDir must match etcdsnap's own unexported default save-path fallback.
const defaultSnapshotDir = "/var/backups/etcd"

// ignores a relative dir ("-" would read as a flag) and ".." dirs Clean and the kernel disagree on
func (c *ClusterConfig) SnapshotDir() string {
	if d := c.Spec.Snapshot.Dir; filepath.IsAbs(d) && !hasParentElem(d) {
		return filepath.Clean(d)
	}
	return defaultSnapshotDir
}

func hasParentElem(d string) bool {
	return slices.Contains(strings.Split(d, string(filepath.Separator)), "..")
}

// LoadSnapshotDir reads the same config as LoadSnapshotConfig; unreadable yields the default.
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

// the default path records as empty; MutateSave re-reads, so a mid-start rejoin is not dropped
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

// a state error other than missing warns: the running config may not be this node's.
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

// LoadSnapshotConfig resolves as the loop does: the recorded cluster config, snapshot.conf winning.
func LoadSnapshotConfig() (etcdsnap.Config, error) {
	path, recorded := activeConfigPath(LoadState, os.Stderr)
	return loadSnapshotConfig(path, recorded, func(base etcdsnap.Config) (etcdsnap.Config, bool, error) {
		c, err := etcdsnap.LoadConfigOr(base)
		return c, c.FromFile(), err
	}, os.Stderr)
}

// loadSnapshotConfig treats a vanished recorded config as unreadable; warns only if defaults land.
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
	// the resolved dir keeps every caller acting where the scheduled loop writes
	if cerr == nil {
		warnRelativeSnapshotDir(cc, "warning: ")
		c.Dir = cc.SnapshotDir()
	} else {
		c.Dir = defaultSnapshotDir
	}
	return c, err
}

// scheduledSaveOptions prunes and uploads nothing when snapshot.conf is unreadable.
func scheduledSaveOptions(cfg *ClusterConfig, load func(etcdsnap.Config) (etcdsnap.Config, error), warn io.Writer) etcdsnap.SaveOptions {
	base := cfg.SnapshotBase()
	opts := etcdsnap.SaveOptions{Retain: base.Retain, S3: base.S3Target(), Dir: cfg.SnapshotDir(), Evidence: true}
	sc, err := load(base)
	if err == nil {
		opts.Retain, opts.S3 = sc.Retain, sc.S3Target()
		return opts
	}
	fmt.Fprintf(warn, "stratum: WARN: cannot read snapshot config, pruning skipped this tick (snapshot still taken and kept, no S3 upload): %v\n", err)
	opts.Retain, opts.S3 = 0, nil
	return opts
}

// tickWarnings collects one tick's warning lines for the snapshot evidence record.
type tickWarnings struct {
	lines []string
}

func (w *tickWarnings) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			w.lines = append(w.lines, line)
		}
	}
	return len(p), nil
}

func (w *tickWarnings) recorded() []string { return w.lines }

// retry the first save for one interval; a failed upload ends it: a retry would prune old snapshots
func snapshotLoop(ctx context.Context, cfg *ClusterConfig) {
	if ctx.Err() != nil {
		return
	}
	interval := snapshotInterval(cfg)
	started := time.Now()
	// a hanging save is cut off at the window's end; a window ending during a wait ends the wait
	deadline := started.Add(interval)
	startCtx, cancelStart := context.WithDeadline(ctx, deadline)
	defer cancelStart()
	warned := false
	for {
		path, err := snapshotSave(startCtx, cfg)
		if err == nil || path != "" {
			if err != nil {
				fmt.Fprintf(os.Stderr, "stratum: scheduled etcd snapshot: %v\n", err)
			}
			break
		}
		if !time.Now().Before(deadline) {
			fmt.Fprintf(os.Stderr, "stratum: scheduled etcd snapshot: %v\n", err)
			break
		}
		if failing := time.Since(started); !warned && failing >= bindingWarnAfter {
			warned = true
			fmt.Fprintf(os.Stderr, "stratum: WARN: scheduled etcd snapshot still failing after %v: %v; retrying every %v\n",
				failing.Round(time.Second), err, snapshotStartRetry)
		}
		timer := time.NewTimer(snapshotStartRetry)
		select {
		case <-startCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		if !time.Now().Before(deadline) {
			fmt.Fprintf(os.Stderr, "stratum: scheduled etcd snapshot: %v\n", err)
			break
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := snapshotSave(ctx, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "stratum: scheduled etcd snapshot: %v\n", err)
		}
	}
}

// EvidenceSample is one sample's summary; internal/backupevidence imports this package.
type EvidenceSample struct {
	// NodeUID is the immutable Kubernetes identity; empty means unknown.
	NodeUID string
	// Node is the node the sample's local half is scoped to.
	Node string
	// Complete says the whole read ran within its bounds.
	Complete bool
	// LocalArtifacts and RemoteCopies are the read's bounded counts.
	LocalArtifacts int
	RemoteCopies   int
	// Backups is the deduplicated total; Exact false means it is a floor, the true total unknown.
	Backups int
	Exact   bool
	// Warnings are the read's typed safe diagnostics.
	Warnings []string
}

// EvidenceSampler is the sample seam; ok false means nothing was sampled.
type EvidenceSampler func(ctx context.Context) (sample EvidenceSample, ok bool)

// seams: the sampler stands where internal/backupevidence.Evidence stands.
var (
	evidenceSampler         = noopEvidenceSampler
	evidencePublishInterval = time.Minute
)

// noopEvidenceSampler is the default: a start that wired no sampler records no evidence.
func noopEvidenceSampler(context.Context) (EvidenceSample, bool) {
	return EvidenceSample{}, false
}

// evidenceRecordSchemaVersion lets a later revision refuse a newer record.
const evidenceRecordSchemaVersion = 1

// evidenceRecordPath sits beside the snapshot evidence record, the node-local-record precedent.
func evidenceRecordPath() string {
	return filepath.Join(version.K8sConfDir, "backup-evidence.yaml")
}

// evidenceRecord is what the publisher last saw, persisted so it survives restarts.
type evidenceRecord struct {
	NodeUID        string    `json:"nodeUID"`
	SchemaVersion  int       `json:"schemaVersion"`
	SampledAt      time.Time `json:"sampledAt"`
	Node           string    `json:"node"`
	Complete       bool      `json:"complete"`
	LocalArtifacts int       `json:"localArtifacts"`
	RemoteCopies   int       `json:"remoteCopies"`
	Backups        int       `json:"backups"`
	ExactTotal     bool      `json:"exactTotal"`
	Warnings       []string  `json:"warnings,omitempty"`
}

// publishEvidenceLoop samples at once, then per cadence; an in-flight sample at shutdown joins.
func publishEvidenceLoop(ctx context.Context, sample EvidenceSampler) {
	if sample == nil {
		sample = noopEvidenceSampler
	}
	publishEvidenceSample(ctx, sample)
	t := time.NewTicker(evidencePublishInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			publishEvidenceSample(ctx, sample)
		}
	}
}

// publishEvidenceSample replaces the record whole; a cancelled sample records nothing.
func publishEvidenceSample(ctx context.Context, sample EvidenceSampler) {
	if ctx.Err() != nil {
		return
	}
	s, ok := sample(ctx)
	if !ok || ctx.Err() != nil {
		return
	}
	b, err := yaml.Marshal(evidenceRecord{
		SchemaVersion:  evidenceRecordSchemaVersion,
		SampledAt:      time.Now(),
		Node:           s.Node,
		NodeUID:        s.NodeUID,
		Complete:       s.Complete,
		LocalArtifacts: s.LocalArtifacts,
		RemoteCopies:   s.RemoteCopies,
		Backups:        s.Backups,
		ExactTotal:     s.Exact,
		Warnings:       s.Warnings,
	})
	if err == nil {
		// a start whose config area was wiped under a running controller still records
		err = os.MkdirAll(filepath.Dir(evidenceRecordPath()), 0o755)
	}
	if err == nil {
		// a torn record must never stand where a working one was, and it is not world-readable
		err = atomicfile.WriteFile(evidenceRecordPath(), b, 0o600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "stratum: record the backup evidence sample: %v (non-fatal)\n", err)
	}
}

// importPayloadImages imports the staged OCI archive once the socket is up.
func importPayloadImages(ctx context.Context) error {
	if err := ImportImages(ctx, filepath.Join(version.DataDir, assets.ImagesDir)); err != nil {
		return err
	}
	importOperatorImages(ctx, os.Stdout, os.Stderr)
	return nil
}

// importOperatorImages re-renders kubelet.conf after a new import, so kubelet exempts it.
func importOperatorImages(ctx context.Context, w, warn io.Writer) {
	if !importDropInImages(ctx, w, warn) {
		return
	}
	st, err := LoadState()
	if err == nil {
		err = writeKubeletConf(st)
	}
	if err != nil {
		fmt.Fprintf(warn, "stratum: warning: the operator images imported from %s are not exempt from kubelet's pull-credential check until the next start: render kubelet.conf: %v\n", version.ImagesDropInDir, err)
	}
}

// skips a bad archive (a dropped file must not fail kubelet); a stamp lands only once pins succeed
func importDropInImages(ctx context.Context, w, warn io.Writer) bool {
	tars, _ := filepath.Glob(filepath.Join(version.ImagesDropInDir, "*.tar"))
	if len(tars) == 0 {
		return false
	}
	sock := strings.TrimPrefix(version.ContainerdSock, "unix://")
	ctr := filepath.Join(version.BinDir, "ctr")
	names := map[string]bool{}
	imported := map[string]string{} // archive -> its stamp value before import
	for _, tar := range tars {
		before, err := os.Stat(tar)
		if err != nil {
			fmt.Fprintf(warn, "stratum: warning: skipping %s: %v\n", tar, err)
			continue
		}
		value := stampValue(before)
		current := runtime.DropInImported(tar)
		if !current {
			out, err := exec.CommandContext(ctx, ctr, "--address", sock, "-n", "k8s.io", "images", "import", tar).CombinedOutput()
			if err != nil {
				fmt.Fprintf(warn, "stratum: warning: skipping %s: import: %v\n%s", tar, err, out)
				continue
			}
			fmt.Fprintf(w, "stratum: imported operator images from %s\n", tar)
		}
		got, err := assets.ArchiveImageNames(tar)
		if err != nil {
			fmt.Fprintf(warn, "stratum: warning: skipping %s: read its image names: %v\n", tar, err)
			continue
		}
		if after, err := os.Stat(tar); err != nil || stampValue(after) != value {
			fmt.Fprintf(warn, "stratum: warning: skipping %s, which changed while it was imported; it is imported again on the next start\n", tar)
			continue
		}
		for _, n := range got {
			names[n] = true
		}
		if !current {
			imported[tar] = value
		}
	}
	if err := pinPayloadImages(ctx, ctr, sock, names); err != nil {
		fmt.Fprintf(warn, "stratum: warning: pin the operator images in %s, which are imported again on the next start: %v\n", version.ImagesDropInDir, err)
		return false
	}
	stamped := false
	for tar, value := range imported {
		if err := writeDropInStamp(tar, value); err != nil {
			fmt.Fprintf(warn, "stratum: warning: %s is not exempt from kubelet's pull-credential check, and is imported again on the next start: record its import: %v\n", tar, err)
			continue
		}
		stamped = true
	}
	return stamped
}

// writeDropInStamp records that the operator archive tar was imported and its images pinned.
func writeDropInStamp(tar, value string) error {
	stamp := runtime.DropInImportStamp(tar)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(stamp, []byte(value), 0o644)
}

// ImportImages imports an explicit archive dir; internal/upgrade stages the upgrade images there.
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
		// runs in kubelet's WaitFor on every restart: a size+mtime stamp keeps the re-check cheap
		if stampCurrent(tar) {
			continue
		}
		cmd := exec.CommandContext(ctx, ctr, "--address", sock, "-n", "k8s.io", "images", "import", tar)
		if out, err := cmd.CombinedOutput(); err != nil {
			// fails kubelet's WaitFor: a visible crash-loop, not a forever ImagePullBackOff on airgap
			return fmt.Errorf("import %s: %w\n%s", tar, err, out)
		}
		fmt.Fprintf(w, "stratum: imported payload images from %s\n", filepath.Base(tar))
		writeStamp(tar)
	}
	names, err := assets.ImageNames(tars)
	if err != nil {
		return err
	}
	// cheap to re-assert every start, and covers images imported before pinning existed
	return pinPayloadImages(ctx, ctr, sock, names)
}

// importWaitContainerd is a seam, so a test can run ImportImages against a fake ctr.
var importWaitContainerd = waitContainerd

// stampCurrent's stamp lives under the config dir: assets.Stage stages the images dir read-only.
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

func stampValue(fi os.FileInfo) string { return runtime.ImportStampValue(fi) }

// archived names plus engine-registry images, which catch aliases an earlier start made
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

// the alias serves CRI's name:tag@digest resolution; the pin keeps airgap images off kubelet's GC
func pinPayloadImages(ctx context.Context, ctr, sock string, archived map[string]bool) error {
	out, err := exec.CommandContext(ctx, ctr, "--address", sock, "-n", "k8s.io", "images", "ls").Output()
	if err != nil {
		return fmt.Errorf("pin payload images: list: %w", err)
	}
	for _, row := range payloadImageRows(string(out), archived) {
		ref, digest := row[0], row[1]
		names := []string{ref}
		// deriving repo by last-colon on a ref that carries "@" would split inside "@sha256:"
		if !strings.Contains(ref, "@") {
			repo := ref
			if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
				repo = ref[:i]
			}
			alias := repo + "@" + digest
			cmd := exec.CommandContext(ctx, ctr, "--address", sock, "-n", "k8s.io",
				"images", "tag", "--force", ref, alias)
			if tout, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("alias %s: %w\n%s", alias, err, tout)
			}
			names = append(names, alias)
		}
		for _, name := range names {
			cmd := exec.CommandContext(ctx, ctr, "--address", sock, "-n", "k8s.io",
				"images", "label", name, "io.cri-containerd.pinned=pinned")
			if lout, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("pin %s: %w\n%s", name, err, lout)
			}
		}
	}
	return nil
}

// default settings leave no file: a joining older binary must not meet an entry it cannot decode
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

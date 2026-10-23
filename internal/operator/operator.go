// Package operator is the in-binary SKE operator: the HelmChart /
// HelmChartConfig controller, the manifest deployer — leader-elected so a
// multi-controller cluster runs one active reconciler — and the kubelet CSR
// approvers, which run on every controller.
//
// It runs as a SUPERVISED HOST PROCESS (`stratum operator`, a child of `stratum
// up`), not as an in-cluster Deployment: no operator image to pull keeps the
// airgap story one-artifact.
//
// Rolling node upgrades are NOT here: `stratum upgrade --cluster`
// (internal/upgrade) is the one implementation, driven from a control-plane
// node rather than from a CR.
package operator

import (
	"context"
	"flag"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/netcfg"
)

const (
	// group is the domain of the operator's annotations and labels
	// (requester stamps, managed-by, stack names). That it coincides with the
	// HelmChart API group is spelled through the alias: the group/version pair
	// is declared once in internal/k8sutil (HelmChartGroup/HelmChartVersion),
	// and every GVR/GVK construction reads it from there.
	group        = k8sutil.HelmChartGroup
	fieldManager = "stratum-operator"

	// perIterationTimeout bounds the API calls made during a single reconcile
	// pass so a stuck apiserver call can't wedge the loop.
	perIterationTimeout = 30 * time.Second

	// inventorySyncInterval spaces the per-instance prune-inventory sync: a
	// cheap ConfigMap read-modify-write that unions the local .applied.json
	// into cluster state and retires the file once the migration closes.
	inventorySyncInterval = 30 * time.Second
)

// Options for Run, mirroring the `stratum operator` flags.
type Options struct {
	Kubeconfig   string
	ManifestsDir string
	HelmCache    string
	LeaderElect  bool
	Interval     time.Duration
	// PrivilegedNamespaces are the namespaces whose HelmCharts may install into a
	// DIFFERENT targetNamespace. A HelmChart in any other namespace is confined to
	// its own namespace, so a namespaced tenant CR cannot use the operator's
	// cluster-admin install creds to reach across namespaces.
	PrivilegedNamespaces []string
	// AllowInsecureHelmRepos permits a HelmChart's spec.insecureSkipTLSVerify to be
	// honored. Default false: a tenant CR cannot disable repo TLS verification on
	// its own.
	AllowInsecureHelmRepos bool
	// NodeIP is this control-plane node's address, used as the admission webhook's
	// EndpointSlice endpoint so the apiserver can reach the (hostNetwork) webhook
	// server. Empty disables the webhook on this instance — the reconcile
	// backstop still refuses unstamped non-privileged installs, so this fails
	// closed rather than silently dropping requester scoping.
	NodeIP string
	// APIServerVIP is the cluster VIP, when there is one. With it, the deployer
	// points Cilium at the VIP; without it, at every control plane.
	APIServerVIP string
	// ClusterDomain is the cluster's DNS domain, for the dashboard's and the
	// admission webhook's Service names.
	ClusterDomain string
}

// ParseFlags builds Options from `stratum operator` args.
func ParseFlags(args []string) (Options, error) {
	fs := flag.NewFlagSet("operator", flag.ContinueOnError)
	o := Options{}
	fs.StringVar(&o.Kubeconfig, "kubeconfig", "", "kubeconfig path (empty: in-cluster)")
	fs.StringVar(&o.ManifestsDir, "manifests-dir", "", "directory of *.yaml stacks to server-side-apply and prune")
	fs.StringVar(&o.HelmCache, "helm-cache", "", "Helm repo/chart cache dir")
	fs.BoolVar(&o.LeaderElect, "leader-elect", true, "campaign for the kube-system/stratum-operator Lease")
	fs.DurationVar(&o.Interval, "interval", 20*time.Second, "reconcile interval")
	privileged := fs.String("privileged-namespaces", "kube-system",
		"comma-separated namespaces whose HelmCharts may target another namespace; all others are confined to their own")
	fs.BoolVar(&o.AllowInsecureHelmRepos, "allow-insecure-helm-repos", false,
		"permit a HelmChart's spec.insecureSkipTLSVerify to disable repo TLS verification (default: ignore it)")
	fs.StringVar(&o.NodeIP, "node-ip", "",
		"this node's IP; serves the requester-stamping admission webhook and registers its endpoint. Empty disables the webhook on this node")
	fs.StringVar(&o.APIServerVIP, "apiserver-vip", "",
		"the cluster VIP, if any; Cilium's bundled manifest is pointed at it rather than at the control planes")
	fs.StringVar(&o.ClusterDomain, "cluster-domain", netcfg.Default().ClusterDomain,
		"the cluster's DNS domain; names the dashboard's and the admission webhook's Services in their serving certificates")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	o.PrivilegedNamespaces = splitNamespaces(*privileged)
	return o, nil
}

// splitNamespaces parses a comma-separated namespace list, trimming spaces and
// dropping empties.
func splitNamespaces(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Run reconciles until ctx is cancelled.
func Run(ctx context.Context, o Options) error {
	// One normalization for both consumers of the privileged set:
	// the webhook's exemption selector and install authorization must read the
	// same value, so Run normalizes here rather than trusting either consumer
	// to do it — an Options built outside ParseFlags would otherwise mean two
	// things in one process. Idempotent over the flag-parsed set.
	o.PrivilegedNamespaces = normalizePrivilegedNamespaces(o.PrivilegedNamespaces)
	cfg, err := buildConfig(o.Kubeconfig)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	// The manifest deployer and the CoreDNS spreader are values, not package
	// state: one of each per Run, owned by the leader-loop steps below, so
	// their apply state and cooldown are theirs alone and a restart starts
	// clean.
	deployer, err := newManifestDeployer(cfg, o.ManifestsDir)
	if err != nil {
		return err
	}
	spreader := &corednsSpreader{}
	if o.HelmCache != "" {
		_ = os.Setenv("HELM_CACHE_HOME", o.HelmCache)
	}

	log.Printf("stratum-operator: reconciling HelmCharts + manifests every %s (leader-elect=%t)",
		o.Interval, o.LeaderElect)

	// The CSR approver and the admission webhook both run on every candidate (not
	// leader-gated): kubelet serving certs must renew even during a leadership gap,
	// and requester stamping is stateless, so serving it from every control-plane
	// operator makes admission HA without leader coupling. Their lifetime belongs
	// to this invocation, including a return caused by leadership loss while the
	// caller's context remains active — so both are joined before Run returns.
	ctx, cancel := context.WithCancel(ctx)
	var done []chan struct{}
	defer func() {
		cancel()
		for _, d := range done {
			<-d
		}
	}()

	csrDone := make(chan struct{})
	done = append(done, csrDone)
	go func() {
		defer close(csrDone)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			reconcileNodeClientCSRs(ctx, cs, time.Now())
			reconcileServingCSRs(ctx, cs)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	// The prune-inventory sync runs on every instance, leader or not, on its
	// own interval beside the CSR approvers: each instance unions its local
	// .applied.json into its node's entry in the stratum-addon-inventory
	// ConfigMap and retires the file once the migration to the cluster
	// inventory closes. Leader-gating it would stall the migration on any
	// cluster whose leadership never rotates across all control planes — the
	// leader's manifest pass can publish only its own node's entry, so the
	// followers' contributions have nowhere else to come from. An instance
	// with no manifests directory makes no claim: syncInventory contributes
	// nothing for it, and its node stays an expected contributor the
	// migration waits on (or the recovery entry closes the migration by
	// hand).
	if o.ManifestsDir != "" {
		nodeName := deployer.node
		inventoryDone := make(chan struct{})
		done = append(done, inventoryDone)
		go func() {
			defer close(inventoryDone)
			t := time.NewTicker(inventorySyncInterval)
			defer t.Stop()
			for {
				syncCtx, cancel := context.WithTimeout(ctx, perIterationTimeout)
				if err := syncInventory(syncCtx, cs, o.ManifestsDir, nodeName); err != nil {
					log.Printf("stratum-operator: inventory sync: %v", err)
				}
				cancel()
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}

	// Start CSR protection before waiting for the webhook's signer key. That
	// lookup may retry while an older unrestricted approval binding still exists.
	// The signer is used only by the webhook and manifest reconciler.
	//
	// The key is loaded once up front (six attempts against a transient
	// apiserver), then kept under observation for the process lifetime: the
	// owner below polls the signing Secret, republishes a replaced Secret's key
	// without a restart, retains the previous verifier set for stamps already
	// stored, and keeps retrying in the background so the operator is never left
	// keyless-and-silent. The webhook and the reconciler read a snapshot from
	// the holder per admission and per reconcile pass.
	signerHolder := newRequesterSignerHolder()
	if signer := loadRequesterSignerWithRetry(ctx, cs); signer != nil {
		signerHolder.publish(signer)
	} else {
		log.Printf("stratum-operator: requester-signing key unavailable; requester-scoped installs will be refused until the signer owner loads it in the background")
	}
	signerDone := make(chan struct{})
	done = append(done, signerDone)
	go func() {
		defer close(signerDone)
		maintainRequesterSigner(ctx, cs, signerHolder, requesterKeyWatchInterval)
	}()

	// Admission webhook: stamps the requester onto HelmChart CRs so the
	// reconciler can install with the requester's authority. Skipped when --node-ip
	// is unset (the backstop still refuses unstamped non-privileged installs).
	if o.NodeIP != "" {
		nodeName, _ := os.Hostname()
		webhookDone := make(chan struct{})
		done = append(done, webhookDone)
		go func() {
			defer close(webhookDone)
			runWebhook(ctx, cs, nodeName, o.NodeIP, o.ClusterDomain, signerHolder, o.PrivilegedNamespaces)
		}()
	}

	helmStep := func(loopCtx context.Context) {
		refreshed := false
		reconcileHelmCharts(loopCtx, dyn, o.Kubeconfig, helmPolicy{
			privilegedNamespaces: o.PrivilegedNamespaces,
			allowInsecureRepos:   o.AllowInsecureHelmRepos,
			// Successful verification uses this immutable snapshot. A stamp from
			// a faster-refreshing webhook gets one trusted refresh before refusal.
			signer: signerHolder.current(),
			refreshSigner: func(refreshCtx context.Context) *requesterSigner {
				if !refreshed {
					refreshed = true
					refreshCtx, cancel := context.WithTimeout(refreshCtx, perIterationTimeout)
					defer cancel()
					refreshRequesterSigner(refreshCtx, cs, signerHolder)
				}
				return signerHolder.current()
			},
		})
	}
	dash := newDashboardSecrets(cs, o.APIServerVIP, o.ClusterDomain)
	deployerStep := func(deployerCtx context.Context) {
		deployer.reconcileManifests(deployerCtx, o.APIServerVIP, o.NodeIP)
		reconcileDashboard(deployerCtx, dash, o.ManifestsDir)
	}
	spreadStep := func(spreadCtx context.Context) {
		spreader.reconcileCoreDNSSpread(spreadCtx, cs, time.Now())
	}
	cleanupStep := cleanupStepFor(cs)

	if o.LeaderElect {
		id, _ := os.Hostname()
		return runLeaderElected(ctx, cfg, id, func(loopCtx context.Context) {
			runLeaderLoop(loopCtx, o.Interval, o.ManifestsDir != "", o.LeaderElect, helmStep, deployerStep, spreadStep, cleanupStep)
		})
	}
	runLeaderLoop(ctx, o.Interval, o.ManifestsDir != "", o.LeaderElect, helmStep, deployerStep, spreadStep, cleanupStep)
	return ctx.Err()
}

// leaderStep is one step of the leader's reconcile work. It receives the
// context it must run under: the loop's context for HelmChart work, which
// each chart's own spec.timeout bounds, or a per-iteration deadline for a
// cluster controller.
type leaderStep func(ctx context.Context)

// cleanupStepFor binds the pass's cleanup step — the cluster work that runs on
// every pass regardless of manifests or leadership — to cs: removing departed
// workers' Nodes, and pruning the stratum-certs grant entries whose bootstrap
// token is gone, so the multi-entry Secret every mint appends to does not grow
// one bundle-sized entry per expired or revoked token.
func cleanupStepFor(cs kubernetes.Interface) leaderStep {
	return func(cleanupCtx context.Context) {
		reconcileDepartedWorkers(cleanupCtx, cs, time.Now())
		reconcileGrantEntries(cleanupCtx, cs)
	}
}

// runLeaderLoop drives the leader's reconcile work as two loops on the same
// interval, each running a first pass immediately — a new leader applies the
// bundled add-ons without waiting a tick — and then on every tick of
// interval, until ctx is cancelled.
//
// HelmChart reconciliation runs on its own goroutine and ticker: an install
// is bounded by its chart's spec.timeout rather than the iteration window, so
// a slow chart pass must not hold the cluster controllers behind it, and the
// loop starts the next chart pass only after the previous one returns, so no
// chart is reconciled twice at once. The shared pass runs the manifests
// deployer, when a manifests directory is set, under a deadline that starts
// when the deployer itself starts, so a slow HelmChart pass cannot spend its
// budget; CoreDNS spreading, when one is set and leader election is on — so
// two operators never both evict a replica — and departed-worker cleanup,
// which runs on the leader only, each with a fresh deadline of their own,
// which a slow manifest pass cannot exhaust. A panic in any step is
// recovered, so one broken step stops neither the later steps nor the loops.
//
// runLeaderLoop returns only after the helm loop has returned: the leader
// callback must not return while HelmChart work is still running, so the
// elector — which joins the callback before reporting leadership loss — never
// reports loss while this process is still installing charts.
func runLeaderLoop(ctx context.Context, interval time.Duration, manifestsSet, leaderElected bool, helmStep, deployerStep, spreadStep, cleanupStep leaderStep) {
	// The helm loop, on its own goroutine and ticker, joined before this
	// returns — the pattern of the webhook goroutine in Run.
	helmDone := make(chan struct{})
	go func() {
		defer close(helmDone)
		helmTicker := time.NewTicker(interval)
		defer helmTicker.Stop()
		for {
			safe(func() { helmStep(ctx) })
			select {
			case <-ctx.Done():
				return
			case <-helmTicker.C:
			}
		}
	}()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		func() {
			// Its own deadline, created when the deployer starts: HelmChart
			// work is not bounded by the iteration window, so a deadline
			// anchored to the pass's start would be spent before the
			// deployer ran at all.
			if manifestsSet {
				safe(func() {
					deployerCtx, cancel := context.WithTimeout(ctx, perIterationTimeout)
					defer cancel()
					deployerStep(deployerCtx)
				})
			}
			// Its own deadline, not the deployer's, which a slow manifest
			// pass can exhaust.
			if manifestsSet && leaderElected {
				safe(func() {
					spreadCtx, cancel := context.WithTimeout(ctx, perIterationTimeout)
					defer cancel()
					spreadStep(spreadCtx)
				})
			}
			// A delete another operator already made is NotFound.
			safe(func() {
				nodesCtx, cancel := context.WithTimeout(ctx, perIterationTimeout)
				defer cancel()
				cleanupStep(nodesCtx)
			})
		}()
		select {
		case <-ctx.Done():
			// Join the helm loop before returning: the leader callback must
			// not end while HelmChart work is still running.
			<-helmDone
			return
		case <-t.C:
		}
	}
}

func exists(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string) bool {
	_, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

func nested(obj map[string]interface{}, fields ...string) interface{} {
	v, _, _ := unstructured.NestedFieldNoCopy(obj, fields...)
	return v
}

func str(v interface{}, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func asInt(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i
		}
	}
	return 0
}

func safe(f func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("reconcile panic recovered: %v", r)
		}
	}()
	f()
}

// buildConfig uses the kubeconfig file if present, else in-cluster config (so the
// operator runs both as a host process and as an in-cluster Deployment + SA).
func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		if _, err := os.Stat(kubeconfig); err == nil {
			return clientcmd.BuildConfigFromFlags("", kubeconfig)
		}
	}
	return rest.InClusterConfig()
}

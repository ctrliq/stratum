// Package operator is the in-binary SKE operator, run as a supervised host process.
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
	group        = k8sutil.HelmChartGroup
	fieldManager = "stratum-operator"

	// bounds one reconcile pass so a stuck apiserver call cannot wedge the loop.
	perIterationTimeout = 30 * time.Second

	inventorySyncInterval = 30 * time.Second
)

// Options for Run, mirroring the `stratum operator` flags.
type Options struct {
	Kubeconfig   string
	ManifestsDir string
	HelmCache    string
	LeaderElect  bool
	Interval     time.Duration
	// namespaces whose HelmCharts may target another namespace; others are confined to their own.
	PrivilegedNamespaces []string
	// permits a HelmChart's spec.insecureSkipTLSVerify to be honored. Default false.
	AllowInsecureHelmRepos bool
	// empty disables the webhook; the reconcile backstop still refuses unstamped installs.
	NodeIP        string
	APIServerVIP  string
	ClusterDomain string
}

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
	// one normalization so the webhook exemption and install authorization read the same value.
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
	// per-Run values, not package state: a restart starts clean.
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

	// not leader-gated: serving certs must renew even during a leadership gap.
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

	// not leader-gated: follower contributions have nowhere else to come from.
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

	// the signer-key lookup may retry while an older unrestricted approval binding exists.
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
			// a stamp from a faster-refreshing webhook gets one trusted refresh before refusal.
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

type leaderStep func(ctx context.Context)

func cleanupStepFor(cs kubernetes.Interface) leaderStep {
	return func(cleanupCtx context.Context) {
		reconcileDepartedWorkers(cleanupCtx, cs, time.Now())
		reconcileGrantEntries(cleanupCtx, cs)
	}
}

func runLeaderLoop(ctx context.Context, interval time.Duration, manifestsSet, leaderElected bool, helmStep, deployerStep, spreadStep, cleanupStep leaderStep) {
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
			// a deadline anchored to the pass's start would be spent before the deployer ran.
			if manifestsSet {
				safe(func() {
					deployerCtx, cancel := context.WithTimeout(ctx, perIterationTimeout)
					defer cancel()
					deployerStep(deployerCtx)
				})
			}
			// not the deployer's deadline, which a slow manifest pass can exhaust.
			if manifestsSet && leaderElected {
				safe(func() {
					spreadCtx, cancel := context.WithTimeout(ctx, perIterationTimeout)
					defer cancel()
					spreadStep(spreadCtx)
				})
			}
			// the elector joins the callback before reporting leadership loss; join the helm loop first.
			safe(func() {
				nodesCtx, cancel := context.WithTimeout(ctx, perIterationTimeout)
				defer cancel()
				cleanupStep(nodesCtx)
			})
		}()
		select {
		case <-ctx.Done():
			// the leader callback must not end while HelmChart work is still running.
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

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		if _, err := os.Stat(kubeconfig); err == nil {
			return clientcmd.BuildConfigFromFlags("", kubeconfig)
		}
	}
	return rest.InClusterConfig()
}

package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"helm.sh/helm/v4/pkg/action"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/release/common"
	helmrelease "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
	"helm.sh/helm/v4/pkg/strvals"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/version"
)

var helmCacheDir = version.HelmCacheDir

// A var so a test can point it at a scratch directory.
var legacyHelmCacheDir = version.LegacyHelmCacheDir

const (
	helmFinalizer = "stratum.ciq.com/helmchart"

	hcPhasePending      = "Pending"
	hcPhaseDeployed     = "Deployed"
	hcPhaseFailed       = "Failed"
	hcPhaseUninstalling = "Uninstalling"

	// keeps a broken repo/values combo from hammering the apiserver every tick.
	failedBackoff  = 60 * time.Second
	maxHelmHistory = 5

	// helm v4 fails at runtime with an empty wait strategy; v3WaitClient answers this one.
	helmWaitStrategy = kube.HookOnlyStrategy

	// no structured field for these objects, so tests pin this exact text.
	helmUnverifiedNote = "ownership could not be verified"

	// helm purges the release first, so this status is the only record objects remain.
	uninstallIncompletePrefix = "uninstall incomplete: "
	uninstallingMessage       = "uninstalling release"
	// records the outcome, so writing it clears unrecordedRecoveryField.
	uninstalledMessage         = "release uninstalled; removing finalizer"
	uninstallIncompleteDelete  = uninstallIncompletePrefix + "remove these objects by hand, then the " + helmFinalizer + " finalizer: "
	uninstallIncompleteRecover = uninstallIncompletePrefix + "remove these objects by hand, then change the chart source (spec.repo, spec.chart, spec.version or spec.chartContent) or the merged values (spec.valuesContent, spec.set, spec.setString, or the same-name HelmChartConfig's valuesContent) to reinstall; other spec fields, and values edits that leave the merged values the same, do not reinstall, except on a HelmChart with spec.valuesSecrets, where any edit to those values fields or to a Secret it names reinstalls: "

	// must not start with uninstallIncompletePrefix, or an incomplete-write outage would fail it too.
	recoveringUninstallMarker = "uninstalling release to recover"
	// recording it clears unrecordedRecoveryField.
	recoveringPendingMessage = "recovering pending release"
)

var (
	helmChartGVR       = schema.GroupVersionResource{Group: k8sutil.HelmChartGroup, Version: k8sutil.HelmChartVersion, Resource: "helmcharts"}
	helmChartConfigGVR = schema.GroupVersionResource{Group: k8sutil.HelmChartGroup, Version: k8sutil.HelmChartVersion, Resource: "helmchartconfigs"}
	secretGVR          = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
)

type helmSpec struct {
	chart                 string
	repo                  string
	version               string
	chartContent          string // base64 .tgz
	releaseName           string
	targetNamespace       string
	createNamespace       bool
	valuesContent         string // YAML
	set                   map[string]string
	setString             map[string]string // --set-string semantics: never type-inferred
	valuesSecrets         []valuesSecretRef // values.yaml bodies read from Secrets in the CR namespace
	timeout               time.Duration
	repoCA                string // PEM
	insecureSkipTLSVerify bool
}

type helmChartSpecField struct {
	name string
	kind string
	def  interface{}
}

// single declaration of the .spec contract; the CRD test pins it row for row.
var helmChartSpecTable = []helmChartSpecField{
	{name: "chart", kind: "string"},
	{name: "repo", kind: "string"},
	{name: "version", kind: "string"},
	{name: "chartContent", kind: "string"},
	{name: "releaseName", kind: "string"},
	{name: "targetNamespace", kind: "string"},
	{name: "createNamespace", kind: "boolean", def: false},
	{name: "valuesContent", kind: "string"},
	{name: "set", kind: "object"},
	{name: "setString", kind: "object"},
	{name: "valuesSecrets", kind: "array"},
	{name: "timeout", kind: "string", def: "5m"},
	{name: "repoCA", kind: "string"},
	{name: "insecureSkipTLSVerify", kind: "boolean", def: false},
}

// zero value is most restrictive: no privileged namespaces, insecure refused.
type helmPolicy struct {
	privilegedNamespaces []string
	allowInsecureRepos   bool
	// nil disables verification: every non-privileged install is then refused.
	signer *requesterSigner
	// reloads verification material before refusing a stamp a key refresh raced with.
	refreshSigner func(context.Context) *requesterSigner
}

// retries a lagging verifier once before an authentication refusal.
func (pol helmPolicy) verificationSigner(ctx context.Context, objects ...*unstructured.Unstructured) *requesterSigner {
	if pol.refreshSigner == nil {
		return pol.signer
	}
	for _, obj := range objects {
		if obj == nil || !impersonationRequired(obj.GetNamespace(), pol.privilegedNamespaces) {
			continue
		}
		_, sig, present, err := requesterFromCR(obj)
		if err != nil || !present || (sig == "" && obj.GetAnnotations()[annRequesterSigV2] == "") {
			continue
		}
		if _, authentic, _ := verifiedRequester(obj, pol.signer); !authentic {
			return pol.refreshSigner(ctx)
		}
	}
	return pol.signer
}

// reads controller-written ownership: tightened policy must not orphan a release.
func uninstallTarget(status map[string]interface{}) (relNS, relName string, ok bool) {
	relNS = str(status["releaseNamespace"], "")
	relName = str(status["releaseName"], "")
	return relNS, relName, relNS != "" && relName != ""
}

func targetNamespaceAllowed(crNS, target string, privileged []string) bool {
	if target == "" || target == crNS {
		return true
	}
	return slices.Contains(privileged, crNS)
}

func reconcileHelmCharts(rootCtx context.Context, dyn dynamic.Interface, kubeconfig string, pol helmPolicy) {
	// bound only the listing; each chart's Helm work gets its own spec.timeout budget.
	listCtx, cancel := context.WithTimeout(rootCtx, perIterationTimeout)
	defer cancel()

	list, err := dyn.Resource(helmChartGVR).Namespace("").List(listCtx, metav1.ListOptions{})
	if err != nil {
		return // CRD may not be installed yet
	}

	// the object is kept, not just its values: the requester check verifies its stamp.
	cl, err := dyn.Resource(helmChartConfigGVR).Namespace("").List(listCtx, metav1.ListOptions{})
	if err != nil {
		// without the overlays every merged-values hash is wrong; skip the iteration.
		log.Printf("helmchart: list helmchartconfigs: %v", err)
		return
	}
	configs := map[string]*unstructured.Unstructured{}
	for i := range cl.Items {
		c := &cl.Items[i]
		configs[c.GetNamespace()+"/"+c.GetName()] = c
	}

	for i := range list.Items {
		hc := &list.Items[i]
		hcc := configs[hc.GetNamespace()+"/"+hc.GetName()]
		hccValues := ""
		if hcc != nil {
			hccValues = str(nested(hcc.Object, "spec", "valuesContent"), "")
		}
		reconcileHelmChart(rootCtx, dyn, kubeconfig, hc, hccValues, hcc, pol)
	}
}

func reconcileHelmChart(rootCtx context.Context, dyn dynamic.Interface, kubeconfig string, hc *unstructured.Unstructured, hccValues string, hcc *unstructured.Unstructured, pol helmPolicy) {
	ns, name := hc.GetNamespace(), hc.GetName()
	spec := parseHelmSpec(hc)
	allowed := targetNamespaceAllowed(ns, spec.targetNamespace, pol.privilegedNamespaces)

	// normalize the effective TLS setting up front so every status write reflects it.
	requestedInsecure := spec.insecureSkipTLSVerify
	spec.insecureSkipTLSVerify = requestedInsecure && pol.allowInsecureRepos

	// persist the pre-field hold before any write replaces its message.
	priorStatus, _, _ := unstructured.NestedMap(hc.Object, "status")
	if uninstallIncomplete(priorStatus) {
		if _, ok := incompleteHoldFrom(priorStatus); !ok {
			if err := migrateLegacyIncompleteHold(rootCtx, dyn, ns, name); err != nil {
				log.Printf("helmchart %s/%s: persist earlier uninstall-incomplete hold: %v", ns, name, err)
				return
			}
		}
	}
	// a purge without its outcome write must retry before it reads as a hold.
	if saved, ok := cleanRecoveryOutcomes.Load(incompleteKey(hc)); ok {
		if hc.GetDeletionTimestamp() != nil && !hasFinalizer(hc, helmFinalizer) {
			cleanRecoveryOutcomes.Delete(incompleteKey(hc))
			return
		}
		outcome := saved.(cleanRecoveryOutcome)
		if err := writeHelmStatus(rootCtx, dyn, ns, name, hcPhasePending, outcome.spec, outcome.hash, outcome.revision, "", recoveringPendingMessage, true); err != nil {
			if apierrors.IsNotFound(err) {
				cleanRecoveryOutcomes.Delete(incompleteKey(hc))
			}
			log.Printf("helmchart %s/%s: retry the clean recovery outcome: %v", ns, name, err)
			return
		}
		cleanRecoveryOutcomes.Delete(incompleteKey(hc))
		return // next tick reads the fresh status
	}

	if hc.GetDeletionTimestamp() != nil {
		if !hasFinalizer(hc, helmFinalizer) {
			return // already cleaned up / never adopted
		}
		// the incomplete-uninstall status write failed then; retry it before the finalizer moves.
		if rec, ok := unwrittenIncompleteFor(hc); ok {
			log.Printf("helmchart %s/%s: an earlier uninstall left objects Helm could not verify; retaining finalizer and retrying the status write: %q", ns, name, rec.note)
			recordUninstallIncomplete(rootCtx, dyn, hc, spec, deletionIncompleteMessage(rec.msg), rec.note)
			return
		}
		// uninstall the adopted release from status, never the tenant-writable spec.
		curStatus, _, _ := unstructured.NestedMap(hc.Object, "status")
		relNS, relName, doUninstall := uninstallTarget(curStatus)
		if !doUninstall {
			// adoption precedes Helm work; no recorded identity means no attempt.
			log.Printf("helmchart %s/%s: no recorded release; removing finalizer only", ns, name)
			if err := removeFinalizer(rootCtx, dyn, helmChartGVR, ns, name, helmFinalizer); err != nil {
				log.Printf("helmchart %s/%s: remove finalizer: %v", ns, name, err)
			}
			return
		}
		if uninstallIncomplete(curStatus) {
			if hold, ok := incompleteHoldFrom(curStatus); ok {
				// the hold outlives its message; regenerate the delete-form text from the field.
				hold.path = holdPathDelete
				if want := incompleteHoldMessage(hold); want != str(curStatus["message"], "") {
					setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", want)
				}
			} else if msg := str(curStatus["message"], ""); !strings.HasPrefix(msg, uninstallIncompleteDelete) {
				// a pre-field hold: derive the delete form from the message.
				setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", deletionIncompleteMessage(msg))
			}
			log.Printf("helmchart %s/%s: an earlier uninstall left objects Helm could not verify; retaining finalizer until a person removes them and the finalizer (see status.message)", ns, name)
			return
		}
		pol.signer = pol.verificationSigner(rootCtx, hc)
		uninstallImp, refuse, reason := uninstallIdentity(hc, pol.privilegedNamespaces, pol.signer)
		if refuse {
			log.Printf("helmchart %s/%s: retaining finalizer: %s", ns, name, reason)
			return
		}
		// a pre-Helm marker with no recorded result proves nothing once Helm purges the release.
		_, clean := cleanUninstalls.Load(incompleteKey(hc))
		markerUnrecorded := (str(curStatus["phase"], "") == hcPhaseUninstalling && str(curStatus["message"], "") == uninstallingMessage) ||
			(str(curStatus["phase"], "") == hcPhasePending && str(curStatus["message"], "") == recoveringUninstallMarker) ||
			asBool(curStatus[unrecordedRecoveryField])
		unrecorded := !clean && markerUnrecorded
		acfg, err := newActionConfigFn(rootCtx, kubeconfig, relNS, uninstallImp)
		if err != nil {
			log.Printf("helmchart %s/%s: action config: %v", ns, name, err)
			return // keep finalizer; retry next tick
		}
		if err := writeHelmStatus(rootCtx, dyn, ns, name, hcPhaseUninstalling, spec, "", 0, "", uninstallingMessage, false); err != nil {
			log.Printf("helmchart %s/%s: record the uninstall before Helm work: %v", ns, name, err)
			return // keep finalizer; retry next tick
		}
		un := action.NewUninstall(acfg)
		un.WaitStrategy = helmWaitStrategy
		un.Timeout = spec.timeout
		note, unverified, err := uninstallRelease(un, relName)
		if unverified != "" {
			// checked before err: Helm purges the release even when it also errors.
			errNote := ""
			if err != nil {
				errNote = fmt.Sprintf(" (err: %v)", err)
			}
			log.Printf("helmchart %s/%s: uninstall %s/%s left objects Helm could not verify; retaining finalizer%s: %q", ns, name, relNS, relName, errNote, note)
			recordUninstallIncomplete(rootCtx, dyn, hc, spec, uninstallIncompleteDelete+unverified, note)
			return
		}
		if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
			log.Printf("helmchart %s/%s: uninstall %s/%s: %v", ns, name, relNS, relName, err)
			return // keep finalizer; retry next tick
		}
		if err != nil && unrecorded {
			log.Printf("helmchart %s/%s: release %s/%s is gone but an earlier uninstall never recorded its result; retaining finalizer", ns, name, relNS, relName)
			recordUninstallIncomplete(rootCtx, dyn, hc, spec, uninstallIncompleteDelete+fmt.Sprintf("any objects of release %s/%s that remain (an earlier uninstall ended without recording what it left)", relNS, relName), "")
			return
		}
		if note != "" {
			log.Printf("helmchart %s/%s: uninstall %s/%s kept objects: %q", ns, name, relNS, relName, note)
		}
		// replace the marker so a failed finalizer removal cannot read as unrecorded.
		if err := writeHelmStatus(rootCtx, dyn, ns, name, hcPhaseUninstalling, spec, "", 0, "", uninstalledMessage, false); err != nil {
			cleanUninstalls.Store(incompleteKey(hc), struct{}{})
		}
		if err := removeFinalizer(rootCtx, dyn, helmChartGVR, ns, name, helmFinalizer); err != nil {
			log.Printf("helmchart %s/%s: remove finalizer: %v", ns, name, err)
			return
		}
		cleanUninstalls.Delete(incompleteKey(hc))
		log.Printf("helmchart %s/%s: release %s/%s uninstalled, finalizer removed", ns, name, relNS, relName)
		return
	}

	if rec, ok := unwrittenIncompleteFor(hc); ok {
		log.Printf("helmchart %s/%s: an earlier uninstall left objects Helm could not verify; not reinstalling, retrying the status write: %q", ns, name, rec.note)
		recordUninstallIncomplete(rootCtx, dyn, hc, spec, rec.msg, rec.note)
		return
	}

	// bounds the release namespace only; a chart's templates can still render cluster-scoped objects.
	if !allowed {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "",
			fmt.Sprintf("refusing to install: spec.targetNamespace %q is outside this HelmChart's namespace %q — a HelmChart may install only into its own namespace (only privileged namespaces %v may target another)",
				spec.targetNamespace, ns, pol.privilegedNamespaces))
		return
	}

	if !hasFinalizer(hc, helmFinalizer) {
		if err := addFinalizer(rootCtx, dyn, helmChartGVR, ns, name, helmFinalizer); err != nil {
			log.Printf("helmchart %s/%s: add finalizer: %v", ns, name, err)
		}
		return
	}

	// a Secret's writer need not write the HelmChart, so their values would steer the release.
	var secretValues []map[string]interface{}
	var secretVersions []string
	if len(spec.valuesSecrets) > 0 {
		if impersonationRequired(ns, pol.privilegedNamespaces) {
			setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", fmt.Sprintf(
				"refusing to install: spec.valuesSecrets is honoured only in a privileged namespace %v, where the chart installs as the operator; here a Secret's writer would steer the release with this HelmChart's requester's authority", pol.privilegedNamespaces))
			return
		}
		// marking a Deployed chart Failed would upgrade it with the same values next pass.
		deployed := str(nested(hc.Object, "status", "phase"), "") == hcPhaseDeployed
		var err error
		secretValues, secretVersions, err = readValuesSecrets(rootCtx, dyn, ns, spec.valuesSecrets)
		if err != nil && deployed && retryableSecretRead(err) {
			log.Printf("helmchart %s/%s: %v; keeping the deployed release, retrying next pass", ns, name, err)
			return
		}
		if err != nil {
			setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", "values: "+err.Error())
			return
		}
	}

	vals, err := mergedValues(spec, hccValues, secretValues...)
	if err != nil {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", "values: "+err.Error())
		return
	}
	hash := specHash(spec, vals)
	if len(spec.valuesSecrets) > 0 {
		// the status hash covers the Secrets' versions, not the values merged from them.
		hash = secretSpecHash(spec, hccValues, secretVersions)
	}

	status, _, _ := unstructured.NestedMap(hc.Object, "status")
	if status != nil {
		curHash := str(status["specHash"], "")
		curPhase := str(status["phase"], "")
		if curHash == hash && curPhase == hcPhaseDeployed {
			rev := asInt(status["revision"])
			relSecret := fmt.Sprintf("sh.helm.release.v1.%s.v%d", spec.releaseName, rev)
			if rev > 0 && exists(rootCtx, dyn, secretGVR, spec.targetNamespace, relSecret) {
				// the operator flag is deliberately not part of specHash: flipping it must not upgrade every chart.
				if current, present := status["insecureTLSVerify"].(bool); !present || current != spec.insecureSkipTLSVerify {
					if err := refreshHelmTLSStatus(rootCtx, dyn, ns, name, spec.insecureSkipTLSVerify); err != nil {
						log.Printf("helmchart %s/%s: refresh TLS status: %v", ns, name, err)
					}
				}
				return // converged
			}
		}
		// the hold outlives any later Failed write that replaced its message.
		if hold, ok := incompleteHoldFrom(status); ok {
			if hold.specHash == "" || hold.specHash == hash {
				// regenerate the hold's message from the field for the same reason.
				if want := incompleteHoldMessage(hold); want != str(status["message"], "") {
					setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", want)
				}
				log.Printf("helmchart %s/%s: an earlier uninstall left objects Helm could not verify; not reinstalling until a person removes them and changes the spec (see status.message)", ns, name)
				return
			}
			// the specHash changed: the spec edit the hold waits for arrived.
		} else if curHash == hash && uninstallIncomplete(status) {
			log.Printf("helmchart %s/%s: an earlier uninstall left objects Helm could not verify; not reinstalling until a person removes them and changes the spec (see status.message)", ns, name)
			return
		}
		if curHash == hash && curPhase == hcPhaseFailed {
			if last := str(status["lastAttempt"], ""); last != "" {
				if t, err := time.Parse(time.RFC3339, last); err == nil && time.Since(t) < failedBackoff {
					return
				}
			}
		}
	}

	// key the decision on the field: the marker's message may already be gone.
	unrecordedRecovery := status != nil && asBool(status[unrecordedRecoveryField])

	// Helm errors can quote values; keep them out of a Secret-values chart's status.
	privateErrors := privateChart(hc, spec)

	// after the hot path on purpose: a converged release applies nothing to re-refuse.
	if hccValues != "" {
		pol.signer = pol.verificationSigner(rootCtx, hc, hcc)
	} else {
		pol.signer = pol.verificationSigner(rootCtx, hc)
	}
	imp, refuse, refuseMsg := installIdentity(hc, pol.privilegedNamespaces, pol.signer)
	if refuse {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", refuseMsg)
		return
	}
	// the overlay installs under this chart's identity, so its author must be the same requester.
	hcRequester := requesterInfo{}
	if imp != nil {
		hcRequester = requesterInfo{Username: imp.UserName, UID: imp.UID, Groups: imp.Groups}
	}
	if refuseHCC, msg := hccInstallAllowed(hc, hccValues, hcc, hcRequester, pol.privilegedNamespaces, pol.signer); refuseHCC {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", msg)
		return
	}

	// logged only past the hot path, so a settled release does not repeat it every tick.
	if requestedInsecure {
		if spec.insecureSkipTLSVerify {
			log.Printf("helmchart %s/%s: WARNING repo TLS verification DISABLED via spec.insecureSkipTLSVerify (permitted by --allow-insecure-helm-repos) for repo %q", ns, name, spec.repo)
		} else {
			log.Printf("helmchart %s/%s: spec.insecureSkipTLSVerify ignored — operator did not set --allow-insecure-helm-repos; verifying repo TLS", ns, name)
		}
	}

	ctx, cancel := context.WithTimeout(rootCtx, spec.timeout)
	defer cancel()

	acfg, err := newActionConfigFn(ctx, kubeconfig, spec.targetNamespace, imp)
	if err != nil {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", "action config: "+err.Error())
		return
	}

	chrt, err := loadChart(ctx, spec, ns, name)
	if err != nil {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", "load chart: "+err.Error())
		return
	}

	if err := writeHelmStatus(rootCtx, dyn, ns, name, hcPhasePending, spec, hash, 0, "", "reconciling", true); err != nil {
		log.Printf("helmchart %s/%s: record release before Helm work: %v", ns, name, err)
		return
	}

	got, getErr := action.NewGet(acfg).Run(spec.releaseName)
	if errors.Is(getErr, driver.ErrReleaseNotFound) {
		// Helm keeps no record of a purged release, so a missing one proves nothing.
		if unrecordedRecovery {
			relNS, relName := str(status["releaseNamespace"], spec.targetNamespace), str(status["releaseName"], spec.releaseName)
			log.Printf("helmchart %s/%s: recovery uninstall of release %s/%s never recorded its result; not reinstalling over the objects it may have left", ns, name, relNS, relName)
			recordUninstallIncomplete(rootCtx, dyn, hc, spec,
				uninstallIncompleteRecover+fmt.Sprintf("any objects of release %s/%s that remain (an earlier uninstall ended without recording what it left)", relNS, relName), "")
			return
		}
		inst := action.NewInstall(acfg)
		inst.ReleaseName = spec.releaseName
		inst.Namespace = spec.targetNamespace
		inst.CreateNamespace = spec.createNamespace
		// server-side apply needs requester patch rights; upgrades and rollbacks follow the recorded method
		inst.ServerSideApply = false
		inst.WaitStrategy = helmWaitStrategy
		inst.Timeout = spec.timeout
		// the transport carries ctx; RunWithContext can return while the worker runs; Run waits for it.
		installed, err := inst.Run(chrt, vals)
		if err != nil {
			setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", helmFailure(ns, name, privateErrors, "install", err))
			return
		}
		out, err := asRelease(installed)
		if err != nil {
			setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", helmFailure(ns, name, privateErrors, "install", err))
			return
		}
		recordDeployed(rootCtx, dyn, ns, name, spec, hash, out)
		log.Printf("helmchart %s/%s: installed %s rev %d (chart %s)", ns, name, spec.releaseName, revOf(out), chartVer(out, chrt))
		return
	}
	if getErr != nil {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", "get release: "+getErr.Error())
		return
	}
	rel, err := asRelease(got)
	if err != nil {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", "get release: "+err.Error())
		return
	}

	// a stuck pending-* or uninstalling release must recover before helm allows an upgrade.
	if rel.Info != nil && needsRecovery(rel.Info.Status) {
		stuck := rel.Info.Status
		// written right before Helm work: the marker is what a restart mid-uninstall leaves.
		if err := writeHelmStatus(rootCtx, dyn, ns, name, hcPhasePending, spec, hash, int64(revOf(rel)), "", recoveringUninstallMarker, true); err != nil {
			log.Printf("helmchart %s/%s: record the recovery uninstall before Helm work: %v", ns, name, err)
			return // keep the release; retry next tick
		}
		note, left, err := recoverPendingRelease(ctx, acfg, spec, rel)
		if left != "" {
			if err != nil {
				log.Printf("helmchart %s/%s: recovering %s: %v", ns, name, spec.releaseName, err)
			}
			log.Printf("helmchart %s/%s: recovering %s left objects Helm could not verify; not reinstalling: %q", ns, name, spec.releaseName, left)
			recordUninstallIncomplete(rootCtx, dyn, hc, spec, uninstallIncompleteRecover+left, note)
			return
		}
		if err != nil {
			setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", helmFailure(ns, name, privateErrors, fmt.Sprintf("recover %s release", stuck), err))
			return
		}
		if err := writeHelmStatus(rootCtx, dyn, ns, name, hcPhasePending, spec, hash, int64(revOf(rel)), "", recoveringPendingMessage, true); err != nil {
			if !apierrors.IsNotFound(err) {
				// keep only what the status retry needs; an inline chartContent can be large.
				identity := helmSpec{releaseName: spec.releaseName, targetNamespace: spec.targetNamespace, insecureSkipTLSVerify: spec.insecureSkipTLSVerify}
				cleanRecoveryOutcomes.Store(incompleteKey(hc), cleanRecoveryOutcome{spec: identity, hash: hash, revision: int64(revOf(rel))})
			}
			log.Printf("helmchart %s/%s: record the clean recovery outcome: %v", ns, name, err)
			return
		}
		log.Printf("helmchart %s/%s: release %s was %s, recovering (retry next tick)", ns, name, spec.releaseName, stuck)
		return
	}

	// ResetValues makes the merged values authoritative over prior revisions.
	up := action.NewUpgrade(acfg)
	up.Namespace = spec.targetNamespace
	up.ResetValues = true
	up.MaxHistory = maxHelmHistory
	up.WaitStrategy = helmWaitStrategy
	up.Timeout = spec.timeout
	// RunWithContext can leave Helm's worker blocked; Run waits for it.
	upgraded, err := up.Run(spec.releaseName, chrt, vals)
	if err != nil {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", helmFailure(ns, name, privateErrors, "upgrade", err))
		return
	}
	out, err := asRelease(upgraded)
	if err != nil {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", helmFailure(ns, name, privateErrors, "upgrade", err))
		return
	}
	recordDeployed(rootCtx, dyn, ns, name, spec, hash, out)
	log.Printf("helmchart %s/%s: upgraded %s to rev %d (chart %s)", ns, name, spec.releaseName, revOf(out), chartVer(out, chrt))
}

// a release left uninstalling by a failed recovery needs the same recovery as a pending one.
func needsRecovery(s common.Status) bool {
	return s.IsPending() || s == common.StatusUninstalling
}

// rolls back to the last served revision, else uninstalls; left is Helm's unverified list.
func recoverPendingRelease(ctx context.Context, acfg *action.Configuration, spec helmSpec, rel *release) (note, left string, err error) {
	target := 0
	if rel.Info.Status != common.StatusUninstalling {
		if target, err = lastServedBefore(acfg, spec.releaseName, revOf(rel)); err != nil {
			return "", "", err
		}
	}
	if target == 0 {
		un := action.NewUninstall(acfg)
		un.KeepHistory = false
		un.WaitStrategy = helmWaitStrategy
		un.Timeout = spec.timeout
		note, left, err = uninstallRelease(un, spec.releaseName)
		if note != "" {
			log.Printf("helmchart: recover(uninstall) %s left objects in place: %q", spec.releaseName, note)
		}
		if err != nil && !errors.Is(err, driver.ErrReleaseNotFound) {
			return note, left, fmt.Errorf("uninstall: %w", err)
		}
		return note, left, nil
	}
	rb := action.NewRollback(acfg)
	rb.Version = target
	rb.WaitStrategy = helmWaitStrategy
	rb.Timeout = spec.timeout
	if err := rb.Run(spec.releaseName); err != nil {
		return "", "", fmt.Errorf("roll back to revision %d: %w", target, err)
	}
	return "", "", nil
}

// a superseded revision counts only when its description proves an install or upgrade completed.
var completedDescriptions = map[string]bool{"Install complete": true, "Upgrade complete": true}

func lastServedBefore(acfg *action.Configuration, name string, rev int) (int, error) {
	hist, err := action.NewHistory(acfg).Run(name)
	if err != nil {
		return 0, fmt.Errorf("read release history: %w", err)
	}
	deployed, superseded := 0, 0
	for _, h := range hist {
		r, err := asRelease(h)
		if err != nil {
			return 0, fmt.Errorf("read release history: %w", err)
		}
		if r.Version >= rev || r.Info == nil {
			continue
		}
		switch r.Info.Status {
		case common.StatusDeployed:
			deployed = max(deployed, r.Version)
		case common.StatusSuperseded:
			if completedDescriptions[r.Info.Description] {
				superseded = max(superseded, r.Version)
			}
		}
	}
	if deployed != 0 {
		return deployed, nil
	}
	return superseded, nil
}

// note covers all objects Helm left; unverified only those whose ownership it could not verify.
func uninstallRelease(un *action.Uninstall, name string) (note, unverified string, err error) {
	res, err := un.Run(name)
	if res != nil {
		note = strings.TrimSpace(res.Info)
	}
	return note, unverifiedSection(note), err
}

// Helm separates the note's sections with a blank line.
func unverifiedSection(note string) string {
	i := strings.Index(note, helmUnverifiedNote)
	if i < 0 {
		return ""
	}
	section := note[strings.LastIndex(note[:i], "\n")+1:]
	section, _, _ = strings.Cut(section, "\n\n")
	return strings.TrimSpace(section)
}

const holdField = "uninstallIncomplete"

// survives every other status write, so a restart keeps the recovery evidence.
const unrecordedRecoveryField = "unrecordedRecovery"

const (
	holdPathDelete  = "delete"
	holdPathRecover = "recover"
	// an earlier operator version's shorter instruction; keep recognizing it.
	legacyUninstallIncompleteRecover = uninstallIncompletePrefix + "remove these objects by hand, then change the spec to reinstall: "
)

type incompleteHold struct {
	unverified string
	path       string
	specHash   string
}

// recognizes today's and the legacy hold instructions; the list follows the prefix.
func incompleteMessageParts(msg string) (path, unverified string, ok bool) {
	if list, found := strings.CutPrefix(msg, uninstallIncompleteDelete); found {
		return holdPathDelete, list, true
	}
	if list, found := strings.CutPrefix(msg, uninstallIncompleteRecover); found {
		return holdPathRecover, list, true
	}
	if list, found := strings.CutPrefix(msg, legacyUninstallIncompleteRecover); found {
		return holdPathRecover, list, true
	}
	return "", "", false
}

// promotes a message-only hold from an earlier operator version.
func migrateLegacyIncompleteHold(ctx context.Context, dyn dynamic.Interface, ns, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh, err := dyn.Resource(helmChartGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		st, _, _ := unstructured.NestedMap(fresh.Object, "status")
		if !uninstallIncomplete(st) {
			return nil
		}
		if _, ok := incompleteHoldFrom(st); ok {
			return nil
		}
		path, unverified, ok := incompleteMessageParts(str(st["message"], ""))
		if !ok {
			return fmt.Errorf("unrecognized earlier uninstall-incomplete message")
		}
		st[holdField] = map[string]interface{}{
			"unverified": unverified,
			"path":       path,
			"specHash":   str(st["specHash"], ""),
		}
		if err := unstructured.SetNestedMap(fresh.Object, st, "status"); err != nil {
			return err
		}
		_, err = dyn.Resource(helmChartGVR).Namespace(ns).UpdateStatus(ctx, fresh, metav1.UpdateOptions{})
		return err
	})
}

// the field survives Failed writes that replace the message, so the hold outlives its text.
func incompleteHoldFrom(status map[string]interface{}) (incompleteHold, bool) {
	m, ok := status[holdField].(map[string]interface{})
	if !ok {
		return incompleteHold{}, false
	}
	return incompleteHold{
		unverified: str(m["unverified"], ""),
		path:       str(m["path"], ""),
		specHash:   str(m["specHash"], ""),
	}, true
}

// today's instruction text for the hold's path, followed by the objects.
func incompleteHoldMessage(hold incompleteHold) string {
	if hold.path == holdPathDelete {
		return uninstallIncompleteDelete + hold.unverified
	}
	return uninstallIncompleteRecover + hold.unverified
}

// the field hold, or the message prefix for holds recorded before the field existed.
func uninstallIncomplete(status map[string]interface{}) bool {
	if _, ok := incompleteHoldFrom(status); ok {
		return true
	}
	return str(status["phase"], "") == hcPhaseFailed &&
		strings.HasPrefix(str(status["message"], ""), uninstallIncompletePrefix)
}

// the delete form asks for the finalizer, not a spec change; Helm's list is kept.
func deletionIncompleteMessage(msg string) string {
	if strings.HasPrefix(msg, uninstallIncompleteDelete) {
		return msg
	}
	if _, unverified, ok := incompleteMessageParts(msg); ok {
		return uninstallIncompleteDelete + unverified
	}
	return uninstallIncompleteDelete + strings.TrimPrefix(msg, uninstallIncompleteRecover)
}

// rides out a transient apiserver error without stalling the other charts long.
var incompleteWriteBackoff = wait.Backoff{Duration: 100 * time.Millisecond, Factor: 2, Jitter: 0.1, Steps: 5}

// each failed incomplete write, retried each reconcile instead of finalizer removal or reinstall.
var (
	unwrittenIncompleteMu sync.Mutex
	unwrittenIncomplete   = map[string]incompleteRecord{}
)

// deleted charts whose clean uninstall this process saw but could not record.
var cleanUninstalls sync.Map

// a restart drops the observation, leaving the marker's conservative hold in force.
var cleanRecoveryOutcomes sync.Map // incompleteKey -> cleanRecoveryOutcome

type cleanRecoveryOutcome struct {
	spec     helmSpec
	hash     string
	revision int64
}

type incompleteRecord struct {
	msg, note string
}

// the UID keeps a recreated HelmChart from inheriting an old entry.
func incompleteKey(hc *unstructured.Unstructured) string {
	return hc.GetNamespace() + "/" + hc.GetName() + "/" + string(hc.GetUID())
}

func unwrittenIncompleteFor(hc *unstructured.Unstructured) (incompleteRecord, bool) {
	unwrittenIncompleteMu.Lock()
	defer unwrittenIncompleteMu.Unlock()
	rec, ok := unwrittenIncomplete[incompleteKey(hc)]
	return rec, ok
}

// Helm has purged the release, so this status is the only record objects remain.
func recordUninstallIncomplete(ctx context.Context, dyn dynamic.Interface, hc *unstructured.Unstructured, spec helmSpec, msg, note string) {
	ns, name := hc.GetNamespace(), hc.GetName()
	if _, list, ok := incompleteMessageParts(msg); ok && list != privateHoldObjects && privateChart(hc, spec) {
		log.Printf("helmchart %s/%s: the objects the uninstall left, kept out of the status: %q", ns, name, list)
		msg = strings.TrimSuffix(msg, list) + privateHoldObjects
	}
	attempts := 0
	var writeErr error
	err := wait.ExponentialBackoffWithContext(ctx, incompleteWriteBackoff, func(ctx context.Context) (bool, error) {
		attempts++
		writeErr = writeHelmStatus(ctx, dyn, ns, name, hcPhaseFailed, spec, "", 0, "", msg, false)
		if writeErr == nil {
			return true, nil
		}
		log.Printf("helmchart %s/%s: write the %q status (attempt %d): %v; Helm's note on the objects the uninstall left: %q", ns, name, uninstallIncompletePrefix, attempts, writeErr, note)
		if apierrors.IsNotFound(writeErr) {
			return false, writeErr // the HelmChart is gone, finalizer and all
		}
		return false, nil
	})

	unwrittenIncompleteMu.Lock()
	defer unwrittenIncompleteMu.Unlock()
	if err == nil || apierrors.IsNotFound(err) {
		delete(unwrittenIncomplete, incompleteKey(hc))
		return
	}
	unwrittenIncomplete[incompleteKey(hc)] = incompleteRecord{msg: msg, note: note}
	if writeErr == nil {
		writeErr = err // ctx ended before the first attempt
	}
	log.Printf("helmchart %s/%s: could not write the %q status in %d attempt(s), last error: %v; retaining finalizer and not reinstalling until a later reconcile writes it, which the pre-uninstall status marker keeps a restart from defeating", ns, name, uninstallIncompletePrefix, attempts, writeErr)
}

// a hung repository must fail within spec.timeout, not hold the reconcile pass.
func loadChart(ctx context.Context, spec helmSpec, ns, name string) (*chart.Chart, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.chartContent != "" {
		raw, err := base64.StdEncoding.DecodeString(spec.chartContent)
		if err != nil {
			return nil, fmt.Errorf("decode chartContent: %w", err)
		}
		return loader.LoadArchive(bytes.NewReader(raw))
	}
	if spec.chart == "" {
		return nil, fmt.Errorf("spec.chart or spec.chartContent is required")
	}

	// HELM_CACHE_HOME moves the caches but not repositories.yaml, so both are set.
	if err := migrateLegacyHelmCache(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(version.HelmCacheDir, "cache"), 0700); err != nil {
		return nil, err
	}
	settings := cli.New()
	settings.RepositoryCache = filepath.Join(version.HelmCacheDir, "cache")
	settings.RepositoryConfig = filepath.Join(version.HelmCacheDir, "repositories.yaml")

	fetchCtx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()
	path, err := locateChart(fetchCtx, spec, settings)
	if fetchCtx.Err() != nil {
		return nil, fmt.Errorf("fetch of chart %q stopped (spec.timeout %s): %w", spec.chart, spec.timeout, fetchCtx.Err())
	}
	if err != nil {
		return nil, err
	}
	return loader.Load(path)
}

// a byte-identical refused repositories.yaml is this migration's earlier copy, so the scan resumes
func migrateLegacyHelmCache() error {
	legacyConfig := filepath.Join(legacyHelmCacheDir, "repositories.yaml")
	legacyData, err := os.ReadFile(legacyConfig)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // nothing to migrate: a node joined on this layout
		}
		return err
	}
	mode := fs.FileMode(0o600)
	if fi, statErr := os.Stat(legacyConfig); statErr == nil {
		mode = fi.Mode().Perm()
	}
	dstConfig := filepath.Join(version.HelmCacheDir, "repositories.yaml")
	if err := migrateLegacyHelmFile(dstConfig, legacyData, mode); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("migrate the helm repository configuration from %s: %w", legacyHelmCacheDir, err)
		}
		established, err := os.ReadFile(dstConfig)
		if err != nil {
			return err
		}
		if !bytes.Equal(established, legacyData) {
			return nil // an established home: the legacy copy is stale
		}
	} else {
		log.Printf("helmchart: migrated the helm repository configuration from %s (%d bytes) to %s",
			legacyConfig, len(legacyData), version.HelmCacheDir)
	}

	legacyCache := filepath.Join(legacyHelmCacheDir, "cache")
	entries, err := os.ReadDir(legacyCache)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	migrated := 0
	for _, entry := range entries {
		// an index is named <repo>-index.yaml; that is all the lookup reads.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "-index.yaml") {
			continue
		}
		dst := filepath.Join(version.HelmCacheDir, "cache", entry.Name())
		// an index runs to megabytes; the legacy copy stays for uninstall to remove.
		if _, err := os.Lstat(dst); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		src := filepath.Join(legacyCache, entry.Name())
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o600)
		if fi, statErr := os.Stat(src); statErr == nil {
			mode = fi.Mode().Perm()
		}
		if err := migrateLegacyHelmFile(dst, data, mode); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return fmt.Errorf("migrate cached repository index %s from %s: %w", entry.Name(), legacyHelmCacheDir, err)
		}
		migrated++
	}
	if migrated > 0 {
		log.Printf("helmchart: migrated %d cached repository index(es) from %s to %s", migrated, legacyCache, version.HelmCacheDir)
	}
	return nil
}

// the pre-check spares staging whole files each load; the exclusive create is the race backstop.
func migrateLegacyHelmFile(dst string, data []byte, mode fs.FileMode) error {
	if _, err := os.Lstat(dst); err == nil {
		return fs.ErrExist
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	_, err := atomicfile.CreateExclusive(dst, data, mode)
	return err
}

// helm v4 discards slog output when no logger is set; the std logger stamps its own time.
var helmLogHandler slog.Handler = slog.NewTextHandler(stdLogWriter{}, &slog.HandlerOptions{
	Level: slog.LevelDebug,
	ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	},
})

// so Helm's lines carry the operator log's prefix and flags.
type stdLogWriter struct{}

func (stdLogWriter) Write(p []byte) (int, error) {
	if err := log.Output(2, string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// a var so flow tests can drive reconcileHelmChart without a cluster.
var newActionConfigFn = newActionConfig

// when imp is set, every call runs as the requester, so RBAC gates what a chart applies.
func newActionConfig(ctx context.Context, kubeconfig, namespace string, imp *rest.ImpersonationConfig) (*action.Configuration, error) {
	acfg := action.NewConfiguration(action.ConfigurationSetLogger(helmLogHandler))
	// Helm v4 removed kube.GetConfig; this is its Helm v3 body.
	kubeContext := ""
	getter := genericclioptions.NewConfigFlags(true)
	getter.Namespace = &namespace
	getter.Context = &kubeContext
	getter.KubeConfig = &kubeconfig
	// helm's install/upgrade workers can outlive RunWithContext; stop their requests too.
	getter.WithWrapConfigFn(func(cfg *rest.Config) *rest.Config {
		if imp != nil {
			cfg.Impersonate = *imp
		}
		cfg.Wrap(func(next http.RoundTripper) http.RoundTripper {
			return &contextTransport{ctx: ctx, next: next}
		})
		return cfg
	})
	if err := acfg.Init(getter, namespace, "secret"); err != nil {
		return nil, err
	}
	kc, ok := acfg.KubeClient.(*kube.Client)
	if !ok {
		return nil, fmt.Errorf("helm kube client is %T, want *kube.Client", acfg.KubeClient)
	}
	acfg.KubeClient = v3WaitClient{Client: kc}
	rc, err := registry.NewClient()
	if err != nil {
		return nil, err
	}
	acfg.RegistryClient = rc
	return acfg, nil
}

var crdGK = schema.GroupKind{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}

// Helm v4's waiter lists/watches namespace-wide (impersonation fails) and never waits for new CRDs.
type v3WaitClient struct{ *kube.Client }

var _ kube.InterfaceWaitOptions = v3WaitClient{}

func (c v3WaitClient) GetWaiter(ws kube.WaitStrategy) (kube.Waiter, error) {
	return c.GetWaiterWithOptions(ws)
}

// helm's install asks for StatusWatcherStrategy for crds/; answer it the same way.
func (c v3WaitClient) GetWaiterWithOptions(ws kube.WaitStrategy, opts ...kube.WaitOption) (kube.Waiter, error) {
	if ws != helmWaitStrategy && ws != kube.StatusWatcherStrategy {
		return c.Client.GetWaiterWithOptions(ws, opts...)
	}
	legacy, err := c.Client.GetWaiterWithOptions(kube.LegacyStrategy)
	if err != nil {
		return nil, err
	}
	return v3NoWait{legacy: legacy}, nil
}

// v3's waiter, except Wait also waits for templated CRDs, which v3 did not.
type v3NoWait struct{ legacy kube.Waiter }

func (w v3NoWait) WatchUntilReady(resources kube.ResourceList, timeout time.Duration) error {
	return w.legacy.WatchUntilReady(resources, timeout)
}

func (w v3NoWait) Wait(resources kube.ResourceList, timeout time.Duration) error {
	var crds kube.ResourceList
	for _, info := range resources {
		if info.Mapping != nil && info.Mapping.GroupVersionKind.GroupKind() == crdGK {
			crds = append(crds, info)
		}
	}
	if len(crds) == 0 {
		return nil
	}
	return w.legacy.Wait(crds, timeout)
}

func (w v3NoWait) WaitWithJobs(resources kube.ResourceList, timeout time.Duration) error {
	return w.Wait(resources, timeout)
}

// helm also calls this after an uninstall, on release objects that are not hooks.
func (w v3NoWait) WaitForDelete(resources kube.ResourceList, timeout time.Duration) error {
	if !allHooks(resources) {
		return nil
	}
	return w.legacy.WaitForDelete(resources, timeout)
}

func allHooks(resources kube.ResourceList) bool {
	if len(resources) == 0 {
		return false
	}
	for _, info := range resources {
		obj, err := meta.Accessor(info.Object)
		if err != nil {
			return false
		}
		if _, ok := obj.GetAnnotations()[helmrelease.HookAnnotation]; !ok {
			return false
		}
	}
	return true
}

// layering low to high: valuesContent < config overlay < secrets < set < setString.
func mergedValues(spec helmSpec, hccValues string, secretValues ...map[string]interface{}) (map[string]interface{}, error) {
	base := map[string]interface{}{}
	if spec.valuesContent != "" {
		m, err := parseYAMLValues(spec.valuesContent)
		if err != nil {
			return nil, fmt.Errorf("valuesContent: %w", err)
		}
		base = m
	}
	if hccValues != "" {
		over, err := parseYAMLValues(hccValues)
		if err != nil {
			return nil, fmt.Errorf("HelmChartConfig valuesContent: %w", err)
		}
		base = mergeMaps(base, over)
	}
	for _, m := range secretValues {
		base = mergeMaps(base, m)
	}
	// sorted keys so a parent/child key conflict fails the same way every run.
	for _, k := range slices.Sorted(maps.Keys(spec.set)) {
		v := spec.set[k]
		if err := strvals.ParseInto(k+"="+v, base); err != nil {
			return nil, fmt.Errorf("set %q: %w", k, err)
		}
	}
	// applied after set, so setString wins a same-key collision, like helm's own ordering.
	for _, k := range slices.Sorted(maps.Keys(spec.setString)) {
		v := spec.setString[k]
		if err := strvals.ParseIntoString(k+"="+v, base); err != nil {
			return nil, fmt.Errorf("setString %q: %w", k, err)
		}
	}
	return base, nil
}

func parseYAMLValues(s string) (map[string]interface{}, error) {
	m := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// like helm: nested maps recurse; everything else, slices included, replaces.
func mergeMaps(dst, src map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if vm, ok := v.(map[string]interface{}); ok {
			if dm, ok := out[k].(map[string]interface{}); ok {
				out[k] = mergeMaps(dm, vm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// defaults come from helmChartSpecTable, the CRD-pinned contract.
func parseHelmSpec(hc *unstructured.Unstructured) helmSpec {
	ns, name := hc.GetNamespace(), hc.GetName()
	s := helmSpec{
		chart:                 str(nested(hc.Object, "spec", "chart"), ""),
		repo:                  str(nested(hc.Object, "spec", "repo"), ""),
		version:               str(nested(hc.Object, "spec", "version"), ""),
		chartContent:          str(nested(hc.Object, "spec", "chartContent"), ""),
		releaseName:           str(nested(hc.Object, "spec", "releaseName"), name),
		targetNamespace:       str(nested(hc.Object, "spec", "targetNamespace"), ns),
		valuesContent:         str(nested(hc.Object, "spec", "valuesContent"), ""),
		repoCA:                str(nested(hc.Object, "spec", "repoCA"), ""),
		insecureSkipTLSVerify: specBool(hc, "insecureSkipTLSVerify"),
		createNamespace:       specBool(hc, "createNamespace"),
		set:                   asStringMap(nested(hc.Object, "spec", "set")),
		setString:             asStringMap(nested(hc.Object, "spec", "setString")),
		valuesSecrets:         parseValuesSecrets(nested(hc.Object, "spec", "valuesSecrets")),
	}
	// an absent or invalid value falls back to the CRD's declared default.
	s.timeout, _ = time.ParseDuration(str(helmChartSpecDefault("timeout"), ""))
	if t := str(nested(hc.Object, "spec", "timeout"), ""); t != "" {
		if d, err := time.ParseDuration(t); err == nil && d > 0 {
			s.timeout = d
		} else {
			log.Printf("helmchart %s/%s: invalid spec.timeout %q, defaulting to %s", ns, name, t, s.timeout)
		}
	}
	// an inline chart does no fetch, so the flag is meaningless; zero it.
	if s.chartContent != "" {
		s.insecureSkipTLSVerify = false
	}
	return s
}

func helmChartSpecDefault(field string) interface{} {
	for _, row := range helmChartSpecTable {
		if row.name == field {
			return row.def
		}
	}
	return nil
}

func specBool(hc *unstructured.Unstructured, field string) bool {
	if b, ok := nested(hc.Object, "spec", field).(bool); ok {
		return b
	}
	b, _ := helmChartSpecDefault(field).(bool)
	return b
}

// over the chart source and merged values, so any change forces a reconcile.
func specHash(spec helmSpec, vals map[string]interface{}) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", spec.repo, spec.chart, spec.version)
	if spec.chartContent != "" {
		cc := sha256.Sum256([]byte(spec.chartContent))
		h.Write(cc[:])
	}
	// json.Marshal of a map[string]interface{} is key-sorted => canonical.
	if b, err := json.Marshal(vals); err == nil {
		h.Write(b)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func hasFinalizer(o *unstructured.Unstructured, f string) bool {
	return slices.Contains(o.GetFinalizers(), f)
}

// pure cores so tests need no cluster; both return a fresh slice, never alias cur.
func addFinalizerList(cur []string, f string) []string {
	if slices.Contains(cur, f) {
		return cur
	}
	return append(slices.Clone(cur), f)
}

func removeFinalizerList(cur []string, f string) []string {
	return slices.DeleteFunc(slices.Clone(cur), func(x string) bool { return x == f })
}

func addFinalizer(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name, f string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		next := addFinalizerList(fresh.GetFinalizers(), f)
		if len(next) == len(fresh.GetFinalizers()) {
			return nil // already present
		}
		fresh.SetFinalizers(next)
		_, err = dyn.Resource(gvr).Namespace(ns).Update(ctx, fresh, metav1.UpdateOptions{})
		return err
	})
}

func removeFinalizer(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name, f string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		cur := fresh.GetFinalizers()
		next := removeFinalizerList(cur, f)
		if len(next) == len(cur) {
			return nil // already gone
		}
		fresh.SetFinalizers(next)
		_, err = dyn.Resource(gvr).Namespace(ns).Update(ctx, fresh, metav1.UpdateOptions{})
		return err
	})
}

// no Helm operation took place, so ownership and deployment metadata stay.
func refreshHelmTLSStatus(ctx context.Context, dyn dynamic.Interface, ns, name string, insecure bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh, err := dyn.Resource(helmChartGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := unstructured.SetNestedField(fresh.Object, insecure, "status", "insecureTLSVerify"); err != nil {
			return err
		}
		_, err = dyn.Resource(helmChartGVR).Namespace(ns).UpdateStatus(ctx, fresh, metav1.UpdateOptions{})
		return err
	})
}

func recordDeployed(ctx context.Context, dyn dynamic.Interface, ns, name string, spec helmSpec, hash string, rel *release) {
	setHelmStatusHash(ctx, dyn, ns, name, hcPhaseDeployed, spec, hash, int64(revOf(rel)),
		chartVer(rel, nil), "release deployed")
}

// no hash: a failure must not mark the current spec converged.
func setHelmStatus(ctx context.Context, dyn dynamic.Interface, ns, name, phase string, spec helmSpec, revision int64, chartVersion, msg string) {
	writeHelmStatus(ctx, dyn, ns, name, phase, spec, "", revision, chartVersion, msg, false)
}

func setHelmStatusHash(ctx context.Context, dyn dynamic.Interface, ns, name, phase string, spec helmSpec, hash string, revision int64, chartVersion, msg string) {
	writeHelmStatus(ctx, dyn, ns, name, phase, spec, hash, revision, chartVersion, msg, true)
}

func writeHelmStatus(ctx context.Context, dyn dynamic.Interface, ns, name, phase string, spec helmSpec, hash string, revision int64, chartVersion, msg string, setHash bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh, err := dyn.Resource(helmChartGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		st, _ := fresh.Object["status"].(map[string]interface{})
		if st == nil {
			st = map[string]interface{}{}
		}
		// a stale spec must not move recorded ownership before Helm work.
		recordIdentity := phase == hcPhasePending || phase == hcPhaseDeployed
		if recordIdentity {
			if old := str(st["releaseName"], ""); old != "" && old != spec.releaseName {
				return fmt.Errorf("releaseName differs from recorded ownership")
			}
			if old := str(st["releaseNamespace"], ""); old != "" && old != spec.targetNamespace {
				return fmt.Errorf("targetNamespace differs from recorded ownership")
			}
		}
		_, tlsPresent := st["insecureTLSVerify"].(bool)
		identityMatches := str(st["releaseName"], "") == spec.releaseName && str(st["releaseNamespace"], "") == spec.targetNamespace
		gen := fresh.GetGeneration()
		// skip a redundant write so a settled chart does not hot-loop UpdateStatus.
		if str(st["phase"], "") == phase && str(st["message"], "") == msg &&
			asInt(st["observedGeneration"]) == gen &&
			tlsPresent && asBool(st["insecureTLSVerify"]) == spec.insecureSkipTLSVerify &&
			(!recordIdentity || identityMatches) &&
			(!setHash || str(st["specHash"], "") == hash) &&
			(revision == 0 || asInt(st["revision"]) == revision) {
			return nil
		}
		st["phase"] = phase
		st["message"] = msg
		// the hold outlives its message; only an install under the current spec clears it.
		if path, unverified, ok := incompleteMessageParts(msg); ok {
			st[holdField] = map[string]interface{}{
				"unverified": unverified,
				"path":       path,
				"specHash":   str(st["specHash"], ""),
			}
			// the hold is the stronger record of the unrecorded uninstall.
			delete(st, unrecordedRecoveryField)
		}
		// the marker survives Failed writes that replace its message, so a restart keeps the evidence.
		if msg == recoveringUninstallMarker {
			st[unrecordedRecoveryField] = true
		}
		if msg == recoveringPendingMessage || msg == uninstalledMessage || phase == hcPhaseDeployed {
			delete(st, unrecordedRecoveryField)
		}
		st["observedGeneration"] = gen
		// persist before Helm starts: even a failed install may leave a release.
		if recordIdentity {
			st["releaseName"] = spec.releaseName
			st["releaseNamespace"] = spec.targetNamespace
			delete(st, holdField)
			// a recovery can roll back to history holding Secret values, so the mark never clears.
			if len(spec.valuesSecrets) > 0 {
				st[valuesFromSecretsField] = true
			}
		}
		st["lastAttempt"] = time.Now().UTC().Format(time.RFC3339)
		st["insecureTLSVerify"] = spec.insecureSkipTLSVerify
		if setHash && hash != "" {
			st["specHash"] = hash
		}
		if revision > 0 {
			st["revision"] = revision
		}
		if chartVersion != "" {
			st["chartVersion"] = chartVersion
		}
		fresh.Object["status"] = st
		_, err = dyn.Resource(helmChartGVR).Namespace(ns).UpdateStatus(ctx, fresh, metav1.UpdateOptions{})
		return err
	})
}

// a local alias so signatures stay short throughout this file.
type release = helmrelease.Release

// asserts a release Helm v4 types as release.Releaser (an alias for any) to the v1 release.
func asRelease(r any) (*release, error) {
	rel, ok := r.(*release)
	if !ok {
		return nil, fmt.Errorf("unexpected Helm release type %T", r)
	}
	return rel, nil
}

func revOf(r *release) int {
	if r == nil {
		return 0
	}
	return r.Version
}

// falls back to a directly loaded chart's metadata when the release lacks it.
func chartVer(r *release, chrt *chart.Chart) string {
	if r != nil && r.Chart != nil && r.Chart.Metadata != nil && r.Chart.Metadata.Version != "" {
		return r.Chart.Metadata.Version
	}
	if chrt != nil && chrt.Metadata != nil {
		return chrt.Metadata.Version
	}
	return ""
}

func isOCI(repo string) bool { return strings.HasPrefix(repo, "oci://") }

func asBool(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

func asStringMap(v interface{}) map[string]string {
	out := map[string]string{}
	if m, ok := v.(map[string]interface{}); ok {
		for k, val := range m {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

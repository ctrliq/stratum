package operator

// HelmChart controller (stratum.ciq.com/v1) — an in-cluster Helm operator.
// Reconciles two CRDs:
//   - HelmChart: install/upgrade/uninstall a chart (from an HTTP(S) or oci://
//     repo, or an inline base64 .tgz) as a Helm release, tracking phase +
//     revision in .status. Idempotent + resumable: the desired state (specHash)
//     is compared against the live release each loop, so a controller restart
//     resumes and a no-op reconcile is genuinely cheap (a single Secret GET, no
//     helm/discovery machinery on the hot path).
//   - HelmChartConfig: a same-name+namespace overlay whose valuesContent is
//     deep-merged OVER the HelmChart's own values (letting a cluster
//     operator tweak a bundled chart without editing it).
//
// Values precedence (low -> high):
//   chart defaults < HelmChart.valuesContent < HelmChartConfig.valuesContent
//     < spec.set < spec.setString
//
// spec.setString is last: it is the escape hatch for a value strvals would
// otherwise re-type, so it has to be able to override the spec.set entry that
// produced the wrong type.
//
// Uses the embedded Helm SDK (helm.sh/helm/v4) with the "secret" storage driver,
// so releases live as sh.helm.release.v1.* Secrets exactly like `helm` CLI.
// Deletion is finalizer-gated (stratum.ciq.com/helmchart) so the release is
// helm-uninstalled before the CR disappears. Helm v4's uninstall skips an object
// whose ownership it cannot verify (its GET fails: a 403 under requester
// impersonation, or a transient apiserver error), purges the release record
// anyway and reports success. The operator then keeps the finalizer, sets the
// HelmChart Failed with Helm's list of those objects, and does no further Helm
// work for it, so a person removes the objects and then the finalizer. Objects
// a helm.sh/resource-policy: keep annotation keeps, and objects Helm skips
// because another release owns them, are only logged.

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

// helmCacheDir retains the default cache location for the layout agreement
// test. loadChart reads version.HelmCacheDir directly so a redirected data
// root moves the repository cache and configuration. The legacy
// /var/lib/stratum-helm path remains an uninstall target, and a node
// upgraded from a release that kept its repository state there carries it
// into the data dir on first use (migrateLegacyHelmCache).
var helmCacheDir = version.HelmCacheDir

// legacyHelmCacheDir is where releases before the data-dir layout kept the
// operator's helm cache: the administrator-owned repositories.yaml and the
// cached repo indexes a named-repository fetch consults. A var so a test can
// point it at a scratch directory.
var legacyHelmCacheDir = version.LegacyHelmCacheDir

const (
	helmFinalizer = "stratum.ciq.com/helmchart"

	hcPhasePending      = "Pending"
	hcPhaseDeployed     = "Deployed"
	hcPhaseFailed       = "Failed"
	hcPhaseUninstalling = "Uninstalling"

	// failedBackoff throttles re-attempts of a chart that just failed, so a
	// broken repo/values combo can't hammer the apiserver/registry every tick.
	failedBackoff = 60 * time.Second
	// maxHelmHistory caps saved revisions per release (matches helm's default).
	maxHelmHistory = 5

	// helmWaitStrategy is set on every Helm action the operator runs. Helm v4
	// replaced each action's Wait bool with a strategy whose zero value is not
	// "no wait": install, upgrade, uninstall and rollback all build a waiter,
	// and an empty strategy fails at runtime with "wait strategy not set".
	// Helm's own waiter for this strategy does not wait as Helm v3's
	// Wait = false did, so the Kubernetes client newActionConfig builds
	// answers it with v3NoWait instead (v3WaitClient).
	helmWaitStrategy = kube.HookOnlyStrategy

	// helmUnverifiedNote is the text Helm v4 puts in an uninstall response's
	// Info for objects it did not delete because it could not verify that the
	// release owns them (helm v4.3.0 pkg/action/uninstall.go:379-395). The
	// response has no structured field for them, so the tests in
	// helmchart_test.go pin this text against the Helm version in go.mod.
	helmUnverifiedNote = "ownership could not be verified"

	// uninstallIncompletePrefix starts the Failed status message written when an
	// uninstall left objects whose ownership Helm could not verify.
	// Helm has purged the release record by then, so a later uninstall would
	// find "release not found" and look clean, and a reinstall would go ahead
	// over the leftovers. While the status carries this prefix the operator
	// does no Helm work for the HelmChart: a deleting one keeps its finalizer,
	// and a recovered pending install waits for a spec change.
	uninstallIncompletePrefix = "uninstall incomplete: "
	// uninstallingMessage marks an uninstall begun and not yet resolved.
	uninstallingMessage = "uninstalling release"
	// uninstalledMessage replaces the marker once a deleted HelmChart's
	// uninstall resolved clean and only the finalizer removal is left. Like
	// recoveringPendingMessage it records the uninstall's outcome, so writing
	// it clears unrecordedRecoveryField.
	uninstalledMessage = "release uninstalled; removing finalizer"
	// uninstallIncompleteDelete and uninstallIncompleteRecover precede Helm's
	// list of the objects in that message: the first on a deleting HelmChart,
	// the second after a pending install's recovery uninstall.
	uninstallIncompleteDelete  = uninstallIncompletePrefix + "remove these objects by hand, then the " + helmFinalizer + " finalizer: "
	uninstallIncompleteRecover = uninstallIncompletePrefix + "remove these objects by hand, then change the chart source (spec.repo, spec.chart, spec.version or spec.chartContent) or the merged values (spec.valuesContent, spec.set, spec.setString, or the same-name HelmChartConfig's valuesContent) to reinstall; other spec fields, and values edits that leave the merged values the same, do not reinstall, except on a HelmChart with spec.valuesSecrets, where any edit to those values fields or to a Secret it names reinstalls: "

	// recoveringUninstallMarker marks a pending-install recovery's uninstall
	// begun and not yet resolved, as uninstallingMessage does for a deleted
	// HelmChart's uninstall. The recovery writes it right before Helm work, so
	// a restart between Helm's purge and the "uninstall incomplete:" status
	// write leaves it standing: the next reconcile, finding the marker with
	// the release gone, records the hold instead of reinstalling over the
	// objects the uninstall may have left. It must not start with
	// uninstallIncompletePrefix, so an outage that fails only the incomplete
	// write lets it through.
	recoveringUninstallMarker = "uninstalling release to recover"
	// recoveringPendingMessage records a recovery that resolved without
	// anything left unverified: the release was rolled back, or its recovery
	// uninstall completed clean. Writing it clears unrecordedRecoveryField,
	// the standing evidence of an unresolved recovery uninstall.
	recoveringPendingMessage = "recovering pending release"
)

var (
	helmChartGVR       = schema.GroupVersionResource{Group: k8sutil.HelmChartGroup, Version: k8sutil.HelmChartVersion, Resource: "helmcharts"}
	helmChartConfigGVR = schema.GroupVersionResource{Group: k8sutil.HelmChartGroup, Version: k8sutil.HelmChartVersion, Resource: "helmchartconfigs"}
	secretGVR          = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
)

// helmSpec is the parsed, defaulted view of a HelmChart .spec.
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

// helmChartSpecField is one row of helmChartSpecTable and declares one field
// of the HelmChart .spec contract: name is the helmSpec field parseHelmSpec
// feeds and the CRD property name (they coincide), kind is the schema type
// the CRD declares the property with, and def is the default the CRD
// declares for it (nil = no default declared — an absent field then falls
// back per field in parseHelmSpec).
type helmChartSpecField struct {
	name string
	kind string
	def  interface{}
}

// helmChartSpecTable is the single declaration of the HelmChart .spec
// contract. parseHelmSpec reads each field by its row's name and takes every
// declared default from its row, while TestHelmChartCRDMatchesTheSpecTable
// pins the CRD's spec properties, types and defaults to the same rows in
// both directions — so `kubectl explain` and the controller cannot drift
// apart. A helmSpec field added without a row there fails that test until
// the row and the matching CRD property exist.
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

// helmPolicy carries the operator-level guardrails applied to every HelmChart:
// which namespaces may install cross-namespace, and whether insecure repo TLS is
// permitted. Zero value = most restrictive (no privileged namespaces,
// insecure refused).
type helmPolicy struct {
	privilegedNamespaces []string
	allowInsecureRepos   bool
	// signer verifies the webhook-authored requester stamp before the operator
	// impersonates it. It is the snapshot this pass took from the signer holder
	// (operator.go builds one helmPolicy per reconcile pass; the Secret owner in
	// requestersig.go keeps fresh snapshots published without a restart). Nil
	// disables verification, which fails closed: every non-privileged install is
	// then refused by the backstop.
	signer *requesterSigner
	// refreshSigner reloads verification material before refusing a stamp that
	// may have been admitted by a webhook whose key refresh ran ahead of ours.
	refreshSigner func(context.Context) *requesterSigner
}

// verificationSigner retries a potentially lagging verifier once before an
// authentication refusal. Missing or malformed identities need no key lookup;
// valid stamps continue using the immutable snapshot already captured.
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

// uninstallTarget uses controller-written ownership, independent of current
// install policy. Tightening that policy must not orphan an existing release.
func uninstallTarget(status map[string]interface{}) (relNS, relName string, ok bool) {
	relNS = str(status["releaseNamespace"], "")
	relName = str(status["releaseName"], "")
	return relNS, relName, relNS != "" && relName != ""
}

// targetNamespaceAllowed permits installs into the CR's own namespace, or
// into any namespace when the CR's namespace is operator-configured privileged.
func targetNamespaceAllowed(crNS, target string, privileged []string) bool {
	if target == "" || target == crNS {
		return true
	}
	return slices.Contains(privileged, crNS)
}

func reconcileHelmCharts(rootCtx context.Context, dyn dynamic.Interface, kubeconfig string, pol helmPolicy) {
	// Bound the listing (not the per-chart helm work — that gets its own
	// spec.timeout budget off the ROOT ctx, since an install/upgrade still
	// waits for Job and Pod hooks and new CRDs (helmWaitStrategy) and can
	// outlast a 30s iteration window).
	listCtx, cancel := context.WithTimeout(rootCtx, perIterationTimeout)
	defer cancel()

	list, err := dyn.Resource(helmChartGVR).Namespace("").List(listCtx, metav1.ListOptions{})
	if err != nil {
		return // CRD may not be installed yet
	}

	// Index HelmChartConfig overlays by ns/name (same-name match). The object is
	// kept alongside its values so the requester-scoping check can verify
	// the overlay's own stamp, not just merge its values.
	cl, err := dyn.Resource(helmChartConfigGVR).Namespace("").List(listCtx, metav1.ListOptions{})
	if err != nil {
		// Reconciling without the overlays would compute every merged-values
		// hash without them: a converged chart would look changed and upgrade
		// with the overlay's values dropped, and a held HelmChart's reinstall
		// gate would see a specHash different from the one its hold recorded,
		// ending the hold to the very tick that starts the next install. Skip
		// the iteration; the next one lists again.
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

	// Normalize the EFFECTIVE insecure-TLS setting up front so every status write —
	// including the early-return paths below — reflects reality rather than the raw
	// request. requestedInsecure is kept only to decide whether to
	// log in the fetch path. parseHelmSpec already zeroes it for inline charts (no
	// fetch happens), so this is purely the operator-policy gate.
	requestedInsecure := spec.insecureSkipTLSVerify
	spec.insecureSkipTLSVerify = requestedInsecure && pol.allowInsecureRepos

	// Older operators recorded an incomplete uninstall only in the Failed
	// message. Persist that hold before any path below can replace the message
	// (including a values parse error), or an upgrade would reopen the same
	// reinstall and deletion windows that the structured field closes.
	priorStatus, _, _ := unstructured.NestedMap(hc.Object, "status")
	if uninstallIncomplete(priorStatus) {
		if _, ok := incompleteHoldFrom(priorStatus); !ok {
			if err := migrateLegacyIncompleteHold(rootCtx, dyn, ns, name); err != nil {
				log.Printf("helmchart %s/%s: persist earlier uninstall-incomplete hold: %v", ns, name, err)
				return
			}
		}
	}
	// A clean recovery can purge its release but fail to record that outcome.
	// This process still knows the recovery left nothing unverified, so retry
	// the outcome before a missing release can become a generic hold. After a
	// restart the record is gone, and the durable marker remains fail-safe.
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
		return // next tick uses the fresh status, including deletion if requested
	}

	// ---- Deletion path (finalizer-gated) --------------------------------
	if hc.GetDeletionTimestamp() != nil {
		if !hasFinalizer(hc, helmFinalizer) {
			return // already cleaned up / never adopted
		}
		// An uninstall left objects Helm could not verify, but writing that to
		// the status failed, so the status cannot hold the finalizer yet.
		if rec, ok := unwrittenIncompleteFor(hc); ok {
			log.Printf("helmchart %s/%s: an earlier uninstall left objects Helm could not verify; retaining finalizer and retrying the status write: %q", ns, name, rec.note)
			recordUninstallIncomplete(rootCtx, dyn, hc, spec, deletionIncompleteMessage(rec.msg), rec.note)
			return
		}
		// Uninstall what was ADOPTED, never what the spec currently says. The
		// spec stays tenant-writable right up to the delete, so trusting it
		// here lets a tenant adopt a legal release and then either repoint
		// releaseName/targetNamespace to drive a cluster-admin `helm uninstall`
		// against a release it does not own, or repoint it somewhere refused
		// and walk away — leaving its own release running with nothing tracking
		// it. .status is written only by this controller, through the status
		// subresource, so it is the trustworthy record of what exists.
		curStatus, _, _ := unstructured.NestedMap(hc.Object, "status")
		relNS, relName, doUninstall := uninstallTarget(curStatus)
		if !doUninstall {
			// Adoption precedes Helm work; no recorded identity means no attempt.
			log.Printf("helmchart %s/%s: no recorded release; removing finalizer only", ns, name)
			if err := removeFinalizer(rootCtx, dyn, helmChartGVR, ns, name, helmFinalizer); err != nil {
				log.Printf("helmchart %s/%s: remove finalizer: %v", ns, name, err)
			}
			return
		}
		if uninstallIncomplete(curStatus) {
			if hold, ok := incompleteHoldFrom(curStatus); ok {
				// The hold outlives its message (status.uninstallIncomplete): a
				// values typo or a refused overlay replaces the message without
				// lifting the hold. Regenerate the delete-form message from the
				// field: the recover form asks for a spec change, which does
				// nothing once the HelmChart is being deleted, and any message
				// the hold survived says nothing about the finalizer.
				hold.path = holdPathDelete
				if want := incompleteHoldMessage(hold); want != str(curStatus["message"], "") {
					setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", want)
				}
			} else if msg := str(curStatus["message"], ""); !strings.HasPrefix(msg, uninstallIncompleteDelete) {
				// A hold recorded before the field existed: derive the delete
				// form from the message.
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
		// The status still holding a pre-uninstall marker, or the recovery
		// evidence that marker records, means an earlier uninstall never
		// recorded its result: the operator restarted between Helm's purge and
		// the status write, or Helm purged it and returned an error.
		// uninstallingMessage marks a deleted HelmChart's own uninstall,
		// recoveringUninstallMarker a recovery uninstall whose hold never
		// reached the status before the restart. Helm keeps no record of what
		// either left, so a missing release then proves nothing. Each marker is
		// written right before Helm work, after everything that could stop it,
		// so it survives that and only that.
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
			// Checked before err: Helm purges the release even when it also
			// returns an error, so a retry could never reach these objects.
			// Helm usually reports success here, so err is logged only when set.
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
		// Replace the marker, so a retry after a failed finalizer removal
		// does not take this clean uninstall for an unrecorded one. Should the
		// write fail, this process remembers instead.
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

	// ---- Unwritten incomplete uninstall ---------------------------------
	// A recovery uninstall left objects Helm could not verify, but writing
	// that to the status failed, so the status cannot stop a reinstall over
	// them yet.
	if rec, ok := unwrittenIncompleteFor(hc); ok {
		log.Printf("helmchart %s/%s: an earlier uninstall left objects Helm could not verify; not reinstalling, retrying the status write: %q", ns, name, rec.note)
		recordUninstallIncomplete(rootCtx, dyn, hc, spec, rec.msg, rec.note)
		return
	}

	// ---- Containment ----------------------------------------------------
	// The operator installs with cluster-admin credentials, so a namespaced CR
	// whose targetNamespace names ANOTHER namespace confines nothing on its own —
	// refuse it here, before adoption, so a rejected CR is never installed and never
	// carries a finalizer — unless the CR lives in a privileged namespace. NOTE:
	// this bounds the release NAMESPACE and blast radius; it does NOT stop a chart's
	// own templates from rendering cluster-scoped objects (a ClusterRoleBinding) or
	// objects with an explicit metadata.namespace elsewhere. Closing that requires
	// requester impersonation / SubjectAccessReview or manifest admission, which the
	// requester-scoped install below provides.
	if !allowed {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "",
			fmt.Sprintf("refusing to install: spec.targetNamespace %q is outside this HelmChart's namespace %q — a HelmChart may install only into its own namespace (only privileged namespaces %v may target another)",
				spec.targetNamespace, ns, pol.privilegedNamespaces))
		return
	}

	// ---- Adoption: add the finalizer, then pick up on the next tick -----
	if !hasFinalizer(hc, helmFinalizer) {
		if err := addFinalizer(rootCtx, dyn, helmChartGVR, ns, name, helmFinalizer); err != nil {
			log.Printf("helmchart %s/%s: add finalizer: %v", ns, name, err)
		}
		return
	}

	// ---- Values from Secrets ---------------------------------------------
	// Only in a privileged namespace, where the chart installs as the operator
	// and anyone who can write a Secret is already as trusted (the reason a
	// HelmChartConfig there needs no matching requester either). Elsewhere a
	// Secret's writer, who need not be able to write the HelmChart, would steer
	// the release with its requester's authority.
	var secretValues []map[string]interface{}
	var secretVersions []string
	if len(spec.valuesSecrets) > 0 {
		if impersonationRequired(ns, pol.privilegedNamespaces) {
			setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", fmt.Sprintf(
				"refusing to install: spec.valuesSecrets is honoured only in a privileged namespace %v, where the chart installs as the operator; here a Secret's writer would steer the release with this HelmChart's requester's authority", pol.privilegedNamespaces))
			return
		}
		// A Deployed chart keeps its release through a read that may work next
		// pass (a 429, an apiserver restart): marking it Failed would make the
		// next pass upgrade it with the same values, adding a revision and
		// re-running its hooks.
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

	// ---- Merge values (defaults < spec < HCC < Secrets < set) -----------
	vals, err := mergedValues(spec, hccValues, secretValues...)
	if err != nil {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", "values: "+err.Error())
		return
	}
	hash := specHash(spec, vals)
	if len(spec.valuesSecrets) > 0 {
		// The status carries the hash: cover the Secrets' versions, never
		// values merged from them.
		hash = secretSpecHash(spec, hccValues, secretVersions)
	}

	// ---- Cheap hot path: nothing to do? ---------------------------------
	// A Deployed release whose specHash is unchanged AND whose backing release
	// Secret still exists needs no helm work — just a single Secret GET.
	status, _, _ := unstructured.NestedMap(hc.Object, "status")
	if status != nil {
		curHash := str(status["specHash"], "")
		curPhase := str(status["phase"], "")
		if curHash == hash && curPhase == hcPhaseDeployed {
			rev := asInt(status["revision"])
			relSecret := fmt.Sprintf("sh.helm.release.v1.%s.v%d", spec.releaseName, rev)
			if rev > 0 && exists(rootCtx, dyn, secretGVR, spec.targetNamespace, relSecret) {
				// The EFFECTIVE insecure-TLS value can change with no spec change:
				// --allow-insecure-helm-repos is the operator's flag and is deliberately
				// not part of specHash (flipping it must not force a helm upgrade of
				// every chart). Without this, a settled release keeps reporting whatever
				// it reported when it last reconciled, indefinitely.
				if current, present := status["insecureTLSVerify"].(bool); !present || current != spec.insecureSkipTLSVerify {
					if err := refreshHelmTLSStatus(rootCtx, dyn, ns, name, spec.insecureSkipTLSVerify); err != nil {
						log.Printf("helmchart %s/%s: refresh TLS status: %v", ns, name, err)
					}
				}
				return // converged
			}
		}
		// A recovery uninstall left objects Helm could not verify: no reinstall
		// over them until a person has removed them and changed the spec. The
		// hold is the status.uninstallIncomplete field, so it outlives any later
		// Failed write that replaced the message; the message prefix still
		// counts for a hold recorded before the field existed.
		if hold, ok := incompleteHoldFrom(status); ok {
			if hold.specHash == "" || hold.specHash == hash {
				// Regenerate the hold's message from the field, so a person
				// still sees what to do after a values typo or a refused
				// overlay replaced it.
				if want := incompleteHoldMessage(hold); want != str(status["message"], "") {
					setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", want)
				}
				log.Printf("helmchart %s/%s: an earlier uninstall left objects Helm could not verify; not reinstalling until a person removes them and changes the spec (see status.message)", ns, name)
				return
			}
			// The specHash differs from the one recorded at hold time: the
			// changed spec the hold waits for arrived, and recording the
			// reinstall below clears the field.
		} else if curHash == hash && uninstallIncomplete(status) {
			log.Printf("helmchart %s/%s: an earlier uninstall left objects Helm could not verify; not reinstalling until a person removes them and changes the spec (see status.message)", ns, name)
			return
		}
		// Failed-backoff: don't retry a just-failed chart every tick.
		if curHash == hash && curPhase == hcPhaseFailed {
			if last := str(status["lastAttempt"], ""); last != "" {
				if t, err := time.Parse(time.RFC3339, last); err == nil && time.Since(t) < failedBackoff {
					return
				}
			}
		}
	}

	// The status still carrying the recovery evidence (unrecordedRecoveryField,
	// set with the pre-uninstall marker and kept standing by every write
	// since) means an earlier process's recovery uninstall never recorded its
	// result: the operator restarted between Helm's purge and the "uninstall
	// incomplete:" status write, and the restart discarded the record of the
	// unwritten write. The message the marker came with may be gone — a
	// pre-Get Failed write, this reconcile's own "reconciling" write, or an
	// earlier restart's replaces it — so the decision keys on the field, as
	// the hold gate keys on holdField. If the release is gone below, the
	// field is the only evidence an uninstall ran, so the reconcile holds
	// instead of installing (an unrecorded uninstall).
	unrecordedRecovery := status != nil && asBool(status[unrecordedRecoveryField])

	// Helm's errors can quote values: keep them out of the status for a chart
	// whose release, or its history, was rendered with Secret values.
	privateErrors := privateChart(hc, spec)

	// ---- Requester backstop + install identity --------------------------
	// Gate the actual install/upgrade — the only path that APPLIES rendered
	// objects, which is the escalation vector. A non-privileged CR must carry a
	// requester stamp; without one it is refused here (fail-closed) rather than
	// installed as the operator, so a webhook outage cannot silently escalate.
	// A privileged-namespace CR installs as the operator (imp == nil). imp is
	// used below so every applied object is authorized against the requester,
	// not against the operator's system:masters. This sits AFTER the converged
	// hot path on purpose: an already-deployed release applies nothing, so it
	// is not re-refused (and a release installed before requester stamping
	// keeps running until its next change, which — being an apply through the
	// API — re-stamps it).
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
	// A same-name HelmChartConfig overlay installs under this HelmChart's identity,
	// so it must be the SAME (authentic) requester — otherwise its author would
	// apply values under the chart owner's authority.
	hcRequester := requesterInfo{}
	if imp != nil {
		hcRequester = requesterInfo{Username: imp.UserName, UID: imp.UID, Groups: imp.Groups}
	}
	if refuseHCC, msg := hccInstallAllowed(hc, hccValues, hcc, hcRequester, pol.privilegedNamespaces, pol.signer); refuseHCC {
		setHelmStatus(rootCtx, dyn, ns, name, hcPhaseFailed, spec, 0, "", msg)
		return
	}

	// ---- Insecure-repo TLS log ------------------------------------------
	// The effective value was normalized up front; here we only LOG, and only past
	// the converged hot path so it isn't repeated every tick for a settled release.
	// requestedInsecure is the raw request (already false for inline charts, which
	// do no fetch), spec.insecureSkipTLSVerify the operator-gated effective value.
	if requestedInsecure {
		if spec.insecureSkipTLSVerify {
			log.Printf("helmchart %s/%s: WARNING repo TLS verification DISABLED via spec.insecureSkipTLSVerify (permitted by --allow-insecure-helm-repos) for repo %q", ns, name, spec.repo)
		} else {
			log.Printf("helmchart %s/%s: spec.insecureSkipTLSVerify ignored — operator did not set --allow-insecure-helm-repos; verifying repo TLS", ns, name)
		}
	}

	// ---- Helm work (bounded by spec.timeout off the ROOT ctx) -----------
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
		// An unrecorded recovery uninstall: the marker above proves an earlier
		// process ran the uninstall, Helm purged the release, and nothing
		// recorded what it left — Helm keeps no record of that once purged, so
		// a missing release proves nothing. Record the hold from what is
		// known: whatever of the release remains, a person must remove before
		// a spec change reinstalls. The release the uninstall touched is the
		// one the status recorded, not necessarily the one the spec names now.
		if unrecordedRecovery {
			relNS, relName := str(status["releaseNamespace"], spec.targetNamespace), str(status["releaseName"], spec.releaseName)
			log.Printf("helmchart %s/%s: recovery uninstall of release %s/%s never recorded its result; not reinstalling over the objects it may have left", ns, name, relNS, relName)
			recordUninstallIncomplete(rootCtx, dyn, hc, spec,
				uninstallIncompleteRecover+fmt.Sprintf("any objects of release %s/%s that remain (an earlier uninstall ended without recording what it left)", relNS, relName), "")
			return
		}
		// First install.
		inst := action.NewInstall(acfg)
		inst.ReleaseName = spec.releaseName
		inst.Namespace = spec.targetNamespace
		inst.CreateNamespace = spec.createNamespace
		// Keep Helm v3's client-side apply. Helm v4 installs with server-side
		// apply by default, which under requester impersonation needs the
		// requester to hold patch rights on every object the chart creates.
		// Upgrades and rollbacks stay on "auto", which follows the apply method
		// the release recorded, so a release installed here stays client-side.
		inst.ServerSideApply = false
		inst.WaitStrategy = helmWaitStrategy
		inst.Timeout = spec.timeout
		// The action's transport carries ctx. Wait for the Helm worker to
		// finish; RunWithContext can return while that worker is still running.
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

	// A release stuck mid-transition (pending-install/upgrade/rollback) — from a
	// crash or a killed prior attempt — must be recovered before a clean upgrade,
	// else helm refuses with "another operation is in progress". A release that
	// a failed recovery uninstall left uninstalling needs the same recovery, or
	// the upgrade would run against a partly removed release.
	if rel.Info != nil && needsRecovery(rel.Info.Status) {
		stuck := rel.Info.Status
		// Mark the recovery before Helm work, after everything that could stop
		// it: should the operator restart between Helm's purge and the
		// "uninstall incomplete:" status write, the marker is what survives,
		// and the next reconcile holds on it instead of reinstalling. The
		// write carries the hash, so a hold recorded after that restart keeps
		// the specHash its gate compares against.
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
				// Retain only the fields the status retry needs; an inline
				// chartContent may be large and the outage may persist.
				identity := helmSpec{releaseName: spec.releaseName, targetNamespace: spec.targetNamespace, insecureSkipTLSVerify: spec.insecureSkipTLSVerify}
				cleanRecoveryOutcomes.Store(incompleteKey(hc), cleanRecoveryOutcome{spec: identity, hash: hash, revision: int64(revOf(rel))})
			}
			log.Printf("helmchart %s/%s: record the clean recovery outcome: %v", ns, name, err)
			return
		}
		log.Printf("helmchart %s/%s: release %s was %s, recovering (retry next tick)", ns, name, spec.releaseName, stuck)
		return
	}

	// Upgrade in place. ResetValues so our merged values are authoritative
	// (we don't want stale values from a prior revision leaking in).
	up := action.NewUpgrade(acfg)
	up.Namespace = spec.targetNamespace
	up.ResetValues = true
	up.MaxHistory = maxHelmHistory
	up.WaitStrategy = helmWaitStrategy
	up.Timeout = spec.timeout
	// Cancellation is enforced by the transport. Run waits for the worker;
	// RunWithContext can leave it blocked sending to an abandoned result channel.
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

// needsRecovery reports whether a release must be recovered before an upgrade:
// stuck in a pending-* status, or left uninstalling by a recovery uninstall
// that failed partway.
func needsRecovery(s common.Status) bool {
	return s.IsPending() || s == common.StatusUninstalling
}

// recoverPendingRelease unwedges a release stuck in a pending-* status, or left
// uninstalling. A release with an earlier revision that served is rolled back
// to it, as lastServedBefore picks it. One with none — a first-ever attempt, or
// only failed attempts before it — or one already part way through its
// uninstall has no good state to roll back to, so it is uninstalled (history
// dropped). On success the caller returns and the
// clean install/upgrade happens on the next tick; on error the release is left
// for the next tick to recover again. left is Helm's list of objects the
// uninstall left because it could not verify their ownership, which the caller
// must surface instead of reinstalling; it is empty otherwise. note is Helm's
// whole note on the objects the uninstall left, which also goes to the operator
// log.
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

// lastServedBefore returns the newest revision of release name, older than
// rev, that served: the newest earlier deployed revision, or failing that the
// newest earlier superseded one that completed, or 0 when there is neither.
//
// Superseded alone is not proof a revision served: Helm marks the release a
// failed rollback started from superseded even when it never deployed (helm
// v4.3.0 pkg/action/rollback.go:255), and a crash before that rollback records
// itself failed leaves no other trace. Superseded is still needed as a
// fallback because an upgrade or rollback marks the release it replaces
// superseded in one storage write and the new one deployed only in a later
// write (pkg/action/upgrade.go:514 and :212, rollback.go:316 and :112); cut
// short between the two, history holds a superseded revision that served and
// no deployed one, and that release must be rolled back, not uninstalled. A
// superseded revision counts only with the description install or upgrade
// writes in the same storage write that marks it deployed (install.go:564,
// upgrade.go:521); superseding changes its status and nothing else. A
// rollback's revision carries no such mark, so the fallback passes over one to
// the install or upgrade before it, which served too. Failed and pending-*
// revisions never served.
// completedDescriptions are the descriptions Helm gives a release when an
// install or upgrade of it completes. stratum sets no description of its own.
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

// uninstallRelease runs un against release name. note is Helm's note on the
// objects the uninstall left in place: those a resource policy keeps, those
// another release owns, and those whose ownership Helm could not verify.
// unverified is the note's section on the last kind, or empty when it has
// none. Only that section goes into a status message, which asks a person to
// remove the objects it lists by hand; the other objects must stay.
func uninstallRelease(un *action.Uninstall, name string) (note, unverified string, err error) {
	res, err := un.Run(name)
	if res != nil {
		note = strings.TrimSpace(res.Info)
	}
	return note, unverifiedSection(note), err
}

// unverifiedSection returns the section of Helm's uninstall note that lists
// the objects whose ownership Helm could not verify, from its header line to
// the end of its list, or "" when the note has no such section. Helm separates
// the note's sections with a blank line (helm v4.3.0
// pkg/action/uninstall.go:334-395).
func unverifiedSection(note string) string {
	i := strings.Index(note, helmUnverifiedNote)
	if i < 0 {
		return ""
	}
	section := note[strings.LastIndex(note[:i], "\n")+1:]
	section, _, _ = strings.Cut(section, "\n\n")
	return strings.TrimSpace(section)
}

// holdField is the status field an uninstall-incomplete hold is recorded in,
// beside the hold's message.
const holdField = "uninstallIncomplete"

// unrecordedRecoveryField is the status field a pending-install recovery's
// unresolved uninstall is recorded in, beside the pre-uninstall marker
// message: writeHelmStatus sets it with recoveringUninstallMarker and clears
// it when the recovery records its outcome. Like holdField it survives every
// other status write, so the writes a reconcile makes before it decides on
// the release (a "reconciling" or Failed write) cannot destroy the only
// evidence a restart has of the unrecorded uninstall.
const unrecordedRecoveryField = "unrecordedRecovery"

// The two paths a hold can ask a person to take: holdPathDelete on a deleting
// HelmChart (remove the objects, then the finalizer), holdPathRecover after a
// pending install's recovery uninstall (remove the objects, then change the
// spec to reinstall).
const (
	holdPathDelete  = "delete"
	holdPathRecover = "recover"
	// Earlier operator versions used this shorter recovery instruction. Keep
	// recognizing it when promoting their message-only holds into the field.
	legacyUninstallIncompleteRecover = uninstallIncompletePrefix + "remove these objects by hand, then change the spec to reinstall: "
)

// incompleteHold is the durable hold status.uninstallIncomplete records after
// an uninstall that left objects whose ownership Helm could not verify: the
// objects Helm's note lists, the path the hold asks a person to take, and the
// specHash current when the hold was recorded, which the reinstall gate
// compares against.
type incompleteHold struct {
	unverified string
	path       string
	specHash   string
}

// incompleteMessageParts splits a hold message into its path and the objects
// it lists: the message is one of the two hold instructions followed by Helm's
// list.
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

// migrateLegacyIncompleteHold promotes a message-only hold from an earlier
// operator version. A status-only update preserves every other field, and a
// failed update keeps the reconcile from doing Helm work until it can retry.
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

// incompleteHoldFrom reads the hold from status.uninstallIncomplete. The field
// is written beside the hold's message and survives every later Failed write
// that replaces the message (a values typo, a refused overlay), so the hold
// outlives its text.
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

// incompleteHoldMessage rebuilds the status message from the hold: today's
// instruction text for its path, followed by the objects it lists.
func incompleteHoldMessage(hold incompleteHold) string {
	if hold.path == holdPathDelete {
		return uninstallIncompleteDelete + hold.unverified
	}
	return uninstallIncompleteRecover + hold.unverified
}

// uninstallIncomplete reports whether a HelmChart status records an uninstall
// that left objects whose ownership Helm could not verify: the
// status.uninstallIncomplete field writeHelmStatus sets beside the message, or
// — for a hold recorded before the field existed — the message prefix itself.
func uninstallIncomplete(status map[string]interface{}) bool {
	if _, ok := incompleteHoldFrom(status); ok {
		return true
	}
	return str(status["phase"], "") == hcPhaseFailed &&
		strings.HasPrefix(str(status["message"], ""), uninstallIncompletePrefix)
}

// deletionIncompleteMessage turns an "uninstall incomplete:" message into the
// one for a deleting HelmChart, which asks for the finalizer to be removed
// rather than for a spec change. Helm's list in it is kept.
func deletionIncompleteMessage(msg string) string {
	if strings.HasPrefix(msg, uninstallIncompleteDelete) {
		return msg
	}
	if _, unverified, ok := incompleteMessageParts(msg); ok {
		return uninstallIncompleteDelete + unverified
	}
	return uninstallIncompleteDelete + strings.TrimPrefix(msg, uninstallIncompleteRecover)
}

// incompleteWriteBackoff paces the attempts at an "uninstall incomplete:"
// status write within one reconcile: five attempts over about 1.5s, enough to
// ride out a transient apiserver error such as an etcd leader change without
// holding up the other HelmCharts in the loop for long.
var incompleteWriteBackoff = wait.Backoff{Duration: 100 * time.Millisecond, Factor: 2, Jitter: 0.1, Steps: 5}

// unwrittenIncomplete remembers, by incompleteKey, each "uninstall
// incomplete:" status this process failed to write. Helm has purged the
// release by then, so without the status the next reconcile would find no
// release and remove the finalizer or reinstall. While a HelmChart has an
// entry the operator does neither and retries the write each reconcile
// instead. The entry lives only in this process, but the hold does not die
// with it: each uninstall writes a marker to the status before Helm work
// (uninstallingMessage for a deleted HelmChart, recoveringUninstallMarker for
// a recovery), and finding that marker with the release gone, a reconcile
// after a restart records the hold instead of reinstalling.
var (
	unwrittenIncompleteMu sync.Mutex
	unwrittenIncomplete   = map[string]incompleteRecord{}
)

// cleanUninstalls holds, by incompleteKey, each deleted HelmChart whose
// uninstall this process saw finish clean but could not record in its status,
// until its finalizer is removed.
var cleanUninstalls sync.Map

// cleanRecoveryOutcomes holds a recovery result that this process observed
// cleanly but could not yet write to status. Retrying it clears the durable
// unrecordedRecovery marker. A restart loses this observation and therefore
// leaves the marker's conservative hold in force.
var cleanRecoveryOutcomes sync.Map // incompleteKey -> cleanRecoveryOutcome

type cleanRecoveryOutcome struct {
	spec     helmSpec
	hash     string
	revision int64
}

// incompleteRecord is an "uninstall incomplete:" status message still to be
// written, with Helm's whole uninstall note for the log.
type incompleteRecord struct {
	msg, note string
}

// incompleteKey identifies hc in unwrittenIncomplete. The UID keeps a
// HelmChart recreated under the same name from inheriting an entry.
func incompleteKey(hc *unstructured.Unstructured) string {
	return hc.GetNamespace() + "/" + hc.GetName() + "/" + string(hc.GetUID())
}

// unwrittenIncompleteFor returns hc's entry in unwrittenIncomplete, if any.
func unwrittenIncompleteFor(hc *unstructured.Unstructured) (incompleteRecord, bool) {
	unwrittenIncompleteMu.Lock()
	defer unwrittenIncompleteMu.Unlock()
	rec, ok := unwrittenIncomplete[incompleteKey(hc)]
	return rec, ok
}

// recordUninstallIncomplete sets hc Failed with the "uninstall incomplete:"
// message msg. Helm has purged the release by then, so that status is the only
// lasting record that the uninstall left objects behind. It retries the write
// with incompleteWriteBackoff until it succeeds or ctx ends, logging Helm's
// whole note at each failed attempt. A write that still fails goes into
// unwrittenIncomplete, so that the next reconcile retries it instead of
// removing the finalizer or reinstalling; a write that succeeds, or finds the
// HelmChart gone, clears hc's entry.
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

// ----------------------------- chart loading -------------------------------

// loadChart resolves the chart either from an inline base64 .tgz (chartContent,
// which wins) or by locating it in the configured repo (HTTP(S) or oci://). A
// repo fetch is bounded by spec.timeout: a hung repository fails the chart
// when the timeout expires instead of holding the reconcile pass until Helm's
// own client timeout gives up.
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

	// The cache lives under version.HelmCacheDir (the data dir), read at call
	// time. Both settings are set explicitly because HELM_CACHE_HOME cannot
	// carry this layout: it moves the repo/content caches but not
	// repositories.yaml, which follows the config home of the invoking user
	// (helm v4 pkg/cli/environment.go:122-124), and the operator runs as root.
	// A node upgraded from a release that kept this state at the legacy path
	// carries it over first, or its named repositories stop resolving.
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

// migrateLegacyHelmCache moves an upgraded node's helm repository state into
// the data dir. Releases before the move kept the administrator-owned
// repositories.yaml (named repositories, credentials, TLS file paths) and the
// cached <repo>-index.yaml files at /var/lib/stratum-helm; a named-repository
// fetch consults the data dir now, and nothing in the operator downloads an
// index — with the migration absent, the fetch fails "repo name not found" or
// "no cached repo found" where it resolved before, dropping the entry's
// credentials and TLS options on the floor. Each file is created exclusively
// at its data-dir home, so the copy never overwrites what is there, and it
// preserves each file's mode, so an interrupted reconcile never leaves a
// truncated repositories.yaml behind. A refused configuration copy means one
// of two things, and the content tells them apart: a byte-identical
// repositories.yaml is this migration's own earlier copy, interrupted before
// the indexes landed, and the index scan resumes — anything else is an
// established home whose stale legacy copy must stay out of it, indexes
// included, an index of the same name pointing a lookup at another
// repository's URLs. A failure names the legacy path and fails the fetch —
// the fetch would fail anyway, without saying why.
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
		// CacheIndexFile names an index <repo>-index.yaml; that is all the
		// lookup reads (chartfetch.go's entry resolution and Helm's
		// ResolveChartVersion), so that is all the migration carries.
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "-index.yaml") {
			continue
		}
		dst := filepath.Join(version.HelmCacheDir, "cache", entry.Name())
		// A migrated index needs neither read nor copy: an index runs to
		// megabytes, and the legacy copy stays put for uninstall to remove.
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
			// An index the new cache already holds stays as it is.
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

// migrateLegacyHelmFile copies one file of the legacy helm cache into its
// data-dir home, preserving the source's mode and writing through the atomic
// single-file writer's exclusive create: the copy refuses a destination that
// already exists, so a reconcile can never overwrite the data-dir home with
// the stale legacy copy. The destination is checked before anything is
// staged — the legacy files outlive the migration by design, so every later
// load would otherwise rewrite each of them whole only for the link to
// refuse it. The check narrows the race; the exclusive create remains the
// backstop that keeps it safe. A missing source is an fs.ErrNotExist and an
// existing destination an fs.ErrExist, for the caller to fold into "nothing
// to migrate".
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

// helmLogHandler sends Helm's log output, debug messages included, to the
// operator's log, as Helm v3's Init(..., log.Printf) did. Helm v4 logs through
// slog, and an action.Configuration built without a logger discards it. The
// time attribute is dropped because the standard logger stamps each line
// itself.
var helmLogHandler slog.Handler = slog.NewTextHandler(stdLogWriter{}, &slog.HandlerOptions{
	Level: slog.LevelDebug,
	ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	},
})

// stdLogWriter writes each line it receives through the standard logger, so
// Helm's lines carry the operator log's prefix and flags.
type stdLogWriter struct{}

func (stdLogWriter) Write(p []byte) (int, error) {
	if err := log.Output(2, string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// newActionConfigFn is newActionConfig; the Helm flow tests replace it with an
// in-memory Helm backend so they drive reconcileHelmChart end to end without a
// cluster.
var newActionConfigFn = newActionConfig

// newActionConfig builds a Helm action.Configuration bound to the given
// kubeconfig + namespace, using the "secret" storage driver, gives its
// Kubernetes client Helm v3's waits (v3WaitClient), and attaches an OCI
// registry client so oci:// charts resolve.
//
// When imp != nil, EVERY Kubernetes call this config makes — applying the
// rendered objects AND reading/writing the release-storage Secret — runs
// impersonated as the requester. Impersonation replaces the effective
// identity for authorization, so the operator's system:masters no longer
// short-circuits the apiserver's RBAC check: a chart that renders an object the
// requester may not create is refused by the apiserver, not applied. imp == nil
// keeps the operator's own (cluster-admin) identity, for privileged-namespace
// installs (the bundled add-ons).
func newActionConfig(ctx context.Context, kubeconfig, namespace string, imp *rest.ImpersonationConfig) (*action.Configuration, error) {
	acfg := action.NewConfiguration(action.ConfigurationSetLogger(helmLogHandler))
	// Helm v4 removed kube.GetConfig; this is its Helm v3 body.
	kubeContext := ""
	getter := genericclioptions.NewConfigFlags(true)
	getter.Namespace = &namespace
	getter.Context = &kubeContext
	getter.KubeConfig = &kubeconfig
	// Helm's install/upgrade workers can outlive RunWithContext, and its
	// uninstall/rollback APIs do not take a context. Bind every Kubernetes
	// client (including release storage and discovery) to this invocation so
	// cancellation also stops their requests and prevents subsequent writes.
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

// crdGK is the group and kind of a CustomResourceDefinition.
var crdGK = schema.GroupKind{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}

// v3WaitClient is Helm v4's Kubernetes client with the waits Helm v3 made with
// Wait = false for actions that ask for helmWaitStrategy. Helm v4's own waiter
// for that strategy watches every hook through namespace-wide list and watch
// requests, which fail for an impersonated requester who may create the hook's
// kind but not list or watch it, and does not wait for the CRDs an install
// creates, so the chart's custom resources of those kinds then fail to map, or
// are left out by a .Capabilities.APIVersions check.
type v3WaitClient struct{ *kube.Client }

var _ kube.InterfaceWaitOptions = v3WaitClient{}

func (c v3WaitClient) GetWaiter(ws kube.WaitStrategy) (kube.Waiter, error) {
	return c.GetWaiterWithOptions(ws)
}

// GetWaiterWithOptions makes v3WaitClient a kube.InterfaceWaitOptions, which
// every Helm action asks for in place of GetWaiter.
//
// Helm v4.3.1's install asks for StatusWatcherStrategy, in place of
// HookOnlyStrategy, to wait for a chart's crds/; the operator asks for no other
// strategy, so that request is answered as helmWaitStrategy is.
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

// v3NoWait waits as Helm v3 did with Wait = false, through Helm's legacy (v3)
// waiter: WatchUntilReady watches only Job and Pod hooks, each through a watch
// on its name, Wait waits only for CRDs, until they are Established, and
// WaitForDelete waits only for deleted hooks. It differs from v3 in one way:
// Wait also waits for the CRDs a chart renders from templates/ when they are
// installed, upgraded or rolled back, which v3 did not.
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

// WaitForDelete waits until deleted hooks are gone, as v3 did after deleting
// a hook by its delete policy, so a before-hook-creation hook is gone before
// Helm creates it again. Helm also calls it after an uninstall, where v3 did
// not wait with Wait = false; the objects an uninstall deletes are the
// release's, which never carry the hook annotation.
func (w v3NoWait) WaitForDelete(resources kube.ResourceList, timeout time.Duration) error {
	if !allHooks(resources) {
		return nil
	}
	return w.legacy.WaitForDelete(resources, timeout)
}

// allHooks reports whether resources holds at least one object and every one
// carries Helm's hook annotation.
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

// ----------------------------- values --------------------------------------

// mergedValues computes the effective values map by layering, low precedence to
// high: HelmChart.valuesContent (YAML) < HelmChartConfig.valuesContent (YAML) <
// secretValues, the spec.valuesSecrets bodies readValuesSecrets read, in order <
// spec.set (strvals) < spec.setString (strvals, no type inference). The chart's
// own defaults sit below all of these and are applied by helm at render time.
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
	// spec.set wins over everything (strvals a=b,c.d=e syntax).
	// Sorted keys: map order is random, and with prefix-overlapping keys a
	// fixed order makes the overlap fail the same way every time.
	for _, k := range slices.Sorted(maps.Keys(spec.set)) {
		v := spec.set[k]
		if err := strvals.ParseInto(k+"="+v, base); err != nil {
			return nil, fmt.Errorf("set %q: %w", k, err)
		}
	}
	// spec.setString is the same thing without type inference: "1.0" stays the
	// string "1.0", "true" stays "true". Applied AFTER spec.set so it wins a
	// same-key collision, matching helm's own --set/--set-string ordering in
	// cli/values.Options.MergeValues. That ordering is what makes it a usable
	// escape hatch: a user whose value is arriving with the wrong type adds the
	// key to setString without having to remove the set entry first.
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

// mergeMaps deep-merges src over dst (src wins). Nested maps recurse; every other
// value (including slices) is replaced wholesale, as Helm does.
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

// ----------------------------- spec + hash ---------------------------------

// parseHelmSpec reads the HelmChart .spec into the defaulted helmSpec the
// controller reconciles from. Field names, schema types and defaults are
// declared once, in helmChartSpecTable: parseHelmSpec reads each field by its
// row's name and takes every declared default from its row, so the table —
// not this function — is where the contract lives. Fields whose row declares
// no default fall back per field: strings to empty, set/setString to empty
// maps, releaseName to the CR name and targetNamespace to the CR namespace
// (both documented in the CRD's descriptions).
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
	// timeout: a Go duration string. Absent or invalid falls back to the
	// table's declared default — the same string the CRD declares — so the
	// fallback cannot drift from the promise. ParseDuration can then only
	// fail on a corrupted table, which TestHelmChartCRDMatchesTheSpecTable
	// catches, not this path.
	s.timeout, _ = time.ParseDuration(str(helmChartSpecDefault("timeout"), ""))
	if t := str(nested(hc.Object, "spec", "timeout"), ""); t != "" {
		if d, err := time.ParseDuration(t); err == nil && d > 0 {
			s.timeout = d
		} else {
			log.Printf("helmchart %s/%s: invalid spec.timeout %q, defaulting to %s", ns, name, t, s.timeout)
		}
	}
	// An inline chart does no repo/registry fetch, so insecureSkipTLSVerify is
	// meaningless there (the CRD documents it as ignored). Zero it so it is never
	// logged or surfaced in .status as if a fetch were skipped.
	if s.chartContent != "" {
		s.insecureSkipTLSVerify = false
	}
	return s
}

// helmChartSpecDefault returns the default the table declares for one spec
// field, or nil when its row declares none.
func helmChartSpecDefault(field string) interface{} {
	for _, row := range helmChartSpecTable {
		if row.name == field {
			return row.def
		}
	}
	return nil
}

// specBool reads one boolean HelmChart spec field, falling back to the
// default its table row declares (the same default the CRD promises) when the
// field is absent.
func specBool(hc *unstructured.Unstructured, field string) bool {
	if b, ok := nested(hc.Object, "spec", field).(bool); ok {
		return b
	}
	b, _ := helmChartSpecDefault(field).(bool)
	return b
}

// specHash is a stable fingerprint over everything that would change the
// rendered release: the source coordinates (repo/chart/version), the inline
// chart bytes (hashed, not embedded), and the fully-merged values (canonical
// JSON). Unchanged hash => converged => no helm work (see the hot path). A
// chart with spec.valuesSecrets uses secretSpecHash instead, since its merged
// values carry the Secrets' content and the hash is in the status.
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

// ----------------------------- finalizers ----------------------------------

func hasFinalizer(o *unstructured.Unstructured, f string) bool {
	return slices.Contains(o.GetFinalizers(), f)
}

// addFinalizerList / removeFinalizerList are the pure list-manipulation cores,
// split out so they're unit-testable without a cluster. Both return a fresh
// slice rather than aliasing cur — the caller hands the result to SetFinalizers
// on a live object.
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

// ----------------------------- status --------------------------------------

// refreshHelmTLSStatus changes only the effective policy field, preserving
// ownership and deployment metadata because no Helm operation took place.
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

// recordDeployed stamps a successful install/upgrade: phase Deployed with the
// live revision + resolved chart version.
func recordDeployed(ctx context.Context, dyn dynamic.Interface, ns, name string, spec helmSpec, hash string, rel *release) {
	setHelmStatusHash(ctx, dyn, ns, name, hcPhaseDeployed, spec, hash, int64(revOf(rel)),
		chartVer(rel, nil), "release deployed")
}

// setHelmStatus is the failure/transition path: no specHash update (a failure
// must not mark the current spec as converged) and revision optional.
func setHelmStatus(ctx context.Context, dyn dynamic.Interface, ns, name, phase string, spec helmSpec, revision int64, chartVersion, msg string) {
	writeHelmStatus(ctx, dyn, ns, name, phase, spec, "", revision, chartVersion, msg, false)
}

// setHelmStatusHash also persists the specHash + chartVersion (Deployed/Pending).
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
		// Once ownership is recorded, a stale/mutated spec cannot move it.
		// Refuse before Helm work rather than losing track of the old release.
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
		// Skip a redundant write: same phase+message+hash+revision and generation
		// already observed => don't hot-loop UpdateStatus.
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
		// The hold outlives its message: record it in its own field beside the
		// message, so a later Failed write that replaces the message (a values
		// typo, a refused overlay) does not lift the hold. The field carries
		// what the message alone cannot: the path the hold asks for, and the
		// specHash current when it was recorded — the last recorded hash, which
		// every hold is written beside — which the reinstall gate compares
		// against. Only an install or upgrade recorded under the current spec
		// (recordIdentity) proves the changed spec the hold waits for arrived,
		// so only that clears the field.
		if path, unverified, ok := incompleteMessageParts(msg); ok {
			st[holdField] = map[string]interface{}{
				"unverified": unverified,
				"path":       path,
				"specHash":   str(st["specHash"], ""),
			}
			// The hold replaces the recovery marker as the record of the
			// unrecorded uninstall: it is the stronger evidence, naming the
			// objects and the hash its gate compares against.
			delete(st, unrecordedRecoveryField)
		}
		// The recovery's pre-uninstall marker is recorded the same way, in its
		// own field beside the message: the writes a reconcile makes before it
		// decides on the release (the "reconciling" write, a Failed write for a
		// load-chart or action-config error or a refusal) replace the message,
		// and a restart between one of those and the release decision must not
		// lose the evidence of the unrecorded uninstall. It clears when the
		// uninstall records its outcome: the hold, the recovering-pending
		// write, a deleted HelmChart's clean uninstall, or a Deployed release,
		// which needs neither.
		if msg == recoveringUninstallMarker {
			st[unrecordedRecoveryField] = true
		}
		if msg == recoveringPendingMessage || msg == uninstalledMessage || phase == hcPhaseDeployed {
			delete(st, unrecordedRecoveryField)
		}
		st["observedGeneration"] = gen
		// Persist before Helm starts: even a failed install may leave a release.
		if recordIdentity {
			st["releaseName"] = spec.releaseName
			st["releaseNamespace"] = spec.targetNamespace
			delete(st, holdField)
			// A release rendered with Secret values keeps them in Helm's history,
			// which a recovery can roll back to after spec.valuesSecrets is gone,
			// so the mark is never cleared (helmFailure).
			if len(spec.valuesSecrets) > 0 {
				st[valuesFromSecretsField] = true
			}
		}
		st["lastAttempt"] = time.Now().UTC().Format(time.RFC3339)
		// Surface whether repo TLS verification was actually skipped. By the
		// time this is written on the install/upgrade path, spec.insecureSkipTLSVerify
		// is the EFFECTIVE value (the gate above zeroes it unless the operator opted in).
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

// ----------------------------- small helpers -------------------------------

// release is a local alias so signatures stay short throughout this file.
type release = helmrelease.Release

// asRelease asserts a release returned by Helm v4's Get, Install or Upgrade,
// typed as release.Releaser (an alias for any), to the v1 release they return.
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

// chartVer returns the release's chart version, falling back to a directly
// loaded chart's metadata when the release doesn't carry it.
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

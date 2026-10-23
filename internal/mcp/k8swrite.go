package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/selinux"
)

// The generic Kubernetes write toolset.

// FieldManager is the field-manager name every apply from this server uses.
const FieldManager = "stratum-mcp"

// deniedWrites are resources this server will not create, modify or delete
// at any tier: identity, RBAC, admission and credential objects. This list is
// not a security boundary; Kubernetes RBAC is.
var deniedWrites = map[schema.GroupResource]string{
	{Group: "rbac.authorization.k8s.io", Resource: "roles"}:               "RBAC: writing it can escalate privilege",
	{Group: "rbac.authorization.k8s.io", Resource: "rolebindings"}:        "RBAC: writing it can escalate privilege",
	{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"}:        "RBAC: writing it can escalate privilege",
	{Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"}: "RBAC: writing it can escalate privilege",

	{Group: "admissionregistration.k8s.io", Resource: "validatingwebhookconfigurations"}: "admission control: writing it can disable the cluster's own guardrails",
	{Group: "admissionregistration.k8s.io", Resource: "mutatingwebhookconfigurations"}:   "admission control: writing it can intercept every object in the cluster",

	// The CEL-native equivalents, in the same group and for the same reasons.
	{Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicies"}:       "admission control: writing it can disable the cluster's own guardrails",
	{Group: "admissionregistration.k8s.io", Resource: "validatingadmissionpolicybindings"}: "admission control: writing it can disable the cluster's own guardrails",
	{Group: "admissionregistration.k8s.io", Resource: "mutatingadmissionpolicies"}:         "admission control: writing it can mutate every object in the cluster",
	{Group: "admissionregistration.k8s.io", Resource: "mutatingadmissionpolicybindings"}:   "admission control: writing it can mutate every object in the cluster",

	{Group: "", Resource: "secrets"}:         "credentials",
	{Group: "", Resource: "serviceaccounts"}: "identity: writing it can escalate privilege",

	{Group: "certificates.k8s.io", Resource: "certificatesigningrequests"}: "identity: a signed certificate is a cluster credential",
	{Group: "apiregistration.k8s.io", Resource: "apiservices"}:             "an aggregated API can take over a whole API group",

	{Group: "stratum.ciq.com", Resource: "helmcharts"}:       "a chart's rendered pod specs never pass this server's checks, and in a privileged namespace the operator installs it with its own cluster-admin identity",
	{Group: "stratum.ciq.com", Resource: "helmchartconfigs"}: "the operator merges it into the HelmChart of the same name, and in a privileged namespace installs the result with its own cluster-admin identity",
}

// deniedMutations are resources this server will not create or modify,
// though deleting one may still be possible at the right tier.
var deniedMutations = map[schema.GroupResource]string{
	{Group: "", Resource: "persistentvolumes"}:      "storage can expose host files through a claim",
	{Group: "", Resource: "persistentvolumeclaims"}: "storage can expose host files through a volume",
	{Group: "", Resource: "nodes"}:                  "node objects control placement; writing one can defeat the isolation between workloads and the control plane",

	{Group: "storage.k8s.io", Resource: "storageclasses"}: "a storage class tells a provisioner where on the node to make volumes, and local-path can be told to make them anywhere",
}

// destructiveDeletes are resources whose deletion needs
// --allow-destructive, over and above --allow-write.
var destructiveDeletes = map[schema.GroupResource]string{
	{Group: "", Resource: "namespaces"}:             "deleting a namespace cascades to everything in it",
	{Group: "", Resource: "persistentvolumeclaims"}: "deleting a claim can destroy the data in its volume",
	{Group: "", Resource: "persistentvolumes"}:      "deleting a volume can destroy the data on it",
	{Group: "", Resource: "nodes"}:                  "deleting a node object evicts its workloads",

	{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}: "deleting a definition deletes every custom resource of that kind, cluster-wide",
}

// destructiveMutations are resources whose creation or change needs
// --allow-destructive, over and above --allow-write: an apply to one can be as
// hard to undo as deleting it.
var destructiveMutations = map[schema.GroupResource]string{
	{Group: "", Resource: "namespaces"}: "rewriting a namespace's finalizers or labels can wedge its deletion or change the policy every workload in it runs under",

	{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}: "rewriting a definition's schema or versions changes how every custom resource of that kind is stored and served, cluster-wide",
}

// WritePolicy is what the write tools are allowed to do. It is built from
// the server's flags, never from a tool argument.
type WritePolicy struct {
	// AllowDestructive mirrors --allow-destructive.
	AllowDestructive bool
}

// check reports why a write to gvr is refused, or nil.
func (p WritePolicy) check(gvr schema.GroupVersionResource, deleting bool) error {
	gr := schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}

	if reason, denied := deniedWrites[gr]; denied {
		return fmt.Errorf("%s is not writable through this server (%s). "+
			"This is a fixed policy, not a permission that can be granted by a flag: "+
			"make the change with kubectl, or through whatever manages it",
			describe(gr), reason)
	}

	if !deleting {
		if reason, denied := deniedMutations[gr]; denied {
			return fmt.Errorf("%s cannot be created or modified through this server (%s). "+
				"This is a fixed policy, not a permission that can be granted by a flag",
				describe(gr), reason)
		}
	}

	if p.AllowDestructive {
		return nil
	}
	if deleting {
		if reason, restricted := destructiveDeletes[gr]; restricted {
			return fmt.Errorf("deleting %s requires --allow-destructive (%s). "+
				"The server was started without it", describe(gr), reason)
		}
		return nil
	}
	if reason, restricted := destructiveMutations[gr]; restricted {
		return fmt.Errorf("creating or changing %s requires --allow-destructive (%s). "+
			"The server was started without it", describe(gr), reason)
	}
	return nil
}

// checkEngineConfigMap refuses the kube-system ConfigMaps the operator acts
// on with its own cluster-admin identity, at every tier: an inventory entry is
// a list of objects the operator deletes, and a node-client claim holds a node
// name against reuse by a later join. The names are internal/operator's
// inventoryConfigMapName and nodeClientClaimName.
func checkEngineConfigMap(gvr schema.GroupVersionResource, ns, name string) error {
	if gvr.Group != "" || gvr.Resource != "configmaps" || ns != metav1.NamespaceSystem {
		return nil
	}
	if name != "stratum-addon-inventory" && !strings.HasPrefix(name, "stratum-node-client-claim-") {
		return nil
	}
	return fmt.Errorf("ConfigMap %s/%s is not writable through this server (the operator acts on it "+
		"with its own cluster-admin identity, so writing it would get around this server's tiers). "+
		"This is a fixed policy, not a permission that can be granted by a flag", ns, name)
}

// localPathNamespace is where the bundled local-path provisioner runs.
const localPathNamespace = "local-path-storage"

// checkLocalPathNamespace refuses every write to an object in the local-path
// provisioner's namespace, at every tier. For each volume it makes or removes,
// the provisioner starts a helper pod on the node with a node directory
// mounted, and that pod never passes this server's checks. The ConfigMap there
// holds the helper's pod spec, its script and the directories it may use, and
// the Deployment chooses which ConfigMap and image it runs, so writing either
// runs a client's pod on the node.
func checkLocalPathNamespace(namespaced bool, ns string) error {
	if !namespaced || ns != localPathNamespace {
		return nil
	}
	return fmt.Errorf("objects in namespace %s are not writable through this server (the local-path "+
		"provisioner there runs a pod on the node, with a node directory mounted, for every volume, and "+
		"these objects choose what that pod runs). This is a fixed policy, not a permission that can be "+
		"granted by a flag: make the change with kubectl", ns)
}

// checkPodTemplate refuses the pod-spec fields that turn "can create a
// pod" into "can read the node".
func checkPodTemplate(obj map[string]any) error {
	return forEachPodSpec(obj, checkPodSpec)
}

// podSpecSearchDepth bounds how deep forEachPodSpec looks for an embedded pod
// spec. A manifest nested deeper is refused rather than left unchecked.
const podSpecSearchDepth = 64

// forEachPodSpec calls fn for each pod spec in obj: a Pod's spec, a
// workload's template, a CronJob's template's template, and any other map
// that carries a containers list, wherever it is nested, such as a
// PodTemplate's template or a pod spec inside a custom resource.
func forEachPodSpec(obj map[string]any, fn func(map[string]any) error) error {
	seen := map[uintptr]bool{}
	for _, path := range [][]string{
		{"spec"},
		{"spec", "template", "spec"},
		{"spec", "jobTemplate", "spec", "template", "spec"},
	} {
		spec, found, err := unstructured.NestedFieldNoCopy(obj, path...)
		m, ok := spec.(map[string]any)
		if err != nil || !found || !ok {
			continue
		}
		seen[reflect.ValueOf(m).Pointer()] = true
		if err := fn(m); err != nil {
			return err
		}
	}
	return walkPodSpecs(obj, 0, seen, fn)
}

// walkPodSpecs calls fn on every map under v that carries a containers list
// and is not already in seen.
func walkPodSpecs(v any, depth int, seen map[uintptr]bool, fn func(map[string]any) error) error {
	if depth > podSpecSearchDepth {
		return fmt.Errorf("this server will not apply a manifest nested more than %d levels deep: "+
			"it cannot check what a pod spec that deep would run", podSpecSearchDepth)
	}
	switch v := v.(type) {
	case map[string]any:
		if _, isList := v["containers"].([]any); isList && !seen[reflect.ValueOf(v).Pointer()] {
			seen[reflect.ValueOf(v).Pointer()] = true
			if err := fn(v); err != nil {
				return err
			}
		}
		for _, key := range slices.Sorted(maps.Keys(v)) {
			if err := walkPodSpecs(v[key], depth+1, seen, fn); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range v {
			if err := walkPodSpecs(item, depth+1, seen, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// claimNames collects the PersistentVolumeClaims a pod spec mounts.
func claimNames(spec map[string]any) []string {
	volumes, found, err := unstructured.NestedSlice(spec, "volumes")
	if err != nil || !found {
		return nil
	}
	var out []string
	for _, v := range volumes {
		vol, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if name, found, err := unstructured.NestedString(vol, "persistentVolumeClaim", "claimName"); err == nil && found && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// checkClaimedVolumes refuses a pod spec that mounts, or could come to mount,
// a node-path PersistentVolume: through a claim bound to one, a pending claim
// that could bind one, or a claim template that could.
//
// This server cannot write PersistentVolumes or claims (deniedMutations), but
// it can mount ones an operator left behind, which reaches the node's
// filesystem with no hostPath in the pod spec. An absent claim is allowed:
// nothing can be created for it through this server. A claim that cannot be
// read is refused.
func checkClaimedVolumes(ctx context.Context, typed kubernetes.Interface, obj map[string]any, ns string) error {
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	var pending []*corev1.PersistentVolumeClaim
	if err := forEachPodSpec(obj, func(spec map[string]any) error {
		for _, name := range claimNames(spec) {
			claim, err := typed.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read claim %q to check what backs it: %w", name, err)
			}
			if claim.Spec.VolumeName == "" {
				pending = append(pending, claim)
				continue
			}
			if err := checkBackingVolume(ctx, typed, claim.Spec.VolumeName, name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return checkUnboundClaims(ctx, typed, obj, ns, pending)
}

// annDynamicallyProvisioned is set by a provisioner on every PV it creates.
const annDynamicallyProvisioned = "pv.kubernetes.io/provisioned-by"

// localPathProvisioner is the bundled local-path class's provisioner.
const localPathProvisioner = "rancher.io/local-path"

// nodePathSource returns "hostPath" or "local" for a PV backed by a node path,
// else "". The one node path allowed is a directory the bundled local-path
// provisioner made for the PV's claim (madeByLocalPath): every claim on the
// default class gets one, and it reaches nothing but itself.
func nodePathSource(pv *corev1.PersistentVolume) string {
	switch {
	case pv.Spec.HostPath != nil:
		if madeByLocalPath(pv, pv.Spec.HostPath.Path) {
			return ""
		}
		return "hostPath"
	case pv.Spec.Local != nil:
		if madeByLocalPath(pv, pv.Spec.Local.Path) {
			return ""
		}
		return "local"
	default:
		return ""
	}
}

// madeByLocalPath reports whether dir is the directory local-path makes for
// the claim pv is reserved for: <LocalPathDir>/<pv>_<namespace>_<claim>, on a
// PV that names local-path as its provisioner. The annotation alone is not
// enough: it says which provisioner made a PV, not what its path reaches, and
// other provisioners (the static local-volume provisioner among them) set it
// on PVs over disks and directories an operator chose.
func madeByLocalPath(pv *corev1.PersistentVolume, dir string) bool {
	ref := pv.Spec.ClaimRef
	if pv.Annotations[annDynamicallyProvisioned] != localPathProvisioner || ref == nil {
		return false
	}
	return dir == path.Join(selinux.LocalPathDir, pv.Name+"_"+ref.Namespace+"_"+ref.Name)
}

// errNodePathVolume is the single copy of the node-path refusal text. binds
// carries the pronoun and verb so they can agree with what (a set's claims are
// many, a bound or ephemeral claim is one).
func errNodePathVolume(what, binds, volume, source string) error {
	return fmt.Errorf("this server will not apply a pod spec that mounts %s: %s "+
		"PersistentVolume %q, whose %s source is a path on the node, so mounting it reaches the "+
		"node's own filesystem. This is a fixed policy, not a permission that can be granted by "+
		"a flag", what, binds, volume, source)
}

// checkBackingVolume refuses a PersistentVolume backed by a node path.
func checkBackingVolume(ctx context.Context, typed kubernetes.Interface, volume, claim string) error {
	pv, err := typed.CoreV1().PersistentVolumes().Get(ctx, volume, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read volume %q behind claim %q: %w", volume, claim, err)
	}
	return refuseNodePathBacking(pv, claim)
}

// refuseNodePathBacking refuses a claim bound to a node-path pv.
func refuseNodePathBacking(pv *corev1.PersistentVolume, claim string) error {
	source := nodePathSource(pv)
	if source == "" {
		return nil
	}
	return errNodePathVolume(fmt.Sprintf("claim %q", claim), "it is bound to", pv.Name, source)
}

// claimTemplate is a claim that is not bound yet: one the cluster will create
// from a StatefulSet's volumeClaimTemplates entry or an ephemeral volume, or an
// existing pending claim. The check is on what it could bind to now.
type claimTemplate struct {
	what       string
	volumeName string
	class      *string
	couldName  func(string) bool
	binds      string    // refusal's pronoun+verb, agreeing with what
	uid        types.UID // set only for an existing claim
}

// maxGeneratedNamePrefix is how much of a generateName the API server keeps
// before it appends 5 random characters (63 minus 5).
const maxGeneratedNamePrefix = 58

// isOrdinal reports whether s is a StatefulSet ordinal as the controller
// writes it: a non-negative decimal with no leading zero.
func isOrdinal(s string) bool {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// couldNamePod returns whether a pod of the kind and name could be named pod:
// exactly name for a Pod, <set>-<ordinal> for a StatefulSet, and a name the
// API server generates from <name>- for the controllers that generate them
// (a Deployment's ReplicaSet and a CronJob's Job add more after it, and the
// prefix is cut at maxGeneratedNamePrefix). Any other kind, or no name, can
// be any pod name.
func couldNamePod(kind, name string) func(pod string) bool {
	if name == "" {
		return func(string) bool { return true }
	}
	switch kind {
	case "Pod":
		return func(pod string) bool { return pod == name }
	case "StatefulSet":
		return func(pod string) bool {
			ordinal, ok := strings.CutPrefix(pod, name+"-")
			return ok && isOrdinal(ordinal)
		}
	case "Deployment", "ReplicaSet", "DaemonSet", "Job", "CronJob", "ReplicationController":
		prefix := name + "-"
		if len(prefix) > maxGeneratedNamePrefix {
			prefix = prefix[:maxGeneratedNamePrefix]
		}
		return func(pod string) bool { return strings.HasPrefix(pod, prefix) }
	default:
		return func(string) bool { return true }
	}
}

// claimTemplates collects every claim template in obj. couldName covers every
// name the claim can get: <template>-<set>-<ordinal> for a StatefulSet's
// template, and <pod>-<volume> for an ephemeral volume, with <pod> any name
// the workload's pods can get (couldNamePod).
func claimTemplates(obj map[string]any) []claimTemplate {
	var out []claimTemplate
	setName, _, _ := unstructured.NestedString(obj, "metadata", "name")
	kind, _, _ := unstructured.NestedString(obj, "kind")
	podNamed := couldNamePod(kind, setName)
	if templates, found, err := unstructured.NestedSlice(obj, "spec", "volumeClaimTemplates"); err == nil && found {
		for _, t := range templates {
			tmpl, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _, _ := unstructured.NestedString(tmpl, "metadata", "name")
			if name == "" {
				continue
			}
			prefix := name + "-" + setName + "-"
			out = append(out, claimTemplate{
				what:       fmt.Sprintf("the claims the set's pods will mount from volumeClaimTemplates entry %q", name),
				volumeName: templateVolumeName(tmpl),
				class:      claimTemplateClass(tmpl),
				couldName: func(claim string) bool {
					ordinal, ok := strings.CutPrefix(claim, prefix)
					return ok && isOrdinal(ordinal)
				},
				binds: "they can bind to",
			})
		}
	}
	// The ephemeral walk is its own collector here, not a clause in
	// checkPodSpec: checkPodSpec judges what a pod runs, this judges what
	// the cluster will create around it, and the two change on their own.
	_ = forEachPodSpec(obj, func(spec map[string]any) error {
		volumes, found, err := unstructured.NestedSlice(spec, "volumes")
		if err != nil || !found {
			return nil
		}
		for _, v := range volumes {
			vol, ok := v.(map[string]any)
			if !ok {
				continue
			}
			tmpl, found, err := unstructured.NestedFieldNoCopy(vol, "ephemeral", "volumeClaimTemplate")
			m, ok := tmpl.(map[string]any)
			if err != nil || !found || !ok {
				continue
			}
			volName, _, _ := unstructured.NestedString(vol, "name")
			suffix := "-" + volName
			out = append(out, claimTemplate{
				what:       fmt.Sprintf("the claim the pod will mount from ephemeral volume %q", volName),
				volumeName: templateVolumeName(m),
				class:      claimTemplateClass(m),
				couldName: func(claim string) bool {
					pod, ok := strings.CutSuffix(claim, suffix)
					return ok && podNamed(pod)
				},
				binds: "it can bind to",
			})
		}
		return nil
	})
	return out
}

// couldBind reports whether the PV controller could bind t to pv (pvClass is
// pv's class, beta annotation first). A volumeName pin binds only that PV. A
// reserved PV takes only the reserved name in ns, and, if the reservation has
// a uid, only the existing claim with that uid. Otherwise the class must
// match; a claim with no class may end up with any class.
func (t claimTemplate) couldBind(pv *corev1.PersistentVolume, pvClass, ns string) bool {
	if t.volumeName != "" {
		return t.volumeName == pv.Name
	}
	if ref := pv.Spec.ClaimRef; ref != nil {
		return ref.Namespace == ns && t.couldName(ref.Name) && (ref.UID == "" || ref.UID == t.uid)
	}
	return t.class == nil || pvClass == *t.class
}

// pendingClaim describes an existing unbound claim by its own spec.
func pendingClaim(claim *corev1.PersistentVolumeClaim) claimTemplate {
	class := claim.Spec.StorageClassName
	if ann, ok := claim.Annotations[corev1.BetaStorageClassAnnotation]; ok {
		class = &ann
	}
	return claimTemplate{
		what:      fmt.Sprintf("claim %q", claim.Name),
		class:     class,
		couldName: func(name string) bool { return name == claim.Name },
		binds:     "it can bind to",
		uid:       claim.UID,
	}
}

// templateVolumeName reads the template's pin to a specific PV, if it has one.
func templateVolumeName(template map[string]any) string {
	name, _, _ := unstructured.NestedString(template, "spec", "volumeName")
	return name
}

// claimTemplateClass reads a claim's class as the PV controller does: the beta
// annotation, else spec.storageClassName. nil means neither is set; such a
// claim can get the default class at admission or later, so it matches any.
func claimTemplateClass(template map[string]any) *string {
	if ann, found, _ := unstructured.NestedString(template, "metadata", "annotations", corev1.BetaStorageClassAnnotation); found {
		return &ann
	}
	if class, found, _ := unstructured.NestedString(template, "spec", "storageClassName"); found {
		return &class
	}
	return nil
}

// checkUnboundClaims refuses when a claim that is not bound yet could bind a
// node-path PV that exists now: a claim template in obj, a pending claim the
// pod mounts by name (pending), or an existing claim a template's set would
// reuse by name (<template>-<set>-<ordinal>), which is checked by the PV it
// names or, while pending, by its own spec. A failed list is refused.
func checkUnboundClaims(ctx context.Context, typed kubernetes.Interface, obj map[string]any, ns string, pending []*corev1.PersistentVolumeClaim) error {
	templates := claimTemplates(obj)
	if len(templates) == 0 && len(pending) == 0 {
		return nil
	}
	pvs, err := typed.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list PersistentVolumes to check what a claim template could bind to: %w", err)
	}
	claimants := make([]claimTemplate, 0, len(templates)+len(pending))
	claimants = append(claimants, templates...)
	for _, claim := range pending {
		claimants = append(claimants, pendingClaim(claim))
	}
	if len(templates) > 0 {
		// Index the PV list once instead of reading each reused claim's PV.
		byName := make(map[string]*corev1.PersistentVolume, len(pvs.Items))
		for i := range pvs.Items {
			byName[pvs.Items[i].Name] = &pvs.Items[i]
		}
		claims, err := typed.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list PersistentVolumeClaims in namespace %q to check what a claim template could bind to: %w", ns, err)
		}
		for i := range claims.Items {
			claim := &claims.Items[i]
			reused := false
			for _, t := range templates {
				if t.couldName(claim.Name) {
					reused = true
					break
				}
			}
			if !reused {
				continue
			}
			if claim.Spec.VolumeName != "" {
				// A missing PV backs nothing.
				if pv := byName[claim.Spec.VolumeName]; pv != nil {
					if err := refuseNodePathBacking(pv, claim.Name); err != nil {
						return err
					}
				}
				continue
			}
			claimants = append(claimants, pendingClaim(claim))
		}
	}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		source := nodePathSource(pv)
		if source == "" {
			continue
		}
		pvClass := pv.Spec.StorageClassName
		if ann, ok := pv.Annotations[corev1.BetaStorageClassAnnotation]; ok {
			pvClass = ann
		}
		for _, t := range claimants {
			if t.couldBind(pv, pvClass, ns) {
				return errNodePathVolume(t.what, t.binds, pv.Name, source)
			}
		}
	}
	return nil
}

func checkPodSpec(spec map[string]any) error {
	for _, field := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if on, found, err := unstructured.NestedBool(spec, field); err == nil && found && on {
			return fmt.Errorf("this server will not apply a pod spec with %s: it shares the node's "+
				"namespace with the container, which is an escape from the namespace to the host. "+
				"This is a fixed policy, not a permission that can be granted by a flag", field)
		}
	}

	volumes, found, err := unstructured.NestedSlice(spec, "volumes")
	if err == nil && found {
		for _, v := range volumes {
			vol, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if _, hasHostPath := vol["hostPath"]; hasHostPath {
				return fmt.Errorf("this server will not apply a pod spec with a hostPath volume: " +
					"it reads and writes the node's own filesystem, the cluster's credentials included. " +
					"This is a fixed policy, not a permission that can be granted by a flag")
			}
			if _, hasSecret := vol["secret"]; hasSecret {
				return errSecretInPodSpec("a secret volume")
			}
			sources, found, err := unstructured.NestedSlice(vol, "projected", "sources")
			if err != nil || !found {
				continue
			}
			for _, sv := range sources {
				source, ok := sv.(map[string]any)
				if !ok {
					continue
				}
				if _, hasSecret := source["secret"]; hasSecret {
					return errSecretInPodSpec("a projected secret source")
				}
				if _, hasToken := source["serviceAccountToken"]; hasToken {
					return errSecretInPodSpec("a projected serviceAccountToken source")
				}
			}
		}
	}

	// imagePullSecrets is pod-level, so it sits outside the container
	// loop below. The kubelet presents the named Secret to whatever
	// registry the image string points at, so an attacker-chosen
	// registry receives it -- the Secret leaves the cluster without any
	// container reading it.
	if refs, found, err := unstructured.NestedSlice(spec, "imagePullSecrets"); err == nil && found && len(refs) > 0 {
		return errSecretInPodSpec("an imagePullSecrets reference")
	}

	// Ephemeral containers cannot be set through apply, but they cost
	// nothing to check and the list is the obvious place to forget one.
	for _, list := range []string{"containers", "initContainers", "ephemeralContainers"} {
		containers, found, err := unstructured.NestedSlice(spec, list)
		if err != nil || !found {
			continue
		}
		for _, c := range containers {
			container, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if on, found, err := unstructured.NestedBool(container, "securityContext", "privileged"); err == nil && found && on {
				return fmt.Errorf("this server will not apply a pod spec with a privileged container: " +
					"it has the node's capabilities and devices. This is a fixed policy, not a " +
					"permission that can be granted by a flag")
			}
			if err := checkContainerPrivilege(container); err != nil {
				return err
			}
			if err := checkContainerSecrets(container); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkContainerPrivilege refuses the two ways a container reaches the
// host without setting privileged: added capabilities and an unmasked
// procMount.
func checkContainerPrivilege(container map[string]any) error {
	if adds, found, err := unstructured.NestedStringSlice(container, "securityContext", "capabilities", "add"); err == nil && found && len(adds) > 0 {
		return fmt.Errorf("this server will not apply a pod spec that adds Linux capabilities (%v): "+
			"SYS_ADMIN, SYS_MODULE, SYS_PTRACE and DAC_READ_SEARCH each reach the node as surely as "+
			"a privileged container does. This is a fixed policy, not a permission that can be "+
			"granted by a flag", adds)
	}
	if mount, found, err := unstructured.NestedString(container, "securityContext", "procMount"); err == nil && found && mount != "" && mount != "Default" {
		return fmt.Errorf("this server will not apply a pod spec with procMount %q: an unmasked "+
			"/proc exposes the host's own processes and kernel interfaces to the container. This is "+
			"a fixed policy, not a permission that can be granted by a flag", mount)
	}
	return nil
}

// checkContainerSecrets refuses the two ways a container reads a Secret
// through its environment.
func checkContainerSecrets(container map[string]any) error {
	if vars, found, err := unstructured.NestedSlice(container, "env"); err == nil && found {
		for _, v := range vars {
			variable, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if _, hasRef, _ := unstructured.NestedMap(variable, "valueFrom", "secretKeyRef"); hasRef {
				return errSecretInPodSpec("a secretKeyRef environment variable")
			}
		}
	}
	if froms, found, err := unstructured.NestedSlice(container, "envFrom"); err == nil && found {
		for _, f := range froms {
			from, ok := f.(map[string]any)
			if !ok {
				continue
			}
			if _, hasRef := from["secretRef"]; hasRef {
				return errSecretInPodSpec("an envFrom secretRef")
			}
		}
	}
	return nil
}

// errSecretInPodSpec refuses a pod spec that puts a Secret where the
// container -- and so pod_logs -- can read it.
func errSecretInPodSpec(what string) error {
	return fmt.Errorf("this server will not apply a pod spec with %s: the container could print "+
		"the value and pod_logs would return it, which is the read resource_get refuses at every "+
		"tier. This is a fixed policy, not a permission that can be granted by a flag", what)
}

// checkConversionWebhook refuses a CustomResourceDefinition that routes
// conversion through a webhook.
func checkConversionWebhook(gvr schema.GroupVersionResource, obj map[string]any) error {
	if gvr.Group != "apiextensions.k8s.io" || gvr.Resource != "customresourcedefinitions" {
		return nil
	}
	strategy, found, err := unstructured.NestedString(obj, "spec", "conversion", "strategy")
	if err == nil && found && strategy == "Webhook" {
		return fmt.Errorf("this server will not apply a CustomResourceDefinition whose conversion " +
			"strategy is Webhook: the API server would call that endpoint for every read and write " +
			"of every resource of the kind, and the reply replaces the object. This is a fixed " +
			"policy, not a permission that can be granted by a flag")
	}
	return nil
}

func describe(gr schema.GroupResource) string {
	if gr.Group == "" {
		return gr.Resource
	}
	return gr.Resource + "." + gr.Group
}

// DeniedWriteResources lists the denied resources for the test that pins
// them, which lives in pkg/cli and so needs them across the package
// boundary.
func DeniedWriteResources() []string {
	out := make([]string, 0, len(deniedWrites))
	for gr := range deniedWrites {
		out = append(out, describe(gr))
	}
	sort.Strings(out)
	return out
}

// DestructiveDeleteResources lists the resources whose deletion needs
// --allow-destructive. Pinned by the same test, for the same reason.
func DestructiveDeleteResources() []string {
	out := make([]string, 0, len(destructiveDeletes))
	for gr := range destructiveDeletes {
		out = append(out, describe(gr))
	}
	sort.Strings(out)
	return out
}

// DestructiveMutationResources lists the resources whose creation or change
// needs --allow-destructive. Pinned by the same test, for the same reason.
func DestructiveMutationResources() []string {
	out := make([]string, 0, len(destructiveMutations))
	for gr := range destructiveMutations {
		out = append(out, describe(gr))
	}
	sort.Strings(out)
	return out
}

// ------------------------------------------------------------------ apply

// ResourceApplyIn is resource_apply's input.
type ResourceApplyIn struct {
	Context string `json:"context,omitempty" jsonschema:"kubeconfig context to address; omit for the current one"`

	// Manifest is a Kubernetes object.
	Manifest map[string]any `json:"manifest" jsonschema:"the complete object to apply, including apiVersion, kind and metadata.name"`

	Namespace string `json:"namespace,omitempty" jsonschema:"namespace to apply into; defaults to the manifest's own, then default"`
	DryRun    bool   `json:"dry_run,omitempty" jsonschema:"validate the manifest against the API server's schema and admission chain without persisting it; this reports whether the apply would be accepted, not a diff of what would change"`
	Force     bool   `json:"force,omitempty" jsonschema:"take ownership of fields currently owned by another field manager; without it a conflict is an error"`
}

// ResourceApplyOut is resource_apply's result.
type ResourceApplyOut struct {
	Resource   string `json:"resource"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
	DryRun     bool   `json:"dry_run"`
	Generation int64  `json:"generation,omitempty" jsonschema:"the object's generation after the apply"`
	Note       string `json:"note,omitempty"`
}

// ResourceApplyHandler server-side applies a manifest.
func ResourceApplyHandler(conn *kube.Connector, policy WritePolicy) sdk.ToolHandlerFor[ResourceApplyIn, ResourceApplyOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ResourceApplyIn) (*sdk.CallToolResult, ResourceApplyOut, error) {
		obj := &unstructured.Unstructured{Object: in.Manifest}

		apiVersion, kind := obj.GetAPIVersion(), obj.GetKind()
		if apiVersion == "" || kind == "" {
			return nil, ResourceApplyOut{}, fmt.Errorf("manifest needs apiVersion and kind")
		}
		if obj.GetName() == "" {
			return nil, ResourceApplyOut{}, fmt.Errorf("manifest needs metadata.name " +
				"(server-side apply cannot use generateName)")
		}

		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, ResourceApplyOut{}, err
		}

		// Resolve from the manifest's own apiVersion and kind through the
		// RESTMapper, so the resource is whatever this cluster serves for that GVK —
		// including a CRD's.
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			return nil, ResourceApplyOut{}, fmt.Errorf("manifest apiVersion %q: %w", apiVersion, err)
		}
		mapping, err := cl.Mapper.RESTMappingWithContext(ctx, gv.WithKind(kind).GroupKind(), gv.Version)
		if err != nil {
			return nil, ResourceApplyOut{}, fmt.Errorf("the cluster does not serve %s %s: %w", apiVersion, kind, err)
		}
		gvr := mapping.Resource
		namespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace
		if err := policy.check(gvr, false); err != nil {
			return nil, ResourceApplyOut{}, err
		}
		if err := checkPodTemplate(in.Manifest); err != nil {
			return nil, ResourceApplyOut{}, err
		}
		if err := checkConversionWebhook(gvr, in.Manifest); err != nil {
			return nil, ResourceApplyOut{}, err
		}

		// A manifest that names its own namespace and a namespace argument that
		// disagrees is refused rather than silently resolved. kubectl refuses the
		// same combination, and for the same reason: quietly relocating an object
		// the caller explicitly placed is how a workload ends up running somewhere
		// nobody intended.
		ns := in.Namespace
		if mns := obj.GetNamespace(); mns != "" && ns != "" && mns != ns {
			return nil, ResourceApplyOut{}, fmt.Errorf(
				"the manifest says namespace %q but the namespace argument says %q: "+
					"pass one or the other, not two that disagree", mns, ns)
		}
		if ns == "" {
			ns = obj.GetNamespace()
		}
		if namespaced && ns == "" {
			ns = metav1.NamespaceDefault
		}
		if namespaced {
			obj.SetNamespace(ns)
		}
		if err := checkEngineConfigMap(gvr, ns, obj.GetName()); err != nil {
			return nil, ResourceApplyOut{}, err
		}
		if err := checkLocalPathNamespace(namespaced, ns); err != nil {
			return nil, ResourceApplyOut{}, err
		}

		opts := metav1.ApplyOptions{FieldManager: FieldManager, Force: in.Force}
		if in.DryRun {
			opts.DryRun = []string{metav1.DryRunAll}
		}

		ri := cl.Dynamic.Resource(gvr)
		var applier dynamic.ResourceInterface = ri
		if namespaced {
			applier = ri.Namespace(ns)
		}

		if err := checkClaimedVolumes(ctx, cl.Typed, in.Manifest, ns); err != nil {
			return nil, ResourceApplyOut{}, err
		}

		// SSA merges per field manager, so the object that results is not
		// the manifest: a field this manifest omits stays as another
		// manager left it. The guards above therefore have to run against
		// the object already there as well, or a caller can change a
		// container's command while a Secret volume it never mentioned is
		// retained -- and then read it with pod_logs.
		var got *unstructured.Unstructured
		for attempt := 1; ; attempt++ {
			existing, err := applier.Get(ctx, obj.GetName(), metav1.GetOptions{})
			switch {
			case apierrors.IsNotFound(err):
				existing = nil
			case err != nil:
				return nil, ResourceApplyOut{}, fmt.Errorf("read the existing object before apply: %w", err)
			default:
				if err := checkConversionWebhook(gvr, existing.Object); err != nil {
					return nil, ResourceApplyOut{}, err
				}
				if err := checkPodTemplate(existing.Object); err != nil {
					return nil, ResourceApplyOut{}, err
				}
				if err := checkClaimedVolumes(ctx, cl.Typed, existing.Object, ns); err != nil {
					return nil, ResourceApplyOut{}, err
				}
			}
			preview := opts
			preview.DryRun = []string{metav1.DryRunAll}
			got, err = applier.Apply(ctx, obj.GetName(), obj, preview)
			if err != nil {
				return nil, ResourceApplyOut{}, applyError(err, in.Force)
			}
			if err := checkApplyResult(ctx, cl.Typed, gvr, got.Object, ns); err != nil {
				return nil, ResourceApplyOut{}, err
			}
			if in.DryRun {
				break
			}
			afterApplyPreview()
			bound := obj
			if existing != nil {
				bound = obj.DeepCopy()
				bound.SetResourceVersion(got.GetResourceVersion())
			}
			got, err = applier.Apply(ctx, obj.GetName(), bound, opts)
			if apierrors.IsConflict(err) && !apierrors.HasStatusCause(err, metav1.CauseTypeFieldManagerConflict) && attempt < applyAttempts {
				continue
			}
			if err != nil {
				return nil, ResourceApplyOut{}, applyError(err, in.Force)
			}
			if err := checkApplyResult(ctx, cl.Typed, gvr, got.Object, ns); err != nil {
				return nil, ResourceApplyOut{}, fmt.Errorf("%s %s/%s WAS STORED, but the stored object, or storage "+
					"it references, no longer passes the checks its dry run passed. Review or delete it now: %w",
					gvr.Resource, got.GetNamespace(), got.GetName(), err)
			}
			break
		}

		out := ResourceApplyOut{
			Resource:   gvr.String(),
			Name:       got.GetName(),
			Namespace:  got.GetNamespace(),
			DryRun:     in.DryRun,
			Generation: got.GetGeneration(),
		}
		if in.DryRun {
			out.Note = "Dry run: the API server accepted this manifest against the resource's schema " +
				"and the admission chain, and nothing was persisted. This does not say what would " +
				"change — read the object first if you need to know that."
		}
		return nil, out, nil
	}
}

// checkApplyResult runs the manifest guards on the object an apply would
// produce, and says so in the refusal.
func checkApplyResult(ctx context.Context, cs kubernetes.Interface, gvr schema.GroupVersionResource, obj map[string]any, ns string) error {
	for _, check := range []func() error{
		func() error { return checkConversionWebhook(gvr, obj) },
		func() error { return checkPodTemplate(obj) },
		func() error { return checkClaimedVolumes(ctx, cs, obj, ns) },
	} {
		if err := check(); err != nil {
			return fmt.Errorf("the object the API server would store for this apply, after merging it "+
				"with the existing object and applying defaults and admission: %w", err)
		}
	}
	return nil
}

// afterApplyPreview runs between resource_apply's checked dry run and its
// real apply, indirected for tests.
var afterApplyPreview = func() {}

// SetAfterApplyPreviewForTest makes resource_apply call f between its checked
// dry run and its real apply, so a test can change the object in that window.
// It returns a func that restores the previous hook.
func SetAfterApplyPreviewForTest(f func()) (restore func()) {
	prev := afterApplyPreview
	afterApplyPreview = f
	return func() { afterApplyPreview = prev }
}

// applyAttempts bounds how often resource_apply re-checks and retries when the
// object changes between its dry run and the real apply.
const applyAttempts = 3

// applyError explains a field-ownership conflict, which is the failure a
// caller is most likely to hit and least likely to understand.
func applyError(err error, forced bool) error {
	if forced || !apierrors.HasStatusCause(err, metav1.CauseTypeFieldManagerConflict) {
		return err
	}
	return fmt.Errorf("%w\n\nAnother field manager owns these fields. "+
		"Pass force=true to take ownership, but only if this server should be the thing that "+
		"manages them — forcing a field away from a controller means the controller will set it back", err)
}

// ----------------------------------------------------------------- delete

// ResourceDeleteIn is resource_delete's input.
type ResourceDeleteIn struct {
	ObjectRef
	Resource string `json:"resource" jsonschema:"resource to delete: a plural name (pods), singular (pod), short name (po), Kind (Pod), or group-qualified (deployments.apps)"`
	DryRun   bool   `json:"dry_run,omitempty" jsonschema:"report what would be deleted without deleting it"`
}

// ResourceDeleteOut is resource_delete's result.
type ResourceDeleteOut struct {
	Resource  string `json:"resource"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	DryRun    bool   `json:"dry_run"`
	Note      string `json:"note,omitempty"`
}

// ResourceDeleteHandler deletes one object.
func ResourceDeleteHandler(conn *kube.Connector, policy WritePolicy) sdk.ToolHandlerFor[ResourceDeleteIn, ResourceDeleteOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ResourceDeleteIn) (*sdk.CallToolResult, ResourceDeleteOut, error) {
		if in.Name == "" {
			return nil, ResourceDeleteOut{}, fmt.Errorf("name is required: this tool deletes one named object, " +
				"and has no way to express a bulk delete on purpose")
		}
		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, ResourceDeleteOut{}, err
		}
		gvr, namespaced, err := cl.ResolveResource(ctx, in.Resource)
		if err != nil {
			return nil, ResourceDeleteOut{}, err
		}
		if err := policy.check(gvr, true); err != nil {
			return nil, ResourceDeleteOut{}, err
		}

		// Only namespaced targets get a namespace, in the request and in the result.
		ns := ""
		if namespaced {
			ns = cmp.Or(in.Namespace, metav1.NamespaceDefault)
		}
		if err := checkEngineConfigMap(gvr, ns, in.Name); err != nil {
			return nil, ResourceDeleteOut{}, err
		}
		if err := checkLocalPathNamespace(namespaced, ns); err != nil {
			return nil, ResourceDeleteOut{}, err
		}

		opts := metav1.DeleteOptions{}
		if in.DryRun {
			opts.DryRun = []string{metav1.DryRunAll}
		}

		ri := cl.Dynamic.Resource(gvr)
		var deleter dynamic.ResourceInterface = ri
		if namespaced {
			deleter = ri.Namespace(ns)
		}
		if err := deleter.Delete(ctx, in.Name, opts); err != nil {
			return nil, ResourceDeleteOut{}, err
		}

		out := ResourceDeleteOut{Resource: gvr.String(), Name: in.Name, Namespace: ns, DryRun: in.DryRun}
		if in.DryRun {
			out.Note = "Dry run: nothing was deleted."
		}
		return nil, out, nil
	}
}

// ------------------------------------------------------------------ scale

// ResourceScaleIn is resource_scale's input.
type ResourceScaleIn struct {
	ObjectRef
	Resource string `json:"resource" jsonschema:"workload to scale: deployments, statefulsets or replicasets, or anything else serving a scale subresource"`
	Replicas int32  `json:"replicas" jsonschema:"the desired replica count; zero scales the workload down to nothing"`
}

// ResourceScaleOut is resource_scale's result.
type ResourceScaleOut struct {
	Resource  string `json:"resource"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Replicas  int32  `json:"replicas" jsonschema:"the replica count now recorded"`
	Note      string `json:"note,omitempty"`
}

// ResourceScaleHandler sets a workload's replica count.
func ResourceScaleHandler(conn *kube.Connector, policy WritePolicy) sdk.ToolHandlerFor[ResourceScaleIn, ResourceScaleOut] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in ResourceScaleIn) (*sdk.CallToolResult, ResourceScaleOut, error) {
		if in.Name == "" {
			return nil, ResourceScaleOut{}, fmt.Errorf("name is required")
		}
		if in.Replicas < 0 {
			return nil, ResourceScaleOut{}, fmt.Errorf("replicas cannot be negative, got %d", in.Replicas)
		}
		cl, err := cluster(conn, in.Context)
		if err != nil {
			return nil, ResourceScaleOut{}, err
		}
		gvr, namespaced, err := cl.ResolveResource(ctx, in.Resource)
		if err != nil {
			return nil, ResourceScaleOut{}, err
		}
		if err := policy.check(gvr, false); err != nil {
			return nil, ResourceScaleOut{}, err
		}

		ns := ""
		if namespaced {
			ns = cmp.Or(in.Namespace, metav1.NamespaceDefault)
		}
		if err := checkLocalPathNamespace(namespaced, ns); err != nil {
			return nil, ResourceScaleOut{}, err
		}

		ri := cl.Dynamic.Resource(gvr)
		var patcher dynamic.ResourceInterface = ri
		if namespaced {
			patcher = ri.Namespace(ns)
		}

		patch := []byte(fmt.Sprintf(`{"spec":{"replicas":%d}}`, in.Replicas))
		got, err := patcher.Patch(ctx, in.Name, types.MergePatchType, patch, metav1.PatchOptions{
			FieldManager: FieldManager,
		}, "scale")
		if err != nil {
			return nil, ResourceScaleOut{}, scaleError(err, in.Resource)
		}

		out := ResourceScaleOut{
			Resource:  gvr.String(),
			Name:      in.Name,
			Namespace: ns,
			Replicas:  in.Replicas,
		}
		// Replicas starts as what was asked for.
		replicas, found, _ := unstructured.NestedInt64(got.Object, "spec", "replicas")
		switch {
		case found:
			out.Replicas = int32(replicas)
		default:
			// No spec.replicas came back, so Replicas is still the
			// request under a schema that says otherwise.
			out.Note = "The scale was accepted, but the replica count could not be read back from the " +
				"response; the count reported here is the one requested, not one the server confirmed."
		}
		if in.Replicas == 0 {
			// Appended, not assigned: the caveat above matters more.
			out.Note = strings.TrimSpace(out.Note +
				" Scaled to zero: the workload still exists but is running nothing.")
		}
		return nil, out, nil
	}
}

// scaleError says the useful thing when a resource has no scale
// subresource, which the API server reports only as a 404 on a path the
// caller never typed.
func scaleError(err error, what string) error {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || !apierrors.IsNotFound(err) {
		return err
	}
	if d := status.Status().Details; d != nil && d.Name != "" {
		return err // the object is missing; the error already says so clearly
	}
	return fmt.Errorf("%w\n\n%s may not support scaling: only workloads serving a /scale "+
		"subresource (deployments, statefulsets, replicasets and some custom resources) can be scaled",
		err, what)
}

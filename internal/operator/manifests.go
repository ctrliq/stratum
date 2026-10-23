// manifests.go — auto-deploy plain Kubernetes YAML from a host directory.
//
// reconcileManifests server-side-applies every *.yaml/*.yml file under a
// directory (default /etc/stratum/manifests) so operators can drop manifests on
// the control-plane node and have them applied without a kubectl round-trip —
// the manifest equivalent of the HelmChart controller. This mirrors the SSA
// applier in stratum/internal/addons/apply.go: a discovery-backed RESTMapper and
// a per-object ApplyPatch with FieldManager "stratum-operator", Force.
//
// Stacks and pruning: a first-level subdirectory of the manifests dir is a STACK. Every
// object applied from a stack is labeled stratum.ciq.com/stack=<name> and
// recorded as that stack's object references; objects the record remembers but
// no file of a fully successful pass declares are DELETED on that pass. The
// keep-set is the whole pass's union, matched by canonical identity (the
// RESTMapper's GroupResource, the effective namespace and the name), so an
// object a release moved between stacks, re-versioned or made
// namespace-explicit survives the pass that applies it under its new shape.
// Loose files directly in the manifests dir keep the old apply-only contract —
// their objects join the keep-set but are never recorded, so removing a loose
// file deletes nothing — the operator drop-a-file-and-go path stays safe.
//
// The record pruning is driven by is the prune inventory in cluster state (see
// inventory.go): a stratum-addon-inventory ConfigMap in kube-system with one
// entry per operator instance, whose union every instance of the cluster
// published from its own manifests directory. Until the migration to that
// inventory has closed — every control-plane node contributed an entry, or an
// explicit recovery entry closed it — pruning suspends, and a pass with no
// inventory ConfigMap at all still prunes against the per-node
// <dir>/.applied.json earlier releases wrote.
//
// To avoid hot-looping the apiserver, a file is re-applied only when its bytes
// change OR when the resync window (5m) has elapsed since its last apply.
//
// Leader-only note: this applies the LEADER's local directory. With leader
// election on and multiple control planes, put identical files on every CP (or
// just use kubectl) — a follower never applies its copy. The bundled stacks are
// identical by construction: anything cluster-specific in them is a sentinel
// filled in here, from cluster state every leader sees the same — Cilium's
// apiserver endpoint, and the four cluster inputs the staging records in the
// inventory's cluster-inputs document (the DNS address, the cluster domain,
// dual-stack and the storage default, resolved through manifestSentinels). A
// sentinel the cluster state cannot resolve skips its file rather than guess:
// the file keeps no apply state and retries next pass. The prune inventory is
// the one piece of pass state that outlives a leader change: the entries live
// in the cluster, not in the leading node's directory.
package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
)

// manifestResync forces a periodic re-apply even when a file's bytes are
// unchanged, so drift (someone editing the live object) is corrected.
const manifestResync = 5 * time.Minute

// CoreDNSStack is the stack the bundled CoreDNS deploys under: the name of the
// shipped coredns bundle's first-level directory, which stackOf turns into the
// stratum.ciq.com/stack label stamped on every object the deployer applies
// from it. The CoreDNS spreader gates on the same constant, so the deployer
// that stamps the name and the spreader that consumes it cannot drift apart.
const CoreDNSStack = "coredns"

// appliedRecordName is the per-node prune record a pre-migration release wrote
// into the manifests directory, and the file syncInventory retires once the
// migration to the cluster inventory has closed.
const appliedRecordName = ".applied.json"

// inventoryHostname follows the short hostname used to register the Node.
var inventoryHostname = host.Hostname

// manifestDeployer applies and prunes the YAML stacks of one manifests
// directory. One value per operator process, built by Run and driven by the
// leader loop's deployer step: the dynamic and discovery clients are stable
// for the process lifetime, the RESTMapper is rebuilt on demand when an apply
// hits a no-match error (a freshly-installed CRD the mapper predates), and the
// per-file apply state lives only for this process — an operator restart
// starts with empty state and so re-applies every file once. The typed
// clientset reads and writes the prune inventory and the cluster-inputs
// records in cluster state, and node is the name this instance publishes its
// inventory entry under.
type manifestDeployer struct {
	dyn   dynamic.Interface
	disco discovery.DiscoveryInterface
	// mapper resolves an object's GVK to a resource; rebuildMapper replaces
	// it from live discovery.
	mapper meta.RESTMapper
	// dir is the manifests directory this deployer applies and prunes.
	dir string
	// state is the per-file apply state, keyed by absolute path.
	state map[string]manifestFileState
	// now is the clock the resync window is measured against.
	now func() time.Time
	// cs reads and writes the prune inventory and cluster-inputs records in
	// the inventory ConfigMap.
	cs kubernetes.Interface
	// node is the name this instance's inventory entry is published under.
	node string
	// yielded reports that the file being applied served the dashboard on a
	// ClusterIP because another Service held its NodePort; that file's state
	// is then dropped, not recorded, so the next pass applies it again, even
	// as an earlier rendering whose sum is still cached, and restores the
	// NodePort as soon as the port is free. lastYield is the last
	// message logged for that, so a held port is logged once, not per pass.
	yielded   bool
	lastYield string
}

type manifestFileState struct {
	sum       [sha256.Size]byte
	lastApply time.Time
}

// newManifestDeployer builds the deployer Run drives. The clients come up
// front — their construction reads only cfg, like the operator's other
// clients — while the RESTMapper waits for the first pass that needs it:
// resolving it asks discovery, and the apiserver may not answer yet when the
// operator starts. The node name is the instance's inventory identity, the
// short hostname the kubelet registers as its Node name.
func newManifestDeployer(cfg *rest.Config, dir string) (*manifestDeployer, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	node, err := inventoryHostname()
	if err != nil {
		return nil, fmt.Errorf("resolve inventory node identity: %w", err)
	}
	return &manifestDeployer{
		dyn:   dyn,
		disco: disco,
		dir:   dir,
		state: map[string]manifestFileState{},
		now:   time.Now,
		cs:    cs,
		node:  node,
	}, nil
}

// ensureMapper resolves the RESTMapper from discovery the first time a pass
// needs it.
func (d *manifestDeployer) ensureMapper() error {
	if d.mapper != nil {
		return nil
	}
	return d.rebuildMapper()
}

// rebuildMapper refreshes the RESTMapper from live discovery so a
// just-installed CRD becomes resolvable.
func (d *manifestDeployer) rebuildMapper() error {
	groups, err := restmapper.GetAPIGroupResources(d.disco)
	if err != nil {
		return err
	}
	d.mapper = restmapper.NewDiscoveryRESTMapper(groups)
	return nil
}

// reconcileManifests applies every YAML file under the deployer's directory.
// A missing directory is not an error (the feature is opt-in). Per-file errors
// are logged and skipped so one bad manifest doesn't block the rest.
func (d *manifestDeployer) reconcileManifests(ctx context.Context, vip, selfIP string) {
	if d.dir == "" {
		return
	}
	if _, err := os.Stat(d.dir); err != nil {
		return // no manifests dir -> nothing to do (silent, it's optional)
	}
	// A leader must obey the same local migration gate as the background
	// publisher: publishing its current objects cannot acknowledge a legacy
	// inventory that nobody has successfully read, or overwrite its evidence.
	if _, err := readMigrationRecord(filepath.Join(d.dir, appliedRecordName)); err != nil && !os.IsNotExist(err) {
		inv, ierr := loadInventory(ctx, d.cs)
		if ierr != nil || !inv.Recovery {
			log.Printf("manifests: unreadable local migration inventory: %v (explicit recovery required)", err)
			return
		}
	}

	files, err := manifestFiles(d.dir)
	if err != nil {
		log.Printf("manifests: glob %s: %v", d.dir, err)
		return
	}

	// The desired bundle generation: the staging path records what it rendered
	// in the cluster inventory, and this pass applies only a staged tree whose
	// digest matches that record — applying what nobody recorded would make
	// this directory, not the apiserver record, the source of truth. A cluster
	// with no inventory ConfigMap, or one whose inventory holds no desired
	// record yet — the recording `stratum up` has not reached it — has no
	// intent to check against and applies as today, so an operator upgraded
	// ahead of its first recording staging keeps deploying.
	desired, derr := d.desiredRecord(ctx)
	if derr != nil {
		log.Printf("manifests: desired generation: %v (not applied this pass)", derr)
		return
	}
	if desired != nil && !d.stagedTreeMatches(desired, files) {
		return
	}
	if len(files) == 0 {
		// The early return an empty directory has always made — unless the
		// digest check above matched a desired record, which makes this empty
		// tree the explicitly staged desired-empty generation: the pass then
		// prunes the objects the inventory still remembers.
		if desired == nil {
			return
		}
		if err := d.ensureMapper(); err != nil {
			log.Printf("manifests: client init: %v", err)
			return
		}
		d.pruneStacks(ctx, map[string][]objRef{}, nil)
		return
	}

	if err := d.ensureMapper(); err != nil {
		log.Printf("manifests: client init: %v", err)
		return
	}

	now := d.now()
	current := map[string][]objRef{} // stack -> refs present in files this pass
	var loose []objRef               // refs present in loose files this pass
	pruneReady := true
	api := lazyAPIServer(ctx, d, vip, selfIP)
	inputs := lazyClusterInputs(ctx, d)
	if desired != nil && desired.Inputs != nil {
		inputs = func() (*ClusterInputs, error) { return desired.Inputs, nil }
	}
	for _, path := range files {
		stack := stackOf(d.dir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("manifests: read %s: %v", path, err)
			pruneReady = false
			continue
		}
		if addons.NeedsAPIServer(data) {
			a, err := api()
			if err != nil {
				// Skip rather than guess: applying a different address than the
				// last pass would roll every agent on an API hiccup.
				log.Printf("manifests: %s: resolve the apiserver for Cilium: %v (not applied this pass)", path, err)
				pruneReady = false
				continue
			}
			data = addons.FillAPIServer(data, a)
		}
		if needsClusterInputs(data) {
			in, err := inputs()
			if err == nil {
				data, err = fillClusterInputs(data, in)
			}
			if err != nil {
				// Skip rather than guess: a half-filled or wrongly-filled file
				// would apply the wrong cluster inputs, and a skipped file
				// retries next pass once the staging has recorded them.
				log.Printf("manifests: %s: resolve the cluster inputs: %v (not applied this pass)", path, err)
				pruneReady = false // the pass has not seen this file's intent
				continue
			}
		}
		// The sum covers what is applied, not the file: a control plane joining
		// changes the rendered manifest while the file on disk stays the same.
		sum := sha256.Sum256(data)
		prev, seen := d.state[path]
		unchanged := seen && prev.sum == sum && now.Sub(prev.lastApply) < manifestResync
		d.yielded = false
		refs, n, err := d.applyManifestFile(ctx, data, stack, !unchanged)
		if err != nil {
			log.Printf("manifests: apply %s: %v", path, err)
			pruneReady = false // never prune on a pass that failed to read intent
			continue
		}
		if stack != "" && n == 0 {
			// An empty stack file is as likely caught mid-write (`helm template >
			// file`) as emptied on purpose, and pruning on it would delete the
			// whole stack. Removing the file or the stack's directory deletes.
			log.Printf("manifests: %s declares no objects; pruning waits until it does or is removed", path)
			pruneReady = false
		}
		if stack != "" {
			current[stack] = append(current[stack], refs...)
		} else {
			loose = append(loose, refs...)
		}
		switch {
		case d.yielded:
			delete(d.state, path)
		case !unchanged:
			d.state[path] = manifestFileState{sum: sum, lastApply: now}
			log.Printf("manifests: applied %s (%d docs)", path, n)
		}
	}
	if pruneReady {
		d.pruneStacks(ctx, current, loose)
	}
}

// desiredRecord reads the desired bundle generation the staging path recorded
// in the cluster inventory. No inventory ConfigMap at all, or one without a
// desired record, is no record: the caller applies as today. Every other read
// error suspends the whole pass: unreadable state cannot authorize applying
// an unverified local generation.
func (d *manifestDeployer) desiredRecord(ctx context.Context) (*desiredGeneration, error) {
	inv, err := loadInventory(ctx, d.cs)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return inv.Desired, nil
}

// stagedTreeMatches digests the staged tree the way the staging path recorded
// it: the stack files under the manifests directory, keyed by their paths
// relative to it, digested with the one DigestBundles — but only under the
// stacks the record names. A loose file takes no part in the check: the
// staging never rendered one, and the loose drop-a-file-and-go contract is
// apply-only, so a manifest dropped beside the bundles can never refuse a
// pass. A stack directory the record does not name takes no part either: it is
// a hand-dropped one, the staging's drop-a-file contract deliberately never
// deletes it, so no re-stage can ever cover it — digesting it would refuse
// every pass until a person removed the file, stopping the bundled add-ons
// applying and pruning cluster-wide. Like a loose file it joins the pass
// below, never the digest. The staging owns the namespace of the stacks it
// does name — it rewrites the bundles on change and removes the ones it no
// longer renders (run.go's removeStaleBundledFiles and
// removeStaleImporterFiles), so `stratum up` repairs a mismatch inside them. A
// mismatch refuses the pass: the deployer applies and prunes nothing, leaves
// the record untouched, and logs why, naming the record it refused against and
// a fix that works. A stack file that cannot be read is a refusal too: the
// tree cannot be verified, so nothing it holds is applied this pass.
func (d *manifestDeployer) stagedTreeMatches(desired *desiredGeneration, files []string) bool {
	recorded := make(map[string]bool, len(desired.Stacks))
	for _, stack := range desired.Stacks {
		recorded[stack] = true
	}
	staged := make(map[string][]byte, len(files))
	for _, path := range files {
		stack := stackOf(d.dir, path)
		if stack == "" {
			continue // a loose file is not staged intent
		}
		if !recorded[stack] {
			continue // a hand-dropped stack directory is not staged intent
		}
		rel, err := filepath.Rel(d.dir, path)
		if err != nil {
			log.Printf("manifests: staged tree digest: %v (not applied this pass)", err)
			return false
		}
		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("manifests: staged tree digest: read %s: %v (not applied this pass)", path, err)
			return false
		}
		staged[rel] = data
	}
	digest := DigestBundles(staged)
	if digest == desired.Digest {
		return true
	}
	log.Printf("manifests: staged tree digest %s does not match the recorded desired generation %d (digest %s, staged by %q, stacks %v); applying nothing this pass — run `stratum up` on this node, which re-stages the rendered bundles and records a generation this tree matches",
		digest, desired.Generation, desired.Digest, desired.Provenance, desired.Stacks)
	return false
}

// stackOf maps <dir>/<stack>/file.yaml to "stack"; a loose <dir>/file.yaml is
// stackless ("") and never pruned.
func stackOf(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 {
		return ""
	}
	return parts[0]
}

// manifestFiles returns every *.yaml/*.yml under dir: loose files (apply-only)
// plus one level of stack subdirectories (applied AND pruned). Sorted for a
// deterministic apply order.
func manifestFiles(dir string) ([]string, error) {
	var files []string
	for _, pat := range []string{"*.yaml", "*.yml", "*/*.yaml", "*/*.yml"} {
		matches, err := filepath.Glob(filepath.Join(dir, pat))
		if err != nil {
			return nil, err
		}
		files = append(files, matches...)
	}
	sort.Strings(files)
	return files, nil
}

// applyManifestFile server-side-applies every document in a (multi-doc) YAML
// and returns the number of documents applied. It decodes and (when
// apply=true) server-side-applies every document, returning the object refs it
// found either way — the refs feed the whole-pass keep-set even on unchanged
// files, whatever stack the file belongs to (a loose file's refs keep the
// pass's objects alive without ever being recorded as prune-owned).
func (d *manifestDeployer) applyManifestFile(ctx context.Context, data []byte, stack string, apply bool) ([]objRef, int, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var refs []objRef
	count := 0
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if err == io.EOF {
				return refs, count, nil
			}
			return refs, count, err
		}
		if len(obj.Object) == 0 {
			continue // empty doc (leading ---, comments)
		}
		if stack != "" {
			labels := obj.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels[group+"/stack"] = stack
			obj.SetLabels(labels)
		}
		refs = append(refs, refOf(obj))
		if apply {
			if err := d.recordBundledOwnership(ctx, stack, refOf(obj)); err != nil {
				return refs, count, err
			}
			if d.cs != nil {
				d.yieldDashboardNodePort(ctx, obj)
			}
			if err := d.applyManifestObject(ctx, obj); err != nil {
				return refs, count, err
			}
		}
		count++
	}
}

// applyManifestObject applies a single object, rebuilding the RESTMapper once
// and retrying if the GVK doesn't yet map to a resource (new CRD case).
func (d *manifestDeployer) applyManifestObject(ctx context.Context, obj *unstructured.Unstructured) error {
	ri, err := d.manifestResourceFor(obj)
	if err != nil {
		if !meta.IsNoMatchError(err) {
			return err
		}
		// GVK unknown to the mapper — likely a CRD installed after we cached it.
		// Rebuild once and retry; a persistent no-match then surfaces as an error.
		if rerr := d.rebuildMapper(); rerr != nil {
			return rerr
		}
		ri, err = d.manifestResourceFor(obj)
		if err != nil {
			return err
		}
	}
	payload, err := obj.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, payload, metav1.PatchOptions{
		FieldManager: fieldManager,
		Force:        &force,
	})
	return err
}

// yieldDashboardNodePort applies yieldDashboardNodePort to obj before it is
// applied, records a yield for the pass, and logs a held port once and its
// release once.
func (d *manifestDeployer) yieldDashboardNodePort(ctx context.Context, obj *unstructured.Unstructured) {
	msg, err := yieldDashboardNodePort(ctx, d.cs, obj)
	switch {
	case err != nil:
		log.Printf("manifests: check the dashboard NodePort: %v (applying it as rendered)", err)
	case msg != "":
		d.yielded = true
		if msg != d.lastYield {
			log.Printf("dashboard: %s", msg)
			d.lastYield = msg
		}
	case d.lastYield != "" && isDashboardService(obj):
		log.Printf("dashboard: its NodePort is free again; serving it on the NodePort")
		d.lastYield = ""
	}
}

// manifestResourceFor resolves an object's GVK to a namespaced or cluster-scoped
// dynamic resource client.
func (d *manifestDeployer) manifestResourceFor(obj *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := obj.GroupVersionKind()
	mapping, err := d.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		return d.dyn.Resource(mapping.Resource).Namespace(ns), nil
	}
	return d.dyn.Resource(mapping.Resource), nil
}

// objRef identifies one applied object for the prune index.
type objRef struct {
	GVK       string `json:"gvk"` // group/version,Kind
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

func refOf(obj *unstructured.Unstructured) objRef {
	return objRef{
		GVK:       obj.GetAPIVersion() + "," + obj.GetKind(),
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
}

// objIdentity is an object's canonical identity: the GroupResource the
// RESTMapper resolves its GVK to — a GVK's version string is not identity,
// because two versions of a kind are the same resource — plus the object's
// effective namespace (the default one where the manifest left it implicit,
// empty for cluster-scoped objects) and its name.
type objIdentity struct {
	gr        schema.GroupResource
	namespace string
	name      string
}

// resolveRef maps a record ref to its canonical identity and to the resource
// its GVK is served as this pass. An error means the ref cannot be resolved —
// a malformed ref, or a GVK nothing serves anymore — and the caller must not
// guess: it can neither match the ref against the keep-set nor build a delete
// for it.
func (d *manifestDeployer) resolveRef(r objRef) (objIdentity, schema.GroupVersionResource, error) {
	apiVersion, kind, ok := strings.Cut(r.GVK, ",")
	if !ok {
		return objIdentity{}, schema.GroupVersionResource{}, fmt.Errorf("malformed ref %q", r.GVK)
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return objIdentity{}, schema.GroupVersionResource{}, err
	}
	mapping, err := d.mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
	if err != nil {
		return objIdentity{}, schema.GroupVersionResource{}, err
	}
	ns := ""
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns = r.Namespace
		if ns == "" {
			ns = "default"
		}
	}
	return objIdentity{
		gr:        mapping.Resource.GroupResource(),
		namespace: ns,
		name:      r.Name,
	}, mapping.Resource, nil
}

// readPruneRecord reads the local prune record <dir>/.applied.json. A missing
// or unreadable record is an empty one: the caller contributes what it can and
// never fails the pass over a corrupt file — a truncated record must not
// recover the entries it half-holds, or pruning would delete objects nobody
// declared this pass.
func readPruneRecord(dir string) map[string][]objRef {
	path := filepath.Join(dir, appliedRecordName)
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string][]objRef{}
	}
	var rec map[string][]objRef
	if err := json.Unmarshal(b, &rec); err != nil {
		log.Printf("manifests: %s: unreadable, treating as empty: %v", path, err)
		return map[string][]objRef{}
	}
	if rec == nil {
		rec = map[string][]objRef{}
	}
	return rec
}

// keepSet resolves the pass's keep-set to canonical identities: everything the
// files declared this pass, stacks and loose files alike, whatever stack or
// apiVersion shape each object now carries.
func (d *manifestDeployer) keepSet(current map[string][]objRef, loose []objRef) map[objIdentity]bool {
	keep := map[objIdentity]bool{}
	for _, refs := range current {
		for _, r := range refs {
			if id, _, err := d.resolveRef(r); err == nil {
				keep[id] = true
			}
		}
	}
	for _, r := range loose {
		if id, _, err := d.resolveRef(r); err == nil {
			keep[id] = true
		}
	}
	return keep
}

// pruneStacks closes a complete successful pass: delete what no file now
// declares, then record the keep-set. The cluster inventory drives pruning;
// before migration opens, the legacy local record retains that responsibility.
// An unreadable cluster inventory suspends pruning.
func (d *manifestDeployer) pruneStacks(ctx context.Context, current map[string][]objRef, loose []objRef) {
	inv, err := loadInventory(ctx, d.cs)
	switch {
	case apierrors.IsNotFound(err):
		// No inventory yet: this node's record is all the cluster knows.
		d.pruneLocalRecord(ctx, current, loose)
	case err != nil:
		log.Printf("manifests: prune inventory: %v (pruning suspended this pass)", err)
	default:
		d.pruneClusterInventory(ctx, inv, current, loose)
	}
}

// pruneLocalRecord drives pruning from the per-node .applied.json record — the
// pre-migration contract. Objects the record remembers that the whole pass no
// longer declares are deleted, then the record is rewritten from the pass. The
// keep-set is the union of everything this pass declared — every stack's
// objects plus every loose file's — matched by canonical identity, so a
// release that moves an object between stacks, bumps its apiVersion or makes
// its namespace explicit survives the very pass that applies it under its new
// shape. Loose-file objects join the keep-set but never the record: loose
// files stay apply-only, so the record owns nothing on their behalf.
func (d *manifestDeployer) pruneLocalRecord(ctx context.Context, current map[string][]objRef, loose []objRef) {
	idxPath := filepath.Join(d.dir, appliedRecordName)
	prev := readPruneRecord(d.dir)
	if len(loose) > 0 {
		// Late migration can otherwise revive historical ownership after a
		// loose file is removed. Persist the handoff in the shared inventory
		// before relinquishing any local evidence, then obey its migration gate.
		var migrated pruneInventory
		if err := writeInventory(ctx, d.cs, func(inv pruneInventory) pruneInventory {
			inv.addNode(d.node, prev)
			inv.addNode(d.node, current)
			d.updateLooseOwnership(&inv, current, loose)
			migrated = inv
			return inv
		}); err != nil {
			log.Printf("manifests: persist loose-file ownership handoff: %v (local inventory retained; pruning suspended)", err)
			return
		}
		d.pruneClusterInventory(ctx, migrated, current, loose)
		return
	}
	keep := d.keepSet(current, loose)

	for stack, oldRefs := range prev {
		var retry []objRef
		for _, r := range oldRefs {
			id, gvr, err := d.resolveRef(r)
			if err == nil && keep[id] {
				continue // some file this pass still declares it, under some stack — or none
			}
			if err != nil {
				// Unresolvable this pass: keep it in the record and retry next
				// pass rather than guess at what to delete.
				log.Printf("manifests: prune %s %s/%s (stack %s): resolve: %v", r.GVK, r.Namespace, r.Name, stack, err)
				retry = append(retry, r)
				continue
			}
			if err := d.deleteRef(ctx, id, gvr); err != nil {
				log.Printf("manifests: prune %s %s/%s (stack %s): %v", r.GVK, r.Namespace, r.Name, stack, err)
				// Keep it in the record so the next pass retries the delete.
				retry = append(retry, r)
				continue
			}
			log.Printf("manifests: pruned %s %s/%s (removed from stack %s)", r.GVK, r.Namespace, r.Name, stack)
		}
		if len(retry) > 0 {
			current[stack] = append(current[stack], retry...)
		}
	}

	b, err := json.MarshalIndent(current, "", "  ")
	if err == nil {
		if werr := os.WriteFile(idxPath+".tmp", b, 0o644); werr == nil {
			_ = os.Rename(idxPath+".tmp", idxPath)
		}
	}
}

// pruneClusterInventory drives pruning from the cluster inventory once the
// migration has closed: every object reference the inventory's union remembers
// that the whole pass no longer declares is deleted, whichever node's entry
// holds it. A successfully pruned reference leaves every entry it was in — the
// object is gone, so no entry may still remember it — while one whose delete
// fails or that cannot be resolved stays recorded, so a later pass retries it.
// This node's entry is rewritten to the pass's keep-set: what the files declare
// now, under the stacks that declare them. While the migration is open nothing
// is deleted — a control plane that has not contributed may hold the only
// record of objects this pass cannot see — but the pass still publishes its
// keep-set as this node's entry, so the migration can close.
func (d *manifestDeployer) pruneClusterInventory(ctx context.Context, inv pruneInventory, current map[string][]objRef, loose []objRef) {
	nodes, err := controlPlaneNodes(ctx, d.cs)
	if err != nil {
		log.Printf("manifests: prune inventory: list control-plane nodes: %v (pruning suspended this pass)", err)
		return
	}
	d.updateLooseOwnership(&inv, current, loose)
	if !inv.migrationComplete(nodes) {
		if perr := writeInventory(ctx, d.cs, func(fresh pruneInventory) pruneInventory {
			fresh.addNode(d.node, current)
			d.updateLooseOwnership(&fresh, current, loose)
			return fresh
		}); perr != nil {
			log.Printf("manifests: publish inventory: %v", perr)
		}
		log.Printf("manifests: pruning suspended until every control plane has published its inventory")
		return
	}

	keep := d.keepSet(current, loose)
	deleted := map[objIdentity]bool{}
	for _, node := range sortedStringKeys(inv.Nodes) {
		entry := inv.Nodes[node]
		for _, stack := range sortedStringKeys(entry) {
			refs := entry[stack]
			retained := make([]objRef, 0, len(refs))
			for _, r := range refs {
				id, gvr, err := d.resolveRef(r)
				switch {
				case err != nil:
					// Unresolvable this pass: keep it recorded and retry next
					// pass rather than guess at what to delete.
					log.Printf("manifests: prune %s %s/%s (node %s, stack %s): resolve: %v", r.GVK, r.Namespace, r.Name, node, stack, err)
					retained = append(retained, r)
				case keep[id]:
					retained = append(retained, r)
				case deleted[id]:
					// Another entry's reference to the same object was already
					// deleted this pass: the object is gone and this reference
					// leaves the entry with it.
				default:
					if derr := d.deleteRef(ctx, id, gvr); derr != nil {
						log.Printf("manifests: prune %s %s/%s (node %s, stack %s): %v", r.GVK, r.Namespace, r.Name, node, stack, derr)
						retained = append(retained, r)
						continue
					}
					deleted[id] = true
					log.Printf("manifests: pruned %s %s/%s (removed from node %s, stack %s)", r.GVK, r.Namespace, r.Name, node, stack)
				}
			}
			if len(retained) > 0 {
				entry[stack] = retained
			} else {
				delete(entry, stack)
			}
		}
	}

	// Write every entry minus the pruned references, with this node's entry
	// rewritten to the pass's keep-set. A conflict re-reads the ConfigMap and
	// re-applies the same removals and rewrite to the fresh state, so a
	// concurrent writer's entry survives instead of being clobbered.
	if err := writeInventory(ctx, d.cs, func(fresh pruneInventory) pruneInventory {
		d.updateLooseOwnership(&fresh, current, loose)
		for _, entry := range fresh.Nodes {
			for stack, refs := range entry {
				retained := make([]objRef, 0, len(refs))
				for _, r := range refs {
					if id, _, err := d.resolveRef(r); err == nil && deleted[id] {
						continue
					}
					retained = append(retained, r)
				}
				if len(retained) > 0 {
					entry[stack] = retained
				} else {
					delete(entry, stack)
				}
			}
		}
		own := make(map[string][]objRef, len(current))
		for stack, refs := range current {
			own[stack] = append([]objRef(nil), refs...)
		}
		// The background migration publisher may have contributed references
		// since this pass's snapshot and retired their only local copy. Keep
		// those fresh references, along with failed or unresolved deletions,
		// until a later pass can prune them. A kept object follows its current
		// declaring stack instead of retaining its former stack's ownership.
		for stack, refs := range fresh.Nodes[d.node] {
			for _, r := range refs {
				if id, _, err := d.resolveRef(r); err == nil && keep[id] {
					continue
				}
				own[stack] = appendUniqueRefs(own[stack], []objRef{r})
			}
		}
		fresh.Nodes[d.node] = own
		return fresh
	}); err != nil {
		log.Printf("manifests: write prune inventory: %v", err)
	}
}

// updateLooseOwnership remembers apply-only handoffs independently of node
// contributions. It runs against each fresh conflict-retry snapshot, so even a
// controller migrating an old local record after the loose file was removed
// cannot restore its former pruning authority. Only a current stack declaration
// reclaims that authority. Canonical identities also match different API versions
// and implicit default namespaces.
func (d *manifestDeployer) updateLooseOwnership(inv *pruneInventory, current map[string][]objRef, loose []objRef) {
	stackOwned := d.keepSet(current, nil)
	var unowned []inventoryObject
	released := map[objIdentity]bool{}
	remember := func(id objIdentity) {
		if !stackOwned[id] && !released[id] {
			released[id] = true
			unowned = append(unowned, inventoryObject{Resource: id.gr, Namespace: id.namespace, Name: id.name})
		}
	}
	for _, obj := range inv.Unowned {
		remember(objIdentity{gr: obj.Resource, namespace: obj.Namespace, name: obj.Name})
	}
	for _, r := range loose {
		if id, _, err := d.resolveRef(r); err == nil {
			remember(id)
		}
	}
	inv.Unowned = unowned
	for _, entry := range inv.Nodes {
		for stack, refs := range entry {
			var retained []objRef
			for _, r := range refs {
				if id, _, err := d.resolveRef(r); err == nil && released[id] {
					continue
				}
				retained = append(retained, r)
			}
			if len(retained) == 0 {
				delete(entry, stack)
			} else {
				entry[stack] = retained
			}
		}
	}
}

// deleteRef deletes the object a resolved ref stands for, at the effective
// namespace its identity carries — the one the pass applied it to.
func (d *manifestDeployer) deleteRef(ctx context.Context, id objIdentity, gvr schema.GroupVersionResource) error {
	ri := d.dyn.Resource(gvr).Namespace(id.namespace)
	err := ri.Delete(ctx, id.name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil // already gone — pruning is idempotent
	}
	return err
}

// clusterInputsDataKey holds the cluster-inputs document inside the inventory
// ConfigMap, beside the prune inventory's own data key.
const clusterInputsDataKey = "cluster-inputs"

// ClusterInputs is the cluster-inputs document the staging path records in the
// inventory ConfigMap: the four remaining per-control-plane inputs the bundled
// bundles carry as named sentinels instead of the staging node's local values.
// Every control plane resolves its sentinels from this one record, so the
// applied bytes do not depend on which node staged the bundles or which of
// them leads.
type ClusterInputs struct {
	// DNSAddress is the cluster DNS Service IP — the tenth address of the
	// service CIDR, netcfg.DNSIPFor — where the kube-dns Service lands.
	DNSAddress string `json:"dnsAddress"`
	// ClusterDomain is the cluster domain the Corefile serves.
	ClusterDomain string `json:"clusterDomain"`
	// DualStack says whether the cluster runs IPv4 and IPv6 pods.
	DualStack bool `json:"dualStack"`
	// StorageDefault names the cluster's default storage backend —
	// addons.StorageLocalPath when local-path holds the default claim,
	// anything else (StorageNone, an unset config) when it does not.
	StorageDefault string `json:"storageDefault"`
}

// The apply-time sentinel tokens the bundled bundles carry for the four
// remaining per-control-plane inputs, defined where the staging renders them
// (addons.DNSAddressSentinel and friends): the staging stages the tokens, this
// table resolves them from the cluster-inputs record at apply time. The set
// stays small on purpose — a resolver reads only what the record holds, and a
// token the record cannot resolve leaves its file unapplied rather than
// half-filled.
var manifestSentinels = []struct {
	token   string
	resolve func(ClusterInputs) (string, error)
}{
	{
		token: addons.DNSAddressSentinel,
		resolve: func(in ClusterInputs) (string, error) {
			if in.DNSAddress == "" {
				return "", fmt.Errorf("the cluster-inputs record holds no DNS address")
			}
			return in.DNSAddress, nil
		},
	},
	{
		token: addons.ClusterDomainSentinel,
		resolve: func(in ClusterInputs) (string, error) {
			if in.ClusterDomain == "" {
				return "", fmt.Errorf("the cluster-inputs record holds no cluster domain")
			}
			return in.ClusterDomain, nil
		},
	},
	{
		token: addons.EnableIPv6Sentinel,
		resolve: func(in ClusterInputs) (string, error) {
			return strconv.FormatBool(in.DualStack), nil
		},
	},
	{
		token: addons.StorageDefaultSentinel,
		resolve: func(in ClusterInputs) (string, error) {
			return strconv.FormatBool(in.StorageDefault == addons.StorageLocalPath), nil
		},
	},
}

// needsClusterInputs reports whether the bytes carry a cluster-input sentinel
// that fillClusterInputs must resolve before the file is applied.
func needsClusterInputs(data []byte) bool {
	for _, s := range manifestSentinels {
		if bytes.Contains(data, []byte(s.token)) {
			return true
		}
	}
	return false
}

// fillClusterInputs resolves every cluster-input sentinel the bytes carry from
// the record and replaces it with the resolved value. A token the record
// cannot resolve fails the whole fill with the token named: the caller skips
// the file rather than apply it half-filled — a sentinel left in the bytes
// would reach the apiserver literally.
func fillClusterInputs(data []byte, in *ClusterInputs) ([]byte, error) {
	for _, s := range manifestSentinels {
		if !bytes.Contains(data, []byte(s.token)) {
			continue
		}
		v, err := s.resolve(*in)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.token, err)
		}
		data = bytes.ReplaceAll(data, []byte(s.token), []byte(v))
	}
	return data, nil
}

// loadClusterInputs reads the cluster-inputs document the staging path
// recorded in the inventory ConfigMap. No ConfigMap at all, or one without the
// document, is no record; a document that cannot be parsed is an error like
// any other read failure. Both are the caller's resolution failure: a sentinel
// the cluster state cannot resolve skips its file, it never guesses.
func (d *manifestDeployer) loadClusterInputs(ctx context.Context) (*ClusterInputs, error) {
	cm, err := d.cs.CoreV1().ConfigMaps(inventoryNamespace).Get(ctx, inventoryConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	doc := cm.Data[clusterInputsDataKey]
	if doc == "" {
		return nil, fmt.Errorf("%s/%s holds no %s record", inventoryNamespace, inventoryConfigMapName, clusterInputsDataKey)
	}
	var in ClusterInputs
	if err := json.Unmarshal([]byte(doc), &in); err != nil {
		return nil, fmt.Errorf("%s/%s: %s: %w", inventoryNamespace, inventoryConfigMapName, clusterInputsDataKey, err)
	}
	return &in, nil
}

// lazyClusterInputs resolves the cluster-inputs record at most once per pass,
// and only if some file carries a sentinel that needs it.
func lazyClusterInputs(ctx context.Context, d *manifestDeployer) func() (*ClusterInputs, error) {
	var (
		done bool
		in   *ClusterInputs
		err  error
	)
	return func() (*ClusterInputs, error) {
		if !done {
			done = true
			in, err = d.loadClusterInputs(ctx)
		}
		return in, err
	}
}

// RecordClusterInputs records the cluster-inputs document a staging produced
// under its own data key in the inventory ConfigMap, beside the prune
// inventory and the desired generation: the values every control plane's
// deployer resolves the apply-time sentinels from. The last recording staging
// wins — the record is the cluster's intent, not one node's — and the write
// re-reads on a resourceVersion conflict like writeInventory, so a concurrent
// writer's entry and records survive. A missing ConfigMap is created.
func RecordClusterInputs(ctx context.Context, cs kubernetes.Interface, in ClusterInputs) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	configMaps := cs.CoreV1().ConfigMaps(inventoryNamespace)
	var lastErr error
	for attempt := 0; attempt < inventoryWriteAttempts; attempt++ {
		cm, err := configMaps.Get(ctx, inventoryConfigMapName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: inventoryNamespace, Name: inventoryConfigMapName},
				Data:       map[string]string{clusterInputsDataKey: string(b)},
			}
			if _, err := configMaps.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
				if apierrors.IsAlreadyExists(err) {
					lastErr = err
					continue // another writer created it first — re-read and retry
				}
				return err
			}
			return nil
		}
		if err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[clusterInputsDataKey] = string(b)
		if _, err := configMaps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue // lost a race — re-read and retry
			}
			return err
		}
		return nil
	}
	return lastErr
}

// lazyAPIServer resolves the Cilium apiserver settings at most once per pass,
// and only if some file needs them.
func lazyAPIServer(ctx context.Context, d *manifestDeployer, vip, selfIP string) func() (addons.APIServer, error) {
	var (
		done bool
		a    addons.APIServer
		err  error
	)
	return func() (addons.APIServer, error) {
		if !done {
			done = true
			a, err = d.resolveAPIServer(ctx, vip, selfIP)
		}
		return a, err
	}
}

func (d *manifestDeployer) resolveAPIServer(ctx context.Context, vip, selfIP string) (addons.APIServer, error) {
	var ips []string
	if vip == "" {
		nodes, err := d.dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}).
			List(ctx, metav1.ListOptions{LabelSelector: k8sutil.ControlPlaneLabel + "=true"})
		if err != nil {
			return addons.APIServer{}, fmt.Errorf("list control-plane nodes: %w", err)
		}
		// Every labelled control plane must report an address, or the pass is
		// skipped. Leaving one out would apply a URL list that omits a control
		// plane, and an empty list would fall back to this leader's own IP: either
		// way a transient Node status re-renders Cilium and rolls every agent.
		// The self-IP fallback is for a cluster with no
		// labelled Node at all (the first apply), never for a Node missing an address.
		var missing []string
		for _, n := range nodes.Items {
			if ip := nodeInternalIP(n.Object); ip != "" {
				ips = append(ips, ip)
			} else {
				missing = append(missing, n.GetName())
			}
		}
		if len(missing) > 0 {
			return addons.APIServer{}, fmt.Errorf("control-plane node(s) %s report no valid InternalIP", strings.Join(missing, ", "))
		}
	}
	a := addons.APIServerFor(vip, selfIP, ips)
	if a.Host == "" {
		return a, fmt.Errorf("no VIP, no labelled control plane and no --node-ip")
	}
	return a, nil
}

// nodeInternalIP is a Node's InternalIP, IPv4 preferred: the address its
// apiserver certificate carries and its etcd advertises.
func nodeInternalIP(node map[string]any) string {
	addrs, _, _ := unstructured.NestedSlice(node, "status", "addresses")
	var v6 string
	for _, a := range addrs {
		m, ok := a.(map[string]any)
		if !ok || m["type"] != "InternalIP" {
			continue
		}
		s, _ := m["address"].(string)
		ip := net.ParseIP(s)
		switch {
		case ip == nil:
		case ip.To4() != nil:
			return s
		case v6 == "":
			v6 = s
		}
	}
	return v6
}

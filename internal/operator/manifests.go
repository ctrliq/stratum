// Applies the leader's local directory; a follower never applies its copy.
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

// re-applies even unchanged files so live drift is corrected.
const manifestResync = 5 * time.Minute

// The CoreDNS spreader gates on the same constant, so the two cannot drift.
const CoreDNSStack = "coredns"

// legacy per-node prune record written by pre-migration releases.
const appliedRecordName = ".applied.json"

// must match the short hostname the kubelet registers as the Node name.
var inventoryHostname = host.Hostname

type manifestDeployer struct {
	dyn    dynamic.Interface
	disco  discovery.DiscoveryInterface
	mapper meta.RESTMapper
	dir    string
	state  map[string]manifestFileState
	now    func() time.Time
	cs     kubernetes.Interface
	node   string
	// a yielded file drops its apply state so the next pass reapplies and restores the NodePort.
	yielded   bool
	lastYield string
	// RuntimeClasses the cluster already has that are not Stratum's.
	foreignClasses map[string]bool
	foreignRefs    []objRef
}

type manifestFileState struct {
	sum       [sha256.Size]byte
	lastApply time.Time
}

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

func (d *manifestDeployer) ensureMapper() error {
	if d.mapper != nil {
		return nil
	}
	return d.rebuildMapper()
}

func (d *manifestDeployer) rebuildMapper() error {
	groups, err := restmapper.GetAPIGroupResources(d.disco)
	if err != nil {
		return err
	}
	d.mapper = restmapper.NewDiscoveryRESTMapper(groups)
	return nil
}

// a missing directory is not an error: the feature is opt-in.
func (d *manifestDeployer) reconcileManifests(ctx context.Context, vip, selfIP string) {
	if d.dir == "" {
		return
	}
	if _, err := os.Stat(d.dir); err != nil {
		return
	}
	// publishing current objects must not acknowledge a legacy inventory nobody has read.
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

	// applying an unrecorded tree would make this directory, not the record, the source of truth.
	desired, derr := d.desiredRecord(ctx)
	if derr != nil {
		log.Printf("manifests: desired generation: %v (not applied this pass)", derr)
		return
	}
	if desired != nil && !d.stagedTreeMatches(desired, files) {
		return
	}
	if len(files) == 0 {
		// an empty tree matching a desired record is the staged empty generation and still prunes.
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
	current := map[string][]objRef{}
	var loose []objRef
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
				// applying a different address than the last pass would roll every agent on an API hiccup.
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
				// a half-filled file would apply the wrong cluster inputs; a skipped file retries next pass.
				log.Printf("manifests: %s: resolve the cluster inputs: %v (not applied this pass)", path, err)
				pruneReady = false
				continue
			}
		}
		// the sum covers what is applied, not the file: a joining control plane re-renders it.
		sum := sha256.Sum256(data)
		prev, seen := d.state[path]
		unchanged := seen && prev.sum == sum && now.Sub(prev.lastApply) < manifestResync
		d.yielded = false
		d.foreignRefs = nil
		refs, n, err := d.applyManifestFile(ctx, data, stack, !unchanged)
		loose = append(loose, d.foreignRefs...)
		if err != nil {
			log.Printf("manifests: apply %s: %v", path, err)
			pruneReady = false
			continue
		}
		if stack != "" && n == 0 {
			// an empty stack file may be mid-write; pruning on it would delete the whole stack.
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

// loose files and hand-dropped stacks join the pass but never the digest check.
func (d *manifestDeployer) stagedTreeMatches(desired *desiredGeneration, files []string) bool {
	recorded := make(map[string]bool, len(desired.Stacks))
	for _, stack := range desired.Stacks {
		recorded[stack] = true
	}
	staged := make(map[string][]byte, len(files))
	for _, path := range files {
		stack := stackOf(d.dir, path)
		if stack == "" {
			continue
		}
		if !recorded[stack] {
			continue
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
			continue
		}
		if stack != "" {
			labels := obj.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels[group+"/stack"] = stack
			obj.SetLabels(labels)
		}
		if stack == addons.RuntimeClassStack {
			foreign := d.foreignClasses[obj.GetName()]
			if apply {
				var err error
				if foreign, err = d.foreignRuntimeClass(ctx, obj); err != nil {
					return refs, count, err
				}
			}
			if foreign {
				d.foreignRefs = append(d.foreignRefs, refOf(obj))
				count++
				continue
			}
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

// applying over a foreign RuntimeClass would delete it, and a handler mismatch fails every pass.
func (d *manifestDeployer) foreignRuntimeClass(ctx context.Context, obj *unstructured.Unstructured) (bool, error) {
	ri, err := d.manifestResourceFor(obj)
	if err != nil {
		return false, err
	}
	cur, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		delete(d.foreignClasses, obj.GetName())
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read RuntimeClass %s: %w", obj.GetName(), err)
	}
	if cur.GetLabels()[group+"/stack"] == addons.RuntimeClassStack {
		delete(d.foreignClasses, obj.GetName())
		return false, nil
	}
	if !d.foreignClasses[obj.GetName()] {
		if d.foreignClasses == nil {
			d.foreignClasses = map[string]bool{}
		}
		d.foreignClasses[obj.GetName()] = true
		log.Printf("manifests: RuntimeClass %s already exists and is not Stratum's; leaving it as it is (handler %v). "+
			"Pods naming it run under that class; delete it to have Stratum publish its own", obj.GetName(), cur.Object["handler"])
	}
	return true, nil
}

func (d *manifestDeployer) applyManifestObject(ctx context.Context, obj *unstructured.Unstructured) error {
	ri, err := d.manifestResourceFor(obj)
	if err != nil {
		if !meta.IsNoMatchError(err) {
			return err
		}
		// likely a CRD installed after the mapper was cached; rebuild once and retry.
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

// identity is GroupResource, not GVK: two versions of a kind are the same resource.
type objIdentity struct {
	gr        schema.GroupResource
	namespace string
	name      string
}

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

// a corrupt record is treated as empty: it must not recover the entries it half-holds.
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

// an unreadable cluster inventory suspends pruning.
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

func (d *manifestDeployer) pruneLocalRecord(ctx context.Context, current map[string][]objRef, loose []objRef) {
	idxPath := filepath.Join(d.dir, appliedRecordName)
	prev := readPruneRecord(d.dir)
	if len(loose) > 0 {
		// persist the ownership handoff before giving up local evidence, or late migration revives it.
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
				continue
			}
			if err != nil {
				// unresolvable: retry next pass rather than guess at what to delete.
				log.Printf("manifests: prune %s %s/%s (stack %s): resolve: %v", r.GVK, r.Namespace, r.Name, stack, err)
				retry = append(retry, r)
				continue
			}
			if err := d.deleteRef(ctx, id, gvr); err != nil {
				log.Printf("manifests: prune %s %s/%s (stack %s): %v", r.GVK, r.Namespace, r.Name, stack, err)
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
					// unresolvable: retry next pass rather than guess at what to delete.
					log.Printf("manifests: prune %s %s/%s (node %s, stack %s): resolve: %v", r.GVK, r.Namespace, r.Name, node, stack, err)
					retained = append(retained, r)
				case keep[id]:
					retained = append(retained, r)
				case deleted[id]:
					// another entry's delete already removed this object; the ref just leaves the entry.
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
		// keep refs the migration publisher added after this pass's snapshot; a later pass prunes them.
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

// runs on each conflict-retry snapshot so late migration cannot restore prune authority.
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

func (d *manifestDeployer) deleteRef(ctx context.Context, id objIdentity, gvr schema.GroupVersionResource) error {
	ri := d.dyn.Resource(gvr).Namespace(id.namespace)
	err := ri.Delete(ctx, id.name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

const clusterInputsDataKey = "cluster-inputs"

type ClusterInputs struct {
	// the cluster DNS Service IP, netcfg.DNSIPFor.
	DNSAddress    string `json:"dnsAddress"`
	ClusterDomain string `json:"clusterDomain"`
	DualStack     bool   `json:"dualStack"`
	// addons.StorageLocalPath when local-path holds the default claim, else StorageNone.
	StorageDefault string `json:"storageDefault"`
}

// tokens the staging renders into the bundled stacks; this table resolves them at apply time.
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

func needsClusterInputs(data []byte) bool {
	for _, s := range manifestSentinels {
		if bytes.Contains(data, []byte(s.token)) {
			return true
		}
	}
	return false
}

// an unresolved token fails the fill: a leftover sentinel reaches the apiserver literally.
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

// the last recording staging wins: the record is the cluster's intent, not one node's.
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
					continue
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
				continue
			}
			return err
		}
		return nil
	}
	return lastErr
}

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
		// omitting a node or falling back to the leader IP would re-render Cilium and roll every agent.
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

// IPv4 preferred: it is the address the apiserver certificate carries.
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

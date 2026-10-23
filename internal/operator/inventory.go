// inventory.go — the prune inventory in cluster state, and the desired bundle
// generation recorded beside it.
//
// The prune record is a per-node disk file: <manifests dir>/.applied.json,
// written by whichever operator instance ran the manifest pass. On a
// multi-controller cluster that record is node-local, so a leader change read
// whatever the node holding the record had last applied and pruned against a
// fraction of what the cluster once applied — and an object only another
// node's record remembered was orphaned forever. The inventory moves the
// record into cluster state: a ConfigMap in kube-system, stratum-addon-inventory,
// holds one entry per operator instance under its node's name, each entry the
// map from stack to object references that instance's manifests directory
// applied.
//
// Migration: entries are published by union, never rewritten wholesale. Run
// drives syncInventory on every operator instance — leader or not, on its own
// background interval beside the CSR approvers — and syncInventory unions the
// instance's local .applied.json into its node's entry with resourceVersion
// conflict retry: a conflict re-reads the ConfigMap, re-unions, and retries,
// so a concurrent writer's entry survives instead of being clobbered and a
// lost race never fails the pass. The migration is complete when every
// control-plane node in cluster state has contributed an entry, or when an
// explicit recovery entry closes it by hand — the escape hatch for a
// decommissioned node whose contribution can no longer be expected. The
// recovery entry is a hand edit of the ConfigMap's inventory document (set
// "recovery": true beside the nodes; there is deliberately no CLI for it), so
// closing the migration without a missing node is a human decision, never
// something the operator does on its own. Until then the manifest pass
// suspends pruning: pruning before the last contribution could delete the
// objects only that node's record remembers. Once the migration closes, the
// union of the cluster inventory — not any local file — is the record pruning
// is driven by, and syncInventory retires the local file, so a stale record
// can never re-introduce an object the cluster inventory already pruned.
//
// Desired generation: the same ConfigMap also holds the desired bundle
// generation the staging path (writeBundles in internal/cluster) records
// whenever it renders a changed bundle set — {generation, digest, provenance},
// written through the apiserver, so RBAC is the integrity boundary and the
// record is separate from the operator binary version. The deployer reads it to
// tell what it is about to apply apart from what was staged: a staged tree
// whose digest does not match the record is refused for the pass, and an
// explicitly recorded empty set — the desired-empty generation — drives pruning
// of the last stack's objects (see manifests.go).
package operator

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

const (
	// inventoryNamespace and inventoryConfigMapName locate the inventory
	// ConfigMap: kube-system, the namespace every bundled stack applies to,
	// beside the operator's other cluster-stored state.
	inventoryNamespace     = "kube-system"
	inventoryConfigMapName = "stratum-addon-inventory"

	// inventoryDataKey holds the inventory document inside the ConfigMap.
	inventoryDataKey = "inventory"

	// inventoryWriteAttempts bounds the conflict retry. Each attempt re-reads
	// the ConfigMap, re-applies the caller's change to the fresh state, and
	// writes, so a lost race costs a retry rather than the entry or the pass.
	inventoryWriteAttempts = 5
)

// pruneInventory is the cluster-stored prune inventory: one entry per operator
// instance, keyed by the node the instance runs on, each entry the map from
// stack to object references that instance's manifests directory applied.
// Recovery closes the migration to the cluster inventory by hand — a hand edit
// of the ConfigMap's inventory document, never a CLI flag — for a control
// plane that was decommissioned before it could contribute. Desired is
// the recorded desired bundle generation, nil until the staging path's first
// recording `stratum up`.
type pruneInventory struct {
	Nodes    map[string]map[string][]objRef `json:"nodes"`
	Recovery bool                           `json:"recovery,omitempty"`
	Desired  *desiredGeneration             `json:"desired,omitempty"`
	// Unowned records handoffs to apply-only loose files. Late migration
	// contributions must not revive historical stack ownership after a handoff.
	// A stack explicitly declaring the object again clears its handoff.
	Unowned []inventoryObject `json:"unowned,omitempty"`
}

// inventoryObject persists resource identity independently of served API versions.
type inventoryObject struct {
	Resource  schema.GroupResource `json:"resource"`
	Namespace string               `json:"namespace,omitempty"`
	Name      string               `json:"name"`
}

// desiredGeneration is the cluster's desired bundle generation: what the
// staging path last rendered and staged. Generation opens at 1 and moves only
// when the rendered bytes change, and it carries no monotonic rule —
// re-staging an earlier bundle set, an authorized rollback, records the next
// generation like any other. Digest is the one DigestBundles algorithm the
// deployer's check reads, over the bundle set the staging rendered; the
// deployer refuses a staged tree that digests differently. Provenance is the
// staging that produced the generation — which `stratum up`, on which node —
// not the operator binary version. The record lives in the
// stratum-addon-inventory ConfigMap and is written only through the apiserver:
// RBAC is the integrity boundary.
type desiredGeneration struct {
	Generation int    `json:"generation"`
	Digest     string `json:"digest"`
	Provenance string `json:"provenance"`
	// PublicationID distinguishes separate activations of identical content.
	PublicationID string `json:"publicationID,omitempty"`
	// JoinedPublications records accepted request IDs for lost-receipt recovery.
	// Replacing the desired publication clears these epoch-specific aliases.
	JoinedPublications []string `json:"joinedPublications,omitempty"`
	// Stacks names every directory the staging has managed, including retired
	// stacks. A follower retaining a retired directory still fails the digest
	// check; directories staging has never managed remain independently usable.
	Stacks []string `json:"stacks"`
	// Inputs travels with the desired digest so a reconcile cannot mix the
	// bundles of one generation with another generation's sentinel values.
	Inputs *ClusterInputs `json:"inputs,omitempty"`
}

// union returns every object reference the inventory holds — the union of all
// nodes' entries, stacks in a deterministic order. Once the migration is
// complete this union is what pruning drives against: everything any instance
// applied that nothing has yet pruned.
func (inv pruneInventory) union() []objRef {
	var out []objRef
	for _, node := range sortedStringKeys(inv.Nodes) {
		entry := inv.Nodes[node]
		for _, stack := range sortedStringKeys(entry) {
			out = append(out, entry[stack]...)
		}
	}
	return out
}

// migrationComplete reports whether the migration to the cluster inventory is
// closed: every control-plane node named — the ones cluster state reports —
// has contributed an entry, or a recovery entry closed it by hand. A node
// counts as contributed once an entry exists under its name, whatever the
// entry holds: an instance whose manifests directory never applied anything
// still contributes an empty entry.
func (inv pruneInventory) migrationComplete(controlPlane []string) bool {
	if inv.Recovery {
		return true
	}
	for _, node := range controlPlane {
		if _, ok := inv.Nodes[node]; !ok {
			return false
		}
	}
	return true
}

// addNode unions record into the node's entry: the references the entry
// already holds survive, and the record's are added once each, so
// republishing a node's record neither duplicates an entry nor drops another
// node's.
func (inv pruneInventory) addNode(node string, record map[string][]objRef) {
	if inv.Nodes == nil {
		inv.Nodes = map[string]map[string][]objRef{}
	}
	entry, ok := inv.Nodes[node]
	if !ok || entry == nil {
		entry = map[string][]objRef{}
		inv.Nodes[node] = entry
	}
	for stack, refs := range record {
		entry[stack] = appendUniqueRefs(entry[stack], refs)
	}
}

// loadInventory reads the inventory ConfigMap. A missing ConfigMap surfaces as
// a NotFound error — the caller decides what a cluster without an inventory
// means — and an unreadable one as a parse error: pruning treats both like a
// corrupt local record, never as an empty inventory to prune against.
func loadInventory(ctx context.Context, cs kubernetes.Interface) (pruneInventory, error) {
	cm, err := cs.CoreV1().ConfigMaps(inventoryNamespace).Get(ctx, inventoryConfigMapName, metav1.GetOptions{})
	if err != nil {
		return pruneInventory{}, err
	}
	inv, err := parseInventory(cm.Data[inventoryDataKey])
	if err != nil {
		return pruneInventory{}, fmt.Errorf("%s/%s: %w", inventoryNamespace, inventoryConfigMapName, err)
	}
	return inv, nil
}

// parseInventory decodes the inventory document. No document at all is an
// empty inventory — a ConfigMap written before its first entry — while a
// malformed one is an error the caller must never prune over.
func parseInventory(doc string) (pruneInventory, error) {
	inv := pruneInventory{Nodes: map[string]map[string][]objRef{}}
	if doc == "" {
		return inv, nil
	}
	if err := json.Unmarshal([]byte(doc), &inv); err != nil {
		return pruneInventory{}, err
	}
	if inv.Nodes == nil {
		inv.Nodes = map[string]map[string][]objRef{}
	}
	return inv, nil
}

// publishInventory unions record into the node's entry and writes the
// inventory ConfigMap back. A resourceVersion conflict — another instance
// committed its entry between this read and this write — re-reads the
// ConfigMap, re-unions, and retries, so the concurrent writer's entry survives
// instead of being clobbered. A missing ConfigMap is created: the first
// instance's publish opens the migration.
func publishInventory(ctx context.Context, cs kubernetes.Interface, node string, record map[string][]objRef) error {
	return writeInventory(ctx, cs, func(inv pruneInventory) pruneInventory {
		inv.addNode(node, record)
		return inv
	})
}

// writeInventory applies mutate to the inventory and writes the ConfigMap
// back. A resourceVersion conflict re-reads the ConfigMap, re-applies mutate
// to the fresh state, and retries, so mutate must be a pure function of the
// inventory: a retry then unions into the concurrent writer's state instead of
// clobbering it. An unreadable document refuses the write: replacing it would
// lose both authenticated intent and inventory whose local copy was retired.
func writeInventory(ctx context.Context, cs kubernetes.Interface, mutate func(pruneInventory) pruneInventory) error {
	configMaps := cs.CoreV1().ConfigMaps(inventoryNamespace)
	var lastErr error
	for attempt := 0; attempt < inventoryWriteAttempts; attempt++ {
		cm, err := configMaps.Get(ctx, inventoryConfigMapName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			inv := mutate(pruneInventory{Nodes: map[string]map[string][]objRef{}})
			b, merr := json.Marshal(inv)
			if merr != nil {
				return merr
			}
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: inventoryNamespace, Name: inventoryConfigMapName},
				Data:       map[string]string{inventoryDataKey: string(b)},
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
		inv, perr := parseInventory(cm.Data[inventoryDataKey])
		if perr != nil {
			return fmt.Errorf("%s/%s: unreadable inventory: %w", inventoryNamespace, inventoryConfigMapName, perr)
		}
		inv = mutate(inv)
		b, merr := json.Marshal(inv)
		if merr != nil {
			return merr
		}
		if string(b) == cm.Data[inventoryDataKey] {
			return nil // the stored document already is the desired one — a no-op write would only churn the resourceVersion
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[inventoryDataKey] = string(b)
		if _, err := configMaps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue // lost a race — re-read, re-union, retry
			}
			return err
		}
		return nil
	}
	return lastErr
}

// DigestBundles digests a bundle set — a map from a bundle's path under the
// manifests directory to its rendered bytes — the one algorithm the staging
// path records and the deployer checks the staged tree against, so the digest
// a `stratum up` writes and the digest a pass computes can never be two.
// The digest is a pure function of the set: each bundle's path is hashed in
// sorted order with its byte length, so a map's walk order cannot move the
// digest, and both a changed byte and the same bytes under a different path
// change it. The empty set — the desired-empty generation — digests to its own
// stable value, so an explicitly staged empty set is recordable and matchable
// like any other.
func DigestBundles(files map[string][]byte) string {
	h := sha256.New()
	var length [8]byte
	for _, path := range sortedStringKeys(files) {
		b := files[path]
		h.Write([]byte(path))
		h.Write([]byte{0})
		binary.BigEndian.PutUint64(length[:], uint64(len(b)))
		h.Write(length[:])
		h.Write(b)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// RecordDesiredGeneration records the desired bundle generation the staging
// path produced: the digest of the rendered bundle set, the provenance of the
// staging, and the names of the stack directories the digest covers — the
// staging's own namespace, which the deployer's gate digests to the exclusion
// of hand-dropped stack directories (see desiredGeneration.Stacks). The
// generation opens at 1 and moves only when the digest changes — recording an
// unchanged digest returns the recorded generation and leaves its provenance,
// the staging that produced it, exactly as it was — and it has no monotonic
// rule: a digest an earlier generation held is a changed digest like any other,
// so an authorized rollback records the next generation. The write goes through
// writeInventory, so a resourceVersion conflict re-reads the ConfigMap,
// re-applies the record onto the fresh state, and retries: a concurrent
// publisher's entry survives beside the new record and a lost race never fails
// the staging. Returns the generation the record now holds.
func RecordDesiredGeneration(ctx context.Context, cs kubernetes.Interface, digest, provenance string, stacks []string) (int, error) {
	generation := 0
	err := writeInventory(ctx, cs, func(inv pruneInventory) pruneInventory {
		if inv.Desired != nil && inv.Desired.Digest == digest {
			generation = inv.Desired.Generation
			return inv
		}
		next := 1
		if inv.Desired != nil {
			next = inv.Desired.Generation + 1
		}
		managed := slices.Clone(stacks)
		if inv.Desired != nil {
			managed = append(managed, inv.Desired.Stacks...)
		}
		inv.Desired = &desiredGeneration{Generation: next, Digest: digest, Provenance: provenance, Stacks: sortedUniqueStrings(managed)}
		generation = next
		return inv
	})
	if err != nil {
		return 0, err
	}
	return generation, nil
}

// sortedUniqueStrings returns the strings sorted and deduplicated, so a
// recorded stack list never depends on the order a map was walked in.
func sortedUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// syncInventory is the per-instance migration step: Run schedules it on every
// operator instance, leader or not, on its own interval beside the CSR
// approvers. It publishes the local prune record of the given manifests
// directory as the node's entry and, once the migration is complete, retires
// the local file. While the migration is open the record stays — it is the
// instance's contribution, and pruning is suspended against the union
// until every control plane has contributed. An unreadable local record
// requires explicit recovery before it can be retired. An empty directory name contributes
// nothing: an instance pointed at no manifests directory makes no claim about
// what was ever applied from disk, so its node stays an expected contributor
// the migration waits on — or the recovery entry closes it by hand. Once the
// migration has closed and the record is retired, the sync writes nothing:
// the cluster inventory is the record, so a publish could contribute nothing,
// and an instance whose record is gone still contributes its entry — an
// empty one — only while the migration can still wait on it.
func syncInventory(ctx context.Context, cs kubernetes.Interface, dir, node string) error {
	if dir == "" {
		return nil // no manifests directory, no claim — see the doc comment
	}
	local := filepath.Join(dir, appliedRecordName)
	if _, err := os.Stat(local); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat local inventory %s: %w", local, err)
		}
		// No local record: this instance already retired it, or its manifests
		// directory never applied anything. The entry under the node's name is
		// what an open migration waits on, so an empty contribution still
		// publishes; once the migration has closed the publish can contribute
		// nothing, so the steady state costs the gate's reads and no writes.
		closed, err := migrationClosed(ctx, cs)
		if err != nil {
			log.Printf("inventory: migration gate: %v (nothing published)", err)
			return nil
		}
		if closed {
			return nil
		}
		return publishInventory(ctx, cs, node, readPruneRecord(dir))
	}
	record, err := readMigrationRecord(local)
	if err != nil {
		inv, ierr := loadInventory(ctx, cs)
		if ierr != nil || !inv.Recovery {
			return fmt.Errorf("read local inventory %s (explicit recovery required): %w", local, err)
		}
		// Recovery explicitly accepts the missing contribution. Preserve the
		// cluster inventory and retire the local evidence only after that choice.
		if err := os.Remove(local); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := publishInventory(ctx, cs, node, record); err != nil {
		return err
	}
	closed, err := migrationClosed(ctx, cs)
	if err != nil {
		log.Printf("inventory: migration gate: %v (local record kept)", err)
		return nil
	}
	if !closed {
		return nil
	}
	if err := os.Remove(local); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// readMigrationRecord preserves read and parse failures so no missing union
// contribution can silently become an empty acknowledgment.
func readMigrationRecord(path string) (map[string][]objRef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record map[string][]objRef
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	return record, nil
}

// migrationClosed reports whether the migration to the cluster inventory has
// closed: every control-plane node cluster state names has contributed an
// entry, or a recovery entry closed it by hand. An inventory that does not
// exist yet is an open migration — a publish creates the ConfigMap — while
// any other apiserver error reads as unknown, which the caller must treat
// conservatively: nothing published, nothing retired, the next tick retries.
func migrationClosed(ctx context.Context, cs kubernetes.Interface) (bool, error) {
	nodes, err := controlPlaneNodes(ctx, cs)
	if err != nil {
		return false, fmt.Errorf("list control-plane nodes: %w", err)
	}
	inv, err := loadInventory(ctx, cs)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load: %w", err)
	}
	return inv.migrationComplete(nodes), nil
}

// controlPlaneNodes names the cluster's control-plane nodes — the migration
// gate's contributor list, read from the same labelled Nodes the Cilium
// apiserver resolution reads.
func controlPlaneNodes(ctx context.Context, cs kubernetes.Interface) ([]string, error) {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: k8sutil.ControlPlaneLabel + "=true",
	})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nodes.Items))
	for _, n := range nodes.Items {
		names = append(names, n.GetName())
	}
	sort.Strings(names)
	return names, nil
}

// appendUniqueRefs adds add to dst, skipping references dst already holds, so
// a union neither duplicates an entry nor reorders what it keeps.
func appendUniqueRefs(dst []objRef, add []objRef) []objRef {
	for _, r := range add {
		if !containsRef(dst, r) {
			dst = append(dst, r)
		}
	}
	return dst
}

func containsRef(refs []objRef, want objRef) bool {
	for _, r := range refs {
		if r == want {
			return true
		}
	}
	return false
}

// sortedStringKeys returns a map's string keys in sorted order, for a
// deterministic walk of the inventory's nodes and stacks.
func sortedStringKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

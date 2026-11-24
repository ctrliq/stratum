package operator

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"maps"
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
	inventoryNamespace     = "kube-system"
	inventoryConfigMapName = "stratum-addon-inventory"

	inventoryDataKey = "inventory"

	inventoryWriteAttempts = 5
)

type pruneInventory struct {
	Nodes    map[string]map[string][]objRef `json:"nodes"`
	Recovery bool                           `json:"recovery,omitempty"`
	Desired  *desiredGeneration             `json:"desired,omitempty"`
	// Handoff to apply-only loose files; a late migration contribution must not revive stack ownership.
	Unowned []inventoryObject `json:"unowned,omitempty"`
}

type inventoryObject struct {
	Resource  schema.GroupResource `json:"resource"`
	Namespace string               `json:"namespace,omitempty"`
	Name      string               `json:"name"`
}

type desiredGeneration struct {
	Generation         int      `json:"generation"`
	Digest             string   `json:"digest"`
	Provenance         string   `json:"provenance"`
	PublicationID      string   `json:"publicationID,omitempty"`
	JoinedPublications []string `json:"joinedPublications,omitempty"`
	// Every managed directory, retired stacks included: a follower keeping one fails the digest check.
	Stacks []string `json:"stacks"`
	// Copied beside the digest so a reconcile cannot mix generations' inputs.
	Inputs *ClusterInputs `json:"inputs,omitempty"`
}

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

func publishInventory(ctx context.Context, cs kubernetes.Interface, node string, record map[string][]objRef) error {
	return writeInventory(ctx, cs, func(inv pruneInventory) pruneInventory {
		inv.addNode(node, record)
		return inv
	})
}

// mutate must be pure in inv; replacing an unreadable inventory would lose other nodes' entries.
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
					continue
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
			return nil // unchanged: skip the write to avoid churning resourceVersion
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[inventoryDataKey] = string(b)
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

// DigestBundles digests a bundle set to a stable value, independent of map walk order.
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

// RecordDesiredGeneration records the desired generation; an unchanged digest keeps it.
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

func sortedUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

func syncInventory(ctx context.Context, cs kubernetes.Interface, dir, node string) error {
	if dir == "" {
		return nil
	}
	local := filepath.Join(dir, appliedRecordName)
	if _, err := os.Stat(local); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat local inventory %s: %w", local, err)
		}
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

// Failures must surface, not become an empty acknowledgment of a missing contribution.
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

// A missing inventory is an open migration; any other error is unknown, treated conservatively.
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

func appendUniqueRefs(dst []objRef, add []objRef) []objRef {
	for _, r := range add {
		if !slices.Contains(dst, r) {
			dst = append(dst, r)
		}
	}
	return dst
}

func sortedStringKeys[M ~map[string]V, V any](m M) []string {
	return slices.Sorted(maps.Keys(m))
}

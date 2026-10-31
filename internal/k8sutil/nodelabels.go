package k8sutil

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// The startup gate a GPU node joins behind: the taint holds workloads off it
// until it is released, and the annotation records who released it and when.
const (
	UnverifiedTaint    = "stratum.ciq.com/unverified"
	VerifiedAnnotation = "stratum.ciq.com/verified"
)

// nodeLabelChanges is the label patch that gives a Node the labels it
// declares and the runtime-handler label of each handler it declares, and
// removes the handler label of a handler it no longer declares. A label the
// node does not declare, outside the handler prefix, is left alone: it is
// another writer's. nil means the Node already carries them.
func nodeLabelChanges(current, declared map[string]string, handlers []string) map[string]*string {
	want := maps.Clone(declared)
	if want == nil {
		want = map[string]string{}
	}
	for _, h := range handlers {
		want[RuntimeHandlerLabelPrefix+h] = "true"
	}
	changes := map[string]*string{}
	for _, k := range slices.Sorted(maps.Keys(want)) {
		if v, ok := current[k]; !ok || v != want[k] {
			changes[k] = new(string)
			*changes[k] = want[k]
		}
	}
	for k := range current {
		if _, ok := want[k]; !ok && strings.HasPrefix(k, RuntimeHandlerLabelPrefix) {
			changes[k] = nil
		}
	}
	if len(changes) == 0 {
		return nil
	}
	return changes
}

// ReconcileOwnNodeLabels gives a node's own Node the labels it declares and
// the runtime-handler labels of its handlers, as its kubelet: NodeRestriction
// lets a node set those on itself, and the patch carries labels only, so it
// never touches the taints a kubelet may not change. It reads the Node first
// and patches only what differs, so a start that changes nothing writes
// nothing.
func ReconcileOwnNodeLabels(ctx context.Context, node string, declared map[string]string, handlers []string) error {
	cs, err := kubeletClientset()
	if err != nil {
		return fmt.Errorf("label %s: %w", node, err)
	}
	return reconcileNodeLabels(ctx, cs, node, declared, handlers)
}

func reconcileNodeLabels(ctx context.Context, cs kubernetes.Interface, node string, declared map[string]string, handlers []string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	n, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("label %s: %w", node, err)
	}
	changes := nodeLabelChanges(n.Labels, declared, handlers)
	if changes == nil {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": changes}})
	if err != nil {
		return err
	}
	if _, err := cs.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("label %s: %w", node, err)
	}
	return nil
}

package addons

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// RuntimeClassStack is the stack the cluster's RuntimeClasses are staged in,
// only while spec.runtimeClasses names any.
const RuntimeClassStack = "runtime-classes"

// RenderRuntimeClasses renders a node.k8s.io/v1 RuntimeClass per handler name,
// sorted, each scheduling its pods onto the nodes that carry the handler's
// runtime-handler label. None is a default: a pod runs under a handler only by
// naming its RuntimeClass. nil for no names.
func RenderRuntimeClasses(names []string) []byte {
	if len(names) == 0 {
		return nil
	}
	docs := make([]string, 0, len(names))
	for _, n := range slices.Sorted(slices.Values(names)) {
		docs = append(docs, fmt.Sprintf(`apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: %[1]s
handler: %[1]s
scheduling:
  nodeSelector:
    %[2]s%[1]s: "true"
`, n, k8sutil.RuntimeHandlerLabelPrefix))
	}
	return []byte(strings.Join(docs, "---\n"))
}

package cluster

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/runtime"
)

// NodeSpec is spec.node: what this node declares to the scheduler and containerd.
type NodeSpec struct {
	Labels          map[string]string `json:"labels,omitempty"`
	Taints          []string          `json:"taints,omitempty"` // key[=value]:Effect
	RuntimeHandlers map[string]string `json:"runtimeHandlers,omitempty"`
}

// Keys stratum manages itself, which a node must not declare.
func engineOwnedKey(key string) bool {
	return key == k8sutil.ControlPlaneLabel || strings.HasPrefix(key, k8sutil.RuntimeHandlerLabelPrefix)
}

// kubernetes.io labels NodeRestriction lets a node set and the kubelet does not overwrite.
var kubeletLabels = []string{
	"topology.kubernetes.io/zone", "topology.kubernetes.io/region",
	"failure-domain.beta.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/region",
	"beta.kubernetes.io/instance-type",
}

func nodeMaySetLabel(key string) bool {
	ns, _, ok := strings.Cut(key, "/")
	if !ok {
		return true
	}
	under := func(domain string) bool { return ns == domain || strings.HasSuffix(ns, "."+domain) }
	switch {
	case under("node-restriction.kubernetes.io"):
		return false
	case under("kubelet.kubernetes.io"), under("node.kubernetes.io"):
		return true
	case under("kubernetes.io"), under("k8s.io"):
		return slices.Contains(kubeletLabels, key)
	}
	return true
}

// Reports the first refusal; reads nothing from the host (CheckBinaries does).
func (n NodeSpec) Validate() error {
	for _, k := range slices.Sorted(maps.Keys(n.Labels)) {
		if err := validateLabel(k, n.Labels[k]); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, t := range n.Taints {
		key, _, effect, err := parseTaint(t)
		if err != nil {
			return err
		}
		if seen[key+":"+effect] {
			return fmt.Errorf("node taint %q: %s:%s is declared twice", t, key, effect)
		}
		seen[key+":"+effect] = true
	}
	for _, name := range slices.Sorted(maps.Keys(n.RuntimeHandlers)) {
		if err := runtime.ValidateHandler(name, n.RuntimeHandlers[name]); err != nil {
			return err
		}
	}
	return nil
}

// A join refuses a missing handler binary; a restart only warns.
func (n NodeSpec) CheckBinaries() error {
	for _, name := range slices.Sorted(maps.Keys(n.RuntimeHandlers)) {
		if err := runtime.CheckHandlerBinary(n.RuntimeHandlers[name]); err != nil {
			return fmt.Errorf("runtime handler %s: %w", name, err)
		}
	}
	return nil
}

func validateLabel(key, value string) error {
	if errs := validation.IsQualifiedName(key); len(errs) > 0 {
		return fmt.Errorf("node label %q: %s", key, strings.Join(errs, "; "))
	}
	if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
		return fmt.Errorf("node label %s=%q: %s", key, value, strings.Join(errs, "; "))
	}
	if engineOwnedKey(key) {
		return fmt.Errorf("node label %s is set by stratum itself and cannot be declared", key)
	}
	if !nodeMaySetLabel(key) {
		return fmt.Errorf("node label %s is in a namespace a node may not label itself with (the API server's NodeRestriction); "+
			"label the Node from a control plane with `stratum kubectl label node` instead", key)
	}
	return nil
}

func parseTaint(t string) (key, value, effect string, err error) {
	key, value, effect, err = runtime.ParseTaint(t)
	if err != nil {
		return "", "", "", err
	}
	if engineOwnedKey(key) {
		return "", "", "", fmt.Errorf("node taint %q: %s is set by stratum itself and cannot be declared", t, key)
	}
	return key, value, effect, nil
}

// No values gives a nil map, which a restart reads as not given.
func ParseKeyValues(flag string, values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, v := range values {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%s %q is not name=value", flag, v)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("%s %s is given twice", flag, k)
		}
		out[k] = val
	}
	return out, nil
}

// Reads only handler binaries from the host, so it can run before touching it.
func JoinNodeSpec(file, flags NodeSpec) (NodeSpec, error) {
	node := mergeNodeSpec(file, flags)
	if err := node.Validate(); err != nil {
		return NodeSpec{}, err
	}
	if err := node.CheckBinaries(); err != nil {
		return NodeSpec{}, fmt.Errorf("%w; install it before this node joins", err)
	}
	return node, nil
}

func mergeNodeSpec(file, flags NodeSpec) NodeSpec {
	out := NodeSpec{Labels: mergeMaps(file.Labels, flags.Labels), RuntimeHandlers: mergeMaps(file.RuntimeHandlers, flags.RuntimeHandlers)}
	keyEffect := func(t string) string {
		key, _, effect, _ := runtime.SplitTaint(t)
		return key + ":" + effect
	}
	overridden := map[string]bool{}
	for _, t := range flags.Taints {
		overridden[keyEffect(t)] = true
	}
	for _, t := range file.Taints {
		if !overridden[keyEffect(t)] {
			out.Taints = append(out.Taints, t)
		}
	}
	out.Taints = append(out.Taints, flags.Taints...)
	return out
}

func mergeMaps(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := maps.Clone(base)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, over)
	return out
}

func validateRuntimeClasses(names []string) error {
	seen := map[string]bool{}
	for _, n := range names {
		if errs := validation.IsDNS1123Label(n); len(errs) > 0 {
			return fmt.Errorf("spec.runtimeClasses: %q: %s", n, strings.Join(errs, "; "))
		}
		if n == "runc" {
			return errors.New("spec.runtimeClasses: runc is containerd's default handler and needs no RuntimeClass")
		}
		if seen[n] {
			return fmt.Errorf("spec.runtimeClasses: %s is listed twice", n)
		}
		seen[n] = true
	}
	return nil
}

// Order-insensitive; a taint that does not parse compares by its spelling.
func sameTaints(a, b []string) bool {
	canon := func(ts []string) []string {
		out := make([]string, 0, len(ts))
		for _, t := range ts {
			key, value, effect, err := parseTaint(t)
			if err != nil {
				out = append(out, t)
				continue
			}
			out = append(out, key+"="+value+":"+effect)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(canon(a), canon(b))
}

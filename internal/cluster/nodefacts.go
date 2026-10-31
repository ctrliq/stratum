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

// NodeSpec is spec.node: what this node declares about itself to the
// scheduler and to containerd. Labels and taints are its Node object's;
// runtimeHandlers are extra containerd runtime handlers, by name, each the
// absolute path of its OCI runtime binary. A control plane reads it at its
// first boot or join and on every start after; a worker reads it at its join,
// and a restart changes its labels and handlers only through its flags.
type NodeSpec struct {
	Labels          map[string]string `json:"labels,omitempty"`
	Taints          []string          `json:"taints,omitempty"` // key[=value]:Effect
	RuntimeHandlers map[string]string `json:"runtimeHandlers,omitempty"`
}

// engineOwnedKey reports the label and taint keys stratum manages itself,
// which a node must not declare: the control-plane mark, and the per-handler labels
// a RuntimeClass selects nodes by.
func engineOwnedKey(key string) bool {
	return key == k8sutil.ControlPlaneLabel || strings.HasPrefix(key, k8sutil.RuntimeHandlerLabelPrefix)
}

// kubeletLabels are the kubernetes.io labels outside the kubelet.kubernetes.io
// and node.kubernetes.io namespaces that NodeRestriction lets a node set on
// itself and the kubelet does not set to its own values.
var kubeletLabels = []string{
	"topology.kubernetes.io/zone", "topology.kubernetes.io/region",
	"failure-domain.beta.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/region",
	"beta.kubernetes.io/instance-type",
}

// nodeMaySetLabel reports whether a node may declare key on its own Node.
// Its kubelet applies the labels, and NodeRestriction refuses a node the
// kubernetes.io and k8s.io namespaces outside the labels the kubelet may set,
// and node-restriction.kubernetes.io always; the kubelet also overwrites the
// hostname, os and arch labels with its own.
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

var taintEffects = []string{"NoSchedule", "PreferNoSchedule", "NoExecute"}

// Validate reports the first label, taint or runtime handler the node cannot
// declare. It reads nothing from the host; CheckBinaries does.
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

// CheckBinaries reports the first runtime handler whose binary is not an
// executable file on this host. A join refuses it; a restart only warns.
func (n NodeSpec) CheckBinaries() error {
	for _, name := range slices.Sorted(maps.Keys(n.RuntimeHandlers)) {
		if err := runtime.CheckHandlerBinary(n.RuntimeHandlers[name]); err != nil {
			return fmt.Errorf("runtime handler %s: %w; install it before this node joins", name, err)
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

// parseTaint splits key[=value]:Effect and validates each part as the API
// server would.
func parseTaint(t string) (key, value, effect string, err error) {
	kv, effect, ok := strings.Cut(t, ":")
	if !ok || strings.Contains(effect, ":") {
		return "", "", "", fmt.Errorf("node taint %q is not key[=value]:Effect", t)
	}
	if !slices.Contains(taintEffects, effect) {
		return "", "", "", fmt.Errorf("node taint %q: effect %q is not one of %s", t, effect, strings.Join(taintEffects, ", "))
	}
	key, value, _ = strings.Cut(kv, "=")
	if errs := validation.IsQualifiedName(key); len(errs) > 0 {
		return "", "", "", fmt.Errorf("node taint %q: key: %s", t, strings.Join(errs, "; "))
	}
	if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
		return "", "", "", fmt.Errorf("node taint %q: value: %s", t, strings.Join(errs, "; "))
	}
	if engineOwnedKey(key) {
		return "", "", "", fmt.Errorf("node taint %q: %s is set by stratum itself and cannot be declared", t, key)
	}
	return key, value, effect, nil
}

// ParseKeyValues turns repeated name=value flag values into a map, refusing a
// value without "=" and a name given twice. flag names the flag in errors.
// No values is nil, which a restart reads as "not given".
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

// JoinNodeSpec is the spec.node a worker join saves: its flags laid over its
// config file's, validated, with every runtime handler's binary installed. It
// reads only the host's handler binaries, so a caller can run it before
// anything on the host is changed.
func JoinNodeSpec(file, flags NodeSpec) (NodeSpec, error) {
	node := mergeNodeSpec(file, flags)
	if err := node.Validate(); err != nil {
		return NodeSpec{}, err
	}
	if err := node.CheckBinaries(); err != nil {
		return NodeSpec{}, err
	}
	return node, nil
}

// mergeNodeSpec lays a join's flags over its config file's spec.node: a flag
// label or handler replaces the file's of the same name, and a flag taint the
// file's with the same key and effect.
func mergeNodeSpec(file, flags NodeSpec) NodeSpec {
	out := NodeSpec{Labels: mergeMaps(file.Labels, flags.Labels), RuntimeHandlers: mergeMaps(file.RuntimeHandlers, flags.RuntimeHandlers)}
	keyEffect := func(t string) string {
		kv, effect, _ := strings.Cut(t, ":")
		key, _, _ := strings.Cut(kv, "=")
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

// validateRuntimeClasses reports the first spec.runtimeClasses entry that is
// not a handler name a RuntimeClass can carry.
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

// sameTaints reports whether two taint lists declare the same taints,
// whatever their order or spelling of an empty value. A taint that does not
// parse compares by its spelling.
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

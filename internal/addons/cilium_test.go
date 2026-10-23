package addons

import (
	"bytes"
	"io"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// decodeAll parses a multi-document YAML into unstructured objects, asserting the
// manifest is structurally valid (the same decode path applyYAML uses).
func decodeAll(t *testing.T, data []byte) []*unstructured.Unstructured {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		err := dec.Decode(obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(obj.Object) > 0 {
			out = append(out, obj)
		}
	}
	return out
}

func TestCiliumManifestsValid(t *testing.T) {
	// Only the kube-proxy-replacement rendering exists: Stratum
	// ships one service dataplane.
	for name, data := range map[string][]byte{
		"kpr-true": ciliumKPRTrueYAML,
	} {
		t.Run(name, func(t *testing.T) {
			objs := decodeAll(t, data)
			if len(objs) == 0 {
				t.Fatal("no objects decoded")
			}
			var haveDS bool
			for _, o := range objs {
				if o.GetKind() == "DaemonSet" && o.GetName() == "cilium" {
					haveDS = true
				}
			}
			if !haveDS {
				t.Error("manifest missing the cilium DaemonSet")
			}
			// No TLS secrets must be baked into a distributed binary.
			for _, o := range objs {
				if o.GetKind() == "Secret" {
					t.Errorf("manifest bakes a Secret %q — should be hubble.enabled=false", o.GetName())
				}
			}
		})
	}
}

func TestCiliumKPRModeMatchesFile(t *testing.T) {
	if !bytes.Contains(ciliumKPRTrueYAML, []byte(`kube-proxy-replacement: "true"`)) {
		t.Error("kpr-true manifest does not set kube-proxy-replacement true")
	}
}

func findObject(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	i := slices.IndexFunc(objs, func(o *unstructured.Unstructured) bool {
		return o.GetKind() == kind && o.GetName() == name
	})
	if i < 0 {
		t.Fatalf("the rendered manifest has no %s named %q", kind, name)
	}
	return objs[i]
}

func TestCiliumAgentRollsOnAConfigChange(t *testing.T) {
	const (
		checksumAnn  = "cilium.io/cilium-configmap-checksum"
		sha256HexLen = 64
	)
	if !bytes.Contains(ciliumKPRTrueYAML, []byte(`enable-l2-announcements: "true"`)) {
		t.Error("kpr-true manifest does not enable l2 announcements")
	}
	if bytes.Contains(ciliumKPRTrueYAML, []byte("cni-uninstall:")) {
		t.Error("cilium-config sets cni-uninstall, so the agent's preStop hook will delete each node's " +
			"CNI conflist on every restart — with the rollOutCiliumPods checksum below, a cilium-config " +
			"change would then take every node NotReady in turn")
	}
	objs := decodeAll(t, ciliumKPRTrueYAML)
	sums := map[string]string{}
	for _, w := range []struct{ kind, name, value string }{
		{"DaemonSet", "cilium", "rollOutCiliumPods=true"},
		{"Deployment", "cilium-operator", "operator.rollOutPods=true"},
	} {
		o := findObject(t, objs, w.kind, w.name)
		sum, _, err := unstructured.NestedString(o.Object, "spec", "template", "metadata", "annotations", checksumAnn)
		if err != nil {
			t.Fatalf("read %s on the %s %s pod template: %v", checksumAnn, w.kind, w.name, err)
		}
		switch {
		case sum == "":
			t.Errorf("the %s %s pod template carries no %s, so regenerating this manifest with new "+
				"cilium-config values leaves it running on its old startup options (regenerate with %s). "+
				"Note this only pins that the annotation exists: a checksum left stale by a hand edit to the "+
				"committed asset is not detectable here", w.kind, w.name, checksumAnn, w.value)
		case len(sum) != sha256HexLen:
			t.Errorf("the %s %s %s is %q (%d chars), not a %d-char sha256 hex digest: if upstream "+
				"changed the checksum algorithm update this assertion, otherwise the manifest was edited by hand",
				w.kind, w.name, checksumAnn, sum, len(sum), sha256HexLen)
		}
		sums[w.name] = sum
	}
	if sums["cilium"] != sums["cilium-operator"] {
		t.Errorf("the agent and operator checksums differ (%q, %q), though both hash the same ConfigMap",
			sums["cilium"], sums["cilium-operator"])
	}
}

// TestCiliumAgentsRestartOneNodeAtATime: a cilium-config change restarts every
// agent, and no new pod starts on a node while its agent is down.
func TestCiliumAgentsRestartOneNodeAtATime(t *testing.T) {
	ds := findObject(t, decodeAll(t, ciliumKPRTrueYAML), "DaemonSet", "cilium")
	v, found, err := unstructured.NestedFieldNoCopy(ds.Object, "spec", "updateStrategy", "rollingUpdate", "maxUnavailable")
	if err != nil || !found {
		t.Fatalf("spec.updateStrategy.rollingUpdate.maxUnavailable: found=%v err=%v", found, err)
	}
	if n, ok := v.(int64); !ok || n != 1 {
		t.Errorf("the cilium DaemonSet's maxUnavailable is %v, want 1 "+
			"(regenerate with updateStrategy.rollingUpdate.maxUnavailable=1)", v)
	}
}

func TestCiliumAgentCanElectL2Announcers(t *testing.T) {
	cr := findObject(t, decodeAll(t, ciliumKPRTrueYAML), "ClusterRole", "cilium")
	rules, _, err := unstructured.NestedSlice(cr.Object, "rules")
	if err != nil {
		t.Fatalf("read the cilium ClusterRole rules: %v", err)
	}
	var verbs []string
	var hasRule bool
	for i, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		groups, _, err := unstructured.NestedStringSlice(m, "apiGroups")
		if err != nil {
			t.Fatalf("cilium ClusterRole rule %d apiGroups: %v", i, err)
		}
		resources, _, err := unstructured.NestedStringSlice(m, "resources")
		if err != nil {
			t.Fatalf("cilium ClusterRole rule %d resources: %v", i, err)
		}
		if !slices.Contains(groups, "coordination.k8s.io") || !slices.Contains(resources, "leases") {
			continue
		}
		hasRule = true
		got, _, err := unstructured.NestedStringSlice(m, "verbs")
		if err != nil {
			t.Fatalf("cilium ClusterRole rule %d verbs: %v", i, err)
		}
		verbs = append(verbs, got...)
	}
	if !hasRule {
		t.Fatal("the cilium agent ClusterRole has no coordination.k8s.io/leases rule, so the L2 announcer " +
			"cannot elect a per-service announcer")
	}
	for _, want := range []string{"create", "get", "update", "list", "delete"} {
		if !slices.Contains(verbs, want) {
			t.Errorf("the cilium agent ClusterRole leases rule is missing verb %q (grants %v)", want, verbs)
		}
	}
}

// TestCiliumSentinelSubstitution: both apiserver sentinels are in the asset,
// RenderCilium leaves them for the operator, and FillAPIServer resolves them
// completely.
func TestCiliumSentinelSubstitution(t *testing.T) {
	for _, s := range []string{apiserverHostSentinel, apiserverURLsSentinel} {
		if !bytes.Contains(ciliumKPRTrueYAML, []byte(s)) {
			t.Errorf("sentinel %q not found in asset", s)
		}
	}
	rendered := RenderCilium(false, CiliumSettings{})
	if !NeedsAPIServer(rendered) {
		t.Fatal("RenderCilium resolved the apiserver itself; it must be left for the operator")
	}
	out := FillAPIServer(rendered, APIServer{Host: "10.0.0.5", URLs: []string{"https://10.0.0.5:6443", "https://10.0.0.6:6443"}})
	if NeedsAPIServer(out) {
		t.Error("a sentinel survived FillAPIServer")
	}
	for _, want := range []string{
		`value: "10.0.0.5"`,
		`k8s-api-server-urls: "https://10.0.0.5:6443 https://10.0.0.6:6443"`,
		`- "--k8s-api-server-urls=https://10.0.0.5:6443 https://10.0.0.6:6443"`,
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("filled manifest lacks %s", want)
		}
	}
}

// TestCiliumOperatorFitsASingleNode guards single-node installs: the rendered cilium-operator
// Deployment must not ask for more replicas than one node can hold.
//
// The operator pod is hostNetwork with fixed hostPorts (9234/9963) and carries a
// *required* podAntiAffinity on kubernetes.io/hostname, so at most one replica
// can ever land on a given node — the scheduler's NodePorts filter and the
// affinity rule each rule the second one out on their own. stratum bootstraps as
// a single node and single-node is a supported end state, so every replica past
// the first sits Pending forever there. Whether that is fixed by lowering the
// count or by letting two operators share a node, the invariant below is the one
// that matters.
func TestCiliumOperatorFitsASingleNode(t *testing.T) {
	objs := decodeAll(t, FillAPIServer(RenderCilium(false, CiliumSettings{}), APIServer{Host: "10.0.0.5"}))

	dep := findObject(t, objs, "Deployment", "cilium-operator")

	replicas, found, err := unstructured.NestedInt64(dep.Object, "spec", "replicas")
	if err != nil || !found {
		t.Fatalf("spec.replicas: found=%v err=%v", found, err)
	}
	if replicas < 1 {
		t.Fatalf("spec.replicas = %d, want at least 1", replicas)
	}
	if replicas == 1 {
		// A lone replica holding hostPorts cannot be replaced by a surge pod:
		// the new one lands on the same node and cannot bind :9234/:9963 while
		// the old one is up, so the rollout wedges. maxUnavailable has to let
		// the running pod go down first.
		if len(hostPorts(t, dep)) > 0 {
			assertRolloutCanTakeThePodDownFirst(t, dep, replicas)
		}
		return
	}

	// Past one replica, the pod spec has to actually permit two operators on the
	// same node. Report every reason it does not.
	if key := requiredHostnameAntiAffinity(t, dep); key != "" {
		t.Errorf("spec.replicas = %d but a required podAntiAffinity on %q admits only one operator pod per node", replicas, key)
	}
	if ports := hostPorts(t, dep); len(ports) > 0 {
		t.Errorf("spec.replicas = %d but the pod claims hostPort(s) %v, which only one pod per node can bind", replicas, ports)
	}
}

// requiredHostnameAntiAffinity returns the per-node topology key of a hard pod
// anti-affinity term, or "" when nothing pins the pods one-per-node.
func requiredHostnameAntiAffinity(t *testing.T, dep *unstructured.Unstructured) string {
	t.Helper()
	terms, _, err := unstructured.NestedSlice(dep.Object, "spec", "template", "spec",
		"affinity", "podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution")
	if err != nil {
		t.Fatalf("podAntiAffinity: %v", err)
	}
	for _, term := range terms {
		m, ok := term.(map[string]any)
		if !ok {
			continue
		}
		if key, _, _ := unstructured.NestedString(m, "topologyKey"); key == "kubernetes.io/hostname" {
			return key
		}
	}
	return ""
}

// hostPorts returns every hostPort the pod's containers claim.
func hostPorts(t *testing.T, dep *unstructured.Unstructured) []int64 {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	if err != nil {
		t.Fatalf("containers: %v", err)
	}
	var out []int64
	for _, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		ports, _, err := unstructured.NestedSlice(m, "ports")
		if err != nil {
			t.Fatalf("ports: %v", err)
		}
		for _, p := range ports {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if hp, found, _ := unstructured.NestedInt64(pm, "hostPort"); found {
				out = append(out, hp)
			}
		}
	}
	return out
}

// assertRolloutCanTakeThePodDownFirst checks that spec.strategy lets the whole
// Deployment be unavailable during an update.
func assertRolloutCanTakeThePodDownFirst(t *testing.T, dep *unstructured.Unstructured, replicas int64) {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(dep.Object, "spec", "strategy", "rollingUpdate", "maxUnavailable")
	if err != nil || !found {
		t.Fatalf("spec.strategy.rollingUpdate.maxUnavailable: found=%v err=%v", found, err)
	}
	switch mu := v.(type) {
	case string:
		if mu != "100%" {
			t.Errorf("maxUnavailable = %q with %d replica(s) holding hostPorts: the update would wait forever "+
				"on a surge pod that cannot bind them, want \"100%%\"", mu, replicas)
		}
	case int64:
		if mu < replicas {
			t.Errorf("maxUnavailable = %d with %d replica(s) holding hostPorts: the update would wait forever "+
				"on a surge pod that cannot bind them, want at least %d", mu, replicas, replicas)
		}
	default:
		t.Fatalf("maxUnavailable: unexpected type %T", v)
	}
}

// TestCiliumRenderAnchorsAreUnique keeps RenderCilium's byte substitutions
// targeted. Each anchor it rewrites has exactly one home in the generated asset;
// a regeneration that duplicates one would have ReplaceAll quietly rewrite an
// unrelated object, and a regeneration that renames one would have it quietly
// rewrite nothing.
func TestCiliumRenderAnchorsAreUnique(t *testing.T) {
	for _, anchor := range []string{
		"\n  replicas: 2\n",
		"maxUnavailable: 50%",
		`enable-ipv6: "false"`,
		ciliumRoutingModeAnchor,
		ciliumDevicesAnchor,
		ciliumDirectRoutesAnchor,
	} {
		if got := bytes.Count(ciliumKPRTrueYAML, []byte(anchor)); got != 1 {
			t.Errorf("anchor %q occurs %d times in the asset, want exactly 1", anchor, got)
		}
	}
	if got := bytes.Count(ciliumKPRTrueYAML, []byte(ciliumChecksumPrefix)); got != 2 {
		t.Errorf("the configmap checksum line occurs %d times in the asset, want 2: the agent's and the operator's", got)
	}
}

// TestCiliumNodePortsAnswerOnEveryAddress pins nodeport-addresses to every
// address on the native devices, as kube-proxy answers NodePorts on every
// local address. Left empty, Cilium serves a NodePort on each device's
// primary address only, so the kube-vip VIP, a secondary /32 on its holder,
// did not answer one from another host. It holds with spec.network.cilium
// settings applied too.
func TestCiliumNodePortsAnswerOnEveryAddress(t *testing.T) {
	for name, m := range map[string][]byte{
		"shipped":       ciliumKPRTrueYAML,
		"with settings": RenderCilium(false, CiliumSettings{MTU: 1400, Devices: []string{"eth0"}}),
	} {
		cm := findObject(t, decodeAll(t, m), "ConfigMap", "cilium-config")
		got, _, err := unstructured.NestedString(cm.Object, "data", "nodeport-addresses")
		if err != nil {
			t.Fatalf("%s: cilium-config nodeport-addresses: %v", name, err)
		}
		if got != "0.0.0.0/0,::/0" {
			t.Errorf("%s: nodeport-addresses = %q, want %q (regenerate with nodePort.addresses={0.0.0.0/0,::/0})",
				name, got, "0.0.0.0/0,::/0")
		}
	}
}

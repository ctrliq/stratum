package addons

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ctrliq/stratum/internal/version"
)

// TestCoreDNSSurvivesLosingANode pins the node-loss shape: two replicas,
// hostname anti-affinity that is preferred rather than required, and 30 s
// not-ready/unreachable tolerations.
func TestCoreDNSSurvivesLosingANode(t *testing.T) {
	dep := coreDNSDeployment(t)

	if r, _, _ := unstructured.NestedInt64(dep.Object, "spec", "replicas"); r != 2 {
		t.Errorf("replicas = %d, want 2", r)
	}

	anti, _, _ := unstructured.NestedMap(dep.Object, "spec", "template", "spec", "affinity", "podAntiAffinity")
	if _, required := anti["requiredDuringSchedulingIgnoredDuringExecution"]; required {
		t.Error("anti-affinity is required: a single-node cluster could then never schedule the second replica")
	}
	pref, _ := anti["preferredDuringSchedulingIgnoredDuringExecution"].([]any)
	spread := false
	for _, p := range pref {
		key, _, _ := unstructured.NestedString(p.(map[string]any), "podAffinityTerm", "topologyKey")
		sel, _, _ := unstructured.NestedString(p.(map[string]any), "podAffinityTerm", "labelSelector", "matchLabels", "k8s-app")
		if key == "kubernetes.io/hostname" && sel == "kube-dns" {
			spread = true
		}
	}
	if !spread {
		t.Error("no preferred anti-affinity spreading kube-dns pods across hostnames")
	}

	tols, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "tolerations")
	for _, key := range []string{"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable"} {
		found := false
		for _, tl := range tols {
			m := tl.(map[string]any)
			if m["key"] == key && m["effect"] == "NoExecute" {
				found = true
				if s, _, _ := unstructured.NestedInt64(m, "tolerationSeconds"); s != 30 {
					t.Errorf("%s tolerationSeconds = %d, want 30", key, s)
				}
			}
		}
		if !found {
			t.Errorf("no NoExecute toleration for %s: a replica on a dead node would wait the default 300 s", key)
		}
	}
}

// TestCoreDNSResistsDiskPressureEviction pins the disk-pressure shape: critical
// priority, which makes the kubelet refuse to evict it, and a positive
// ephemeral-storage request sized for retained logs. There is deliberately no
// disk-pressure toleration, so the pod is never placed onto a pressured node.
func TestCoreDNSResistsDiskPressureEviction(t *testing.T) {
	dep := coreDNSDeployment(t)

	if pc, _, _ := unstructured.NestedString(dep.Object, "spec", "template", "spec", "priorityClassName"); pc != "system-cluster-critical" {
		t.Errorf("priorityClassName = %q, want system-cluster-critical", pc)
	}

	ctrs, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	found := false
	for _, c := range ctrs {
		m := c.(map[string]any)
		if m["name"] != "coredns" {
			continue
		}
		found = true
		req, _, _ := unstructured.NestedString(m, "resources", "requests", "ephemeral-storage")
		if q, err := resource.ParseQuantity(req); err != nil || q.Sign() <= 0 {
			t.Errorf("coredns ephemeral-storage request = %q, want a positive quantity", req)
		}
		if _, limited, _ := unstructured.NestedString(m, "resources", "limits", "ephemeral-storage"); limited {
			t.Error("coredns container has an ephemeral-storage limit: exceeding it evicts the pod")
		}
	}
	if !found {
		t.Error("no coredns container")
	}

	tols, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "tolerations")
	for _, tl := range tols {
		if k, _ := tl.(map[string]any)["key"].(string); k == "node.kubernetes.io/disk-pressure" {
			t.Error("disk-pressure toleration present: the pod could be placed onto a pressured node")
		}
	}
}

// TestCoreDNSHasNoDisruptionBudget pins a deliberate absence: a PDB would fail
// every single-node upgrade, where both replicas share the cordoned node.
func TestCoreDNSHasNoDisruptionBudget(t *testing.T) {
	for _, o := range decodeAll(t, RenderCoreDNS()) {
		if o.GetKind() == "PodDisruptionBudget" {
			t.Errorf("the CoreDNS stack carries PodDisruptionBudget %q: it would wedge single-node upgrades", o.GetName())
		}
	}
}

func coreDNSDeployment(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	for _, o := range decodeAll(t, RenderCoreDNS()) {
		if o.GetKind() == "Deployment" && o.GetName() == "coredns" {
			return o
		}
	}
	t.Fatal("rendered CoreDNS stack has no coredns Deployment")
	return nil
}

// TestCoreDNSForwardsToTheNodesResolvers: CoreDNS forwards through
// the copy its init container makes of the node's chosen upstream, mounted
// from version.CoreDNSUpstreamDir, and nothing else in the pod changes how it
// resolves.
func TestCoreDNSForwardsToTheNodesResolvers(t *testing.T) {
	var corefile string
	for _, o := range decodeAll(t, RenderCoreDNS()) {
		if o.GetKind() == "ConfigMap" && o.GetName() == "coredns" {
			corefile, _, _ = unstructured.NestedString(o.Object, "data", "Corefile")
		}
	}
	if corefile == "" {
		t.Fatal("rendered CoreDNS stack has no coredns ConfigMap with a Corefile")
	}
	var forwards []string
	for _, line := range strings.Split(corefile, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "forward" {
			forwards = append(forwards, strings.Join(f, " "))
		}
	}
	if want := []string{"forward . /etc/coredns-upstream/resolv.conf {"}; !slices.Equal(forwards, want) {
		t.Errorf("Corefile forward directives = %q, want %q", forwards, want)
	}

	dep := coreDNSDeployment(t)
	spec, _, _ := unstructured.NestedMap(dep.Object, "spec", "template", "spec")
	if spec["dnsPolicy"] != "Default" {
		t.Errorf("coredns dnsPolicy = %v, want Default: anything else hands it a resolv.conf naming cluster DNS, which is itself", spec["dnsPolicy"])
	}
	volumes := map[string]map[string]any{}
	vs, _, _ := unstructured.NestedSlice(spec, "volumes")
	for _, v := range vs {
		m := v.(map[string]any)
		volumes[m["name"].(string)] = m
	}
	if hp, _, _ := unstructured.NestedMap(volumes["node-dns"], "hostPath"); hp["path"] != version.CoreDNSUpstreamDir || hp["type"] != "DirectoryOrCreate" {
		t.Errorf("node-dns volume = %v, want hostPath %s, DirectoryOrCreate: the directory, so a renamed-in file shows, and created when the node has not", volumes["node-dns"], version.CoreDNSUpstreamDir)
	}
	if _, ok := volumes["upstream"]["emptyDir"]; !ok {
		t.Errorf("upstream volume = %v, want an emptyDir", volumes["upstream"])
	}

	mounts := func(c map[string]any) map[string]string {
		got := map[string]string{}
		ms, _, _ := unstructured.NestedSlice(c, "volumeMounts")
		for _, m := range ms {
			mm := m.(map[string]any)
			path := mm["mountPath"].(string)
			if mm["readOnly"] == true {
				path += " ro"
			}
			got[mm["name"].(string)] = path
		}
		return got
	}
	cs, _, _ := unstructured.NestedSlice(spec, "containers")
	if got := mounts(cs[0].(map[string]any))["upstream"]; got != "/etc/coredns-upstream ro" {
		t.Errorf("coredns mounts upstream at %q, want /etc/coredns-upstream ro", got)
	}
	ics, _, _ := unstructured.NestedSlice(spec, "initContainers")
	if len(ics) != 1 {
		t.Fatalf("coredns has %d init containers, want the upstream-resolv one", len(ics))
	}
	ic := ics[0].(map[string]any)
	if ic["image"] != version.NsenterImage {
		t.Errorf("upstream-resolv image = %v, want the payload's busybox %s", ic["image"], version.NsenterImage)
	}
	if got := mounts(ic); got["node-dns"] != "/node-dns ro" || got["node-systemd-resolve"] != "/node-systemd-resolve ro" || got["upstream"] != "/upstream" {
		t.Errorf("upstream-resolv mounts = %v, want node-dns and node-systemd-resolve read-only and upstream at /upstream", got)
	}
	if got, _, _ := unstructured.NestedString(volumes["node-systemd-resolve"], "hostPath", "path"); got != "/run/systemd/resolve" {
		t.Errorf("node-systemd-resolve hostPath = %q, want /run/systemd/resolve", got)
	}
	sc, _, _ := unstructured.NestedMap(ic, "securityContext")
	drop, _, _ := unstructured.NestedStringSlice(sc, "capabilities", "drop")
	seccomp, _, _ := unstructured.NestedString(sc, "seccompProfile", "type")
	if sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false || sc["readOnlyRootFilesystem"] != true || !slices.Equal(drop, []string{"ALL"}) || seccomp != "RuntimeDefault" {
		t.Errorf("upstream-resolv securityContext = %v, want non-root, no privilege escalation, a read-only root, ALL dropped, RuntimeDefault", sc)
	}
	for _, k := range []string{"requests", "limits"} {
		if r, _, _ := unstructured.NestedStringMap(ic, "resources", k); r["cpu"] == "" || r["memory"] == "" {
			t.Errorf("upstream-resolv resources.%s = %v, want cpu and memory", k, r)
		}
	}
}

// TestCoreDNSUpstreamScript runs the init container's script against stand-in
// paths. The node's file wins when it names a nameserver. Without one (a node
// not yet upgraded, or racing its first start) the script makes the node's
// choice itself, by internal/runtime.chooseCoreDNSUpstream's rules: the pod's
// resolv.conf with the nameservers a pod cannot reach left out, else the
// node's systemd-resolved upstream file, else the public pair, keeping the
// chosen file's search, domain and options lines. A loopback resolver is never
// copied through: from inside the pod it is CoreDNS itself, and the loop
// plugin would stop CoreDNS on every node that had not written its choice.
func TestCoreDNSUpstreamScript(t *testing.T) {
	ics, _, _ := unstructured.NestedSlice(coreDNSDeployment(t).Object, "spec", "template", "spec", "initContainers")
	cmd, _, _ := unstructured.NestedStringSlice(ics[0].(map[string]any), "command")
	if len(cmd) != 3 || cmd[0] != "/bin/sh" || cmd[1] != "-c" {
		t.Fatalf("upstream-resolv command = %q, want /bin/sh -c <script>", cmd)
	}
	const podPlain = "nameserver 192.168.104.2\n"
	const podStub = "nameserver 127.0.0.53\nsearch corp.example\noptions edns0 trust-ad\n"
	for name, tc := range map[string]struct {
		node, pod, systemd string
		want               string
		warns              bool
		ipv6               bool
	}{
		"the node chose an upstream":      {node: "nameserver 10.60.0.2\n", pod: podPlain, want: "nameserver 10.60.0.2\n"},
		"the node wrote none":             {pod: podPlain, want: podPlain},
		"the node's file names no server": {node: "search corp.example\n", pod: podPlain, want: podPlain},
		"the node's file is the loopback stub and systemd-resolved knows the upstream": {
			pod: podStub, systemd: "nameserver 10.10.1.1\nnameserver fe80::1%eth0\nsearch corp.example\n",
			want: "nameserver 10.10.1.1\nsearch corp.example\n",
		},
		"the node's file mixes the stub with a reachable resolver": {
			pod:  "nameserver 127.0.0.53\nnameserver 10.0.0.2\noptions ndots:2\n",
			want: "nameserver 10.0.0.2\noptions ndots:2\n",
		},
		"a nameserver without an address falls through to systemd-resolved": {
			pod: "nameserver\n", systemd: "nameserver 10.0.0.2\n",
			want: "nameserver 10.0.0.2\n",
		},
		"equivalent loopback and mapped unspecified addresses fall through": {
			pod:     "nameserver 0:0:0:0:0:0:0:1\nnameserver 0:0:0:0:0:0:0:0\nnameserver ::ffff:127.0.0.1\nnameserver 0:0:0:0:0:ffff:7f00:1\nnameserver ::ffff:0.0.0.0\n",
			systemd: "nameserver 10.0.0.2\n", want: "nameserver 10.0.0.2\n", ipv6: true,
		},
		"invalid addresses fall through": {
			pod:     "nameserver not-an-address\nnameserver 999.999.999.999\nnameserver 2001:db8:::53\nnameserver ::a1.2.3.4\n",
			systemd: "nameserver 10.0.0.2\n", want: "nameserver 10.0.0.2\n", ipv6: true,
		},
		"an IPv4-only pod skips a native IPv6 resolver": {
			pod: "nameserver 2001:db8::53\n", systemd: "nameserver 10.0.0.2\n",
			want: "nameserver 10.0.0.2\n",
		},
		"an IPv4-only pod with only IPv6 resolvers uses the public fallback": {
			pod: "nameserver 2001:db8::53\n", systemd: "nameserver 2001:db8::54\n",
			want: "nameserver 8.8.8.8\nnameserver 1.1.1.1\n", warns: true,
		},
		"a dual-stack pod keeps a native IPv6 resolver": {
			pod: "nameserver 2001:db8::53\n", want: "nameserver 2001:db8::53\n", ipv6: true,
		},
		"an IPv4-only pod keeps a reachable mapped IPv4 resolver": {
			pod: "nameserver ::ffff:192.0.2.53\n", want: "nameserver ::ffff:192.0.2.53\n",
		},
		"nothing on the node names a reachable resolver": {
			pod: podStub, systemd: "nameserver ::1\nnameserver 0.0.0.0\n",
			want:  "nameserver 8.8.8.8\nnameserver 1.1.1.1\nsearch corp.example\noptions edns0 trust-ad\n",
			warns: true,
		},
		"no systemd-resolved at all": {
			pod: "nameserver 127.0.0.1\n", want: "nameserver 8.8.8.8\nnameserver 1.1.1.1\n", warns: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			node, systemd, up, pod := filepath.Join(dir, "node-dns"), filepath.Join(dir, "systemd-resolve"), filepath.Join(dir, "upstream"), filepath.Join(dir, "pod-resolv.conf")
			procIPv6 := filepath.Join(dir, "if_inet6")
			for _, d := range []string{node, systemd, up} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(pod, []byte(tc.pod), 0o644); err != nil {
				t.Fatal(err)
			}
			addr := "00000000000000000000000000000001 01 80 10 80 lo\n"
			if tc.ipv6 {
				addr += "fd000000000000000000000000000002 02 40 00 80 eth0\n"
			}
			if err := os.WriteFile(procIPv6, []byte(addr), 0o644); err != nil {
				t.Fatal(err)
			}
			for file, body := range map[string]string{filepath.Join(node, "resolv.conf"): tc.node, filepath.Join(systemd, "resolv.conf"): tc.systemd} {
				if body == "" {
					continue
				}
				if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			script := strings.NewReplacer("/node-dns", node, "/node-systemd-resolve", systemd, "/upstream", up, "/etc/resolv.conf", pod, "/proc/net/if_inet6", procIPv6).Replace(cmd[2])
			out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("script: %v: %s", err, out)
			}
			if warned := strings.Contains(string(out), "8.8.8.8"); warned != tc.warns {
				t.Errorf("script output = %q; warned about the public fallback = %v, want %v", out, warned, tc.warns)
			}
			got, err := os.ReadFile(filepath.Join(up, "resolv.conf"))
			if err != nil || string(got) != tc.want {
				t.Errorf("upstream resolv.conf = %q, %v; want %q", got, err, tc.want)
			}
			if strings.Contains(string(got), "127.") || strings.Contains(string(got), "::1") {
				t.Errorf("upstream resolv.conf %q forwards to a loopback address, which is CoreDNS itself", got)
			}
		})
	}
}

// The Corefile's served zone is the cluster-domain sentinel: the staging does
// not bake a domain in (the cluster's domain is an apply-time input), and no
// literal zone survives the render.
func TestCoreDNSServesTheClusterDomainSentinel(t *testing.T) {
	m := string(RenderCoreDNS())
	if !strings.Contains(m, "kubernetes "+ClusterDomainSentinel+" in-addr.arpa ip6.arpa {") {
		t.Error("the staged Corefile does not carry the cluster-domain sentinel where the served zone goes")
	}
	if strings.Contains(m, "cluster.local") {
		t.Error("the staged render still carries cluster.local")
	}
}

package addons

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func ciliumConfigData(t *testing.T, m []byte) map[string]string {
	t.Helper()
	cm := findObject(t, decodeAll(t, m), "ConfigMap", "cilium-config")
	data, _, err := unstructured.NestedStringMap(cm.Object, "data")
	if err != nil {
		t.Fatalf("read cilium-config data: %v", err)
	}
	return data
}

func podChecksum(t *testing.T, m []byte, kind, name string) string {
	t.Helper()
	o := findObject(t, decodeAll(t, m), kind, name)
	sum, _, err := unstructured.NestedString(o.Object, "spec", "template", "metadata", "annotations", ciliumConfigmapChecksumName)
	if err != nil {
		t.Fatalf("read the %s %s checksum: %v", kind, name, err)
	}
	return sum
}

// TestCiliumDefaultSettingsRenderTheShippedManifest: a cluster with no
// spec.network.cilium renders the manifest as generated plus the BGP secrets
// Role and RoleBinding, granting nothing, and the agents keep the shipped
// checksum, so they do not restart.
func TestCiliumDefaultSettingsRenderTheShippedManifest(t *testing.T) {
	m := RenderCilium(false, CiliumSettings{})
	shipped := pinCiliumOperatorToOneReplica(bytes.ReplaceAll(ciliumKPRTrueYAML, []byte(registrySentinel), []byte(ImageRegistry)))
	if want := appendCiliumBGPSecretsRBAC(shipped, false); !bytes.Equal(m, want) {
		t.Fatal("the default settings changed the rendered manifest beyond the BGP secrets Role and RoleBinding")
	}
	if got, want := podChecksum(t, m, "DaemonSet", "cilium"), podChecksum(t, shipped, "DaemonSet", "cilium"); got != want {
		t.Errorf("default settings changed the agents' checksum from %q to %q, restarting them", want, got)
	}
}

// TestCiliumSettingsReachCiliumConfig: each setting sets its own key and
// leaves the others as shipped.
func TestCiliumSettingsReachCiliumConfig(t *testing.T) {
	shipped := ciliumConfigData(t, RenderCilium(false, CiliumSettings{}))
	for _, tc := range []struct {
		name string
		s    CiliumSettings
		want map[string]string
	}{
		{"mtu", CiliumSettings{MTU: 9000}, map[string]string{"mtu": "9000"}},
		{"devices", CiliumSettings{Devices: []string{"fe-nic+", "fblo"}}, map[string]string{"devices": "fe-nic+ fblo"}},
		{"bgp", CiliumSettings{BGPControlPlane: true}, map[string]string{"enable-bgp-control-plane": "true",
			"bgp-secrets-namespace": "kube-system", "enable-bgp-control-plane-status-report": "true",
			"bgp-router-id-allocation-mode": "default", "bgp-router-id-allocation-ip-pool": "",
			"enable-bgp-legacy-origin-attribute": "false"}},
		{"native", CiliumSettings{NativeRouting: true, IPv4NativeRoutingCIDR: "10.245.0.0/16"},
			map[string]string{"routing-mode": "native", "ipv4-native-routing-cidr": "10.245.0.0/16"}},
		{"native dual-stack with direct routes", CiliumSettings{NativeRouting: true, IPv4NativeRoutingCIDR: "10.0.0.0/8",
			IPv6NativeRoutingCIDR: "fd00::/48", AutoDirectNodeRoutes: true},
			map[string]string{"routing-mode": "native", "ipv4-native-routing-cidr": "10.0.0.0/8",
				"ipv6-native-routing-cidr": "fd00::/48", "auto-direct-node-routes": "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ciliumConfigData(t, RenderCilium(false, tc.s))
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
			for k, v := range shipped {
				if _, changed := tc.want[k]; !changed && got[k] != v {
					t.Errorf("%s changed from %q to %q, though the settings do not touch it", k, v, got[k])
				}
			}
			for k := range got {
				if _, ok := shipped[k]; !ok {
					if _, wanted := tc.want[k]; !wanted {
						t.Errorf("added %s, which the settings do not set", k)
					}
				}
			}
		})
	}
}

// TestCiliumSettingsRollTheAgentsAndTheOperator: both read cilium-config only
// at start, so any change to the settings must change both pod templates.
func TestCiliumSettingsRollTheAgentsAndTheOperator(t *testing.T) {
	shipped := RenderCilium(false, CiliumSettings{})
	mtu := RenderCilium(false, CiliumSettings{MTU: 9000})
	bgp := RenderCilium(false, CiliumSettings{BGPControlPlane: true})
	base := podChecksum(t, shipped, "DaemonSet", "cilium")
	for name, m := range map[string][]byte{"mtu": mtu, "bgp": bgp} {
		agent, operator := podChecksum(t, m, "DaemonSet", "cilium"), podChecksum(t, m, "Deployment", "cilium-operator")
		if agent == base || len(agent) != 64 {
			t.Errorf("%s: agent checksum %q, want a new sha256 hex", name, agent)
		}
		if operator != agent {
			t.Errorf("%s: operator checksum %q, want the agent's %q", name, operator, agent)
		}
	}
	if podChecksum(t, mtu, "DaemonSet", "cilium") == podChecksum(t, bgp, "DaemonSet", "cilium") {
		t.Error("two different settings rendered the same checksum")
	}
	if !bytes.Equal(RenderCilium(false, CiliumSettings{MTU: 9000}), mtu) {
		t.Error("the same settings rendered two different manifests")
	}
}

func TestCiliumSettingsEncodeRoundTrips(t *testing.T) {
	s := CiliumSettings{MTU: 9216, Devices: []string{"a+", "b"}, BGPControlPlane: true}
	got, err := DecodeCiliumSettings(s.Encode())
	if err != nil || !bytes.Equal(got.Encode(), s.Encode()) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if def, err := DecodeCiliumSettings(nil); err != nil || !def.IsDefault() {
		t.Errorf("empty input = %+v, %v; want the shipped settings", def, err)
	}
	for _, in := range []string{`{"futureSetting":true}`, `{}{"futureSetting":true}`, `{"mtu":9000} {}`, `{"mtu":9000}x`, `{"mtu":9000}]`} {
		if got, err := DecodeCiliumSettings([]byte(in)); err == nil {
			t.Errorf("%s decoded as %+v; it must be refused", in, got)
		}
	}
	if _, err := DecodeCiliumSettings([]byte(`{"mtu":9000`)); err == nil || strings.Contains(err.Error(), "does not know") {
		t.Errorf("a truncated record: %v, want a decode error that does not blame a setting this build lacks", err)
	}
	if got, err := DecodeCiliumSettings([]byte("{\"mtu\":9000}\n")); err != nil || got.MTU != 9000 {
		t.Errorf("one value with a trailing newline = %+v, %v", got, err)
	}
}

// TestCiliumBGPReadsPeerAuthSecrets: with the BGP control plane on, the agent
// can read the kube-system Secrets peers authenticate with, as Cilium's chart
// renders it; with it off the same Role is rendered granting nothing, so the
// operator's apply takes the access away whichever control plane leads.
func TestCiliumBGPReadsPeerAuthSecrets(t *testing.T) {
	rules := func(m []byte) []any {
		r, _, _ := unstructured.NestedSlice(findObject(t, decodeAll(t, m), "Role", "cilium-bgp-control-plane-secrets").Object, "rules")
		return r
	}
	if r := rules(RenderCilium(false, CiliumSettings{MTU: 9000})); len(r) != 0 {
		t.Errorf("with the BGP control plane off the Role grants %v, want nothing", r)
	}
	on := RenderCilium(false, CiliumSettings{BGPControlPlane: true})
	wantRules := []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "verbs": []any{"get", "list", "watch"}}}
	if r := rules(on); !reflect.DeepEqual(r, wantRules) {
		t.Errorf("Role rules = %v, want %v", r, wantRules)
	}
	rb := findObject(t, decodeAll(t, on), "RoleBinding", "cilium-bgp-control-plane-secrets")
	if rb.GetNamespace() != "kube-system" {
		t.Errorf("RoleBinding namespace = %q", rb.GetNamespace())
	}
	roleRef, _, _ := unstructured.NestedMap(rb.Object, "roleRef")
	if want := map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "cilium-bgp-control-plane-secrets"}; !reflect.DeepEqual(roleRef, want) {
		t.Errorf("RoleBinding roleRef = %v, want %v", roleRef, want)
	}
	subjects, _, _ := unstructured.NestedSlice(rb.Object, "subjects")
	if want := []any{map[string]any{"kind": "ServiceAccount", "name": "cilium", "namespace": "kube-system"}}; !reflect.DeepEqual(subjects, want) {
		t.Errorf("RoleBinding subjects = %v, want %v", subjects, want)
	}
}

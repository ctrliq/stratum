package addons

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// CiliumSettings are the cluster's Cilium options; the zero value ships.
type CiliumSettings struct {
	MTU                   int      `json:"mtu,omitempty"`
	Devices               []string `json:"devices,omitempty"`
	BGPControlPlane       bool     `json:"bgpControlPlane,omitempty"`
	NativeRouting         bool     `json:"nativeRouting,omitempty"`
	IPv4NativeRoutingCIDR string   `json:"ipv4NativeRoutingCIDR,omitempty"`
	IPv6NativeRoutingCIDR string   `json:"ipv6NativeRoutingCIDR,omitempty"`
	AutoDirectNodeRoutes  bool     `json:"autoDirectNodeRoutes,omitempty"`
}

func (s CiliumSettings) IsDefault() bool {
	return s.MTU == 0 && len(s.Devices) == 0 && !s.BGPControlPlane && !s.NativeRouting &&
		s.IPv4NativeRoutingCIDR == "" && s.IPv6NativeRoutingCIDR == "" && !s.AutoDirectNodeRoutes
}

func (s CiliumSettings) Encode() []byte {
	b, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("encode Cilium settings: %v", err))
	}
	return b
}

// DecodeCiliumSettings reads an Encode; empty input is the shipped configuration.
func DecodeCiliumSettings(b []byte) (CiliumSettings, error) {
	var s CiliumSettings
	if len(bytes.TrimSpace(b)) == 0 {
		return s, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field") {
			return CiliumSettings{}, fmt.Errorf("decode Cilium settings: %w: a setting this build does not know", err)
		}
		return CiliumSettings{}, fmt.Errorf("decode Cilium settings: %w", err)
	}
	switch err := dec.Decode(&json.RawMessage{}); {
	case err == nil:
		return CiliumSettings{}, errors.New("decode Cilium settings: more than one JSON value")
	case !errors.Is(err, io.EOF):
		return CiliumSettings{}, fmt.Errorf("decode Cilium settings: after the first value: %w", err)
	}
	return s, nil
}

// Each anchor must occur exactly once in the generated asset for targeted substitution.
const (
	ciliumRoutingModeAnchor     = "\n  routing-mode: \"tunnel\"\n"
	ciliumDevicesAnchor         = "\n  devices: \"\"\n"
	ciliumDirectRoutesAnchor    = "\n  auto-direct-node-routes: \"false\"\n"
	ciliumChecksumPrefix        = "\n        cilium.io/cilium-configmap-checksum: \""
	ciliumConfigmapChecksumName = "cilium.io/cilium-configmap-checksum"
)

// ciliumBGPConfig is the cilium-config the chart renders with the BGP control plane on.
const ciliumBGPConfig = `  enable-bgp-control-plane: "true"
  bgp-secrets-namespace: "kube-system"
  enable-bgp-control-plane-status-report: "true"
  bgp-router-id-allocation-mode: "default"
  bgp-router-id-allocation-ip-pool: ""
  enable-bgp-legacy-origin-attribute: "false"
`

// ciliumBGPSecretsRules is the access the chart's cilium-bgp-control-plane-secrets Role grants.
const ciliumBGPSecretsRules = `rules:
- apiGroups:
  - ""
  resources:
  - secrets
  verbs:
  - get
  - list
  - watch
`

// Rendered even with the BGP control plane off, so another leader's apply prunes the access.
func ciliumBGPSecretsRBAC(rules string) string {
	return `---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: cilium-bgp-control-plane-secrets
  namespace: "kube-system"
  labels:
    app.kubernetes.io/part-of: cilium
` + rules + `---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cilium-bgp-control-plane-secrets
  namespace: "kube-system"
  labels:
    app.kubernetes.io/part-of: cilium
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: cilium-bgp-control-plane-secrets
subjects:
- kind: ServiceAccount
  name: "cilium"
  namespace: kube-system
`
}

// appendCiliumBGPSecretsRBAC adds the BGP secrets RBAC; neither object changes the checksum.
func appendCiliumBGPSecretsRBAC(m []byte, bgp bool) []byte {
	rules := "rules: []\n"
	if bgp {
		rules = ciliumBGPSecretsRules
	}
	return append(bytes.TrimRight(m, "\n"), []byte("\n"+ciliumBGPSecretsRBAC(rules))...)
}

// applyCiliumSettings rewrites cilium-config and replaces the pod checksums, which roll the pods.
func applyCiliumSettings(m []byte, s CiliumSettings) []byte {
	if s.IsDefault() {
		return m
	}
	mode := "tunnel"
	if s.NativeRouting {
		mode = "native"
	}
	keys := fmt.Sprintf("\n  routing-mode: %q\n", mode)
	if s.MTU != 0 {
		keys += fmt.Sprintf("  mtu: \"%d\"\n", s.MTU)
	}
	if s.BGPControlPlane {
		keys += ciliumBGPConfig
	}
	if s.IPv4NativeRoutingCIDR != "" {
		keys += fmt.Sprintf("  ipv4-native-routing-cidr: %q\n", s.IPv4NativeRoutingCIDR)
	}
	if s.IPv6NativeRoutingCIDR != "" {
		keys += fmt.Sprintf("  ipv6-native-routing-cidr: %q\n", s.IPv6NativeRoutingCIDR)
	}
	m = bytes.Replace(m, []byte(ciliumRoutingModeAnchor), []byte(keys), 1)
	if len(s.Devices) > 0 {
		m = bytes.Replace(m, []byte(ciliumDevicesAnchor),
			[]byte(fmt.Sprintf("\n  devices: %q\n", strings.Join(s.Devices, " "))), 1)
	}
	if s.AutoDirectNodeRoutes {
		m = bytes.Replace(m, []byte(ciliumDirectRoutesAnchor), []byte("\n  auto-direct-node-routes: \"true\"\n"), 1)
	}
	return setCiliumChecksums(m, s)
}

func setCiliumChecksums(m []byte, s CiliumSettings) []byte {
	i := bytes.Index(m, []byte(ciliumChecksumPrefix))
	if i < 0 {
		return m
	}
	start := i + len(ciliumChecksumPrefix)
	end := bytes.IndexByte(m[start:], '"')
	if end < 0 {
		return m
	}
	shipped := m[start : start+end]
	h := sha256.Sum256(append(append(append([]byte{}, shipped...), '\n'), s.Encode()...))
	line := func(sum []byte) []byte {
		return append(append([]byte(ciliumChecksumPrefix), sum...), '"')
	}
	return bytes.ReplaceAll(m, line(shipped), line([]byte(hex.EncodeToString(h[:]))))
}

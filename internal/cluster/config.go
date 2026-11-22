// Package cluster assembles a SKE node: its config, state, and bring-up and join flows.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/boundedfile"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// The state types live in internal/nodestate, a leaf package shared with internal/ha.
type (
	NodeState = nodestate.NodeState
	VIP       = nodestate.VIP
)

var (
	SaveState = nodestate.SaveState
	LoadState = nodestate.LoadState
	StatePath = nodestate.StatePath
)

// ClusterConfig is the user-facing declarative config (/etc/stratum/config.yaml).
type ClusterConfig struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Spec       Spec   `json:"spec"`

	// path is the file LoadConfig read this from, even when it was missing.
	path string
}

type Spec struct {
	Network   Network    `json:"network,omitempty"`
	VIP       VIP        `json:"vip,omitempty"`
	Storage   Storage    `json:"storage,omitempty"`
	Operator  *bool      `json:"operator,omitempty"`  // HelmChart + manifest-deployer controller (default on)
	Profile   string     `json:"profile,omitempty"`   // the node profile, by name; "" for none
	CIS       bool       `json:"cis,omitempty"`       // legacy spelling of profile: cis; LoadConfig folds it into Profile
	Snapshot  Snapshot   `json:"snapshots,omitempty"` //
	CertRenew *bool      `json:"certRenew,omitempty"` // rotate expiring leaves automatically (default on)
	SELinux   *bool      `json:"selinux,omitempty"`   // label Stratum's files for container-selinux (default on)
	Dashboard *Dashboard `json:"dashboard,omitempty"`
	Node      NodeSpec   `json:"node,omitempty"`
	// Read by control planes only.
	RuntimeClasses []string `json:"runtimeClasses,omitempty"`
}

// Dashboard is spec.dashboard: on by default whenever the operator is.
type Dashboard struct {
	Enabled         *bool  `json:"enabled,omitempty"`
	NodePort        *int   `json:"nodePort,omitempty"` // default 30443; 0 serves on a ClusterIP only
	PublicURL       string `json:"publicURL,omitempty"`
	TLSSecretName   string `json:"tlsSecretName,omitempty"`
	EnableNodeShell *bool  `json:"enableNodeShell,omitempty"` // the privileged host shell (default on, off under cis)
	TokenSecrets    *bool  `json:"tokenSecrets,omitempty"`    // admin and viewer sign-in tokens as Secrets (default on)
}

func (d *Dashboard) validate(operator bool) error {
	if d == nil {
		return nil
	}
	if d.Enabled != nil && *d.Enabled && !operator {
		return errors.New("enabled needs the operator, which keeps its certificate; spec.operator is false")
	}
	if p := d.NodePort; p != nil && *p != 0 && (*p < 30000 || *p > 32767) {
		return fmt.Errorf("nodePort %d is out of the NodePort range 30000-32767 (0 serves on a ClusterIP only)", *p)
	}
	if d.PublicURL != "" {
		u, err := url.Parse(d.PublicURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return fmt.Errorf("publicURL %q is not an https://host[:port] URL (the dashboard always serves TLS)", d.PublicURL)
		}
	}
	if n := d.TLSSecretName; n != "" && len(validation.IsDNS1123Subdomain(n)) > 0 {
		return fmt.Errorf("tlsSecretName %q is not a Secret name: %s", n, strings.Join(validation.IsDNS1123Subdomain(n), "; "))
	}
	return nil
}

// A control plane records the ranges at first boot and inherits them; a worker reads only nodeIP.
type Network struct {
	PodCIDR       string  `json:"podCIDR,omitempty"`     // default 10.245.0.0/16; "v4,v6" for dual-stack; set at first boot, a join must match
	ServiceCIDR   string  `json:"serviceCIDR,omitempty"` // default 10.96.0.0/12; set at first boot, a join must match
	ClusterDomain string  `json:"clusterDomain,omitempty"`
	NodeIP        string  `json:"nodeIP,omitempty"` // override autodetection
	Cilium        *Cilium `json:"cilium,omitempty"`
}

// Cluster-wide, so every control plane's file must carry the same block.
type Cilium struct {
	MTU                   int      `json:"mtu,omitempty"`
	Devices               []string `json:"devices,omitempty"`
	BGPControlPlane       *bool    `json:"bgpControlPlane,omitempty"`
	RoutingMode           string   `json:"routingMode,omitempty"` // tunnel (default) or native
	IPv4NativeRoutingCIDR string   `json:"ipv4NativeRoutingCIDR,omitempty"`
	IPv6NativeRoutingCIDR string   `json:"ipv6NativeRoutingCIDR,omitempty"`
	AutoDirectNodeRoutes  *bool    `json:"autoDirectNodeRoutes,omitempty"`
}

// Settings is the block as the render and the join check take it; nil is the shipped config.
func (c *Cilium) Settings() addons.CiliumSettings {
	if c == nil {
		return addons.CiliumSettings{}
	}
	return addons.CiliumSettings{
		MTU:                   c.MTU,
		Devices:               c.Devices,
		BGPControlPlane:       c.BGPControlPlane != nil && *c.BGPControlPlane,
		NativeRouting:         c.RoutingMode == "native",
		IPv4NativeRoutingCIDR: c.IPv4NativeRoutingCIDR,
		IPv6NativeRoutingCIDR: c.IPv6NativeRoutingCIDR,
		AutoDirectNodeRoutes:  c.AutoDirectNodeRoutes != nil && *c.AutoDirectNodeRoutes,
	}
}

// An interface name with Cilium's '+' wildcard; nothing that can break out of the ConfigMap value.
var ciliumDevice = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}\+?$`)

// Dual-stack needs the resolved network, so CheckStack does that part of the check.
func (c *Cilium) validate() error {
	if c == nil {
		return nil
	}
	if c.MTU != 0 && (c.MTU < 1280 || c.MTU > 65535) {
		return fmt.Errorf("mtu %d is out of range 1280-65535", c.MTU)
	}
	seen := map[string]bool{}
	for _, d := range c.Devices {
		if !ciliumDevice.MatchString(d) || d == "." || d == ".." {
			return fmt.Errorf("devices: %q is not an interface name (up to 15 of A-Z a-z 0-9 _ . -, with an optional trailing + wildcard)", d)
		}
		if seen[d] {
			return fmt.Errorf("devices: %q is listed twice", d)
		}
		seen[d] = true
	}
	native := false
	switch c.RoutingMode {
	case "", "tunnel":
	case "native":
		native = true
	default:
		return fmt.Errorf("routingMode %q is not tunnel or native", c.RoutingMode)
	}
	if !native {
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"ipv4NativeRoutingCIDR", c.IPv4NativeRoutingCIDR != ""},
			{"ipv6NativeRoutingCIDR", c.IPv6NativeRoutingCIDR != ""},
			{"autoDirectNodeRoutes", c.AutoDirectNodeRoutes != nil},
		} {
			if f.set {
				return fmt.Errorf("%s applies only to routingMode: native", f.name)
			}
		}
		return nil
	}
	if c.IPv4NativeRoutingCIDR == "" {
		return errors.New("ipv4NativeRoutingCIDR is required with routingMode: native: the range pods reach without masquerading, usually the pod CIDR or the fabric's")
	}
	if err := checkFamilyCIDR(c.IPv4NativeRoutingCIDR, false); err != nil {
		return fmt.Errorf("ipv4NativeRoutingCIDR: %w", err)
	}
	if c.IPv6NativeRoutingCIDR != "" {
		if err := checkFamilyCIDR(c.IPv6NativeRoutingCIDR, true); err != nil {
			return fmt.Errorf("ipv6NativeRoutingCIDR: %w", err)
		}
	}
	return nil
}

func checkFamilyCIDR(s string, v6 bool) error {
	ip, _, err := net.ParseCIDR(s)
	if err != nil {
		return fmt.Errorf("%q is not a CIDR", s)
	}
	if (ip.To4() == nil) != v6 {
		family := "IPv4"
		if v6 {
			family = "IPv6"
		}
		return fmt.Errorf("%q is not an %s CIDR", s, family)
	}
	return nil
}

// Native routing on dual-stack needs the IPv6 range; a single-stack one has no use for it.
func (c *Cilium) CheckStack(dualStack bool) error {
	if c == nil || c.RoutingMode != "native" {
		return nil
	}
	switch {
	case dualStack && c.IPv6NativeRoutingCIDR == "":
		return errors.New("spec.network.cilium.ipv6NativeRoutingCIDR is required with routingMode: native on a dual-stack cluster")
	case !dualStack && c.IPv6NativeRoutingCIDR != "":
		return errors.New("spec.network.cilium.ipv6NativeRoutingCIDR is set, but the cluster is single-stack IPv4")
	}
	return nil
}

type Storage struct {
	// Default StorageClass backend: "local-path" (default) or "none".
	Default string `json:"default,omitempty"`
}

type Snapshot struct {
	Disabled      bool   `json:"disabled,omitempty"`
	IntervalHours int    `json:"intervalHours,omitempty"` // default 12
	Retain        int    `json:"retain,omitempty"`        // default 5
	S3            *S3    `json:"s3,omitempty"`
	Dir           string `json:"dir,omitempty"`
}

type S3 struct {
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix,omitempty"`
	Region   string `json:"region,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// Path-style addressing, which MinIO, Ceph and most non-AWS endpoints need.
	PathStyle bool `json:"pathStyle,omitzero"`
}

// DefaultConfigPath is the declarative config file `stratum up` reads.
const DefaultConfigPath = "/etc/stratum/config.yaml"

// LoadConfig reads the cluster config; a missing file yields pure defaults.
func LoadConfig(path string) (*ClusterConfig, error) {
	return loadConfig(path, os.ReadFile)
}

// LoadConfigContext uses the same parser and defaults with bounded metadata I/O.
func LoadConfigContext(ctx context.Context, path string) (*ClusterConfig, error) {
	return loadConfig(path, func(path string) ([]byte, error) { return boundedfile.Read(ctx, path) })
}

func loadConfig(path string, read func(string) ([]byte, error)) (*ClusterConfig, error) {
	c, _, err := parseConfigWithRead(path, read)
	if err != nil {
		return nil, err
	}
	if err := c.validateNode(); err != nil {
		return nil, err
	}
	if err := validateRuntimeClasses(c.Spec.RuntimeClasses); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.defaults()
	if err := c.resolveProfile(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Unset stays empty: PrepareController tells "not set" from "set to cluster.local".
	if d := c.Spec.Network.ClusterDomain; d != "" {
		if err := netcfg.ValidateDomain(d); err != nil {
			return nil, fmt.Errorf("%s: spec.network.clusterDomain: %w", path, err)
		}
	}
	return c, nil
}

// WorkerConfig is what a --worker reads from its config file.
type WorkerConfig struct {
	NodeIP        string   // spec.network.nodeIP
	SELinuxOptOut bool     // spec.selinux: false
	Node          NodeSpec // spec.node
}

// workerFields are the fields a --worker honours: a leaf, or a map whose every entry it honours.
var workerFields = []string{"spec.network.nodeIP", "spec.selinux", "spec.node.labels", "spec.node.taints", "spec.node.runtimeHandlers"}

func workerField(p string) bool {
	return slices.ContainsFunc(workerFields, func(f string) bool {
		return strings.EqualFold(p, f) || len(p) > len(f) && strings.EqualFold(p[:len(f)+1], f+".")
	})
}

// set is false when the file is missing or does not set spec.selinux.
func WorkerSELinux(path string) (optOut, set bool, err error) {
	c, raw, err := parseConfig(path)
	if err != nil || raw == nil {
		return false, false, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return false, false, fmt.Errorf("%s: %w", path, err)
	}
	for k, v := range doc {
		if !strings.EqualFold(k, "spec") {
			continue
		}
		for _, p := range presentFields(v, "spec") {
			if strings.EqualFold(p, "spec.selinux") {
				return c.SELinuxOptOut(), true, nil
			}
		}
	}
	return false, false, nil
}

// Read on a worker restart only to refuse a profile other than the one it joined with.
func WorkerProfile(path string) (string, error) {
	c, _, err := parseConfig(path)
	if err != nil {
		return "", err
	}
	if err := c.resolveProfile(); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return c.ProfileName(), nil
}

// Any other set field is an error naming it; a missing file yields the zero WorkerConfig.
func LoadWorkerConfig(path string) (WorkerConfig, error) {
	c, raw, err := parseConfig(path)
	if err != nil {
		return WorkerConfig{}, err
	}
	// Presence is read off the document: zero values still decode from settings the operator wrote.
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return WorkerConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	var set []string
	// Every key matches case-insensitively, the root included, as in decoding.
	for k, v := range doc {
		if !strings.EqualFold(k, "spec") {
			continue
		}
		for _, p := range presentFields(v, "spec") {
			if !workerField(p) {
				set = append(set, p)
			}
		}
	}
	if len(set) > 0 {
		slices.Sort(set)
		return WorkerConfig{}, fmt.Errorf("%s: %s set, but a --worker reads only spec.network.nodeIP, spec.selinux and spec.node from it; remove them from this node's file (pod and service CIDRs, the cluster domain and --profile are worker flags)",
			path, strings.Join(set, ", "))
	}
	if err := c.validateNode(); err != nil {
		return WorkerConfig{}, err
	}
	return WorkerConfig{NodeIP: c.Spec.Network.NodeIP, SELinuxOptOut: c.SELinuxOptOut(), Node: c.Spec.Node}, nil
}

// presentFields lists the dotted paths of the leaves a decoded document sets.
func presentFields(v any, path string) []string {
	switch v := v.(type) {
	case nil:
		return nil
	case map[string]any:
		var out []string
		for k, e := range v {
			out = append(out, presentFields(e, path+"."+k)...)
		}
		return out
	default:
		return []string{path}
	}
}

// Checked where spec.node is read, not in parseConfig: a worker restart reads only spec.selinux.
func (c *ClusterConfig) validateNode() error {
	if err := c.Spec.Node.Validate(); err != nil {
		return fmt.Errorf("%s: spec.node: %w", c.path, err)
	}
	return nil
}

func parseConfig(path string) (*ClusterConfig, []byte, error) {
	return parseConfigWithRead(path, os.ReadFile)
}

func parseConfigWithRead(path string, read func(string) ([]byte, error)) (*ClusterConfig, []byte, error) {
	c := &ClusterConfig{path: path}
	b, err := read(path)
	if os.IsNotExist(err) {
		return c, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if err := yaml.UnmarshalStrict(b, c); err != nil {
		return nil, nil, fmt.Errorf("%s: %w (the config is strict — a misspelled key must fail, not silently do nothing)", path, err)
	}
	if c.Kind != "" && c.Kind != "ClusterConfig" {
		return nil, nil, fmt.Errorf("%s: kind %q is not ClusterConfig", path, c.Kind)
	}
	// Refused here, as an error: a panic in defaults() would crash-loop the unit.
	if c.Spec.VIP.Address != "" && net.ParseIP(c.Spec.VIP.Address) == nil {
		return nil, nil, fmt.Errorf("%s: spec.vip.address %q is not an IP address", path, c.Spec.VIP.Address)
	}
	if err := c.Spec.Network.Cilium.validate(); err != nil {
		return nil, nil, fmt.Errorf("%s: spec.network.cilium.%w", path, err)
	}
	if err := c.Spec.Dashboard.validate(c.OperatorEnabled()); err != nil {
		return nil, nil, fmt.Errorf("%s: spec.dashboard.%w", path, err)
	}
	// time.NewTicker panics on a non-positive interval; the run group restart-loops the task forever.
	if c.Spec.Snapshot.IntervalHours < 0 {
		return nil, nil, fmt.Errorf("%s: spec.snapshots.intervalHours %d is negative", path, c.Spec.Snapshot.IntervalHours)
	}
	// The hours-to-duration product overflows, and the wrap is not reliably non-positive.
	const maxIntervalHours = math.MaxInt64 / int64(time.Hour)
	if h := int64(c.Spec.Snapshot.IntervalHours); h > maxIntervalHours {
		return nil, nil, fmt.Errorf("%s: spec.snapshots.intervalHours %d is too large", path, h)
	}
	// A negative retention would silently stop pruning until the disk fills.
	if c.Spec.Snapshot.Retain < 0 {
		return nil, nil, fmt.Errorf("%s: spec.snapshots.retain %d is negative", path, c.Spec.Snapshot.Retain)
	}
	return c, b, nil
}

func (c *ClusterConfig) defaults() {
	// resolveNetConfig tells "not set" from "set to the default" and applies the default there.
	if c.Spec.Storage.Default == "" {
		c.Spec.Storage.Default = "local-path"
	}
	if c.Spec.Snapshot.IntervalHours == 0 {
		c.Spec.Snapshot.IntervalHours = version.SnapshotIntervalHours
	}
	if c.Spec.Snapshot.Retain == 0 {
		c.Spec.Snapshot.Retain = version.SnapshotRetainDefault
	}
}

// ProfileName is spec.profile, or the legacy spec.cis: true as "cis"; empty for none.
func (c *ClusterConfig) ProfileName() string {
	if c.Spec.Profile != "" {
		return c.Spec.Profile
	}
	if c.Spec.CIS {
		return nodestate.CISProfile
	}
	return ""
}

// SetProfile selects a profile from outside the file (the --profile flag).
func (c *ClusterConfig) SetProfile(name string) {
	c.Spec.Profile = name
	c.Spec.CIS = name == nodestate.CISProfile
}

func (c *ClusterConfig) resolveProfile() error {
	if c.Spec.CIS && c.Spec.Profile != "" && c.Spec.Profile != nodestate.CISProfile {
		return fmt.Errorf("spec.profile %q and spec.cis: true name two profiles; keep spec.profile and remove spec.cis", c.Spec.Profile)
	}
	name := c.ProfileName()
	if name != "" {
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			return fmt.Errorf("spec.profile %q: a profile name is a DNS-1123 label: %s", name, strings.Join(errs, "; "))
		}
	}
	c.SetProfile(name)
	return nil
}

// A *bool distinguishes "unset" from "explicitly false"; both default to on.
func (c *ClusterConfig) OperatorEnabled() bool  { return c.Spec.Operator == nil || *c.Spec.Operator }
func (c *ClusterConfig) CertRenewEnabled() bool { return c.Spec.CertRenew == nil || *c.Spec.CertRenew }

func (c *ClusterConfig) DashboardEnabled() bool {
	if !c.OperatorEnabled() {
		return false
	}
	d := c.Spec.Dashboard
	return d == nil || d.Enabled == nil || *d.Enabled
}

// An unresolvable profile leaves the shell off, the safe side; such a node is refused at start.
func (c *ClusterConfig) nodeShellDefault() bool {
	p, err := profile.ForNode(c.ProfileName())
	return err == nil && !p.DisableNodeShell
}

// DashboardSettings is spec.dashboard as the render takes it.
func (c *ClusterConfig) DashboardSettings() addons.DashboardSettings {
	s := addons.DashboardSettings{NodePort: addons.DefaultDashboardPort, NodeShell: c.nodeShellDefault(), TokenSecrets: true}
	d := c.Spec.Dashboard
	if d == nil {
		return s
	}
	if d.TokenSecrets != nil {
		s.TokenSecrets = *d.TokenSecrets
	}
	if d.NodePort != nil {
		s.NodePort = *d.NodePort
	}
	if d.EnableNodeShell != nil {
		s.NodeShell = *d.EnableNodeShell
	}
	s.PublicURL, s.TLSSecretName = d.PublicURL, d.TLSSecretName
	return s
}

func (c *ClusterConfig) SELinuxOptOut() bool { return c.Spec.SELinux != nil && !*c.Spec.SELinux }

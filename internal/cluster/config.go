// Package cluster assembles a SKE node: the declarative cluster config, the
// persisted per-node state, the component processes the supervisor runs, and
// the bring-up/join flows around them.
//
// The shape is one API-versioned config file rather than flag soup, component
// configs generated from it, and the control plane as kubelet-managed static
// pods rendered by internal/staticpod, with the host-process roster in
// processes.go.
package cluster

import (
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
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// Aliases: the state types live in internal/nodestate (a leaf package shared
// with internal/ha); cluster is their primary consumer.
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
// CRD-shaped even though it is consumed locally, so the same document can
// later live in-cluster.
type ClusterConfig struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Spec       Spec   `json:"spec"`

	// path is the file LoadConfig read this from (even when it was missing and
	// the config is pure defaults).
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
}

// Dashboard is spec.dashboard: the dashboard the cluster runs for itself. It is
// on by default whenever the operator is.
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

// Network is spec.network. A control plane never reads its pod and service
// ranges or cluster domain from flags: the cluster's first controller boot
// takes podCIDR, serviceCIDR and clusterDomain from this block and records
// them, a joining control plane inherits the recorded values and refuses a
// different one set here, and every later start keeps the recorded values.
// A worker reads only nodeIP and takes the cluster's network from --pod-cidr,
// --service-cidr and --cluster-domain, which `stratum up` refuses without
// --worker.
type Network struct {
	PodCIDR       string  `json:"podCIDR,omitempty"`     // default 10.245.0.0/16; "v4,v6" for dual-stack; set at first boot, a join must match
	ServiceCIDR   string  `json:"serviceCIDR,omitempty"` // default 10.96.0.0/12; set at first boot, a join must match
	ClusterDomain string  `json:"clusterDomain,omitempty"`
	NodeIP        string  `json:"nodeIP,omitempty"` // override autodetection
	Cilium        *Cilium `json:"cilium,omitempty"`
}

// Cilium is spec.network.cilium: the cluster's Cilium options. Every field is
// optional, and an absent block is the shipped configuration. It is
// cluster-wide, so every control plane's file must carry the same block.
type Cilium struct {
	MTU                   int      `json:"mtu,omitempty"`
	Devices               []string `json:"devices,omitempty"`
	BGPControlPlane       *bool    `json:"bgpControlPlane,omitempty"`
	RoutingMode           string   `json:"routingMode,omitempty"` // tunnel (default) or native
	IPv4NativeRoutingCIDR string   `json:"ipv4NativeRoutingCIDR,omitempty"`
	IPv6NativeRoutingCIDR string   `json:"ipv6NativeRoutingCIDR,omitempty"`
	AutoDirectNodeRoutes  *bool    `json:"autoDirectNodeRoutes,omitempty"`
}

// Settings is the block as the render and the join check take it; a nil block
// is the shipped configuration.
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

// ciliumDevice is an interface name, with Cilium's trailing "+" wildcard
// allowed: what the kernel accepts as a name, nothing that could break out of
// the ConfigMap value.
var ciliumDevice = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}\+?$`)

// validate checks the block on its own; dual-stack needs the resolved network,
// so CheckStack does that part. Errors name the field under spec.network.cilium.
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

// CheckStack checks the parts of the block that depend on the cluster's
// network: native routing on a dual-stack cluster needs the IPv6 range too,
// and a single-stack one has no use for it.
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
	// PathStyle selects path-style addressing, which MinIO, Ceph and most
	// non-AWS endpoints need.
	PathStyle bool `json:"pathStyle,omitzero"`
}

// DefaultConfigPath is the declarative config file `stratum up` reads.
const DefaultConfigPath = "/etc/stratum/config.yaml"

// LoadConfig reads the cluster config; a missing file yields pure defaults, so
// `stratum up` with no config at all still brings up a sane single node.
func LoadConfig(path string) (*ClusterConfig, error) {
	c, _, err := parseConfig(path)
	if err != nil {
		return nil, err
	}
	c.defaults()
	if err := c.resolveProfile(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Unset stays empty: PrepareController tells "not set" from "set to
	// cluster.local" so a restart can keep the persisted domain.
	if d := c.Spec.Network.ClusterDomain; d != "" {
		if err := netcfg.ValidateDomain(d); err != nil {
			return nil, fmt.Errorf("%s: spec.network.clusterDomain: %w", path, err)
		}
	}
	return c, nil
}

// WorkerConfig is what a --worker reads from its config file.
type WorkerConfig struct {
	NodeIP        string // spec.network.nodeIP
	SELinuxOptOut bool   // spec.selinux: false
}

// workerFields are the fields a --worker honours.
var workerFields = []string{"spec.network.nodeIP", "spec.selinux"}

// WorkerSELinux reads spec.selinux from a worker's config file on a restart,
// which reads nothing else from it. set is false when the file is missing or
// does not set spec.selinux, and the worker keeps the setting it saved. Other
// fields are not checked here: a worker's join already refused a file that
// set them.
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

// LoadWorkerConfig is the config a --worker honours: spec.network.nodeIP and
// spec.selinux, and nothing else. The file is parsed as strictly as a control
// plane parses it, and any other field that is set is an error naming it: a
// worker has no use for it, and a setting that appears to apply and does not is
// worse than a refusal. A missing file yields the zero WorkerConfig.
func LoadWorkerConfig(path string) (WorkerConfig, error) {
	c, raw, err := parseConfig(path)
	if err != nil {
		return WorkerConfig{}, err
	}
	// Presence is read off the document, not the decoded struct: `operator: false`
	// and `snapshots.retain: 0` decode to the zero value, which looks the same as
	// absent, but they are still settings the operator wrote.
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return WorkerConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	var set []string
	// Every key matches case-insensitively, the root included, as in decoding:
	// `Spec:` and `spec:` both populate Spec, and may appear together.
	for k, v := range doc {
		if !strings.EqualFold(k, "spec") {
			continue
		}
		for _, p := range presentFields(v, "spec") {
			if !slices.ContainsFunc(workerFields, func(f string) bool { return strings.EqualFold(p, f) }) {
				set = append(set, p)
			}
		}
	}
	if len(set) > 0 {
		slices.Sort(set)
		return WorkerConfig{}, fmt.Errorf("%s: %s set, but a --worker reads only spec.network.nodeIP and spec.selinux from it; remove them from this node's file (pod and service CIDRs, the cluster domain and --profile are worker flags)",
			path, strings.Join(set, ", "))
	}
	return WorkerConfig{NodeIP: c.Spec.Network.NodeIP, SELinuxOptOut: c.SELinuxOptOut()}, nil
}

// presentFields lists the dotted paths of the leaves a decoded document sets.
// A null leaf and an empty mapping set nothing.
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

// parseConfig reads and strictly parses path without applying defaults, and
// returns the raw document with it. A missing file is an empty config.
func parseConfig(path string) (*ClusterConfig, []byte, error) {
	c := &ClusterConfig{path: path}
	b, err := os.ReadFile(path)
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
	// Caught here rather than deep in kube-vip's logs -- and as an error, like
	// every other bad value in this file. It was a panic in defaults(), which
	// has no error to return, so a typo crashed `stratum up` with a stack
	// trace and crash-looped the unit.
	if c.Spec.VIP.Address != "" && net.ParseIP(c.Spec.VIP.Address) == nil {
		return nil, nil, fmt.Errorf("%s: spec.vip.address %q is not an IP address", path, c.Spec.VIP.Address)
	}
	if err := c.Spec.Network.Cilium.validate(); err != nil {
		return nil, nil, fmt.Errorf("%s: spec.network.cilium.%w", path, err)
	}
	if err := c.Spec.Dashboard.validate(c.OperatorEnabled()); err != nil {
		return nil, nil, fmt.Errorf("%s: spec.dashboard.%w", path, err)
	}
	// Refused here, as an error like every other bad value in this file,
	// rather than passed through to the scheduled snapshot loop: its ticker
	// interval comes straight from this field, and time.NewTicker panics on
	// a non-positive one, taking the whole `stratum up` down. Zero is not a
	// bad value — it is the unset marker defaults() replaces with the
	// built-in interval.
	if c.Spec.Snapshot.IntervalHours < 0 {
		return nil, nil, fmt.Errorf("%s: spec.snapshots.intervalHours %d is negative", path, c.Spec.Snapshot.IntervalHours)
	}
	// The same panic, reached by overflow: the interval the loop's ticker
	// takes is time.Duration(hours) * time.Hour (run.go's snapshotInterval),
	// and that product wraps for hours above the duration's representable
	// range. The wrap is not reliably to a non-positive duration — a band of
	// wrapping values lands positive but tiny (5124096 hours, say, wraps to
	// about 25 minutes) — so the product's sign cannot be the check; the
	// refusal is a direct bound on the hours. The run group would recover the
	// panic and restart the task forever, so the refusal belongs here, at
	// load, with the negative one. Zero is exempt like it is above: it is the
	// unset marker defaults() replaces with the built-in interval.
	const maxIntervalHours = math.MaxInt64 / int64(time.Hour)
	if h := int64(c.Spec.Snapshot.IntervalHours); h > maxIntervalHours {
		return nil, nil, fmt.Errorf("%s: spec.snapshots.intervalHours %d is too large", path, h)
	}
	// A negative retention is refused at load too, though it fails quietly
	// rather than with a panic: the scheduled save prunes only for a positive
	// one, so on a control plane with no snapshot.conf a stray minus sign
	// would stop pruning until the disk fills. Zero is exempt here too: it is
	// the unset marker defaults() replaces with the built-in retention.
	if c.Spec.Snapshot.Retain < 0 {
		return nil, nil, fmt.Errorf("%s: spec.snapshots.retain %d is negative", path, c.Spec.Snapshot.Retain)
	}
	return c, b, nil
}

func (c *ClusterConfig) defaults() {
	// PodCIDR / ServiceCIDR / ClusterDomain stay empty when unset:
	// resolveNetConfig tells "not set" from "set to the default", and the default
	// is applied there.
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

// ProfileName is the node profile the config selects: spec.profile, or the
// legacy spec.cis: true as "cis"; empty for none.
func (c *ClusterConfig) ProfileName() string {
	if c.Spec.Profile != "" {
		return c.Spec.Profile
	}
	if c.Spec.CIS {
		return nodestate.CISProfile
	}
	return ""
}

// SetProfile selects a profile from outside the file (the --profile flag) and
// keeps spec.profile and the legacy spec.cis in step.
func (c *ClusterConfig) SetProfile(name string) {
	c.Spec.Profile = name
	c.Spec.CIS = name == nodestate.CISProfile
}

// resolveProfile folds the legacy spec.cis into spec.profile and refuses a
// file that names two different profiles or one that is not a label.
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

// OperatorEnabled/CertRenewEnabled default to on; a *bool distinguishes
// "unset" from "explicitly false".
func (c *ClusterConfig) OperatorEnabled() bool  { return c.Spec.Operator == nil || *c.Spec.Operator }
func (c *ClusterConfig) CertRenewEnabled() bool { return c.Spec.CertRenew == nil || *c.Spec.CertRenew }

// DashboardEnabled reports whether the cluster runs its dashboard: on unless
// spec.dashboard.enabled is false or the operator is off.
func (c *ClusterConfig) DashboardEnabled() bool {
	if !c.OperatorEnabled() {
		return false
	}
	d := c.Spec.Dashboard
	return d == nil || d.Enabled == nil || *d.Enabled
}

// nodeShellDefault is the dashboard node shell's default: on unless the
// node's profile turns it off. A profile that cannot be resolved leaves it off,
// the safe side; such a node is refused at start anyway.
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

// SELinuxOptOut reports spec.selinux: false.
func (c *ClusterConfig) SELinuxOptOut() bool { return c.Spec.SELinux != nil && !*c.Spec.SELinux }

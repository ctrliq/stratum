// Package registry translates an RKE2/k3s registries.yaml into containerd 2.x certs.d config.
package registry

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/version"
)

// Config is the RKE2/k3s registries.yaml schema; the field names match RKE2.
type Config struct {
	Mirrors map[string]Mirror    `json:"mirrors"`
	Configs map[string]RegConfig `json:"configs"`
}

// Mirror lists a registry's endpoints to try, in order, plus unsupported rewrite rules.
type Mirror struct {
	Endpoint []string          `json:"endpoint"`
	Rewrite  map[string]string `json:"rewrite"`
}

// RegConfig holds the auth and TLS of one `configs` host entry.
type RegConfig struct {
	Auth Auth `json:"auth"`
	TLS  TLS  `json:"tls"`
}

// Auth is registry credentials; Username/Password take precedence over the pre-encoded Auth.
type Auth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
}

// TLS is per-registry transport security for talking to a mirror endpoint.
type TLS struct {
	CAFile             string `json:"ca_file"`
	CertFile           string `json:"cert_file"`
	KeyFile            string `json:"key_file"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
}

// generatedMarker prefixes every file stratum writes, so prune leaves hand-authored ones.
const generatedMarker = version.GeneratedMarker + " (source: stratum registries.yaml)\n"

// NothingConfigured is the one shared sentence; a present file can still configure nothing.
const NothingConfigured = "no registry mirrors or credentials are configured; this node pulls from upstream registries directly"

// DefaultIsNotAFault is the reassurance the MCP notes append to NothingConfigured.
const DefaultIsNotAFault = "That is the default and not a fault."

// confdDropin is a func, not a const: version.CertsDir is a var, redirected in tests.
func confdDropin() string {
	return generatedMarker + "[plugins.'io.containerd.cri.v1.images'.registry]\n  config_path = '" + version.CertsDir + "'\n"
}

// Load parses registries.yaml; an absent file returns (nil, nil).
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// authHeader renders an `authorization` header value, or "" with no credentials.
func authHeader(a Auth) string {
	switch {
	case a.IdentityToken != "":
		return "Bearer " + a.IdentityToken
	case a.Auth != "":
		return "Basic " + a.Auth
	case a.Username != "" || a.Password != "":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(a.Username+":"+a.Password))
	}
	return ""
}

// EndpointHost strips scheme and path, matching RKE2's host[:port]-keyed `configs` lookup.
func EndpointHost(endpoint string) string {
	h := endpoint
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	return h
}

// isLoopbackHost mirrors containerd's localhost special case, which the server line defeats.
func isLoopbackHost(reg string) bool {
	if h, _, err := net.SplitHostPort(reg); err == nil {
		reg = h
	}
	if reg == "localhost" {
		return true
	}
	if ip := net.ParseIP(reg); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// certsDirName maps RKE2's wildcard "*" to containerd's "_default"; it is never certs.d/*/.
func certsDirName(mirrorKey string) string {
	if mirrorKey == "*" {
		return "_default"
	}
	return mirrorKey
}

// WildcardCredentialsIgnored is the one copy of the warning for a configs."*" credential.
const WildcardCredentialsIgnored = `configs."*" auth and client certificate are not applied: ` +
	`the catch-all hosts.toml cannot name a server, and containerd turns TLS verification off ` +
	`for a loopback registry reached that way, so they could be sent to a local listener ` +
	`unverified; give each registry its own configs entry`

// Effective trims configs."*" to its CA and skip_verify: _default has no server.
func Effective(reg string, c RegConfig) (RegConfig, bool) {
	if reg != "*" {
		return c, false
	}
	dropped := authHeader(c.Auth) != "" || c.TLS.CertFile != "" || c.TLS.KeyFile != ""
	c.Auth = Auth{}
	c.TLS.CertFile, c.TLS.KeyFile = "", ""
	return c, dropped
}

// WildcardCredentialsDropped reports whether Effective trimmed configs."*".
func WildcardCredentialsDropped(c *Config) bool {
	if c == nil {
		return false
	}
	_, dropped := Effective("*", c.Configs["*"])
	return dropped
}

// Registries reports the mirror keys and configs keys with auth or TLS; a configs-only host counts.
func Registries(c *Config) []string {
	if c == nil {
		return nil
	}
	regs := make([]string, 0, len(c.Mirrors)+len(c.Configs))
	for k := range c.Mirrors {
		regs = append(regs, k)
	}
	for k, cfg := range c.Configs {
		cfg, _ = Effective(k, cfg)
		if _, mirrored := c.Mirrors[k]; !mirrored && hasAuthOrTLS(cfg) {
			regs = append(regs, k)
		}
	}
	sort.Strings(regs)
	return regs
}

// hasAuthOrTLS: `client` needs both cert and key, or hosts.toml would shadow _default.
func hasAuthOrTLS(c RegConfig) bool {
	return authHeader(c.Auth) != "" || c.TLS.CAFile != "" || c.TLS.InsecureSkipVerify || (c.TLS.CertFile != "" && c.TLS.KeyFile != "")
}

// insecureSkipVerifyWarning is one copy of the warning, for mirrors and configs-only alike.
func insecureSkipVerifyWarning(target string) string {
	return "insecure_skip_verify is set for " + target + " — TLS verification is DISABLED"
}

// hostsToml renders a certs.d hosts.toml for one registry.
func hostsToml(reg string, self RegConfig, cfgs map[string]RegConfig, m Mirror) (out string, secret bool, warnings []string) {
	var b strings.Builder
	b.WriteString(generatedMarker)
	// A TOML root key after the first table header lands inside that table.
	var root []string
	// containerd forces skip_verify on an implicit localhost host; an explicit server does not.
	if hasAuthOrTLS(self) && !self.TLS.InsecureSkipVerify && isLoopbackHost(reg) {
		server := "https://" + reg
		if ip := net.ParseIP(reg); ip != nil && strings.Contains(reg, ":") {
			server = "https://[" + reg + "]"
		}
		root = append(root, fmt.Sprintf("server = %q\n", server))
	}
	if self.TLS.CAFile != "" {
		root = append(root, fmt.Sprintf("ca = %q\n", self.TLS.CAFile))
	}
	if self.TLS.CertFile != "" && self.TLS.KeyFile != "" {
		root = append(root, fmt.Sprintf("client = [[%q, %q]]\n", self.TLS.CertFile, self.TLS.KeyFile))
	}
	if self.TLS.InsecureSkipVerify {
		root = append(root, "skip_verify = true\n")
		warnings = append(warnings, insecureSkipVerifyWarning(reg))
	}
	if h := authHeader(self.Auth); h != "" {
		secret = true
		root = append(root, fmt.Sprintf("[header]\n  authorization = %q", h))
	}
	if len(root) > 0 {
		b.WriteString("\n")
		b.WriteString(strings.Join(root, "\n"))
		b.WriteString("\n")
	}
	seen := map[string]bool{}
	for _, ep := range m.Endpoint {
		// Two identical [host] tables are a TOML error: containerd drops the file.
		if seen[ep] {
			warnings = append(warnings, "duplicate endpoint "+ep+" ignored")
			continue
		}
		seen[ep] = true
		host := EndpointHost(ep)
		cfg := cfgs[host]
		fmt.Fprintf(&b, "\n[host.%q]\n", ep)
		b.WriteString("  capabilities = [\"pull\", \"resolve\"]\n")
		if cfg.TLS.CAFile != "" {
			fmt.Fprintf(&b, "  ca = %q\n", cfg.TLS.CAFile)
		}
		if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
			fmt.Fprintf(&b, "  client = [[%q, %q]]\n", cfg.TLS.CertFile, cfg.TLS.KeyFile)
		}
		if cfg.TLS.InsecureSkipVerify {
			b.WriteString("  skip_verify = true\n")
			warnings = append(warnings, insecureSkipVerifyWarning(ep))
		}
		if h := authHeader(cfg.Auth); h != "" {
			secret = true
			fmt.Fprintf(&b, "  [host.%q.header]\n    authorization = %q\n", ep, h)
		}
	}
	if len(m.Rewrite) > 0 {
		// Upstream containerd's certs.d has no regex path-rewrite.
		warnings = append(warnings, "rewrite rules are not supported by upstream containerd certs.d and were ignored")
	}
	return b.String(), secret, warnings
}

// Result reports what Apply did; a caller restarts containerd when Changed is set.
type Result struct {
	Configured bool     // a registries.yaml configuring mirrors or configs was present and applied
	Changed    bool     // the conf.d drop-in was created/updated; restarting containerd is the caller's decision
	Registries []string // registries a hosts.toml was written for
	Warnings   []string
}

// Apply writes the drop-in and hosts.toml files, pruning entries left the config.
func Apply() (Result, error) {
	var r Result
	c, err := Load(version.RegistriesConfig)
	if err != nil {
		return r, err
	}
	if WildcardCredentialsDropped(c) {
		r.Warnings = append(r.Warnings, WildcardCredentialsIgnored)
	}
	regs := Registries(c)
	if c == nil || len(regs) == 0 {
		// Prune stale stratum files so a removed registries.yaml fully reverts.
		changed, err := prune(nil)
		if err != nil {
			return r, err
		}
		r.Changed = changed
		return r, nil
	}
	r.Configured = true

	if err := os.MkdirAll(version.CertsDir, 0o755); err != nil {
		return r, err
	}
	keep := map[string]bool{}
	for _, reg := range regs {
		// A configs-only reg has no Mirror entry: hostsToml renders only root keys.
		self, _ := Effective(reg, c.Configs[reg])
		content, secret, warns := hostsToml(reg, self, c.Configs, c.Mirrors[reg])
		r.Warnings = append(r.Warnings, warns...)
		dirName := certsDirName(reg)
		dir := filepath.Join(version.CertsDir, dirName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return r, err
		}
		mode := os.FileMode(0o644)
		if secret {
			mode = 0o600
		}
		hp := filepath.Join(dir, "hosts.toml")
		if err := os.WriteFile(hp, []byte(content), mode); err != nil {
			return r, err
		}
		// os.WriteFile does not chmod an existing file on re-apply.
		if err := os.Chmod(hp, mode); err != nil {
			return r, err
		}
		keep[dirName] = true
		r.Registries = append(r.Registries, reg)
	}
	if _, err := prune(keep); err != nil {
		return r, err
	}

	changed, err := writeConfd()
	if err != nil {
		return r, err
	}
	r.Changed = changed
	return r, nil
}

// writeConfd writes the drop-in idempotently: no needless containerd restart.
func writeConfd() (bool, error) {
	if err := os.MkdirAll(filepath.Dir(version.ContainerdConfD), 0o755); err != nil {
		return false, err
	}
	if b, err := os.ReadFile(version.ContainerdConfD); err == nil && string(b) == confdDropin() {
		return false, nil
	}
	if err := os.WriteFile(version.ContainerdConfD, []byte(confdDropin()), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// prune removes stratum-generated files not in keep, never hand-authored ones.
func prune(keep map[string]bool) (confdRemoved bool, err error) {
	entries, err := os.ReadDir(version.CertsDir)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() || keep[e.Name()] {
			continue
		}
		f := filepath.Join(version.CertsDir, e.Name(), "hosts.toml")
		b, rerr := os.ReadFile(f)
		if rerr != nil || !strings.HasPrefix(string(b), generatedMarker) {
			continue
		}
		if err := os.Remove(f); err != nil {
			return false, err
		}
		_ = os.Remove(filepath.Join(version.CertsDir, e.Name()))
	}
	if len(keep) == 0 {
		if b, rerr := os.ReadFile(version.ContainerdConfD); rerr == nil && strings.HasPrefix(string(b), generatedMarker) {
			if err := os.Remove(version.ContainerdConfD); err != nil {
				return false, err
			}
			confdRemoved = true
		}
	}
	return confdRemoved, nil
}

package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/ctrliq/stratum/internal/version"
)

func TestLoadAbsentIsNoOp(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("absent file should not error: %v", err)
	}
	if c != nil {
		t.Fatalf("absent file should return nil config, got %+v", c)
	}
}

func TestHostsToml(t *testing.T) {
	tests := []struct {
		name       string
		cfgs       map[string]RegConfig
		mirror     Mirror
		wantSecret bool
		want       []string // substrings that must appear
		absent     []string // substrings that must NOT appear
	}{
		{
			name:   "mirror only",
			mirror: Mirror{Endpoint: []string{"https://mirror.internal:5000"}},
			want: []string{
				`[host."https://mirror.internal:5000"]`,
				`capabilities = ["pull", "resolve"]`,
			},
			absent: []string{"ca =", "authorization", "skip_verify"},
		},
		{
			name: "mirror + tls",
			cfgs: map[string]RegConfig{"mirror.internal:5000": {TLS: TLS{
				CAFile: "/etc/stratum/mirror-ca.crt", CertFile: "/c.crt", KeyFile: "/c.key", InsecureSkipVerify: true,
			}}},
			mirror: Mirror{Endpoint: []string{"https://mirror.internal:5000"}},
			want: []string{
				`ca = "/etc/stratum/mirror-ca.crt"`,
				`client = [["/c.crt", "/c.key"]]`,
				`skip_verify = true`,
			},
		},
		{
			name:       "auth username/password -> basic",
			cfgs:       map[string]RegConfig{"mirror.internal:5000": {Auth: Auth{Username: "user", Password: "pass"}}},
			mirror:     Mirror{Endpoint: []string{"https://mirror.internal:5000"}},
			wantSecret: true,
			// base64("user:pass") = dXNlcjpwYXNz
			want: []string{`authorization = "Basic dXNlcjpwYXNz"`},
		},
		{
			name:       "auth preencoded -> basic",
			cfgs:       map[string]RegConfig{"mirror.internal:5000": {Auth: Auth{Auth: "YWJjOmRlZg=="}}},
			mirror:     Mirror{Endpoint: []string{"https://mirror.internal:5000"}},
			wantSecret: true,
			want:       []string{`authorization = "Basic YWJjOmRlZg=="`},
		},
		{
			name:       "identity token -> bearer",
			cfgs:       map[string]RegConfig{"mirror.internal:5000": {Auth: Auth{IdentityToken: "tok123"}}},
			mirror:     Mirror{Endpoint: []string{"https://mirror.internal:5000"}},
			wantSecret: true,
			want:       []string{`authorization = "Bearer tok123"`},
		},
		{
			name: "multiple endpoints in order",
			mirror: Mirror{Endpoint: []string{
				"https://a.internal:5000", "https://b.internal:5000",
			}},
			want: []string{`[host."https://a.internal:5000"]`, `[host."https://b.internal:5000"]`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, secret, _ := hostsToml("", RegConfig{}, tt.cfgs, tt.mirror)
			if secret != tt.wantSecret {
				t.Errorf("secret = %v, want %v\n%s", secret, tt.wantSecret, out)
			}
			for _, s := range tt.want {
				if !strings.Contains(out, s) {
					t.Errorf("output missing %q\n%s", s, out)
				}
			}
			for _, s := range tt.absent {
				if strings.Contains(out, s) {
					t.Errorf("output should not contain %q\n%s", s, out)
				}
			}
		})
	}
}

// TestHostsTomlLoopbackRootServer pins the loopback guard: containerd 2.x
// forces skip_verify on an implicit localhost host when hosts.toml carries no
// root `server`, so a loopback registry configured with TLS and credentials
// would silently drop verification. An explicit root `server = "https://<reg>"`
// makes that host explicit, so containerd keeps the configured TLS; a
// non-loopback registry (containerd already defaults it to https) and one with
// skip_verify set anyway need no server line.
func TestHostsTomlLoopbackRootServer(t *testing.T) {
	out, _, _ := hostsToml("localhost:5000", RegConfig{TLS: TLS{CAFile: "/etc/stratum/local-ca.crt"}}, nil, Mirror{})
	if !strings.Contains(out, "server = \"https://localhost:5000\"") {
		t.Errorf("loopback registry with TLS must get an explicit root server line:\n%s", out)
	}
	for _, reg := range []string{"localhost", "127.0.0.1:5000", "127.0.0.9", "[::1]:5000"} {
		out, _, _ := hostsToml(reg, RegConfig{TLS: TLS{CAFile: "/ca"}}, nil, Mirror{})
		if !strings.Contains(out, "server = \"https://"+reg+"\"") {
			t.Errorf("loopback %s must get an explicit root server line:\n%s", reg, out)
		}
	}
	// A bare IPv6 key must be bracketed in the URL: net/url rejects an
	// unbracketed IPv6 literal, which would make containerd discard the whole
	// hosts.toml. IPv4-mapped IPv6 loopback keys need it too.
	for _, reg := range []string{"::1", "::ffff:127.0.0.1", "::ffff:7f00:1", "0:0:0:0:0:0:0:1"} {
		out, _, _ := hostsToml(reg, RegConfig{TLS: TLS{CAFile: "/ca"}}, nil, Mirror{})
		if !strings.Contains(out, "server = \"https://["+reg+"]\"") {
			t.Errorf("bare IPv6 %s must get a bracketed root server line:\n%s", reg, out)
		}
	}
	if out, _, _ = hostsToml("registry.corp:5000", RegConfig{TLS: TLS{CAFile: "/ca"}}, nil, Mirror{}); strings.Contains(out, "server =") {
		t.Errorf("a non-loopback registry must not get a root server line:\n%s", out)
	}
	if out, _, _ = hostsToml("localhost:5000", RegConfig{TLS: TLS{InsecureSkipVerify: true}}, nil, Mirror{}); strings.Contains(out, "server =") {
		t.Errorf("a registry with skip_verify set anyway needs no root server line:\n%s", out)
	}
	if out, _, _ = hostsToml("localhost:5000", RegConfig{}, nil, Mirror{}); strings.Contains(out, "server =") {
		t.Errorf("a registry with no auth/TLS of its own must not get a root server line:\n%s", out)
	}
}

func TestHostsTomlRewriteWarns(t *testing.T) {
	_, _, warns := hostsToml("", RegConfig{}, nil, Mirror{
		Endpoint: []string{"https://m:5000"},
		Rewrite:  map[string]string{"^rancher/(.*)": "mirror/$1"},
	})
	if len(warns) == 0 {
		t.Fatal("rewrite rules should produce a warning, not be silently dropped")
	}
}

func TestHostsTomlDedupEndpoints(t *testing.T) {
	// Two identical [host] tables would be invalid TOML → containerd drops the whole
	// hosts.toml. The dup must be emitted once and warned about.
	out, _, warns := hostsToml("", RegConfig{}, nil, Mirror{Endpoint: []string{"https://m:5000", "https://m:5000"}})
	if n := strings.Count(out, `[host."https://m:5000"]`); n != 1 {
		t.Errorf("duplicate endpoint emitted %d times, want 1:\n%s", n, out)
	}
	if !hasWarn(warns, "duplicate endpoint") {
		t.Errorf("duplicate endpoint should warn, got %v", warns)
	}
}

func TestHostsTomlSkipVerifyWarns(t *testing.T) {
	_, _, warns := hostsToml(
		"m:5000", RegConfig{}, map[string]RegConfig{"m:5000": {TLS: TLS{InsecureSkipVerify: true}}},
		Mirror{Endpoint: []string{"https://m:5000"}},
	)
	// Pin the full sentence: the same helper serves mirror endpoints and
	// configs-only hosts pulled directly, so it must not say "mirror".
	if !hasWarn(warns, "insecure_skip_verify is set for https://m:5000 — TLS verification is DISABLED") {
		t.Errorf("insecure_skip_verify warning must name the target and say TLS verification is DISABLED, got %v", warns)
	}
}

func TestEndpointHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://mirror.internal:5000": "mirror.internal:5000",
		"http://10.0.0.1:30591/v2":     "10.0.0.1:30591",
		"mirror.internal:5000":         "mirror.internal:5000",
	} {
		if got := EndpointHost(in); got != want {
			t.Errorf("EndpointHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyWildcardToDefaultDir(t *testing.T) {
	certsDir, _, regYaml := redirect(t)
	if err := os.WriteFile(regYaml, []byte("mirrors:\n  \"*\":\n    endpoint: [\"https://m:5000\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(); err != nil {
		t.Fatal(err)
	}
	// "*" → containerd's _default catch-all dir, not a literal certs.d/*/
	if _, err := os.Stat(filepath.Join(certsDir, "_default", "hosts.toml")); err != nil {
		t.Errorf("wildcard '*' should map to certs.d/_default: %v", err)
	}
	if _, err := os.Stat(filepath.Join(certsDir, "*", "hosts.toml")); !os.IsNotExist(err) {
		t.Error("a literal certs.d/*/ dir must not be created")
	}
	// Idempotent re-apply must NOT prune _default (prune keeps on the dir name, so
	// keep must be keyed by "_default", not the mirror key "*").
	if _, err := Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(certsDir, "_default", "hosts.toml")); err != nil {
		t.Errorf("_default was pruned on idempotent re-apply (keep keyed by mirror key not dir name?): %v", err)
	}
}

func hasWarn(warns []string, sub string) bool {
	for _, w := range warns {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// redirect points the package paths at a temp tree for a hermetic Apply test.
func redirect(t *testing.T) (certsDir, confd, regYaml string) {
	t.Helper()
	root := t.TempDir()
	certsDir = filepath.Join(root, "certs.d")
	confd = filepath.Join(root, "conf.d", "stratum-registry.toml")
	regYaml = filepath.Join(root, "registries.yaml")
	oc, od, or := version.CertsDir, version.ContainerdConfD, version.RegistriesConfig
	version.CertsDir, version.ContainerdConfD, version.RegistriesConfig = certsDir, confd, regYaml
	t.Cleanup(func() { version.CertsDir, version.ContainerdConfD, version.RegistriesConfig = oc, od, or })
	return
}

func TestApplyWritesAndModes(t *testing.T) {
	certsDir, confd, regYaml := redirect(t)
	yaml := `mirrors:
  docker.io:
    endpoint: ["https://mirror.internal:5000"]
  registry.k8s.io:
    endpoint: ["https://mirror.internal:5000"]
configs:
  "mirror.internal:5000":
    auth: {username: u, password: p}
    tls: {ca_file: /etc/stratum/mirror-ca.crt}
`
	if err := os.WriteFile(regYaml, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !r.Configured || !r.Changed {
		t.Fatalf("expected Configured+Changed, got %+v", r)
	}
	// conf.d drop-in sets config_path.
	cb, err := os.ReadFile(confd)
	if err != nil || !strings.Contains(string(cb), "config_path = '"+certsDir+"'") {
		t.Fatalf("conf.d drop-in wrong: %v / %s", err, cb)
	}
	// hosts.toml with a credential must be 0600.
	hp := filepath.Join(certsDir, "docker.io", "hosts.toml")
	fi, err := os.Stat(hp)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("credentialed hosts.toml mode = %o, want 600", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(certsDir, "registry.k8s.io", "hosts.toml")); err != nil {
		t.Errorf("second registry hosts.toml missing: %v", err)
	}

	// Second Apply with identical input must be idempotent (no restart needed).
	r2, err := Apply()
	if err != nil {
		t.Fatal(err)
	}
	if r2.Changed {
		t.Error("re-apply of identical config should report Changed=false (no needless restart)")
	}
}

func TestApplyTightensModeWhenCredAdded(t *testing.T) {
	certsDir, _, regYaml := redirect(t)
	hp := filepath.Join(certsDir, "docker.io", "hosts.toml")

	// First: no creds → 0644.
	if err := os.WriteFile(regYaml, []byte("mirrors:\n  docker.io:\n    endpoint: [\"https://m:5000\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(hp); fi.Mode().Perm() != 0o644 {
		t.Fatalf("no-cred hosts.toml mode = %o, want 644", fi.Mode().Perm())
	}
	// Re-apply WITH a credential for the same registry: mode must tighten to 0600
	// even though the file already exists (os.WriteFile alone would leave it 0644).
	withAuth := "mirrors:\n  docker.io:\n    endpoint: [\"https://m:5000\"]\nconfigs:\n  \"m:5000\":\n    auth: {username: u, password: p}\n"
	if err := os.WriteFile(regYaml, []byte(withAuth), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(hp); fi.Mode().Perm() != 0o600 {
		t.Errorf("credentialed hosts.toml mode = %o, want 600 (WriteFile does not chmod an existing file)", fi.Mode().Perm())
	}
}

func TestApplyPrunesRemovedMirror(t *testing.T) {
	certsDir, confd, regYaml := redirect(t)
	if err := os.WriteFile(regYaml, []byte("mirrors:\n  docker.io:\n    endpoint: [\"https://m:5000\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(certsDir, "docker.io", "hosts.toml")); err != nil {
		t.Fatalf("docker.io not written: %v", err)
	}
	// Hand-authored entry must survive prune.
	hand := filepath.Join(certsDir, "private.example.com")
	if err := os.MkdirAll(hand, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hand, "hosts.toml"), []byte("server = \"https://x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Remove docker.io from config and re-apply.
	if err := os.WriteFile(regYaml, []byte("mirrors:\n  quay.io:\n    endpoint: [\"https://m:5000\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(certsDir, "docker.io", "hosts.toml")); !os.IsNotExist(err) {
		t.Error("stale stratum-generated docker.io hosts.toml should have been pruned")
	}
	if _, err := os.Stat(filepath.Join(hand, "hosts.toml")); err != nil {
		t.Error("hand-authored certs.d entry must NOT be pruned")
	}
	_ = confd
}

// TestApplyConfigsOnly pins the configs-only shape: a registries.yaml with only
// a `configs` entry (auth + TLS, no mirrors) must still produce that registry's
// certs.d hosts.toml, with the registry's own auth/TLS as ROOT-level keys (no
// `server` line, no [host.*] table) so containerd uses them when pulling from
// the registry directly.
func TestApplyConfigsOnly(t *testing.T) {
	certsDir, confd, regYaml := redirect(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"registry.corp:5000\":\n    auth: {auth: dTpw}\n    tls: {ca_file: /etc/stratum/corp-ca.crt}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !r.Configured {
		t.Fatal("configs-only file must report Configured=true")
	}
	if !r.Changed {
		t.Error("first apply of a configs-only file writes the drop-in, so Changed=true")
	}
	if len(r.Registries) != 1 || r.Registries[0] != "registry.corp:5000" {
		t.Errorf("Registries = %v, want [registry.corp:5000]", r.Registries)
	}
	hp := filepath.Join(certsDir, "registry.corp:5000", "hosts.toml")
	fi, err := os.Stat(hp)
	if err != nil {
		t.Fatalf("configs-only hosts.toml missing: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("hosts.toml carrying a credential mode = %o, want 600", fi.Mode().Perm())
	}
	b, err := os.ReadFile(hp)
	if err != nil {
		t.Fatal(err)
	}
	want := generatedMarker + "\nca = \"/etc/stratum/corp-ca.crt\"\n\n[header]\n  authorization = \"Basic dTpw\"\n"
	if string(b) != want {
		t.Errorf("hosts.toml content mismatch:\ngot:\n%s\nwant:\n%s", b, want)
	}
	cb, err := os.ReadFile(confd)
	if err != nil || !strings.Contains(string(cb), "config_path") {
		t.Errorf("conf.d drop-in not written for configs-only config: %v / %s", err, cb)
	}
}

// TestApplyMixedConfigsAndMirrors pins the mixed shape: a registry that is BOTH
// mirrored and configured keeps its [host.*] mirror tables AND gains the
// registry's own auth as a root-level [header] table ABOVE them; a configs-only
// registry in the same file gets its own hosts.toml too.
func TestApplyMixedConfigsAndMirrors(t *testing.T) {
	certsDir, _, regYaml := redirect(t)
	yaml := `mirrors:
  docker.io:
    endpoint: ["https://mirror.internal:5000"]
configs:
  docker.io:
    auth: {username: user, password: pass}
  "registry.corp:5000":
    auth: {username: u, password: p}
`
	if err := os.WriteFile(regYaml, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !r.Configured || len(r.Registries) != 2 {
		t.Fatalf("mixed file must configure both registries, got %+v", r)
	}
	hp := filepath.Join(certsDir, "docker.io", "hosts.toml")
	b, err := os.ReadFile(hp)
	if err != nil {
		t.Fatalf("docker.io hosts.toml missing: %v", err)
	}
	out := string(b)
	// base64("user:pass") = dXNlcjpwYXNz
	if i, j := strings.Index(out, "[header]"), strings.Index(out, `[host."https://mirror.internal:5000"]`); i < 0 || j < 0 || i > j {
		t.Errorf("root [header] must precede the [host.*] table (TOML root keys before tables):\n%s", out)
	}
	// The toml.Unmarshal below asserts the header is a ROOT table carrying
	// docker.io's credential; it does NOT prove ordering — a [header] table
	// after the [host.*] table still decodes at root. The strings.Index check
	// above is the only ordering pin.
	var doc struct {
		Header map[string]string `toml:"header"`
	}
	if err := toml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("hosts.toml is not valid TOML: %v\n%s", err, out)
	}
	if doc.Header["authorization"] != "Basic dXNlcjpwYXNz" {
		t.Errorf("root-level header.authorization = %q, want the docker.io credential:\n%s", doc.Header["authorization"], out)
	}
	if _, err := os.Stat(filepath.Join(certsDir, "registry.corp:5000", "hosts.toml")); err != nil {
		t.Errorf("configs-only registry.corp:5000 hosts.toml missing: %v", err)
	}
}

// TestApplyConfigsWildcardTLS pins that a configs "*" entry with only TLS gets
// the _default catch-all dir, with its CA as a root-level key.
func TestApplyConfigsWildcardTLS(t *testing.T) {
	certsDir, _, regYaml := redirect(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"*\":\n    tls: {ca_file: /etc/pki/ca.crt}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Apply()
	if err != nil {
		t.Fatal(err)
	}
	if !r.Configured {
		t.Fatal("a configs entry with TLS must count as configured")
	}
	b, err := os.ReadFile(filepath.Join(certsDir, "_default", "hosts.toml"))
	if err != nil {
		t.Fatalf("'*' configs should map to certs.d/_default: %v", err)
	}
	if !strings.Contains(string(b), `ca = "/etc/pki/ca.crt"`) {
		t.Errorf("_default/hosts.toml missing root ca:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(certsDir, "*", "hosts.toml")); !os.IsNotExist(err) {
		t.Error("a literal certs.d/*/ dir must not be created")
	}
}

// TestApplyWildcardCredentialsAreNotWritten pins that configs."*" credentials
// and client certificate never reach _default/hosts.toml: that file has no
// server, so containerd fills in the pulled registry and, for a loopback one,
// turns TLS verification off. Its CA and the mirror endpoints still apply, and
// the operator is warned. The file then holds no secret, so it is not 0600.
func TestApplyWildcardCredentialsAreNotWritten(t *testing.T) {
	for _, tc := range []struct {
		name, yaml     string
		wantConfigured bool
		wantInFile     []string
	}{
		{
			name:           "credentials, client cert and CA",
			yaml:           "configs:\n  \"*\":\n    auth: {username: u, password: p}\n    tls: {ca_file: /etc/pki/ca.crt, cert_file: /c.crt, key_file: /c.key}\n",
			wantConfigured: true,
			wantInFile:     []string{`ca = "/etc/pki/ca.crt"`},
		},
		{
			name: "credentials only",
			yaml: "configs:\n  \"*\":\n    auth: {username: u, password: p}\n",
		},
		{
			name:           "credentials beside a catch-all mirror",
			yaml:           "mirrors:\n  \"*\":\n    endpoint: [\"https://mirror.internal\"]\nconfigs:\n  \"*\":\n    auth: {username: u, password: p}\n",
			wantConfigured: true,
			wantInFile:     []string{`[host."https://mirror.internal"]`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			certsDir, _, regYaml := redirect(t)
			if err := os.WriteFile(regYaml, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			r, err := Apply()
			if err != nil {
				t.Fatal(err)
			}
			if !hasWarn(r.Warnings, WildcardCredentialsIgnored) {
				t.Errorf("warnings = %v, want the wildcard-credentials warning", r.Warnings)
			}
			if r.Configured != tc.wantConfigured {
				t.Errorf("Configured = %v, want %v", r.Configured, tc.wantConfigured)
			}
			hp := filepath.Join(certsDir, "_default", "hosts.toml")
			if !tc.wantConfigured {
				if _, err := os.Stat(hp); !os.IsNotExist(err) {
					t.Errorf("_default/hosts.toml written for credentials alone: %v", err)
				}
				return
			}
			b, err := os.ReadFile(hp)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{"authorization", "client =", "server ="} {
				if strings.Contains(string(b), bad) {
					t.Errorf("_default/hosts.toml carries %q:\n%s", bad, b)
				}
			}
			for _, want := range tc.wantInFile {
				if !strings.Contains(string(b), want) {
					t.Errorf("_default/hosts.toml missing %q:\n%s", want, b)
				}
			}
			st, err := os.Stat(hp)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0o644 {
				t.Errorf("_default/hosts.toml mode = %v, want 0644 (no secret written)", st.Mode().Perm())
			}
		})
	}
}

// TestEffectiveKeepsNamedRegistries pins that only the catch-all entry is
// trimmed: a named registry keeps its credentials and client certificate.
func TestEffectiveKeepsNamedRegistries(t *testing.T) {
	full := RegConfig{
		Auth: Auth{Username: "u", Password: "p"},
		TLS:  TLS{CAFile: "/ca", CertFile: "/c.crt", KeyFile: "/c.key", InsecureSkipVerify: true},
	}
	if got, dropped := Effective("registry.corp:5000", full); dropped || got != full {
		t.Errorf("Effective(named) = %+v, %v; want the entry unchanged and nothing dropped", got, dropped)
	}
	want := RegConfig{TLS: TLS{CAFile: "/ca", InsecureSkipVerify: true}}
	if got, dropped := Effective("*", full); !dropped || got != want {
		t.Errorf("Effective(\"*\") = %+v, %v; want %+v, true", got, dropped, want)
	}
	if _, dropped := Effective("*", RegConfig{TLS: TLS{CAFile: "/ca"}}); dropped {
		t.Error("a catch-all entry with only a CA has nothing to drop")
	}
}

// TestApplyConfigsOnlySkipVerifyWarns pins that disabling TLS verification on a
// configs-only host is warned about and lands as a root-level skip_verify.
func TestApplyConfigsOnlySkipVerifyWarns(t *testing.T) {
	certsDir, _, regYaml := redirect(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"registry.corp:5000\":\n    tls: {insecure_skip_verify: true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Apply()
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarn(r.Warnings, "insecure_skip_verify is set for registry.corp:5000") {
		t.Errorf("configs-only insecure_skip_verify should warn, got %v", r.Warnings)
	}
	b, err := os.ReadFile(filepath.Join(certsDir, "registry.corp:5000", "hosts.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\nskip_verify = true\n") {
		t.Errorf("root-level skip_verify missing:\n%s", b)
	}
}

// TestApplyConfigsEntryWithoutAuthOrTLSIsNothing pins the "nothing to do" exit:
// a configs entry that carries neither auth nor TLS configures nothing, so a
// file holding only that is treated as unconfigured and stale stratum files are
// pruned.
func TestApplyConfigsEntryWithoutAuthOrTLSIsNothing(t *testing.T) {
	certsDir, confd, regYaml := redirect(t)
	// Leftovers from an earlier configuration must be pruned.
	stale := filepath.Join(certsDir, "old.corp:5000")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "hosts.toml"), []byte(generatedMarker+"server = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(confd), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(confd, []byte(confdDropin()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"registry.corp:5000\": {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Apply()
	if err != nil {
		t.Fatal(err)
	}
	if r.Configured {
		t.Error("a configs entry with neither auth nor TLS is nothing configured")
	}
	if len(r.Registries) != 0 {
		t.Errorf("Registries = %v, want empty", r.Registries)
	}
	if _, err := os.Stat(filepath.Join(stale, "hosts.toml")); !os.IsNotExist(err) {
		t.Error("stale stratum-generated hosts.toml should be pruned when nothing is configured")
	}
	if _, err := os.Stat(confd); !os.IsNotExist(err) {
		t.Error("conf.d drop-in should be pruned when nothing is configured")
	}
	if !r.Changed {
		t.Error("pruning the drop-in is a change (restart needed)")
	}
}

// TestApplyPrunesRemovedConfigsEntry pins keep/prune agreement for configs-only
// hosts: removing the entry and re-applying removes its hosts.toml.
func TestApplyPrunesRemovedConfigsEntry(t *testing.T) {
	certsDir, _, regYaml := redirect(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"registry.corp:5000\":\n    auth: {username: u, password: p}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(); err != nil {
		t.Fatal(err)
	}
	corp := filepath.Join(certsDir, "registry.corp:5000", "hosts.toml")
	if _, err := os.Stat(corp); err != nil {
		t.Fatalf("configs-only hosts.toml not written: %v", err)
	}
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"other.corp:5000\":\n    auth: {username: u, password: p}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(corp); !os.IsNotExist(err) {
		t.Error("removed configs entry's hosts.toml should have been pruned")
	}
	if _, err := os.Stat(filepath.Join(certsDir, "other.corp:5000", "hosts.toml")); err != nil {
		t.Errorf("new configs entry's hosts.toml missing: %v", err)
	}
}

// TestRegistriesCertOnlyIsNothing pins that an incomplete client cert/key pair
// (one file without the other) configures nothing: the renderer emits `client`
// only when BOTH are set, so a hosts.toml written for the lone file would be a
// marker with no payload that still shadows the _default ("*") mirror.
func TestRegistriesCertOnlyIsNothing(t *testing.T) {
	c := &Config{Configs: map[string]RegConfig{
		"certonly.corp:5000": {TLS: TLS{CertFile: "/c.crt"}},
		"keyonly.corp:5000":  {TLS: TLS{KeyFile: "/c.key"}},
		"pair.corp:5000":     {TLS: TLS{CertFile: "/c.crt", KeyFile: "/c.key"}},
	}}
	got := Registries(c)
	if len(got) != 1 || got[0] != "pair.corp:5000" {
		t.Errorf("Registries = %v, want [pair.corp:5000] only (cert-only and key-only configure nothing)", got)
	}
}

// TestApplyCertOnlyConfigsWritesNothing pins the Apply side of the same rule:
// a configs entry with a cert_file but no key_file must produce no certs.d
// directory at all.
func TestApplyCertOnlyConfigsWritesNothing(t *testing.T) {
	certsDir, _, regYaml := redirect(t)
	if err := os.WriteFile(regYaml, []byte("configs:\n  \"certonly.corp:5000\":\n    tls: {cert_file: /c.crt}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Apply()
	if err != nil {
		t.Fatal(err)
	}
	if r.Configured {
		t.Error("a cert-only configs entry must not report Configured=true")
	}
	if _, err := os.Stat(filepath.Join(certsDir, "certonly.corp:5000")); !os.IsNotExist(err) {
		t.Error("a cert-only configs entry must not write certs.d/<host>/ (a marker-only hosts.toml would shadow the _default mirror)")
	}
}

// TestRegistries pins the shared "what is configured" predicate: the sorted
// union of mirror keys and configs keys that carry auth or TLS on their own.
// A configs entry with neither auth nor TLS configures nothing, and a nil
// config configures nothing.
func TestRegistries(t *testing.T) {
	c := &Config{
		Mirrors: map[string]Mirror{"docker.io": {Endpoint: []string{"https://m:5000"}}},
		Configs: map[string]RegConfig{
			"registry.corp:5000": {Auth: Auth{Username: "u", Password: "p"}},
			"tls.corp:5000":      {TLS: TLS{CAFile: "/ca"}},
			"docker.io":          {Auth: Auth{Auth: "dTpw"}}, // mirrored; not listed twice
			"empty.corp:5000":    {},
		},
	}
	got := Registries(c)
	want := []string{"docker.io", "registry.corp:5000", "tls.corp:5000"}
	if len(got) != len(want) {
		t.Fatalf("Registries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Registries[%d] = %q, want %q (sorted union, no duplicates)", i, got[i], want[i])
		}
	}
	if r := Registries(nil); r != nil {
		t.Errorf("Registries(nil) = %v, want nil", r)
	}
	if r := Registries(&Config{}); len(r) != 0 {
		t.Errorf("Registries(empty) = %v, want empty", r)
	}
}

func TestApplyAbsentRegistriesRemovesDropin(t *testing.T) {
	certsDir, confd, regYaml := redirect(t)
	// First configure, then remove registries.yaml entirely.
	if err := os.WriteFile(regYaml, []byte("mirrors:\n  docker.io:\n    endpoint: [\"https://m:5000\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(regYaml); err != nil {
		t.Fatal(err)
	}
	r, err := Apply()
	if err != nil {
		t.Fatal(err)
	}
	if !r.Changed {
		t.Error("removing registries.yaml should remove the conf.d drop-in (Changed=true → restart)")
	}
	if _, err := os.Stat(confd); !os.IsNotExist(err) {
		t.Error("conf.d drop-in should be removed when no registries configured")
	}
	if _, err := os.Stat(filepath.Join(certsDir, "docker.io", "hosts.toml")); !os.IsNotExist(err) {
		t.Error("stale hosts.toml should be pruned when registries.yaml is removed")
	}
}

// Nodes upgraded in place carry certs.d and conf.d files opened by this exact
// line; prune recognises stratum's files by it, so it must not change.
func TestGeneratedMarkerMatchesFilesAlreadyOnNodes(t *testing.T) {
	const onNodes = "# generated by stratum — do not edit (source: stratum registries.yaml)\n"
	if generatedMarker != onNodes {
		t.Errorf("generatedMarker = %q, want %q: prune would stop removing files earlier builds wrote", generatedMarker, onNodes)
	}
}

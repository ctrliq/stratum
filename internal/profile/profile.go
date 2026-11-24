// Package profile declares node profiles: named sets of hardened settings a node selects.
package profile

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
)

// CISName is the name of the engine's own cis profile.
const CISName = "cis"

// Source says where a value comes from; the zero value deliberately reads as undeclared.
type Source int

const (
	// Literal uses the entry's Value as written.
	Literal Source = iota + 1
	// ConfigFile resolves Value, a declared file name, to the path it was written at.
	ConfigFile
	// LogFile resolves Value, a file name, to a path under the log directory.
	LogFile
	// PKIFile resolves Value, a file name, to a path under the PKI directory.
	PKIFile
)

// FileKind says how the engine treats a config file beyond writing it.
type FileKind int

const (
	// PlainFile is written and may be named by a ConfigFile flag.
	PlainFile FileKind = iota + 1
	// AdmissionConfiguration: the engine names the merged file on the apiserver's argv.
	AdmissionConfiguration
)

// Context is what a profile may read when it renders a file.
type Context struct {
	// KubernetesVersion is the binary's version, with any pre-release suffix.
	KubernetesVersion string
}

// Flag is one apiserver flag, rendered on control planes.
type Flag struct {
	Name string
	// Value is the literal, or what From resolves.
	Value string
	From  Source
}

// Setting is one kubelet.conf line, rendered on every node.
type Setting struct {
	Key string
	// Value is the literal, or what From resolves.
	Value string
	From  Source
}

// File is a config file written on control planes and carried across upgrades.
type File struct {
	Name string
	Kind FileKind
	// Render returns the file's bytes.
	Render func(Context) ([]byte, error)
}

// Secret is minted once on the first control plane and carried in the HA join bundle.
type Secret struct {
	Name string
	// Mint returns the bytes a new cluster starts with.
	Mint func() ([]byte, error)
}

// Profile is one registered node profile.
type Profile struct {
	// Name selects the profile: a DNS-1123 label, unique in the binary.
	Name string
	// APIServerFlags are spliced into the apiserver's argv, in order.
	APIServerFlags []Flag
	// KubeletSettings are rendered into kubelet.conf, in order.
	KubeletSettings []Setting
	ConfigFiles     []File
	SharedSecrets   []Secret
	// Sysctls are a sysctl drop-in; a node that cannot apply them does not start.
	Sysctls []string
	// RequiresOperator refuses a control plane that turned the operator off.
	RequiresOperator bool
	// RequiresSELinux refuses spec.selinux: false and a missing SELinux policy.
	RequiresSELinux bool
	// KubeletServerTLSBootstrap bootstraps kubelet serving certs via the CSR flow.
	KubeletServerTLSBootstrap bool
	// DisableNodeShell turns the dashboard's privileged node shell off by default.
	DisableNodeShell bool
}

// Validate reports the first thing wrong with the profile's declaration.
func (p Profile) Validate() error {
	if errs := validation.IsDNS1123Label(p.Name); len(errs) > 0 {
		return fmt.Errorf("profile %q: the name must be a DNS-1123 label: %s", p.Name, strings.Join(errs, "; "))
	}
	files := map[string]bool{}
	admission := false
	for _, f := range p.ConfigFiles {
		if err := checkFileName(p.Name, f.Name, files); err != nil {
			return err
		}
		if EngineOwnsConfigName(f.Name) {
			return fmt.Errorf("profile %q: config file %q would replace a file the engine keeps in its config directory", p.Name, f.Name)
		}
		if f.Kind != PlainFile && f.Kind != AdmissionConfiguration {
			return fmt.Errorf("profile %q: config file %q has no kind", p.Name, f.Name)
		}
		if f.Kind == AdmissionConfiguration {
			if admission {
				return fmt.Errorf("profile %q: config file %q is a second AdmissionConfiguration; the apiserver reads one", p.Name, f.Name)
			}
			admission = true
		}
		if f.Render == nil {
			return fmt.Errorf("profile %q: config file %q has no Render", p.Name, f.Name)
		}
	}
	for _, s := range p.SharedSecrets {
		if err := checkFileName(p.Name, s.Name, files); err != nil {
			return err
		}
		if s.Mint == nil {
			return fmt.Errorf("profile %q: shared secret %q has no Mint", p.Name, s.Name)
		}
		if ext := path.Ext(s.Name); pkiExtensions[ext] {
			return fmt.Errorf("profile %q: shared secret %q has the extension %s, which the engine's own certificates and keys in its PKI directory use", p.Name, s.Name, ext)
		}
		if strings.HasPrefix(s.Name, ReservedSecretPrefix) {
			return fmt.Errorf("profile %q: shared secret %q starts with %s, which names the engine's CA-rotation state in its PKI directory", p.Name, s.Name, ReservedSecretPrefix)
		}
		if ReservedSecretName(s.Name) {
			return fmt.Errorf("profile %q: shared secret %q is a name the engine's join bundle uses itself", p.Name, s.Name)
		}
		if s.Name == CISEncryptionConfigName && p.Name != CISName {
			return fmt.Errorf("profile %q: shared secret %q is the cis profile's at-rest encryption config: the join bundle proves that secret's key from its secretbox layout, so only the cis profile may declare it", p.Name, s.Name)
		}
	}
	flags := map[string]bool{}
	for _, f := range p.APIServerFlags {
		if !strings.HasPrefix(f.Name, "--") || len(f.Name) < 3 || strings.Contains(f.Name, "=") {
			return fmt.Errorf("profile %q: apiserver flag %q must be a --name with its value in Value", p.Name, f.Name)
		}
		if EngineOwnsAPIServerFlag(f.Name) {
			return fmt.Errorf("profile %q: apiserver flag %q is one the engine renders itself; the apiserver takes the last value of a repeated flag, so the profile's would replace it", p.Name, f.Name)
		}
		if flags[f.Name] {
			return fmt.Errorf("profile %q: apiserver flag %q is declared twice", p.Name, f.Name)
		}
		flags[f.Name] = true
		if err := checkSource(p.Name, "apiserver flag "+f.Name, f.From, f.Value, files); err != nil {
			return err
		}
	}
	keys := map[string]bool{}
	for _, s := range p.KubeletSettings {
		if s.Key == "" || strings.ContainsAny(s.Key, ": \n") {
			return fmt.Errorf("profile %q: kubelet setting %q must be a bare kubelet.conf key", p.Name, s.Key)
		}
		if EngineOwnsKubeletKey(s.Key) {
			return fmt.Errorf("profile %q: kubelet setting %q is one the engine writes into kubelet.conf itself", p.Name, s.Key)
		}
		if keys[s.Key] {
			return fmt.Errorf("profile %q: kubelet setting %q is declared twice", p.Name, s.Key)
		}
		keys[s.Key] = true
		if strings.ContainsAny(s.Value, "\n\r") {
			return fmt.Errorf("profile %q: kubelet setting %q has a line break in its value; a setting is one kubelet.conf line", p.Name, s.Key)
		}
		if err := checkSource(p.Name, "kubelet setting "+s.Key, s.From, s.Value, files); err != nil {
			return err
		}
	}
	for _, line := range p.Sysctls {
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" || strings.ContainsAny(line, "\n#") {
			return fmt.Errorf("profile %q: sysctl %q must be key=value", p.Name, line)
		}
		if EngineOwnsSysctl(k) {
			return fmt.Errorf("profile %q: sysctl %q is one the engine sets in 99-stratum.conf, which sysctl --system applies after the profile's drop-in", p.Name, line)
		}
	}
	return nil
}

// engineConfigNames are the files the engine keeps in the profile config-file directory.
var engineConfigNames = map[string]bool{
	"admin.conf":                true,
	"admission-webhook.yaml":    true,
	"backup-evidence.yaml":      true,
	"certs.d":                   true,
	"cilium.json":               true,
	"components":                true,
	"conf.d":                    true,
	"containerd-root":           true,
	"containerd-state":          true,
	"containerd.toml":           true,
	"join-intent.json":          true,
	"kubelet.conf":              true,
	"leaf-subject-migration":    true,
	"manifests":                 true,
	"netconfig":                 true,
	"node.yaml":                 true,
	"prepare-incomplete":        true,
	"registries.yaml":           true,
	"renewal.yaml":              true,
	"resolv.conf":               true,
	"rotation-incomplete":       true,
	"selinux-confined-since":    true,
	"snapshot-evidence.yaml":    true,
	"webhook-client.kubeconfig": true,
}

// engineAPIServerFlags: the last repeated flag wins, so a profile's would replace it.
var engineAPIServerFlags = map[string]bool{
	"--admission-control-config-file":      true,
	"--advertise-address":                  true,
	"--allow-privileged":                   true,
	"--api-audiences":                      true,
	"--authorization-mode":                 true,
	"--client-ca-file":                     true,
	"--enable-admission-plugins":           true,
	"--enable-aggregator-routing":          true,
	"--enable-bootstrap-token-auth":        true,
	"--etcd-cafile":                        true,
	"--etcd-certfile":                      true,
	"--etcd-keyfile":                       true,
	"--etcd-servers":                       true,
	"--kubelet-certificate-authority":      true,
	"--kubelet-client-certificate":         true,
	"--kubelet-client-key":                 true,
	"--kubelet-preferred-address-types":    true,
	"--profiling":                          true,
	"--proxy-client-cert-file":             true,
	"--proxy-client-key-file":              true,
	"--request-timeout":                    true,
	"--requestheader-allowed-names":        true,
	"--requestheader-client-ca-file":       true,
	"--requestheader-extra-headers-prefix": true,
	"--requestheader-group-headers":        true,
	"--requestheader-username-headers":     true,
	"--secure-port":                        true,
	"--service-account-issuer":             true,
	"--service-account-key-file":           true,
	"--service-account-lookup":             true,
	"--service-account-signing-key-file":   true,
	"--service-cluster-ip-range":           true,
	"--tls-cert-file":                      true,
	"--tls-cipher-suites":                  true,
	"--tls-private-key-file":               true,
}

// EngineOwnsAPIServerFlag reports whether a profile may not declare that flag.
func EngineOwnsAPIServerFlag(name string) bool { return engineAPIServerFlags[name] }

// engineKubeletKeys are the kubelet.conf keys the engine writes; a profile's would duplicate it.
var engineKubeletKeys = map[string]bool{
	"apiVersion":                             true,
	"authentication":                         true,
	"authorization":                          true,
	"cgroupDriver":                           true,
	"clusterDNS":                             true,
	"clusterDomain":                          true,
	"containerRuntimeEndpoint":               true,
	"eventRecordQPS":                         true,
	"failSwapOn":                             true,
	"imagePullCredentialsVerificationPolicy": true,
	"kind":                                   true,
	"makeIPTablesUtilChains":                 true,
	"preloadedImagesVerificationAllowlist":   true,
	"readOnlyPort":                           true,
	"registerWithTaints":                     true,
	"rotateCertificates":                     true,
	"serverTLSBootstrap":                     true,
	"staticPodPath":                          true,
	"streamingConnectionIdleTimeout":         true,
	"tlsCertFile":                            true,
	"tlsCipherSuites":                        true,
	"tlsPrivateKeyFile":                      true,
}

// EngineOwnsKubeletKey reports whether a profile may not set that kubelet.conf key.
func EngineOwnsKubeletKey(key string) bool { return engineKubeletKeys[key] }

// engineSysctls: the engine's drop-in sorts after a profile's, so the engine's value would win.
var engineSysctls = map[string]bool{
	"fs.inotify.max_user_instances":       true,
	"fs.inotify.max_user_watches":         true,
	"net.bridge.bridge-nf-call-ip6tables": true,
	"net.bridge.bridge-nf-call-iptables":  true,
	"net.ipv4.ip_forward":                 true,
	"net.ipv6.conf.all.forwarding":        true,
}

// EngineOwnsSysctl accepts dotted or slashed keys with an optional sysctl.d leading "-".
func EngineOwnsSysctl(key string) bool {
	key = strings.TrimPrefix(strings.TrimSpace(key), "-")
	return engineSysctls[strings.ReplaceAll(strings.TrimSpace(key), "/", ".")]
}

// EngineOwnsConfigName: an engine file or a payload import marker (<archive>.imported).
func EngineOwnsConfigName(name string) bool {
	return engineConfigNames[name] || strings.HasSuffix(name, ".imported")
}

// pkiExtensions are the extensions of files the engine keeps in its PKI directory.
var pkiExtensions = map[string]bool{".crt": true, ".key": true, ".pub": true, ".csr": true, ".pem": true}

// ReservedSecretPrefix starts the CA-rotation state files, which are read by presence.
const ReservedSecretPrefix = "carotate-"

// CISEncryptionConfigName is the one shared secret the engine reads by name; only cis declares it.
const CISEncryptionConfigName = "encryption-config.yaml"

// engineBundleNames are the join bundle's own non-certificate entries.
var engineBundleNames = map[string]bool{"netconfig": true, "cilium.json": true}

// ReservedSecretName: "version" in any case would make the bundle undecodable.
func ReservedSecretName(name string) bool {
	return engineBundleNames[name] || strings.EqualFold(name, "version")
}

// PlainFileName: a profile's file names are recorded one per line, so a name must round-trip.
func PlainFileName(name string) error {
	switch {
	case name == "" || path.Base(name) != name || name == "." || name == "..":
		return fmt.Errorf("file %q must be a bare file name", name)
	case !utf8.ValidString(name):
		return fmt.Errorf("file %q is not valid UTF-8", name)
	case strings.ContainsFunc(name, unicode.IsControl) || strings.ContainsRune(name, '\\'):
		return fmt.Errorf("file %q may not contain a control character, a newline or a backslash", name)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("file %q may not start with a dot", name)
	}
	return nil
}

func checkFileName(profile, name string, seen map[string]bool) error {
	if err := PlainFileName(name); err != nil {
		return fmt.Errorf("profile %q: %w", profile, err)
	}
	if seen[name] {
		return fmt.Errorf("profile %q: file %q is declared twice", profile, name)
	}
	seen[name] = true
	return nil
}

func checkSource(profile, what string, from Source, value string, files map[string]bool) error {
	switch from {
	case Literal:
		return nil
	case ConfigFile:
		if !files[value] {
			return fmt.Errorf("profile %q: %s names config file %q, which the profile does not declare", profile, what, value)
		}
		return nil
	case LogFile, PKIFile:
		if value == "" || path.Base(value) != value || strings.HasPrefix(value, ".") {
			return fmt.Errorf("profile %q: %s names %q, want a bare file name", profile, what, value)
		}
		return nil
	}
	return fmt.Errorf("profile %q: %s says nothing about where its value comes from", profile, what)
}

// ErrUnregistered is wrapped by Select when a name is not registered.
var ErrUnregistered = errors.New("profile is not registered in this binary")

var registered []Profile

// registrationErr is set when profile registration failed; ForNode refuses with it.
var registrationErr error

// SetRegistrationError records a profile-registration failure; nil clears it.
func SetRegistrationError(err error) { registrationErr = err }

// SetRegistered validates each profile and refuses a name declared twice.
func SetRegistered(ps []Profile) error {
	names := map[string]bool{}
	for _, p := range ps {
		if err := p.Validate(); err != nil {
			return err
		}
		if names[p.Name] {
			return fmt.Errorf("profile %q is registered twice", p.Name)
		}
		names[p.Name] = true
	}
	registered = append([]Profile(nil), ps...)
	return nil
}

// All returns a copy of the registered profiles, in registration order.
func All() []Profile { return append([]Profile(nil), registered...) }

// Registered returns the registered profiles' names, sorted.
func Registered() []string {
	names := make([]string, 0, len(registered))
	for _, p := range registered {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

// Lookup returns the registered profile of that name.
func Lookup(name string) (Profile, bool) {
	for _, p := range registered {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// ProDocsURL is the Stratum PRO documentation page; the address is written once.
const ProDocsURL = "https://docs.ciq.com/stratum/pro/"

// CISUnregisteredNote is appended to the cis refusal; tests match the constant, not its text.
const CISUnregisteredNote = "The cis profile is part of Stratum PRO; on a node that does not already run cis, remove --profile cis and any cis selection from the config; " +
	"for a node already running cis, move it onto a Stratum PRO binary with `stratum upgrade --from <binary>` (see " + ProDocsURL + ")"

// Select returns the profile a node runs or an error wrapping ErrUnregistered.
func Select(name string) (Profile, error) {
	if name == "" {
		return Profile{}, nil
	}
	if p, ok := Lookup(name); ok {
		return p, nil
	}
	have := "none"
	if names := Registered(); len(names) > 0 {
		have = strings.Join(names, ", ")
	}
	if name == CISName {
		return Profile{}, fmt.Errorf("profile %q: %w (registered: %s). %s",
			name, ErrUnregistered, have, CISUnregisteredNote)
	}
	return Profile{}, fmt.Errorf("profile %q: %w (registered: %s)", name, ErrUnregistered, have)
}

// ForNode returns the profile a node's state names, refusing one it cannot resolve.
func ForNode(name string) (Profile, error) {
	if name == "" {
		return Profile{}, nil
	}
	if registrationErr != nil {
		return Profile{}, fmt.Errorf("profile %q: the binary's profiles could not be registered: %w", name, registrationErr)
	}
	return Select(name)
}

// SecretNames returns the names of the profile's shared secrets, sorted.
func (p Profile) SecretNames() []string {
	names := make([]string, 0, len(p.SharedSecrets))
	for _, s := range p.SharedSecrets {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return names
}

// SharedSecretNames returns every registered profile's secret names, without repeats.
func SharedSecretNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, p := range registered {
		for _, s := range p.SharedSecrets {
			if !seen[s.Name] {
				seen[s.Name] = true
				names = append(names, s.Name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// None reports whether p is the absence of a profile.
func (p Profile) None() bool { return p.Name == "" }

// Locations resolves the names a profile's flags and settings use to engine paths.
type Locations struct {
	Config func(name string) string
	Secret func(name string) string
	Log    func(name string) string
	PKI    func(name string) string
}

// resolve returns the value an entry renders with.
func (p Profile) resolve(from Source, value string, at Locations) string {
	switch from {
	case ConfigFile:
		for _, s := range p.SharedSecrets {
			if s.Name == value {
				return at.Secret(value)
			}
		}
		return at.Config(value)
	case LogFile:
		return at.Log(value)
	case PKIFile:
		return at.PKI(value)
	}
	return value
}

// APIServerArgs is the profile's apiserver flags as argv entries, values resolved.
func (p Profile) APIServerArgs(at Locations) []string {
	out := make([]string, 0, len(p.APIServerFlags))
	for _, f := range p.APIServerFlags {
		out = append(out, f.Name+"="+p.resolve(f.From, f.Value, at))
	}
	return out
}

// KubeletLines is the profile's kubelet settings as kubelet.conf lines.
func (p Profile) KubeletLines(at Locations) string {
	var b strings.Builder
	for _, s := range p.KubeletSettings {
		b.WriteString(s.Key + ": " + p.resolve(s.From, s.Value, at) + "\n")
	}
	return b.String()
}

// AdmissionFile returns the profile's AdmissionConfiguration file, if declared.
func (p Profile) AdmissionFile() (File, bool) {
	for _, f := range p.ConfigFiles {
		if f.Kind == AdmissionConfiguration {
			return f, true
		}
	}
	return File{}, false
}

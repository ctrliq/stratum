// Package profile is the engine's notion of a node profile: a named set of
// hardened settings an importer registers through pkg/cli.WithProfiles and a
// node selects by name. The engine renders what a profile declares and
// nothing more: apiserver flags, kubelet settings, config files written
// before the apiserver, secrets minted once and carried in the HA join
// bundle, sysctls, and a few booleans the start and the lifecycle read.
//
// The package is a leaf. The renderers (internal/staticpod, internal/runtime,
// internal/host), the start (internal/cluster), the HA join and the upgrade
// import it, so it imports no stratum package back. A profile never names a
// path: a file is a name under a directory the engine owns, and a flag or a
// setting that needs a path says which kind of file it means.
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

// CISName is the name of the engine's own cis profile. The engine spells it
// in the few places that treat that profile specially (its legacy flag and
// state field, its sysctl drop-in) while it still ships one; they go with it.
const CISName = "cis"

// Source says where a flag's or a setting's value comes from. The zero value
// is invalid on purpose: an entry that fails to say reads as undeclared.
type Source int

const (
	// Literal uses the entry's Value as written.
	Literal Source = iota + 1
	// ConfigFile resolves Value, the name of one of the profile's ConfigFiles
	// or SharedSecrets, to the path the engine wrote it at.
	ConfigFile
	// LogFile resolves Value, a file name, to a path under the engine's log
	// directory.
	LogFile
	// PKIFile resolves Value, a file name, to a path under the engine's PKI
	// directory.
	PKIFile
)

// FileKind says how the engine treats a config file beyond writing it.
type FileKind int

const (
	// PlainFile is written and may be named by a ConfigFile flag.
	PlainFile FileKind = iota + 1
	// AdmissionConfiguration is an apiserver AdmissionConfiguration the engine
	// merges its own admission plugins into. The engine names the merged file
	// on the apiserver's argv itself; the profile does not.
	AdmissionConfiguration
)

// Context is what a profile may read when it renders a file.
type Context struct {
	// KubernetesVersion is the version the binary runs, as vMAJOR.MINOR.PATCH
	// with any pre-release suffix the stamp carries.
	KubernetesVersion string
}

// Flag is one apiserver flag, rendered on control planes.
type Flag struct {
	// Name is the flag with its leading --.
	Name string
	// Value is the literal, or what From resolves.
	Value string
	From  Source
}

// Setting is one kubelet.conf line, rendered on every node.
type Setting struct {
	// Key is the kubelet.conf key.
	Key string
	// Value is the literal, or what From resolves.
	Value string
	From  Source
}

// File is a config file the engine writes under its config directory on a
// control plane before the apiserver renders, backs up across an upgrade and
// restores first on a rollback.
type File struct {
	// Name is the file name; no directory.
	Name string
	Kind FileKind
	// Render returns the file's bytes.
	Render func(Context) ([]byte, error)
}

// Secret is a file the engine mints once, on a cluster's first control
// plane, and carries to every later control plane in the HA join bundle. It
// is written 0600 under the engine's PKI directory and never regenerated: a
// node that already holds it keeps it, and a join whose bundle lacks it while
// the cluster's profile declares it is refused.
type Secret struct {
	// Name is the file name; no directory.
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
	// ConfigFiles are written on control planes.
	ConfigFiles []File
	// SharedSecrets are minted on the first control plane and shared.
	SharedSecrets []Secret
	// Sysctls are key=value lines the engine writes as a sysctl drop-in on
	// every node. A node that cannot apply them does not start.
	Sysctls []string
	// RequiresOperator refuses a control plane that turned the operator off.
	RequiresOperator bool
	// RequiresSELinux refuses spec.selinux: false and treats a missing SELinux
	// policy as fatal.
	RequiresSELinux bool
	// KubeletServerTLSBootstrap makes every node, control planes included,
	// bootstrap its kubelet serving certificate through the CSR flow.
	KubeletServerTLSBootstrap bool
	// DisableNodeShell turns the dashboard's privileged node shell off by
	// default.
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

// engineConfigNames are the files and directories the engine keeps in the
// directory a profile's config files are written to.
var engineConfigNames = map[string]bool{
	"admin.conf":                true,
	"admission-webhook.yaml":    true,
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
	"selinux-confined-since":    true,
	"webhook-client.kubeconfig": true,
}

// engineAPIServerFlags are the apiserver flags the engine renders itself on
// every control plane. kube-apiserver takes the last value of a flag given
// twice, and a profile's flags follow the engine's, so a profile declaring
// one of these would replace the engine's value: --admission-control-config-file
// names the merged admission file that carries the engine's webhook client
// configuration, and a profile naming its own file there would drop it.
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

// EngineOwnsAPIServerFlag reports whether name is an apiserver flag the
// engine renders itself, which a profile may not declare.
func EngineOwnsAPIServerFlag(name string) bool { return engineAPIServerFlags[name] }

// engineKubeletKeys are the top-level kubelet.conf keys the engine writes
// itself. A profile setting of one of them would put the key in the file
// twice.
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
	"rotateCertificates":                     true,
	"serverTLSBootstrap":                     true,
	"staticPodPath":                          true,
	"streamingConnectionIdleTimeout":         true,
	"tlsCertFile":                            true,
	"tlsCipherSuites":                        true,
	"tlsPrivateKeyFile":                      true,
}

// EngineOwnsKubeletKey reports whether key is a top-level kubelet.conf key
// the engine writes itself, which a profile may not set.
func EngineOwnsKubeletKey(key string) bool { return engineKubeletKeys[key] }

// engineSysctls are the keys the engine sets in its own sysctl drop-in,
// 99-stratum.conf. A profile's drop-in sorts before it, so the engine's value
// would win over the profile's.
var engineSysctls = map[string]bool{
	"fs.inotify.max_user_instances":       true,
	"fs.inotify.max_user_watches":         true,
	"net.bridge.bridge-nf-call-ip6tables": true,
	"net.bridge.bridge-nf-call-iptables":  true,
	"net.ipv4.ip_forward":                 true,
	"net.ipv6.conf.all.forwarding":        true,
}

// EngineOwnsSysctl reports whether key, in dotted or slashed form and with or
// without sysctl.d's leading "-" (apply, ignoring a failure), is one the
// engine sets in its own sysctl drop-in.
func EngineOwnsSysctl(key string) bool {
	key = strings.TrimPrefix(strings.TrimSpace(key), "-")
	return engineSysctls[strings.ReplaceAll(strings.TrimSpace(key), "/", ".")]
}

// EngineOwnsConfigName reports whether name is a file or directory the
// engine keeps in its config directory, which a profile's config file of
// that name would replace: one of engineConfigNames, or a payload import
// marker (<archive>.imported).
func EngineOwnsConfigName(name string) bool {
	return engineConfigNames[name] || strings.HasSuffix(name, ".imported")
}

// pkiExtensions are the extensions of the files the engine keeps in its PKI
// directory, where a shared secret is written. A secret with one of them could
// take the name of a certificate or key the engine issues or shares.
var pkiExtensions = map[string]bool{".crt": true, ".key": true, ".pub": true, ".csr": true, ".pem": true}

// ReservedSecretPrefix starts the name of every CA-rotation state file the
// engine keeps in its PKI directory (carotate-forced, carotate-promoted and
// the rest). Those files are the rotation's state, read by their presence
// and removed by its cleanup, so a shared secret may not take one of their
// names.
const ReservedSecretPrefix = "carotate-"

// CISEncryptionConfigName is the cis profile's at-rest encryption config, the
// one shared secret the engine reads by name: the join bundle derives a proof
// of its key from the secretbox layout the cis profile writes, and the join
// refuses a bundle without it on a cis cluster. Another profile's secret of
// that name would be minted and then fail every upload, so only the cis
// profile may declare it.
const CISEncryptionConfigName = "encryption-config.yaml"

// engineBundleNames are the join bundle's own entries that are not
// certificates or keys: the cluster network config and the Cilium settings,
// which the engine writes into the PKI directory on its own, before a
// profile's secrets are minted and whatever a profile declares.
var engineBundleNames = map[string]bool{"netconfig": true, "cilium.json": true}

// ReservedSecretName reports whether a shared secret may not be named name:
// it is one of the join bundle's own non-certificate entries, or "version" in
// any case. The legacy flat-map bundle is decoded first for a "version" field
// to tell it from the v1 envelope, and Go's JSON decoder matches a field name
// whatever its case, so a secret of that name would make the bundle
// undecodable.
func ReservedSecretName(name string) bool {
	return engineBundleNames[name] || strings.EqualFold(name, "version")
}

// PlainFileName reports why name is not a plain file name a profile may
// declare: it must be a bare name, UTF-8, with no control character, newline
// or backslash. The engine records the names of a profile's files one per
// line (the upgrade's backup record), reads them back as path elements, and
// a name a record cannot round-trip would make a rollback restore or remove
// the wrong file.
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

// registrationErr is why the binary's profiles could not be registered, when
// they could not; ForNode returns it for any profile a node names.
var registrationErr error

// SetRegistrationError records that registering the binary's profiles
// failed, so a renderer that later needs a node's profile reports why
// instead of rendering the node without it. Nil clears it.
func SetRegistrationError(err error) { registrationErr = err }

// SetRegistered replaces the binary's registered profiles after validating
// each and refusing a name declared twice.
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

// ProDocsURL is the Stratum PRO documentation page, which lists what the
// commercial edition adds. Every message that sends an operator there uses
// this constant, so the address is written once.
const ProDocsURL = "https://docs.ciq.com/stratum/pro/"

// CISUnregisteredNote is appended to the ErrUnregistered error Select
// returns for the cis profile, which this engine does not carry: the note
// says the profile is part of Stratum PRO and gives the two ways forward,
// both through flags of this binary. Tests and wrappers match this constant,
// never a copy of its text.
const CISUnregisteredNote = "The cis profile is part of Stratum PRO; on a node that does not already run cis, remove --profile cis and any cis selection from the config; " +
	"for a node already running cis, move it onto a Stratum PRO binary with `stratum upgrade --from <binary>` (see " + ProDocsURL + ")"

// Select returns the profile a node runs: none for an empty name, the
// registered one for a known name, and an error wrapping ErrUnregistered
// that names both the profile and the binary's registered ones otherwise.
// For the cis profile, which this engine does not carry, the error also
// carries CISUnregisteredNote.
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

// ForNode returns the profile a node's state names, for a renderer: none for
// an empty name, and an error for a name that is not registered or when
// registering the binary's profiles failed. A renderer that cannot resolve
// the profile stops; it never renders the node without it.
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

// SharedSecretNames returns the names of every registered profile's shared
// secrets, sorted and without repeats: the profile entries an HA join bundle
// may carry.
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

// Locations resolves the file names a profile's flags and settings name to
// the paths the engine writes them at. The engine supplies it; the profile
// never names a path.
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

// APIServerArgs is the profile's apiserver flags as argv entries, in order,
// with their values resolved.
func (p Profile) APIServerArgs(at Locations) []string {
	out := make([]string, 0, len(p.APIServerFlags))
	for _, f := range p.APIServerFlags {
		out = append(out, f.Name+"="+p.resolve(f.From, f.Value, at))
	}
	return out
}

// KubeletLines is the profile's kubelet settings as kubelet.conf lines, in
// order, with their values resolved.
func (p Profile) KubeletLines(at Locations) string {
	var b strings.Builder
	for _, s := range p.KubeletSettings {
		b.WriteString(s.Key + ": " + p.resolve(s.From, s.Value, at) + "\n")
	}
	return b.String()
}

// AdmissionFile returns the profile's AdmissionConfiguration file, if it
// declares one.
func (p Profile) AdmissionFile() (File, bool) {
	for _, f := range p.ConfigFiles {
		if f.Kind == AdmissionConfiguration {
			return f, true
		}
	}
	return File{}, false
}

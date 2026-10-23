package cli

import (
	"fmt"

	"github.com/ctrliq/stratum/internal/profile"
)

// Profile is a named node profile an importer registers with WithProfiles.
// Registering validates the declaration and makes the profile selectable by
// name; the engine release's notes say which of its fields the node start
// reads and renders, as the engine's own profile moves onto this seam. Once
// selection is in place, a node selects a profile with `stratum up --profile
// <name>` or `spec.profile`, records the name in its state, and hands it to
// every worker's join line; a node whose state names a profile the running
// binary did not register refuses to start, so every node of a cluster must
// run a binary that carries its profile. An importer must not take a
// registered profile for an applied one on a release whose notes do not say
// the start reads it.
type Profile struct {
	// Name selects the profile: a DNS-1123 label, unique in the binary.
	Name string
	// APIServerFlags are spliced into the apiserver's argv on control planes,
	// in order.
	APIServerFlags []ProfileFlag
	// KubeletSettings are rendered into kubelet.conf on every node, in order.
	KubeletSettings []ProfileSetting
	// ConfigFiles are written under the engine's config directory on control
	// planes before the apiserver renders, backed up across an upgrade and
	// restored first on a rollback.
	ConfigFiles []ProfileFile
	// SharedSecrets are minted once, on a cluster's first control plane,
	// carried to every later control plane in the join bundle, written 0600
	// and never regenerated.
	SharedSecrets []ProfileSecret
	// Sysctls are key=value lines written as a sysctl drop-in on every node.
	// A node that cannot apply them does not start.
	Sysctls []string
	// RequiresOperator refuses a control plane with spec.operator: false.
	RequiresOperator bool
	// RequiresSELinux refuses spec.selinux: false and treats a missing
	// SELinux policy as fatal.
	RequiresSELinux bool
	// KubeletServerTLSBootstrap makes every node, control planes included,
	// bootstrap its kubelet serving certificate through the CSR flow.
	KubeletServerTLSBootstrap bool
	// DisableNodeShell turns the dashboard's privileged node shell off by
	// default.
	DisableNodeShell bool
}

// ProfileSource says where a flag's or a setting's value comes from.
type ProfileSource int

const (
	// ProfileLiteral uses Value as written.
	ProfileLiteral ProfileSource = ProfileSource(profile.Literal)
	// ProfileConfigFile resolves Value, the name of one of the profile's
	// ConfigFiles or SharedSecrets, to the path the engine wrote it at.
	ProfileConfigFile ProfileSource = ProfileSource(profile.ConfigFile)
	// ProfileLogFile resolves Value, a file name, to a path under the
	// engine's log directory.
	ProfileLogFile ProfileSource = ProfileSource(profile.LogFile)
	// ProfilePKIFile resolves Value, a file name, to a path under the
	// engine's PKI directory.
	ProfilePKIFile ProfileSource = ProfileSource(profile.PKIFile)
)

// ProfileFileKind says how the engine treats a config file beyond writing it.
type ProfileFileKind int

const (
	// ProfilePlainFile is written, and may be named by a ProfileConfigFile
	// flag.
	ProfilePlainFile ProfileFileKind = ProfileFileKind(profile.PlainFile)
	// ProfileAdmissionConfiguration is an apiserver AdmissionConfiguration
	// the engine merges its own admission plugins into. The engine names the
	// merged file on the apiserver's argv itself.
	ProfileAdmissionConfiguration ProfileFileKind = ProfileFileKind(profile.AdmissionConfiguration)
)

// ProfileContext is what a profile may read when it renders a file.
type ProfileContext struct {
	// KubernetesVersion is the version the binary runs, as vMAJOR.MINOR.PATCH
	// with any pre-release suffix the stamp carries.
	KubernetesVersion string
}

// ProfileFlag is one apiserver flag.
type ProfileFlag struct {
	// Name is the flag with its leading --; the value goes in Value.
	Name  string
	Value string
	From  ProfileSource
}

// ProfileSetting is one kubelet.conf line.
type ProfileSetting struct {
	// Key is the kubelet.conf key.
	Key string
	// Value is the setting's value, on one line.
	Value string
	From  ProfileSource
}

// ProfileFile is a config file the engine writes for the profile.
type ProfileFile struct {
	// Name is the file name; no directory, and not the name of a file the
	// engine keeps in its config directory, such as kubelet.conf.
	Name string
	Kind ProfileFileKind
	// Render returns the file's bytes.
	Render func(ProfileContext) ([]byte, error)
}

// ProfileSecret is a file the engine mints once and shares between control
// planes.
type ProfileSecret struct {
	// Name is the file name; no directory.
	Name string
	// Mint returns the bytes a new cluster starts with.
	Mint func() ([]byte, error)
}

// WithProfiles registers node profiles a node may select by name. A profile
// that is not declared completely, or a name registered twice, fails the
// node commands that read profiles.
func WithProfiles(ps ...Profile) Option {
	return func(c *rootConfig) {
		c.profiles = append(c.profiles, ps...)
	}
}

func importerProfiles(ps []Profile) ([]profile.Profile, error) {
	out := make([]profile.Profile, 0, len(ps))
	for _, p := range ps {
		out = append(out, toProfile(p))
	}
	names := map[string]bool{}
	for _, p := range out {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if names[p.Name] {
			return nil, fmt.Errorf("profile %q is registered twice", p.Name)
		}
		names[p.Name] = true
	}
	return out, nil
}

func toProfile(p Profile) profile.Profile {
	out := profile.Profile{
		Name:                      p.Name,
		Sysctls:                   append([]string(nil), p.Sysctls...),
		RequiresOperator:          p.RequiresOperator,
		RequiresSELinux:           p.RequiresSELinux,
		KubeletServerTLSBootstrap: p.KubeletServerTLSBootstrap,
		DisableNodeShell:          p.DisableNodeShell,
	}
	for _, f := range p.APIServerFlags {
		out.APIServerFlags = append(out.APIServerFlags, profile.Flag{Name: f.Name, Value: f.Value, From: profile.Source(f.From)})
	}
	for _, s := range p.KubeletSettings {
		out.KubeletSettings = append(out.KubeletSettings, profile.Setting{Key: s.Key, Value: s.Value, From: profile.Source(s.From)})
	}
	for _, f := range p.ConfigFiles {
		render := f.Render
		var fn func(profile.Context) ([]byte, error)
		if render != nil {
			fn = func(c profile.Context) ([]byte, error) { return render(ProfileContext(c)) }
		}
		out.ConfigFiles = append(out.ConfigFiles, profile.File{Name: f.Name, Kind: profile.FileKind(f.Kind), Render: fn})
	}
	for _, s := range p.SharedSecrets {
		out.SharedSecrets = append(out.SharedSecrets, profile.Secret{Name: s.Name, Mint: s.Mint})
	}
	return out
}

// registerProfiles is what the node commands call before they read a
// profile: it validates the importer's declarations and makes them the
// binary's registered set.
func registerProfiles(cfg rootConfig) error {
	ps, err := importerProfiles(cfg.profiles)
	if err != nil {
		return err
	}
	return profile.SetRegistered(ps)
}

// registerProfilesForRender registers the importer's profiles for every
// command, not only up: upgrade, the certs commands, ha repoint and snapshot
// restore render static pods and kubelet.conf too, and must render a node's
// profile as up does. A declaration that does not validate is recorded rather
// than returned, so a command that renders nothing, such as version or help,
// still runs; a renderer that needs a profile gets the reason from
// profile.ForNode. up validates again and refuses to start.
func registerProfilesForRender(cfg rootConfig) {
	profile.SetRegistrationError(registerProfiles(cfg))
}

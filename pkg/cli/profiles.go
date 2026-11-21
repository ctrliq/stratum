package cli

import (
	"fmt"

	"github.com/ctrliq/stratum/internal/profile"
)

// A node refuses to start if its state names a profile this binary did not register.
type Profile struct {
	// A DNS-1123 label, unique in the binary.
	Name string
	// Spliced into the apiserver's argv on control planes, in order.
	APIServerFlags []ProfileFlag
	// Rendered into kubelet.conf on every node, in order.
	KubeletSettings []ProfileSetting
	// Written in the engine's config dir on control planes, restored first on a rollback.
	ConfigFiles []ProfileFile
	// Minted once on the first control plane, carried in the join bundle, never regenerated.
	SharedSecrets []ProfileSecret
	// A node that cannot apply these does not start.
	Sysctls []string
	// Refuses a control plane with spec.operator: false.
	RequiresOperator bool
	// Refuses spec.selinux: false and treats a missing SELinux policy as fatal.
	RequiresSELinux bool
	// Bootstraps the kubelet serving certificate through the CSR flow, control planes included.
	KubeletServerTLSBootstrap bool
	// Turns the dashboard's privileged node shell off by default.
	DisableNodeShell bool
}

type ProfileSource int

const (
	// Uses Value as written.
	ProfileLiteral ProfileSource = ProfileSource(profile.Literal)
	// Resolves Value, a ConfigFiles or SharedSecrets name, to the path the engine wrote it at.
	ProfileConfigFile ProfileSource = ProfileSource(profile.ConfigFile)
	// Resolves Value, a file name, to a path under the engine's log directory.
	ProfileLogFile ProfileSource = ProfileSource(profile.LogFile)
	// Resolves Value, a file name, to a path under the engine's PKI directory.
	ProfilePKIFile ProfileSource = ProfileSource(profile.PKIFile)
)

// How the engine treats a config file beyond writing it.
type ProfileFileKind int

const (
	// Written, and may be named by a ProfileConfigFile flag.
	ProfilePlainFile ProfileFileKind = ProfileFileKind(profile.PlainFile)
	// The engine merges its own admission plugins in and names the merged file on the argv.
	ProfileAdmissionConfiguration ProfileFileKind = ProfileFileKind(profile.AdmissionConfiguration)
)

// ProfileContext is what a profile may read when it renders a file.
type ProfileContext struct {
	// The version the binary runs, vMAJOR.MINOR.PATCH with any stamp pre-release suffix.
	KubernetesVersion string
}

// ProfileFlag is one apiserver flag.
type ProfileFlag struct {
	// Includes its leading --; the value goes in Value.
	Name  string
	Value string
	From  ProfileSource
}

// ProfileSetting is one kubelet.conf line.
type ProfileSetting struct {
	Key string
	// On one line.
	Value string
	From  ProfileSource
}

type ProfileFile struct {
	// No directory, and not a file the engine keeps in its config dir, such as kubelet.conf.
	Name   string
	Kind   ProfileFileKind
	Render func(ProfileContext) ([]byte, error)
}

type ProfileSecret struct {
	// No directory.
	Name string
	// Returns the bytes a new cluster starts with.
	Mint func() ([]byte, error)
}

// An incomplete declaration or a duplicate name fails the node commands that read profiles.
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

func registerProfiles(cfg rootConfig) error {
	ps, err := importerProfiles(cfg.profiles)
	if err != nil {
		return err
	}
	return profile.SetRegistered(ps)
}

// Records validation errors instead of returning them, so commands that render nothing still run.
func registerProfilesForRender(cfg rootConfig) {
	profile.SetRegistrationError(registerProfiles(cfg))
}

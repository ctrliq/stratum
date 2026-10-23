// Package profiletest gives tests a hardened node profile shaped like the
// cis profile a Stratum PRO binary registers: an audit policy and an admission
// configuration as config files, an at-rest encryption config as a shared
// secret, kernel sysctls, and every property set. The engine carries no
// profile of its own, so tests that exercise the profile seam register this
// one. It is named cis because the engine's legacy readers (`--cis`,
// `spec.cis`, a node state's `cis: true`) resolve to that name.
package profiletest

import (
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

// The fixture's name and the file names its derivations resolve to.
const (
	Name                 = "cis"
	AuditPolicyFile      = "audit-policy.yaml"
	AdmissionConfigFile  = "admission-config.yaml"
	EncryptionConfigFile = "encryption-config.yaml"
	AuditLogFile         = "audit.log"
)

// Sysctls is the fixture's node sysctl set.
var Sysctls = []string{
	"vm.overcommit_memory=1",
	"vm.panic_on_oom=0",
	"kernel.panic=10",
	"kernel.panic_on_oops=1",
	"kernel.keys.root_maxbytes=25000000",
	"kernel.keys.root_maxkeys=1000000",
	"kernel.kptr_restrict=1",
	"kernel.dmesg_restrict=1",
	"kernel.yama.ptrace_scope=1",
	"net.ipv4.conf.all.rp_filter=1",
}

// Hardened is the fixture with the given file bodies and secret: files are
// its ConfigFiles, in the order they are written; secrets its SharedSecrets.
func Hardened(files []profile.File, secrets []profile.Secret) profile.Profile {
	return profile.Profile{
		Name: Name,
		APIServerFlags: []profile.Flag{
			{Name: "--audit-policy-file", Value: AuditPolicyFile, From: profile.ConfigFile},
			{Name: "--audit-log-path", Value: AuditLogFile, From: profile.LogFile},
			{Name: "--audit-log-maxage", Value: "30", From: profile.Literal},
			{Name: "--audit-log-maxbackup", Value: "10", From: profile.Literal},
			{Name: "--audit-log-maxsize", Value: "100", From: profile.Literal},
			{Name: "--encryption-provider-config", Value: EncryptionConfigFile, From: profile.ConfigFile},
		},
		KubeletSettings: []profile.Setting{
			{Key: "protectKernelDefaults", Value: "true", From: profile.Literal},
			{Key: "seccompDefault", Value: "true", From: profile.Literal},
		},
		ConfigFiles:               files,
		SharedSecrets:             secrets,
		Sysctls:                   append([]string(nil), Sysctls...),
		RequiresOperator:          true,
		RequiresSELinux:           true,
		KubeletServerTLSBootstrap: true,
		DisableNodeShell:          true,
	}
}

// Profile is the fixture with stub file bodies and secret.
func Profile() profile.Profile {
	stub := func(profile.Context) ([]byte, error) { return nil, nil }
	return Hardened(
		[]profile.File{
			{Name: AuditPolicyFile, Kind: profile.PlainFile, Render: stub},
			{Name: AdmissionConfigFile, Kind: profile.AdmissionConfiguration, Render: stub},
		},
		[]profile.Secret{{Name: EncryptionConfigFile, Mint: func() ([]byte, error) { return nil, nil }}},
	)
}

// For is the profile a node with the legacy CIS flag runs: the fixture, or none.
func For(cis bool) profile.Profile {
	if cis {
		return Profile()
	}
	return profile.Profile{}
}

// Register registers the fixture for the test, as a binary's root command
// does, and puts the registry back as it was when the test ends.
func Register(t testing.TB) {
	t.Helper()
	prev := profile.All()
	if err := profile.SetRegistered([]profile.Profile{Profile()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

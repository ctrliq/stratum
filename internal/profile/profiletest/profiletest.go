// Package profiletest gives tests a hardened profile named cis, per the legacy readers.
package profiletest

import (
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

const (
	Name                 = "cis"
	AuditPolicyFile      = "audit-policy.yaml"
	AdmissionConfigFile  = "admission-config.yaml"
	EncryptionConfigFile = "encryption-config.yaml"
	AuditLogFile         = "audit.log"
)

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

func Register(t testing.TB) {
	t.Helper()
	prev := profile.All()
	if err := profile.SetRegistered([]profile.Profile{Profile()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

package profile

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func render(Context) ([]byte, error) { return []byte("x"), nil }
func mint() ([]byte, error)          { return []byte("k"), nil }

func full() Profile {
	return Profile{
		Name: "cis",
		APIServerFlags: []Flag{
			{"--audit-policy-file", "audit-policy.yaml", ConfigFile},
			{"--audit-log-path", "audit.log", LogFile},
			{"--audit-log-maxage", "30", Literal},
			{"--encryption-provider-config", "encryption-config.yaml", ConfigFile},
			{"--oidc-ca-file", "oidc-ca.crt", PKIFile},
		},
		KubeletSettings: []Setting{
			{"protectKernelDefaults", "true", Literal},
			{"exampleCAFile", "kubelet-ca.crt", PKIFile},
		},
		ConfigFiles: []File{
			{"audit-policy.yaml", PlainFile, render},
			{"admission-config.yaml", AdmissionConfiguration, render},
		},
		SharedSecrets:             []Secret{{"encryption-config.yaml", mint}},
		Sysctls:                   []string{"vm.overcommit_memory=1", "kernel.panic=10"},
		RequiresOperator:          true,
		RequiresSELinux:           true,
		KubeletServerTLSBootstrap: true,
		DisableNodeShell:          true,
	}
}

func TestValidateAcceptsAFullProfile(t *testing.T) {
	if err := full().Validate(); err != nil {
		t.Fatalf("Validate = %v", err)
	}
}

func TestValidateRefusesEachBrokenDeclaration(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Profile)
		wants string
	}{
		{"an upper-case name", func(p *Profile) { p.Name = "CIS" }, "DNS-1123"},
		{"an empty name", func(p *Profile) { p.Name = "" }, "DNS-1123"},
		{"a file with a directory", func(p *Profile) { p.ConfigFiles[0].Name = "etc/audit.yaml" }, "bare file name"},
		{"a dot file", func(p *Profile) { p.ConfigFiles[0].Name = ".audit.yaml" }, "start with a dot"},
		{"a file with a newline", func(p *Profile) { p.ConfigFiles[0].Name = "policy.yaml\nnode.yaml" }, "control character, a newline or a backslash"},
		{"a file with a backslash", func(p *Profile) { p.ConfigFiles[0].Name = `policy\node.yaml` }, "control character, a newline or a backslash"},
		{"a file with a control character", func(p *Profile) { p.ConfigFiles[0].Name = "policy\x01.yaml" }, "control character, a newline or a backslash"},
		{"a secret with a newline", func(p *Profile) { p.SharedSecrets[0].Name = "key\n.yaml" }, "control character, a newline or a backslash"},
		{"a file that is not UTF-8", func(p *Profile) { p.ConfigFiles[0].Name = "policy\xff.yaml" }, "not valid UTF-8"},
		{"a file declared twice", func(p *Profile) { p.SharedSecrets[0].Name = "audit-policy.yaml" }, "declared twice"},
		{"a file without a kind", func(p *Profile) { p.ConfigFiles[0].Kind = 0 }, "no kind"},
		{"a file without Render", func(p *Profile) { p.ConfigFiles[0].Render = nil }, "no Render"},
		{"a secret without Mint", func(p *Profile) { p.SharedSecrets[0].Mint = nil }, "no Mint"},
		{"a secret named like a key", func(p *Profile) { p.SharedSecrets[0].Name = "ca.key" }, "extension .key"},
		{"a secret named like a certificate", func(p *Profile) { p.SharedSecrets[0].Name = "extra.crt" }, "extension .crt"},
		{"a secret named like the forced-rotation marker", func(p *Profile) { p.SharedSecrets[0].Name = "carotate-forced" }, "CA-rotation state"},
		{"a secret named like the promoted marker", func(p *Profile) { p.SharedSecrets[0].Name = "carotate-promoted" }, "CA-rotation state"},
		{"a secret named like the network config", func(p *Profile) { p.SharedSecrets[0].Name = "netconfig" }, "join bundle uses itself"},
		{"a secret named like the Cilium settings", func(p *Profile) { p.SharedSecrets[0].Name = "cilium.json" }, "join bundle uses itself"},
		{"another profile declaring the cis at-rest config", func(p *Profile) { p.Name = "stig" }, "only the cis profile may declare it"},
		{"a secret named version", func(p *Profile) { p.SharedSecrets[0].Name = "version" }, "join bundle uses itself"},
		{"a secret named Version", func(p *Profile) { p.SharedSecrets[0].Name = "Version" }, "join bundle uses itself"},
		{"a flag without dashes", func(p *Profile) { p.APIServerFlags[2].Name = "audit-log-maxage" }, "--name"},
		{"a flag with its value in the name", func(p *Profile) { p.APIServerFlags[2].Name = "--audit-log-maxage=30" }, "--name"},
		{"a flag naming an undeclared file", func(p *Profile) { p.APIServerFlags[0].Value = "policy.yaml" }, "does not declare"},
		{"a flag without a source", func(p *Profile) { p.APIServerFlags[2].From = 0 }, "where its value comes from"},
		{"the engine's admission flag", func(p *Profile) {
			p.APIServerFlags[2] = Flag{"--admission-control-config-file", "admission-config.yaml", ConfigFile}
		}, "engine renders"},
		{"an engine flag with a literal", func(p *Profile) { p.APIServerFlags[2] = Flag{"--authorization-mode", "AlwaysAllow", Literal} }, "engine renders"},
		{"a flag declared twice", func(p *Profile) { p.APIServerFlags[3].Name = p.APIServerFlags[2].Name }, "declared twice"},
		{"a PKI file with a directory", func(p *Profile) { p.APIServerFlags[4].Value = "../ca.crt" }, "bare file name"},
		{"a kubelet key with a colon", func(p *Profile) { p.KubeletSettings[0].Key = "protectKernelDefaults: true" }, "bare kubelet.conf key"},
		{"a sysctl without a value", func(p *Profile) { p.Sysctls[0] = "vm.overcommit_memory" }, "key=value"},
		{"a sysctl with a comment", func(p *Profile) { p.Sysctls[0] = "kernel.panic=10 # x" }, "key=value"},
		{"a sysctl the engine's drop-in sets", func(p *Profile) { p.Sysctls[0] = "fs.inotify.max_user_watches=1048576" }, "99-stratum.conf"},
		{"an engine sysctl in slashed form", func(p *Profile) { p.Sysctls[0] = "net/ipv4/ip_forward = 0" }, "99-stratum.conf"},
		{"an engine sysctl marked ignore-errors", func(p *Profile) { p.Sysctls[0] = "-net.ipv4.ip_forward=0" }, "99-stratum.conf"},
		{"an engine sysctl marked ignore-errors in slashed form", func(p *Profile) { p.Sysctls[0] = " -net/ipv4/ip_forward = 0" }, "99-stratum.conf"},
		{"a config file named after the engine's kubelet.conf", func(p *Profile) { p.ConfigFiles[0].Name = "kubelet.conf"; p.APIServerFlags[0].Value = "kubelet.conf" }, "engine keeps"},
		{"a config file named after the engine's admission file", func(p *Profile) { p.ConfigFiles[1].Name = "admission-webhook.yaml" }, "engine keeps"},
		{"a kubelet key the engine writes", func(p *Profile) { p.KubeletSettings[0].Key = "staticPodPath" }, "engine writes"},
		{"the serving-cert key the engine decides", func(p *Profile) { p.KubeletSettings[0].Key = "serverTLSBootstrap" }, "engine writes"},
		{"a kubelet key declared twice", func(p *Profile) { p.KubeletSettings[1].Key = p.KubeletSettings[0].Key }, "declared twice"},
		{"a kubelet value with a newline", func(p *Profile) { p.KubeletSettings[0].Value = "true\nauthentication: {anonymous: {enabled: true}}" }, "line break"},
		{"a kubelet value with a carriage return", func(p *Profile) { p.KubeletSettings[0].Value = "true\rx: y" }, "line break"},
	}
	for _, c := range cases {
		p := full()
		c.edit(&p)
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), c.wants) {
			t.Errorf("%s: Validate = %v, want an error containing %q", c.name, err, c.wants)
		}
	}
}

func TestRegistryValidatesAndRefusesDuplicateNames(t *testing.T) {
	t.Cleanup(func() { registered = nil })
	p := full()
	if err := SetRegistered([]Profile{p, p}); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Errorf("two profiles named cis = %v, want a refusal", err)
	}
	bad := full()
	bad.Name = "Bad"
	if err := SetRegistered([]Profile{bad}); err == nil {
		t.Error("an invalid profile registered")
	}
	other := full()
	other.Name = "stig"
	other.SharedSecrets[0].Name = "stig-key.yaml"
	other.APIServerFlags[3].Value = "stig-key.yaml"
	if err := SetRegistered([]Profile{other, p}); err != nil {
		t.Fatalf("SetRegistered = %v", err)
	}
	if got := Registered(); strings.Join(got, ",") != "cis,stig" {
		t.Errorf("Registered = %v, want the names sorted", got)
	}
	if got, ok := Lookup("stig"); !ok || got.Name != "stig" {
		t.Errorf("Lookup(stig) = %+v, %v", got, ok)
	}
}

func TestSelectNamesTheMissingProfileAndTheRegisteredOnes(t *testing.T) {
	t.Cleanup(func() { registered = nil })
	if p, err := Select(""); err != nil || !p.None() {
		t.Errorf("Select(\"\") = %+v, %v; want no profile and no error", p, err)
	}
	if _, err := Select("cis"); !errors.Is(err, ErrUnregistered) || !strings.Contains(err.Error(), `"cis"`) || !strings.Contains(err.Error(), "none") {
		t.Errorf("Select(cis) with nothing registered = %v", err)
	}
	if _, err := Select("cis"); err == nil || !strings.Contains(err.Error(), CISUnregisteredNote) ||
		strings.Contains(err.Error(), "was changed") ||
		strings.Contains(err.Error(), "hardening check") {
		t.Errorf("Select(cis) with nothing registered = %v, want it to carry the cis note and not to vouch for the host", err)
	}
	if _, err := Select("stig"); !errors.Is(err, ErrUnregistered) || strings.Contains(err.Error(), CISUnregisteredNote) {
		t.Errorf("Select(stig) with nothing registered = %v, want the plain refusal", err)
	}
	if err := SetRegistered([]Profile{full()}); err != nil {
		t.Fatal(err)
	}
	if p, err := Select("cis"); err != nil || p.Name != "cis" || p.None() {
		t.Errorf("Select(cis) = %+v, %v", p, err)
	}
	if _, err := Select("stig"); !errors.Is(err, ErrUnregistered) || !strings.Contains(err.Error(), "registered: cis") {
		t.Errorf("Select(stig) = %v, want a refusal listing cis", err)
	}
}

func TestForNodeRefusesAProfileItCannotResolve(t *testing.T) {
	prev := All()
	t.Cleanup(func() {
		SetRegistrationError(nil)
		_ = SetRegistered(prev)
	})
	if err := SetRegistered([]Profile{full()}); err != nil {
		t.Fatal(err)
	}

	if p, err := ForNode(""); err != nil || !p.None() {
		t.Errorf("ForNode(\"\") = %+v, %v, want no profile", p, err)
	}
	if p, err := ForNode("cis"); err != nil || p.Name != "cis" {
		t.Errorf("ForNode(cis) = %q, %v, want the registered cis", p.Name, err)
	}
	if _, err := ForNode("edge"); !errors.Is(err, ErrUnregistered) {
		t.Errorf("ForNode(edge) = %v, want ErrUnregistered", err)
	}

	cause := errors.New("profile \"CIS\" is not a DNS-1123 label")
	SetRegistrationError(cause)
	if _, err := ForNode("cis"); !errors.Is(err, cause) {
		t.Errorf("ForNode(cis) after registration failed = %v, want the registration error", err)
	}
	if p, err := ForNode(""); err != nil || !p.None() {
		t.Errorf("ForNode(\"\") after registration failed = %+v, %v, want no profile", p, err)
	}
}

// SharedSecretNames is every registered profile's secrets, sorted and
// without repeats, and SecretNames one profile's, sorted.
func TestSharedSecretNames(t *testing.T) {
	prev := All()
	t.Cleanup(func() { _ = SetRegistered(prev) })
	mint := func() ([]byte, error) { return nil, nil }
	a := Profile{Name: "a", SharedSecrets: []Secret{{Name: "z.yaml", Mint: mint}, {Name: "k.yaml", Mint: mint}}}
	b := Profile{Name: "b", SharedSecrets: []Secret{{Name: "k.yaml", Mint: mint}}}
	if err := SetRegistered([]Profile{a, b, {Name: "c"}}); err != nil {
		t.Fatal(err)
	}
	if got := SharedSecretNames(); !slices.Equal(got, []string{"k.yaml", "z.yaml"}) {
		t.Errorf("SharedSecretNames = %v", got)
	}
	if got := a.SecretNames(); !slices.Equal(got, []string{"k.yaml", "z.yaml"}) {
		t.Errorf("SecretNames = %v", got)
	}
	if got := (Profile{}).SecretNames(); len(got) != 0 {
		t.Errorf("no profile's SecretNames = %v", got)
	}
}

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profilefiles"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

func tempLayout(t *testing.T) string {
	t.Helper()
	data, conf, run := version.DataDir, version.ConfDir, version.RunDir
	lock, start := version.NodeUpgradeLockPath, version.StartLockPath
	t.Cleanup(func() {
		version.SetRoots(data, conf, run)
		version.NodeUpgradeLockPath, version.StartLockPath = lock, start
	})
	root := t.TempDir()
	version.SetRoots(filepath.Join(root, "data"), filepath.Join(root, "conf"), filepath.Join(root, "run"))
	return root
}

func TestLayoutFollowsTheRoots(t *testing.T) {
	root := tempLayout(t)
	l := Layout()
	at := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	for name, pair := range map[string][2]string{
		"ConfDir":                      {l.ConfDir, at("conf")},
		"DataDir":                      {l.DataDir, at("data")},
		"PKIDir":                       {l.PKIDir, at("data", "pki")},
		"KubeconfigDir":                {l.KubeconfigDir, at("data", "kubeconfig")},
		"K8sConfigDir":                 {l.K8sConfigDir, at("data", "config")},
		"StaticPodDir":                 {l.StaticPodDir, at("data", "config", "manifests")},
		"KubeletConfig":                {l.KubeletConfig, at("data", "config", "kubelet.conf")},
		"AdminConf":                    {l.AdminConf, at("data", "kubeconfig", "admin.conf")},
		"SuperAdminConf":               {l.SuperAdminConf, at("data", "kubeconfig", "super-admin.conf")},
		"BinDir":                       {l.BinDir, at("data", "bin")},
		"ContainerdConfig":             {l.ContainerdConfig, at("data", "config", "containerd.toml")},
		"ContainerdConfinedSinceStamp": {l.ContainerdConfinedSinceStamp, at("data", "config", "selinux-confined-since")},
		"SELinuxLabelStamp":            {l.SELinuxLabelStamp, at("data", "selinux-labels")},
		"EtcdDataDir":                  {l.EtcdDataDir, at("data", "etcd")},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
}

func TestPKIRotationCopies(t *testing.T) {
	got := PKIRotationCopies("front-proxy-ca.key")
	want := []string{"front-proxy-ca-new.key", "front-proxy-ca.pre-carotate.key"}
	if !slices.Equal(got, want) {
		t.Errorf("PKIRotationCopies(front-proxy-ca.key) = %q, want %q", got, want)
	}
	if got := PKIRotationCopies("front-proxy-client.key"); got != nil {
		t.Errorf("PKIRotationCopies(front-proxy-client.key) = %q, want none: a rotation reissues the leaf in place", got)
	}
}

func renderControlPlane(t *testing.T, p Profile) ([]string, []byte) {
	t.Helper()
	tempLayout(t)
	registerProfilesForTest(t, toProfile(p))
	for _, d := range []string{version.PKIDir, version.K8sConfDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestCA(t, "ca-bundle")
	st := &nodestate.NodeState{
		Role: "controller", Hostname: "cp1", NodeIP: "192.0.2.10",
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12", Profile: p.Name,
		EtcdName: "cp1", EtcdInitialCluster: "cp1=https://192.0.2.10:2380", EtcdState: "new",
	}
	if err := profilefiles.Write(toProfile(p)); err != nil {
		t.Fatal(err)
	}
	if err := staticpod.WriteAll(st); err != nil {
		t.Fatal(err)
	}
	if err := runtime.WriteKubeletConfig(st); err != nil {
		t.Fatal(err)
	}
	spec, err := staticpod.ReadContainerSpec("kube-apiserver")
	if err != nil {
		t.Fatal(err)
	}
	conf, err := os.ReadFile(Layout().KubeletConfig)
	if err != nil {
		t.Fatal(err)
	}
	return spec.Command, conf
}

func renderProfile() Profile {
	return Profile{
		Name: "render-test",
		APIServerFlags: []ProfileFlag{
			{"--audit-policy-file", "audit-policy.yaml", ProfileConfigFile},
			{"--audit-log-path", "audit.log", ProfileLogFile},
			{"--audit-log-maxage", "30", ProfileLiteral},
			{"--service-account-extend-token-expiration", "false", ProfileLiteral},
			{"--encryption-provider-config", "render-secret.yaml", ProfilePKIFile},
		},
		ConfigFiles: []ProfileFile{
			{"audit-policy.yaml", ProfilePlainFile, func(ProfileContext) ([]byte, error) { return []byte("policy"), nil }},
		},
		SharedSecrets: []ProfileSecret{{"render-secret.yaml", func() ([]byte, error) { return []byte("key"), nil }}},
	}
}

func TestTLSCipherSuitesMatchTheRender(t *testing.T) {
	argv, conf := renderControlPlane(t, renderProfile())
	got := TLSCipherSuites()
	if len(got) == 0 {
		t.Fatal("TLSCipherSuites is empty")
	}
	if !slices.Contains(argv, "--tls-cipher-suites="+strings.Join(got, ",")) {
		t.Errorf("the rendered apiserver argv carries no --tls-cipher-suites equal to %q:\n%s", got, strings.Join(argv, "\n"))
	}
	var kubelet struct {
		TLSCipherSuites []string `json:"tlsCipherSuites"`
	}
	if err := yaml.Unmarshal(conf, &kubelet); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kubelet.TLSCipherSuites, got) {
		t.Errorf("kubelet.conf tlsCipherSuites = %q, want %q", kubelet.TLSCipherSuites, got)
	}
	got[0] = "mutated"
	if TLSCipherSuites()[0] == "mutated" {
		t.Error("TLSCipherSuites returned the engine's own slice, not a copy")
	}
}

func TestEngineAPIServerArgsAreRendered(t *testing.T) {
	argv, _ := renderControlPlane(t, renderProfile())
	engine := EngineAPIServerArgs()
	if len(engine) == 0 {
		t.Fatal("EngineAPIServerArgs is empty")
	}
	for _, a := range engine {
		if !slices.Contains(argv, a) {
			t.Errorf("%q is not in the rendered apiserver argv:\n%s", a, strings.Join(argv, "\n"))
		}
	}
}

func TestProfileAPIServerArgsMatchTheRender(t *testing.T) {
	p := renderProfile()
	argv, _ := renderControlPlane(t, p)
	got, err := ProfileAPIServerArgs(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(p.APIServerFlags) {
		t.Fatalf("ProfileAPIServerArgs = %q, want one entry per declared flag (%d)", got, len(p.APIServerFlags))
	}
	at := slices.Index(argv, EngineAPIServerArgs()[0])
	if at < len(got) {
		t.Fatalf("the engine's flags are not where the render puts them:\n%s", strings.Join(argv, "\n"))
	}
	if rendered := argv[at-len(got) : at]; !slices.Equal(rendered, got) {
		t.Errorf("the rendered profile slice is %q, ProfileAPIServerArgs is %q", rendered, got)
	}
}

func TestProfileAPIServerArgsRefusesAnInvalidProfile(t *testing.T) {
	p := renderProfile()
	p.Name = "Not_A_Label"
	got, err := ProfileAPIServerArgs(p)
	if err == nil || !strings.Contains(err.Error(), "DNS-1123") {
		t.Errorf("ProfileAPIServerArgs on an invalid profile = %q, %v; want the validation error", got, err)
	}
}

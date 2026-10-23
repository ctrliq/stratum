package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

func cisLike() Profile {
	return Profile{
		Name: "cis",
		APIServerFlags: []ProfileFlag{
			{"--audit-policy-file", "audit-policy.yaml", ProfileConfigFile},
			{"--audit-log-path", "audit.log", ProfileLogFile},
			{"--encryption-provider-config", "encryption-config.yaml", ProfileConfigFile},
		},
		KubeletSettings: []ProfileSetting{{"protectKernelDefaults", "true", ProfileLiteral}},
		ConfigFiles: []ProfileFile{
			{"audit-policy.yaml", ProfilePlainFile, func(ProfileContext) ([]byte, error) { return []byte("policy"), nil }},
			{"admission-config.yaml", ProfileAdmissionConfiguration, func(c ProfileContext) ([]byte, error) { return []byte(c.KubernetesVersion), nil }},
		},
		SharedSecrets:             []ProfileSecret{{"encryption-config.yaml", func() ([]byte, error) { return []byte("key"), nil }}},
		Sysctls:                   []string{"vm.overcommit_memory=1"},
		RequiresOperator:          true,
		RequiresSELinux:           true,
		KubeletServerTLSBootstrap: true,
		DisableNodeShell:          true,
	}
}

func TestWithProfilesReachesTheNodeStart(t *testing.T) {
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
		{&version.K8sVersion, "v1.37.1"},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	oldInstall, oldRunController, oldPrepare := installUnit, runControllerFn, prepareControllerFn
	t.Cleanup(func() {
		installUnit, runControllerFn, prepareControllerFn = oldInstall, oldRunController, oldPrepare
	})
	isolateProfileRegistryForTest(t)
	old := syscall.Umask(0)
	syscall.Umask(old)
	t.Cleanup(func() { syscall.Umask(old) })

	installUnit = func([]string) error { return nil }
	prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
		return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
	}
	var got profile.Profile
	runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error {
		p, ok := profile.Lookup("cis")
		if !ok {
			return errors.New("the control-plane start has no cis profile registered")
		}
		got = p
		return nil
	}

	root := NewRootCommand(WithProfiles(cisLike()))
	root.SetArgs([]string{"up", "--config", writeEmptyConfig(t, tmp)})
	if err := root.Execute(); err != nil {
		t.Fatalf("up = %v", err)
	}
	if got.Name != "cis" || len(got.APIServerFlags) != 3 || got.APIServerFlags[1].From != profile.LogFile ||
		len(got.KubeletSettings) != 1 || len(got.ConfigFiles) != 2 || got.ConfigFiles[1].Kind != profile.AdmissionConfiguration ||
		len(got.SharedSecrets) != 1 || len(got.Sysctls) != 1 ||
		!got.RequiresOperator || !got.RequiresSELinux || !got.KubeletServerTLSBootstrap || !got.DisableNodeShell {
		t.Errorf("the start saw %+v, want the importer's declaration converted field for field", got)
	}
	body, err := got.ConfigFiles[1].Render(profile.Context{KubernetesVersion: "v1.37.1"})
	if err != nil || string(body) != "v1.37.1" {
		t.Errorf("Render through the seam = %q, %v; want the context handed through", body, err)
	}
	key, err := got.SharedSecrets[0].Mint()
	if err != nil || string(key) != "key" {
		t.Errorf("Mint through the seam = %q, %v", key, err)
	}
}

func TestImporterProfilesRefusesDuplicatesAndBrokenDeclarations(t *testing.T) {
	cfg := rootConfig{}
	for _, o := range []Option{WithProfiles(cisLike()), WithProfiles(), WithProfiles(cisLike())} {
		o(&cfg)
	}
	if _, err := importerProfiles(cfg.profiles); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Errorf("cis registered twice = %v, want a refusal", err)
	}

	broken := cisLike()
	broken.APIServerFlags[0].Value = "missing.yaml"
	if _, err := importerProfiles([]Profile{broken}); err == nil || !strings.Contains(err.Error(), "missing.yaml") {
		t.Errorf("a flag naming an undeclared file = %v, want a refusal naming it", err)
	}

	nilRender := cisLike()
	nilRender.ConfigFiles[0].Render = nil
	if _, err := importerProfiles([]Profile{nilRender}); err == nil || !strings.Contains(err.Error(), "no Render") {
		t.Errorf("a file without Render = %v, want a refusal", err)
	}

	got, err := importerProfiles(nil)
	if err != nil || len(got) != 0 {
		t.Errorf("no profiles = %v, %v; want an empty set and no error", got, err)
	}
}

func TestUpRefusesABrokenProfileBeforeAnyHostWrite(t *testing.T) {
	oldPrepare := prepareControllerFn
	t.Cleanup(func() { prepareControllerFn = oldPrepare })
	isolateProfileRegistryForTest(t)
	old := version.K8sVersion
	version.K8sVersion = "v1.37.1"
	t.Cleanup(func() { version.K8sVersion = old })
	prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
		t.Fatal("prepare ran although the profile declaration is broken")
		return nil, nil
	}
	broken := cisLike()
	broken.Name = "Not A Label"
	root := NewRootCommand(WithProfiles(broken))
	root.SetArgs([]string{"up", "--config", writeEmptyConfig(t, t.TempDir())})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "DNS-1123") {
		t.Errorf("up with a broken profile = %v, want the declaration error", err)
	}
}

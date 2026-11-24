package kubetest

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// env is the control plane shared by every test in a binary.
var env *envtest.Environment

func Available() bool { return os.Getenv("KUBEBUILDER_ASSETS") != "" }

// Main starts the control plane and returns the exit code, from TestMain.
func Main(m *testing.M) int {
	if Available() {
		env = &envtest.Environment{}
		if _, err := env.Start(); err != nil {
			panic("start envtest control plane: " + err.Error())
		}
	}
	code := m.Run()
	if env != nil {
		_ = env.Stop()
	}
	return code
}

// Start returns the control plane's config, or skips when binaries are absent.
func Start(t *testing.T) *rest.Config {
	t.Helper()
	if !Available() {
		t.Skip("KUBEBUILDER_ASSETS is unset; run `make test-envtest`")
	}
	if env == nil {
		t.Fatal("control plane not started: this package's TestMain must call kubetest.Main")
	}
	return env.Config
}

func IsolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("KUBECONFIG", "")
	return home
}

func Kubeconfig(t *testing.T, cfg *rest.Config, contexts ...string) string {
	t.Helper()
	if len(contexts) == 0 {
		contexts = []string{"envtest"}
	}

	api := clientcmdapi.NewConfig()
	api.Clusters["envtest"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	api.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
		Token:                 cfg.BearerToken,
	}
	for _, name := range contexts {
		api.Contexts[name] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "envtest"}
	}
	api.CurrentContext = contexts[0]

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*api, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

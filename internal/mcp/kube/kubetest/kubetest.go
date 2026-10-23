// Package kubetest starts a real Kubernetes control plane for tests.
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

// Available reports whether the control-plane binaries are present.
func Available() bool { return os.Getenv("KUBEBUILDER_ASSETS") != "" }

// Main starts the control plane, runs the tests and stops it, returning the
// exit code. Use it from TestMain:
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

// Start returns the running control plane's client config, or skips the
// test when the binaries are absent.
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

// IsolateHome points $HOME at a scratch directory and clears $KUBECONFIG.
func IsolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	t.Setenv("KUBECONFIG", "")
	return home
}

// Kubeconfig writes a kubeconfig addressing the running control plane and
// returns its path.
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

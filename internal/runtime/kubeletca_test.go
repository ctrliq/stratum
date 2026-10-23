package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/version"
)

const testCABundle = "-----BEGIN CERTIFICATE-----\nY2x1c3Rlcg==\n-----END CERTIFICATE-----\n"

// joinedPKIDir is a PKI directory as a joined node has it: the cluster trust
// bundle is on disk.
func joinedPKIDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca-bundle.crt"), []byte(testCABundle), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func kubeletClientCAFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "kubelet.conf"))
	if err != nil {
		t.Fatal(err)
	}
	var conf struct {
		Authentication struct {
			X509 struct {
				ClientCAFile string `json:"clientCAFile"`
			} `json:"x509"`
		} `json:"authentication"`
	}
	if err := yaml.Unmarshal(b, &conf); err != nil {
		t.Fatalf("kubelet.conf does not parse: %v\n%s", err, b)
	}
	return conf.Authentication.X509.ClientCAFile
}

// The kubelet authenticates :10250 clients against its own bundle, not the
// cluster trust bundle, so what it trusts can narrow without touching the
// trust every other component reads.
func TestKubeletClientCAFileIsItsOwnBundle(t *testing.T) {
	profiletest.Register(t)
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })

	for _, st := range []*nodestate.NodeState{
		{Role: "worker", Hostname: "w1"},
		{Role: "worker", Hostname: "w1", CIS: true},
		{Role: "controller", Hostname: "cp1"},
	} {
		version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()
		if err := writeKubeletConfig(st); err != nil {
			t.Fatal(err)
		}
		got := kubeletClientCAFile(t)
		if got != pki.KubeletClientBundlePath() {
			t.Errorf("%+v: clientCAFile is %q, want %q", st, got, pki.KubeletClientBundlePath())
		}
		if got == filepath.Join(version.PKIDir, "ca-bundle.crt") {
			t.Errorf("%+v: clientCAFile is the cluster trust bundle", st)
		}
	}
}

// A node installed before the kubelet had its own clientCAFile gets one
// holding the trust its kubelet already had, so re-rendering kubelet.conf on
// upgrade neither stops the kubelet starting nor locks the apiserver out.
func TestKubeletConfigSeedsTheClientCABundleOnUpgrade(t *testing.T) {
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })
	version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()

	if err := writeKubeletConfig(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(pki.KubeletClientBundlePath())
	if err != nil {
		t.Fatalf("clientCAFile not written: %v", err)
	}
	if string(got) != testCABundle {
		t.Errorf("seeded clientCAFile holds %q, want the cluster trust bundle %q", got, testCABundle)
	}
}

// Re-rendering kubelet.conf leaves an existing clientCAFile alone: what it
// trusts is decided where trust changes, not on every kubelet.conf write.
func TestKubeletConfigKeepsAnExistingClientCABundle(t *testing.T) {
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })
	version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()

	const own = "-----BEGIN CERTIFICATE-----\na3ViZWxldA==\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(pki.KubeletClientBundlePath(), []byte(own), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeKubeletConfig(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(pki.KubeletClientBundlePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != own {
		t.Errorf("clientCAFile rewritten to %q, want it left as %q", got, own)
	}
}

// With no cluster trust bundle there is nothing to seed the clientCAFile
// from, and a kubelet.conf naming a file that does not exist would only fail
// later, at kubelet start.
func TestKubeletConfigRefusesWithoutATrustBundle(t *testing.T) {
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })
	version.PKIDir, version.K8sConfDir = t.TempDir(), t.TempDir()

	if err := writeKubeletConfig(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err == nil {
		t.Fatal("writeKubeletConfig succeeded with no trust bundle to seed the clientCAFile from")
	}
	if _, err := os.Stat(filepath.Join(version.K8sConfDir, "kubelet.conf")); !os.IsNotExist(err) {
		t.Errorf("kubelet.conf written although its clientCAFile could not be: %v", err)
	}
}

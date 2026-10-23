package runtime

import (
	"archive/tar"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// Stratum's images are exempt from kubelet's pull-credential check, so an
// image kubelet pulls under another name with the same image ID cannot send
// kubelet to the registry for an image an airgapped node already holds.
func TestKubeletConfigExemptsTheShippedImages(t *testing.T) {
	isolatePayload(t)
	version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()
	addons.ImageRegistry = "ghcr.io/ctrliq/stratum"
	if err := writeKubeletConfig(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "kubelet.conf"))
	if err != nil {
		t.Fatal(err)
	}
	var conf struct {
		Policy    string   `json:"imagePullCredentialsVerificationPolicy"`
		Allowlist []string `json:"preloadedImagesVerificationAllowlist"`
	}
	if err := yaml.Unmarshal(b, &conf); err != nil {
		t.Fatalf("kubelet.conf does not parse: %v\n%s", err, b)
	}
	if conf.Policy != "NeverVerifyAllowlistedImages" {
		t.Errorf("imagePullCredentialsVerificationPolicy is %q, want NeverVerifyAllowlistedImages", conf.Policy)
	}
	if len(conf.Allowlist) != 1 || conf.Allowlist[0] != "ghcr.io/ctrliq/stratum/*" {
		t.Errorf("preloadedImagesVerificationAllowlist is %q, want [ghcr.io/ctrliq/stratum/*]", conf.Allowlist)
	}
}

// A build that did not stamp the payload registry exempts nothing, so kubelet
// keeps its default policy instead of trusting a namespace like unknown/*.
func TestKubeletConfigExemptsNothingWithoutAPayloadRegistry(t *testing.T) {
	isolatePayload(t)
	for _, reg := range []string{"", "unknown"} {
		version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()
		addons.ImageRegistry = reg
		if err := writeKubeletConfig(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "kubelet.conf"))
		if err != nil {
			t.Fatal(err)
		}
		var conf map[string]any
		if err := yaml.Unmarshal(b, &conf); err != nil {
			t.Fatalf("ImageRegistry %q: kubelet.conf does not parse: %v\n%s", reg, err, b)
		}
		for _, k := range []string{"imagePullCredentialsVerificationPolicy", "preloadedImagesVerificationAllowlist"} {
			if v, ok := conf[k]; ok {
				t.Errorf("ImageRegistry %q: kubelet.conf sets %s to %v, want it left to kubelet's default", reg, k, v)
			}
		}
	}
}

// isolatePayload points the kubelet config, the PKI and the staged payload at a
// test's own directories, and restores the registry stamp afterwards.
func isolatePayload(t *testing.T) {
	t.Helper()
	oldPKI, oldConf, oldData, oldReg := version.PKIDir, version.K8sConfDir, version.DataDir, addons.ImageRegistry
	t.Cleanup(func() {
		version.PKIDir, version.K8sConfDir, version.DataDir, addons.ImageRegistry = oldPKI, oldConf, oldData, oldReg
	})
	version.DataDir = t.TempDir()
}

func stageArchive(t *testing.T, name, index string) {
	t.Helper()
	dir := filepath.Join(version.DataDir, "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "index.json", Mode: 0o644, Size: int64(len(index)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(index)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func kubeletAllowlist(t *testing.T) []string {
	t.Helper()
	version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()
	if err := writeKubeletConfig(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "kubelet.conf"))
	if err != nil {
		t.Fatal(err)
	}
	var conf struct {
		Allowlist []string `json:"preloadedImagesVerificationAllowlist"`
	}
	if err := yaml.Unmarshal(b, &conf); err != nil {
		t.Fatalf("kubelet.conf does not parse: %v\n%s", err, b)
	}
	return conf.Allowlist
}

// An importer's images are outside the payload registry namespace. Without
// their own entries kubelet asks the registry for them, and an airgapped node
// goes to ImagePullBackOff on an image it holds.
func TestKubeletConfigExemptsEveryPayloadRepository(t *testing.T) {
	isolatePayload(t)
	addons.ImageRegistry = "ghcr.io/ctrliq/stratum"
	stageArchive(t, "stratum-images.tar", `{"manifests":[
		{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/stratum/etcd:3.7.0"}},
		{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/stratum/pause:3.10.2"}}]}`)
	stageArchive(t, "addon-images.tar", `{"manifests":[
		{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/addon-manager:v1.37.1-addon.0-rc.2"}},
		{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/addon-agent:v1.37.1-addon.0-rc.2"}}]}`)

	want := []string{"ghcr.io/ctrliq/stratum/*", "ghcr.io/ctrliq/addon-agent", "ghcr.io/ctrliq/addon-manager"}
	if got := kubeletAllowlist(t); !slices.Equal(got, want) {
		t.Errorf("preloadedImagesVerificationAllowlist = %q, want %q", got, want)
	}

	addons.ImageRegistry = "unknown"
	want = []string{"ghcr.io/ctrliq/addon-agent", "ghcr.io/ctrliq/addon-manager", "ghcr.io/ctrliq/stratum/etcd", "ghcr.io/ctrliq/stratum/pause"}
	if got := kubeletAllowlist(t); !slices.Equal(got, want) {
		t.Errorf("without a registry stamp, preloadedImagesVerificationAllowlist = %q, want each payload repository: %q", got, want)
	}
}

func TestKubeletConfigRefusesAnUnreadablePayloadArchive(t *testing.T) {
	isolatePayload(t)
	addons.ImageRegistry = "ghcr.io/ctrliq/stratum"
	stageArchive(t, "addon-images.tar", "{")
	version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()
	err := writeKubeletConfig(&nodestate.NodeState{Role: "worker", Hostname: "w1"})
	if err == nil || !strings.Contains(err.Error(), "addon-images.tar") {
		t.Errorf("an unreadable payload archive = %v, want an error naming it", err)
	}
}

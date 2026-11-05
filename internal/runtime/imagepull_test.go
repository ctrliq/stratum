package runtime

import (
	"archive/tar"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	oldPKI, oldConf, oldData, oldDropIn, oldReg := version.PKIDir, version.K8sConfDir, version.DataDir, version.ImagesDropInDir, addons.ImageRegistry
	t.Cleanup(func() {
		version.PKIDir, version.K8sConfDir, version.DataDir, version.ImagesDropInDir, addons.ImageRegistry = oldPKI, oldConf, oldData, oldDropIn, oldReg
	})
	version.DataDir, version.K8sConfDir = t.TempDir(), t.TempDir()
	version.ImagesDropInDir = filepath.Join(t.TempDir(), "images.d")
}

func stageArchive(t *testing.T, name, index string) {
	t.Helper()
	stageArchiveIn(t, filepath.Join(version.DataDir, "images"), name, index)
}

// markImported writes the stamp the import leaves once an operator archive's
// images are pinned.
func markImported(t *testing.T, name string) {
	t.Helper()
	tar := filepath.Join(version.ImagesDropInDir, name)
	fi, err := os.Stat(tar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(DropInImportStamp(tar)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DropInImportStamp(tar), []byte(ImportStampValue(fi)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stageArchiveIn(t *testing.T, dir, name, index string) {
	t.Helper()
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
	version.PKIDir = joinedPKIDir(t)
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

// An operator who stages an image for a node with no registry access puts its
// archive in images.d. Without an allowlist entry kubelet still asks the
// registry for it, so the node goes to ImagePullBackOff on an image it holds.
func TestKubeletConfigExemptsTheOperatorsDropInImages(t *testing.T) {
	isolatePayload(t)
	addons.ImageRegistry = "ghcr.io/ctrliq/stratum"
	stageArchive(t, "stratum-images.tar", `{"manifests":[
		{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/stratum/etcd:3.7.0"}}]}`)
	stageArchiveIn(t, version.ImagesDropInDir, "fleet-agent.tar", `{"manifests":[
		{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/stratum-fleet-agent:v1.37.1"}}]}`)
	stageArchiveIn(t, version.ImagesDropInDir, "notes.txt", `{"manifests":[
		{"annotations":{"io.containerd.image.name":"example.com/not-an-archive:v1"}}]}`)
	markImported(t, "fleet-agent.tar")

	want := []string{"ghcr.io/ctrliq/stratum/*", "ghcr.io/ctrliq/stratum-fleet-agent"}
	if got := kubeletAllowlist(t); !slices.Equal(got, want) {
		t.Errorf("preloadedImagesVerificationAllowlist = %q, want %q", got, want)
	}
}

// A bad file the operator dropped must not keep the kubelet from starting: it
// is skipped, and the operator's other archives are still exempted.
func TestKubeletConfigSkipsAnUnreadableDropInArchive(t *testing.T) {
	isolatePayload(t)
	addons.ImageRegistry = "ghcr.io/ctrliq/stratum"
	stageArchiveIn(t, version.ImagesDropInDir, "broken.tar", "{")
	stageArchiveIn(t, version.ImagesDropInDir, "fleet-agent.tar", `{"manifests":[
		{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/stratum-fleet-agent:v1.37.1"}}]}`)
	markImported(t, "broken.tar")
	markImported(t, "fleet-agent.tar")

	want := []string{"ghcr.io/ctrliq/stratum/*", "ghcr.io/ctrliq/stratum-fleet-agent"}
	if got := kubeletAllowlist(t); !slices.Equal(got, want) {
		t.Errorf("preloadedImagesVerificationAllowlist = %q, want %q", got, want)
	}
}

// The exemption covers a whole repository, so an archive that is not imported
// and pinned, whether it is new or its import failed, exempts nothing: pods
// would otherwise use other images of that repository on the node without
// their own credentials. A replaced archive waits for its new import too.
func TestKubeletConfigExemptsOnlyImportedDropInArchives(t *testing.T) {
	isolatePayload(t)
	addons.ImageRegistry = "ghcr.io/ctrliq/stratum"
	stageArchiveIn(t, version.ImagesDropInDir, "fleet-agent.tar", `{"manifests":[
		{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/stratum-fleet-agent:v1.37.1"}}]}`)
	stageArchiveIn(t, version.ImagesDropInDir, "skipped.tar", `{"manifests":[
		{"annotations":{"io.containerd.image.name":"registry.example.com/team/app:v2"}}]}`)
	markImported(t, "fleet-agent.tar")

	want := []string{"ghcr.io/ctrliq/stratum/*", "ghcr.io/ctrliq/stratum-fleet-agent"}
	if got := kubeletAllowlist(t); !slices.Equal(got, want) {
		t.Errorf("preloadedImagesVerificationAllowlist = %q, want %q, without the archive that was not imported", got, want)
	}

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(version.ImagesDropInDir, "fleet-agent.tar"), later, later); err != nil {
		t.Fatal(err)
	}
	if got := kubeletAllowlist(t); slices.Contains(got, "ghcr.io/ctrliq/stratum-fleet-agent") {
		t.Errorf("a replaced archive stayed exempt before its new import: %q", got)
	}
}

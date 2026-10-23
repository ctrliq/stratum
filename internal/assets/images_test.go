package assets

import (
	"archive/tar"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeImageArchive(t *testing.T, members map[string]string, order []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "images.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, name := range order {
		body := members[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestArchiveImageNames(t *testing.T) {
	index := `{"schemaVersion":2,"manifests":[
		{"digest":"sha256:aa","annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/addon-manager:v1","org.opencontainers.image.ref.name":"v1"}},
		{"digest":"sha256:bb","annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/addon-agent:v1"}},
		{"digest":"sha256:cc"}]}`
	path := writeImageArchive(t, map[string]string{
		"blobs/sha256/aa": strings.Repeat("x", 4096),
		"./index.json":    index,
		"oci-layout":      `{"imageLayoutVersion":"1.0.0"}`,
	}, []string{"blobs/sha256/aa", "oci-layout", "./index.json"})

	got, err := ArchiveImageNames(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ghcr.io/ctrliq/addon-manager:v1", "ghcr.io/ctrliq/addon-agent:v1"}
	if !slices.Equal(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}

	dockerFormat := writeImageArchive(t, map[string]string{
		"manifest.json": `[{"RepoTags":["addon-manager:v1","ctrliq/addon-agent:v1","ghcr.io/ctrliq/addon-cli:v1"]}]`,
		"repositories":  `{}`,
	}, []string{"manifest.json", "repositories"})
	got, err = ArchiveImageNames(dockerFormat)
	if err != nil {
		t.Fatalf("a Docker-format archive, which ctr imports, failed: %v", err)
	}
	want = []string{"docker.io/library/addon-manager:v1", "docker.io/ctrliq/addon-agent:v1", "ghcr.io/ctrliq/addon-cli:v1"}
	if !slices.Equal(got, want) {
		t.Errorf("Docker-format names = %v, want the RepoTags as containerd names them: %v", got, want)
	}

	both := writeImageArchive(t, map[string]string{
		"manifest.json": `[{"RepoTags":["other:v1"]}]`,
		"index.json":    index,
	}, []string{"manifest.json", "index.json"})
	if got, _ := ArchiveImageNames(both); len(got) != 2 || got[0] != "ghcr.io/ctrliq/addon-manager:v1" {
		t.Errorf("an archive with both files = %v, want index.json's names", got)
	}

	corrupt := writeImageArchive(t, map[string]string{"index.json": "{"}, []string{"index.json"})
	if _, err := ArchiveImageNames(corrupt); err == nil {
		t.Error("a corrupt index.json was read as naming no images")
	}
}

func TestDockerImageName(t *testing.T) {
	for tag, want := range map[string]string{
		"busybox:1":                "docker.io/library/busybox:1",
		"docker.io/busybox:1":      "docker.io/library/busybox:1",
		"ctrliq/agent:1":           "docker.io/ctrliq/agent:1",
		"docker.io/ctrliq/agent:1": "docker.io/ctrliq/agent:1",
		"ghcr.io/ctrliq/agent:1":   "ghcr.io/ctrliq/agent:1",
		"localhost/agent:1":        "localhost/agent:1",
		"registry:5000/agent:1":    "registry:5000/agent:1",
	} {
		if got := dockerImageName(tag); got != want {
			t.Errorf("dockerImageName(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestImageNamesMergesEveryArchive(t *testing.T) {
	engine := writeImageArchive(t, map[string]string{
		"index.json": `{"manifests":[{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/stratum/etcd:3.7.0"}}]}`,
	}, []string{"index.json"})
	importer := writeImageArchive(t, map[string]string{
		"index.json": `{"manifests":[{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/addon-manager:v1"}}]}`,
	}, []string{"index.json"})
	got, err := ImageNames([]string{engine, importer})
	if err != nil {
		t.Fatal(err)
	}
	if !got["ghcr.io/ctrliq/stratum/etcd:3.7.0"] || !got["ghcr.io/ctrliq/addon-manager:v1"] || len(got) != 2 {
		t.Errorf("names = %v, want both archives' names", got)
	}

	bad := writeImageArchive(t, map[string]string{"index.json": "{"}, []string{"index.json"})
	if _, err := ImageNames([]string{engine, bad}); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("an unreadable archive = %v, want an error naming it", err)
	}
}

func TestRepository(t *testing.T) {
	for name, want := range map[string]string{
		"ghcr.io/ctrliq/addon-manager:v1.37.1-addon.0-rc.2": "ghcr.io/ctrliq/addon-manager",
		"ghcr.io/ctrliq/stratum/etcd@sha256:abc":            "ghcr.io/ctrliq/stratum/etcd",
		"ghcr.io/ctrliq/stratum/etcd:3.7.0@sha256:abc":      "ghcr.io/ctrliq/stratum/etcd",
		"registry:5000/agent:1":                             "registry:5000/agent",
		"registry:5000/agent":                               "registry:5000/agent",
		"docker.io/library/busybox":                         "docker.io/library/busybox",
	} {
		if got := Repository(name); got != want {
			t.Errorf("Repository(%q) = %q, want %q", name, got, want)
		}
	}
}

package cluster

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/version"
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

func TestPayloadImageRowsPinsTheRegistryAndEveryArchivedName(t *testing.T) {
	reg := addons.ImageRegistry
	ls := strings.Join([]string{
		"REF TYPE DIGEST SIZE PLATFORMS LABELS",
		reg + "/etcd:3.7.0 application/vnd.oci.image.index.v1+json sha256:e1 20MiB linux/amd64 -",
		reg + "/etcd@sha256:e1 application/vnd.oci.image.index.v1+json sha256:e1 20MiB linux/amd64 -",
		"ghcr.io/ctrliq/addon-manager:v1 application/vnd.oci.image.index.v1+json sha256:f1 30MiB linux/amd64 -",
		"docker.io/library/nginx:1 application/vnd.oci.image.index.v1+json sha256:n1 50MiB linux/amd64 -",
		"short-row",
	}, "\n")
	got := payloadImageRows(ls, map[string]bool{"ghcr.io/ctrliq/addon-manager:v1": true})
	want := [][2]string{
		{reg + "/etcd:3.7.0", "sha256:e1"},
		{reg + "/etcd@sha256:e1", "sha256:e1"},
		{"ghcr.io/ctrliq/addon-manager:v1", "sha256:f1"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows = %v, want %v (a workload's own image must not be pinned)", got, want)
	}
}

func TestImportImagesPinsEveryArchivedName(t *testing.T) {
	origBin, origConf, origWait := version.BinDir, version.K8sConfDir, importWaitContainerd
	t.Cleanup(func() { version.BinDir, version.K8sConfDir, importWaitContainerd = origBin, origConf, origWait })
	version.BinDir, version.K8sConfDir = t.TempDir(), t.TempDir()
	importWaitContainerd = func(context.Context, time.Duration) error { return nil }

	reg := addons.ImageRegistry
	calls := filepath.Join(t.TempDir(), "calls")
	ls := "REF TYPE DIGEST SIZE PLATFORMS LABELS\n" +
		reg + "/etcd:3.7.0 index sha256:e1 20MiB linux/amd64 -\n" +
		"ghcr.io/ctrliq/addon-manager:v1 index sha256:f1 30MiB linux/amd64 -\n" +
		"docker.io/library/nginx:1 index sha256:n1 50MiB linux/amd64 -\n"
	script := "#!/bin/sh\nshift 4\necho \"$*\" >> " + calls + "\n" +
		"[ \"$1 $2\" = \"images ls\" ] && printf '" + strings.ReplaceAll(ls, "\n", "\\n") + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(version.BinDir, "ctr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, image := range map[string]string{"engine.tar": reg + "/etcd:3.7.0", "importer.tar": "ghcr.io/ctrliq/addon-manager:v1"} {
		archive := writeImageArchive(t, map[string]string{
			"index.json": `{"manifests":[{"annotations":{"io.containerd.image.name":"` + image + `"}}]}`,
		}, []string{"index.json"})
		b, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := ImportImages(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	for _, want := range []string{
		"images label ghcr.io/ctrliq/addon-manager:v1 io.cri-containerd.pinned=pinned",
		"images label ghcr.io/ctrliq/addon-manager@sha256:f1 io.cri-containerd.pinned=pinned",
		"images label " + reg + "/etcd:3.7.0 io.cri-containerd.pinned=pinned",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("ctr was never asked to %q; calls:\n%s", want, log)
		}
	}
	if strings.Contains(log, "label docker.io/library/nginx") {
		t.Errorf("a workload's own image was pinned; calls:\n%s", log)
	}
}

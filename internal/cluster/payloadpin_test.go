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
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/runtime"
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

// An operator's archive is imported and pinned like a payload one, once per
// archive version. One that fails to import is skipped with a warning rather
// than failing kubelet's start.
func TestImportDropInImagesImportsPinsAndSkipsAFailure(t *testing.T) {
	origBin, origConf, origDropIn := version.BinDir, version.K8sConfDir, version.ImagesDropInDir
	t.Cleanup(func() { version.BinDir, version.K8sConfDir, version.ImagesDropInDir = origBin, origConf, origDropIn })
	version.BinDir, version.K8sConfDir, version.ImagesDropInDir = t.TempDir(), t.TempDir(), t.TempDir()

	calls := filepath.Join(t.TempDir(), "calls")
	ls := "REF TYPE DIGEST SIZE PLATFORMS LABELS\n" +
		"ghcr.io/ctrliq/stratum-fleet-agent:v1 index sha256:a1 30MiB linux/arm64 -\n" +
		"docker.io/library/nginx:1 index sha256:n1 50MiB linux/arm64 -\n"
	script := "#!/bin/sh\nshift 4\necho \"$*\" >> " + calls + "\n" +
		"case \"$*\" in *broken.tar) echo 'ctr: unexpected EOF'; exit 1;; esac\n" +
		"[ \"$1 $2\" = \"images ls\" ] && printf '" + strings.ReplaceAll(ls, "\n", "\\n") + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(version.BinDir, "ctr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, index := range map[string]string{
		"fleet-agent.tar": `{"manifests":[{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/stratum-fleet-agent:v1"}}]}`,
		"broken.tar":      `{"manifests":[{"annotations":{"io.containerd.image.name":"example.com/broken:v1"}}]}`,
	} {
		b, err := os.ReadFile(writeImageArchive(t, map[string]string{"index.json": index}, []string{"index.json"}))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(version.ImagesDropInDir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var out, warn strings.Builder
	if !importDropInImages(context.Background(), &out, &warn) {
		t.Error("the first start imported fleet-agent.tar but reported nothing new")
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	for _, want := range []string{
		"images import " + filepath.Join(version.ImagesDropInDir, "fleet-agent.tar"),
		"images label ghcr.io/ctrliq/stratum-fleet-agent:v1 io.cri-containerd.pinned=pinned",
		"images label ghcr.io/ctrliq/stratum-fleet-agent@sha256:a1 io.cri-containerd.pinned=pinned",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("ctr was never asked to %q; calls:\n%s", want, log)
		}
	}
	if strings.Contains(log, "label docker.io/library/nginx") || strings.Contains(log, "label example.com/broken") {
		t.Errorf("an image no imported archive carries was pinned; calls:\n%s", log)
	}
	if !strings.Contains(warn.String(), "broken.tar") {
		t.Errorf("warnings = %q, want the archive that failed to import named", warn.String())
	}

	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	if importDropInImages(context.Background(), &out, &warn) {
		t.Error("the next start imported nothing new but reported it did")
	}
	b, err = os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "import "+filepath.Join(version.ImagesDropInDir, "fleet-agent.tar")) {
		t.Errorf("an unchanged archive was imported again on the next start; calls:\n%s", b)
	}
}

// dropInHost stages one operator archive and a ctr that fails any call
// matching failOn, and returns the file ctr logs its calls to.
func dropInHost(t *testing.T, failOn string) (calls string) {
	t.Helper()
	origBin, origConf, origDropIn := version.BinDir, version.K8sConfDir, version.ImagesDropInDir
	t.Cleanup(func() { version.BinDir, version.K8sConfDir, version.ImagesDropInDir = origBin, origConf, origDropIn })
	version.BinDir, version.K8sConfDir, version.ImagesDropInDir = t.TempDir(), t.TempDir(), t.TempDir()
	calls = filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\nshift 4\necho \"$*\" >> " + calls + "\n" +
		"case \"$*\" in " + failOn + ") echo 'ctr: refused'; exit 1;; esac\n" +
		"[ \"$1 $2\" = \"images ls\" ] && printf 'REF TYPE DIGEST SIZE PLATFORMS LABELS\\nghcr.io/ctrliq/stratum-fleet-agent:v1 index sha256:a1 30MiB linux/arm64 -\\n'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(version.BinDir, "ctr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(writeImageArchive(t, map[string]string{"index.json": `{"manifests":[{"annotations":{"io.containerd.image.name":"ghcr.io/ctrliq/stratum-fleet-agent:v1"}}]}`}, []string{"index.json"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version.ImagesDropInDir, "fleet-agent.tar"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return calls
}

// An archive whose images could not be pinned is not stamped: image garbage
// collection may remove its unpinned images, so the next start imports and
// pins it again rather than skipping it as current.
func TestImportDropInImagesStampsOnlyAfterThePin(t *testing.T) {
	calls := dropInHost(t, "images\\ label*")
	tar := filepath.Join(version.ImagesDropInDir, "fleet-agent.tar")
	var out, warn strings.Builder
	if importDropInImages(context.Background(), &out, &warn) {
		t.Error("a start whose pin failed reported a fresh import")
	}
	if !strings.Contains(warn.String(), "imported again on the next start") {
		t.Errorf("warnings = %q, want the failed pin reported", warn.String())
	}
	if runtime.DropInImported(tar) {
		t.Fatal("the archive was stamped though its images were not pinned")
	}
	if err := os.Remove(calls); err != nil {
		t.Fatal(err)
	}
	importDropInImages(context.Background(), &out, &warn)
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "images import "+tar) {
		t.Errorf("the next start did not import the unpinned archive again; calls:\n%s", b)
	}
}

// An archive replaced while it was imported is skipped and not stamped:
// containerd consumed the old content, so a stamp of the new one would exempt
// images that were never imported and skip importing them on the next start.
func TestImportDropInImagesSkipsAnArchiveReplacedDuringItsImport(t *testing.T) {
	calls := dropInHost(t, "nothing")
	tar := filepath.Join(version.ImagesDropInDir, "fleet-agent.tar")
	grow := "#!/bin/sh\nshift 4\necho \"$*\" >> " + calls + "\n" +
		"[ \"$1 $2\" = \"images import\" ] && printf 'x' >> \"$3\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(version.BinDir, "ctr"), []byte(grow), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, warn strings.Builder
	if importDropInImages(context.Background(), &out, &warn) {
		t.Error("a start whose only archive changed mid-import reported a fresh import")
	}
	if !strings.Contains(warn.String(), "changed while it was imported") {
		t.Errorf("warnings = %q, want the replaced archive reported", warn.String())
	}
	if runtime.DropInImported(tar) {
		t.Fatal("an archive replaced during its import was stamped as imported")
	}
	if b, _ := os.ReadFile(calls); strings.Contains(string(b), "images label") {
		t.Errorf("the replaced archive's images were pinned; calls:\n%s", b)
	}
}

// A stamp that cannot be written is reported, and the start does not count the
// archive as freshly imported, since kubelet.conf would not exempt it.
func TestImportDropInImagesReportsAStampItCouldNotWrite(t *testing.T) {
	dropInHost(t, "nothing")
	// A file where the stamp directory goes fails the write even as root.
	if err := os.WriteFile(filepath.Dir(runtime.DropInImportStamp("x")), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out, warn strings.Builder
	if importDropInImages(context.Background(), &out, &warn) {
		t.Error("a start that recorded no import reported a fresh one")
	}
	if !strings.Contains(warn.String(), "fleet-agent.tar is not exempt") {
		t.Errorf("warnings = %q, want the unwritten stamp reported", warn.String())
	}
}

// A start that imports a new operator archive renders kubelet.conf again, so
// kubelet, which reads it as it starts, exempts the new images; a start with
// nothing new leaves it alone.
func TestImportOperatorImagesRendersKubeletConfAfterAFreshImport(t *testing.T) {
	dropInHost(t, "nothing")
	oldLoad, oldWrite := LoadState, writeKubeletConf
	t.Cleanup(func() { LoadState, writeKubeletConf = oldLoad, oldWrite })
	LoadState = func() (*nodestate.NodeState, error) { return &nodestate.NodeState{Role: "worker", Hostname: "w1"}, nil }
	renders := 0
	writeKubeletConf = func(*nodestate.NodeState) error {
		renders++
		if !runtime.DropInImported(filepath.Join(version.ImagesDropInDir, "fleet-agent.tar")) {
			t.Error("kubelet.conf was rendered before the archive was stamped, so it would not exempt it")
		}
		return nil
	}
	var out, warn strings.Builder
	importOperatorImages(context.Background(), &out, &warn)
	importOperatorImages(context.Background(), &out, &warn)
	if renders != 1 {
		t.Errorf("kubelet.conf was rendered %d times, want once, after the fresh import", renders)
	}
}

// A stalled import ends with the start's context, so a shutdown or re-exec
// does not wait for it.
func TestImportDropInImagesStopsWithItsContext(t *testing.T) {
	dropInHost(t, "nothing")
	hang := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(version.BinDir, "ctr"), []byte(hang), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var out, warn strings.Builder
	start := time.Now()
	importDropInImages(ctx, &out, &warn)
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the import ran %s past its cancelled context", took)
	}
	if runtime.DropInImported(filepath.Join(version.ImagesDropInDir, "fleet-agent.tar")) {
		t.Error("a cancelled import was stamped")
	}
}

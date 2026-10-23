package cluster

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
)

func TestImportImagesProgressUsesOperationWriter(t *testing.T) {
	oldBin, oldConf, oldWait := version.BinDir, version.K8sConfDir, importWaitContainerd
	t.Cleanup(func() { version.BinDir, version.K8sConfDir, importWaitContainerd = oldBin, oldConf, oldWait })
	version.BinDir, version.K8sConfDir = t.TempDir(), t.TempDir()
	importWaitContainerd = func(context.Context, time.Duration) error { return nil }
	if err := os.WriteFile(filepath.Join(version.BinDir, "ctr"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive := writeImageArchive(t, map[string]string{"index.json": `{"manifests":[{"annotations":{"io.containerd.image.name":"example.test/image:v1"}}]}`}, []string{"index.json"})
	var progress bytes.Buffer
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = saved; r.Close(); w.Close() })
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	err = ImportImagesWithWriter(context.Background(), &progress, filepath.Dir(archive))
	os.Stdout = saved
	w.Close()
	stdout := <-done
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Errorf("image import leaked to process stdout: %q", stdout)
	}
	if want := "stratum: imported payload images from images.tar\n"; progress.String() != want {
		t.Errorf("operation progress=%q, want %q", progress.String(), want)
	}
	if !stampCurrent(archive) {
		t.Fatal("successful import did not preserve the import stamp")
	}
	progress.Reset()
	if err := ImportImagesWithWriter(context.Background(), &progress, filepath.Dir(archive)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(progress.String(), "imported payload images") {
		t.Errorf("current archive re-imported: %q", progress.String())
	}
}

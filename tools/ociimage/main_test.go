package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readArchive(t *testing.T, file string) map[string][]byte {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string][]byte{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = b
	}
}

func blob(t *testing.T, files map[string][]byte, d descriptor) []byte {
	t.Helper()
	b, ok := files["blobs/sha256/"+strings.TrimPrefix(d.Digest, "sha256:")]
	if !ok {
		t.Fatalf("blob %s missing", d.Digest)
	}
	if digestOf(b) != d.Digest || int64(len(b)) != d.Size {
		t.Fatalf("blob %s: digest %s size %d, want size %d", d.Digest, digestOf(b), len(b), d.Size)
	}
	return b
}

func TestWriteBuildsAnImportableImage(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "stratum-dash")
	if err := os.WriteFile(bin, []byte("\x7fELF fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := options{Binary: bin, Ref: "ghcr.io/ctrliq/stratum/stratum-dash:v1.37.1-stratum.0", OS: "linux", Arch: "amd64", User: "65532:65532"}
	out := filepath.Join(dir, "a.tar")
	digest, err := write(o, out)
	if err != nil {
		t.Fatal(err)
	}
	files := readArchive(t, out)

	var index struct{ Manifests []descriptor }
	if err := json.Unmarshal(files["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != digest {
		t.Fatalf("index %+v, want one manifest %s", index.Manifests, digest)
	}
	if got := index.Manifests[0].Annotations["io.containerd.image.name"]; got != o.Ref {
		t.Fatalf("image name %q, want %q", got, o.Ref)
	}

	var manifest struct {
		Config descriptor
		Layers []descriptor
	}
	if err := json.Unmarshal(blob(t, files, index.Manifests[0]), &manifest); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Architecture string
		Config       struct {
			Entrypoint []string
			User       string
		}
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		}
	}
	if err := json.Unmarshal(blob(t, files, manifest.Config), &config); err != nil {
		t.Fatal(err)
	}
	if config.Architecture != "amd64" || config.Config.User != "65532:65532" || len(config.Config.Entrypoint) != 1 || config.Config.Entrypoint[0] != "/stratum-dash" {
		t.Fatalf("config %+v", config)
	}
	if len(manifest.Layers) != 1 {
		t.Fatalf("%d layers, want 1", len(manifest.Layers))
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob(t, files, manifest.Layers[0])))
	if err != nil {
		t.Fatal(err)
	}
	layer, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.RootFS.DiffIDs) != 1 || config.RootFS.DiffIDs[0] != digestOf(layer) {
		t.Fatalf("diff_ids %v, want [%s]", config.RootFS.DiffIDs, digestOf(layer))
	}
	h, err := tar.NewReader(bytes.NewReader(layer)).Next()
	if err != nil {
		t.Fatal(err)
	}
	if h.Name != "stratum-dash" || h.Mode != 0o755 {
		t.Fatalf("layer entry %q mode %o", h.Name, h.Mode)
	}

	again := filepath.Join(dir, "b.tar")
	if _, err := write(o, again); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(out)
	b, _ := os.ReadFile(again)
	if !bytes.Equal(a, b) {
		t.Fatal("two writes of the same input differ")
	}
}

func TestCheckRef(t *testing.T) {
	for ref, ok := range map[string]bool{
		"ghcr.io/ctrliq/stratum/stratum-dash:v1.0.0": true,
		"localhost:5000/stratum-dash":                false,
		"ghcr.io/x/y:v1+meta":                        false,
		"ghcr.io/x/y:v1@sha256:00":                   false,
	} {
		if err := checkRef(ref); (err == nil) != ok {
			t.Errorf("checkRef(%q) = %v, want ok=%v", ref, err, ok)
		}
	}
}

// Command ociimage writes a single-binary OCI image archive for `ctr images
// import`: one layer holding the binary at its root, no base image, and
// byte-identical output for the same input.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

const (
	mediaManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaConfig   = "application/vnd.oci.image.config.v1+json"
	mediaLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"
	mediaIndex    = "application/vnd.oci.image.index.v1+json"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type options struct {
	Binary string
	Ref    string
	OS     string
	Arch   string
	User   string
}

func main() {
	var o options
	var out string
	flag.StringVar(&o.Binary, "bin", "", "the static binary to put at the image root")
	flag.StringVar(&o.Ref, "ref", "", "the image name the archive registers, <repo>:<tag>")
	flag.StringVar(&o.OS, "os", "linux", "image OS")
	flag.StringVar(&o.Arch, "arch", "", "image architecture")
	flag.StringVar(&o.User, "user", "65532:65532", "the image's default user")
	flag.StringVar(&out, "out", "", "the archive to write")
	flag.Parse()
	if o.Binary == "" || o.Ref == "" || o.Arch == "" || out == "" {
		fmt.Fprintln(os.Stderr, "usage: ociimage -bin <file> -ref <repo>:<tag> -arch <arch> -out <archive.tar>")
		os.Exit(2)
	}
	digest, err := write(o, out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ociimage:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%s, %s)\n", out, o.Ref, digest)
}

func write(o options, out string) (string, error) {
	if err := checkRef(o.Ref); err != nil {
		return "", err
	}
	bin, err := os.ReadFile(o.Binary)
	if err != nil {
		return "", err
	}
	name := path.Base(o.Binary)
	layerTar, err := tarOf([]entry{{name: name, mode: 0o755, body: bin}})
	if err != nil {
		return "", err
	}
	layer, err := gzipOf(layerTar)
	if err != nil {
		return "", err
	}
	config, err := json.Marshal(map[string]any{
		"architecture": o.Arch,
		"os":           o.OS,
		"config": map[string]any{
			"Entrypoint": []string{"/" + name},
			"User":       o.User,
			"WorkingDir": "/",
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{digestOf(layerTar)}},
	})
	if err != nil {
		return "", err
	}
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaManifest,
		"config":        descriptor{MediaType: mediaConfig, Digest: digestOf(config), Size: int64(len(config))},
		"layers":        []descriptor{{MediaType: mediaLayer, Digest: digestOf(layer), Size: int64(len(layer))}},
	})
	if err != nil {
		return "", err
	}
	tag := o.Ref[strings.LastIndex(o.Ref, ":")+1:]
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     mediaIndex,
		"manifests": []descriptor{{
			MediaType: mediaManifest, Digest: digestOf(manifest), Size: int64(len(manifest)),
			Annotations: map[string]string{
				"io.containerd.image.name":          o.Ref,
				"org.opencontainers.image.ref.name": tag,
			},
		}},
	})
	if err != nil {
		return "", err
	}
	var entries []entry
	for _, blob := range [][]byte{config, layer, manifest} {
		entries = append(entries, entry{name: "blobs/sha256/" + strings.TrimPrefix(digestOf(blob), "sha256:"), mode: 0o644, body: blob})
	}
	entries = append(entries,
		entry{name: "index.json", mode: 0o644, body: index},
		entry{name: "oci-layout", mode: 0o644, body: []byte(`{"imageLayoutVersion":"1.0.0"}`)},
	)
	archive, err := tarOf(entries)
	if err != nil {
		return "", err
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, archive, 0o644); err != nil {
		return "", err
	}
	return digestOf(manifest), os.Rename(tmp, out)
}

func checkRef(ref string) error {
	i := strings.LastIndex(ref, ":")
	if i < 0 || i < strings.LastIndex(ref, "/") || strings.Contains(ref, "@") {
		return fmt.Errorf("-ref %q: want <repo>:<tag>", ref)
	}
	for _, r := range ref[i+1:] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			return fmt.Errorf("-ref %q: the tag may hold only A-Z a-z 0-9 _ . -", ref)
		}
	}
	return nil
}

type entry struct {
	name string
	mode int64
	body []byte
}

func tarOf(entries []entry) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for _, e := range entries {
		for d := path.Dir(e.name); d != "." && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}); err != nil {
			return nil, err
		}
	}
	for _, e := range entries {
		h := &tar.Header{Typeflag: tar.TypeReg, Name: e.name, Mode: e.mode, Size: int64(len(e.body)), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gzipOf(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(zw, bytes.NewReader(b)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

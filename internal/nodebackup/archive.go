package nodebackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/atomicfile"
)

// The caps keep a bundle the size of what it is for, keys and configuration.
// A file or a tree past them is an error naming it, never a silent omission:
// a backup that left something out is found out only at a restore. Vars so a
// test can lower them.
var (
	maxFileBytes   int64 = 16 << 20
	maxBundleBytes int64 = 64 << 20
)

const manifestName = "manifest.json"

// Sources is where a node keeps what a bundle holds.
type Sources struct {
	// PKIDir is the node's PKI directory, taken whole: the cluster's CAs and
	// keys, the node's own certificates, and any secret its profile shares
	// between control planes, such as the key that encrypts Secrets at rest.
	PKIDir string
	// StatePath is node.yaml.
	StatePath string
	// ConfDir is the operator's configuration directory.
	ConfDir string
	// ManifestsDir is the operator's own manifests; absent on most nodes.
	ManifestsDir string
	// ExtraConfig is the cluster config file when it is kept outside ConfDir,
	// else empty.
	ExtraConfig string
	// Skip is directories under the others that are not node state and are
	// not descended into: image archives measured in gigabytes.
	Skip []string
}

// Meta is what a bundle records about where and when it was taken.
type Meta struct {
	Hostname          string    `json:"hostname"`
	NodeIP            string    `json:"nodeIP,omitempty"`
	StratumVersion    string    `json:"stratumVersion"`
	KubernetesVersion string    `json:"kubernetesVersion"`
	CreatedAt         time.Time `json:"createdAt"`
	// Snapshot is the etcd snapshot taken with this bundle, the one to
	// restore beside it.
	Snapshot string `json:"snapshot,omitempty"`
}

// File is one entry of a bundle.
type File struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Link is the name, in the same directory, a symbolic link points to.
	Link string `json:"link,omitempty"`
	Dir  bool   `json:"dir,omitempty"`
}

// Manifest is a bundle's first entry: what it is and what it holds.
type Manifest struct {
	Format int `json:"format"`
	Meta
	// ClusterCASHA256 is the SHA-256 of the cluster CA certificate, for the
	// person restoring to check against their own record of the cluster.
	ClusterCASHA256 string `json:"clusterCASHA256,omitempty"`
	// ExtraConfig is the path of the cluster config file the bundle holds
	// from outside the configuration directory, else empty.
	ExtraConfig string `json:"extraConfig,omitempty"`
	Files       []File `json:"files"`
}

type entry struct {
	File
	data []byte
}

// Collect reads the node state at src into an archive, unencrypted: the
// caller seals it and never writes this form anywhere.
func Collect(src Sources, meta Meta) ([]byte, Manifest, error) {
	var entries []entry
	var total int64
	add := func(e entry) error {
		total += e.Size
		if total > maxBundleBytes {
			return fmt.Errorf("the node state is over %d MiB at %s; a bundle holds keys and configuration, not data",
				maxBundleBytes>>20, e.Path)
		}
		entries = append(entries, e)
		return nil
	}
	for _, root := range []struct {
		path     string
		required bool
	}{{src.PKIDir, true}, {src.ConfDir, false}, {src.ManifestsDir, false}} {
		if root.path == "" {
			continue
		}
		if err := collectTree(root.path, root.required, src.Skip, add); err != nil {
			return nil, Manifest{}, err
		}
	}
	for _, f := range []struct {
		path     string
		required bool
	}{{src.StatePath, true}, {src.ExtraConfig, false}} {
		if f.path == "" {
			continue
		}
		e, err := readFile(f.path)
		if errors.Is(err, fs.ErrNotExist) && !f.required {
			continue
		}
		if err != nil {
			return nil, Manifest{}, err
		}
		if err := add(e); err != nil {
			return nil, Manifest{}, err
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.Path, b.Path) })
	m := Manifest{Format: formatVersion, Meta: meta, ExtraConfig: src.ExtraConfig}
	m.ClusterCASHA256 = caFingerprint(filepath.Join(src.PKIDir, "ca.crt"))
	for _, e := range entries {
		m.Files = append(m.Files, e.File)
	}
	plain, err := writeArchive(m, entries)
	return plain, m, err
}

func collectTree(root string, required bool, skip []string, add func(entry) error) error {
	fi, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch mode := info.Mode(); {
		case mode.IsDir():
			if slices.Contains(skip, path) {
				return fs.SkipDir
			}
			return add(entry{File: File{Path: path, Mode: uint32(mode.Perm()), Dir: true}})
		case mode.IsRegular():
			e, err := readFile(path)
			if err != nil {
				return err
			}
			return add(e)
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) && filepath.Dir(target) == filepath.Dir(path) {
				target = filepath.Base(target)
			}
			if target == "" || target == "." || target == ".." || strings.ContainsRune(target, filepath.Separator) {
				return fmt.Errorf("%s links to %s, outside its own directory; a bundle holds only links beside their target", path, target)
			}
			return add(entry{File: File{Path: path, Mode: uint32(mode.Perm()), Link: target}})
		}
		return fmt.Errorf("%s is neither a file, a directory nor a link; a bundle cannot hold it", path)
	})
}

func readFile(path string) (entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return entry{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return entry{}, err
	}
	if !info.Mode().IsRegular() {
		return entry{}, fmt.Errorf("%s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return entry{}, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(data)) > maxFileBytes {
		return entry{}, fmt.Errorf("%s is over %d MiB; a bundle holds keys and configuration, not data", path, maxFileBytes>>20)
	}
	sum := sha256.Sum256(data)
	return entry{File: File{Path: path, Mode: uint32(info.Mode().Perm()), Size: int64(len(data)),
		SHA256: hex.EncodeToString(sum[:])}, data: data}, nil
}

// caFingerprint is the SHA-256 of the first certificate in the PEM file at
// path, or empty when there is none to read.
func caFingerprint(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

func writeArchive(m Manifest, entries []entry) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(mb)), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(mb); err != nil {
		return nil, err
	}
	for _, e := range entries {
		h := &tar.Header{Name: strings.TrimPrefix(e.Path, "/"), Mode: int64(e.Mode)}
		switch {
		case e.Dir:
			h.Typeflag, h.Name = tar.TypeDir, h.Name+"/"
		case e.Link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.Link
		default:
			h.Typeflag, h.Size = tar.TypeReg, e.Size
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ReadManifest returns the manifest of an opened bundle, without writing
// anything, for the caller to check before it restores.
func ReadManifest(plain []byte) (Manifest, error) {
	tr, err := openArchive(plain)
	if err != nil {
		return Manifest{}, err
	}
	return readManifest(tr)
}

func openArchive(plain []byte) (*tar.Reader, error) {
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, fmt.Errorf("node-state bundle: %w", err)
	}
	return tar.NewReader(io.LimitReader(gz, maxBundleBytes+(1<<20))), nil
}

func readManifest(tr *tar.Reader) (Manifest, error) {
	h, err := tr.Next()
	if err != nil || h.Name != manifestName || h.Typeflag != tar.TypeReg {
		return Manifest{}, errors.New("node-state bundle does not begin with its manifest")
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("node-state bundle manifest: %w", err)
	}
	if m.Format != formatVersion {
		return Manifest{}, fmt.Errorf("node-state bundle manifest is format %d; this stratum reads format %d", m.Format, formatVersion)
	}
	return m, nil
}

// Destination is where a restore may write. Nothing in a bundle lands
// anywhere else, whatever the bundle says: a bundle is sealed to a public key,
// which proves who can read it and nothing about who wrote it.
type Destination struct {
	PKIDir       string
	StatePath    string
	ConfDir      string
	ManifestsDir string
	// AllowExtra is the one path outside the others the operator has said the
	// bundle's cluster config file may be written to, else empty.
	AllowExtra string
}

// Restored is what Extract did.
type Restored struct {
	Files int
	// ExtraConfigSkipped is the path of a cluster config file the bundle
	// holds and Extract did not write, because the operator did not allow it.
	ExtraConfigSkipped string
}

// Extract writes an opened bundle's files to dst. It checks the whole bundle
// against its manifest and against dst before it writes the first file, so a
// bundle it refuses changes nothing.
func Extract(plain []byte, dst Destination) (Restored, error) {
	tr, err := openArchive(plain)
	if err != nil {
		return Restored{}, err
	}
	m, err := readManifest(tr)
	if err != nil {
		return Restored{}, err
	}
	listed := map[string]File{}
	for _, f := range m.Files {
		listed[f.Path] = f
	}
	var res Restored
	var entries []entry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Restored{}, fmt.Errorf("node-state bundle: %w", err)
		}
		path := "/" + strings.TrimSuffix(h.Name, "/")
		if path != filepath.Clean(path) {
			return Restored{}, fmt.Errorf("node-state bundle holds the path %q, which is not a plain path", h.Name)
		}
		want, ok := listed[path]
		if !ok {
			return Restored{}, fmt.Errorf("node-state bundle holds %s, which its manifest does not list", path)
		}
		delete(listed, path)
		if !dst.allows(path) {
			if path == m.ExtraConfig && h.Typeflag == tar.TypeReg {
				res.ExtraConfigSkipped = path
				continue
			}
			return Restored{}, fmt.Errorf("node-state bundle holds %s, which is not node state; nothing was restored", path)
		}
		e := entry{File: want}
		switch h.Typeflag {
		case tar.TypeDir:
			if !want.Dir {
				return Restored{}, fmt.Errorf("node-state bundle: %s does not match its manifest", path)
			}
		case tar.TypeSymlink:
			if want.Link != h.Linkname || h.Linkname == "" || h.Linkname == "." || h.Linkname == ".." ||
				strings.ContainsRune(h.Linkname, filepath.Separator) || !within(dst.PKIDir, path) {
				return Restored{}, fmt.Errorf("node-state bundle holds a link at %s it may not; nothing was restored", path)
			}
		case tar.TypeReg:
			data, err := io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
			if err != nil {
				return Restored{}, fmt.Errorf("node-state bundle: %s: %w", path, err)
			}
			sum := sha256.Sum256(data)
			if want.Dir || want.Link != "" || hex.EncodeToString(sum[:]) != want.SHA256 {
				return Restored{}, fmt.Errorf("node-state bundle: %s does not match its manifest", path)
			}
			e.data = data
		default:
			return Restored{}, fmt.Errorf("node-state bundle holds %s, which is neither a file, a directory nor a link", path)
		}
		entries = append(entries, e)
	}
	for path := range listed {
		return Restored{}, fmt.Errorf("node-state bundle is missing %s, which its manifest lists", path)
	}
	for _, e := range entries {
		if err := writeEntry(e); err != nil {
			return res, fmt.Errorf("restore %s: %w", e.Path, err)
		}
		if !e.Dir {
			res.Files++
		}
	}
	return res, nil
}

func writeEntry(e entry) error {
	perm := fs.FileMode(e.Mode).Perm()
	switch {
	case e.Dir:
		if err := os.MkdirAll(e.Path, 0o700); err != nil {
			return err
		}
		return os.Chmod(e.Path, perm)
	case e.Link != "":
		if err := os.MkdirAll(filepath.Dir(e.Path), 0o700); err != nil {
			return err
		}
		if err := os.Remove(e.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return os.Symlink(e.Link, e.Path)
	}
	if err := os.MkdirAll(filepath.Dir(e.Path), 0o700); err != nil {
		return err
	}
	return atomicfile.WriteFile(e.Path, e.data, perm)
}

func (d Destination) allows(path string) bool {
	for _, root := range []string{d.PKIDir, d.ConfDir, d.ManifestsDir} {
		if root != "" && (path == root || within(root, path)) {
			return true
		}
	}
	return (d.StatePath != "" && path == d.StatePath) || (d.AllowExtra != "" && path == d.AllowExtra)
}

// within reports whether path is strictly under root.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

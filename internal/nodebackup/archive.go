package nodebackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
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
	"unicode"

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
	// ConfigPath is the cluster config file the node runs with, wherever it
	// is kept, recorded for the person who restores: it is the file to start
	// the replacement with. Empty when the node runs with the default.
	ConfigPath string
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
	// ConfigPath is the cluster config file the node ran with, the one to
	// start its replacement with; empty when it ran with the default.
	ConfigPath string `json:"configPath,omitempty"`
	Files      []File `json:"files"`
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
	}{{src.StatePath, true}, {src.ExtraConfig, true}} {
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
	m := Manifest{Format: formatVersion, Meta: meta, ExtraConfig: src.ExtraConfig, ConfigPath: src.ConfigPath}
	for _, e := range entries {
		m.Files = append(m.Files, e.File)
		if e.Path == filepath.Join(src.PKIDir, caCertName) {
			m.ClusterCASHA256 = certFingerprint(e.data)
		}
	}
	plain, err := writeArchive(m, entries)
	if err != nil {
		return nil, Manifest{}, err
	}
	// A bundle that is taken must be one a restore accepts: the two sides
	// have their own rules and limits, and a bundle only the save side is
	// happy with is found out when the node it was for is gone.
	back := Destination{PKIDir: src.PKIDir, StatePath: src.StatePath, ConfDir: src.ConfDir, ManifestsDir: src.ManifestsDir,
		AllowExtra: src.ExtraConfig}
	if _, _, _, err := readBundle(plain, back); err != nil {
		return nil, Manifest{}, fmt.Errorf("the node state makes a bundle a restore would refuse: %w", err)
	}
	return plain, m, nil
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

const (
	caCertName = "ca.crt"
	caKeyName  = "ca.key"
)

// certFingerprint is the SHA-256 of the first certificate in the PEM data,
// or empty when there is none.
func certFingerprint(pemData []byte) string {
	block, _ := pem.Decode(pemData)
	if block == nil || block.Type != "CERTIFICATE" {
		return ""
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}

// caKeyMatches reports whether keyPEM is the private key of the certificate
// in certPEM. A bundle carrying a CA certificate beside another CA's key
// would restore a control plane that signs with a key nothing trusts, or with
// one someone else holds.
func caKeyMatches(certPEM, keyPEM []byte) error {
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return errors.New("the cluster CA certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return fmt.Errorf("the cluster CA certificate: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return errors.New("the cluster CA key is not PEM")
	}
	var key any
	switch kb.Type {
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(kb.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(kb.Bytes)
	default:
		key, err = x509.ParsePKCS8PrivateKey(kb.Bytes)
	}
	if err != nil {
		return fmt.Errorf("the cluster CA key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return errors.New("the cluster CA key is not a signing key")
	}
	pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(cert.PublicKey) {
		return errors.New("the cluster CA key is not the key of the cluster CA certificate")
	}
	return nil
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

// Inspect checks an opened bundle whole against its manifest and against dst,
// without writing anything, and returns the manifest. The cluster CA
// fingerprint it returns is computed from the certificate the bundle carries,
// not taken from what the bundle says of itself.
func Inspect(plain []byte, dst Destination) (Manifest, error) {
	m, _, _, err := readBundle(plain, dst)
	return m, err
}

func openArchive(plain []byte) (*tar.Reader, error) {
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, fmt.Errorf("node-state bundle: %w", err)
	}
	return tar.NewReader(io.LimitReader(gz, 2*maxBundleBytes)), nil
}

func readManifest(tr *tar.Reader) (Manifest, error) {
	h, err := tr.Next()
	if err != nil || h.Name != manifestName || h.Typeflag != tar.TypeReg {
		return Manifest{}, errors.New("node-state bundle does not begin with its manifest")
	}
	b, err := io.ReadAll(io.LimitReader(tr, maxBundleBytes+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("node-state bundle manifest: %w", err)
	}
	if int64(len(b)) > maxBundleBytes {
		return Manifest{}, errors.New("node-state bundle manifest is too large: the node state holds too many files")
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("node-state bundle manifest: %w", err)
	}
	if m.Format != formatVersion {
		return Manifest{}, fmt.Errorf("node-state bundle manifest is format %d; this stratum reads format %d", m.Format, formatVersion)
	}
	if err := checkManifest(m); err != nil {
		return Manifest{}, fmt.Errorf("node-state bundle manifest: %w; nothing was restored", err)
	}
	return m, nil
}

// checkManifest refuses a manifest whose text could mislead the person
// restoring it. A restore prints these fields, and whoever sealed the bundle
// wrote them: a control character could redraw the CA fingerprint on the
// terminal, and a config path is printed as part of a command to run.
func checkManifest(m Manifest) error {
	for name, v := range map[string]string{
		"hostname": m.Hostname, "nodeIP": m.NodeIP, "stratumVersion": m.StratumVersion,
		"kubernetesVersion": m.KubernetesVersion, "snapshot": m.Snapshot, "clusterCASHA256": m.ClusterCASHA256,
		"extraConfig": m.ExtraConfig, "configPath": m.ConfigPath,
	} {
		if !printable(v) {
			return fmt.Errorf("%s %q holds a character that is not printable", name, v)
		}
	}
	for _, f := range m.Files {
		if !printable(f.Path) || !printable(f.Link) {
			return fmt.Errorf("the entry %q holds a character that is not printable", f.Path)
		}
	}
	if m.ConfigPath != "" && (!filepath.IsAbs(m.ConfigPath) || filepath.Clean(m.ConfigPath) != m.ConfigPath) {
		return fmt.Errorf("configPath %q is not a plain absolute path", m.ConfigPath)
	}
	// The cluster config kept outside the configuration directory is the one
	// file a restore may write outside node state. It is the config the node
	// ran with and a YAML file, never a path of the bundle's choosing.
	if m.ExtraConfig != "" {
		if m.ExtraConfig != m.ConfigPath {
			return fmt.Errorf("extraConfig %q is not the config the node ran with, %q", m.ExtraConfig, m.ConfigPath)
		}
		if ext := filepath.Ext(m.ExtraConfig); ext != ".yaml" && ext != ".yml" {
			return fmt.Errorf("extraConfig %q is not a .yaml or .yml file", m.ExtraConfig)
		}
	}
	return nil
}

func printable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
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
	_, entries, skipped, err := readBundle(plain, dst)
	if err != nil {
		return Restored{}, err
	}
	res := Restored{ExtraConfigSkipped: skipped}
	// The allowlist is of names, and a write follows a link that is already
	// on the machine: a directory under the configuration directory that
	// links elsewhere would carry an allowed entry out of node state. Every
	// existing directory on the way to an entry is checked before the first
	// write, so a refusal changes nothing.
	for _, e := range entries {
		if err := dst.refuseLinkedParents(e.Path, e.Dir); err != nil {
			return Restored{}, err
		}
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

// readBundle reads an opened bundle whole and checks it: every entry is
// listed in the manifest and matches it, every listed entry is there, each
// lands inside dst, the cluster CA fingerprint the manifest states is the
// certificate's, and the CA key is that certificate's. It returns the
// entries to write and the path of an outside config file it left out.
func readBundle(plain []byte, dst Destination) (Manifest, []entry, string, error) {
	fail := func(err error) (Manifest, []entry, string, error) { return Manifest{}, nil, "", err }
	tr, err := openArchive(plain)
	if err != nil {
		return fail(err)
	}
	m, err := readManifest(tr)
	if err != nil {
		return fail(err)
	}
	listed := map[string]File{}
	for _, f := range m.Files {
		listed[f.Path] = f
	}
	var skipped string
	var entries []entry
	var caCert, caKey []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("node-state bundle: %w", err))
		}
		path := "/" + strings.TrimSuffix(h.Name, "/")
		if path != filepath.Clean(path) {
			return fail(fmt.Errorf("node-state bundle holds the path %q, which is not a plain path", h.Name))
		}
		want, ok := listed[path]
		if !ok {
			return fail(fmt.Errorf("node-state bundle holds %q, which its manifest does not list", path))
		}
		delete(listed, path)
		extra := m.ExtraConfig != "" && path == m.ExtraConfig
		if extra && h.Typeflag != tar.TypeReg {
			return fail(fmt.Errorf("node-state bundle holds its cluster config %s as something other than a file; nothing was restored", path))
		}
		if !dst.allows(path, m.ExtraConfig) {
			if extra {
				skipped = path
				continue
			}
			return fail(fmt.Errorf("node-state bundle holds %s, which is not node state; nothing was restored", path))
		}
		e := entry{File: want}
		if extra {
			// The outside config is written owner-only, whatever mode the
			// bundle states for it.
			e.Mode = want.Mode &^ 0o077
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if !want.Dir {
				return fail(fmt.Errorf("node-state bundle: %s does not match its manifest", path))
			}
		case tar.TypeSymlink:
			if want.Link != h.Linkname || h.Linkname == "" || h.Linkname == "." || h.Linkname == ".." ||
				strings.ContainsRune(h.Linkname, filepath.Separator) || !dst.linkAllowed(path) {
				return fail(fmt.Errorf("node-state bundle holds a link at %s it may not; nothing was restored", path))
			}
		case tar.TypeReg:
			if h.Size > maxFileBytes || h.Size != want.Size {
				return fail(fmt.Errorf("node-state bundle: %s is not the size its manifest states, or is over %d MiB", path, maxFileBytes>>20))
			}
			data, err := io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
			if err != nil {
				return fail(fmt.Errorf("node-state bundle: %s: %w", path, err))
			}
			sum := sha256.Sum256(data)
			if want.Dir || want.Link != "" || int64(len(data)) != want.Size || hex.EncodeToString(sum[:]) != want.SHA256 {
				return fail(fmt.Errorf("node-state bundle: %s does not match its manifest", path))
			}
			e.data = data
			switch path {
			case filepath.Join(dst.PKIDir, caCertName):
				caCert = data
			case filepath.Join(dst.PKIDir, caKeyName):
				caKey = data
			}
		default:
			return fail(fmt.Errorf("node-state bundle holds %s, which is neither a file, a directory nor a link", path))
		}
		entries = append(entries, e)
	}
	for path := range listed {
		return fail(fmt.Errorf("node-state bundle is missing %s, which its manifest lists", path))
	}
	// What the bundle says its cluster CA is counts for nothing: whoever
	// sealed it wrote that too. The fingerprint a restore shows and pins is
	// the certificate's own.
	if got := certFingerprint(caCert); got != m.ClusterCASHA256 {
		return fail(fmt.Errorf("node-state bundle states cluster CA %s and carries %s; nothing was restored",
			orNone(m.ClusterCASHA256), orNone(got)))
	}
	if caCert == nil || caKey == nil {
		return fail(errors.New("node-state bundle does not hold the cluster CA certificate and its key; a node started " +
			"from it would mint a new CA or refuse to start. Nothing was restored"))
	}
	if err := caKeyMatches(caCert, caKey); err != nil {
		return fail(fmt.Errorf("node-state bundle: %w; nothing was restored", err))
	}
	// A link in a bundle points to a file beside it. An entry beneath one
	// would be written through it, into whatever directory it named.
	links := map[string]bool{}
	for _, e := range entries {
		if e.Link != "" {
			links[e.Path] = true
		}
	}
	for _, e := range entries {
		for dir := filepath.Dir(e.Path); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
			if links[dir] {
				return fail(fmt.Errorf("node-state bundle holds %s beneath the link %s; nothing was restored", e.Path, dir))
			}
		}
	}
	return m, entries, skipped, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
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

// refuseLinkedParents refuses path when a directory between its tree's root
// and path exists on this machine as a symbolic link, or when path is a
// directory entry and itself exists as a link: making it and setting its mode
// would follow the link. A file or link entry replaces a link at its own path
// rather than following it.
func (d Destination) refuseLinkedParents(path string, dir bool) error {
	root := ""
	for _, r := range []string{d.PKIDir, d.ConfDir, d.ManifestsDir} {
		if r != "" && within(r, path) {
			root = r
		}
	}
	if root == "" {
		return nil
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	cur := root
	comps := strings.Split(filepath.Dir(rel), string(filepath.Separator))
	if dir {
		comps = append(comps, filepath.Base(rel))
	}
	for _, comp := range comps {
		if comp == "." || comp == "" {
			continue
		}
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s on this machine is a symbolic link, and restoring %s would write through it, outside node state; "+
				"remove the link. Nothing was restored", cur, path)
		}
	}
	return nil
}

// linkAllowed reports whether a link may be restored at path: inside one of
// the trees, where a link to a file beside it reaches nothing outside.
func (d Destination) linkAllowed(path string) bool {
	for _, root := range []string{d.PKIDir, d.ConfDir, d.ManifestsDir} {
		if root != "" && within(root, path) {
			return true
		}
	}
	return false
}

// allows reports whether path may be written. Outside the trees and
// node.yaml, only the bundle's own outside cluster config may be, and only
// when the operator named that same path.
func (d Destination) allows(path, extraConfig string) bool {
	for _, root := range []string{d.PKIDir, d.ConfDir, d.ManifestsDir} {
		if root != "" && (path == root || within(root, path)) {
			return true
		}
	}
	if d.StatePath != "" && path == d.StatePath {
		return true
	}
	return d.AllowExtra != "" && path == d.AllowExtra && path == extraConfig
}

// within reports whether path is strictly under root.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

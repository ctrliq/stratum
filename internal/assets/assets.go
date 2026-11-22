// A ZIP appended to the ELF is found by scanning back from EOF; the loader ignores trailing bytes.
package assets

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/version"
)

// Snapshotted at init, so version.SetRoots in a test does not redirect this var.
var DefaultRoot = version.DefaultRoot

// Directory names must match the archive's top-level layout.
const (
	BinDir    = "bin"
	ImagesDir = "images"
)

// Set for EMBEDDED_BINS=none builds; callers fall back to Lookup.
var ErrNoPayload = errors.New("no embedded payload in this executable")

// Skips files already staged by this executable: same size and its own mtime.
func Stage(root string) ([]string, error) { return StageWithWriter(os.Stdout, root) }

func StageWithWriter(w io.Writer, root string) ([]string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return StageFromWithWriter(w, exePath, root)
}

// Exists so tests stage a stand-in executable without re-execing the test binary.
func StageFrom(exePath, root string) ([]string, error) {
	return StageFromWithWriter(os.Stdout, exePath, root)
}

func StageFromWithWriter(w io.Writer, exePath, root string) ([]string, error) {
	exeInfo, err := os.Stat(exePath)
	if err != nil {
		return nil, err
	}

	zr, err := zip.OpenReader(exePath)
	if err != nil {
		if errors.Is(err, zip.ErrFormat) {
			return nil, ErrNoPayload
		}
		return nil, err
	}
	defer zr.Close()

	var staged []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel, ok := payloadPath(f.Name)
		if !ok {
			continue
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}

		if !upToDate(dst, int64(f.UncompressedSize64), exeInfo.ModTime()) {
			mode := os.FileMode(0o644)
			if strings.HasPrefix(rel, BinDir+"/") {
				mode = 0o755
			}
			if err := extract(w, f, dst, mode, exeInfo.ModTime()); err != nil {
				return nil, fmt.Errorf("staging %s: %w", rel, err)
			}
		}
		staged = append(staged, rel)
	}
	if len(staged) == 0 {
		return nil, ErrNoPayload
	}
	return staged, nil
}

// Only members under the payload directories pass, which also blocks traversal.
func payloadPath(name string) (string, bool) {
	clean := path.Clean("/" + name)[1:] // absolute-then-trim: kills . and ..
	dir, file := path.Split(clean)
	if file == "" {
		return "", false
	}
	switch strings.TrimSuffix(dir, "/") {
	case BinDir, ImagesDir:
		return clean, true
	default:
		return "", false
	}
}

func upToDate(path string, size int64, mtime time.Time) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Size() == size && fi.ModTime().Equal(mtime)
}

// A var so a test can see which files are labelled.
var labelLike = selinux.LabelLikeWithWriter

// Writes and renames a temp file, so a crash or concurrent staging leaves no half-written binary.
func extract(w io.Writer, f *zip.File, dst string, mode os.FileMode, mtime time.Time) error {
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	// The mtime is the staging cache key, so stamp before the rename makes it visible.
	if err := os.Chtimes(tmpName, mtime, mtime); err != nil {
		return err
	}
	if err := labelLike(w, tmpName, dst); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// Lists payload members without extracting, for `stratum version --components`.
func Names(exePath string) ([]string, error) {
	zr, err := zip.OpenReader(exePath)
	if err != nil {
		if errors.Is(err, zip.ErrFormat) {
			return nil, ErrNoPayload
		}
		return nil, err
	}
	defer zr.Close()

	var names []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if rel, ok := payloadPath(f.Name); ok {
			names = append(names, rel)
		}
	}
	if len(names) == 0 {
		return nil, ErrNoPayload
	}
	return names, nil
}

// For EMBEDDED_BINS=none builds: a staged file first, then PATH.
func Lookup(root, name string) (string, error) {
	staged := filepath.Join(root, BinDir, name)
	if fi, err := os.Stat(staged); err == nil && !fi.IsDir() {
		return staged, nil
	}
	return exec.LookPath(name)
}

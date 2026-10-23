// Package assets stages the payload embedded in the stratum executable onto disk
// at runtime.
//
// The executable is a normal ELF binary with a ZIP archive concatenated onto
// the end (see the package target in the repo Makefile). ZIP readers locate the
// central directory by scanning backwards from EOF, so the archive is found
// regardless of how much ELF precedes it, and the ELF loader ignores the
// trailing bytes. That trick is the whole reason one artifact can carry a control
// plane.
//
// SKE carries the container images in that payload as well as the binaries, so
// the single artifact is airgap-complete: nothing is fetched from a registry at
// install time. The archive has two top-level directories:
//
//	bin/     component binaries, staged executable
//	images/  an OCI archive of the pinned image set, staged read-only
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

// DefaultRoot is where the payload is staged. Binaries land in
// DefaultRoot/bin, images in DefaultRoot/images.
//
// The path itself lives in internal/version, derived from the data root. This
// name stays for its existing callers and snapshots version.DefaultRoot once
// at package init: production never reassigns the roots, so the snapshot and
// the derived path agree, but a test that moves the roots with version.SetRoots
// moves version.DefaultRoot and NOT this var — read version.DefaultRoot
// directly when a test-redirectable path is what the caller needs.
var DefaultRoot = version.DefaultRoot

// Payload directory names, matching the archive's top-level layout.
const (
	BinDir    = "bin"
	ImagesDir = "images"
)

// ErrNoPayload reports an executable built without an embedded archive
// (EMBEDDED_BINS=none). Callers fall back to Lookup.
var ErrNoPayload = errors.New("no embedded payload in this executable")

// Stage extracts the whole payload under root, preserving the archive's bin/
// and images/ layout, and returns the staged paths relative to root.
//
// Extraction is skipped per file when the destination already has the same size
// and modification time, which is what makes a restart cheap: the executable's
// own mtime is stamped onto each staged file, so "already staged by this exact
// binary" is a two-field comparison rather than a hash of half a gigabyte.
func Stage(root string) ([]string, error) { return StageWithWriter(os.Stdout, root) }

// StageWithWriter stages this executable's payload and reports label warnings to w.
func StageWithWriter(w io.Writer, root string) ([]string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return StageFromWithWriter(w, exePath, root)
}

// StageFrom is Stage with the executable named explicitly: it extracts the
// ZIP-append payload of the binary at exePath. The upgrade path uses it to
// stage a staged upgrade binary's payload; it is also what makes staging
// testable without re-execing the test binary.
func StageFrom(exePath, root string) ([]string, error) {
	return StageFromWithWriter(os.Stdout, exePath, root)
}

// StageFromWithWriter stages a payload and reports label warnings to w.
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

// payloadPath validates an archive member name and returns its path relative to
// the staging root. Only members under the known payload directories are
// accepted, which is also what makes traversal ("../../etc/passwd") impossible:
// a cleaned path that no longer starts with a payload directory is dropped.
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

// labelLike is selinux.LabelLikeWithWriter; a var so a test can see which files are
// labelled.
var labelLike = selinux.LabelLikeWithWriter

// extract writes to a temp file in the destination directory and renames it
// into place, so a crashed or concurrent staging never leaves a half-written
// binary that a supervisor would happily exec.
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
	// Stamp before rename: the mtime is the staging cache key, so it must be
	// correct the instant the file becomes visible under its real name.
	if err := os.Chtimes(tmpName, mtime, mtime); err != nil {
		return err
	}
	if err := labelLike(w, tmpName, dst); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// Names lists the payload members of the given executable without extracting
// anything, so `stratum version --components` can report what the artifact
// carries cheaply.
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

// Lookup resolves a component binary for a no-payload build: an already-staged
// file first, then PATH. Used by EMBEDDED_BINS=none builds (dev and
// OS-packaged variants, where the OS package places the binaries).
func Lookup(root, name string) (string, error) {
	staged := filepath.Join(root, BinDir, name)
	if fi, err := os.Stat(staged); err == nil && !fi.IsDir() {
		return staged, nil
	}
	return exec.LookPath(name)
}

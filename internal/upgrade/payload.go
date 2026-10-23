package upgrade

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/version"
)

// Source is a staged upgrade payload.
type Source struct {
	Dir    string // dir holding the payload's host binaries
	Images string // dir of payload image archives; "" when the payload carries none (registry path)
}

// imagesDirIn reports root's image-archive dir, or "" when there is none —
// a payload built without images, on a node that pulls from a registry.
func imagesDirIn(root string) string {
	d := filepath.Join(root, assets.ImagesDir)
	if fi, err := os.Stat(d); err == nil && fi.IsDir() {
		return d
	}
	return ""
}

// Resolve extracts the upgrade payload into version.StageDir and verifies it
// carries every binary in required.
//
// When from=="" it extracts this stratum binary's OWN ZIP-append payload — a
// newer stratum binary IS the upgrade artifact. When from!="" it
// must name the upgrade binary: the new stratum binary staged onto this node,
// whose payload is extracted the same way. A directory, missing file, or
// payload-less file is refused before anything on the node changes.
func Resolve(from string, required []string) (Source, error) {
	return ResolveWithWriter(os.Stdout, from, required)
}

// ResolveWithWriter resolves the upgrade payload and reports staging warnings to w.
func ResolveWithWriter(w io.Writer, from string, required []string) (Source, error) {
	// One resolution per call: the extract and the verify that follows it must
	// answer the same staging directory, and it is read at call time so a
	// SetRoots redirect moves staging with the data root.
	staged := version.StageDir
	if from != "" {
		fi, err := os.Stat(from)
		if err != nil {
			return Source{}, fmt.Errorf("upgrade binary %s: %w", from, err)
		}
		if fi.IsDir() {
			return Source{}, fmt.Errorf("%s is a directory — --from takes the upgrade binary itself (the new stratum binary), not a directory of binaries", from)
		}
		if !fi.Mode().IsRegular() {
			return Source{}, fmt.Errorf("upgrade binary %s is not a regular file", from)
		}
		if _, err := assets.StageFromWithWriter(w, from, staged); err != nil {
			if errors.Is(err, assets.ErrNoPayload) {
				return Source{}, fmt.Errorf("%s carries no embedded payload — is it really a stratum upgrade binary?", from)
			}
			return Source{}, err
		}
	} else if _, err := assets.StageWithWriter(w, staged); err != nil {
		if errors.Is(err, assets.ErrNoPayload) {
			return Source{}, fmt.Errorf("this stratum binary carries no payload (EMBEDDED_BINS=none build); use --from <upgrade-binary>")
		}
		return Source{}, err
	}
	dir := filepath.Join(staged, assets.BinDir)
	for _, b := range required {
		if _, err := os.Stat(filepath.Join(dir, b)); err != nil {
			return Source{}, fmt.Errorf("payload missing %s after extract: %w", b, err)
		}
	}
	return Source{Dir: dir, Images: imagesDirIn(staged)}, nil
}

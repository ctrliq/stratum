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

type Source struct {
	Dir    string
	Images string // "" when the payload carries no image archives and the node pulls from a registry
}

// imagesDirIn reports root's image-archive dir, or "" for a payload built without images.
func imagesDirIn(root string) string {
	d := filepath.Join(root, assets.ImagesDir)
	if fi, err := os.Stat(d); err == nil && fi.IsDir() {
		return d
	}
	return ""
}

// Resolve stages the upgrade payload into version.StageDir: --from's own payload, or this binary's.
func Resolve(from string, required []string) (Source, error) {
	return ResolveWithWriter(os.Stdout, from, required)
}

func ResolveWithWriter(w io.Writer, from string, required []string) (Source, error) {
	// Read at call time, so a SetRoots redirect moves staging with the data root.
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

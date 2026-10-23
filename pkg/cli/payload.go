package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/version"
)

// stagePayload is assets.Stage behind a seam: the test binary carries no
// payload, so a test stubs the extraction to see whether and where stage ran
// it.
var stagePayload = assets.Stage

// lockStageRoot takes the etcd data lock when staging can write into the node
// data tree, and returns a no-op release for unrelated roots. Uninstall holds that
// lock while it deletes version.DataDir, and `stratum up` and the HA join hold
// it while they stage into it; a stage that skipped it could recreate part of
// the dir uninstall is removing. Other roots stay unlocked because CI stages
// into temp dirs as an unprivileged user, who cannot create the lock beside the
// data dir. Both lexical descendants and symlink aliases take the lock. The
// payload's bin and images directories are checked too: an unrelated root can
// have a child symlink into the data tree. Paths that cannot be resolved refuse
// staging before extraction.
func lockStageRoot(root string) (release func(), err error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve staging root %s: %w", root, err)
	}
	dataDir, err := filepath.Abs(version.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve node data directory %s: %w", version.DataDir, err)
	}
	realDataDir, err := resolveStagePath(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve node data directory %s: %w", dataDir, err)
	}
	for _, path := range []string{abs, filepath.Join(abs, assets.BinDir), filepath.Join(abs, assets.ImagesDir)} {
		// Uninstall removes lexical descendants even if one currently links
		// outside the tree. Check before resolving that link.
		if stagePathWithin(dataDir, path) {
			return etcddata.Lock(version.EtcdDataDir)
		}
		real, err := resolveStagePath(path)
		if err != nil {
			return nil, fmt.Errorf("resolve staging path %s: %w", path, err)
		}
		if stagePathWithin(realDataDir, real) {
			return etcddata.Lock(version.EtcdDataDir)
		}
	}
	return func() {}, nil
}

func stagePathWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}

// resolveStagePath resolves existing ancestors without creating the missing
// tail that staging would write. A dangling link is refused: its target can
// reappear in the node data tree after uninstall removes it.
func resolveStagePath(path string) (string, error) {
	rest := ""
	for p := path; ; p = filepath.Dir(p) {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) || p == filepath.Dir(p) {
			return "", err
		}
		if _, err := os.Lstat(p); err == nil {
			return "", fmt.Errorf("%s is a dangling symlink", p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

func newStageCommand() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:         "stage",
		Short:       "Extract the embedded payload (binaries + images)",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{ownsNodeStateAnnotation: "1"},
		RunE: func(cmd *cobra.Command, args []string) error {
			release, err := lockStageRoot(root)
			if err != nil {
				return err
			}
			staged, err := stagePayload(root)
			release()
			if errors.Is(err, assets.ErrNoPayload) {
				return fmt.Errorf("this build carries no embedded payload (EMBEDDED_BINS=none); components must come from %s or PATH", filepath.Join(root, assets.BinDir))
			}
			if err != nil {
				return err
			}
			for _, rel := range staged {
				fmt.Println(filepath.Join(root, rel))
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "staged %d payload members into %s\n", len(staged), filepath.Clean(root))
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", assets.DefaultRoot, "directory to stage the payload into")
	return cmd
}

// newWhichCommand answers "where would the supervisor find this component" —
// the question that matters for a no-payload build, where components come
// from the staging dir or PATH rather than from inside the binary.
func newWhichCommand() *cobra.Command {
	var root string
	cmd := &cobra.Command{
		Use:   "which NAME",
		Short: "Resolve a component: staged file, then PATH",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			found, err := assets.Lookup(root, args[0])
			if err != nil {
				return fmt.Errorf("%s not found in %s or on PATH", args[0], filepath.Join(root, assets.BinDir))
			}
			fmt.Println(found)
			return nil
		},
	}
	cmd.Flags().StringVar(&root, "root", assets.DefaultRoot, "staging root to look in first")
	return cmd
}

package backupevidence

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ctrliq/stratum/internal/host"
)

// No cheap digest source; the catalog never hashes a snapshot to guess one.
const DigestUnknown = "unknown"

const (
	DefaultLocalMaxEntries = 1000
	DefaultLocalMaxBytes   = int64(1) << 40
)

// A sha256 digest fits far below this; a longer read is not a digest.
const localSidecarReadLimit = 128

const localReadPageSize = 128

const cancelledWalkWarning = "WARN: the local snapshot walk stopped early: the request was cancelled; the local catalog is not a complete total"

// Artifact is one local snapshot file the catalog enumerated.
type Artifact struct {
	// The engine derives it from node and UTC stamp, so a snapshot matches its remote copy.
	ID         string
	LocationID string
	CopyID     string
	Location   string
	// The modification time: a snapshot file is write-once.
	Created time.Time
	Size    int64
	// A valid sha256 sidecar beside the file, else DigestUnknown.
	Digest string
}

type LocalOptions struct {
	Dir        string
	Node       string
	MaxEntries int
	MaxBytes   int64
}

type LocalResult struct {
	Node      string
	Artifacts []Artifact
	// False means len(Artifacts) is a floor, never the store's total.
	Complete bool
	// The condition, never a raw storage error or a credential.
	Warnings []string
}

var (
	localHostname = host.Hostname
	// localOpen opens the snapshot directory for the paged walk.
	localOpen = func(path string) (*os.File, error) {
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		fi, err := f.Stat()
		if err == nil && !fi.IsDir() {
			err = errors.New("snapshot store is not a directory")
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
	localEntryInfo = func(ctx context.Context, dir, name string) (int64, time.Time, error) {
		if err := ctx.Err(); err != nil {
			return 0, time.Time{}, err
		}
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return 0, time.Time{}, err
		}
		if !fi.Mode().IsRegular() {
			return 0, time.Time{}, errNotSnapshot
		}
		return fi.Size(), fi.ModTime(), nil
	}
	// present with an empty digest means the sidecar carries no sha256.
	localSidecar = func(ctx context.Context, dir, name string) (digest string, present bool, err error) {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		// Nonblocking open also covers a FIFO swapped in before the type check.
		f, err := os.OpenFile(filepath.Join(dir, name+".sha256"), os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", false, nil
			}
			return "", true, err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return "", true, err
		}
		if !fi.Mode().IsRegular() {
			return "", true, errors.New("digest sidecar is not a regular file")
		}
		buf := make([]byte, localSidecarReadLimit)
		n, rerr := f.Read(buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return "", true, rerr
		}
		fields := strings.Fields(string(buf[:n]))
		if len(fields) == 0 {
			return "", true, nil
		}
		b, derr := hex.DecodeString(strings.TrimPrefix(fields[0], "sha256:"))
		if derr != nil || len(b) != 32 {
			return "", true, nil
		}
		return "sha256:" + hex.EncodeToString(b), true, nil
	}
)

var errNotSnapshot = errors.New("snapshot entry is not a regular file")

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Local enumerates the node's snapshot directory as a bounded catalog.
func Local(ctx context.Context, o LocalOptions) LocalResult {
	var res LocalResult
	var warns []string

	if res.Node = o.Node; res.Node == "" {
		hn, err := localHostname()
		if err != nil {
			warns = append(warns, "WARN: this node's name could not be determined; the local catalog's location is not node-scoped")
		} else {
			res.Node = hn
		}
	}

	maxEntries, maxBytes := o.MaxEntries, o.MaxBytes
	if maxEntries <= 0 {
		maxEntries = DefaultLocalMaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = DefaultLocalMaxBytes
	}

	if err := ctx.Err(); err != nil {
		res.Warnings = append(warns, "WARN: the local snapshot walk was cancelled before it started; the local catalog is not a complete total")
		return res
	}

	f, err := localOpen(o.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			res.Complete = true
			res.Warnings = warns
			return res
		}
		res.Warnings = append(warns, fmt.Sprintf("WARN: the local snapshot directory %s cannot be read; the local catalog is unknown and not a total", o.Dir))
		return res
	}
	defer f.Close()

	var artifacts []Artifact
	var accounted int64
	complete := true
walk:
	for {
		if err := ctx.Err(); err != nil {
			warns = append(warns, cancelledWalkWarning)
			complete = false
			break walk
		}
		page, perr := f.ReadDir(localReadPageSize)
		for _, e := range page {
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".db") {
				continue
			}
			if err := ctx.Err(); err != nil {
				warns = append(warns, cancelledWalkWarning)
				complete = false
				break walk
			}
			if len(artifacts) == maxEntries {
				warns = append(warns, fmt.Sprintf("WARN: the local snapshot walk stopped early: the page cap of %d entries was reached; the local catalog is not a complete total", maxEntries))
				complete = false
				break walk
			}
			size, mod, ierr := localEntryInfo(ctx, o.Dir, e.Name())
			switch {
			case isCancellation(ierr):
				warns = append(warns, cancelledWalkWarning)
				complete = false
				break walk
			case errors.Is(ierr, fs.ErrNotExist), errors.Is(ierr, errNotSnapshot):
				continue
			case ierr != nil:
				warns = append(warns, fmt.Sprintf("WARN: the local snapshot %s cannot be read; its size and creation time are unknown, and the local catalog is not a complete total", e.Name()))
				complete = false
				continue
			}
			if accounted+size > maxBytes {
				warns = append(warns, fmt.Sprintf("WARN: the local snapshot walk stopped early: the byte cap of %d bytes was reached; the local catalog is not a complete total", maxBytes))
				complete = false
				break walk
			}
			accounted += size
			a := Artifact{ID: e.Name(), Location: "local", Created: mod, Size: size, Digest: DigestUnknown}
			digest, present, derr := localSidecar(ctx, o.Dir, e.Name())
			switch {
			case isCancellation(derr):
				warns = append(warns, cancelledWalkWarning)
				complete = false
				artifacts = append(artifacts, a)
				break walk
			case derr != nil:
				warns = append(warns, fmt.Sprintf("WARN: the digest sidecar for %s cannot be read; its digest is unknown", e.Name()))
			case present && digest == "":
				warns = append(warns, fmt.Sprintf("WARN: the digest sidecar for %s does not carry a sha256 digest; the digest is unknown", e.Name()))
			case present:
				a.Digest = digest
			}
			artifacts = append(artifacts, a)
		}
		if perr != nil {
			if !errors.Is(perr, io.EOF) {
				warns = append(warns, fmt.Sprintf("WARN: the local snapshot directory %s stopped yielding entries; the local catalog is not a complete total", o.Dir))
				complete = false
			}
			break walk
		}
		if len(page) == 0 {
			break walk
		}
	}

	slices.SortFunc(artifacts, func(a, b Artifact) int { return strings.Compare(a.ID, b.ID) })
	res.Artifacts = artifacts
	res.Complete = complete
	res.Warnings = warns
	return res
}

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

// DigestUnknown is an artifact's digest value when nothing cheaply knows one:
// no sidecar records it, and the local store has no ETag. The catalog never
// hashes a snapshot to guess one.
const DigestUnknown = "unknown"

// The built-in bounds: the walk is always finite, and a caller that passes no
// cap gets these, not an unbounded walk.
const (
	// DefaultLocalMaxEntries is the page cap: how many artifacts one walk may
	// enumerate before it stops and reports the catalog partial.
	DefaultLocalMaxEntries = 1000
	// DefaultLocalMaxBytes is the byte cap: how many artifact bytes one walk
	// may account for before it stops and reports the catalog partial.
	DefaultLocalMaxBytes = int64(1) << 40
)

// The per-read bound of the digest sidecar lookup: a whole sha256 digest fits
// in far less, so anything longer is not one.
const localSidecarReadLimit = 128

// localReadPageSize is how many directory entries the walk reads per
// filesystem page: the walk streams the directory instead of loading it.
const localReadPageSize = 128

// cancelledWalkWarning is what a walk stopped by its context says: the
// entries already described stand, but the store's total is not known.
const cancelledWalkWarning = "WARN: the local snapshot walk stopped early: the request was cancelled; the local catalog is not a complete total"

// Artifact is one local snapshot file the catalog enumerated.
type Artifact struct {
	// ID is the artifact's stable identity: its file name, which the engine
	// derives from the node and the save's UTC stamp — so the same snapshot
	// uploaded off-box carries the same ID there and the two count once in a
	// deduplicated union.
	ID string
	// LocationID identifies the effective store without exposing its settings.
	LocationID string
	// CopyID identifies this backup at that location. Empty means unknown.
	CopyID string
	// Location is "local": this node's snapshot directory. The result's Node
	// names the node; a remote copy carries its own location.
	Location string
	// Created is the file's modification time: the moment the save finished
	// writing it, since a snapshot file is write-once.
	Created time.Time
	// Size is the file's size in bytes.
	Size int64
	// Digest is the artifact's digest when one is cheaply known — a valid
	// sha256 sidecar beside the file — else DigestUnknown.
	Digest string
}

// LocalOptions bounds and scopes the local snapshot catalog walk. A zero cap
// means the built-in default, never "unbounded".
type LocalOptions struct {
	// Dir is the snapshot directory to enumerate.
	Dir string
	// Node is the node whose store Dir is, scoping the catalog's location.
	// Empty means this host's short name.
	Node string
	// MaxEntries caps how many artifacts the walk may enumerate (the page
	// cap). Zero or negative means DefaultLocalMaxEntries.
	MaxEntries int
	// MaxBytes caps the artifact bytes the walk may account for. Zero or
	// negative means DefaultLocalMaxBytes.
	MaxBytes int64
}

// LocalResult is the bounded local catalog: the artifacts enumerated within
// the bounds, whether the walk covered the whole store, and the walk's safe
// diagnostics.
type LocalResult struct {
	// Node scopes every artifact's local location to this node.
	Node string
	// Artifacts are the enumerated snapshots, sorted by ID. It is the
	// store's exact total only when Complete is true.
	Artifacts []Artifact
	// Complete says the walk covered the whole directory within its bounds
	// and described every candidate: Artifacts is then the store's exact
	// total, an empty one included. False means the walk stopped early — a
	// cap, a cancellation, or an unreadable entry — and len(Artifacts) is a
	// floor, never the store's total.
	Complete bool
	// Warnings carry the walk's WARN conditions as safe diagnostics: they
	// name the condition and the affected snapshot, never a raw storage
	// error or a credential.
	Warnings []string
}

// The reads the walk goes through, each behind a seam so a test can point the
// walk at its own files and its own hung or denied reads (the same seam
// convention as the policy's config reads and the loop's seams).
var (
	// localHostname resolves this host's short name when the options do not
	// scope the walk to a node.
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
	// localEntryInfo reads one candidate snapshot's size and modification
	// time, honouring the request context the walk passes down.
	localEntryInfo = func(ctx context.Context, dir, name string) (int64, time.Time, error) {
		if err := ctx.Err(); err != nil {
			return 0, time.Time{}, err
		}
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return 0, time.Time{}, err
		}
		return fi.Size(), fi.ModTime(), nil
	}
	// localSidecar reads the digest sidecar beside a snapshot — <name>.sha256
	// in the sha256sum layout — in one bounded read. present says a sidecar
	// exists; an empty digest with present means it carries no sha256, which
	// the walk reports as unknown rather than guess from.
	localSidecar = func(ctx context.Context, dir, name string) (digest string, present bool, err error) {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		// A sidecar must be a regular file. Nonblocking open also handles a
		// FIFO replaced between opening and checking its file type.
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

// isCancellation says the walk's own request ended — the bound the caller put
// on it — rather than storage failing.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Local enumerates the node's snapshot directory as a bounded catalog: every
// snapshot file with its stable ID, node-scoped location, creation time, size
// and sidecar-known digest, under the page, byte and time bounds the options
// and the context set. The walk streams the directory in pages, stops the
// moment the context ends, and turns an unreadable entry or directory into a
// safe warning — never a raw storage error. A store that is empty, or whose
// directory was never created, is a complete exact zero; a walk stopped by a
// cap or a cancellation reports Complete false so a partial listing is never
// presented as the store's total.
func Local(ctx context.Context, o LocalOptions) LocalResult {
	var res LocalResult
	var warns []string

	// The node whose store this is, scoping the location every reported
	// artifact carries. A host whose name cannot be read is a warning, not
	// a failure: the artifacts are still this node's, they just cannot say
	// whose.
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

	// A request that already ended reads nothing: the store may hold
	// anything, so the result is not a total.
	if err := ctx.Err(); err != nil {
		res.Warnings = append(warns, "WARN: the local snapshot walk was cancelled before it started; the local catalog is not a complete total")
		return res
	}

	f, err := localOpen(o.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The directory was never created: the store has never held a
			// snapshot — an exact zero, not an unknown.
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
			// Only snapshot files are artifacts: partial saves, sidecars,
			// unrelated files and nested directories are not, and naming
			// each would be noise, not evidence.
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
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
			case errors.Is(ierr, fs.ErrNotExist):
				// Vanished between the listing and the read: no longer an
				// artifact, so not a diagnostic either.
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
				// The entry is fully described; it joins the catalog with an
				// unknown digest, and the walk ends right here.
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
				// The directory stopped yielding entries mid-walk: what was
				// read stands, but the walk did not reach the end.
				warns = append(warns, fmt.Sprintf("WARN: the local snapshot directory %s stopped yielding entries; the local catalog is not a complete total", o.Dir))
				complete = false
			}
			break walk
		}
		if len(page) == 0 {
			break walk
		}
	}

	// A stable order whatever order the directory streamed in, so consumers
	// can page and diff against the catalog across calls.
	slices.SortFunc(artifacts, func(a, b Artifact) int { return strings.Compare(a.ID, b.ID) })
	res.Artifacts = artifacts
	res.Complete = complete
	res.Warnings = warns
	return res
}

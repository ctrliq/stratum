package backupevidence

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/objstore"
)

// remoteLocation is the Location a remote copy's artifact carries: the S3
// target the options name, beside the local snapshot directory's "local".
const remoteLocation = "remote"

// RemoteOptions bounds and scopes the remote catalog. The bounds pass
// straight through to the object store's bounded listing; a zero or negative
// cap means that listing's built-in default, never an unbounded listing.
type RemoteOptions struct {
	// Target is the S3-compatible store to list. A nil or disabled target
	// holds zero remote copies exactly: nothing outside the node exists to
	// hold one.
	Target *objstore.Target
	// MaxPages, MaxBytes and MaxTime bound the listing.
	MaxPages int
	MaxBytes int64
	MaxTime  time.Duration
	// RetentionCutoff flags the copies created before it — the old remote
	// copies — to retention diagnostics. The zero time flags none: a caller
	// with no retention window in hand judges nothing against one.
	RetentionCutoff time.Time
}

// RemoteResult is the bounded remote catalog: the remote copies enumerated
// within the bounds, which of them predate the retention cutoff, whether the
// listing covered the whole store, and the catalog's safe diagnostics.
type RemoteResult struct {
	// Copies are the enumerated remote copies, sorted by ID, each carrying
	// the remote location and an unknown digest (a listing exposes no
	// digest). They are the remote store's exact total only when Complete
	// is true.
	Copies []Artifact
	// Old are the copies created before the retention cutoff, with their
	// creation times — remote copies predating the retention window or
	// surviving a failed prune.
	Old []Artifact
	// Complete says the listing covered the whole store within its bounds:
	// len(Copies) is then the remote store's exact total, an empty one
	// included. False means the listing stopped short — denied, timeout, a
	// cancellation or a cap — and len(Copies) is a floor while the store's
	// total is unknown, never zero.
	Complete bool
	// Warnings carry the catalog's WARN conditions as safe diagnostics:
	// they name the condition, never the store's address or raw error.
	Warnings []string
}

// Remote lists the remote copies the target holds as a bounded catalog: every
// object under the prefix with its stable name-derived ID, the remote
// location, its creation time (the object's LastModified), its size and an
// unknown digest. A listing that stops short — a hung or denied store, a
// cancelled request, a cap — returns what it listed as a floor with unknown
// completeness and a safe warning, never a raw error.
func Remote(ctx context.Context, o RemoteOptions) RemoteResult {
	var res RemoteResult

	// No remote target is configured: nothing outside the node exists to
	// hold a copy, which zero remote copies states exactly.
	if o.Target == nil || !o.Target.Enabled() {
		res.Complete = true
		return res
	}
	// A request that already ended reads nothing: the store may hold
	// anything, so the catalog is not a total.
	if err := ctx.Err(); err != nil {
		res.Warnings = append(res.Warnings, "WARN: the remote listing was cancelled before it started; the remote catalog is unknown and not a complete total")
		return res
	}

	lr := o.Target.ListBounded(ctx, objstore.ListOptions{
		MaxPages: o.MaxPages, MaxBytes: o.MaxBytes, MaxTime: o.MaxTime,
	})
	for _, ob := range lr.Objects {
		res.Copies = append(res.Copies, Artifact{
			ID:       ob.Name,
			Location: remoteLocation,
			Created:  ob.Modified,
			Size:     ob.Size,
			Digest:   DigestUnknown,
		})
	}
	slices.SortFunc(res.Copies, func(a, b Artifact) int { return strings.Compare(a.ID, b.ID) })

	if lr.End == objstore.ListEndedComplete {
		res.Complete = true
	} else {
		res.Warnings = append(res.Warnings, remoteEndWarning(lr.End))
	}

	// The old remote copies: created before the retention cutoff, whatever
	// kept them — a window that moved past them or a prune that failed to
	// delete them. They are listed with every other copy; this only marks
	// them, with their creation times, for retention diagnostics.
	if !o.RetentionCutoff.IsZero() {
		for _, c := range res.Copies {
			if c.Created.Before(o.RetentionCutoff) {
				res.Old = append(res.Old, c)
			}
		}
	}
	return res
}

// remoteEndWarning is the safe warning for how a listing ended short of
// complete: it names the condition, never the store's address or raw error.
func remoteEndWarning(end objstore.ListEnd) string {
	switch end {
	case objstore.ListEndedDenied:
		return "WARN: the remote store refused the listing; the remote catalog is unknown and not a complete total"
	case objstore.ListEndedTimeout:
		return "WARN: the remote listing exceeded its time bound; the remote catalog is unknown and not a complete total"
	case objstore.ListEndedCancelled:
		return "WARN: the remote listing was cancelled; the remote catalog is unknown and not a complete total"
	case objstore.ListEndedCapped:
		return "WARN: the remote listing stopped early at its page or byte bound; the remote catalog is unknown and not a complete total"
	default:
		return "WARN: the remote listing failed; the remote catalog is unknown and not a complete total"
	}
}

// CensusEntry is one stable backup ID counted exactly once in the union: the
// local artifact, the remote copy, or both when the same snapshot exists in
// both places — saved to the node's snapshot directory and uploaded off-box.
type CensusEntry struct {
	// ID is the backup's stable identity: its name, shared by the local
	// artifact and the remote copy of the same snapshot.
	ID string
	// Local and Remote are the ID's local artifact and remote copy, each
	// nil when that location does not hold it.
	Local  *Artifact
	Remote *Artifact
	// OldRemote says the remote copy predates the retention cutoff.
	OldRemote bool
}

// Census is the deduplicated union of the local and remote catalogs: every
// stable backup ID counted once, with whether the union's total is exact or
// unknown. A partial listing on either side never yields an exact total: the
// entries counted stand, and the true total is unknown.
type Census struct {
	// Node scopes the union's local half to the node whose store it is.
	Node string
	// Entries are the distinct backups, sorted by ID. Their count is the
	// union's exact total only when Complete is true; otherwise it is a
	// floor.
	Entries []CensusEntry
	// Complete says both catalogs covered their stores within their
	// bounds, so the entry count is the exact total of distinct backups.
	// False means a catalog is partial and the true total is unknown —
	// never an exact count, however many entries were counted.
	Complete bool
	// Warnings carry both catalogs' warnings plus the union's own, as safe
	// diagnostics.
	Warnings []string
	// Retention holds the retention diagnostics: every old remote copy,
	// named with its creation time.
	Retention []string
}

// Union merges the local and remote catalogs into a census counted once by
// stable backup ID. The census is exact only when both catalogs are
// complete; a partial catalog on either side leaves the total unknown with
// the entries counted as a floor, and a warning says so.
func Union(local LocalResult, remote RemoteResult) Census {
	var c Census
	c.Node = local.Node

	byID := make(map[string]*CensusEntry)
	entry := func(id string) *CensusEntry {
		e, ok := byID[id]
		if !ok {
			e = &CensusEntry{ID: id}
			byID[id] = e
		}
		return e
	}
	// The same snapshot saved locally and uploaded off-box carries the same
	// name in both stores: one entry, two locations.
	for i := range local.Artifacts {
		a := local.Artifacts[i]
		entry(a.ID).Local = &a
	}
	for i := range remote.Copies {
		a := remote.Copies[i]
		entry(a.ID).Remote = &a
	}

	old := make(map[string]bool)
	for _, oc := range remote.Old {
		old[oc.ID] = true
	}
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		e := byID[id]
		e.OldRemote = old[id]
		c.Entries = append(c.Entries, *e)
	}

	c.Complete = local.Complete && remote.Complete
	c.Warnings = append(c.Warnings, local.Warnings...)
	c.Warnings = append(c.Warnings, remote.Warnings...)
	if !c.Complete {
		c.Warnings = append(c.Warnings, "WARN: the union's total is unknown: a catalog in the union is partial, and a partial listing never yields an exact total")
	}

	// The retention diagnostics: every old remote copy named with its
	// creation time, whether the local half also holds it or not.
	for _, e := range c.Entries {
		if e.OldRemote {
			c.Retention = append(c.Retention, fmt.Sprintf(
				"WARN: the remote copy %s was created %s, before the retention cutoff; it predates the retention window or survived a failed prune",
				e.ID, e.Remote.Created.UTC().Format(time.RFC3339)))
		}
	}
	return c
}

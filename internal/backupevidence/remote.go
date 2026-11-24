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

const remoteLocation = "remote"

type RemoteOptions struct {
	Target   *objstore.Target
	MaxPages int
	MaxBytes int64
	MaxTime  time.Duration
	// The zero time flags nothing.
	RetentionCutoff time.Time
}

type RemoteResult struct {
	Copies []Artifact
	Old    []Artifact
	// False means len(Copies) is a floor, never the store's total.
	Complete bool
	// The condition, never the store's address or raw error.
	Warnings []string
}

// Remote lists every copy under the target's shared prefix, across all controllers.
func Remote(ctx context.Context, o RemoteOptions) RemoteResult {
	var res RemoteResult

	if o.Target == nil || !o.Target.Enabled() {
		res.Complete = true
		return res
	}
	if err := ctx.Err(); err != nil {
		res.Warnings = append(res.Warnings, "WARN: the remote listing was cancelled before it started; the remote catalog is unknown and not a complete total")
		return res
	}

	lr := o.Target.ListBounded(ctx, objstore.ListOptions{
		MaxPages: o.MaxPages, MaxBytes: o.MaxBytes, MaxTime: o.MaxTime,
	})
	for _, ob := range lr.Objects {
		if strings.HasSuffix(ob.Key, "/") || !strings.HasSuffix(ob.Name, ".db") {
			continue
		}
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

	if !o.RetentionCutoff.IsZero() {
		for _, c := range res.Copies {
			if c.Created.Before(o.RetentionCutoff) {
				res.Old = append(res.Old, c)
			}
		}
	}
	return res
}

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

type CensusEntry struct {
	ID        string
	Local     *Artifact
	Remote    *Artifact
	OldRemote bool
}

type Census struct {
	Node    string
	Entries []CensusEntry
	// False means the entry count is a floor, never the total.
	Complete bool
	// Both catalogs' warnings plus the union's own.
	Warnings  []string
	Retention []string
}

// Union merges the local and remote catalogs into a census counted once by backup ID.
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
	// The same snapshot carries the same name in both stores: one entry, two locations.
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

	for _, e := range c.Entries {
		if e.OldRemote {
			c.Retention = append(c.Retention, fmt.Sprintf(
				"WARN: the remote copy %s was created %s, before the retention cutoff; it predates the retention window or survived a failed prune",
				e.ID, e.Remote.Created.UTC().Format(time.RFC3339)))
		}
	}
	return c
}

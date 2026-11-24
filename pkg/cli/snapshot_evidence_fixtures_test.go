package cli

import (
	"encoding/json"
	"github.com/ctrliq/stratum/internal/version"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type shippedFixture struct {
	name string
	doc  snapshotListJSON
}

func shippedFixtures(t *testing.T) []shippedFixture {
	t.Helper()
	const dir = "../../docs/docs/appendices/backup-evidence-fixtures"
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures found under %s", dir)
	}
	slices.Sort(paths)
	fixtures := make([]shippedFixture, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var doc snapshotListJSON
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s does not decode as the pinned wire contract: %v", p, err)
		}
		fixtures = append(fixtures, shippedFixture{name: filepath.Base(p), doc: doc})
	}
	return fixtures
}

// A census counts every stable ID once, so a repeating ID collides here.
func byID(arts []snapshotListJSONArt) map[string]snapshotListJSONArt {
	m := make(map[string]snapshotListJSONArt, len(arts))
	for _, a := range arts {
		m[a.ID] = a
	}
	return m
}

func TestShippedFixturesReportADocumentedSituation(t *testing.T) {
	states := map[string]bool{
		"configured":   true,
		"disabled":     true,
		"unconfigured": true,
		"unknown":      true,
		"unreadable":   true,
	}
	for _, f := range shippedFixtures(t) {
		if !states[f.doc.Policy.State] {
			t.Errorf("%s: policy.state = %q, want one of the documented situations configured, disabled, unconfigured, unknown, unreadable",
				f.name, f.doc.Policy.State)
		}
		if f.doc.Node == "" {
			t.Errorf("%s: node is empty, want the node the read describes", f.name)
		}
	}
}

// The engine sorts these rows by ID, and the fixtures document its output.
func TestShippedFixturesKeepTheEngineSortedOrders(t *testing.T) {
	sorted := func(what, file string, ids []string) {
		less := slices.IsSortedFunc(ids, strings.Compare)
		if !less {
			t.Errorf("%s: %s are not sorted ascending by ID: %v", file, what, ids)
		}
	}
	for _, f := range shippedFixtures(t) {
		localIDs := make([]string, len(f.doc.Local.Artifacts))
		for i, a := range f.doc.Local.Artifacts {
			localIDs[i] = a.ID
		}
		sorted("local.artifacts", f.name, localIDs)

		copyIDs := make([]string, len(f.doc.Remote.Copies))
		for i, a := range f.doc.Remote.Copies {
			copyIDs[i] = a.ID
		}
		sorted("remote.copies", f.name, copyIDs)

		entryIDs := make([]string, len(f.doc.Backups.Entries))
		for i, e := range f.doc.Backups.Entries {
			entryIDs[i] = e.ID
		}
		sorted("backups.entries", f.name, entryIDs)
	}
}

func TestShippedFixturesHoldTheCensusInvariants(t *testing.T) {
	for _, f := range shippedFixtures(t) {
		locals, remotes := byID(f.doc.Local.Artifacts), byID(f.doc.Remote.Copies)
		oldIDs := make(map[string]bool, len(f.doc.Remote.Old))
		for _, o := range f.doc.Remote.Old {
			oldIDs[o.ID] = true
			want, ok := remotes[o.ID]
			if !ok || want != o {
				t.Errorf("%s: remote.old holds %q, which is not one of remote.copies' entries: the old copies are drawn from the copies",
					f.name, o.ID)
			}
		}
		for _, a := range f.doc.Remote.Copies {
			if a.Digest != "unknown" {
				t.Errorf("%s: the remote copy %q carries digest %q; a remote listing exposes no digest", f.name, a.ID, a.Digest)
			}
		}

		seen := make(map[string]bool, len(f.doc.Backups.Entries))
		oldEntries := 0
		for _, e := range f.doc.Backups.Entries {
			if seen[e.ID] {
				t.Errorf("%s: backups.entries counts %q more than once; the census counts every stable ID once", f.name, e.ID)
			}
			seen[e.ID] = true
			if e.Local != nil {
				if a, ok := locals[e.ID]; !ok || a != *e.Local {
					t.Errorf("%s: the entry for %q carries a local half that is not local.artifacts' entry for that ID", f.name, e.ID)
				}
			}
			if e.Remote != nil {
				if a, ok := remotes[e.ID]; !ok || a != *e.Remote {
					t.Errorf("%s: the entry for %q carries a remote half that is not remote.copies' entry for that ID", f.name, e.ID)
				}
				if e.Remote.Digest != "unknown" {
					t.Errorf("%s: the entry for %q carries a remote digest; a remote listing exposes no digest", f.name, e.ID)
				}
			}
			if e.OldRemote != (e.Remote != nil && oldIDs[e.ID]) {
				t.Errorf("%s: the entry for %q has oldRemote = %v, want it true exactly when its remote copy predates the retention cutoff",
					f.name, e.ID, e.OldRemote)
			}
			if e.OldRemote {
				oldEntries++
			}
		}

		if len(f.doc.Backups.Retention) != oldEntries {
			t.Errorf("%s: backups.retention names %d old copies, want one retention line for each of the %d entries flagged oldRemote",
				f.name, len(f.doc.Backups.Retention), oldEntries)
		}
		for _, e := range f.doc.Backups.Entries {
			if !e.OldRemote {
				continue
			}
			named := false
			for _, line := range f.doc.Backups.Retention {
				if strings.Contains(line, e.ID) && strings.Contains(line, e.Remote.Created) {
					named = true
					break
				}
			}
			if !named {
				t.Errorf("%s: no retention line names the old remote copy %q with its creation time %q",
					f.name, e.ID, e.Remote.Created)
			}
		}
	}
}

func TestShippedFixturesComposeTheirCompletenessVerdicts(t *testing.T) {
	for _, f := range shippedFixtures(t) {
		if f.doc.Backups.Complete && !(f.doc.Local.Complete && f.doc.Remote.Complete) {
			t.Errorf("%s: backups.complete is true while local.complete = %v and remote.complete = %v; the census is exact only when both catalogs were complete",
				f.name, f.doc.Local.Complete, f.doc.Remote.Complete)
		}
		if f.doc.Complete && !(f.doc.Backups.Complete && f.doc.Maintenance.AttemptsKnown) {
			t.Errorf("%s: complete is true while backups.complete = %v and attemptsKnown = %v; the whole read is complete only when the census was exact and the maintenance record was read",
				f.name, f.doc.Backups.Complete, f.doc.Maintenance.AttemptsKnown)
		}
	}
}

func TestShippedFixturesComposeTheirWarnings(t *testing.T) {
	for _, f := range shippedFixtures(t) {
		want := slices.Concat(f.doc.Policy.Warnings, f.doc.Backups.Warnings, f.doc.Maintenance.Warnings)
		if !slices.Equal(f.doc.Warnings, want) {
			t.Errorf("%s: warnings = %q, want the policy's, census's and maintenance's warnings composed in that order: %q",
				f.name, f.doc.Warnings, want)
		}
	}
}

// Retention and the copy requirement resolve together, or neither from snapshot.conf.
func TestShippedFixturesHoldThePolicyProvenanceContract(t *testing.T) {
	for _, f := range shippedFixtures(t) {
		p := f.doc.Policy
		if p.IntervalSeconds == int64(version.SnapshotIntervalHours)*3600 && p.IntervalFrom != "default" {
			t.Errorf("%s: the built-in interval must carry default provenance", f.name)
		}
		if (p.RetainFrom == "snapshot-conf") != (p.CopyFrom == "snapshot-conf") {
			t.Errorf("%s: retainFrom = %q with copyFrom = %q, want both from snapshot-conf or neither: the provenances resolve together",
				f.name, p.RetainFrom, p.CopyFrom)
		}
	}
}

// An unreadable snapshot.conf leaves the remote catalog unknown, never a complete exact zero.
func TestShippedFixturesHoldTheUnreadableConfContract(t *testing.T) {
	for _, f := range shippedFixtures(t) {
		if f.doc.Policy.State == "unreadable" && f.doc.Remote.Complete {
			t.Errorf("%s: remote.complete is true under an unreadable snapshot config, want false: the target cannot be determined, so the remote catalog is unknown and not a complete total",
				f.name)
		}
	}
}

func TestShippedFixturesCarryImmutableIdentities(t *testing.T) {
	for _, f := range shippedFixtures(t) {
		if f.doc.NodeUID == "" {
			t.Errorf("%s: missing nodeUID", f.name)
		}
		for _, a := range append(f.doc.Local.Artifacts, f.doc.Remote.Copies...) {
			if a.LocationID == "" || a.CopyID == "" {
				t.Errorf("%s: artifact %s has no location/copy identity", f.name, a.ID)
			}
		}
	}
}

package cli

// The fixtures shipped under docs/docs/appendices/backup-evidence-fixtures/
// are complete documents in the shape `snapshot list -o json` prints, and a
// consumer or PRO implementer keys off them directly. These tests pin every
// shipped fixture to the same wire contract the CLI output tests pin
// (snapshotListJSON): each fixture decodes into that contract, keeps the
// orders the engine's output keeps, and holds the structural invariants the
// consumer page documents (docs/docs/cluster-admin/backup-evidence.md) — so a
// fixture that drifts from the binary fails the suite here instead of a
// consumer.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// shippedFixture is one decoded fixture, named by its file so a failure says
// which document drifted.
type shippedFixture struct {
	name string
	doc  snapshotListJSON
}

// shippedFixtures reads every *.json document in the fixtures appendix,
// sorted by file name, and decodes each into the pinned wire contract. A
// fixture added under the directory is validated the moment it lands; a
// fixture the contract cannot parse fails every test below at its name.
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

// byID indexes one artifact list by stable ID. A census counts every stable
// ID once, so a list that repeats an ID collides here for the caller to
// reject.
func byID(arts []snapshotListJSONArt) map[string]snapshotListJSONArt {
	m := make(map[string]snapshotListJSONArt, len(arts))
	for _, a := range arts {
		m[a.ID] = a
	}
	return m
}

// TestShippedFixturesReportADocumentedSituation: every fixture documents one
// of the five situations the consumer page names, from a node the document
// identifies, so a reworded state or a faceless document fails here instead
// of shipping a shape the page no longer describes.
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

// TestShippedFixturesKeepTheEngineSortedOrders: the engine's local walk,
// remote listing and census all sort their rows by ID
// (internal/backupevidence/local.go:303, remote.go:94, remote.go:204), and
// the page promises those orders — so a fixture that shows any other order
// documents output the binary can never print.
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

// TestShippedFixturesHoldTheCensusInvariants: the census counts every stable
// ID once, carries each half only where that location holds the backup, and
// the halves are the artifacts the catalogs hold — the same snapshot saved
// locally and uploaded off-box is one entry, two locations. A remote listing
// exposes no digest, and every old copy is one of the copies, named with its
// creation time in the retention diagnostics.
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

// TestShippedFixturesComposeTheirCompletenessVerdicts: a partial catalog on
// either side never yields an exact total, and the document's top-level
// verdict is true only when the census was exact and the maintenance record
// was read — the same rules the engine applies when it composes the document
// (internal/backupevidence/remote.go:210, reader.go:504).
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

// TestShippedFixturesComposeTheirWarnings: the document's top-level
// warnings are what the engine composes — the policy's, the census's and
// the maintenance record's, in that order — so a consumer alerting on the
// top level alone sees every stage's safe diagnostics. A fixture that
// drops a stage's warning from the top level teaches the opposite.
func TestShippedFixturesComposeTheirWarnings(t *testing.T) {
	for _, f := range shippedFixtures(t) {
		want := slices.Concat(f.doc.Policy.Warnings, f.doc.Backups.Warnings, f.doc.Maintenance.Warnings)
		if !slices.Equal(f.doc.Warnings, want) {
			t.Errorf("%s: warnings = %q, want the policy's, census's and maintenance's warnings composed in that order: %q",
				f.name, f.doc.Warnings, want)
		}
	}
}

// TestShippedFixturesHoldThePolicyProvenanceContract: retention and the
// copy requirement resolve together — both from snapshot.conf when it
// supplies the policy, both from the cluster config or the defaults
// otherwise, both unknown when neither could be read — so a fixture that
// pairs the provenances across that line documents a field combination the
// binary cannot print.
func TestShippedFixturesHoldThePolicyProvenanceContract(t *testing.T) {
	for _, f := range shippedFixtures(t) {
		p := f.doc.Policy
		if (p.RetainFrom == "snapshot-conf") != (p.CopyFrom == "snapshot-conf") {
			t.Errorf("%s: retainFrom = %q with copyFrom = %q, want both from snapshot-conf or neither: the provenances resolve together",
				f.name, p.RetainFrom, p.CopyFrom)
		}
	}
}

// TestShippedFixturesHoldTheUnreadableConfContract: an unreadable
// snapshot.conf leaves the remote target undetermined — the store may hold
// the copies it already uploaded, so the remote catalog is unknown, never a
// complete exact zero — and a fixture that marks it complete teaches the
// one reader mistake the totals rule exists to prevent.
func TestShippedFixturesHoldTheUnreadableConfContract(t *testing.T) {
	for _, f := range shippedFixtures(t) {
		if f.doc.Policy.State == "unreadable" && f.doc.Remote.Complete {
			t.Errorf("%s: remote.complete is true under an unreadable snapshot config, want false: the target cannot be determined, so the remote catalog is unknown and not a complete total",
				f.name)
		}
	}
}

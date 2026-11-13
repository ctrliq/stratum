package backupevidence

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/etcdsnap"
)

func TestEvidenceIdentifiesNodeLocationsAndCopies(t *testing.T) {
	withEvidencePaths(t)
	dir := t.TempDir()
	writeSnapshot(t, dir, "shared.db", 1, copyTime(0))
	recordedConfig(t, "spec:\n  snapshots:\n    dir: "+dir+"\n")
	tgt := stubRemote(t, []remoteObj{ro("shared.db", 1, copyTime(0))})
	tgt.SecretKey = "private-secret-marker"
	snapshotConfSupplies(t, etcdsnap.Config{Retain: 2, S3: *tgt})
	original := readerNodeUID
	t.Cleanup(func() { readerNodeUID = original })
	readerNodeUID = func(context.Context, string) (string, error) { return "node-uid-1", nil }
	read := func() EvidenceResult { return Evidence(context.Background(), EvidenceOptions{Node: "cp-1"}) }
	first := read()
	if first.NodeUID != "node-uid-1" {
		t.Fatalf("node UID = %q", first.NodeUID)
	}
	a, b := first.Local.Artifacts[0], first.Remote.Copies[0]
	if a.LocationID == "" || b.LocationID == "" || a.LocationID == b.LocationID || a.CopyID == "" || a.CopyID == b.CopyID {
		t.Fatalf("copies lack distinct stable identities: local %+v remote %+v", a, b)
	}
	if len(first.Backups.Entries) != 1 {
		t.Fatalf("copy identities changed backup deduplication: %+v", first.Backups)
	}
	if strings.Contains(b.LocationID, tgt.Endpoint) || strings.Contains(b.LocationID, tgt.SecretKey) {
		t.Fatal("location identity exposes target settings")
	}
	tgt.AccessKey, tgt.SecretKey = "rotated-key", "rotated-secret"
	snapshotConfSupplies(t, etcdsnap.Config{Retain: 2, S3: *tgt})
	second := read()
	if second.Remote.Copies[0].CopyID != b.CopyID || second.Local.Artifacts[0].CopyID != a.CopyID {
		t.Fatal("credential rotation changed copy identities")
	}
	tgt.Bucket = "different-bucket"
	snapshotConfSupplies(t, etcdsnap.Config{Retain: 2, S3: *tgt})
	third := read()
	if third.Remote.Copies[0].CopyID == b.CopyID {
		t.Fatal("changed target reused copy identity")
	}
	readerNodeUID = func(context.Context, string) (string, error) { return "replacement-node-uid", nil }
	fourth := read()
	if fourth.Local.Artifacts[0].CopyID == a.CopyID {
		t.Fatal("replacement node reused local copy identity")
	}
	readerNodeUID = func(context.Context, string) (string, error) { return "", errors.New("secret raw failure") }
	unknown := read()
	if unknown.Complete || unknown.NodeUID != "" || unknown.Local.Artifacts[0].CopyID != "" || len(unknown.Local.Artifacts) != 1 {
		t.Fatalf("unknown node identity lost evidence or invented identity: %+v", unknown)
	}
	if strings.Contains(strings.Join(unknown.Warnings, "\n"), "secret raw failure") {
		t.Fatal("node lookup exposed raw error")
	}
}

func TestNodeIdentityLookupSharesEvidenceDeadline(t *testing.T) {
	withEvidencePaths(t)
	recordedConfig(t, "spec: {}\n")
	noSnapshotConf(t)
	original := readerNodeUID
	t.Cleanup(func() { readerNodeUID = original })
	cancelled := false
	readerNodeUID = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		cancelled = true
		return "", ctx.Err()
	}
	result := Evidence(context.Background(), EvidenceOptions{Node: "cp-1", MaxTime: 10 * time.Millisecond})
	if !cancelled || result.Complete {
		t.Fatalf("node identity lookup ignored read deadline: %+v", result)
	}
}

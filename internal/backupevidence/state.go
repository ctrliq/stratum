// The durable attempt-state record the reader loads — the snapshot
// evidence file: what the scheduled snapshot loop's last save, upload and
// prune attempts did and when the save and upload last succeeded,
// persisted the way the renewal record persists (an atomically-written
// 0600 YAML under the node's config area) so it survives process
// restarts. Nothing else on the node says any of this: a failed tick
// reaches only the daemon's stderr, and the only evidence a backup ran is
// the artifact itself.
//
// The record's writer sits in internal/etcdsnap — the scheduled save
// records through it — while this package is its reader: the reader
// resolves its policy through the writer's package and so cannot be
// imported from there. The file is the contract between the two, and the
// recording tests (internal/etcdsnap) hold the writer's shape and this
// reader's together.
package backupevidence

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ctrliq/stratum/internal/version"
	"sigs.k8s.io/yaml"
)

// Attempt is one maintenance pass of the scheduled snapshot loop, the
// renewal-record precedent's pair: when the attempt started and how it went,
// the result empty when it succeeded.
type Attempt struct {
	// AttemptedAt is when the loop started the attempt.
	AttemptedAt time.Time `json:"attemptedAt"`
	// Result is the attempt's error, empty when it succeeded.
	Result string `json:"result,omitempty"`
}

// IsZero reports whether no attempt is recorded in this value. It is what the
// record's omitempty-style omission reads (encoding/json's omitzero), so an
// attempt slot the loop has never run is absent from the file instead of a
// zero-timestamped entry a reader must know to read as "never".
func (a Attempt) IsZero() bool {
	return a.AttemptedAt.IsZero() && a.Result == ""
}

// AttemptsSchemaVersion is the schema version this binary reads. It exists
// so a later revision of the record can change what a field means and an
// older binary can refuse the newer record instead of misreading it. The
// writer in internal/etcdsnap stamps the same version; the recording tests
// pin the two together.
const AttemptsSchemaVersion = 1

// Attempts is the scheduled snapshot loop's durable attempt state. Save and
// upload last-success are separate because a snapshot can save locally and
// fail its remote copy: each says on its own whether the artifact exists on
// the node and whether a remote copy does. Prune attempts are recorded
// independently of the save attempt because pruning is maintenance, not the
// backup itself: a save that succeeded while pruning failed is a working
// backup with broken retention.
type Attempts struct {
	// SchemaVersion is the record's schema version, stamped by every save.
	SchemaVersion int `json:"schemaVersion"`
	// Save and Upload are the latest save and upload attempts, each empty
	// until the loop first ran it.
	Save   Attempt `json:"save,omitzero"`
	Upload Attempt `json:"upload,omitzero"`
	// Prune is the latest prune attempt, independent of Save and Upload.
	Prune Attempt `json:"prune,omitzero"`
	// LastSaveSuccess and LastUploadSuccess are when the save and the upload
	// last succeeded — kept separately from the latest attempts so a streak
	// of failures does not erase when backups last worked.
	LastSaveSuccess   time.Time `json:"lastSaveSuccess,omitzero"`
	LastUploadSuccess time.Time `json:"lastUploadSuccess,omitzero"`
	// Warnings are the scheduling and retention warnings the loop's last tick
	// ran with — an unreadable snapshot.conf, a cluster config that cannot be
	// read — the same WARN lines the effective policy reports.
	Warnings []string `json:"warnings,omitempty"`
}

// EvidencePath is where the attempt-state record persists: beside node.yaml
// and renewal.yaml in the node's config area, the renewal-record precedent's
// location for a node-local record a start and the CLI both read.
func EvidencePath() string {
	return filepath.Join(version.K8sConfDir, "snapshot-evidence.yaml")
}

// LoadAttempts reads the persisted attempt state. A record that does not
// exist is an error wrapping fs.ErrNotExist, which callers read as no
// attempt recorded yet. A record whose schema version is newer than this
// binary's refuses to load: a newer revision may mean something else by a
// field, and misreading the evidence is worse than not having it.
func LoadAttempts() (*Attempts, error) {
	b, err := os.ReadFile(EvidencePath())
	if err != nil {
		return nil, err
	}
	var a Attempts
	if err := yaml.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", EvidencePath(), err)
	}
	if a.SchemaVersion > AttemptsSchemaVersion {
		return nil, fmt.Errorf("%s: schema version %d is newer than this binary's %d", EvidencePath(), a.SchemaVersion, AttemptsSchemaVersion)
	}
	return &a, nil
}

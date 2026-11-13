// The durable attempt-state record — the snapshot evidence file: what the
// scheduled snapshot loop's last save, upload and prune attempts did and when
// the save and upload last succeeded, persisted the way the renewal record
// persists (an atomically-written 0600 YAML under the node's config area) so
// it survives process restarts. Nothing else on the node says any of this: a
// failed tick reaches only the daemon's stderr, and the only evidence a
// backup ran is the artifact itself.
package backupevidence

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/etcddata"
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

// AttemptsSchemaVersion is the schema version this binary writes and reads.
// It exists so a later revision of the record can change what a field means
// and an older binary can refuse the newer record instead of misreading it.
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

// RecordSave records a save attempt: the attempt itself always, and the save
// last-success stamp only when the attempt succeeded, so a failed retry never
// moves it.
func (a *Attempts) RecordSave(at time.Time, err error) {
	a.Save = attempt(at, err)
	if err == nil {
		a.LastSaveSuccess = at
	}
}

// RecordUpload records an upload attempt the way RecordSave records a save:
// the last-success stamp moves only on success.
func (a *Attempts) RecordUpload(at time.Time, err error) {
	a.Upload = attempt(at, err)
	if err == nil {
		a.LastUploadSuccess = at
	}
}

// RecordPrune records a prune attempt. Pruning has no last-success stamp: it
// is maintenance beside the backup, and the attempt itself says whether
// retention last worked.
func (a *Attempts) RecordPrune(at time.Time, err error) {
	a.Prune = attempt(at, err)
}

// SetWarnings replaces the scheduling and retention warnings with the loop's
// current ones, so a warning from a condition that has healed does not linger.
func (a *Attempts) SetWarnings(warnings ...string) {
	a.Warnings = warnings
}

// attempt shapes one attempt from its outcome.
func attempt(at time.Time, err error) Attempt {
	rec := Attempt{AttemptedAt: at}
	if err != nil {
		rec.Result = err.Error()
	}
	return rec
}

// EvidencePath is where the attempt-state record persists: beside node.yaml
// and renewal.yaml in the node's config area, the renewal-record precedent's
// location for a node-local record a start and the CLI both read.
func EvidencePath() string {
	return filepath.Join(version.K8sConfDir, "snapshot-evidence.yaml")
}

// SaveAttempts persists the attempt state through the shared atomic writer at
// mode 0600: the record names failures, so a torn write must never leave a
// half-parsable one where a working one was, and a node-local record of what
// failed must not be world-readable. The save stamps the record's schema
// version, whatever the caller's copy carried.
func SaveAttempts(a *Attempts) error {
	a.SchemaVersion = AttemptsSchemaVersion
	b, err := yaml.Marshal(a)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(EvidencePath()), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(EvidencePath(), b, 0o600)
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

// ErrUnchanged is what a mutation returns when it changed nothing: the owner
// treats it as success and writes nothing, keeping what the file already
// holds. A tick that finds nothing to record must not rewrite the file with
// the bytes it just read, tick after tick, for as long as the node stays in
// that state.
var ErrUnchanged = errors.New("backupevidence: the mutation changed nothing")

// MutateAttempts is the one mutation owner for the snapshot evidence file,
// the discipline nodestate's owner holds for node.yaml: every writer records
// its change by load-modify-save on the one shared file, so a writer that
// applies its change to a copy it loaded earlier — or to a fresh struct —
// loses whatever another writer recorded in between. The owner re-reads the
// record from disk, runs mutate against what it read — keeping what the
// writer did not set — and writes it back with SaveAttempts' durable write.
// A mutate that returns an error writes nothing; one that returns ErrUnchanged
// changed nothing and writes nothing, as a success. A missing record is a
// fresh empty one: the loop's first tick has nothing to read.
//
// The read-modify-write runs under the etcd data lock (etcddata.Lock): while
// a rejoin, a restore or an `up` holds that lock, the recording refuses
// instead of writing between that operation's read and write; the loop
// retries its recording on the next tick. A failure to acquire the lock
// refuses the mutation: an unavailable lock provides no exclusion against
// other writers.
func MutateAttempts(mutate func(*Attempts) error) error {
	return mutateAttempts(mutate, SaveAttempts)
}

// MutateAttemptsSave is MutateAttempts with the caller's writer for the final
// persist, for a caller whose save sits behind a seam. The lock contract is
// the owner's.
func MutateAttemptsSave(mutate func(*Attempts) error, save func(*Attempts) error) error {
	return mutateAttempts(mutate, save)
}

// mutateAttempts holds the etcd data lock across the read, mutation and save.
func mutateAttempts(mutate func(*Attempts) error, save func(*Attempts) error) error {
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		return err
	}
	defer release()

	a, err := attemptsForMutation()
	if err != nil {
		return err
	}
	if err := mutate(a); err != nil {
		if errors.Is(err, ErrUnchanged) {
			return nil
		}
		return err
	}
	return save(a)
}

// attemptsForMutation reads the record a mutation applies to: what is on
// disk, or a fresh empty record on a node whose loop has not run yet. A
// record that exists but cannot be read is an error, never a silently fresh
// one.
func attemptsForMutation() (*Attempts, error) {
	a, err := LoadAttempts()
	if err == nil {
		return a, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return &Attempts{}, nil
	}
	return nil, err
}

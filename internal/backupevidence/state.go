package backupevidence

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ctrliq/stratum/internal/boundedfile"
	"github.com/ctrliq/stratum/internal/version"
	"sigs.k8s.io/yaml"
)

type Attempt struct {
	AttemptedAt time.Time `json:"attemptedAt"`
	Result      string    `json:"result,omitempty"`
}

// encoding/json's omitzero reads this, so never-run attempt slots stay out of the file.
func (a Attempt) IsZero() bool {
	return a.AttemptedAt.IsZero() && a.Result == ""
}

// A newer record may mean something else by a field and is refused, not misread.
const AttemptsSchemaVersion = 1

type Attempts struct {
	SchemaVersion int     `json:"schemaVersion"`
	Save          Attempt `json:"save,omitzero"`
	Upload        Attempt `json:"upload,omitzero"`
	// Recorded separately from Save: a saved backup with failed pruning still works.
	Prune Attempt `json:"prune,omitzero"`
	// Separate from the latest attempts so a failure streak does not erase them.
	LastSaveSuccess   time.Time `json:"lastSaveSuccess,omitzero"`
	LastUploadSuccess time.Time `json:"lastUploadSuccess,omitzero"`
	Warnings          []string  `json:"warnings,omitempty"`
}

func EvidencePath() string {
	return filepath.Join(version.K8sConfDir, "snapshot-evidence.yaml")
}

// A missing record wraps fs.ErrNotExist; a newer schema version is refused, not misread.
func LoadAttempts() (*Attempts, error) {
	return loadAttempts(context.Background())
}

func loadAttempts(ctx context.Context) (*Attempts, error) {
	b, err := boundedfile.Read(ctx, EvidencePath())
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

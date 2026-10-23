package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/operator"
	"github.com/ctrliq/stratum/internal/version"
)

const (
	stagedFingerprintName = ".staged-fingerprint"
	pendingStagingName    = ".pending-staging.json"
)

type pendingStaging struct {
	Generation operator.StagedGeneration `json:"generation"`
	Previous   string                    `json:"previous"`
	Refused    string                    `json:"refused,omitempty"`
}

var operatorRecordStaged = func(ctx context.Context, staged operator.StagedGeneration, previous string) (operator.StagingPublication, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return operator.StagingPublication{}, err
	}
	return operator.RecordStagedGeneration(ctx, cs, staged, previous)
}

// stageDesiredBundles saves the pending activation before contacting the API.
// Only a successful publish advances the acknowledged publication, so a cold first
// boot can retry after the apiserver starts or after this process restarts.
func stageDesiredBundles(cfg *ClusterConfig, st *NodeState, bundles map[string][]byte) error {
	inputs, err := clusterInputs(cfg, st)
	if err != nil {
		return err
	}
	previous, err := os.ReadFile(filepath.Join(version.ManifestsDir, stagedFingerprintName))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read the last staged fingerprint: %w", err)
	}
	stamp := strings.SplitN(strings.TrimSpace(string(previous)), "\n", 2)
	acknowledged := ""
	if len(stamp) == 2 {
		acknowledged = stamp[1]
	}
	pending := pendingStaging{
		Generation: operator.StagedGeneration{
			Digest:     operator.DigestBundles(bundles),
			Provenance: fmt.Sprintf("stratum up (%s) on %s", version.StratumVersion, st.Hostname),
			Stacks:     renderedStacks(bundles), Inputs: inputs,
		},
		Previous: stamp[0],
	}
	ctx, cancel := context.WithTimeout(context.Background(), desiredRecordBudget)
	defer cancel()
	// A prior API commit can precede its local success stamp. Preserve those
	// possible predecessors until acknowledged; the API accepts one only if
	// it is the currently active publication ID. An acknowledged request clears
	// its history so ordinary stale restarts gain no additional authority.
	priorBytes, err := os.ReadFile(filepath.Join(version.ManifestsDir, pendingStagingName))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		var prior pendingStaging
		if err := json.Unmarshal(priorBytes, &prior); err != nil {
			return err
		}
		if prior.Generation.PublicationID != "" && prior.Generation.Fingerprint() == pending.Generation.Fingerprint() {
			// An unchanged restart retries the same request, including any
			// terminal refusal, rather than creating a new activation attempt.
			return publishPendingStaging(ctx)
		}
		matchedAck := prior.Generation.PublicationID != "" && prior.Generation.PublicationID == acknowledged
		if !matchedAck && prior.Refused == "" && prior.Generation.PublicationID != "" {
			candidates := append(slices.Clone(prior.Generation.Unacknowledged), prior.Generation.PublicationID)
			slices.Sort(candidates)
			pending.Generation.Unacknowledged = slices.Compact(candidates)
		}
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	pending.Generation.PublicationID = hex.EncodeToString(id[:])
	b, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	if err := writeStagingRecord(pendingStagingName, b); err != nil {
		return err
	}
	return publishPendingStaging(ctx)
}

func publishPendingStaging(ctx context.Context) error {
	b, err := os.ReadFile(filepath.Join(version.ManifestsDir, pendingStagingName))
	if err != nil {
		return err
	}
	var pending pendingStaging
	if err := json.Unmarshal(b, &pending); err != nil {
		return err
	}
	if pending.Refused != "" {
		return fmt.Errorf("%w: %s", operator.ErrStagingRefused, pending.Refused)
	}
	publication, err := operatorRecordStaged(ctx, pending.Generation, pending.Previous)
	if err != nil {
		if errors.Is(err, operator.ErrStagingRefused) {
			pending.Refused = err.Error()
			data, encodeErr := json.Marshal(pending)
			if encodeErr != nil {
				return errors.Join(err, encodeErr)
			}
			if writeErr := writeStagingRecord(pendingStagingName, data); writeErr != nil {
				return errors.Join(err, writeErr)
			}
		}
		return err
	}
	return writeStagingRecord(stagedFingerprintName, []byte(publication.ID+"\n"+pending.Generation.PublicationID+"\n"))
}

func writeStagingRecord(name string, data []byte) error {
	return atomicfile.WriteFile(filepath.Join(version.ManifestsDir, name), data, 0o600)
}

package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"k8s.io/client-go/kubernetes"
)

// StagedGeneration carries one local render and its cluster-wide inputs.
type StagedGeneration struct {
	// PublicationID is fresh for each changed local staging request and stable
	// across retries of that request. Content fingerprints cannot identify epochs.
	PublicationID string        `json:"publicationID"`
	Digest        string        `json:"digest"`
	Provenance    string        `json:"provenance"`
	Stacks        []string      `json:"stacks"`
	Inputs        ClusterInputs `json:"inputs"`
	// Unacknowledged names earlier locally persisted publications whose API
	// commit may have succeeded before the local success stamp was written.
	// Entries are publication IDs, never content fingerprints or known refusals.
	Unacknowledged []string `json:"unacknowledged,omitempty"`
}

// StagingPublication acknowledges the exact cluster publication a node joined.
type StagingPublication struct {
	Generation int
	ID         string
}

// ErrStagingRefused distinguishes a terminal activation decision from an API
// failure whose commit outcome may be unknown to the caller.
var ErrStagingRefused = errors.New("staged generation activation refused")

// Fingerprint includes the inputs because changing a sentinel's value does
// not change the staged bundle bytes. Provenance identifies the producer and
// takes no part in determining whether its intent changed.
func (s StagedGeneration) Fingerprint() string {
	b, _ := json.Marshal(s.Inputs)
	return DigestBundles(map[string][]byte{"bundles": []byte(s.Digest), "inputs": b})
}

// RecordStagedGeneration publishes bundles and inputs in one inventory update.
// previous is the node's last acknowledged cluster publication ID: an unchanged
// service restart cannot replace another node's newer intent. A changed local
// render can activate an upgrade or rollback; a first-time participant joins
// existing intent without replacing it.
func RecordStagedGeneration(ctx context.Context, cs kubernetes.Interface, staged StagedGeneration, previous string) (StagingPublication, error) {
	if staged.PublicationID == "" {
		return StagingPublication{}, fmt.Errorf("%w: missing publication ID", ErrStagingRefused)
	}
	var publication StagingPublication
	var refused error
	err := writeInventory(ctx, cs, func(inv pruneInventory) pruneInventory {
		refused = nil // conflict retries assess the fresh record
		if desired := inv.Desired; desired != nil {
			matches := desired.Digest == staged.Digest && (desired.Inputs == nil || *desired.Inputs == staged.Inputs)
			current := desired.PublicationID
			advances := current != "" && (previous == current || slices.Contains(staged.Unacknowledged, current))
			if current != "" && !advances {
				for _, joined := range desired.JoinedPublications {
					if slices.Contains(staged.Unacknowledged, joined) {
						advances = true
						break
					}
				}
			}
			if !matches && !advances {
				refused = fmt.Errorf("%w: local staging does not advance the active desired generation %d staged by %q", ErrStagingRefused, desired.Generation, desired.Provenance)
				return inv
			}
			if matches && desired.Inputs != nil {
				// Adopt a pre-publication-ID record only by joining its exact
				// content. A fingerprint alone never grants replacement authority.
				if desired.PublicationID == "" {
					desired.PublicationID = staged.PublicationID
				}
				// Persist joins with the publication they actually observed: a
				// lost receipt can recover without granting authority to a later
				// activation of the same content.
				if staged.PublicationID != desired.PublicationID && !slices.Contains(desired.JoinedPublications, staged.PublicationID) {
					desired.JoinedPublications = append(desired.JoinedPublications, staged.PublicationID)
				}
				publication = StagingPublication{Generation: desired.Generation, ID: desired.PublicationID}
				return inv
			}
		}
		next := 1
		managed := slices.Clone(staged.Stacks)
		if inv.Desired != nil {
			next = inv.Desired.Generation + 1
			managed = append(managed, inv.Desired.Stacks...)
		}
		inputs := staged.Inputs
		inv.Desired = &desiredGeneration{
			Generation: next, Digest: staged.Digest, Provenance: staged.Provenance,
			PublicationID: staged.PublicationID,
			Stacks:        sortedUniqueStrings(managed), Inputs: &inputs,
		}
		publication = StagingPublication{Generation: next, ID: staged.PublicationID}
		return inv
	})
	if err != nil {
		return StagingPublication{}, err
	}
	return publication, refused
}

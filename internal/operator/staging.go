package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"k8s.io/client-go/kubernetes"
)

type StagedGeneration struct {
	// Fresh per changed request, stable across its retries; fingerprints cannot identify epochs.
	PublicationID string        `json:"publicationID"`
	Digest        string        `json:"digest"`
	Provenance    string        `json:"provenance"`
	Stacks        []string      `json:"stacks"`
	Inputs        ClusterInputs `json:"inputs"`
	// Publications whose API commit may have succeeded before their local success stamp.
	Unacknowledged []string `json:"unacknowledged,omitempty"`
}

type StagingPublication struct {
	Generation int
	ID         string
}

// A terminal activation decision, not an API failure of unknown commit outcome.
var ErrStagingRefused = errors.New("staged generation activation refused")

// Includes the inputs: changing a sentinel's value does not change the bundle bytes.
func (s StagedGeneration) Fingerprint() string {
	b, _ := json.Marshal(s.Inputs)
	return DigestBundles(map[string][]byte{"bundles": []byte(s.Digest), "inputs": b})
}

// previous is the node's last acknowledged cluster publication ID.
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
				// A fingerprint alone never grants replacement authority.
				if desired.PublicationID == "" {
					desired.PublicationID = staged.PublicationID
				}
				// A lost receipt can recover without granting a later activation authority.
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

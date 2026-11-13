package backupevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/objstore"
)

var readerNodeUID = k8sutil.OwnNodeUID

const warningNodeUIDUnknown = "WARN: this node's immutable UID could not be determined; local location and copy identities are unknown"

// identity hashes a domain and an unambiguous sequence of effective settings.
// Credentials never participate, so rotating them cannot change a copy's ID.
func identity(parts ...string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func remoteLocationID(t *objstore.Target) string {
	if !t.Enabled() {
		return ""
	}
	region := t.Region
	if region == "" {
		region = "us-east-1"
	}
	return identity("backup-remote-location-v1", strings.TrimSuffix(t.Endpoint, "/"), region, t.Bucket, strings.TrimSuffix(t.Prefix, "/"))
}

func identifyArtifacts(artifacts []Artifact, location string) {
	if location == "" {
		return
	}
	for i := range artifacts {
		artifacts[i].LocationID = location
		artifacts[i].CopyID = identity("backup-copy-v1", location, artifacts[i].ID)
	}
}

// identifyLocations runs after the catalogs, sharing the request deadline so
// an unavailable API cannot prevent useful local evidence from being read.
func identifyLocations(ctx context.Context, node, dir string, target *objstore.Target, local *LocalResult, remote *RemoteResult) string {
	var uid string
	if ctx.Err() == nil && node != "" {
		uid, _ = readerNodeUID(ctx, node)
	}
	if uid == "" {
		local.Warnings = append(local.Warnings, warningNodeUIDUnknown)
	} else if dir != "" {
		if absolute, err := filepath.Abs(dir); err == nil {
			identifyArtifacts(local.Artifacts, identity("backup-local-location-v1", uid, absolute))
		}
	}
	location := remoteLocationID(target)
	identifyArtifacts(remote.Copies, location)
	identifyArtifacts(remote.Old, location)
	return uid
}

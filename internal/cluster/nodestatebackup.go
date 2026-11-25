package cluster

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/nodebackup"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// NodeStateBackup is spec.snapshots.nodeState: the node-state bundle a
// control plane takes beside each etcd snapshot. It is read from the cluster
// config alone and never from snapshot.conf, which replaces spec.snapshots
// whole when it exists: a recipient kept there would be dropped, and the
// bundle with it, the first time `stratum snapshot schedule` wrote the file.
type NodeStateBackup struct {
	// Recipients are the public keys a bundle is sealed to, as `stratum
	// snapshot keygen` prints them. With none, no bundle is taken.
	Recipients []string `json:"recipients,omitempty"`
}

func (c *ClusterConfig) nodeStateRecipients() []string {
	if c == nil || c.Spec.Snapshot.NodeState == nil {
		return nil
	}
	return c.Spec.Snapshot.NodeState.Recipients
}

// validateNodeStateBackup refuses a recipient that does not parse when the
// config is loaded: one found only when a snapshot is taken would leave the
// cluster without a bundle and nobody told.
func (c *ClusterConfig) validateNodeStateBackup(path string) error {
	if _, err := nodebackup.ParseRecipients(c.nodeStateRecipients()); err != nil {
		return fmt.Errorf("%s: spec.snapshots.nodeState.recipients: %w", path, err)
	}
	return nil
}

// NodeStateSources is where this node keeps what a node-state bundle holds.
// configPath is the cluster config the node runs with; it is named apart only
// when it sits outside the configuration directory, which the bundle takes
// whole. The staged image archives are left out: they are not node state.
func NodeStateSources(configPath string) nodebackup.Sources {
	src := nodebackup.Sources{
		PKIDir:       version.PKIDir,
		StatePath:    nodestate.StatePath(),
		ConfDir:      version.ConfDir,
		ManifestsDir: version.ManifestsDir,
		Skip:         []string{version.ImagesDropInDir},
	}
	if configPath != "" && filepath.IsAbs(configPath) {
		src.ConfigPath = filepath.Clean(configPath)
	}
	if configPath != "" && filepath.IsAbs(configPath) && !strings.HasPrefix(filepath.Clean(configPath)+"/", filepath.Clean(version.ConfDir)+"/") {
		src.ExtraConfig = filepath.Clean(configPath)
	}
	return src
}

// nodeStateFor is the bundle a save on this node takes for c, nil when c
// names no recipient. A var so a test can supply one without a node.
var nodeStateFor = func(c *ClusterConfig, warn io.Writer) *etcdsnap.NodeState {
	recipients, err := nodebackup.ParseRecipients(c.nodeStateRecipients())
	if err != nil || len(recipients) == 0 {
		return nil
	}
	path, _ := activeConfigPath(LoadState, warn)
	ip, _ := nodestate.NodeIP()
	return &etcdsnap.NodeState{Recipients: recipients, Sources: NodeStateSources(path), NodeIP: ip}
}

// LoadNodeStateBackup is the bundle a save on this node takes, for the CLI:
// resolved from the cluster config the controller was started with, as the
// scheduled loop resolves it. Nil when that config names no recipient.
func LoadNodeStateBackup() (*etcdsnap.NodeState, error) {
	path, _ := activeConfigPath(LoadState, io.Discard)
	cc, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return nodeStateFor(cc, io.Discard), nil
}

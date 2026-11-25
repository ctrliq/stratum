package cluster

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodebackup"
	"github.com/ctrliq/stratum/internal/version"
)

func testRecipient(t *testing.T) string {
	t.Helper()
	id, err := nodebackup.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}

// A recipient that does not parse is refused when the config is loaded: found
// only when a snapshot is taken, it would leave the cluster with no bundle
// and nobody told.
func TestLoadConfigChecksTheNodeStateRecipients(t *testing.T) {
	good := testRecipient(t)
	for name, tc := range map[string]struct {
		recipients string
		wantErr    string
	}{
		"a recipient":          {recipients: "[" + good + "]"},
		"none":                 {recipients: "[]"},
		"a mistyped recipient": {recipients: "[" + good[:len(good)-3] + "aaa]", wantErr: "spec.snapshots.nodeState.recipients"},
		"something else":       {recipients: "[not-a-key]", wantErr: "spec.snapshots.nodeState.recipients"},
		"one recipient twice":  {recipients: "[" + good + ", " + good + "]", wantErr: "listed twice"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("spec:\n  snapshots:\n    nodeState:\n      recipients: "+tc.recipients+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadConfig(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadConfig = %v, want an error naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if got, want := len(c.nodeStateRecipients()), strings.Count(tc.recipients, "stratum-backup-recipient-"); got != want {
				t.Errorf("config carries %d recipients, want %d", got, want)
			}
		})
	}
}

// A config with no recipient takes no bundle, and one with a recipient
// bundles the node's own paths, without the staged image archives.
func TestNodeStateForFollowsTheRecipients(t *testing.T) {
	oldLoad := LoadState
	t.Cleanup(func() { LoadState = oldLoad })
	LoadState = func() (*NodeState, error) { return &NodeState{NodeIP: "10.0.0.1"}, nil }
	if ns := nodeStateFor(&ClusterConfig{}, io.Discard); ns != nil {
		t.Errorf("a config with no recipient takes a bundle: %+v", ns)
	}
	c := &ClusterConfig{}
	c.Spec.Snapshot.NodeState = &NodeStateBackup{Recipients: []string{testRecipient(t)}}
	ns := nodeStateFor(c, io.Discard)
	if ns == nil || len(ns.Recipients) != 1 {
		t.Fatalf("nodeStateFor = %+v, want one recipient", ns)
	}
	if ns.Sources.PKIDir != version.PKIDir || ns.Sources.ConfDir != version.ConfDir || ns.Sources.ExtraConfig != "" {
		t.Errorf("sources = %+v, want the node's PKI and configuration directories", ns.Sources)
	}
	if len(ns.Sources.Skip) != 1 || ns.Sources.Skip[0] != version.ImagesDropInDir {
		t.Errorf("sources skip %v, want the staged image archives", ns.Sources.Skip)
	}
}

// The cluster config is named apart only when it sits outside the
// configuration directory, which the bundle takes whole.
func TestNodeStateSourcesNamesAConfigKeptElsewhere(t *testing.T) {
	for path, want := range map[string]string{
		"": "",
		filepath.Join(version.ConfDir, "config.yaml"):   "",
		filepath.Join(version.ConfDir, "sub", "c.yaml"): "",
		"/srv/site/cluster.yaml":                        "/srv/site/cluster.yaml",
		version.ConfDir + "-other/config.yaml":          version.ConfDir + "-other/config.yaml",
		"relative.yaml":                                 "",
	} {
		if got := NodeStateSources(path).ExtraConfig; got != want {
			t.Errorf("NodeStateSources(%q).ExtraConfig = %q, want %q", path, got, want)
		}
	}
}

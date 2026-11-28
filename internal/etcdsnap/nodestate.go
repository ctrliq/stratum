package etcdsnap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/nodebackup"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

// NodeStateDir is where the node-state bundles sit: a directory of their own
// under the snapshot directory, and the same name as a prefix of their own
// under the S3 prefix. Everything that lists, counts or prunes snapshots
// reads one level only, so no bundle is ever taken for a snapshot.
const NodeStateDir = "node-state"

const (
	nodeStatePrefix = "node-state-"
	nodeStateSuffix = ".bundle"
)

// NodeState is the node-state bundle a save takes beside its snapshot.
type NodeState struct {
	// Recipients are who the bundle is sealed to. A save with none takes no
	// bundle: there is no unencrypted form.
	Recipients []nodebackup.Recipient
	// Sources is where the node keeps its state.
	Sources nodebackup.Sources
	// NodeIP is recorded in the bundle, for the person who restores it.
	NodeIP string
}

// NodeStateName is the bundle taken with the snapshot of host at stamp.
func NodeStateName(host, stamp string) string {
	return nodeStatePrefix + host + "-" + stamp + nodeStateSuffix
}

// nodeStateNameFor is the bundle taken with snapshot. A scheduled snapshot's
// bundle carries its host and stamp, and retention owns both alike. A
// snapshot saved under a name of its own is never pruned, so neither is its
// bundle: it is named for its host and the snapshot, with a double dash
// retention does not match. The host is in the name because a bundle is one
// node's: the same snapshot name saved on each control plane is one bundle
// per node under one S3 prefix, not one that the last upload wins.
func nodeStateNameFor(host, stamp, snapshot string, named bool) string {
	if !named {
		return NodeStateName(host, stamp)
	}
	return nodeStatePrefix + host + "--" + strings.TrimSuffix(snapshot, ".db") + nodeStateSuffix
}

// hostNodeState matches host's bundle names the way hostSnapshot matches its
// snapshots: exactly one stamp after the host, so "cp" never owns "cp-2"'s.
func hostNodeState(host string) func(name string) bool {
	hp := nodeStatePrefix + host + "-"
	return func(name string) bool {
		ts, ok := cutHostPrefix(name, hp)
		if !ok {
			return false
		}
		ts, ok = strings.CutSuffix(ts, nodeStateSuffix)
		if !ok || len(ts) != len(stampLayout) {
			return false
		}
		_, err := time.Parse(stampLayout, ts)
		return err == nil
	}
}

// NodeStateTarget is where the bundles of the snapshots at t go: the same
// bucket, one prefix down. Nil when t is no target.
func NodeStateTarget(t *objstore.Target) *objstore.Target {
	if !t.Enabled() {
		return nil
	}
	sub := *t
	sub.Prefix = sub.Key(NodeStateDir)
	return &sub
}

// nodeStateLockWait is how long a bundle waits for the node maintenance lock
// before it gives up, and how often it asks. Vars so a test can shorten them.
var (
	nodeStateLockWait  = 30 * time.Second
	nodeStateLockEvery = 2 * time.Second
)

// nodeStateLock takes the node maintenance lock for a bundle's collection,
// waiting a short while for an operation that holds it: a rotation is quick,
// and a bundle given up at once is not taken again until the next snapshot.
// One still held after the wait is an error, and the bundle is not taken.
var nodeStateLock = func(ctx context.Context) (func(), error) {
	deadline := time.Now().Add(nodeStateLockWait)
	for {
		release, err := nodelock.Acquire("node-state backup")
		if err == nil {
			return release, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(nodeStateLockEvery):
		}
	}
}

// nodeStateResult is what saveNodeState did, each step's error apart: the
// local bundle and its off-box copy are evidence of different things, as a
// snapshot's are.
type nodeStateResult struct {
	path     string
	save     error
	uploaded bool
	upload   error
	prune    []error
}

// saveNodeState seals the node's state into a bundle beside the snapshot
// named snapshot, uploads it when remote is a target, and prunes this host's
// older bundles to retain. The plaintext archive exists only in this
// process's memory.
func saveNodeState(ctx context.Context, w io.Writer, dir, host, name, snapshot string, ns *NodeState,
	remote *objstore.Target, retain int) nodeStateResult {
	var res nodeStateResult
	sub := filepath.Join(dir, NodeStateDir)
	// Collected under the node maintenance lock: a certificate or CA rotation
	// replaces a certificate and its key as two files, and a bundle read
	// between the two holds a pair that does not match.
	release, err := nodeStateLock(ctx)
	if err != nil {
		res.save = err
		return res
	}
	plain, m, err := nodebackup.Collect(ns.Sources, nodebackup.Meta{
		Hostname:          host,
		NodeIP:            ns.NodeIP,
		StratumVersion:    version.StratumVersion,
		KubernetesVersion: version.K8sVersion,
		CreatedAt:         time.Now().UTC().Truncate(time.Second),
		Snapshot:          snapshot,
	})
	release()
	if err != nil {
		res.save = err
		return res
	}
	sealed, err := nodebackup.Seal(ns.Recipients, plain)
	if err != nil {
		res.save = err
		return res
	}
	if err := os.MkdirAll(sub, 0o700); err != nil {
		res.save = err
		return res
	}
	path := filepath.Join(sub, name)
	if err := atomicfile.WriteFile(path, sealed, 0o600); err != nil {
		res.save = err
		return res
	}
	res.path = path
	fmt.Fprintf(w, "node state saved: %s (sealed to %d recipient(s), cluster CA SHA-256 %s)\n", path, len(ns.Recipients), m.ClusterCASHA256)
	target := NodeStateTarget(remote)
	if target != nil {
		res.uploaded = true
		if res.upload = target.Upload(ctx, path, name); res.upload != nil {
			target = nil
		}
	}
	if retain > 0 && ctx.Err() == nil {
		owned := hostNodeState(host)
		if err := Prune(sub, owned, retain); err != nil {
			res.prune = append(res.prune, fmt.Errorf("local node-state prune: %w", err))
		}
		if target != nil {
			if err := target.Prune(ctx, owned, retain); err != nil {
				res.prune = append(res.prune, fmt.Errorf("S3 node-state prune: %w", err))
			}
		}
	}
	return res
}

// err is the one error a save returns for the bundle: what failed, or nil.
// A failed prune is a warning the save prints, as a snapshot's is.
func (r nodeStateResult) err() error {
	switch {
	case r.save != nil:
		return fmt.Errorf("the node-state bundle was not saved, so this snapshot cannot rebuild a control plane on its own: %w", r.save)
	case r.upload != nil:
		return fmt.Errorf("node state saved locally (%s) but S3 upload failed: %w", r.path, r.upload)
	}
	return nil
}

// ErrNodeStateIncomplete marks a save whose snapshot was taken and whose
// node-state bundle was not, or was not copied off the node.
var ErrNodeStateIncomplete = errors.New("node-state bundle incomplete")

// ListNodeState lists the node-state bundles beside the snapshots in dir and,
// when s3 is a target, under it. Like List, a failed S3 listing returns the
// local ones with the error.
func ListNodeState(dir string, s3 *objstore.Target) ([]SnapItem, error) {
	var items []SnapItem
	entries, err := os.ReadDir(filepath.Join(dir, NodeStateDir))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), nodeStateSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, SnapItem{Name: NodeStateDir + "/" + e.Name(), Location: "local", Size: info.Size(), Modified: info.ModTime()})
	}
	if target := NodeStateTarget(s3); target != nil {
		objs, err := target.List(context.Background())
		if err != nil {
			return items, fmt.Errorf("list S3 node-state bundles (local shown): %w", err)
		}
		for _, o := range objs {
			items = append(items, SnapItem{Name: NodeStateDir + "/" + o.Name, Location: "s3", Size: o.Size, Modified: o.Modified})
		}
	}
	return items, nil
}

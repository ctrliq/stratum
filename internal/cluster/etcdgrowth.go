package cluster

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// Bounds how stale a node.yaml backup can be about HA growth.
var etcdGrowthPoll = time.Minute

// A var so a test can answer for an etcd it does not run.
var localEtcdMemberCount = func() (int, error) { return ha.MemberCount([]string{"127.0.0.1"}) }

// A replacement from the seed's backup would bootstrap an empty etcd beside surviving members.
func withPeers(st *NodeState, members int) *NodeState {
	if st.Role != "controller" || st.EtcdState != "new" || st.EtcdHasPeers || members < 2 {
		return nil
	}
	grown := *st
	grown.EtcdHasPeers = true
	return &grown
}

func recordEtcdGrowth(ctx context.Context) {
	t := time.NewTicker(etcdGrowthPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if st, err := LoadState(); err == nil && (st.EtcdState != "new" || st.EtcdHasPeers) {
			return
		}
		members, err := localEtcdMemberCount()
		if err != nil {
			continue
		}
		if done := saveGrowth(members); done {
			return
		}
	}
}

// Re-reads state under the caller's data lock so a concurrent rejoin's record survives.
func saveGrowth(members int) (done bool) {
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		return false
	}
	defer release()
	err = nodestate.MutateHeld(release, func(st *NodeState) error {
		if st.EtcdState != "new" || st.EtcdHasPeers {
			done = true
			return nodestate.ErrUnchanged
		}
		grown := withPeers(st, members)
		if grown == nil {
			// A no-record pass must not rewrite node.yaml with the bytes it just read.
			return nodestate.ErrUnchanged
		}
		*st = *grown
		done = true
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stratum: record etcd membership: %v\n", err)
		return false
	}
	return done
}

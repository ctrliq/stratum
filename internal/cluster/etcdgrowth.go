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

// etcdGrowthPoll is how often a seed still recording state=new without peers looks at its own
// etcd. It bounds how stale a node.yaml backup can be about HA growth.
var etcdGrowthPoll = time.Minute

// localEtcdMemberCount is ha.MemberCount against this node's etcd; a var so a
// test can answer for an etcd it does not run.
var localEtcdMemberCount = func() (int, error) { return ha.MemberCount([]string{"127.0.0.1"}) }

// withPeers returns a copy of st recording that its etcd cluster has more than
// one member, or nil when there is nothing to record.
//
// HA joins save membership on the joining node only, so the original seed keeps
// the single-member state=new it bootstrapped with. A replacement started from a
// backup of that file is indistinguishable from a sole-controller replacement
// and would bootstrap an empty etcd beside survivors that still hold quorum.
// Recording EtcdHasPeers makes `stratum up` demand the member's WAL
// (pkg/cli recordsLargerEtcdCluster). It is a separate field because etcd's
// argv is rendered from EtcdInitialCluster and EtcdState: rewriting those
// changed the manifest, so the next controller start, such as the one a cluster
// upgrade does, made kubelet recreate etcd (seen in the ha-cluster-upgrade integration test).
func withPeers(st *NodeState, members int) *NodeState {
	if st.Role != "controller" || st.EtcdState != "new" || st.EtcdHasPeers || members < 2 {
		return nil
	}
	grown := *st
	grown.EtcdHasPeers = true
	return &grown
}

// recordEtcdGrowth polls until this seed's etcd has grown past one member, then
// persists that in the node state.
//
// The answer from etcd is only an observation; the write is decided under the
// etcd data lock that rejoin, restore and `stratum up` hold while they change
// node.yaml, against state read after taking it. Without that, a rejoin of the
// seed that saved its new membership while this waited on etcd was overwritten
// by the older peer list. A held lock means one of those is
// running, so this pass writes nothing and the next one looks again.
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
			return // already recorded (a join, a rejoin, or an earlier pass)
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

// saveGrowth records the observed member count under the data lock and reports
// whether the poll is finished: it is once the state says existing or records
// peers, whoever wrote it. The write is the mutation owner's under-lock
// variant: the recorder holds the data lock it took here and hands the owner
// its capability, and the owner re-reads the state under that lock — against
// state read after taking it, so a rejoin of the seed that saved its new
// membership while this poll waited on etcd is carried, not overwritten — and
// applies the growth to what it read.
func saveGrowth(members int) (done bool) {
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		return false
	}
	defer release()
	err = nodestate.MutateHeld(release, func(st *NodeState) error {
		if st.EtcdState != "new" || st.EtcdHasPeers {
			done = true
			// Already recorded — the state on disk carries the record a
			// join or an earlier pass wrote, so this pass saves nothing.
			return nodestate.ErrUnchanged
		}
		grown := withPeers(st, members)
		if grown == nil {
			// Nothing to record yet — the poll looks again on the next
			// tick, and a pass that records nothing must not rewrite
			// node.yaml with the bytes it just read.
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

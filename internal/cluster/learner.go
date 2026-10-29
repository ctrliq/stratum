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

// learnerPromotePoll is how often a joined control plane looks at its own etcd
// member until it is a voter.
var learnerPromotePoll = 5 * time.Second

// The learner task's seams, so a test can drive it without etcd or the host's
// data dir.
var (
	promoteLocalLearner = ha.PromoteLearner
	lockEtcdData        = func() (func(), error) { return etcddata.Lock(version.EtcdDataDir) }
)

// promoteLearner promotes the learner a control-plane join added for this node
// (ha.MemberAdd) once it has caught up, then holds until shutdown. Only a
// running etcd can catch up, so the promote belongs to the controller's start,
// and to every start while node state records it pending
// (EtcdPromotePending): one that stopped before the promote landed leaves a
// learner the next start promotes.
func promoteLearner(ctx context.Context) {
	t := time.NewTicker(learnerPromotePoll)
	defer t.Stop()
	var warned string
	for {
		if promoteLearnerOnce(&warned) {
			<-ctx.Done()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// promoteLearnerOnce is one look, and reports whether nothing is left to
// promote. It runs under the etcd data lock, against node state read after
// taking it: a snapshot rejoin holds that lock while it replaces this node's
// member, promotes its own learner and clears EtcdPromotePending, so a held
// lock skips the look. warned is the last warning printed, so a refusal that
// repeats is printed once.
func promoteLearnerOnce(warned *string) bool {
	release, err := lockEtcdData()
	if err != nil {
		return false
	}
	defer release()
	st, err := LoadState()
	if err != nil {
		return false
	}
	if !st.EtcdPromotePending {
		return true
	}
	state, err := promoteLocalLearner(st.NodeIP, st.EtcdInitialCluster)
	if err != nil {
		if msg := err.Error(); msg != *warned {
			*warned = msg
			fmt.Fprintf(os.Stderr, "stratum: WARN: %v (retrying)\n", err)
		}
		return false
	}
	if state == ha.LearnerWaiting {
		return false
	}
	if state == ha.LearnerPromoted {
		fmt.Println("stratum: this node's etcd member caught up and was promoted from learner to voter")
	}
	err = nodestate.MutateHeld(release, func(cur *NodeState) error {
		if !cur.EtcdPromotePending {
			return nodestate.ErrUnchanged
		}
		cur.EtcdPromotePending = false
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stratum: WARN: record the etcd member's promotion: %v (retrying)\n", err)
		return false
	}
	return true
}

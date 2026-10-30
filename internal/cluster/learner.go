package cluster

import (
	"context"
	"errors"
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

// learnerWarnAfter is how long the learner task waits on a learner that
// cannot be promoted yet before it warns.
var learnerWarnAfter = 5 * time.Minute

// learnerLook is what the learner task carries from one look to the next:
// warned is the last refusal printed, so a refusal that repeats is printed
// once, and waitingSince is when the learner was first found waiting, so a
// wait that lasts learnerWarnAfter is printed once (waitWarned).
type learnerLook struct {
	warned       string
	waitingSince time.Time
	waitWarned   bool
}

// warn prints msg unless it was the last refusal printed.
func (l *learnerLook) warn(msg string) {
	if msg != l.warned {
		l.warned = msg
		fmt.Fprintf(os.Stderr, "stratum: WARN: %s (retrying)\n", msg)
	}
}

// promoteLearner promotes the learner a control-plane join added for this node
// (ha.MemberAdd) once it has caught up, then holds until shutdown. Only a
// running etcd can catch up, so the promote belongs to the controller's start,
// and to every start while node state records it pending
// (EtcdPromotePending): one that stopped before the promote landed leaves a
// learner the next start promotes.
//
// etcd allows one learner at a time, so while this one waits no other control
// plane can join. A learner still waiting after learnerWarnAfter is printed
// once, with the reason etcd gave.
func promoteLearner(ctx context.Context) {
	var look learnerLook
	for {
		if promoteLearnerOnce(&look) {
			<-ctx.Done()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(learnerPromotePoll):
		}
	}
}

// promoteLearnerOnce is one look, and reports whether nothing is left to
// promote. It reads node state under the etcd data lock, then releases the
// lock for the etcd calls, which can take seconds and would otherwise refuse
// a rejoin, repoint, restore or uninstall that needs the lock, and retakes it
// to clear EtcdPromotePending only if it is still recorded. A snapshot rejoin
// holds that lock while it replaces this node's member, promotes its own
// learner and clears EtcdPromotePending, so a held lock skips the look.
func promoteLearnerOnce(look *learnerLook) bool {
	st, err := readPendingPromote()
	if err != nil {
		look.warn(fmt.Sprintf("read node state to promote this node's etcd learner: %v", err))
		return false
	}
	if st == nil {
		return false
	}
	if !st.EtcdPromotePending {
		return true
	}
	state, err := promoteLocalLearner(st.NodeIP, st.EtcdInitialCluster)
	if state == ha.LearnerWaiting {
		if look.waitingSince.IsZero() {
			look.waitingSince = time.Now()
		}
		switch {
		case err != nil && !errors.Is(err, ha.ErrLearnerWaiting):
			look.warn(err.Error())
		case !look.waitWarned && time.Since(look.waitingSince) >= learnerWarnAfter:
			look.waitWarned = true
			fmt.Fprintf(os.Stderr, "stratum: WARN: this node's etcd member is still a learner after %v (%v); "+
				"no other control plane can join until it is promoted (still retrying)\n", learnerWarnAfter, err)
		}
		return false
	}
	if err != nil {
		look.warn(err.Error())
		return false
	}
	if state == ha.LearnerPromoted {
		fmt.Println("stratum: this node's etcd member caught up and was promoted from learner to voter")
	}
	release, err := lockEtcdData()
	if err != nil {
		return false
	}
	defer release()
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

// readPendingPromote reads node state under the etcd data lock and releases
// it. It returns no state and no error when the lock is held, and an error
// when the state cannot be read.
func readPendingPromote() (*NodeState, error) {
	release, err := lockEtcdData()
	if err != nil {
		return nil, nil
	}
	defer release()
	return LoadState()
}

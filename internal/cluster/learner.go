package cluster

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

var learnerPromotePoll = 5 * time.Second

// The learner task's seams, so a test can drive it without etcd or the host's data dir.
var (
	promoteLocalLearner = ha.PromoteLearner
	confirmLocalVoter   = ha.ConfirmVoter
	lockEtcdData        = func() (func(), error) { return etcddata.Lock(version.EtcdDataDir) }
)

var learnerWarnAfter = 5 * time.Minute

type learnerLook struct {
	warned       string
	waitingSince time.Time
	waitWarned   bool
}

func (l *learnerLook) warn(msg string) {
	if msg != l.warned {
		l.warned = msg
		fmt.Fprintf(os.Stderr, "stratum: WARN: %s (retrying)\n", msg)
	}
}

// etcd allows one learner at a time, so an unpromoted learner blocks later control-plane joins.
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

// The promote runs without the etcd data lock, which it would deny to a rejoin for seconds.
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
		if !lockHeld(err) {
			look.warn(fmt.Sprintf("record the etcd member's promotion: %v", err))
		}
		return false
	}
	defer release()
	err = nodestate.MutateHeld(release, func(cur *NodeState) error {
		if !cur.EtcdPromotePending {
			return nodestate.ErrUnchanged
		}
		if err := confirmLocalVoter(cur.NodeIP); err != nil {
			return fmt.Errorf("%w: %v", errNotVoterYet, err)
		}
		cur.EtcdPromotePending = false
		return nil
	})
	if errors.Is(err, errNotVoterYet) {
		return false
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "stratum: WARN: record the etcd member's promotion: %v (retrying)\n", err)
		return false
	}
	return true
}

// Not recorded as a promote: the member no longer answers as a voter under the retaken lock.
var errNotVoterYet = errors.New("this node's etcd member is not a voter")

func lockHeld(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK)
}

// Returns nil, nil when another operation holds the lock.
func readPendingPromote() (*NodeState, error) {
	release, err := lockEtcdData()
	if lockHeld(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer release()
	return LoadState()
}

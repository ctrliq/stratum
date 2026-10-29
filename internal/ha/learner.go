package ha

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// LearnerState is where PromoteLearner left this node's etcd member.
type LearnerState int

const (
	// LearnerWaiting is a learner that cannot be promoted yet: its etcd is
	// not answering, knows no leader, or has not caught up with the leader.
	LearnerWaiting LearnerState = iota
	// LearnerPromoted is a learner this call made a voter.
	LearnerPromoted
	// MemberIsVoter is a member that was already a voter.
	MemberIsVoter
)

// The etcd operations PromoteLearner runs, indirected so its decisions can be
// driven without a cluster.
var (
	learnerLocalStatus = memberStatusAt
	learnerPromote     = promoteThrough
)

// PromoteLearner makes this node's etcd member a voter once it has caught up
// with the leader. A control-plane join adds its member as a learner
// (MemberAdd), and the promote has to wait for the member's etcd to start and
// replay the leader's log, so the controller's start retries this until it
// reports MemberIsVoter or LearnerPromoted.
//
// A learner serves only Status and serializable reads, so the promote goes
// through the other members recorded in initialCluster, never through this
// node's own etcd. Every answer that a later attempt can still land
// (isPromoteNotYet) is LearnerWaiting with no error; an error is a promote
// etcd refused for another reason.
func PromoteLearner(nodeIP, initialCluster string) (LearnerState, error) {
	st, err := learnerLocalStatus(nodeIP)
	if err != nil || st.Leader == 0 {
		return LearnerWaiting, nil
	}
	if !st.IsLearner {
		return MemberIsVoter, nil
	}
	voters := otherMemberHosts(initialCluster, nodeIP)
	if len(voters) == 0 {
		return LearnerWaiting, fmt.Errorf("etcd member %x is a learner, but node state records no other member "+
			"to promote it through (initial cluster %q)", st.MemberID, initialCluster)
	}
	perr := learnerPromote(voters, st.MemberID)
	switch {
	case perr == nil, isMemberNotLearner(perr):
		return LearnerPromoted, nil
	case isPromoteNotYet(perr):
		return LearnerWaiting, nil
	}
	return LearnerWaiting, fmt.Errorf("promote etcd learner %x via %s: %w", st.MemberID, strings.Join(voters, ","), perr)
}

// otherMemberHosts are the hosts of the members an ETCD_INITIAL_CLUSTER string
// names, other than self, each once.
func otherMemberHosts(initialCluster, self string) []string {
	var hosts []string
	seen := map[string]bool{}
	for entry := range strings.SplitSeq(initialCluster, ",") {
		_, peer, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			continue
		}
		h, _, err := peerHostPort(peer)
		if err != nil || h == "" || sameIP(h, self) || seen[h] {
			continue
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	return hosts
}

// promoteThrough promotes member id through the etcd at voters. No endpoint
// sync: the synced list would include the learner itself, which refuses the
// promote.
func promoteThrough(voters []string, id uint64) error {
	cli, err := etcdClient(voters...)
	if err != nil {
		return err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = cli.MemberPromote(ctx, id)
	return err
}

package ha

import (
	"errors"
	"fmt"
	"strings"
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

// ErrLearnerWaiting is what PromoteLearner's error wraps when the learner
// cannot be promoted yet but a later attempt can still land the promote. The
// rest of the error says why it is waiting.
var ErrLearnerWaiting = errors.New("etcd learner not promoted yet")

// The etcd operations PromoteLearner runs, indirected so its decisions can be
// driven without a cluster.
var (
	learnerLocalStatus = memberStatusAt
	learnerPromote     = memberPromote
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
// (isPromoteNotYet) is LearnerWaiting with an error wrapping ErrLearnerWaiting
// that says why, so a caller can tell an operator what the learner is waiting
// on; any other error is a promote etcd refused for another reason.
func PromoteLearner(nodeIP, initialCluster string) (LearnerState, error) {
	st, err := learnerLocalStatus(nodeIP)
	if err != nil {
		return LearnerWaiting, fmt.Errorf("%w: this node's etcd does not answer: %v", ErrLearnerWaiting, err)
	}
	if st.Leader == 0 {
		return LearnerWaiting, fmt.Errorf("%w: this node's etcd member %x knows no leader", ErrLearnerWaiting, st.MemberID)
	}
	if !st.IsLearner {
		return MemberIsVoter, nil
	}
	voters := otherMemberHosts(initialCluster, nodeIP)
	if len(voters) == 0 {
		return LearnerWaiting, fmt.Errorf("etcd member %x is a learner, but node state records no other member "+
			"to promote it through (initial cluster %q)", st.MemberID, initialCluster)
	}
	perr := learnerPromote(st.MemberID, voters...)
	switch {
	case perr == nil, isMemberNotLearner(perr):
		return LearnerPromoted, nil
	case isPromoteNotYet(perr):
		return LearnerWaiting, fmt.Errorf("%w: etcd did not promote member %x yet: %v", ErrLearnerWaiting, st.MemberID, perr)
	}
	return LearnerWaiting, fmt.Errorf("promote etcd learner %x via %s: %w", st.MemberID, strings.Join(voters, ","), perr)
}

// ConfirmVoter reports an error unless this node's etcd answers as a voter.
// The controller asks it under the etcd data lock before it records a promote
// done: a snapshot rejoin can replace this node's member between a promote and
// that record, and leave its own learner for the next start to promote.
func ConfirmVoter(nodeIP string) error {
	st, err := learnerLocalStatus(nodeIP)
	if err != nil {
		return fmt.Errorf("this node's etcd does not answer: %w", err)
	}
	if st.IsLearner {
		return fmt.Errorf("this node's etcd member %x is a learner", st.MemberID)
	}
	return nil
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

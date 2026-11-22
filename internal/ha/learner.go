package ha

import (
	"errors"
	"fmt"
	"strings"
)

// LearnerState is where PromoteLearner left this node's etcd member.
type LearnerState int

const (
	LearnerWaiting LearnerState = iota
	LearnerPromoted
	MemberIsVoter
)

// what PromoteLearner's error wraps when a later attempt can still land; the rest says why it waits
var ErrLearnerWaiting = errors.New("etcd learner not promoted yet")

// indirected so PromoteLearner's decisions can be driven without a cluster
var (
	learnerLocalStatus = memberStatusAt
	learnerPromote     = memberPromote
)

// promotes through the other members initialCluster names; a learner serves only serializable reads
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

// asked under the data lock before recording the promote: a snapshot rejoin can replace this member
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

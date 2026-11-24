package ha

import (
	"io"

	"context"
	"errors"
	"strings"
	"testing"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
)

func TestConfirmQuorumLost(t *testing.T) {
	const cluster = 7
	member := func(id uint64, learner bool) *etcdserverpb.Member {
		return &etcdserverpb.Member{ID: id, IsLearner: learner}
	}
	five := []*etcdserverpb.Member{member(1, false), member(2, false), member(3, false), member(4, false), member(5, false)}
	answers := func(ids map[uint64]uint64) func(*etcdserverpb.Member) (uint64, bool) {
		return func(m *etcdserverpb.Member) (uint64, bool) { id, ok := ids[m.ID]; return id, ok }
	}
	for _, tc := range []struct {
		name    string
		lc      localCluster
		listErr error
		peers   map[uint64]uint64
		want    string
	}{
		{"five members, local one cut off, three others answer",
			localCluster{cluster, 2, five}, nil, map[uint64]uint64{3: cluster, 4: cluster, 5: cluster}, "still has quorum"},
		{"local plus one peer of three",
			localCluster{cluster, 1, five[:3]}, nil, map[uint64]uint64{2: cluster, 3: 99}, "still has quorum"},
		{"local alone of three, both peers silent: they may hold a majority",
			localCluster{cluster, 1, five[:3]}, nil, nil, "cannot rule out"},
		{"local alone of five, the rest silent",
			localCluster{cluster, 1, five}, nil, nil, "cannot rule out"},
		{"one silent peer is enough to doubt it",
			localCluster{cluster, 1, five[:3]}, nil, map[uint64]uint64{2: 99}, "cannot rule out"},
		{"every peer answers from another cluster",
			localCluster{cluster, 1, five[:3]}, nil, map[uint64]uint64{2: 99, 3: 99}, ""},
		{"a replaced seed and the rest silent short of a majority",
			localCluster{cluster, 1, five}, nil, map[uint64]uint64{2: 99, 3: 99, 4: 99}, ""},
		{"a replaced seed does not count, the others are silent",
			localCluster{cluster, 2, five[:3]}, nil, map[uint64]uint64{1: 99}, "cannot rule out"},
		{"a learner does not vote",
			localCluster{cluster, 1, []*etcdserverpb.Member{member(1, false), member(2, false), member(3, true)}}, nil,
			map[uint64]uint64{2: 99}, ""},
		{"membership unreadable", localCluster{}, errors.New("connection refused"), nil, "unknown whether"},
		{"no voting members", localCluster{cluster, 1, nil}, nil, nil, "unknown whether"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := confirmQuorumLost(func() (localCluster, error) { return tc.lc, tc.listErr }, answers(tc.peers))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("confirmQuorumLost = %v, want a confirmed loss", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("confirmQuorumLost = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

func TestReviewRejoinRefusesUnknownPeerMajority(t *testing.T) {
	wiped := false
	restoreSeams(t, func() error { return nil }, func(string) error { return nil },
		func(w io.Writer) error { wiped = true; return errors.New("stopped after wipe") })
	members := make([]*etcdserverpb.Member, 5)
	for i := range members {
		members[i] = &etcdserverpb.Member{ID: uint64(i + 1)}
	}
	rejoinConfirmQuorumLost = func() error {
		return confirmQuorumLost(func() (localCluster, error) {
			return localCluster{ClusterID: 7, SelfID: 2, Members: members}, nil
		}, func(m *etcdserverpb.Member) (uint64, bool) {
			if m.ID == 1 {
				return 99, true
			}
			return 0, false
		})
	}
	err := RejoinEtcd(context.Background(), io.Discard, "10.255.255.1", false)
	if wiped {
		t.Fatalf("wiped without --force despite unknown peer majority: %v", err)
	}
	if err == nil {
		t.Fatal("expected refusal for inconclusive peer probes")
	}
}

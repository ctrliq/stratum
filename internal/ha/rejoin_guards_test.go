package ha

import (
	"bytes"
	"io"

	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/status"

	"github.com/ctrliq/stratum/internal/nodestate"
)

// TestPlanRejoinRefusesToActOnAStaleMembership: a member list read without
// quorum may be stale, and every action but resume removes a member or wipes a
// data dir on the strength of it -- in a cluster that cannot commit the
// removal. It is the recovery path for the shape a timed-out voting add left behind, where a
// linearizable member list cannot answer at all.
func TestPlanRejoinRefusesToActOnAStaleMembership(t *testing.T) {
	self := etcdMember{ID: 2, PeerURLs: []string{selfPeer}, ClientURLs: []string{"https://10.0.0.2:2379"}}
	other := etcdMember{ID: 3, Name: "cp3", PeerURLs: []string{"https://10.0.0.3:2380"},
		ClientURLs: []string{"https://10.0.0.3:2379"}}
	byIP := map[string]uint64{"10.0.0.1": 1, "10.0.0.2": 2, "10.0.0.3": 3}
	allHealthy := func(ip string) (localEtcdStatus, bool) {
		return localEtcdStatus{ClusterID: fakeCluster, MemberID: byIP[ip]}, true
	}
	refusedLocal := errors.New("connection refused")

	for _, tc := range []struct {
		name   string
		quorum bool
		self   etcdMember
		want   rejoinAction
	}{
		{"a voter, with quorum, is replaced", true, self, rejoinReplace},
		{"a voter, without quorum, is resumed", false, self, rejoinResume},
		{"a learner, with quorum, is replaced", true,
			etcdMember{ID: 2, PeerURLs: []string{selfPeer}, IsLearner: true}, rejoinReplace},
		{"a learner, without quorum, is resumed", false,
			etcdMember{ID: 2, PeerURLs: []string{selfPeer}, IsLearner: true}, rejoinResume},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := etcdMembership{ClusterID: fakeCluster, Quorum: tc.quorum,
				Members: []etcdMember{seedMember, tc.self, other}}
			got, _ := planRejoin(seed, &seed.Members[1], localEtcdStatus{}, refusedLocal, false, allHealthy)
			if got != tc.want {
				t.Errorf("planRejoin = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRejoinWithoutQuorumAndWithoutAMemberStops: resuming needs a member to
// resume. Wiping and adding one needs a cluster that can commit the add, which
// is exactly what a quorum-less cluster cannot do, so the old data is the only
// thing of value left and rejoin must not destroy it.
func TestRejoinWithoutQuorumAndWithoutAMemberStops(t *testing.T) {
	f := newFakeRejoin(t, etcdMembership{ClusterID: fakeCluster, Members: []etcdMember{seedMember}}, controller())
	err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false)
	if err == nil {
		t.Fatal("rejoin proceeded against a cluster with no quorum")
	}
	if !strings.Contains(err.Error(), "no quorum") || !strings.Contains(err.Error(), "data was left alone") {
		t.Errorf("error does not say what was and was not done: %v", err)
	}
	if got := f.steps(); got != "" {
		t.Errorf("steps = %q, want none", got)
	}
}

// TestWaitRejoinedChecksIdentityNotJustReachability: a local etcd that answers
// is not necessarily THIS member of THIS cluster. One still serving the
// pre-restore data answers Status perfectly well, and treating that as
// rejoined starts kube-apiserver against the data the restore replaced.
func TestWaitRejoinedChecksIdentityNotJustReachability(t *testing.T) {
	f := newFakeRejoin(t, seedWith(etcdMember{ID: 2, Name: "cp2", PeerURLs: []string{selfPeer},
		ClientURLs: []string{"https://10.0.0.2:2379"}, IsLearner: true}),
		&nodestate.NodeState{Role: "controller", EtcdName: "cp2", EtcdState: "existing",
			EtcdInitialCluster: "cp1=https://10.0.0.1:2380,cp2=" + selfPeer})
	var applied uint64
	f.local = func(time.Duration) (localEtcdStatus, error) {
		applied++
		return localEtcdStatus{ClusterID: 0xDEAD, MemberID: 99, Leader: 1, AppliedIndex: applied}, nil
	}
	err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false)
	if err == nil {
		t.Fatal("an etcd serving a different cluster was reported rejoined")
	}
	if !strings.Contains(err.Error(), "answers as member") {
		t.Errorf("error does not name the wrong identity: %v", err)
	}
	if strings.Contains(f.steps(), "kube-apiserver") {
		t.Errorf("kube-apiserver was started against the wrong cluster: %q", f.steps())
	}
}

// TestMemberHealthySkipsLoopback: a member's advertised client URL may be a
// loopback address, and probing it reaches THIS node's etcd rather than that
// member's. A false positive here is the direction that wipes a vote.
func TestMemberHealthySkipsLoopback(t *testing.T) {
	up := map[string]bool{"127.0.0.1": true, "localhost": true, "10.0.0.3": false}
	m := etcdMember{ID: 3, ClientURLs: []string{"https://127.0.0.1:2379", "https://10.0.0.3:2379"}}
	probe := func(ip string) (localEtcdStatus, bool) {
		return localEtcdStatus{ClusterID: fakeCluster, MemberID: 3}, up[ip]
	}
	if memberHealthy(m, fakeCluster, probe) {
		t.Error("a loopback client URL was probed, so another member's health was read off this node's etcd")
	}
}

// TestMemberHealthyChecksIdentityNotJustReachability: during a restore another
// control plane can still be up and serving the PRE-restore cluster. Counting
// it as a healthy voter of the restored one inflates the quorum arithmetic in
// the one direction that wipes a vote the cluster is waiting on.
func TestMemberHealthyChecksIdentityNotJustReachability(t *testing.T) {
	m := etcdMember{ID: 3, ClientURLs: []string{"https://10.0.0.3:2379"}}
	for _, tc := range []struct {
		name string
		st   localEtcdStatus
		want bool
	}{
		{"this member of this cluster", localEtcdStatus{ClusterID: fakeCluster, MemberID: 3}, true},
		{"still serving the pre-restore cluster", localEtcdStatus{ClusterID: 0xDEAD, MemberID: 3}, false},
		{"a different member of this cluster", localEtcdStatus{ClusterID: fakeCluster, MemberID: 9}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := memberHealthy(m, fakeCluster, func(string) (localEtcdStatus, bool) { return tc.st, true })
			if got != tc.want {
				t.Errorf("memberHealthy = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSameInitialClusterRejectsExtraRecordedMembers: a recorded initial
// cluster naming a member the cluster no longer has makes etcd refuse to
// start, so it must count as different and be rewritten.
func TestSameInitialClusterRejectsExtraRecordedMembers(t *testing.T) {
	members := []etcdMember{
		{ID: 1, Name: "cp1", PeerURLs: []string{"https://10.0.0.1:2380"}},
		{ID: 2, Name: "cp2", PeerURLs: []string{selfPeer}},
	}
	stale := "cp1=https://10.0.0.1:2380,cp2=" + selfPeer + ",cp3=https://10.0.0.3:2380"
	if sameInitialCluster(stale, members, selfPeer, "cp2") {
		t.Error("a recorded cluster with a removed member was accepted as unchanged")
	}
	exact := "cp1=https://10.0.0.1:2380,cp2=" + selfPeer
	if !sameInitialCluster(exact, members, selfPeer, "cp2") {
		t.Error("an exact match was rewritten")
	}
}

// TestAddSettlingOnlyRetriesRefusals is addSettling's stated contract: every
// error the predicate accepts must be a refusal that added nothing, because a
// retry of anything else adds the member twice.
func TestAddSettlingOnlyRetriesRefusals(t *testing.T) {
	t.Cleanup(func() { memberAddSettle = 90 * time.Second })
	memberAddSettle = 10 * time.Millisecond

	calls := 0
	_, err := addSettling(io.Discard, func(context.Context) (*clientv3.MemberAddResponse, error) {
		calls++
		return nil, context.DeadlineExceeded
	}, isUnhealthyCluster, "")
	if calls != 1 {
		t.Errorf("a client deadline was attempted %d times; a second add would add the member twice", calls)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline returned unchanged", err)
	}

	calls = 0
	if _, err := addSettling(io.Discard, func(context.Context) (*clientv3.MemberAddResponse, error) {
		calls++
		return nil, rpctypes.ErrGRPCUnhealthy
	}, isUnhealthyCluster, ""); !isUnhealthyCluster(err) {
		t.Errorf("err = %v, want the refusal after the settle window", err)
	}
	if calls < 2 {
		t.Errorf("a settling refusal was attempted %d times, want a retry", calls)
	}
}

// TestMemberAddSettlingRetriesOnlyRefusalsThatAddedNothing pins the refusals
// a learner add retries. etcd allows one learner at a time, so a join that
// follows another closely is refused with too-many-learners until the earlier
// joiner is promoted, and that refusal must be retried, not returned.
func TestMemberAddSettlingRetriesOnlyRefusalsThatAddedNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the strict reconfiguration refusal", rpctypes.ErrGRPCUnhealthy, true},
		{"another member still a learner", rpctypes.ErrGRPCTooManyLearners, true},
		{"another member still a learner, wrapped", fmt.Errorf("member add: %w", rpctypes.ErrTooManyLearners), true},
		{"a client deadline, which may have added the member", context.DeadlineExceeded, false},
		{"a duplicate peer URL", rpctypes.ErrGRPCPeerURLExist, false},
		{"no error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := memberAddSettling(tc.err); got != tc.want {
				t.Errorf("memberAddSettling(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}

	t.Cleanup(func() { memberAddSettle = 90 * time.Second })
	memberAddSettle = 10 * time.Millisecond
	calls := 0
	if _, err := addSettling(io.Discard, func(context.Context) (*clientv3.MemberAddResponse, error) {
		calls++
		if calls == 1 {
			return nil, rpctypes.ErrGRPCTooManyLearners
		}
		return &clientv3.MemberAddResponse{}, nil
	}, memberAddSettling, ""); err != nil {
		t.Errorf("an add refused once with too-many-learners = %v, want it retried to success", err)
	}
	if calls != 2 {
		t.Errorf("the add ran %d times, want 2", calls)
	}
}

func TestAddSettlingKeepsRejoinRetriesOffStdout(t *testing.T) {
	var progress bytes.Buffer
	calls := 0
	onStdout := captureRealStdout(t, func() {
		_, err := addSettling(&progress, func(context.Context) (*clientv3.MemberAddResponse, error) {
			calls++
			if calls == 1 {
				return nil, rpctypes.ErrGRPCUnhealthy
			}
			return &clientv3.MemberAddResponse{}, nil
		}, isUnhealthyCluster, "rejoin is waiting for etcd to settle")
		if err != nil {
			t.Fatalf("retry: %v", err)
		}
	})
	if calls != 2 {
		t.Fatalf("add calls = %d, want refusal then successful retry", calls)
	}
	if progress.String() != "rejoin is waiting for etcd to settle\n" {
		t.Fatalf("retry progress = %q, want the waiting notice", progress.String())
	}
	if onStdout != "" {
		t.Fatalf("rejoin retry corrupted the MCP stdout transport: %q", onStdout)
	}
}

// TestPromoteErrorsAreClassifiedByWhatEtcdCanReturn. etcd never answers a
// promote with ErrUnhealthy -- that comes from add, remove and leadership
// transfer -- while the errors it does return transiently were all treated as
// final, aborting a rejoin that one more attempt would have finished.
func TestPromoteErrorsAreClassifiedByWhatEtcdCanReturn(t *testing.T) {
	for _, err := range []error{
		rpctypes.ErrGRPCLearnerNotReady, rpctypes.ErrMemberLearnerNotReady,
		rpctypes.ErrGRPCMemberNotEnoughStarted,
		rpctypes.ErrGRPCNoLeader, rpctypes.ErrGRPCNotLeader, rpctypes.ErrGRPCLeaderChanged,
		rpctypes.ErrGRPCTimeout, rpctypes.ErrGRPCTimeoutWaitAppliedIndex,
		context.DeadlineExceeded, context.Canceled,
	} {
		if !isPromoteNotYet(err) {
			t.Errorf("isPromoteNotYet(%v) = false; a rejoin aborts on an error a retry would have cleared", err)
		}
	}
	for _, err := range []error{nil, rpctypes.ErrGRPCMemberNotLearner, errors.New("connection refused")} {
		if isPromoteNotYet(err) {
			t.Errorf("isPromoteNotYet(%v) = true, want false", err)
		}
	}
	if !isMemberNotLearner(rpctypes.ErrGRPCMemberNotLearner) || !isMemberNotLearner(rpctypes.ErrMemberNotLearner) {
		t.Error("an already-promoted member must read as success")
	}
	if !isTooManyLearners(rpctypes.ErrGRPCTooManyLearners) || !isTooManyLearners(rpctypes.ErrTooManyLearners) {
		t.Error("etcd's one-learner-at-a-time refusal must be recognised")
	}
}

// TestEtcdErrorsSurviveWrapping is why these predicates use errors.Is and not
// a comparison of rendered messages: one %w anywhere between the client and
// the predicate silently stopped every string comparison from matching, with
// no compile error and no test failure.
func TestEtcdErrorsSurviveWrapping(t *testing.T) {
	wrapped := fmt.Errorf("rejoin: %w", fmt.Errorf("etcd member promote: %w", rpctypes.ErrMemberLearnerNotReady))
	if !isPromoteNotYet(wrapped) {
		t.Error("a twice-wrapped learner-not-ready stopped matching")
	}
	if !isUnhealthyCluster(fmt.Errorf("etcd member add: %w", rpctypes.ErrUnhealthy)) {
		t.Error("a wrapped unhealthy-cluster refusal stopped matching")
	}
}

// TestPromoteErrorsMatchAsTheClientReturnsThem builds each error the way it
// arrives from the wire, a fresh gRPC status rather than rpctypes' singleton,
// and passes it through clientv3.ContextError as MemberPromote does before
// memberPromote wraps it. ErrTimeoutWaitAppliedIndex is the case that needs
// this: rpctypes has no client-side mapping for it, so it reaches the
// predicate as a raw *status.Error and matches only through status's own Is.
func TestPromoteErrorsMatchAsTheClientReturnsThem(t *testing.T) {
	for _, sentinel := range []error{
		rpctypes.ErrGRPCTimeoutWaitAppliedIndex,
		rpctypes.ErrGRPCLearnerNotReady,
		rpctypes.ErrGRPCMemberNotEnoughStarted,
		rpctypes.ErrGRPCNoLeader,
		rpctypes.ErrGRPCNotLeader,
		rpctypes.ErrGRPCLeaderChanged,
		rpctypes.ErrGRPCTimeout,
	} {
		if err := promoteWireErr(t, sentinel); !isPromoteNotYet(err) {
			t.Errorf("isPromoteNotYet(%v) = false for a fresh wire error; a rejoin aborts on a transient", err)
		}
	}
	if err := promoteWireErr(t, rpctypes.ErrGRPCMemberNotLearner); !isMemberNotLearner(err) {
		t.Errorf("isMemberNotLearner(%v) = false for a fresh wire error; a landed promote reads as failure", err)
	}
	if err := promoteWireErr(t, rpctypes.ErrGRPCUnhealthy); isPromoteNotYet(err) {
		t.Errorf("isPromoteNotYet(%v) = true: it shares the Unavailable code with the transients, "+
			"so the match is on the code alone, not the error", err)
	}
}

// promoteWireErr is sentinel as memberPromote returns it: a fresh status with
// the same code and message, through clientv3.ContextError, then wrapped.
func promoteWireErr(t *testing.T, sentinel error) error {
	t.Helper()
	s, ok := status.FromError(sentinel)
	if !ok {
		t.Fatalf("%v is not a gRPC status", sentinel)
	}
	wire := clientv3.ContextError(context.Background(), status.Error(s.Code(), s.Message()))
	return fmt.Errorf("rejoin: %w", fmt.Errorf("etcd member promote: %w", wire))
}

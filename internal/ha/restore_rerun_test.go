package ha

import (
	"io"

	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"

	"github.com/ctrliq/stratum/internal/nodestate"
)

// fakeRejoin stands in for the seed, this node's etcd and the clock, and
// records every step RejoinEtcd takes in the order it takes them. The time
// is fake so a test can wait thirty minutes in microseconds.
type fakeRejoin struct {
	calls   []string
	start   time.Time
	now     time.Time
	started time.Time // when etcd's manifest was (re)written; zero until then
	saved   *nodestate.NodeState
	// local is this node's etcd, as a function of how long it has been
	// running (negative: not started by this run).
	local   func(sinceStart time.Duration) (localEtcdStatus, error)
	promote func(sinceStart time.Duration) error
	running func() bool
	data    func(sinceStart time.Duration) etcdDataMark
	// seed is the seed's membership, with the learner this run adds.
	seed etcdMembership
}

const (
	fakeCluster = 0xc1
	selfPeer    = "https://10.0.0.2:2380"
	// addedID is the member a learner add creates: not 2, the ID of the stale
	// member a replace removes, so a test catches a wait on the wrong one.
	addedID = 4
)

func newFakeRejoin(t *testing.T, seed etcdMembership, st *nodestate.NodeState) *fakeRejoin {
	t.Helper()
	f := &fakeRejoin{start: time.Unix(1e9, 0), seed: seed}
	f.now = f.start
	restoreSeams(t, func() error { return nil }, func(string) error { return nil },
		func(w io.Writer) error { f.calls = append(f.calls, "wipe"); return nil })
	rejoinLoadState = func() (*nodestate.NodeState, error) { return st, nil }
	rejoinSeedMembers = func(string) (etcdMembership, error) { return f.seed, nil }
	rejoinNow = func() time.Time { return f.now }
	rejoinSleep = func(d time.Duration) {
		f.now = f.now.Add(d)
		if f.elapsed() > 2*rejoinMaxWait {
			t.Fatalf("rejoin still waiting after %v of fake time: the wait has no bound", f.elapsed())
		}
	}
	rejoinLocalStatus = func(string) (localEtcdStatus, error) { return f.local(f.since()) }
	rejoinEtcdRunning = func() bool { return f.running() }
	rejoinDataMark = func() etcdDataMark { return f.data(f.since()) }
	rejoinAddLearner = func(_ io.Writer, _, name, peer string) (uint64, string, error) {
		f.calls = append(f.calls, "add-learner")
		f.seed.Members = append(slices.Clone(f.seed.Members), etcdMember{ID: addedID, IsLearner: true, PeerURLs: []string{peer}})
		return addedID, "cp1=https://10.0.0.1:2380," + name + "=" + peer, nil
	}
	rejoinRemoveMember = func(_ string, id uint64) error {
		f.calls = append(f.calls, fmt.Sprintf("remove %x", id))
		return nil
	}
	rejoinPromote = func(string, uint64) error {
		f.calls = append(f.calls, "promote")
		return f.promote(f.now.Sub(f.started))
	}
	rejoinSaveState = func(s *nodestate.NodeState) error {
		f.calls = append(f.calls, "save")
		c := *s
		f.saved = &c
		return nil
	}
	rejoinStartPod = func(name string) error {
		f.calls = append(f.calls, "start "+name)
		if name == "etcd" {
			f.started = f.now
		}
		return nil
	}
	rejoinStopPod = func(name string) error {
		f.calls = append(f.calls, "stop "+name)
		return nil
	}
	f.promote = func(time.Duration) error { return nil }
	f.running = func() bool { return !f.started.IsZero() }
	f.data = func(time.Duration) etcdDataMark { return etcdDataMark{} }
	return f
}

func (f *fakeRejoin) elapsed() time.Duration { return f.now.Sub(f.start) }

// since is how long this run has had etcd running; negative before it starts it.
func (f *fakeRejoin) since() time.Duration {
	if f.started.IsZero() {
		return -1
	}
	return f.now.Sub(f.started)
}

// steps is the call log without the repeated promote attempts, which are
// polling rather than steps.
func (f *fakeRejoin) steps() string {
	var out []string
	for _, c := range f.calls {
		if c == "promote" && len(out) > 0 && out[len(out)-1] == "promote" {
			continue
		}
		out = append(out, c)
	}
	return strings.Join(out, ", ")
}

// servingAfter is this node's etcd answering as member id once this run has
// had it running for d (refused before this run starts it), with an applied index that grows every poll.
func servingAfter(d time.Duration, id uint64, learner bool) func(time.Duration) (localEtcdStatus, error) {
	var applied uint64
	return func(since time.Duration) (localEtcdStatus, error) {
		if since < d {
			return localEtcdStatus{}, errors.New("connection refused")
		}
		applied++
		return localEtcdStatus{ClusterID: fakeCluster, MemberID: id, Leader: 1, AppliedIndex: applied, IsLearner: learner}, nil
	}
}

// catchingUp answers nothing until d, and until then only its data dir changes.
func catchingUp(d time.Duration, id uint64, learner bool) (func(time.Duration) (localEtcdStatus, error), func(time.Duration) etcdDataMark) {
	local := servingAfter(d, id, learner)
	data := func(since time.Duration) etcdDataMark {
		if since < 0 || since >= d {
			return etcdDataMark{}
		}
		return etcdDataMark{bytes: int64(since / time.Second)}
	}
	return local, data
}

// leaderAfter is a member that answers at once but knows no leader until d.
func leaderAfter(d time.Duration, id uint64, learner bool) func(time.Duration) (localEtcdStatus, error) {
	var applied uint64
	return func(since time.Duration) (localEtcdStatus, error) {
		applied++
		st := localEtcdStatus{ClusterID: fakeCluster, MemberID: id, AppliedIndex: applied, IsLearner: learner}
		if since >= d {
			st.Leader = 1
		}
		return st, nil
	}
}

func controller() *nodestate.NodeState {
	return &nodestate.NodeState{Role: "controller", EtcdName: "cp2", EtcdState: "new",
		EtcdInitialCluster: "cp2=https://10.0.0.2:2380"}
}

// rejoinHealthFrom answers the health probe the way a real cluster would: an
// endpoint that is up reports the member it actually is, in the cluster it
// actually serves.
func rejoinHealthFrom(seed etcdMembership, up map[string]bool) func(string) (localEtcdStatus, bool) {
	byIP := map[string]uint64{}
	for _, m := range seed.Members {
		for _, cu := range m.ClientURLs {
			if u, err := url.Parse(cu); err == nil {
				byIP[u.Hostname()] = m.ID
			}
		}
	}
	return func(ip string) (localEtcdStatus, bool) {
		return localEtcdStatus{ClusterID: seed.ClusterID, MemberID: byIP[ip]}, up[ip]
	}
}

func seedWith(ms ...etcdMember) etcdMembership {
	return etcdMembership{ClusterID: fakeCluster, Quorum: true, Members: append([]etcdMember{seedMember}, ms...)}
}

// TestRejoinAddsALearnerAndPromotesIt: the add is a learner, never
// a voter. A voting add turns the one-member seed into a two-member cluster
// with a quorum of two before this node has started, which is the state the
// re-run then could not undo.
func TestRejoinAddsALearnerAndPromotesIt(t *testing.T) {
	f := newFakeRejoin(t, seedWith(), controller())
	f.local = servingAfter(10*time.Second, addedID, true)
	notYet := 3
	f.promote = func(time.Duration) error {
		if notYet > 0 {
			notYet--
			return rpctypes.ErrGRPCLearnerNotReady
		}
		return nil
	}

	if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if got, want := f.steps(), "wipe, add-learner, save, start etcd, promote, start kube-apiserver"; got != want {
		t.Errorf("steps = %q, want %q", got, want)
	}
	if notYet != 0 {
		t.Errorf("rejoin stopped retrying a learner that was not in sync yet (%d refusals left)", notYet)
	}
	if f.saved == nil || f.saved.EtcdState != "existing" || f.saved.EtcdName != "cp2" ||
		f.saved.EtcdInitialCluster != "cp1=https://10.0.0.1:2380,cp2="+selfPeer {
		t.Errorf("saved state = %+v", f.saved)
	}
}

// TestRejoinWaitFollowsProgress: the wait gives up only on a member
// that shows no progress, so a slow but converging member still rejoins.
func TestRejoinWaitFollowsProgress(t *testing.T) {
	t.Run("a member slower than a flat 90s still rejoins", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(), controller())
		f.local = servingAfter(4*time.Minute, addedID, true)
		if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err != nil {
			t.Fatalf("a member that answered after 4m was given up on: %v", err)
		}
	})

	t.Run("a learner still catching up keeps the wait going past the stall window", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(), controller())
		f.local = servingAfter(30*time.Second, addedID, true)
		f.promote = func(since time.Duration) error {
			if since < 20*time.Minute {
				return rpctypes.ErrGRPCLearnerNotReady
			}
			return nil
		}
		if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err != nil {
			t.Fatalf("a learner whose applied index kept moving was given up on: %v", err)
		}
		if f.elapsed() < 20*time.Minute {
			t.Errorf("returned after %v, before the promote could succeed", f.elapsed())
		}
	})

	t.Run("a member that never answers fails after the stall window, with safe advice", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(), controller())
		f.local = func(time.Duration) (localEtcdStatus, error) {
			return localEtcdStatus{}, errors.New("connection refused")
		}
		err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false)
		if err == nil {
			t.Fatal("a member that never answered was reported rejoined")
		}
		if e := f.elapsed(); e < rejoinStallTimeout || e > rejoinStallTimeout+time.Minute {
			t.Errorf("gave up after %v, want about %v", e, rejoinStallTimeout)
		}
		for _, want := range []string{"has not answered", "data dir has not changed", "was stopped, so a re-run replaces it",
			"learner, which has no vote", "is safe"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
		if !strings.HasSuffix(f.steps(), "start etcd, stop etcd") {
			t.Errorf("steps = %q: a stalled learner's etcd was left running, so a re-run would resume the same stall", f.steps())
		}
		if strings.Contains(err.Error(), "clears the stale member") {
			t.Errorf("error still carries the advice that bricked the restore: %v", err)
		}
		if strings.Contains(f.steps(), "start kube-apiserver") {
			t.Error("started the apiserver on a member that never joined")
		}
	})

	t.Run("a learner whose applied index stops fails after the stall window", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(), controller())
		f.local = func(since time.Duration) (localEtcdStatus, error) {
			if since < 0 {
				return localEtcdStatus{}, errors.New("connection refused")
			}
			return localEtcdStatus{ClusterID: fakeCluster, MemberID: addedID, Leader: 1, AppliedIndex: 7, IsLearner: true}, nil
		}
		f.promote = func(time.Duration) error { return rpctypes.ErrGRPCLearnerNotReady }
		err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false)
		if err == nil || !strings.Contains(err.Error(), "made no progress") {
			t.Fatalf("a stuck learner: %v", err)
		}
		if !strings.Contains(err.Error(), "promote refused") || !strings.Contains(err.Error(), "in sync with leader") {
			t.Errorf("the error does not say the seed kept refusing the promote: %v", err)
		}
	})

	t.Run("a learner catching up before it answers is waited on past the stall window", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(), controller())
		f.local, f.data = catchingUp(12*time.Minute, addedID, true)
		if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err != nil {
			t.Fatalf("a learner whose data dir kept changing was given up on before it was ready: %v", err)
		}
		if f.elapsed() < 12*time.Minute {
			t.Errorf("returned after %v, before the member was ready", f.elapsed())
		}
	})

	t.Run("a learner whose promote committed unseen is not stopped", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(), controller())
		f.local = func(since time.Duration) (localEtcdStatus, error) {
			if since < 0 {
				return localEtcdStatus{}, errors.New("connection refused")
			}
			return localEtcdStatus{ClusterID: fakeCluster, MemberID: addedID, Leader: 1, AppliedIndex: 7, IsLearner: true}, nil
		}
		f.promote = func(time.Duration) error {
			for i := range f.seed.Members {
				if f.seed.Members[i].ID == addedID {
					f.seed.Members[i].IsLearner = false
				}
			}
			return context.DeadlineExceeded
		}
		err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false)
		if err == nil || !strings.Contains(err.Error(), "made no progress") {
			t.Fatalf("a stalled member: %v", err)
		}
		if strings.Contains(f.steps(), "stop etcd") {
			t.Errorf("steps = %q: stopped a member the seed lists as a voter", f.steps())
		}
		if !strings.Contains(err.Error(), "left running") {
			t.Errorf("the error does not say the member's etcd was left running: %v", err)
		}
	})

	for name, later := range map[string]func(etcdMembership) (etcdMembership, error){
		"cannot be read": func(etcdMembership) (etcdMembership, error) {
			return etcdMembership{}, errors.New("context deadline exceeded")
		},
		"is read without quorum": func(m etcdMembership) (etcdMembership, error) {
			m.Quorum = false
			return m, nil
		},
	} {
		t.Run("a learner is not stopped when the seed's member list "+name, func(t *testing.T) {
			f := newFakeRejoin(t, seedWith(), controller())
			f.local = func(time.Duration) (localEtcdStatus, error) {
				return localEtcdStatus{}, errors.New("connection refused")
			}
			reads := 0
			rejoinSeedMembers = func(string) (etcdMembership, error) {
				if reads++; reads > 1 {
					return later(f.seed)
				}
				return f.seed, nil
			}
			if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err == nil || !strings.Contains(err.Error(), "left running") {
				t.Fatalf("err = %v, want the member's etcd left running", err)
			}
			if strings.Contains(f.steps(), "stop etcd") {
				t.Errorf("steps = %q: stopped a member the seed could not confirm is a learner", f.steps())
			}
		})
	}

	t.Run("a voter that stalls is not stopped", func(t *testing.T) {
		voter := etcdMember{ID: 2, PeerURLs: []string{selfPeer}}
		f := newFakeRejoin(t, seedWith(voter), controller())
		f.local = func(time.Duration) (localEtcdStatus, error) {
			return localEtcdStatus{}, errors.New("connection refused")
		}
		if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err == nil {
			t.Fatal("a voter that never answered was reported rejoined")
		}
		if strings.Contains(f.steps(), "stop etcd") {
			t.Errorf("steps = %q: stopped a voter the cluster may need", f.steps())
		}
	})

	t.Run("a member that answers without a leader is not rejoined yet", func(t *testing.T) {
		voter := etcdMember{ID: 2, Name: "cp2", PeerURLs: []string{selfPeer}, ClientURLs: []string{"https://10.0.0.2:2379"}}
		f := newFakeRejoin(t, seedWith(voter), controller())
		f.local = leaderAfter(time.Minute, 2, false)
		if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err != nil {
			t.Fatalf("rejoin: %v", err)
		}
		if f.elapsed() < time.Minute {
			t.Errorf("returned after %v, before this node's etcd knew a leader", f.elapsed())
		}
	})

	t.Run("a learner without a leader is not promoted yet", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(), controller())
		f.local = leaderAfter(time.Minute, addedID, true)
		first := time.Duration(-1)
		f.promote = func(since time.Duration) error {
			if first < 0 {
				first = since
			}
			return nil
		}
		if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err != nil {
			t.Fatalf("rejoin: %v", err)
		}
		if first < time.Minute {
			t.Errorf("promoted %v after etcd started, before it knew a leader", first)
		}
	})

	t.Run("one that moves forever still ends at the cap", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(), controller())
		f.local = servingAfter(0, addedID, true)
		f.promote = func(time.Duration) error { return rpctypes.ErrGRPCLearnerNotReady }
		err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false)
		if err == nil || !strings.Contains(err.Error(), "had not finished joining") {
			t.Fatalf("an endlessly catching-up learner: %v", err)
		}
		if f.elapsed() < rejoinMaxWait {
			t.Errorf("gave up after %v, before the %v cap", f.elapsed(), rejoinMaxWait)
		}
	})

	t.Run("a promote that already landed is success", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(), controller())
		f.local = servingAfter(0, addedID, true)
		f.promote = func(time.Duration) error { return rpctypes.ErrGRPCMemberNotLearner }
		if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err != nil {
			t.Fatalf("an already-promoted member: %v", err)
		}
	})
}

// TestRejoinRerunAfterAPartialRun is every state a first run can
// stop in, and what the re-run must do there. The bricking re-run wiped this
// node and re-added it while the cluster was waiting on its vote.
func TestRejoinRerunAfterAPartialRun(t *testing.T) {
	learner := etcdMember{ID: 2, Name: "cp2", PeerURLs: []string{selfPeer},
		ClientURLs: []string{"https://10.0.0.2:2379"}, IsLearner: true}
	unstartedLearner := etcdMember{ID: 2, PeerURLs: []string{selfPeer}, IsLearner: true}
	unstartedVoter := etcdMember{ID: 2, PeerURLs: []string{selfPeer}}
	voter := etcdMember{ID: 2, Name: "cp2", PeerURLs: []string{selfPeer}, ClientURLs: []string{"https://10.0.0.2:2379"}}
	cp3 := etcdMember{ID: 3, Name: "cp3", PeerURLs: []string{"https://10.0.0.3:2380"},
		ClientURLs: []string{"https://10.0.0.3:2379"}}
	cp3Learner := etcdMember{ID: 3, Name: "cp3", PeerURLs: []string{"https://10.0.0.3:2380"},
		ClientURLs: []string{"https://10.0.0.3:2379"}, IsLearner: true}
	recorded := func() *nodestate.NodeState {
		return &nodestate.NodeState{Role: "controller", EtcdName: "cp2", EtcdState: "existing",
			EtcdInitialCluster: "cp1=https://10.0.0.1:2380,cp2=" + selfPeer}
	}
	refused := func(time.Duration) (localEtcdStatus, error) {
		return localEtcdStatus{}, errors.New("connection refused")
	}
	runningAs := func(id uint64, isLearner bool) func(time.Duration) (localEtcdStatus, error) {
		var applied uint64
		return func(time.Duration) (localEtcdStatus, error) {
			applied++
			return localEtcdStatus{ClusterID: fakeCluster, MemberID: id, Leader: 1, AppliedIndex: applied, IsLearner: isLearner}, nil
		}
	}

	joinedPending := func() *nodestate.NodeState {
		st := recorded()
		st.EtcdPromotePending = true
		return st
	}
	staleRecord := func() *nodestate.NodeState {
		return &nodestate.NodeState{Role: "controller", EtcdName: "cp2", EtcdState: "existing",
			EtcdInitialCluster: "cp2=" + selfPeer}
	}
	oldClusterUntilStarted := func(id, then uint64, learner bool) func(time.Duration) (localEtcdStatus, error) {
		after := servingAfter(0, then, learner)
		return func(since time.Duration) (localEtcdStatus, error) {
			if since < 0 {
				return localEtcdStatus{ClusterID: 0xDEAD, MemberID: id, Leader: 1, AppliedIndex: 9}, nil
			}
			return after(since)
		}
	}

	for _, tc := range []struct {
		name    string
		seed    etcdMembership
		st      *nodestate.NodeState
		local   func(time.Duration) (localEtcdStatus, error)
		running bool
		data    bool
		healthy map[string]bool
		want    string
	}{
		{
			// The flat 90-second timeout: the learner came up on its own after the
			// first run gave up. Nothing is wiped, removed, added or even
			// re-saved -- rewriting the state would restart the member.
			name: "learner serving as itself", seed: seedWith(learner), st: recorded(),
			local: runningAs(2, true),
			want:  "start etcd, promote, start kube-apiserver",
		},
		{
			// A later control plane joined after this one did, so the recorded
			// initial cluster has fallen behind. The member is serving, so it
			// reads its membership from its own WAL: rewriting the state here
			// would restart a healthy voter for nothing.
			name: "voter serving as itself after another member joined",
			seed: seedWith(voter, cp3), st: recorded(),
			local:   runningAs(2, false),
			healthy: map[string]bool{"10.0.0.1": true, "10.0.0.3": true},
			want:    "start etcd, start kube-apiserver",
		},
		{
			// Interrupted between the add and etcd starting. A learner has no
			// vote, so replacing it can cost nothing.
			name: "learner added but never started", seed: seedWith(unstartedLearner), st: controller(),
			local: servingAfter(0, addedID, true),
			want:  "remove 2, wipe, add-learner, save, start etcd, promote, start kube-apiserver",
		},
		{
			name: "learner still catching up", seed: seedWith(learner), st: recorded(),
			running: true, local: servingAfter(3*time.Minute, 2, true), data: true,
			want: "start etcd, promote, start kube-apiserver",
		},
		{
			name: "learner whose etcd answers as the pre-restore cluster", seed: seedWith(learner), st: recorded(),
			running: true, local: oldClusterUntilStarted(2, addedID, true),
			want: "remove 2, wipe, add-learner, save, start etcd, promote, start kube-apiserver",
		},
		{
			name: "learner already promoted", seed: seedWith(learner), st: recorded(),
			local: runningAs(2, false),
			want:  "start etcd, start kube-apiserver",
		},
		{
			// What the old voting add left behind on a timeout: a two-voter
			// cluster that cannot commit until this member starts. Removing it
			// cannot commit and wiping it throws away the vote; the only way
			// out is to start it, with whatever data it has.
			name: "unstarted voter the seed needs", seed: seedWith(unstartedVoter), st: controller(),
			local: servingAfter(2*time.Minute, 2, false), healthy: map[string]bool{"10.0.0.1": true},
			want: "save, start etcd, start kube-apiserver",
		},
		{
			name: "voter already rejoined", seed: seedWith(voter), st: recorded(),
			local: runningAs(2, false),
			want:  "start etcd, start kube-apiserver",
		},
		{
			// The documented "this member's data is gone, the cluster is
			// healthy" use: the other two voters are a quorum without it.
			name: "voter not serving, the others have quorum", seed: seedWith(voter, cp3), st: recorded(),
			local: servingAfter(0, addedID, true), healthy: map[string]bool{"10.0.0.1": true, "10.0.0.3": true},
			want: "remove 2, wipe, add-learner, save, start etcd, promote, start kube-apiserver",
		},
		{
			// The same three voters with one of the others down: this member's
			// vote is needed, so it is kept.
			name: "voter not serving, the others lack quorum", seed: seedWith(voter, cp3), st: recorded(),
			local: servingAfter(time.Minute, 2, false), healthy: map[string]bool{"10.0.0.1": true},
			want: "save, start etcd, start kube-apiserver",
		},
		{
			// This node's etcd is up and healthy but still serving the OLD
			// cluster, so it is not this member. Counting it among the healthy
			// others would make a two-voter cluster look like it has a quorum
			// without this node, and wipe the vote it is waiting on.
			name: "voter answering as the pre-restore cluster", seed: seedWith(voter), st: staleRecord(),
			local:   oldClusterUntilStarted(2, 2, false),
			healthy: map[string]bool{"10.0.0.1": true, "10.0.0.2": true},
			want:    "save, start etcd, start kube-apiserver",
		},
		{
			// A healthy LEARNER among the others is not a vote. Counting it
			// turns this two-voter cluster into an apparent three, and the
			// removal it authorises can never commit -- how the restore was bricked.
			// etcd allows one learner at a time, so a serial rejoin passes
			// through exactly this shape.
			name: "voter not serving, the only other healthy member is a learner",
			seed: seedWith(voter, cp3Learner), st: recorded(),
			local:   servingAfter(time.Minute, 2, false),
			healthy: map[string]bool{"10.0.0.1": true, "10.0.0.3": true},
			want:    "save, start etcd, start kube-apiserver",
		},
		{
			// A join's learner the controller had not promoted yet: the
			// rejoin promotes it and clears the pending record, so the
			// controller's learner task does not race it. The save changes
			// no field etcd's manifest is rendered from, so the serving
			// member is not restarted.
			name: "a joined learner still pending, serving as itself", seed: seedWith(learner), st: joinedPending(),
			local: runningAs(2, true),
			want:  "save, start etcd, promote, start kube-apiserver",
		},
		{
			name: "a joined learner still pending, never started", seed: seedWith(unstartedLearner), st: joinedPending(),
			local: servingAfter(0, addedID, true),
			want:  "remove 2, wipe, add-learner, save, start etcd, promote, start kube-apiserver",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRejoin(t, tc.seed, tc.st)
			rejoinMemberHealthy = rejoinHealthFrom(tc.seed, tc.healthy)
			f.local = tc.local
			if tc.running {
				f.running = func() bool { return true }
			}
			if tc.data {
				var n int64
				f.data = func(time.Duration) etcdDataMark { n++; return etcdDataMark{bytes: n} }
			}
			if err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false); err != nil {
				t.Fatalf("re-run: %v", err)
			}
			if got := f.steps(); got != tc.want {
				t.Errorf("steps = %q, want %q", got, tc.want)
			}
			if f.saved != nil && (f.saved.EtcdState != "existing" || !strings.Contains(f.saved.EtcdInitialCluster, "cp2="+selfPeer) ||
				f.saved.EtcdPromotePending) {
				t.Errorf("saved state = %+v", f.saved)
			}
			if tc.st.EtcdPromotePending && f.saved == nil {
				t.Error("the rejoin left the join's pending promote recorded")
			}
		})
	}

	t.Run("a needed voter that does not come up is never wiped, and the advice says so", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(unstartedVoter), controller())
		f.local = refused
		err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", false)
		if err == nil {
			t.Fatal("a voter that never came up was reported rejoined")
		}
		if got := f.steps(); got != "save, start etcd" {
			t.Errorf("steps = %q, want only save and start", got)
		}
		for _, want := range []string{"VOTING", "do not remove it or wipe", "without wiping or re-adding"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error lacks %q: %v", want, err)
			}
		}
	})
}

// TestRejoinRefusesWhenTheSeedIsThisNode: the seed reached under an address this node does not record.
func TestRejoinRefusesWhenTheSeedIsThisNode(t *testing.T) {
	self := etcdMember{ID: 2, Name: "cp2", PeerURLs: []string{selfPeer}, ClientURLs: []string{"https://10.0.0.2:2379"}}
	seed := etcdMembership{ClusterID: fakeCluster, Responder: 2, Quorum: true, Members: []etcdMember{self}}
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			f := newFakeRejoin(t, seed, controller())
			f.local = servingAfter(0, 2, false)
			f.running = func() bool { return true }
			err := RejoinEtcd(context.Background(), io.Discard, "192.168.50.2", force)
			if err == nil || !strings.Contains(err.Error(), "this node is the seed") ||
				!strings.Contains(err.Error(), "Nothing was changed") {
				t.Fatalf("err = %v; want the seed refusal", err)
			}
			if f.steps() != "" || f.saved != nil {
				t.Errorf("steps = %q, saved = %+v; want nothing changed", f.steps(), f.saved)
			}
		})
	}
}

// TestRejoinGuardsOnlyAWipeWithDataToLose: the wipe guards (proof that this
// node's own cluster lost quorum, or --force) stand before every wipe that could
// discard data this node may still be serving from, and nowhere else. A first
// run, and a replacement, over member data are guarded; either over a data dir
// an earlier run already emptied, and a resume, are not, so re-running after a
// partial run does not need --force.
func TestRejoinGuardsOnlyAWipeWithDataToLose(t *testing.T) {
	unstartedLearner := etcdMember{ID: 2, PeerURLs: []string{selfPeer}, IsLearner: true}
	learner := etcdMember{ID: 2, Name: "cp2", PeerURLs: []string{selfPeer},
		ClientURLs: []string{"https://10.0.0.2:2379"}, IsLearner: true}
	serving := func(time.Duration) (localEtcdStatus, error) {
		return localEtcdStatus{ClusterID: fakeCluster, MemberID: 2, Leader: 1, AppliedIndex: 1, IsLearner: true}, nil
	}
	for _, tc := range []struct {
		name      string
		seed      etcdMembership
		held      bool
		local     func(time.Duration) (localEtcdStatus, error)
		force     bool
		wantGuard bool
		wantWipe  bool
	}{
		{name: "first run", seed: seedWith(), held: true, wantGuard: true},
		{name: "first run, --force", seed: seedWith(), held: true, force: true, wantWipe: true},
		{name: "first run over an emptied data dir", seed: seedWith(), wantWipe: true},
		{name: "replace over member data", seed: seedWith(unstartedLearner), held: true, wantGuard: true},
		{name: "replace over member data, --force", seed: seedWith(unstartedLearner), held: true, force: true, wantWipe: true},
		{name: "replace over an emptied data dir", seed: seedWith(unstartedLearner), wantWipe: true},
		{name: "resume", seed: seedWith(learner), held: true, local: serving},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRejoin(t, tc.seed, controller())
			if tc.local != nil {
				f.local = tc.local
			} else {
				f.local = servingAfter(0, addedID, true)
			}
			rejoinHoldsMemberData = func() (bool, error) { return tc.held, nil }
			guarded := false
			rejoinConfirmQuorumLost = func() error {
				guarded = true
				return errors.New("this node's etcd cluster still has quorum")
			}
			err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", tc.force)
			wiped := strings.Contains(f.steps(), "wipe")
			if tc.wantGuard && f.steps() != "" {
				t.Errorf("a refused rejoin still ran %q", f.steps())
			}
			if tc.wantGuard {
				if err == nil || !guarded || !strings.Contains(err.Error(), "re-run with --force") {
					t.Fatalf("err = %v, guarded = %v; want the quorum refusal", err, guarded)
				}
			} else if err != nil {
				t.Fatalf("rejoin: %v", err)
			}
			if guarded != tc.wantGuard && !tc.force {
				t.Errorf("quorum check ran = %v, want %v", guarded, tc.wantGuard)
			}
			if tc.force && guarded {
				t.Error("--force still asked for the quorum proof")
			}
			if wiped != tc.wantWipe {
				t.Errorf("wiped = %v, want %v (steps %q)", wiped, tc.wantWipe, f.steps())
			}
		})
	}
	t.Run("a data dir that cannot be read refuses the replacement", func(t *testing.T) {
		f := newFakeRejoin(t, seedWith(unstartedLearner), controller())
		f.local = servingAfter(0, addedID, true)
		rejoinHoldsMemberData = func() (bool, error) { return true, errors.New("EACCES") }
		err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", true)
		if err == nil || !strings.Contains(err.Error(), "left alone") || f.steps() != "" {
			t.Fatalf("err = %v, steps %q; want a refusal before the wipe", err, f.steps())
		}
	})
}

func TestInitialClusterFor(t *testing.T) {
	ms := []etcdMember{seedMember, {ID: 0xab, PeerURLs: []string{"https://10.0.0.3:2380"}},
		{ID: 2, PeerURLs: []string{selfPeer}}}
	got := initialClusterFor(ms, selfPeer, "cp2")
	want := "cp1=https://10.0.0.1:2380,unstarted-ab=https://10.0.0.3:2380,cp2=" + selfPeer
	if got != want {
		t.Errorf("initialClusterFor = %q, want %q", got, want)
	}
	if !sameInitialCluster("cp2="+selfPeer+",cp1=https://10.0.0.1:2380", []etcdMember{seedMember, ms[2]}, selfPeer, "cp2") {
		t.Error("the same members in another order were called different")
	}
	if sameInitialCluster("cp2="+selfPeer, []etcdMember{seedMember, ms[2]}, selfPeer, "cp2") {
		t.Error("a recorded cluster missing the seed was called the same")
	}
	if sameInitialCluster("other="+selfPeer+",cp1=https://10.0.0.1:2380", []etcdMember{seedMember, ms[2]}, selfPeer, "cp2") {
		t.Error("a recorded name for this node that etcd would refuse was called the same")
	}
}

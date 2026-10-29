package cluster

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ctrliq/stratum/internal/ha"
)

// joinedLearnerState is a control plane a join just added as a learner.
func joinedLearnerState() *NodeState {
	st := seedState()
	st.Hostname, st.NodeIP, st.EtcdName = "cp2", "10.0.0.2", "cp2"
	st.EtcdState, st.EtcdInitialCluster, st.EtcdPromotePending = "existing", grownThree, true
	return st
}

// stubLearnerPromotion saves st as this node's state and points the learner
// task's seams at answer and a lock that is free unless held is set. It
// returns the number of promote attempts made.
func stubLearnerPromotion(t *testing.T, st *NodeState, held *bool, answer func() (ha.LearnerState, error)) *int {
	t.Helper()
	redirectStateArea(t)
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}
	oldPromote, oldLock := promoteLocalLearner, lockEtcdData
	t.Cleanup(func() { promoteLocalLearner, lockEtcdData = oldPromote, oldLock })
	lockEtcdData = func() (func(), error) {
		if held != nil && *held {
			return nil, errors.New("etcd data lock: another operation is changing the data dir")
		}
		return func() {}, nil
	}
	var attempts int
	promoteLocalLearner = func(nodeIP, initialCluster string) (ha.LearnerState, error) {
		attempts++
		if nodeIP != st.NodeIP || initialCluster != st.EtcdInitialCluster {
			t.Errorf("promote asked about %s in %q, want this node's %s in %q", nodeIP, initialCluster, st.NodeIP, st.EtcdInitialCluster)
		}
		return answer()
	}
	return &attempts
}

func pendingOnDisk(t *testing.T) bool {
	t.Helper()
	st, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	return st.EtcdPromotePending
}

// TestPromoteLearnerOnceClearsThePendingRecordOnlyOnceAVoter pins one look of
// the controller's learner task: it keeps the pending record while the learner
// waits or etcd refuses, and clears it once the member is a voter, whether this
// look promoted it or an earlier one whose answer was lost did.
func TestPromoteLearnerOnceClearsThePendingRecordOnlyOnceAVoter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       ha.LearnerState
		err         error
		wantDone    bool
		wantPending bool
	}{
		{"a learner still catching up", ha.LearnerWaiting, nil, false, true},
		{"a refused promote", ha.LearnerWaiting, errors.New("promote etcd learner c2: permission denied"), false, true},
		{"a promoted learner", ha.LearnerPromoted, nil, true, false},
		{"already a voter", ha.MemberIsVoter, nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := stubLearnerPromotion(t, joinedLearnerState(), nil, func() (ha.LearnerState, error) { return tc.state, tc.err })
			var warned string
			if done := promoteLearnerOnce(&warned); done != tc.wantDone {
				t.Errorf("done = %v, want %v", done, tc.wantDone)
			}
			if *attempts != 1 {
				t.Errorf("promote attempts = %d, want 1", *attempts)
			}
			if got := pendingOnDisk(t); got != tc.wantPending {
				t.Errorf("EtcdPromotePending on disk = %v, want %v", got, tc.wantPending)
			}
			if (warned != "") != (tc.err != nil) {
				t.Errorf("warned = %q for error %v", warned, tc.err)
			}
		})
	}
}

// TestPromoteLearnerOnceLeavesARejoinsLearnerAlone: a snapshot rejoin holds
// the etcd data lock while it replaces this node's member and promotes its own
// learner, and clears the pending record, so the task neither promotes under a
// held lock nor promotes a member nothing records pending.
func TestPromoteLearnerOnceLeavesARejoinsLearnerAlone(t *testing.T) {
	t.Run("the data lock is held", func(t *testing.T) {
		held := true
		attempts := stubLearnerPromotion(t, joinedLearnerState(), &held, func() (ha.LearnerState, error) { return ha.LearnerPromoted, nil })
		var warned string
		if promoteLearnerOnce(&warned) || *attempts != 0 || !pendingOnDisk(t) {
			t.Fatalf("a held lock must skip the look: attempts = %d, pending = %v", *attempts, pendingOnDisk(t))
		}
	})
	t.Run("nothing is pending", func(t *testing.T) {
		st := joinedLearnerState()
		st.EtcdPromotePending = false
		attempts := stubLearnerPromotion(t, st, nil, func() (ha.LearnerState, error) { return ha.LearnerPromoted, nil })
		var warned string
		if !promoteLearnerOnce(&warned) || *attempts != 0 {
			t.Fatalf("a member nothing records pending is not the task's: attempts = %d", *attempts)
		}
	})
}

// TestPromoteLearnerRetriesUntilPromoted drives the task: it looks again every
// poll, through refusals, until the learner is promoted, and then holds until
// shutdown without looking again.
func TestPromoteLearnerRetriesUntilPromoted(t *testing.T) {
	answers := []error{errors.New("refused"), errors.New("refused"), nil}
	attempts := stubLearnerPromotion(t, joinedLearnerState(), nil, func() (ha.LearnerState, error) {
		return ha.LearnerWaiting, nil
	})
	n := 0
	promoteLocalLearner = func(string, string) (ha.LearnerState, error) {
		*attempts++
		if n < len(answers)-1 {
			n++
			return ha.LearnerWaiting, answers[n-1]
		}
		return ha.LearnerPromoted, nil
	}
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { promoteLearner(ctx); close(done) }()
		time.Sleep(10 * learnerPromotePoll)
		synctest.Wait()
		if *attempts != len(answers) {
			t.Errorf("promote attempts = %d, want %d: one per poll until promoted, none after", *attempts, len(answers))
		}
		if pendingOnDisk(t) {
			t.Error("the pending record survived the promote")
		}
		cancel()
		<-done
	})
}

// TestPostStartPromotesOnlyARecordedLearner: the learner task runs on a
// control plane whose node state records a pending promote, and on no other.
func TestPostStartPromotesOnlyARecordedLearner(t *testing.T) {
	fakeHostKubectl(t)
	cfg := loadClusterConfig(t, "spec: {}\n")
	for _, pending := range []bool{true, false} {
		st := joinedLearnerState()
		st.EtcdPromotePending = pending
		stubLoops(t, time.Hour)
		attempts := stubLearnerPromotion(t, st, nil, func() (ha.LearnerState, error) { return ha.LearnerWaiting, nil })
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			g := newRunGroup(ctx)
			postStart(g, cfg, st)
			time.Sleep(learnerPromotePoll / 2)
			synctest.Wait()
			if got := *attempts > 0; got != pending {
				t.Errorf("pending = %v: promote attempts = %d", pending, *attempts)
			}
			cancel()
			g.wait()
		})
	}
}

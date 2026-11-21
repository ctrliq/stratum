package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

const (
	seedMember = "cp1=https://10.0.0.1:2380"
	grownThree = seedMember + ",cp2=https://10.0.0.2:2380,cp3=https://10.0.0.3:2380"
)

func seedState() *NodeState {
	return &NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.1", KubeletNodeIP: "10.0.0.1",
		EtcdName: "cp1", EtcdState: "new", EtcdInitialCluster: seedMember,
		PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16"}
}

func TestWithPeers(t *testing.T) {
	seed := seedState()
	got := withPeers(seed, 3)
	if got == nil || !got.EtcdHasPeers || got.EtcdState != "new" || got.EtcdInitialCluster != seedMember {
		t.Fatalf("grown seed = %+v, want etcdHasPeers with state and membership untouched", got)
	}
	if seed.EtcdHasPeers {
		t.Error("the input state was modified in place")
	}

	recorded := seedState()
	recorded.EtcdHasPeers = true
	for _, tc := range []struct {
		name    string
		st      *NodeState
		members int
	}{
		{"still a single member", seedState(), 1},
		{"already recorded", recorded, 3},
		{"a joined member", &NodeState{Role: "controller", EtcdName: "cp2", EtcdState: "existing", EtcdInitialCluster: grownThree}, 3},
		{"a worker", &NodeState{Role: "worker", EtcdState: "new"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := withPeers(tc.st, tc.members); got != nil {
				t.Fatalf("withPeers = %+v, want no change", got)
			}
		})
	}
}

func growthFixture(t *testing.T) {
	t.Helper()
	oldDir, oldData, oldRoot, oldPoll, oldCount := version.K8sConfDir, version.EtcdDataDir, version.DataDir, etcdGrowthPoll, localEtcdMemberCount
	t.Cleanup(func() {
		version.K8sConfDir, version.EtcdDataDir, version.DataDir, etcdGrowthPoll, localEtcdMemberCount = oldDir, oldData, oldRoot, oldPoll, oldCount
	})
	root := t.TempDir()
	version.K8sConfDir, version.DataDir, version.EtcdDataDir = filepath.Join(root, "config"), filepath.Join(root, "data"), filepath.Join(root, "etcd")
	etcdGrowthPoll = time.Millisecond
	if err := SaveState(seedState()); err != nil {
		t.Fatal(err)
	}
}

func TestRecordEtcdGrowthPersistsOnceTheClusterGrows(t *testing.T) {
	growthFixture(t)
	answers := []func() (int, error){
		func() (int, error) { return 0, errors.New("etcd not up") },
		func() (int, error) { return 1, nil },
		func() (int, error) { return 3, nil },
	}
	calls := 0
	localEtcdMemberCount = func() (int, error) { calls++; return answers[min(calls, len(answers))-1]() }

	done := make(chan struct{})
	go func() { recordEtcdGrowth(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recordEtcdGrowth did not finish once the cluster had grown")
	}
	st, err := LoadState()
	if err != nil || !st.EtcdHasPeers {
		t.Fatalf("saved state = %+v, %v; want etcdHasPeers", st, err)
	}
}

func TestSaveGrowthWritesNothingWhileEtcdHasNotGrown(t *testing.T) {
	growthFixture(t)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(nodestate.StatePath(), past, past); err != nil {
		t.Fatal(err)
	}

	if saveGrowth(1) {
		t.Fatal("saveGrowth recorded growth from a single-member etcd")
	}
	if fi, err := os.Stat(nodestate.StatePath()); err != nil || fi.ModTime().After(time.Now().Add(-time.Minute)) {
		t.Errorf("a no-record pass rewrote node.yaml (mtime %v, err %v)", fi.ModTime(), err)
	}

	rejoined := seedState()
	rejoined.EtcdState = "existing"
	rejoined.EtcdInitialCluster = seedMember + ",cp4=https://10.0.0.4:2380"
	if err := SaveState(rejoined); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(nodestate.StatePath(), past, past); err != nil {
		t.Fatal(err)
	}
	if !saveGrowth(3) {
		t.Fatal("saveGrowth did not finish once the state was recorded")
	}
	if fi, err := os.Stat(nodestate.StatePath()); err != nil || fi.ModTime().After(time.Now().Add(-time.Minute)) {
		t.Errorf("a pass that only read the record rewrote node.yaml (mtime %v, err %v)", fi.ModTime(), err)
	}
	st, err := LoadState()
	if err != nil || st.EtcdState != "existing" || st.EtcdInitialCluster != rejoined.EtcdInitialCluster {
		t.Fatalf("node.yaml = %+v, %v; want the rejoin's record untouched", st, err)
	}
}

func TestRecordEtcdGrowthDoesNotOverwriteARejoin(t *testing.T) {
	growthFixture(t)
	rejoined := seedState()
	rejoined.EtcdState = "existing"
	rejoined.EtcdInitialCluster = seedMember + ",cp4=https://10.0.0.4:2380"
	first := true
	localEtcdMemberCount = func() (int, error) {
		if first { // the rejoin lands while the poll is waiting on etcd
			first = false
			if err := SaveState(rejoined); err != nil {
				t.Error(err)
			}
			return 3, nil
		}
		return 0, errors.New("the loop must stop once state is existing")
	}
	recordEtcdGrowth(context.Background())

	st, err := LoadState()
	if err != nil || st.EtcdState != "existing" || st.EtcdInitialCluster != rejoined.EtcdInitialCluster || st.EtcdHasPeers {
		t.Fatalf("saved state = %+v, %v; want the rejoin's record kept as it was", st, err)
	}
}

func TestRecordEtcdGrowthWaitsOutARunningRejoin(t *testing.T) {
	growthFixture(t)
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatal(err)
	}
	passes := 0
	localEtcdMemberCount = func() (int, error) {
		passes++
		if passes == 3 {
			if st, err := LoadState(); err != nil || st.EtcdHasPeers {
				t.Errorf("state written while the lock was held: %+v, %v", st, err)
			}
			release()
		}
		return 2, nil
	}
	recordEtcdGrowth(context.Background())

	if passes != 3 {
		t.Errorf("recorded on pass %d, want 3: the lock was held through passes 1 and 2", passes)
	}
	if st, err := LoadState(); err != nil || !st.EtcdHasPeers {
		t.Fatalf("saved state = %+v, %v; want etcdHasPeers", st, err)
	}
}

func TestRecordedGrowthDoesNotChangeTheEtcdManifest(t *testing.T) {
	growthFixture(t)
	st := seedState()
	if err := staticpod.WriteAll(st); err != nil {
		t.Fatal(err)
	}
	before, err := staticpod.Read("etcd")
	if err != nil {
		t.Fatal(err)
	}

	if !saveGrowth(2) {
		t.Fatal("the growth was not recorded")
	}
	after, err := LoadState()
	if err != nil || !after.EtcdHasPeers {
		t.Fatalf("saved state = %+v, %v", after, err)
	}
	if err := staticpod.WriteAll(after); err != nil {
		t.Fatal(err)
	}
	got, err := staticpod.Read("etcd")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(before) {
		t.Fatalf("recording growth changed the etcd manifest, so the next controller start restarts etcd:\n--- before\n%s\n--- after\n%s", before, got)
	}
}

// Dropping the record at PrepareController would turn the startup guard off after one restart.
func TestKeepEtcdMembershipCarriesTheGrowthRecord(t *testing.T) {
	prev := seedState()
	prev.EtcdHasPeers = true
	fresh := &NodeState{Role: "controller", EtcdName: "cp1", EtcdState: "new", EtcdInitialCluster: "other"}
	keepEtcdMembership(fresh, prev)
	if !fresh.EtcdHasPeers || fresh.EtcdInitialCluster != seedMember {
		t.Fatalf("fresh = %+v, want the saved membership and etcdHasPeers kept", fresh)
	}
	first := &NodeState{EtcdName: "cp1", EtcdState: "new", EtcdInitialCluster: seedMember}
	keepEtcdMembership(first, nil)
	if first.EtcdHasPeers {
		t.Error("a first boot recorded peers")
	}
}

// Dropping the record leaves the member a learner; etcd's one-learner limit refuses joins.
func TestKeepEtcdMembershipCarriesAPendingPromote(t *testing.T) {
	prev := seedState()
	prev.EtcdState, prev.EtcdPromotePending = "existing", true
	fresh := &NodeState{Role: "controller", EtcdName: "cp1", EtcdState: "new", EtcdInitialCluster: "other"}
	keepEtcdMembership(fresh, prev)
	if !fresh.EtcdPromotePending {
		t.Fatalf("fresh = %+v, want the pending promote kept", fresh)
	}
}

// SaveState replaces the file; a writer from a fresh NodeState drops fields it does not own.
func TestNodeStateWritersKeepFieldsTheyDoNotOwn(t *testing.T) {
	growthFixture(t)
	seed := seedState()
	seed.Server = "https://10.0.0.1:6443"
	seed.ConfigPath = "/srv/stratum.yaml"
	seed.ClusterDomain = "a.internal"
	seed.ClusterDNSIP = "10.43.0.10"
	seed.VIP = VIP{Address: "10.0.0.254", Interface: "eth0", BGP: true}
	seed.CIS = true
	seed.DualStack = true
	seed.EtcdPromotePending = true

	fields := []struct {
		name string
		get  func(*NodeState) any
	}{
		{"role", func(s *NodeState) any { return s.Role }},
		{"hostname", func(s *NodeState) any { return s.Hostname }},
		{"nodeIP", func(s *NodeState) any { return s.NodeIP }},
		{"kubeletNodeIP", func(s *NodeState) any { return s.KubeletNodeIP }},
		{"server", func(s *NodeState) any { return s.Server }},
		{"etcdName", func(s *NodeState) any { return s.EtcdName }},
		{"etcdInitialCluster", func(s *NodeState) any { return s.EtcdInitialCluster }},
		{"etcdState", func(s *NodeState) any { return s.EtcdState }},
		{"dualStack", func(s *NodeState) any { return s.DualStack }},
		{"podCIDR", func(s *NodeState) any { return s.PodCIDR }},
		{"serviceCIDR", func(s *NodeState) any { return s.ServiceCIDR }},
		{"clusterDNSIP", func(s *NodeState) any { return s.ClusterDNSIP }},
		{"clusterDomain", func(s *NodeState) any { return s.ClusterDomain }},
		{"cis", func(s *NodeState) any { return s.CIS }},
		{"vip", func(s *NodeState) any { return s.VIP }},
		{"etcdHasPeers", func(s *NodeState) any { return s.EtcdHasPeers }},
		{"etcdPromotePending", func(s *NodeState) any { return s.EtcdPromotePending }},
		{"configPath", func(s *NodeState) any { return s.ConfigPath }},
	}

	for _, tc := range []struct {
		name string
		own  string
		run  func(t *testing.T, st *NodeState)
	}{
		{"saveGrowth", "etcdHasPeers", func(t *testing.T, _ *NodeState) {
			if !saveGrowth(3) {
				t.Fatal("saveGrowth recorded nothing")
			}
		}},
		{"promoteLearnerOnce", "etcdPromotePending", func(t *testing.T, _ *NodeState) {
			old, oldConfirm := promoteLocalLearner, confirmLocalVoter
			t.Cleanup(func() { promoteLocalLearner, confirmLocalVoter = old, oldConfirm })
			promoteLocalLearner = func(string, string) (ha.LearnerState, error) { return ha.LearnerPromoted, nil }
			confirmLocalVoter = func(string) error { return nil }
			var look learnerLook
			if !promoteLearnerOnce(&look) {
				t.Fatal("promoteLearnerOnce recorded nothing")
			}
		}},
		{"recordConfigPath", "configPath", func(t *testing.T, st *NodeState) {
			cfg := &ClusterConfig{path: "/srv/other.yaml"}
			if err := recordConfigPath(cfg, st, SaveState); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := SaveState(seed); err != nil {
				t.Fatal(err)
			}
			st, err := LoadState()
			if err != nil {
				t.Fatal(err)
			}
			tc.run(t, st)

			got, err := LoadState()
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range fields {
				want, have := f.get(seed), f.get(got)
				if f.name == tc.own {
					if want == have {
						t.Errorf("the writer's own field %s did not change: %v", f.name, have)
					}
					continue
				}
				if want != have {
					t.Errorf("field %s = %v, want the seeded %v: the writer dropped a field it does not own", f.name, have, want)
				}
			}
		})
	}
}

// The prepared copy is stale once a rejoin lands mid-start; the save must re-read under the lock.
func TestRecordConfigPathKeepsAStateWrittenAfterPrepare(t *testing.T) {
	growthFixture(t)
	prepared := seedState()
	if err := SaveState(prepared); err != nil {
		t.Fatal(err)
	}
	restored := seedState()
	restored.EtcdState = "existing"
	restored.EtcdInitialCluster = seedMember + ",cp4=https://10.0.0.4:2380"
	restored.EtcdHasPeers = true
	if err := SaveState(restored); err != nil {
		t.Fatal(err)
	}

	if err := recordConfigPath(&ClusterConfig{path: "/srv/stratum.yaml"}, prepared, SaveState); err != nil {
		t.Fatalf("recordConfigPath = %v", err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if got.EtcdState != "existing" || got.EtcdInitialCluster != restored.EtcdInitialCluster || !got.EtcdHasPeers {
		t.Fatalf("node.yaml = %+v; the restored membership was overwritten by the prepared copy", got)
	}
	if got.ConfigPath != "/srv/stratum.yaml" {
		t.Errorf("configPath = %q, want the recorded path", got.ConfigPath)
	}
}

// The save is best-effort; RunController only warns and the next start records the path again.
func TestRecordConfigPathRefusesWhileTheDataLockIsHeld(t *testing.T) {
	growthFixture(t)
	restored := seedState()
	restored.EtcdState = "existing"
	restored.EtcdInitialCluster = seedMember + ",cp4=https://10.0.0.4:2380"
	restored.EtcdHasPeers = true
	if err := SaveState(restored); err != nil {
		t.Fatal(err)
	}
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := recordConfigPath(&ClusterConfig{path: "/srv/stratum.yaml"}, seedState(), SaveState); err == nil {
		t.Fatal("recordConfigPath wrote while another operation held the data lock")
	}
	got, err := LoadState()
	if err != nil || got.EtcdState != "existing" || got.EtcdInitialCluster != restored.EtcdInitialCluster || !got.EtcdHasPeers {
		t.Fatalf("node.yaml = %+v, %v; want the restored state untouched", got, err)
	}
}

// The learner's save must carry a field another writer recorded while the start ran.
func TestWorkerDNSLearnerKeepsAStateWrittenWhileItRan(t *testing.T) {
	growthFixture(t)
	st := &NodeState{Role: "worker", Hostname: "w1", NodeIP: "192.0.2.8", KubeletNodeIP: "192.0.2.8",
		Server: "https://10.0.0.1:6443"}
	if err := SaveState(st); err != nil {
		t.Fatal(err)
	}

	oldFetch, oldDNS, oldKubelet := fetchClusterDNSIP, writeCoreDNSUpstream, writeKubeletConf
	t.Cleanup(func() { fetchClusterDNSIP, writeCoreDNSUpstream, writeKubeletConf = oldFetch, oldDNS, oldKubelet })
	// An address different from the built-in default, so the learner records it.
	fetchClusterDNSIP = func() (string, error) { return "10.96.0.11", nil }
	writeCoreDNSUpstream = func(*NodeState) error { return nil }
	writeKubeletConf = func(*NodeState) error {
		// Another node.yaml writer lands while this start renders its configs.
		cur, err := LoadState()
		if err != nil {
			return err
		}
		cur.ConfigPath = "/srv/stratum.yaml"
		return SaveState(cur)
	}

	if err := prepareWorkerKubelet(st); err != nil {
		t.Fatalf("prepareWorkerKubelet = %v", err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if got.ClusterDNSIP != "10.96.0.11" {
		t.Errorf("clusterDNSIP = %q, want the learned address", got.ClusterDNSIP)
	}
	if got.ConfigPath != "/srv/stratum.yaml" {
		t.Errorf("configPath = %q, want the field the other writer recorded — the learner's save dropped it", got.ConfigPath)
	}
}

// The recorder needs only this node's etcd, so it starts without the apiserver wait.
func TestPostStartStartsTheGrowthRecorderWithoutTheAPI(t *testing.T) {
	oldStart := startEtcdGrowthRecorder
	t.Cleanup(func() { startEtcdGrowthRecorder = oldStart })
	var started atomic.Int32
	fakeHostKubectl(t)
	// Safety net: this stub keeps a real recorder off the host's /var/lib/stratum.
	growthFixture(t)
	localEtcdMemberCount = func() (int, error) { return 0, errors.New("no etcd in this test") }

	cfg := loadClusterConfig(t, "spec:\n  certRenew: false\n  snapshots:\n    disabled: true\n")
	recorded := seedState()
	recorded.EtcdHasPeers = true
	joined := seedState()
	joined.EtcdState = "existing"
	for _, tc := range []struct {
		name string
		st   *NodeState
		want int32
	}{
		{"a seed that has not recorded peers", seedState(), 1},
		{"a seed that already recorded them", recorded, 0},
		{"a joined member", joined, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started.Store(0)
			// A channel created outside the synctest bubble never durably blocks; synctest.Wait then hangs.
			var release chan struct{}
			startEtcdGrowthRecorder = func(context.Context) { started.Add(1); <-release }
			synctest.Test(t, func(t *testing.T) {
				release = make(chan struct{})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				g := newRunGroup(ctx)
				postStart(g, cfg, tc.st)

				synctest.Wait()
				if got := started.Load(); got != tc.want {
					t.Fatalf("growth recorder started %d times, want %d", got, tc.want)
				}

				waitDone := make(chan struct{})
				go func() { g.wait(); close(waitDone) }()
				synctest.Wait()
				if tc.want == 1 {
					select {
					case <-waitDone:
						t.Fatal("the run group's wait returned while the recorder was still running: the recorder is not a run-group task")
					default:
					}
				}

				cancel()
				close(release)
				select {
				case <-waitDone:
				case <-time.After(time.Minute):
					t.Fatal("the recorder task never stopped")
				}
			})
		})
	}
}

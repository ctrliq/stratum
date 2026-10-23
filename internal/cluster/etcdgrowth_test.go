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

// TestWithPeers covers the seed's half of HA growth: HA joins write membership
// only on the joining node, so the seed's saved state kept state=new with itself
// as the only member for the life of the cluster. A replacement started from a
// backup of that file looks like a sole controller and bootstraps an empty etcd
// beside the survivors. The seed must record the growth it can see in its own
// etcd, without touching the fields etcd's argv is rendered from.
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

// growthFixture points node.yaml and the etcd data lock at temp dirs and saves
// a seed that still records state=new.
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

// TestRecordEtcdGrowthPersistsOnceTheClusterGrows drives the loop: errors from
// etcd and a still-single cluster leave node.yaml alone, and the pass that sees
// three members records it and ends the loop.
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

// TestSaveGrowthWritesNothingWhileEtcdHasNotGrown pins the no-record pass:
// the recorder polls every minute for as long as this seed stays an ungrown
// singleton, and a pass that finds nothing to record must leave node.yaml
// alone — not rewrite it with the bytes it just read. The mtime is forced
// into the past first, so any save moves it forward. Fails if the
// under-lock mutation owner saves the unchanged state on a no-record pass.
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

	// A pass that sees the record already present — a rejoin saved it between
	// the poll's own unlocked read and its locked re-read — ends the loop and
	// also writes nothing: the state on disk already carries the record.
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

// TestRecordEtcdGrowthDoesNotOverwriteARejoin: the poll reads
// node.yaml, asks etcd, then saves. A rejoin of the seed that saves its new
// membership in between must not be replaced by the older record the poll read.
// The save runs under the etcd data lock, against freshly read state.
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

// TestRecordEtcdGrowthWaitsOutARunningRejoin: while another operation holds the
// etcd data lock the poll writes nothing, and records the growth on a later pass.
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

// TestRecordedGrowthDoesNotChangeTheEtcdManifest is the inttest regression
// (ha-cluster-upgrade): etcd's argv carries --initial-cluster and
// --initial-cluster-state, so rewriting them in node.yaml changed the seed's
// etcd manifest at its next controller restart. kubelet then recreated etcd-cp1
// in the middle of the cluster upgrade, taking cp1's apiserver unready while the
// orchestrator on cp1 was renewing its lease through the VIP. A controller
// restart must not bounce static pods (staticpod.WriteAll), so recording growth
// must leave every rendered manifest alone, and PrepareController must carry the
// record forward.
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
	// What the next controller start does.
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

// TestKeepEtcdMembershipCarriesTheGrowthRecord: PrepareController builds a fresh
// state on every start and copies the etcd record from the saved one. Dropping
// etcdHasPeers there would turn the startup guard off again after one restart.
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

// TestNodeStateWritersKeepFieldsTheyDoNotOwn is the write rule for node.yaml:
// SaveState replaces the whole file, so a writer that applies its change to a
// fresh NodeState instead of one read back from disk drops every field another
// writer recorded — the lost-update shape. Each writer a unit test can reach
// runs against a state with every field set, and must change only the field it
// owns. Bring-up's save is deliberately absent from the table: it owns nearly
// every field and copies them onto the state read back under the lock, so no
// single-own-field row can pin it — TestPrepareKeepsFieldsAnotherWriterRecordedMidStart
// and TestPrepareDoesNotResurrectOwnedFieldsItClears pin it instead.
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
		{"configPath", func(s *NodeState) any { return s.ConfigPath }},
	}

	for _, tc := range []struct {
		name string
		own  string // the one field the writer may change; empty for none
		run  func(t *testing.T, st *NodeState)
	}{
		{"saveGrowth", "etcdHasPeers", func(t *testing.T, _ *NodeState) {
			if !saveGrowth(3) {
				t.Fatal("saveGrowth recorded nothing")
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

// TestRecordConfigPathKeepsAStateWrittenAfterPrepare pins the controller-run
// writer's side of the one-mutation-owner contract.
// recordConfigPath runs at the top of RunController, after `stratum up`
// released the data lock (pkg/cli/up.go releases it before runControllerFn),
// and the state it is handed is the copy PrepareController built — stale the
// moment a restore or a rejoin of this node lands while the start runs. The
// save must re-read under the lock and carry that writer's record, not
// overwrite it with the prepared copy. Fails if recordConfigPath
// saves the caller's copy, dropping the restored membership — the
// risk of bootstrapping an empty etcd beside survivors.
func TestRecordConfigPathKeepsAStateWrittenAfterPrepare(t *testing.T) {
	growthFixture(t)
	prepared := seedState()
	if err := SaveState(prepared); err != nil {
		t.Fatal(err)
	}
	// A restore or a rejoin of this node lands while the lock is released:
	// it records the restored membership, the record a bare start must not
	// lose.
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

// TestRecordConfigPathRefusesWhileTheDataLockIsHeld pins the lock half of the
// same contract: the controller-run writer's save takes the data lock itself,
// so while a restore or a rejoin holds it the save refuses — it is
// best-effort (RunController only warns), and the next start records the
// path again. Writing between that operation's read and write is the
// lost-update the owner exists to remove.
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

// TestWorkerDNSLearnerKeepsAStateWrittenWhileItRan pins the worker's other
// node.yaml write: prepareWorkerKubelet saves the cluster DNS address its
// learner picked up, and the state it saves is the copy the start loaded when
// it began. A writer that changed node.yaml in between must survive the save.
// On a control plane the interleaving writer is a rejoin or a restore; the
// configPath write below stands in for any field another writer records. The
// contract, not a regression, is what this pins: the save must carry what it
// did not write. Fails if the save writes the caller's copy and
// drops the other writer's field.
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

// TestPostStartStartsTheGrowthRecorderWithoutTheAPI: the recorder needs only
// this node's own etcd, not the apiserver, so a seed whose apiserver is down
// while a second member joins still records the growth — otherwise its
// node.yaml keeps looking like a safe singleton backup. postStart hands the
// group the recorder task (startEtcdGrowthRecorder), with no apiserver wait
// in front of it, so the pin watches what the group starts — and
// that the group owns it: the stub parks like a real loop until released, so
// the counter reads exactly the group's starts, and the group's wait must
// outlive the parked task, which a stray goroutine started by postStart
// would not. The ungrown seed must get its recorder despite the apiserver
// never becoming ready; a seed that already recorded and a joined member
// must not.
func TestPostStartStartsTheGrowthRecorderWithoutTheAPI(t *testing.T) {
	oldStart := startEtcdGrowthRecorder
	t.Cleanup(func() { startEtcdGrowthRecorder = oldStart })
	var started atomic.Int32
	fakeHostKubectl(t)
	// Safety net, not the pin: if the seam stops intercepting the recorder,
	// the real one runs against these temp paths and this stub's error keeps
	// it from writing anything, instead of the host's /var/lib/stratum.
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
			// Parks until released, not on the context, so the group's wait
			// visibly outlives the task: an early-returning run-group task
			// would be restarted, and a stray goroutine would leave wait
			// free to return without it. The channel itself is created
			// inside the bubble below: a task parked on a channel created
			// outside it is never durably blocked, and synctest.Wait hangs.
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
				// A fake minute is far more than the parked stub needs to
				// return and the group to join it; a recorder task that
				// never stops must fail, not hang the test.
				select {
				case <-waitDone:
				case <-time.After(time.Minute):
					t.Fatal("the recorder task never stopped")
				}
			})
		})
	}
}

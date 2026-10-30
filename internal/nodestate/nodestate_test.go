package nodestate

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/version"
	"sigs.k8s.io/yaml"
)

func TestEtcdPeerURL(t *testing.T) {
	for _, tc := range []struct{ ip, want string }{
		{"192.0.2.9", "https://192.0.2.9:2380"},
		{"fd00::9", "https://[fd00::9]:2380"},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			got := EtcdPeerURL(tc.ip)
			if got != tc.want {
				t.Fatalf("peer URL = %q, want %q", got, tc.want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatal(err)
			}
			host, port, err := net.SplitHostPort(u.Host)
			if err != nil || host != tc.ip || port != "2380" {
				t.Fatalf("peer host/port = %q/%q, error %v", host, port, err)
			}
		})
	}
}

// TestEtcdClientURL pins the client-URL builder beside the peer-URL one: the
// URL etcd advertises (static-pod args) and the one the HA machinery dials
// must come from one builder, so member lookups and health checks cannot
// drift from what the member serves. An IPv6 host must be bracketed here
// exactly as in the peer URL — concatenating reads the port as another
// hextet of the address.
func TestEtcdClientURL(t *testing.T) {
	for _, tc := range []struct{ ip, want string }{
		{"192.0.2.9", "https://192.0.2.9:2379"},
		{"fd00::9", "https://[fd00::9]:2379"},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			got := EtcdClientURL(tc.ip)
			if got != tc.want {
				t.Fatalf("client URL = %q, want %q", got, tc.want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatal(err)
			}
			host, port, err := net.SplitHostPort(u.Host)
			if err != nil || host != tc.ip || port != "2379" {
				t.Fatalf("client host/port = %q/%q, error %v", host, port, err)
			}
		})
	}
}

// TestEtcdHasPeersIsOmittedUntilSet: the field is a bool recorded once, so an
// unset one must not appear in every node.yaml (omitzero, per modern-go).
func TestEtcdHasPeersIsOmittedUntilSet(t *testing.T) {
	b, err := yaml.Marshal(&NodeState{Role: "controller"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "etcdHasPeers") {
		t.Errorf("unset field written:\n%s", b)
	}
	b, err = yaml.Marshal(&NodeState{Role: "controller", EtcdHasPeers: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "etcdHasPeers: true") {
		t.Errorf("set field not written:\n%s", b)
	}
}

// The node facts round-trip through node.yaml and are omitted when unset, so
// a node that declares none writes the file it always did.
func TestNodeFactsRoundTrip(t *testing.T) {
	withStateAndLockPaths(t)
	b, err := yaml.Marshal(&NodeState{Role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"nodeLabels", "nodeTaints", "runtimeHandlers"} {
		if strings.Contains(string(b), key) {
			t.Errorf("unset %s written:\n%s", key, b)
		}
	}
	want := &NodeState{Role: "worker", Hostname: "w1",
		NodeLabels:      map[string]string{"topology.kubernetes.io/zone": "rack-1"},
		NodeTaints:      []string{"stratum.ciq.com/unverified=true:NoSchedule"},
		RuntimeHandlers: map[string]string{"nvidia": "/usr/bin/nvidia-container-runtime"}}
	if err := SaveState(want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.NodeLabels, want.NodeLabels) || !reflect.DeepEqual(got.NodeTaints, want.NodeTaints) || !reflect.DeepEqual(got.RuntimeHandlers, want.RuntimeHandlers) {
		t.Errorf("node facts after a round trip = %v %v %v, want %v %v %v", got.NodeLabels, got.NodeTaints, got.RuntimeHandlers, want.NodeLabels, want.NodeTaints, want.RuntimeHandlers)
	}
}

// From here down: the one mutation owner for node.yaml.
// Every node.yaml writer records its change by load-modify-save on the one
// shared file, so a writer that applies its change to a copy it loaded
// earlier — or to a fresh struct — loses whatever another writer recorded in
// between. The owner is the single place that re-reads the state and writes
// it back, and it does so under the etcd data lock, which every node.yaml
// writer (bring-up, rejoin, restore, `stratum up`, the growth recorder)
// already holds while it changes the file.

// withStateAndLockPaths points node.yaml and the etcd data lock at one temp
// tree: the owner takes the lock on the data dir, so the test must redirect
// both it and the state's directory or the lock file lands in
// /var/lib/stratum.
func withStateAndLockPaths(t *testing.T) {
	t.Helper()
	oldConf, oldData, oldEtcd := version.K8sConfDir, version.DataDir, version.EtcdDataDir
	root := t.TempDir()
	version.K8sConfDir = filepath.Join(root, "config")
	version.DataDir = filepath.Join(root, "data")
	version.EtcdDataDir = filepath.Join(root, "etcd")
	t.Cleanup(func() {
		version.K8sConfDir, version.DataDir, version.EtcdDataDir = oldConf, oldData, oldEtcd
	})
}

// TestMutateAppliesTheChangeToTheStateOnDisk pins the owner's merge: the
// mutation is applied to the state freshly read from disk, so what the writer
// did not set is kept — a save of the caller's own copy would drop it — and
// the write itself is SaveState's durable write at mode 0600.
func TestMutateAppliesTheChangeToTheStateOnDisk(t *testing.T) {
	withStateAndLockPaths(t)
	base := &NodeState{
		Role:         "controller",
		Hostname:     "cp1",
		NodeIP:       "192.0.2.10",
		Server:       "https://192.0.2.10:6443",
		EtcdState:    "new",
		EtcdHasPeers: true,
		ClusterDNSIP: "10.96.0.10",
		VIP:          VIP{Address: "192.0.2.1"},
	}
	if err := SaveState(base); err != nil {
		t.Fatal(err)
	}

	if err := Mutate(func(st *NodeState) error { st.ConfigPath = "/srv/stratum.yaml"; return nil }); err != nil {
		t.Fatalf("Mutate = %v", err)
	}
	want := *base
	want.ConfigPath = "/srv/stratum.yaml"
	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, &want) {
		t.Fatalf("node.yaml holds\n\t%+v\nwant\n\t%+v — the owner dropped a field the writer did not set", got, &want)
	}
	fi, err := os.Stat(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("node.yaml written as %04o, want 0600 — the owner keeps SaveState's mode", perm)
	}

	t.Run("a missing node.yaml is a fresh state", func(t *testing.T) {
		withStateAndLockPaths(t)
		var saw *NodeState
		if err := Mutate(func(st *NodeState) error {
			// Capture the state the mutation is handed before setting any
			// field: saw must hold what the owner read, not what this
			// callback then wrote into it.
			cur := *st
			saw = &cur
			st.Role, st.Hostname = "worker", "w1"
			return nil
		}); err != nil {
			t.Fatalf("Mutate = %v", err)
		}
		if saw == nil || saw.Role != "" || saw.Hostname != "" {
			t.Errorf("the mutation ran against %+v, want a fresh empty state — a first join's write has nothing to read", saw)
		}
		if got, err := LoadState(); err != nil || got.Role != "worker" || got.Hostname != "w1" {
			t.Fatalf("node.yaml = %+v, %v; want the first save", got, err)
		}
	})

	t.Run("a mutate error writes nothing", func(t *testing.T) {
		withStateAndLockPaths(t)
		if err := SaveState(base); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("boom")
		if err := Mutate(func(*NodeState) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("Mutate = %v, want the mutation's error", err)
		}
		if got, err := LoadState(); err != nil || !reflect.DeepEqual(got, base) {
			t.Fatalf("node.yaml = %+v, %v; want it untouched after a failed mutation", got, err)
		}
	})
}

// TestMutateRefusesWhileTheDataLockIsHeld pins the standalone lock contract:
// the owner takes the etcd data lock itself, so while a rejoin, a restore or
// an `up` holds it the mutation refuses instead of writing between that
// operation's read and write. Callers retry on the next pass or start, the
// way the growth recorder's poll does.
func TestMutateRefusesWhileTheDataLockIsHeld(t *testing.T) {
	withStateAndLockPaths(t)
	base := &NodeState{Role: "controller", Hostname: "cp1", EtcdState: "new"}
	if err := SaveState(base); err != nil {
		t.Fatal(err)
	}
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := Mutate(func(*NodeState) error { return nil }); err == nil {
		t.Fatal("the owner wrote while another operation held the data lock")
	}
	if got, err := LoadState(); err != nil || !reflect.DeepEqual(got, base) {
		t.Fatalf("node.yaml = %+v, %v; want it untouched while the lock was held", got, err)
	}
}

// TestMutateHeldWritesUnderTheHeldLock pins the under-lock variant: a caller
// already holding the data lock — a join mid-join, a restore, the growth
// recorder — hands the capability its etcddata.Lock returned and the owner
// writes without acquiring again, which would refuse: the flock is held by
// the caller's own descriptor.
func TestMutateHeldWritesUnderTheHeldLock(t *testing.T) {
	withStateAndLockPaths(t)
	base := &NodeState{Role: "controller", Hostname: "cp1", EtcdState: "new",
		EtcdName: "cp1", EtcdInitialCluster: "cp1=https://192.0.2.10:2380"}
	if err := SaveState(base); err != nil {
		t.Fatal(err)
	}
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if err := MutateHeld(release, func(st *NodeState) error { st.EtcdHasPeers = true; return nil }); err != nil {
		t.Fatalf("MutateHeld = %v; the under-lock variant must write while the caller holds the lock", err)
	}
	want := *base
	want.EtcdHasPeers = true
	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, &want) {
		t.Fatalf("node.yaml holds\n\t%+v\nwant\n\t%+v", got, &want)
	}
	fi, err := os.Stat(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("node.yaml written as %04o, want 0600", perm)
	}
}

// TestMutateHeldRefusesWithoutTheCapability pins the other side of the
// under-lock contract: a caller with no capability is not holding the flock,
// and writing anyway is exactly the unlocked write the owner exists to
// remove. The refusal is expected input, not a panic or a silent write.
func TestMutateHeldRefusesWithoutTheCapability(t *testing.T) {
	withStateAndLockPaths(t)
	base := &NodeState{Role: "controller", Hostname: "cp1", EtcdState: "new"}
	if err := SaveState(base); err != nil {
		t.Fatal(err)
	}
	if err := MutateHeld(nil, func(st *NodeState) error { st.EtcdHasPeers = true; return nil }); err == nil {
		t.Fatal("the under-lock variant wrote for a caller with no held-lock capability")
	}
	if got, err := LoadState(); err != nil || !reflect.DeepEqual(got, base) {
		t.Fatalf("node.yaml = %+v, %v; want it untouched after the refusal", got, err)
	}
}

// TestMutateWritesNothingWhenTheMutationChangedNothing pins the no-change
// contract: a mutate that reports ErrUnchanged — the growth recorder's poll
// finding nothing to record on a seed whose etcd has not grown — is a success
// the owner answers without saving, so a poll pass must not rewrite node.yaml
// with the bytes it just read. The file's mtime is forced into the past first,
// so no save can hide behind timestamp granularity: any write moves it
// forward.
func TestMutateWritesNothingWhenTheMutationChangedNothing(t *testing.T) {
	withStateAndLockPaths(t)
	base := &NodeState{Role: "controller", Hostname: "cp1", EtcdState: "new"}
	if err := SaveState(base); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(StatePath(), past, past); err != nil {
		t.Fatal(err)
	}

	// The under-lock variant: the poller's path.
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := MutateHeld(release, func(*NodeState) error { return ErrUnchanged }); err != nil {
		t.Fatalf("MutateHeld = %v; a no-change mutation is a success, not an error", err)
	}
	release()
	if err := unchangedOnDisk(t, base); err != nil {
		t.Error(err)
	}

	// The standalone owner, with a save seam that counts writes — airtight
	// where an mtime is only strong evidence.
	saves := 0
	if err := MutateSave(func(*NodeState) error { return ErrUnchanged },
		func(*NodeState) error { saves++; return nil }); err != nil {
		t.Fatalf("MutateSave = %v; a no-change mutation is a success, not an error", err)
	}
	if saves != 0 {
		t.Errorf("the no-change mutation saved %d times, want 0", saves)
	}
	if err := unchangedOnDisk(t, base); err != nil {
		t.Error(err)
	}

	// The control: a real change still writes — ErrUnchanged is the mutate's
	// report, not a blanket write skip.
	release2, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if err := MutateHeld(release2, func(st *NodeState) error { st.EtcdHasPeers = true; return nil }); err != nil {
		t.Fatalf("MutateHeld = %v", err)
	}
	if err := unchangedOnDisk(t, base); err == nil {
		t.Error("a real change did not reach node.yaml")
	}
}

// unchangedOnDisk reports whether node.yaml still holds want and still has
// the mtime the test pinned into the past: a write since then moved it
// forward.
func unchangedOnDisk(t *testing.T, want *NodeState) error {
	t.Helper()
	fi, err := os.Stat(StatePath())
	if err != nil {
		return err
	}
	if fi.ModTime().After(time.Now().Add(-time.Minute)) {
		return fmt.Errorf("node.yaml was rewritten (mtime %s)", fi.ModTime())
	}
	got, err := LoadState()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("node.yaml holds %+v, want %+v", got, want)
	}
	return nil
}

// TestConcurrentMutationsLoseNoField is the concurrent-update proof: eight
// goroutines, each recording one field, none holding the lock itself. Every
// mutation re-reads under the lock the previous goroutine's field, so all
// eight land; a save of a pre-loaded copy would keep only the last writer's
// field and lose the rest.
func TestConcurrentMutationsLoseNoField(t *testing.T) {
	withStateAndLockPaths(t)
	if err := SaveState(&NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	writers := []struct {
		name string
		set  func(*NodeState)
	}{
		{"server", func(st *NodeState) { st.Server = "https://10.0.0.1:6443" }},
		{"nodeIP", func(st *NodeState) { st.NodeIP = "192.0.2.8" }},
		{"kubeletNodeIP", func(st *NodeState) { st.KubeletNodeIP = "192.0.2.8" }},
		{"clusterDNSIP", func(st *NodeState) { st.ClusterDNSIP = "10.96.0.10" }},
		{"clusterDomain", func(st *NodeState) { st.ClusterDomain = "cluster.internal" }},
		{"podCIDR", func(st *NodeState) { st.PodCIDR = "10.245.0.0/16" }},
		{"serviceCIDR", func(st *NodeState) { st.ServiceCIDR = "10.96.0.0/12" }},
		{"configPath", func(st *NodeState) { st.ConfigPath = "/srv/stratum.yaml" }},
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(writers))
	for _, w := range writers {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Content on the lock refuses rather than blocks (LOCK_NB);
			// retry the way the growth recorder's poll does.
			for try := 0; ; try++ {
				if err := Mutate(func(st *NodeState) error { w.set(st); return nil }); err == nil {
					return
				}
				if try == 2000 {
					errs <- errors.New(w.name + ": the mutation never got the data lock")
					return
				}
				time.Sleep(500 * time.Microsecond)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	want := &NodeState{Role: "worker", Hostname: "w1"}
	for _, w := range writers {
		w.set(want)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("node.yaml holds\n\t%+v\nwant\n\t%+v — a concurrent update was lost", got, want)
	}
}

// TestSaveStateMatchesAtomicfileWriteFile pins the owner's write backend to
// the shared atomic writer's contract: same bytes, same explicit 0600 mode,
// and the directory left holding the written file alone — no stray temp name
// for a reader to trip over.
func TestSaveStateMatchesAtomicfileWriteFile(t *testing.T) {
	st := &NodeState{Role: "controller", Hostname: "cp1", EtcdState: "new"}
	saveDir, atomicDir := t.TempDir(), t.TempDir()
	oldConf := version.K8sConfDir
	version.K8sConfDir = saveDir
	t.Cleanup(func() { version.K8sConfDir = oldConf })
	if err := SaveState(st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	atomicPath := filepath.Join(atomicDir, "node.yaml")
	b, err := yaml.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.WriteFile(atomicPath, b, 0o600); err != nil {
		t.Fatalf("atomicfile.WriteFile: %v", err)
	}
	saved, err := os.ReadFile(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	atomic, err := os.ReadFile(atomicPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(atomic) {
		t.Error("SaveState wrote different bytes than atomicfile.WriteFile")
	}
	savedFi, err := os.Stat(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	atomicFi, err := os.Stat(atomicPath)
	if err != nil {
		t.Fatal(err)
	}
	if savedFi.Mode().Perm() != 0o600 || atomicFi.Mode().Perm() != 0o600 {
		t.Errorf("modes = %04o and %04o, want 0600 for both — a torn state must not also be world-readable", savedFi.Mode().Perm(), atomicFi.Mode().Perm())
	}
	saveEntries, _ := os.ReadDir(saveDir)
	atomicEntries, _ := os.ReadDir(atomicDir)
	if len(saveEntries) != 1 || len(atomicEntries) != 1 {
		t.Errorf("dirs hold %d and %d entries, want the written file alone in each", len(saveEntries), len(atomicEntries))
	}
}

// TestSaveStateReplacesTheFileWhole pins the property the durable write exists
// for: a state that shrinks is replaced by rename, never truncated in place —
// a crash mid-write of a truncating writer would leave a half file a reader
// cannot parse.
func TestSaveStateReplacesTheFileWhole(t *testing.T) {
	withStateAndLockPaths(t)
	long := &NodeState{Role: "controller", Hostname: "a-very-long-hostname-that-dwarfs-the-replacement", EtcdState: "new",
		EtcdInitialCluster: "a-very-long-initial-cluster-record-that-dwarfs-the-replacement=https://192.0.2.10:2380"}
	if err := SaveState(long); err != nil {
		t.Fatal(err)
	}
	short := &NodeState{Role: "worker", Hostname: "w1"}
	if err := SaveState(short); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState()
	if err != nil {
		t.Fatalf("the rewritten state does not load: %v", err)
	}
	if !reflect.DeepEqual(got, short) {
		t.Fatalf("node.yaml holds %+v, want %+v — the replacement left residue behind", got, short)
	}
}

// A failed lock acquisition cannot authorize either half of a mutation.
func TestMutateRefusesUnavailableLock(t *testing.T) {
	withStateAndLockPaths(t)
	if err := SaveState(&NodeState{Role: "controller", Hostname: "original"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(version.DataDir+"-etcd.lock", 0o700); err != nil {
		t.Fatal(err)
	}
	for _, useSave := range []bool{false, true} {
		mutated, saved := false, false
		mutate := func(st *NodeState) error { mutated = true; st.Hostname = "changed"; return nil }
		var err error
		if useSave {
			err = MutateSave(mutate, func(*NodeState) error { saved = true; return nil })
		} else {
			err = Mutate(mutate)
		}
		if err == nil {
			t.Error("mutation succeeded without acquiring the lock")
		}
		if mutated || saved {
			t.Errorf("lock failure called mutation/save: %v/%v", mutated, saved)
		}
		after, err := os.ReadFile(StatePath())
		if err != nil || string(after) != string(before) {
			t.Errorf("node state changed after lock failure: %q, %v", after, err)
		}
	}
}

package cluster

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
	"sigs.k8s.io/yaml"
)

// From here: the node-owned backup-evidence publisher. It is a run-group
// task postStart starts beside the snapshot loop — started by `stratum up`,
// parked on the group's context, joined at shutdown, backoff-restarted by
// the group — and it samples the engine's assembled backup-evidence reader
// through the seam wired from above (pkg/cli passes the reader as a
// function, because its package imports this one), refreshing the durable
// record under the node's config area on the contract cadence. It needs no
// operator and no apiserver, so it runs on an operator-off controller like
// every other task, and it shares the snapshot loop's spec.snapshots
// disabled gate.

// stubPublisherSeam points the publisher's sampler and cadence seams at the
// given sampler ticking on the given cadence, restoring both on cleanup:
// the publisher reads both through package seams, so a test drives it with
// a stub and a shrunk interval the way the loop tests drive snapshotSave.
func stubPublisherSeam(t *testing.T, interval time.Duration, sample EvidenceSampler) {
	t.Helper()
	oldSampler, oldInterval := evidenceSampler, evidencePublishInterval
	t.Cleanup(func() { evidenceSampler, evidencePublishInterval = oldSampler, oldInterval })
	evidenceSampler, evidencePublishInterval = sample, interval
}

// recordedPublisherEvidence is the publisher's durable record as these
// tests read it back from disk, json tags and all — the restart-persistence
// proof: nothing carries a sample across processes but through the file.
type recordedPublisherEvidence struct {
	SchemaVersion  int       `json:"schemaVersion"`
	SampledAt      time.Time `json:"sampledAt"`
	Node           string    `json:"node"`
	Complete       bool      `json:"complete"`
	LocalArtifacts int       `json:"localArtifacts"`
	RemoteCopies   int       `json:"remoteCopies"`
	Backups        int       `json:"backups"`
	ExactTotal     bool      `json:"exactTotal"`
	Warnings       []string  `json:"warnings"`
}

// publisherRecordPath is where the publisher's record persists, the same
// place the production writer and the wiring test's supervisor stub look.
func publisherRecordPath() string {
	return filepath.Join(version.K8sConfDir, "backup-evidence.yaml")
}

// readPublisherRecord reads the durable record fresh from disk.
func readPublisherRecord(t *testing.T) recordedPublisherEvidence {
	t.Helper()
	b, err := os.ReadFile(publisherRecordPath())
	if err != nil {
		t.Fatalf("read the backup evidence record: %v", err)
	}
	var rec recordedPublisherEvidence
	if err := yaml.Unmarshal(b, &rec); err != nil {
		t.Fatalf("parse the backup evidence record (%s): %v", b, err)
	}
	return rec
}

// TestPublisherRecordIsOwnedAndReservedFromProfiles pins the record's
// repo-gate obligations, asserted on the record's own path because the
// coverage tests only iterate files already in the record: the path is in
// internal/version's owning-package record with this package as its owner —
// the node-file write guard derives the files and packages it covers from
// that record, so without the entry the publisher's writes stand outside the
// guard — and its name is one the engine keeps in its config directory
// (internal/profile's reserved names), so no node profile may declare a
// config file of that name: one that could would fight the publisher's
// every-cadence refresh of the same path, each silently destroying the
// other's file.
func TestPublisherRecordIsOwnedAndReservedFromProfiles(t *testing.T) {
	if got, ok := version.NodeFileOwners[publisherRecordPath()]; !ok || got != "internal/cluster" {
		t.Errorf("the owning-package record has %q for the publisher's record %s, want internal/cluster: the node-file write guard covers a node file only through that record", got, publisherRecordPath())
	}
	if !profile.EngineOwnsConfigName("backup-evidence.yaml") {
		t.Error("the publisher's record name backup-evidence.yaml is not reserved: a profile may declare a config file of that name and replace the record the publisher refreshes on its cadence")
	}
}

// TestPublisherSamplesAndRecordsDurably pins the publisher's core contract:
// the task postStart starts samples at once and then on its cadence, and
// every sample reaches the durable record under the config area — the
// latest sample whole, never an append — written atomically at 0600 the
// way every node record is.
func TestPublisherSamplesAndRecordsDurably(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Hour)
	cfg := loadClusterConfig(t, "spec: {}\n")
	joined := seedState()
	joined.EtcdState = "existing"

	// The first sample describes two exact backups, every later one three
	// with an unknown total: the record must end at the latest sample, not
	// stay at the first.
	var samples atomic.Int32
	stubPublisherSeam(t, 10*time.Millisecond, func(context.Context) (EvidenceSample, bool) {
		s := EvidenceSample{
			Node:           "cp-1",
			Complete:       true,
			LocalArtifacts: 2,
			RemoteCopies:   1,
			Backups:        2,
			Exact:          true,
			Warnings:       []string{"WARN: the union's total is unknown: a catalog in the union is partial, and a partial listing never yields an exact total"},
		}
		if samples.Add(1) > 1 {
			s.Backups, s.Exact = 3, false
		}
		return s, true
	})

	// The bubble only orchestrates — start, wait out the cadence, stop, join
	// — and every assertion runs after it, so a failure leaves no parked
	// goroutine behind.
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		cancel()
		g.wait()
	})
	if got := samples.Load(); got < 2 {
		t.Fatalf("publisher samples = %d; the task must sample at once and then on its cadence", got)
	}

	rec := readPublisherRecord(t)
	if rec.SampledAt.IsZero() {
		t.Error("the record carries no sampled-at: a reader cannot tell when the publisher last sampled")
	}
	if rec.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", rec.SchemaVersion)
	}
	if rec.Node != "cp-1" {
		t.Errorf("Node = %q, want the sample's cp-1", rec.Node)
	}
	if !rec.Complete {
		t.Error("Complete = false, want the sample's whole read")
	}
	if rec.LocalArtifacts != 2 || rec.RemoteCopies != 1 {
		t.Errorf("local/remote = %d/%d, want the sample's 2/1", rec.LocalArtifacts, rec.RemoteCopies)
	}
	if rec.Backups != 3 || rec.ExactTotal {
		t.Errorf("backups = %d exact %v, want the latest sample's 3 with an unknown total: the record must end at the latest sample", rec.Backups, rec.ExactTotal)
	}
	if len(rec.Warnings) != 1 || rec.Warnings[0] != "WARN: the union's total is unknown: a catalog in the union is partial, and a partial listing never yields an exact total" {
		t.Errorf("warnings = %q, want the sample's one safe warning", rec.Warnings)
	}
	fi, err := os.Stat(publisherRecordPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the record's mode is %v, want 0600: a node record names this node's backups", fi.Mode().Perm())
	}
}

// TestPublisherRecordsIntoAMissingConfigArea pins the record's durability
// precondition: every node-record writer creates its directory before the
// atomic write — the snapshot evidence record, the renewal record, the
// node state — so a publisher whose config area is missing (wiped under a
// running controller, a state save that failed at start) records into it
// on its next tick instead of warning forever that the write found no
// directory, the one condition the loop it shares a gate with heals by
// itself.
func TestPublisherRecordsIntoAMissingConfigArea(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Hour)
	// A config area that does not exist: redirectStateArea's temp dir is
	// there, so point the record's directory one level deeper, at a path
	// nothing has created.
	missing := filepath.Join(t.TempDir(), "config")
	oldConf := version.K8sConfDir
	version.K8sConfDir = missing
	t.Cleanup(func() { version.K8sConfDir = oldConf })

	cfg := loadClusterConfig(t, "spec: {}\n")
	joined := seedState()
	joined.EtcdState = "existing"
	stubPublisherSeam(t, 10*time.Millisecond, func(context.Context) (EvidenceSample, bool) {
		return EvidenceSample{Node: "cp-1", Backups: 1, Exact: true}, true
	})

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		cancel()
		g.wait()
	})
	if _, err := os.Stat(publisherRecordPath()); err != nil {
		t.Fatalf("the publisher's record did not land in a config area that did not exist: %v", err)
	}
}

// TestPublisherStopsOnTheGroupContextAndJoinsItsInFlightSample pins the
// shutdown half: the publisher is a run-group task, not a stray goroutine,
// so a sample in flight when the controller stops finishes — the group's
// wait holds until it does — and the task then parks nowhere else, ending
// on the cancelled group context like every other task.
func TestPublisherStopsOnTheGroupContextAndJoinsItsInFlightSample(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Hour)
	cfg := loadClusterConfig(t, "spec: {}\n")
	joined := seedState()
	joined.EtcdState = "existing"

	// The sample parks until released: the parked sample is the in-flight
	// work the join watches. The channel is created inside the bubble — a
	// task parked on a channel created outside it is never durably blocked,
	// and synctest.Wait would hang.
	var release chan struct{}
	var parked, returned atomic.Int32
	stubPublisherSeam(t, time.Minute, func(context.Context) (EvidenceSample, bool) {
		parked.Add(1)
		<-release
		returned.Add(1)
		return EvidenceSample{Node: "cp-1", Backups: 1, Exact: true}, true
	})

	var joinedWhileInFlight bool
	synctest.Test(t, func(t *testing.T) {
		release = make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if parked.Load() < 1 {
			// Nothing sampled: stop the group cleanly and let the
			// assertion after the bubble say it.
			cancel()
			g.wait()
			return
		}

		waitDone := make(chan struct{})
		go func() { g.wait(); close(waitDone) }()
		synctest.Wait()
		select {
		case <-waitDone:
			joinedWhileInFlight = true
		default:
		}
		if joinedWhileInFlight {
			// The join already returned with the sample parked; stop the
			// group and report after the bubble.
			cancel()
			close(release)
			<-waitDone
			return
		}

		cancel()
		close(release)
		// A fake minute is far past any backoff floor and far more than
		// the task needs to notice the cancelled context; a task that
		// never stops must fail, not hang the test.
		select {
		case <-waitDone:
		case <-time.After(time.Minute):
			t.Error("the publisher never stopped on the cancelled context")
		}
	})
	if parked.Load() < 1 {
		t.Fatal("the publisher never took its first sample")
	}
	if joinedWhileInFlight {
		t.Fatal("the run group's wait returned while a sample was still mid-flight: the publisher is not a joined run-group task")
	}
	if returned.Load() != parked.Load() {
		t.Fatalf("the in-flight sample was cut down mid-flight: %d parked, %d returned", parked.Load(), returned.Load())
	}
}

// TestPublisherIsOnTheRosterWithTheOperatorOff pins the operator
// independence of the publisher's lifecycle: postStart starts it with
// spec.operator off exactly as with it on — the publisher needs no operator
// process and no operator API — and only spec.snapshots.disabled drops it,
// the same gate that drops the snapshot loop. The context ends before the
// start, so the group starts none of the tasks and the roster is all this
// test reads.
func TestPublisherIsOnTheRosterWithTheOperatorOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{"operator on", "spec: {}\n", true},
		{"operator off", "spec:\n  operator: false\n", true},
		{"snapshots disabled", "spec:\n  snapshots:\n    disabled: true\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadClusterConfig(t, tc.yaml)
			joined := seedState()
			joined.EtcdState = "existing"
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			g := newRunGroup(ctx)
			postStart(g, cfg, joined)
			g.wait()
			if got := slices.Contains(g.names, "backup evidence publisher"); got != tc.want {
				t.Errorf("the publisher is on the roster: %v, want %v (roster: %v)", got, tc.want, g.names)
			}
		})
	}
}

// TestPublisherIsGatedLikeTheSnapshotLoop pins the gate the publisher
// shares with the snapshot loop: with spec.snapshots.disabled the publisher
// samples nothing — no record is written, no sampler runs — while the
// config's other loops keep their own gates.
func TestPublisherIsGatedLikeTheSnapshotLoop(t *testing.T) {
	fakeHostKubectl(t)
	renewals, snapshots := stubLoops(t, time.Millisecond)
	cfg := loadClusterConfig(t, "spec:\n  snapshots:\n    disabled: true\n")
	joined := seedState()
	joined.EtcdState = "existing"

	var samples atomic.Int32
	stubPublisherSeam(t, time.Millisecond, func(context.Context) (EvidenceSample, bool) {
		samples.Add(1)
		return EvidenceSample{Node: "cp-1"}, true
	})

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		cancel()
		g.wait()
	})
	if got := samples.Load(); got != 0 {
		t.Errorf("publisher samples = %d with spec.snapshots.disabled: the publisher must share the snapshot loop's gate", got)
	}
	if got := snapshots(); got != 0 {
		t.Errorf("snapshot ticks = %d with spec.snapshots.disabled", got)
	}
	if got := renewals(); got == 0 {
		t.Error("the disabled snapshot gate stopped the renewal loop too: the gates are separate")
	}
}

// TestPublisherSamplerParameterIsWiredByRunController pins the seam's
// wiring: the evidence sampler RunController takes — the only way the
// engine's assembled reader, whose package imports this one, can reach the
// publisher — lands on the sample seam and the publisher's first sample is
// taken and recorded durably before the controller's run ends. The
// supervisor stands in for itself returning at once after the record
// lands; everything before it — the renders, the roster, postStart — is the
// real start path.
func TestPublisherSamplerParameterIsWiredByRunController(t *testing.T) {
	fakeHostKubectl(t)
	redirectStateArea(t)
	// The renders before the supervisor write into these roots; point them
	// at temp trees so the start touches nothing of the host.
	for _, sw := range []struct{ p *string }{
		{&version.ManifestsDir},
		{&version.CoreDNSUpstreamDir},
		{&version.CiliumSettingsPath},
		{&version.EtcdDataDir},
		{&version.KubeconfigDir},
	} {
		dir := filepath.Join(t.TempDir(), "scratch")
		old := *sw.p
		*sw.p = dir
		t.Cleanup(func() { *sw.p = old })
	}
	stubLoops(t, time.Hour)
	cfg := loadClusterConfig(t, "spec: {}\n")
	st := seedState()
	st.EtcdState = "existing"
	stubNodeTask(t, time.Millisecond, time.Hour, st.Hostname,
		func(context.Context, string) bool { return true })

	var samples atomic.Int32
	sample := func(context.Context) (EvidenceSample, bool) {
		samples.Add(1)
		return EvidenceSample{
			Node:           "cp-1",
			Complete:       true,
			LocalArtifacts: 1,
			RemoteCopies:   1,
			Backups:        1,
			Exact:          true,
		}, true
	}

	oldRun := runSupervisor
	t.Cleanup(func() { runSupervisor = oldRun })
	// The stub stands where the supervisor stands — after every render,
	// after postStart — and returns once the publisher's first sample has
	// landed in the durable record, so the wiring the test pins is the
	// whole real path up to the one call it must not make (a real
	// supervisor starts host processes).
	runSupervisor = func(context.Context, []supervisor.Process, supervisor.Options) error {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(publisherRecordPath()); err == nil {
				return nil
			}
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunController(ctx, cfg, st, sample) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("RunController did not return after its supervisor returned")
	}
	if got := samples.Load(); got < 1 {
		t.Fatal("the evidence-sampler parameter never reached the publisher: no sample was taken through the real start path")
	}
	rec := readPublisherRecord(t)
	if rec.Node != "cp-1" || rec.Backups != 1 || !rec.ExactTotal || rec.LocalArtifacts != 1 || rec.RemoteCopies != 1 || !rec.Complete {
		t.Errorf("the record = %+v, want the wired sampler's sample recorded whole", rec)
	}
}

// TestPublisherKeepsSamplingWhenTheRecordCannotBeWritten pins the write's
// error path: a record write that fails — a config area that cannot be
// written — is a warning in the daemon's log, never the end of the task.
// The publisher keeps sampling on its cadence: the run group's backoff
// restart exists for panics and early returns, and a failed write must not
// become one, or a node whose config area heals would stop publishing
// until a restart.
func TestPublisherKeepsSamplingWhenTheRecordCannotBeWritten(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Hour)
	// A regular file where the config directory belongs: every record write
	// into it fails, while the sampler keeps answering.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldConf := version.K8sConfDir
	version.K8sConfDir = blocker
	t.Cleanup(func() { version.K8sConfDir = oldConf })

	cfg := loadClusterConfig(t, "spec: {}\n")
	joined := seedState()
	joined.EtcdState = "existing"

	var samples atomic.Int32
	stubPublisherSeam(t, 10*time.Millisecond, func(context.Context) (EvidenceSample, bool) {
		samples.Add(1)
		return EvidenceSample{Node: "cp-1", Backups: 1, Exact: true}, true
	})

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		g := newRunGroup(ctx)
		postStart(g, cfg, joined)
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		cancel()
		g.wait()
	})
	if got := samples.Load(); got < 2 {
		t.Fatalf("publisher samples = %d beside a failed record write: the task must keep sampling on its cadence", got)
	}
	if _, err := os.Stat(publisherRecordPath()); err == nil {
		t.Error("a record landed although every write into the config area fails")
	}
}

// TestRunControllerResetsTheSamplerSeamWhenNoSamplerIsPassed pins the
// seam's reset: the sample seam is a package global, so a RunController
// call that wires no sampler must leave the seam at its no-op default —
// not keep the sampler a previous call left behind — exactly as the
// parameter's doc promises ("Absent, the sample seam keeps its no-op
// default and the publisher records nothing"). The supervisor stands in
// for itself returning at once, the same stand-in the wiring test uses;
// the assignment is the start's own first step, so the whole real start
// path is what resets it.
func TestRunControllerResetsTheSamplerSeamWhenNoSamplerIsPassed(t *testing.T) {
	fakeHostKubectl(t)
	redirectStateArea(t)
	for _, sw := range []struct{ p *string }{
		{&version.ManifestsDir},
		{&version.CoreDNSUpstreamDir},
		{&version.CiliumSettingsPath},
		{&version.EtcdDataDir},
		{&version.KubeconfigDir},
	} {
		dir := filepath.Join(t.TempDir(), "scratch")
		old := *sw.p
		*sw.p = dir
		t.Cleanup(func() { *sw.p = old })
	}
	stubLoops(t, time.Hour)
	cfg := loadClusterConfig(t, "spec: {}\n")
	st := seedState()
	st.EtcdState = "existing"
	stubNodeTask(t, time.Millisecond, time.Hour, st.Hostname,
		func(context.Context, string) bool { return true })

	// A sampler a previous RunController call left behind: the seam must
	// not keep it. It answers ok=true so a left-behind seam is tellable
	// from the reset no-op default.
	evidenceSampler = func(context.Context) (EvidenceSample, bool) {
		return EvidenceSample{}, true
	}
	t.Cleanup(func() { evidenceSampler = noopEvidenceSampler })

	oldRun := runSupervisor
	t.Cleanup(func() { runSupervisor = oldRun })
	runSupervisor = func(context.Context, []supervisor.Process, supervisor.Options) error { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunController(ctx, cfg, st) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("RunController did not return after its supervisor returned")
	}
	if _, ok := evidenceSampler(context.Background()); ok {
		t.Error("the sample seam kept a previously wired sampler: a start with no sampler must reset it to the no-op default, as the parameter's doc promises")
	}
}

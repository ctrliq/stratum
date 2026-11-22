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

func stubPublisherSeam(t *testing.T, interval time.Duration, sample EvidenceSampler) {
	t.Helper()
	oldSampler, oldInterval := evidenceSampler, evidencePublishInterval
	t.Cleanup(func() { evidenceSampler, evidencePublishInterval = oldSampler, oldInterval })
	evidenceSampler, evidencePublishInterval = sample, interval
}

// Nothing carries a sample across processes but through the record file.
type recordedPublisherEvidence struct {
	SchemaVersion  int       `json:"schemaVersion"`
	SampledAt      time.Time `json:"sampledAt"`
	NodeUID        string    `json:"nodeUID"`
	Node           string    `json:"node"`
	Complete       bool      `json:"complete"`
	LocalArtifacts int       `json:"localArtifacts"`
	RemoteCopies   int       `json:"remoteCopies"`
	Backups        int       `json:"backups"`
	ExactTotal     bool      `json:"exactTotal"`
	Warnings       []string  `json:"warnings"`
}

func publisherRecordPath() string {
	return filepath.Join(version.K8sConfDir, "backup-evidence.yaml")
}

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

// The write guard covers the record only via the owning-package record; no profile may shadow it.
func TestPublisherRecordIsOwnedAndReservedFromProfiles(t *testing.T) {
	if got, ok := version.NodeFileOwners[publisherRecordPath()]; !ok || got != "internal/cluster" {
		t.Errorf("the owning-package record has %q for the publisher's record %s, want internal/cluster: the node-file write guard covers a node file only through that record", got, publisherRecordPath())
	}
	if !profile.EngineOwnsConfigName("backup-evidence.yaml") {
		t.Error("the publisher's record name backup-evidence.yaml is not reserved: a profile may declare a config file of that name and replace the record the publisher refreshes on its cadence")
	}
}

// Samples at once and then on its cadence; every sample replaces the record whole.
func TestPublisherSamplesAndRecordsDurably(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Hour)
	cfg := loadClusterConfig(t, "spec: {}\n")
	joined := seedState()
	joined.EtcdState = "existing"

	// The record must end at the latest sample, not the first.
	var samples atomic.Int32
	stubPublisherSeam(t, 10*time.Millisecond, func(context.Context) (EvidenceSample, bool) {
		s := EvidenceSample{
			Node:           "cp-1",
			NodeUID:        "immutable-node-uid",
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
	if rec.NodeUID != "immutable-node-uid" {
		t.Errorf("NodeUID = %q, want the sample immutable identity", rec.NodeUID)
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

// Every node-record writer creates its directory before the atomic write.
func TestPublisherRecordsIntoAMissingConfigArea(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Hour)
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

// A sample in flight at shutdown finishes before the group's wait returns.
func TestPublisherStopsOnTheGroupContextAndJoinsItsInFlightSample(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Hour)
	cfg := loadClusterConfig(t, "spec: {}\n")
	joined := seedState()
	joined.EtcdState = "existing"

	// The channel is created inside the bubble; synctest.Wait would hang otherwise.
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
			cancel()
			close(release)
			<-waitDone
			return
		}

		cancel()
		close(release)
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

// The stub returns once the record lands; the renders, roster and postStart before it are real.
func TestPublisherSamplerParameterIsWiredByRunController(t *testing.T) {
	fakeHostKubectl(t)
	redirectStateArea(t)
	// The renders before the supervisor write into these roots.
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

// A failed write must not end the task, or a healed config area stops publishing.
func TestPublisherKeepsSamplingWhenTheRecordCannotBeWritten(t *testing.T) {
	fakeHostKubectl(t)
	stubLoops(t, time.Hour)
	// A regular file where the config directory belongs makes every write fail.
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

	// Answers ok=true so a left-behind seam is tellable from the reset no-op default.
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

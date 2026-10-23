package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/operator"
	"github.com/ctrliq/stratum/internal/version"
)

func TestStagedGenerationRetriesAfterColdStartAndProcessRestart(t *testing.T) {
	dir := t.TempDir()
	savedDir := version.ManifestsDir
	version.ManifestsDir = dir
	savedRecord := operatorRecordStaged
	t.Cleanup(func() { version.ManifestsDir, operatorRecordStaged = savedDir, savedRecord })
	calls := 0
	var previous []string
	operatorRecordStaged = func(_ context.Context, staged operator.StagedGeneration, last string) (operator.StagingPublication, error) {
		calls++
		previous = append(previous, last)
		if calls <= 2 {
			return operator.StagingPublication{}, errors.New("apiserver unavailable")
		}
		return operator.StagingPublication{Generation: 1, ID: "publication-1"}, nil
	}
	cfg := &ClusterConfig{}
	st := &NodeState{Hostname: "cp1"}
	bundles := map[string][]byte{"alpha/a.yaml": []byte("first")}
	if err := stageDesiredBundles(cfg, st, bundles); err == nil {
		t.Fatal("cold first boot unexpectedly published")
	}
	if _, err := os.Stat(filepath.Join(dir, stagedFingerprintName)); !os.IsNotExist(err) {
		t.Fatalf("failed publish must leave the successful fingerprint absent: %v", err)
	}
	// A process restart stages the same bytes before the apiserver is ready.
	if err := stageDesiredBundles(cfg, st, bundles); err == nil {
		t.Fatal("second unavailable attempt unexpectedly published")
	}
	if err := publishPendingStaging(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, last := range previous {
		if last != "" {
			t.Fatalf("failed publishes changed the successful fingerprint: %v", previous)
		}
	}
	stamp, err := os.ReadFile(filepath.Join(dir, stagedFingerprintName))
	if err != nil || len(stamp) == 0 {
		t.Fatalf("successful retry must persist the fingerprint: %q, %v", stamp, err)
	}
}

func TestStagedBundlesPreserveActiveIntentAcrossControllerRestarts(t *testing.T) {
	savedDir, savedRecord := version.ManifestsDir, operatorRecordStaged
	t.Cleanup(func() { version.ManifestsDir, operatorRecordStaged = savedDir, savedRecord })
	cs := k8sfake.NewSimpleClientset()
	operatorRecordStaged = func(ctx context.Context, staged operator.StagedGeneration, previous string) (operator.StagingPublication, error) {
		return operator.RecordStagedGeneration(ctx, cs, staged, previous)
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	cfg := &ClusterConfig{}
	st := &NodeState{Hostname: "cp1", ClusterDomain: "cluster.local"}
	old := map[string][]byte{"alpha/a.yaml": []byte("old")}
	newer := map[string][]byte{"alpha/a.yaml": []byte("new")}
	for _, dir := range []string{dirA, dirB} {
		version.ManifestsDir = dir
		if err := stageDesiredBundles(cfg, st, old); err != nil {
			t.Fatal(err)
		}
	}
	version.ManifestsDir = dirA
	if err := stageDesiredBundles(cfg, st, newer); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	for _, dir := range []string{dirB, t.TempDir()} {
		version.ManifestsDir = dir
		if err := stageDesiredBundles(cfg, st, old); err == nil {
			t.Fatal("stale controller replaced active intent")
		}
		if err := publishPendingStaging(context.Background()); err == nil {
			t.Fatal("post-start retry activated a stale controller's pending request")
		}
	}
	cm, err := cs.CoreV1().ConfigMaps("kube-system").Get(context.Background(), "stratum-addon-inventory", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var stored struct{ Desired struct{ Digest string } }
	if err := json.Unmarshal([]byte(cm.Data["inventory"]), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Desired.Digest != operator.DigestBundles(newer) {
		t.Fatalf("active digest = %s; old restart changed the desired release", stored.Desired.Digest)
	}
	version.ManifestsDir = dirA
	if err := stageDesiredBundles(cfg, st, old); err != nil {
		t.Fatalf("actual changed local rollback: %v", err)
	}
}

func TestStagedBundlesRollbackAfterCommittedPublicationLosesLocalAcknowledgment(t *testing.T) {
	savedDir, savedRecord := version.ManifestsDir, operatorRecordStaged
	version.ManifestsDir = t.TempDir()
	t.Cleanup(func() { version.ManifestsDir, operatorRecordStaged = savedDir, savedRecord })
	cs := k8sfake.NewSimpleClientset()
	operatorRecordStaged = func(ctx context.Context, staged operator.StagedGeneration, previous string) (operator.StagingPublication, error) {
		return operator.RecordStagedGeneration(ctx, cs, staged, previous)
	}
	cfg, st := &ClusterConfig{}, &NodeState{Hostname: "cp1"}
	old := map[string][]byte{"alpha/a.yaml": []byte("old")}
	newer := map[string][]byte{"alpha/a.yaml": []byte("new")}
	if err := stageDesiredBundles(cfg, st, old); err != nil {
		t.Fatal(err)
	}
	stampPath := filepath.Join(version.ManifestsDir, stagedFingerprintName)
	oldStamp, err := os.ReadFile(stampPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := stageDesiredBundles(cfg, st, newer); err != nil {
		t.Fatal(err)
	}
	// The API committed the new generation, but a crash lost the local ack.
	if err := os.WriteFile(stampPath, oldStamp, 0o600); err != nil {
		t.Fatal(err)
	}
	record := operatorRecordStaged
	operatorRecordStaged = func(context.Context, operator.StagedGeneration, string) (operator.StagingPublication, error) {
		return operator.StagingPublication{}, errors.New("apiserver temporarily unavailable during rollback")
	}
	if err := stageDesiredBundles(cfg, st, old); err == nil {
		t.Fatal("unavailable rollback API unexpectedly succeeded")
	}
	operatorRecordStaged = record
	if err := stageDesiredBundles(cfg, st, old); err != nil {
		t.Fatalf("whole-node rollback must recognize its committed pending predecessor: %v", err)
	}
	inputs, err := clusterInputs(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	newGeneration := operator.StagedGeneration{PublicationID: "later-new", Digest: operator.DigestBundles(newer), Inputs: inputs}
	if _, err := operator.RecordStagedGeneration(context.Background(), cs, newGeneration, mustActiveStagingPublication(t, cs)); err != nil {
		t.Fatal(err)
	}
	if err := stageDesiredBundles(cfg, st, old); err == nil {
		t.Fatal("acknowledged rollback retained predecessor authority and overwrote a later release on unchanged restart")
	}
}

func TestRefusedStagingCannotBecomeAnUnacknowledgedPredecessor(t *testing.T) {
	cs, cfg, st := stagedPublicationFixture(t)
	owner, newcomer := t.TempDir(), t.TempDir()
	a := map[string][]byte{"alpha/a.yaml": []byte("A")}
	b := map[string][]byte{"alpha/a.yaml": []byte("B")}
	c := map[string][]byte{"alpha/a.yaml": []byte("C")}
	version.ManifestsDir = owner
	for _, files := range []map[string][]byte{a, b} {
		if err := stageDesiredBundles(cfg, st, files); err != nil {
			t.Fatal(err)
		}
	}
	version.ManifestsDir = newcomer
	for _, files := range []map[string][]byte{a, c} {
		if err := stageDesiredBundles(cfg, st, files); err == nil {
			t.Fatal("first-time participant replaced active intent")
		}
	}
	version.ManifestsDir = owner
	if err := stageDesiredBundles(cfg, st, a); err != nil {
		t.Fatal(err)
	}
	version.ManifestsDir = newcomer
	if err := publishPendingStaging(t.Context()); err == nil {
		t.Fatal("refused A was treated as an uncertain publication and authorized C after rollback")
	}
	cm, err := cs.CoreV1().ConfigMaps("kube-system").Get(t.Context(), "stratum-addon-inventory", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var stored struct{ Desired struct{ Digest string } }
	if err := json.Unmarshal([]byte(cm.Data["inventory"]), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Desired.Digest != operator.DigestBundles(a) {
		t.Fatal("refused staging changed the rollback's active intent")
	}
}

func TestPendingStagingCannotReuseAnEarlierContentEpoch(t *testing.T) {
	_, cfg, st := stagedPublicationFixture(t)
	owner, follower := t.TempDir(), t.TempDir()
	a := map[string][]byte{"alpha/a.yaml": []byte("A")}
	b := map[string][]byte{"alpha/a.yaml": []byte("B")}
	c := map[string][]byte{"alpha/a.yaml": []byte("C")}
	for _, dir := range []string{owner, follower} {
		version.ManifestsDir = dir
		if err := stageDesiredBundles(cfg, st, a); err != nil {
			t.Fatal(err)
		}
	}
	version.ManifestsDir = owner
	if err := stageDesiredBundles(cfg, st, b); err != nil {
		t.Fatal(err)
	}
	version.ManifestsDir = follower
	if err := stageDesiredBundles(cfg, st, c); err == nil {
		t.Fatal("stale follower replaced B")
	}
	version.ManifestsDir = owner
	if err := stageDesiredBundles(cfg, st, a); err != nil {
		t.Fatal(err)
	}
	version.ManifestsDir = follower
	if err := publishPendingStaging(t.Context()); err == nil {
		t.Fatal("pending C from the first A activated after A -> B -> A")
	}
}

func TestJoinedStagingRecoversLostAcknowledgment(t *testing.T) {
	_, cfg, st := stagedPublicationFixture(t)
	a := map[string][]byte{"alpha/a.yaml": []byte("A")}
	b := map[string][]byte{"alpha/a.yaml": []byte("B")}
	for range 2 {
		version.ManifestsDir = t.TempDir()
		if err := stageDesiredBundles(cfg, st, a); err != nil {
			t.Fatal(err)
		}
	}
	// The follower joined an existing publication, so its request ID differs
	// from the cluster publication ID. A crash loses its local receipt.
	if err := os.Remove(filepath.Join(version.ManifestsDir, stagedFingerprintName)); err != nil {
		t.Fatal(err)
	}
	if err := stageDesiredBundles(cfg, st, b); err != nil {
		t.Fatalf("lost joined-publication acknowledgment must recover: %v", err)
	}
}

func TestStorageConfigChangeStagesAndRecordsBundleRemoval(t *testing.T) {
	cs, _, st := stagedPublicationFixture(t)
	version.ManifestsDir = t.TempDir()
	savedCilium := version.CiliumSettingsPath
	version.CiliumSettingsPath = filepath.Join(t.TempDir(), "cilium.json")
	t.Cleanup(func() { version.CiliumSettingsPath = savedCilium })
	configPath := filepath.Join(t.TempDir(), "storage.yaml")
	for i, storage := range []string{"local-path", "none"} {
		if err := os.WriteFile(configPath, []byte("spec:\n  storage:\n    default: "+storage+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeBundles(cfg, st); err != nil {
			t.Fatal(err)
		}
		files := stagedStacks(t, version.ManifestsDir)
		_, localPathPresent := files[localPathFile]
		if localPathPresent != (storage == "local-path") {
			t.Fatalf("storage %s: local-path bundle present = %v", storage, localPathPresent)
		}
		cm, err := cs.CoreV1().ConfigMaps("kube-system").Get(t.Context(), "stratum-addon-inventory", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var inv struct {
			Desired struct {
				Generation int
				Digest     string
				Stacks     []string
				Inputs     operator.ClusterInputs
			}
		}
		if err := json.Unmarshal([]byte(cm.Data["inventory"]), &inv); err != nil {
			t.Fatal(err)
		}
		desired := inv.Desired
		if desired.Generation != i+1 || desired.Digest != operator.DigestBundles(files) || desired.Inputs.StorageDefault != storage {
			t.Fatalf("storage %s: desired record does not match staged files and inputs: %+v", storage, desired)
		}
		if !slices.Contains(desired.Stacks, "local-path") {
			t.Fatal("record lost the retired stack namespace needed to reject stale trees")
		}
	}
}

func stagedPublicationFixture(t *testing.T) (*k8sfake.Clientset, *ClusterConfig, *NodeState) {
	t.Helper()
	savedDir, savedRecord := version.ManifestsDir, operatorRecordStaged
	t.Cleanup(func() { version.ManifestsDir, operatorRecordStaged = savedDir, savedRecord })
	cs := k8sfake.NewSimpleClientset()
	operatorRecordStaged = func(ctx context.Context, staged operator.StagedGeneration, previous string) (operator.StagingPublication, error) {
		return operator.RecordStagedGeneration(ctx, cs, staged, previous)
	}
	return cs, &ClusterConfig{}, &NodeState{Hostname: "cp1"}
}

func mustActiveStagingPublication(t *testing.T, cs *k8sfake.Clientset) string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps("kube-system").Get(t.Context(), "stratum-addon-inventory", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Desired struct{ PublicationID string }
	}
	if err := json.Unmarshal([]byte(cm.Data["inventory"]), &stored); err != nil {
		t.Fatal(err)
	}
	return stored.Desired.PublicationID
}

func TestWriteStagingRecordLeavesFixedTemporarySymlinkAlone(t *testing.T) {
	previous := version.ManifestsDir
	version.ManifestsDir = t.TempDir()
	t.Cleanup(func() { version.ManifestsDir = previous })
	outside := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(version.ManifestsDir, pendingStagingName)
	if err := os.Symlink(outside, path+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := writeStagingRecord(pendingStagingName, []byte("record")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("staging overwrote an unrelated file: %q, %v", data, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "record" {
		t.Fatalf("record = %q, %v", data, err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("record is not a private regular file: %v, %v", info, err)
	}
}

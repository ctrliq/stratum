package cluster

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/version"
)

func TestWriteBundlesStagesRuntimeClassesOnlyWhileDeclared(t *testing.T) {
	withImporterStacks(t, nil)
	st := &NodeState{Role: "controller", NodeIP: "10.0.0.11"}
	if err := writeBundles(&ClusterConfig{}, st); err != nil {
		t.Fatal(err)
	}
	if _, ok := stagedStacks(t, version.ManifestsDir)[runtimeClassFile]; ok {
		t.Fatal("RuntimeClasses were staged with none declared")
	}

	cfg := &ClusterConfig{Spec: Spec{RuntimeClasses: []string{"nvidia"}}}
	if err := writeBundles(cfg, st); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(version.ManifestsDir, runtimeClassFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(addons.RenderRuntimeClasses(cfg.Spec.RuntimeClasses)) {
		t.Errorf("staged %q, want the rendered RuntimeClasses", got)
	}

	cfg.Spec.RuntimeClasses = nil
	if err := writeBundles(cfg, st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(version.ManifestsDir, addons.RuntimeClassStack)); !os.IsNotExist(err) {
		t.Errorf("the runtime-classes stack is left behind once none are declared: %v", err)
	}
}

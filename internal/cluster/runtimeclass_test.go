package cluster

import (
	"os"
	"path/filepath"
	"strings"
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

// Earlier releases staged nothing in the runtime-classes directory, so a file
// an administrator dropped there is theirs: a start refuses rather than
// overwrite or remove it, naming it, and leaves it as it was.
func TestWriteBundlesRefusesARuntimeClassFileItDidNotStage(t *testing.T) {
	for name, file := range map[string]string{
		"another file":            "gpu.yaml",
		"its name without header": "runtime-classes.yaml",
	} {
		t.Run(name, func(t *testing.T) {
			withImporterStacks(t, nil)
			path := filepath.Join(version.ManifestsDir, addons.RuntimeClassStack, file)
			mine := "apiVersion: node.k8s.io/v1\nkind: RuntimeClass\nmetadata:\n  name: kata\nhandler: kata\n"
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, cfg := range []*ClusterConfig{{}, {Spec: Spec{RuntimeClasses: []string{"nvidia"}}}} {
				err := writeBundles(cfg, &NodeState{Role: "controller", NodeIP: "10.0.0.11"})
				if err == nil || !strings.Contains(err.Error(), path) {
					t.Errorf("runtimeClasses %v: writeBundles = %v, want a refusal naming %s", cfg.Spec.RuntimeClasses, err, path)
				}
				if got, _ := os.ReadFile(path); string(got) != mine {
					t.Errorf("runtimeClasses %v: the administrator's file became %q", cfg.Spec.RuntimeClasses, got)
				}
			}
		})
	}
}

// RuntimeClasses need the operator to publish them, and to remove them once
// published.
func TestRuntimeClassesRefuseTheOperatorOff(t *testing.T) {
	withImporterStacks(t, nil)
	off := false
	st := &NodeState{Role: "controller", NodeIP: "10.0.0.11"}
	noDash := &Dashboard{Enabled: &off}
	if err := writeBundles(&ClusterConfig{Spec: Spec{Operator: &off, Dashboard: noDash, RuntimeClasses: []string{"nvidia"}}}, st); err == nil || !strings.Contains(err.Error(), "spec.runtimeClasses needs the operator") {
		t.Errorf("runtimeClasses with the operator off: writeBundles = %v, want a refusal", err)
	}
	if err := writeBundles(&ClusterConfig{Spec: Spec{Dashboard: noDash, RuntimeClasses: []string{"nvidia"}}}, st); err != nil {
		t.Fatal(err)
	}
	if err := writeBundles(&ClusterConfig{Spec: Spec{Operator: &off, Dashboard: noDash}}, st); err == nil || !strings.Contains(err.Error(), "nothing to remove them") {
		t.Errorf("operator off with RuntimeClasses staged: writeBundles = %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(version.ManifestsDir, runtimeClassFile)); err != nil {
		t.Errorf("the refused start removed the staged RuntimeClasses: %v", err)
	}
	if err := writeBundles(&ClusterConfig{Spec: Spec{Dashboard: noDash}}, st); err != nil {
		t.Fatal(err)
	}
	if err := writeBundles(&ClusterConfig{Spec: Spec{Operator: &off, Dashboard: noDash}}, st); err != nil {
		t.Errorf("operator off once the RuntimeClasses are gone: writeBundles = %v", err)
	}
}

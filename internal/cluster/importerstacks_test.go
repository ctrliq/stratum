package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/version"
)

func withImporterStacks(t *testing.T, f func(StackContext) (map[string][]byte, error)) {
	t.Helper()
	origStacks, origDir, origCilium := ImporterStacks, version.ManifestsDir, version.CiliumSettingsPath
	t.Cleanup(func() {
		ImporterStacks, version.ManifestsDir, version.CiliumSettingsPath = origStacks, origDir, origCilium
	})
	ImporterStacks = f
	version.ManifestsDir = t.TempDir()
	version.CiliumSettingsPath = filepath.Join(t.TempDir(), "cilium.json")
}

func importerFiles(t *testing.T) []string {
	t.Helper()
	var got []string
	for _, rel := range sortedStacks(stagedStacks(t, version.ManifestsDir)) {
		if stack, _, _ := strings.Cut(rel, "/"); !slices.Contains(bundledStacks, stack) {
			got = append(got, rel)
		}
	}
	return got
}

func TestWriteBundlesStagesImporterStacks(t *testing.T) {
	var seen StackContext
	withImporterStacks(t, func(c StackContext) (map[string][]byte, error) {
		seen = c
		return map[string][]byte{"addon/manager.yaml": []byte("kind: ConfigMap\n"), "addon/agent.yml": []byte("kind: Secret\n")}, nil
	})
	st := &NodeState{Role: "controller", NodeIP: "10.0.0.11", ServiceCIDR: "10.43.0.0/16", ClusterDomain: "k8s.example.internal"}
	if err := writeBundles(&ClusterConfig{}, st); err != nil {
		t.Fatal(err)
	}
	if want := (StackContext{ClusterDomain: "k8s.example.internal", ServiceCIDR: "10.43.0.0/16"}); seen != want {
		t.Errorf("the importer was rendered for %+v, want %+v", seen, want)
	}
	staged := stagedStacks(t, version.ManifestsDir)
	if got := string(staged["addon/manager.yaml"]); got != "kind: ConfigMap\n" {
		t.Errorf("addon/manager.yaml = %q, want the importer's bytes", got)
	}
	if got := string(staged["addon/agent.yml"]); got != "kind: Secret\n" {
		t.Errorf("addon/agent.yml = %q, want the importer's bytes: the deployer applies .yml too", got)
	}
	if _, ok := staged["coredns/coredns.yaml"]; !ok {
		t.Error("the bundled stacks were not staged beside the importer's")
	}

	seen = StackContext{}
	if err := writeBundles(&ClusterConfig{}, &NodeState{Role: "controller", NodeIP: "10.0.0.11"}); err != nil {
		t.Fatal(err)
	}
	if want := (StackContext{ClusterDomain: "cluster.local", ServiceCIDR: netdefault.ServiceCIDR}); seen != want {
		t.Errorf("a node with no recorded range was rendered for %+v, want the defaults %+v", seen, want)
	}
}

func TestWriteBundlesRefusesABadImporterPath(t *testing.T) {
	for _, rel := range []string{
		"cilium/cilium.yaml",
		"local-path/extra.yaml",
		"manager.yaml",
		"../manager.yaml",
		"addon/../coredns/coredns.yaml",
		"addon/sub/manager.yaml",
		"/addon/manager.yaml",
		"addon/manager.json",
		".hidden/manager.yaml",
		"addon/.applied.yaml",
		"addon//manager.yaml",
		"Addon/manager.yaml",
		"addon ops/manager.yaml",
		"addon:ops/manager.yaml",
		"-addon/manager.yaml",
		"addon-/manager.yaml",
		strings.Repeat("f", 64) + "/manager.yaml",
	} {
		t.Run(rel, func(t *testing.T) {
			withImporterStacks(t, func(StackContext) (map[string][]byte, error) {
				return map[string][]byte{rel: []byte("x"), "addon/ok.yaml": []byte("x")}, nil
			})
			err := writeBundles(&ClusterConfig{}, &NodeState{Role: "controller", NodeIP: "10.0.0.11"})
			if err == nil || !strings.Contains(err.Error(), "importer stack file") {
				t.Fatalf("writeBundles = %v, want a refusal of %q", err, rel)
			}
			if got := stagedStacks(t, version.ManifestsDir); len(got) != 0 {
				t.Errorf("a refused render staged %v; it must stage nothing", sortedStacks(got))
			}
		})
	}
}

func TestWriteBundlesFailsWhenTheImporterFails(t *testing.T) {
	boom := errors.New("no addon config")
	withImporterStacks(t, func(StackContext) (map[string][]byte, error) { return nil, boom })
	if err := writeBundles(&ClusterConfig{}, &NodeState{Role: "controller", NodeIP: "10.0.0.11"}); !errors.Is(err, boom) {
		t.Fatalf("writeBundles = %v, want the importer's error", err)
	}
}

func TestWriteBundlesRemovesDroppedImporterFiles(t *testing.T) {
	var returns map[string][]byte
	withImporterStacks(t, func(StackContext) (map[string][]byte, error) { return returns, nil })
	st := &NodeState{Role: "controller", NodeIP: "10.0.0.11"}
	start := func(want ...string) {
		t.Helper()
		if err := writeBundles(&ClusterConfig{}, st); err != nil {
			t.Fatal(err)
		}
		if got := importerFiles(t); !slices.Equal(got, want) {
			t.Fatalf("staged importer files %v, want %v", got, want)
		}
	}

	returns = map[string][]byte{"addon/a.yaml": []byte("a"), "addon/b.yaml": []byte("b"), "extra/c.yaml": []byte("c")}
	start(".importer-stacks.json", "addon/a.yaml", "addon/b.yaml", "extra/c.yaml")
	returns = map[string][]byte{"addon/a.yaml": []byte("a")}
	start(".importer-stacks.json", "addon/a.yaml")
	if _, err := os.Stat(filepath.Join(version.ManifestsDir, "extra")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied extra stack directory was left behind: %v", err)
	}
	returns = nil
	start()
	if _, err := os.Stat(filepath.Join(version.ManifestsDir, "addon")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied addon stack directory was left behind: %v", err)
	}

	returns = map[string][]byte{"addon/a.yaml": []byte("a")}
	start(".importer-stacks.json", "addon/a.yaml")
	ImporterStacks = nil
	start()
}

func TestWriteBundlesKeepsBundledFilesAnIndexNames(t *testing.T) {
	withImporterStacks(t, nil)
	if err := os.WriteFile(filepath.Join(version.ManifestsDir, importerIndex), []byte(`["coredns/coredns.yaml","../x.yaml"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(version.ManifestsDir), "x.yaml")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeBundles(&ClusterConfig{}, &NodeState{Role: "controller", NodeIP: "10.0.0.11"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(version.ManifestsDir, "coredns/coredns.yaml")); err != nil {
		t.Errorf("a bundled file named in the importer index was removed: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file outside the manifests dir named in the index was removed: %v", err)
	}
}

func TestBundledStacksNamesEveryStackWriteBundlesStages(t *testing.T) {
	withImporterStacks(t, nil)
	cfg := &ClusterConfig{Spec: Spec{Storage: Storage{Default: addons.StorageLocalPath}}}
	if err := writeBundles(cfg, &NodeState{Role: "controller", NodeIP: "10.0.0.11"}); err != nil {
		t.Fatal(err)
	}
	if got := importerFiles(t); len(got) != 0 {
		t.Errorf("writeBundles staged %v outside bundledStacks, so an importer could overwrite them", got)
	}
}

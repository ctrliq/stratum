package cli

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/version"
)

func TestWithStacksReachesTheControlPlaneStart(t *testing.T) {
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
		// `up` refuses an unstamped build before it starts, and this test
		// is about the stacks hook reaching that start, not about the
		// stamp, so it runs stamped.
		{&version.K8sVersion, "v1.37.1"},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	oldInstall, oldRunController, oldPrepare, oldStacks := installUnit, runControllerFn, prepareControllerFn, cluster.ImporterStacks
	t.Cleanup(func() {
		installUnit, runControllerFn, prepareControllerFn, cluster.ImporterStacks = oldInstall, oldRunController, oldPrepare, oldStacks
	})
	old := syscall.Umask(0)
	syscall.Umask(old)
	t.Cleanup(func() { syscall.Umask(old) })

	installUnit = func([]string) error { return nil }
	prepareControllerFn = func(context.Context, *cluster.ClusterConfig, string, *k8sutil.MaintenanceLease, func()) (*cluster.NodeState, error) {
		return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
	}
	var got map[string][]byte
	runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error {
		if cluster.ImporterStacks == nil {
			return errors.New("the control-plane start has no importer stacks")
		}
		var err error
		got, err = cluster.ImporterStacks(cluster.StackContext{ClusterDomain: "d", ServiceCIDR: "c"})
		return err
	}

	var seen StackContext
	root := NewRootCommand(WithStacks(func(c StackContext) (map[string][]byte, error) {
		seen = c
		return map[string][]byte{"addon/a.yaml": []byte("a")}, nil
	}))
	root.SetArgs([]string{"up", "--config", writeEmptyConfig(t, tmp)})
	if err := root.Execute(); err != nil {
		t.Fatalf("up = %v", err)
	}
	if want := map[string][]byte{"addon/a.yaml": []byte("a")}; !maps.EqualFunc(got, want, func(a, b []byte) bool { return string(a) == string(b) }) {
		t.Errorf("the start rendered %v, want %v", got, want)
	}
	if want := (StackContext{ClusterDomain: "d", ServiceCIDR: "c"}); seen != want {
		t.Errorf("the importer saw %+v, want %+v", seen, want)
	}
}

func TestImporterStacksMergesAndRefusesDuplicates(t *testing.T) {
	if importerStacks(nil) != nil {
		t.Error("no importer stacks must leave the cluster hook unset")
	}
	one := func(rel string) StackFunc {
		return func(StackContext) (map[string][]byte, error) { return map[string][]byte{rel: []byte(rel)}, nil }
	}
	cfg := rootConfig{}
	for _, o := range []Option{WithStacks(one("a/x.yaml")), WithStacks(nil), WithStacks(one("b/x.yaml"))} {
		o(&cfg)
	}
	got, err := importerStacks(cfg.stacks)(cluster.StackContext{})
	if err != nil || len(got) != 2 || string(got["a/x.yaml"]) != "a/x.yaml" || string(got["b/x.yaml"]) != "b/x.yaml" {
		t.Errorf("merged = %v, %v; want both functions' files", got, err)
	}

	if _, err := importerStacks([]StackFunc{one("a/x.yaml"), one("a/x.yaml")})(cluster.StackContext{}); err == nil || !strings.Contains(err.Error(), "a/x.yaml") {
		t.Errorf("a file two functions return = %v, want a refusal naming it", err)
	}
	boom := errors.New("boom")
	failing := func(StackContext) (map[string][]byte, error) { return nil, boom }
	if _, err := importerStacks([]StackFunc{one("a/x.yaml"), failing})(cluster.StackContext{}); !errors.Is(err, boom) {
		t.Errorf("a failing function = %v, want its error", err)
	}
}

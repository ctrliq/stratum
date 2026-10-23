package upgrade

import (
	"io"

	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/supervisor"
)

// hostConfigFixture redirects every seam applyHostConfigs reaches and records
// what it did, in order. reload answers per component; a component missing
// from active is not running.
func hostConfigFixture(t *testing.T, active map[string]bool, writeErr error, reload func(string) error) *[]string {
	t.Helper()
	var calls []string
	oldAct, oldReload, oldRestart := unitActive, reloadUnit, restartUnit
	oldWrite, oldHeal, oldPresent := writeNodeConfigs, removeLegacyConfig, legacyConfigPresent
	t.Cleanup(func() {
		unitActive, reloadUnit, restartUnit = oldAct, oldReload, oldRestart
		writeNodeConfigs, removeLegacyConfig, legacyConfigPresent = oldWrite, oldHeal, oldPresent
	})
	unitActive = func(_ context.Context, name string) bool { return active[name] }
	reloadUnit = func(name string, _ supervisor.ProcSpec) error {
		calls = append(calls, "reload "+name)
		return reload(name)
	}
	restartUnit = func(name string) error {
		calls = append(calls, "restart "+name)
		return nil
	}
	writeNodeConfigs = func(io.Writer, *nodestate.NodeState) error {
		calls = append(calls, "write configs")
		return writeErr
	}
	removeLegacyConfig = func(io.Writer) { calls = append(calls, "heal") }
	legacyConfigPresent = func() (string, bool) { return "/etc/containerd/config.toml", true }
	return &calls
}

// TestApplyHostConfigsHealsOnlyAfterContainerdReloads pins the reload-then-heal order:
// the legacy /etc/containerd/config.toml is deleted only once the supervisor
// holds containerd's new argv. A supervisor that predates reload, a containerd
// the reload skipped, a failed reload and a failed config write all leave the
// supervisor passing the legacy path, and deleting it there left containerd
// unable to ever start again on the upgraded node.
func TestApplyHostConfigsHealsOnlyAfterContainerdReloads(t *testing.T) {
	both := map[string]bool{"containerd": true, "kubelet": true}
	ok := func(string) error { return nil }
	for _, tc := range []struct {
		name     string
		active   map[string]bool
		writeErr error
		reload   func(string) error
		want     []string
		fatal    bool
	}{
		{
			name: "reload lands, then the heal", active: both, reload: ok,
			want: []string{"write configs", "reload containerd", "reload kubelet", "heal"},
		},
		{
			name: "supervisor predates reload", active: both,
			reload: func(string) error {
				return fmt.Errorf("%w: unknown op \"reload\"", supervisor.ErrReloadUnsupported)
			},
			want: []string{"write configs", "reload containerd"},
		},
		{
			name: "containerd not running, so not reloaded", active: map[string]bool{"kubelet": true}, reload: ok,
			want: []string{"write configs", "reload kubelet"},
		},
		{
			name: "reload fails", active: both, fatal: true,
			reload: func(string) error { return errors.New("connection reset") },
			want:   []string{"write configs", "reload containerd"},
		},
		{
			name: "config write fails", active: both, reload: ok, writeErr: errors.New("disk full"),
			want: []string{"write configs"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := hostConfigFixture(t, tc.active, tc.writeErr, tc.reload)
			err := applyHostConfigs(io.Discard, &nodestate.NodeState{Role: "controller"})
			if tc.fatal != (err != nil) {
				t.Fatalf("err = %v, want fatal=%v", err, tc.fatal)
			}
			if !reflect.DeepEqual(*calls, tc.want) {
				t.Errorf("calls = %q, want %q", *calls, tc.want)
			}
		})
	}
}

// When the heal is withheld the operator is told the file was kept and what
// removes it — otherwise a kept host file is as silent as the deleted one was.
func TestApplyHostConfigsSaysWhenItKeepsTheLegacyConfig(t *testing.T) {
	hostConfigFixture(t, map[string]bool{"containerd": true, "kubelet": true}, nil, func(string) error {
		return fmt.Errorf("%w: unknown op \"reload\"", supervisor.ErrReloadUnsupported)
	})
	out, err := runWriter(t, func(w io.Writer) error {
		return applyHostConfigs(w, &nodestate.NodeState{Role: "controller"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kept /etc/containerd/config.toml") || !strings.Contains(out, "stratum up") {
		t.Errorf("output does not say the legacy config was kept and what removes it:\n%s", out)
	}
}

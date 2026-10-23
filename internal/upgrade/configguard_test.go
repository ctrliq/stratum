package upgrade

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// TestUpgradeRefusesAControlPlaneThatWouldRestartOnAMissingConfig: a control
// plane whose unit restarts it with a --config that no longer exists is
// refused before anything is changed, in every mode — with the node state's
// record empty, as a build from before the record or a start whose best-effort
// save failed leaves it. An upgrade that went ahead would hand over into
// `up --config <missing>`, refused by the new build on every restart,
// leaving the node down with nothing for rollback to stand on.
//
// A marker from an upgrade that changed nothing is seeded because a real
// upgrade removes it before it stages anything: the one write a guard placed
// too late would make with these fixtures, which carry no payload.
func TestUpgradeRefusesAControlPlaneThatWouldRestartOnAMissingConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    NodeOptions
	}{
		{"upgrade", NodeOptions{}},
		{"upgrade --check", NodeOptions{Check: true}},
		{"upgrade --dry-run", NodeOptions{DryRun: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resumeFixture(t)
			missing := filepath.Join(t.TempDir(), "nope.yaml")
			writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"), "role: controller\n", 0o600)
			writeFile(t, stratumInstallPath, "installed", 0o755)
			swap(t, &unitArgv, func() ([]string, error) {
				return []string{stratumInstallPath, "up", "--config", missing}, nil
			})
			if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.8"}); err != nil {
				t.Fatal(err)
			}

			err := UpgradeNode(io.Discard, tc.o)
			next := "Then upgrade again; nothing was changed"
			if tc.o.Check {
				next = "Then check again; nothing was changed"
			}
			for _, want := range []string{"--config " + missing, "no longer exists", "sudo systemctl stop stratum", "sudo systemctl start stratum", next} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
			if m, err := readUpgradeMarker(); err != nil || m == nil || m.TS != resumeTS {
				t.Errorf("marker after the refusal = %+v, %v; want the seeded one untouched", m, err)
			}
			if sets, err := scanBackupSets(); err != nil || len(sets) != 0 {
				t.Errorf("backup sets after the refusal = %v, %v; want none", sets, err)
			}
			if got, err := os.ReadFile(stratumInstallPath); err != nil || string(got) != "installed" {
				t.Errorf("installed stratum after the refusal = %q, %v; want it untouched", got, err)
			}
		})
	}
}

// TestUpgradeRefusalAfterAnInterruptedUpgradeSaysSo: a run over an
// interrupted upgrade is refused the same way, but "nothing was changed" would
// be untrue — the interrupted run already changed the node — and what to do
// next depends on whether that upgrade was to this target: only then does
// upgrading again finish it; another target's is finished with its own binary
// or rolled back (refuseOtherInterruptedUpgrade).
func TestUpgradeRefusalAfterAnInterruptedUpgradeSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name, target, want, not string
	}{
		{"to this target", version.K8sVersion, "Then upgrade again, which finishes the interrupted upgrade", "stratum rollback"},
		{"to another target", "v1.0.0", "with that upgrade binary, or undo it with `stratum rollback`, and retry", "Then upgrade again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resumeFixture(t)
			missing := filepath.Join(t.TempDir(), "nope.yaml")
			writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"), "role: controller\n", 0o600)
			writeFile(t, stratumInstallPath, "installed", 0o755)
			swap(t, &unitArgv, func() ([]string, error) {
				return []string{stratumInstallPath, "up", "--config", missing}, nil
			})
			interruptedAt(t, tc.target)

			err := UpgradeNode(io.Discard, NodeOptions{})
			for _, want := range []string{"no longer exists", "the interrupted upgrade to " + tc.target, tc.want} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
			for _, not := range []string{"nothing was changed", tc.not} {
				if err != nil && strings.Contains(err.Error(), not) {
					t.Errorf("error says %q: %v", not, err)
				}
			}
			if m, err := readUpgradeMarker(); err != nil || m == nil || m.TS != resumeTS {
				t.Errorf("marker after the refusal = %+v, %v; want the interrupted run's untouched", m, err)
			}
		})
	}
}

// TestRestartConfigMissing: the unit's and the running supervisor's --config
// both decide, because the handover restarts with one and then the other; the
// node state's record is used only when neither can be read, and a stale
// record never outvotes them.
func TestRestartConfigMissing(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "config.yaml")
	writeFile(t, present, "", 0o600)
	missing := filepath.Join(dir, "nope.yaml")
	refused := filepath.Join(dir, "refused.yaml")
	writeFile(t, refused, "spec:\n  snapshots:\n    retain: -1\n", 0o600)
	unit := func(args ...string) func() ([]string, error) {
		return func() ([]string, error) { return append([]string{stratumInstallPath, "up"}, args...), nil }
	}
	sup := func(cwd string, args ...string) func() ([]string, string, error) {
		return func() ([]string, string, error) { return append([]string{stratumInstallPath, "up"}, args...), cwd, nil }
	}
	noUnit := func() ([]string, error) { return nil, fs.ErrNotExist }
	unreadable := func() ([]string, error) { return nil, errors.New("drop-in overrides the unit's ExecStart=") }
	noSup := func() ([]string, string, error) { return nil, "", errors.New("no supervisor") }
	masked := func() ([]string, error) { return nil, fmt.Errorf("stratum.service is masked: %w", fs.ErrNotExist) }
	for _, tc := range []struct {
		name     string
		role     string
		recorded string
		unit     func() ([]string, error)
		sup      func() ([]string, string, error)
		refused  string
	}{
		{"unit names a missing file, nothing recorded", "controller", "", unit("--config", missing), noSup, "the unit's ExecStart"},
		{"unit names a missing file, --config= spelling", "controller", "", unit("--config=" + missing), noSup, "the unit's ExecStart"},
		{"unit names a missing file, a present one recorded", "controller", present, unit("--config", missing), noSup, "the unit's ExecStart"},
		{"unit names a present file, a missing one recorded", "controller", missing, unit("--config", present), noSup, ""},
		{"unit names the default path, which may be absent", "controller", "", unit("--config", cluster.DefaultConfigPath), noSup, ""},
		{"unit names the default path, spelled unclean", "controller", "", unit("--config", "/etc/stratum//./config.yaml"), noSup, ""},
		{"unit gives an empty --config", "controller", "", unit("--config", ""), noSup, "empty --config"},
		{"unit carries no --config", "controller", missing, unit("--cis"), noSup, ""},
		{"last --config wins", "controller", "", unit("--config", present, "--config", missing), noSup, "the unit's ExecStart"},
		{"unit present, supervisor runs a deleted file", "controller", "", unit("--config", present), sup("/", "--config", missing), "the running supervisor"},
		{"supervisor's relative path, from its own directory", "controller", "", noUnit, sup(dir, "--config", "nope.yaml"), "the running supervisor"},
		{"supervisor's empty --config is the default to a re-exec", "controller", "", noUnit, sup("/", "--config", ""), ""},
		{"both present", "controller", missing, unit("--config", present), sup("/", "--config", present), ""},
		{"neither readable, recorded file missing", "controller", missing, noUnit, noSup, "the node state"},
		{"no unit or supervisor, recorded file present", "controller", present, noUnit, noSup, ""},
		{"neither readable, nothing recorded", "controller", "", noUnit, noSup, ""},
		// A unit that exists but cannot be read is refused, never skipped: a
		// supervisor that predates re-exec is restarted through systemd onto
		// that unchecked command — here a drop-in naming a missing file while
		// the running supervisor's own file exists.
		{"unreadable unit, supervisor runs a present file", "controller", present, unreadable, sup("/", "--config", present), "cannot tell which --config"},
		{"unreadable unit, supervisor runs a missing file", "controller", present, unreadable, sup("/", "--config", missing), "cannot tell which --config"},
		{"unreadable unit, no supervisor", "controller", present, unreadable, noSup, "drop-in overrides the unit's ExecStart="},
		// host.UnitArgv reports a masked unit as fs.ErrNotExist: systemd never
		// starts it, so nothing restarts the node through it.
		{"masked unit, supervisor runs a present file", "controller", missing, masked, sup("/", "--config", present), ""},
		{"worker", "worker", missing, unit("--config", missing), sup("/", "--config", missing), ""},
		// A file that is there but that the upgraded build refuses to load is
		// refused like a missing one, from whichever source names it.
		{"unit names a file the upgraded build refuses", "controller", "", unit("--config", refused), noSup, "the unit's ExecStart"},
		{"supervisor runs a file the upgraded build refuses", "controller", "", unit("--config", present), sup("/", "--config", refused), "the running supervisor"},
		{"neither readable, recorded file refused", "controller", refused, noUnit, noSup, "the node state"},
		{"supervisor's relative path to a refused file, from its own directory", "controller", "", noUnit, sup(dir, "--config", "refused.yaml"), "the running supervisor"},
		{"worker with a file a controller would refuse", "worker", refused, unit("--config", refused), sup("/", "--config", refused), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swap(t, &unitArgv, tc.unit)
			swap(t, &supervisorArgv, tc.sup)
			err := restartConfigMissing(&nodestate.NodeState{Role: tc.role, ConfigPath: tc.recorded})
			if (err != nil) != (tc.refused != "") {
				t.Fatalf("restartConfigMissing() = %v, want refused: %v", err, tc.refused != "")
			}
			if err != nil && !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("refusal does not name %q: %v", tc.refused, err)
			}
		})
	}
}

// TestRestartConfigMissingLoadsTheDefaultConfig: the default path is exempt
// only from the missing-file refusal. A control plane restarted with no
// --config, an empty one, or the default spelled out runs on the default
// file, so the upgraded build's load of it is checked too, and a refusal
// names the path. TestMain keeps the real load off this host's own default
// config, so this swaps in one that refuses it.
func TestRestartConfigMissingLoadsTheDefaultConfig(t *testing.T) {
	unit := func(args ...string) func() ([]string, error) {
		return func() ([]string, error) { return append([]string{stratumInstallPath, "up"}, args...), nil }
	}
	noSup := func() ([]string, string, error) { return nil, "", errors.New("no supervisor") }
	for _, tc := range []struct {
		name     string
		recorded string
		unit     func() ([]string, error)
		sup      func() ([]string, string, error)
	}{
		{"unit carries no --config", "", unit("--cis"), noSup},
		{"unit names the default path", "", unit("--config", cluster.DefaultConfigPath), noSup},
		{"supervisor's empty --config", "", func() ([]string, error) { return nil, fs.ErrNotExist },
			func() ([]string, string, error) { return []string{stratumInstallPath, "up", "--config", ""}, "/", nil }},
		{"neither readable, nothing recorded", "", func() ([]string, error) { return nil, fs.ErrNotExist }, noSup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swap(t, &unitArgv, tc.unit)
			swap(t, &supervisorArgv, tc.sup)
			var loaded []string
			swap(t, &restartConfigLoads, func(path string) error {
				loaded = append(loaded, path)
				return errors.New("spec.snapshots.retain -1 is negative")
			})
			err := restartConfigMissing(&nodestate.NodeState{Role: "controller", ConfigPath: tc.recorded})
			if !slices.Equal(loaded, []string{cluster.DefaultConfigPath}) {
				t.Errorf("loaded %q, want only the default config", loaded)
			}
			for _, want := range []string{"--config " + cluster.DefaultConfigPath, "refuses to start on", "spec.snapshots.retain -1 is negative"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
		})
	}
}

// TestUpgradeRefusesAControlPlaneWhoseConfigTheUpgradedBuildRefuses: a
// control plane whose config the earlier build ran on but this build refuses
// at load — here a negative spec.snapshots.retain, which earlier builds
// accepted — is refused before anything is changed, as a missing config is.
// Without the check the handover restarted `stratum up` on a file the new
// build refuses on every restart, leaving the node down with nothing for
// rollback to stand on.
func TestUpgradeRefusesAControlPlaneWhoseConfigTheUpgradedBuildRefuses(t *testing.T) {
	resumeFixture(t)
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, cfg, "spec:\n  snapshots:\n    retain: -1\n", 0o600)
	writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"), "role: controller\n", 0o600)
	writeFile(t, stratumInstallPath, "installed", 0o755)
	swap(t, &unitArgv, func() ([]string, error) {
		return []string{stratumInstallPath, "up", "--config", cfg}, nil
	})
	if err := writeUpgradeMarker(io.Discard, upgradeMarker{TS: resumeTS, Target: "v1.37.8"}); err != nil {
		t.Fatal(err)
	}

	err := UpgradeNode(io.Discard, NodeOptions{})
	for _, want := range []string{"--config " + cfg, "spec.snapshots.retain -1 is negative", "Then upgrade again; nothing was changed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if m, err := readUpgradeMarker(); err != nil || m == nil || m.TS != resumeTS {
		t.Errorf("marker after the refusal = %+v, %v; want the seeded one untouched", m, err)
	}
	if sets, err := scanBackupSets(); err != nil || len(sets) != 0 {
		t.Errorf("backup sets after the refusal = %v, %v; want none", sets, err)
	}
	if got, err := os.ReadFile(stratumInstallPath); err != nil || string(got) != "installed" {
		t.Errorf("installed stratum after the refusal = %q, %v; want it untouched", got, err)
	}
}

// TestRestartConfigMissingResolvesARelativeUnitPathFromRoot: systemd starts
// the service in /, so a relative --config an older build wrote names a file
// under /, not under the upgrade's own working directory.
func TestRestartConfigMissingResolvesARelativeUnitPathFromRoot(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.yaml"), "", 0o600)
	t.Chdir(dir)
	swap(t, &unitArgv, func() ([]string, error) {
		return []string{stratumInstallPath, "up", "--config", "config.yaml"}, nil
	})
	err := restartConfigMissing(&nodestate.NodeState{Role: "controller"})
	if err == nil || !strings.Contains(err.Error(), "--config /config.yaml") {
		t.Fatalf("restartConfigMissing() = %v, want the relative path refused as /config.yaml", err)
	}
}

// TestRunningSupervisorArgvReadsProc points the supervisor lookup at this test
// process, whose argv and working directory are known.
func TestRunningSupervisorArgvReadsProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	swap(t, &runningSupervisor, func() (comps.RunningSupervisor, error) {
		return comps.RunningSupervisor{PID: os.Getpid()}, nil
	})
	argv, cwd, err := runningSupervisorArgv()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(argv, os.Args) {
		t.Errorf("argv = %q, want %q", argv, os.Args)
	}
	if want, _ := os.Getwd(); cwd != want {
		t.Errorf("cwd = %q, want %q", cwd, want)
	}
}

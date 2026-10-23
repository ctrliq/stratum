package cli

import (
	"errors"
	"io"
	"testing"

	"github.com/ctrliq/stratum/internal/etcdsnap"
)

// An unreadable snapshot.conf makes the load fail with the defaults alongside
// the error. `schedule` must refuse and write nothing rather than
// write the defaults over the operator's file.
func TestSnapshotScheduleRefusesToOverwriteAnUnreadableConfig(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	boom := errors.New("snapshot.conf: permission denied")
	scheduleLoadConfig = func() (etcdsnap.Config, error) { return etcdsnap.DefaultConfig(), boom }
	saved := false
	scheduleSaveConfig = func(io.Writer, etcdsnap.Config) error { saved = true; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--retain", "3"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if !errors.Is(err, boom) {
		t.Fatalf("schedule returned %v, want an error wrapping the load failure", err)
	}
	if saved {
		t.Error("schedule wrote a config over the one it could not read")
	}
}

// The --snapshots flag does not disable anything: false only swaps the
// confirmation for the disable note, and the config is saved either way. The
// help must say so, or operators will think =false stops scheduled snapshots.
func TestSnapshotScheduleSnapshotsFlagHelp(t *testing.T) {
	const wantUsage = "false prints how to disable scheduled snapshots instead of the update confirmation; it does not disable them, and the config is saved either way"

	f := newSnapshotScheduleCommand().Flags().Lookup("snapshots")
	if f == nil {
		t.Fatal("schedule command has no --snapshots flag")
	}
	if f.DefValue != "true" {
		t.Errorf("--snapshots DefValue = %q, want \"true\" (released scripts pass the flag)", f.DefValue)
	}
	if f.Value.Type() != "bool" {
		t.Errorf("--snapshots value type = %T (%v), want a bool flag", f.Value, f.Value)
	}
	if f.Usage != wantUsage {
		t.Errorf("--snapshots usage = %q, want %q", f.Usage, wantUsage)
	}
}

// --snapshots=false must not skip the save either: the help says the config is
// saved whether the flag is set or not.
func TestSnapshotScheduleSavesWhenSnapshotsDisabled(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	scheduleLoadConfig = func() (etcdsnap.Config, error) { return etcdsnap.Config{Retain: 10}, nil }
	var got etcdsnap.Config
	saved := false
	scheduleSaveConfig = func(w io.Writer, c etcdsnap.Config) error { saved, got = true, c; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--retain", "3", "--snapshots=false"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !saved {
		t.Fatal("schedule with --snapshots=false did not save the config")
	}
	if got.Retain != 3 {
		t.Errorf("saved retain = %d, want 3", got.Retain)
	}
}

func TestSnapshotScheduleSavesWhenTheConfigLoads(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	scheduleLoadConfig = func() (etcdsnap.Config, error) { return etcdsnap.Config{Retain: 10}, nil }
	var got etcdsnap.Config
	scheduleSaveConfig = func(w io.Writer, c etcdsnap.Config) error { got = c; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--retain", "3"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.Retain != 3 {
		t.Errorf("saved retain = %d, want 3", got.Retain)
	}
}

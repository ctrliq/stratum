package cli

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/objstore"
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

// The configured S3 target the tests below seed the command from.
func configuredS3() etcdsnap.Config {
	return etcdsnap.Config{S3: objstore.Target{
		Bucket: "backups", Endpoint: "http://minio:9000", Region: "us-east-1", Prefix: "stratum", PathStyle: true,
	}}
}

// A --s3-* flag passed alone over an already-configured bucket must update its
// one field and leave the rest of the configured target alone: the flag exists
// precisely so an operator can change the region without restating everything.
func TestSnapshotScheduleS3RegionAloneUpdatesOnlyTheRegion(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	scheduleLoadConfig = func() (etcdsnap.Config, error) { return configuredS3(), nil }
	var got etcdsnap.Config
	saved := false
	scheduleSaveConfig = func(w io.Writer, c etcdsnap.Config) error { saved, got = true, c; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--s3-region", "us-west-2"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !saved {
		t.Fatal("schedule did not save the config")
	}
	want := configuredS3().S3
	want.Region = "us-west-2"
	if got.S3 != want {
		t.Errorf("saved S3 = %+v, want %+v (only the region changes)", got.S3, want)
	}
}

// Passing --s3-bucket alone must not wipe the configured endpoint/region/
// prefix/path-style back to the flag defaults: unset flags are "not stated",
// not "set to empty".
func TestSnapshotScheduleBucketAloneKeepsTheConfiguredS3Fields(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	scheduleLoadConfig = func() (etcdsnap.Config, error) { return configuredS3(), nil }
	var got etcdsnap.Config
	var saved bool
	scheduleSaveConfig = func(w io.Writer, c etcdsnap.Config) error { saved, got = true, c; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--s3-bucket", "other-bucket"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !saved {
		t.Fatal("schedule did not save the config")
	}
	want := configuredS3().S3
	want.Bucket = "other-bucket"
	if got.S3 != want {
		t.Errorf("saved S3 = %+v, want %+v (a new bucket alone keeps endpoint/region/prefix/path-style)", got.S3, want)
	}
}

// An S3 detail flag with no bucket anywhere is refused before anything is
// saved: an endpoint or region without a bucket configures nothing, and the
// silent no-op made operators believe the setting took.
func TestSnapshotScheduleS3FlagWithoutAnyBucketIsRefused(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	scheduleLoadConfig = func() (etcdsnap.Config, error) { return etcdsnap.DefaultConfig(), nil }
	var saved bool
	scheduleSaveConfig = func(w io.Writer, c etcdsnap.Config) error { saved = true; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--s3-endpoint", "http://minio:9000"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("--s3-endpoint without any bucket was accepted")
	}
	if !strings.Contains(err.Error(), "--s3-bucket") {
		t.Errorf("error %q does not name --s3-bucket", err)
	}
	if saved {
		t.Error("schedule wrote a config for a target it refused")
	}
}

// Changed, not value, decides: an explicit --s3-path-style=false over a
// configured true must save false, though the flag's default is also false.
func TestSnapshotScheduleExplicitPathStyleFalseWins(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	scheduleLoadConfig = func() (etcdsnap.Config, error) { return configuredS3(), nil }
	var got etcdsnap.Config
	scheduleSaveConfig = func(w io.Writer, c etcdsnap.Config) error { got = c; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--s3-path-style=false"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := configuredS3().S3
	want.PathStyle = false
	if got.S3 != want {
		t.Errorf("saved S3 = %+v, want %+v (an explicit --s3-path-style=false overrides the configured true)", got.S3, want)
	}
}

// An explicitly empty --s3-bucket is refused, even over an already-configured
// bucket: saving it would clear the bucket and with it the omitted S3 details
// (SaveConfig serializes them only while a bucket is set), and a lone
// --s3-region us-west-2 alongside it would then configure nothing. The way to
// stop S3 uploads is to drop the S3 target from /etc/stratum/snapshot.conf.
func TestSnapshotScheduleEmptyBucketOverConfiguredBucketIsRefused(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	scheduleLoadConfig = func() (etcdsnap.Config, error) { return configuredS3(), nil }
	var saved bool
	scheduleSaveConfig = func(io.Writer, etcdsnap.Config) error { saved = true; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--s3-bucket=", "--s3-region", "us-west-2"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("an empty --s3-bucket over a configured bucket was accepted")
	}
	if !strings.Contains(err.Error(), "--s3-bucket") {
		t.Errorf("error %q does not name --s3-bucket", err)
	}
	if !strings.Contains(err.Error(), etcdsnap.ConfigPath) {
		t.Errorf("error %q does not say where to stop S3 uploads instead (%s)", err, etcdsnap.ConfigPath)
	}
	if saved {
		t.Error("schedule wrote a config for a target it refused")
	}
}

// A lone empty --s3-bucket is refused by the same rule: it would clear the
// configured bucket and lose the configured endpoint/region/prefix/path-style
// from disk.
func TestSnapshotScheduleLoneEmptyBucketIsRefused(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	scheduleLoadConfig = func() (etcdsnap.Config, error) { return configuredS3(), nil }
	var saved bool
	scheduleSaveConfig = func(io.Writer, etcdsnap.Config) error { saved = true; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--s3-bucket="})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("a lone empty --s3-bucket was accepted")
	}
	if !strings.Contains(err.Error(), "--s3-bucket") {
		t.Errorf("error %q does not name --s3-bucket", err)
	}
	if saved {
		t.Error("schedule wrote a config for a target it refused")
	}
}

// Whitespace is not a bucket either: --s3-bucket="  " is refused like an
// empty value.
func TestSnapshotScheduleWhitespaceBucketIsRefused(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	scheduleLoadConfig = func() (etcdsnap.Config, error) { return configuredS3(), nil }
	var saved bool
	scheduleSaveConfig = func(io.Writer, etcdsnap.Config) error { saved = true; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--s3-bucket", "  "})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil {
		t.Fatal("a whitespace-only --s3-bucket was accepted")
	}
	if !strings.Contains(err.Error(), "--s3-bucket") {
		t.Errorf("error %q does not name --s3-bucket", err)
	}
	if saved {
		t.Error("schedule wrote a config for a target it refused")
	}
}

// The base behaviour the fix keeps: every flag passed together sets every
// field.
func TestSnapshotScheduleAllS3FlagsTogetherSetEveryField(t *testing.T) {
	oldLoad, oldSave := scheduleLoadConfig, scheduleSaveConfig
	t.Cleanup(func() { scheduleLoadConfig, scheduleSaveConfig = oldLoad, oldSave })

	scheduleLoadConfig = func() (etcdsnap.Config, error) { return etcdsnap.DefaultConfig(), nil }
	var got etcdsnap.Config
	scheduleSaveConfig = func(w io.Writer, c etcdsnap.Config) error { got = c; return nil }

	cmd := newSnapshotScheduleCommand()
	cmd.SetArgs([]string{"--s3-bucket", "b", "--s3-endpoint", "http://minio:9000", "--s3-region", "us-west-2", "--s3-prefix", "pre", "--s3-path-style=true"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	want := objstore.Target{Bucket: "b", Endpoint: "http://minio:9000", Region: "us-west-2", Prefix: "pre", PathStyle: true}
	if got.S3 != want {
		t.Errorf("saved S3 = %+v, want %+v", got.S3, want)
	}
}

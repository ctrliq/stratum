package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/interrupt"
	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
	"github.com/ctrliq/stratum/internal/objstore"
)

func newSnapshotCommand() *cobra.Command {
	return newGroupCommand("snapshot", "Scheduled etcd snapshots, S3 offload, and full-cluster restore",
		newSnapshotSaveCommand(),
		newSnapshotListCommand(),
		newSnapshotPruneCommand(),
		newSnapshotRestoreCommand(),
		newSnapshotPruneRestoresCommand(),
		newSnapshotRejoinCommand(),
		newSnapshotScheduleCommand(),
	)
}

// Test seam for restore's bare names; save, list and prune take the dir the scheduled loop owns.
var snapshotDir = cluster.LoadSnapshotDir

// Test seams: tests must not touch the host's snapshot.conf or etcd.
var (
	loadSnapshotConfig = cluster.LoadSnapshotConfig
	saveSnapshot       = etcdsnap.SaveContext
	s3Download         = (*objstore.Target).Download
)

func newSnapshotSaveCommand() *cobra.Command {
	var name string
	var scheduled bool
	cmd := &cobra.Command{
		Use:   "save",
		Short: "Take an etcd snapshot now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadSnapshotConfig()
			if err != nil {
				return err
			}
			// cmd.Context() is Background; this context stops the save at a step boundary on the first signal.
			ctx, stop := interrupt.DerivedContext(cmd.Context())
			defer stop()
			p, err := saveSnapshot(ctx, os.Stdout, etcdsnap.SaveOptions{Dir: cfg.Dir, Name: name, Retain: cfg.Retain, S3: cfg.S3Target()})
			if err != nil {
				if interrupt.ExpectedStop(ctx, err) {
					fmt.Println("stratum: snapshot save stopped (signal received)")
					return &cleanStopExit{stop: err}
				}
				return err
			}
			fmt.Printf("snapshot saved: %s\n", p)
			if t := cfg.S3Target(); t != nil {
				fmt.Printf("uploaded to s3 bucket %q (prefix %q)\n", t.Bucket, t.Prefix)
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&name, "name", "", "snapshot file name (default etcd-snapshot-<host>-<ts>.db)")
	fs.BoolVar(&scheduled, "scheduled", false, "(internal) invoked by the snapshot timer")
	return cmd
}

const snapshotListJSONOutput = "json"

func newSnapshotListCommand() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List local + S3 snapshots",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if output != "" {
				if output != snapshotListJSONOutput {
					return fmt.Errorf("unknown output format %q: %q or the default text table", output, snapshotListJSONOutput)
				}
				// Same rows the backup_evidence tool serves, so the surfaces cannot drift.
				out := backupEvidenceRows(cmd.Context(), mcpsrv.BackupEvidenceIn{})
				return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
			}
			cfg, _ := loadSnapshotConfig()
			items, err := etcdsnap.List(cfg.Dir, cfg.S3Target())
			if err != nil {
				fmt.Fprintln(os.Stderr, "WARN:", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%-46s %-6s %-10s %s\n", "NAME", "WHERE", "SIZE", "MODIFIED (UTC)")
			for _, it := range items {
				fmt.Fprintf(out, "%-46s %-6s %-10d %s\n", it.Name, it.Location, it.Size, it.Modified.UTC().Format("2006-01-02 15:04"))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "",
		"output format: json emits the engine's backup evidence as machine-readable rows (policy, local and remote catalogs, deduplicated census, maintenance); the default text table is unchanged")
	return cmd
}

func newSnapshotPruneCommand() *cobra.Command {
	var retain int
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Prune snapshots to the retention policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadSnapshotConfig()
			if err != nil {
				// An unreadable config yields default retention: pruning on it could delete kept snapshots.
				if retain <= 0 {
					return fmt.Errorf("read snapshot config (pass --retain N to prune to an explicit retention): %w", err)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "WARN: %v; pruning local snapshots to the --retain %d you passed, leaving S3 retention alone\n", err, retain)
				// The unreadable config held the S3 settings, not the dir; resolve it as the scheduled loop does.
				cfg = etcdsnap.DefaultConfig()
				cfg.Dir = snapshotDir()
			}
			keep := cfg.Retain
			if retain > 0 {
				keep = retain
			}
			h, err := host.Hostname()
			if err != nil {
				return fmt.Errorf("snapshot prune: resolve host name: %w", err)
			}
			// A failed prune is a warning, not a failure: the snapshot already succeeded, so exit 0.
			for _, perr := range etcdsnap.PruneScheduled(cfg.Dir, h, keep, cfg.S3Target()) {
				fmt.Printf("  WARN: %v\n", perr)
			}
			fmt.Printf("pruned to newest %d snapshot(s) for %s\n", keep, h)
			return nil
		},
	}
	cmd.Flags().IntVar(&retain, "retain", 0, "keep newest N (default: the configured retention)")
	return cmd
}

// Absolute so a leading - never reaches etcdutl; not cleaned: Clean folds .. past a symlink.
func resolveSnapshotArg(arg string) (string, error) {
	if arg == "." || arg == ".." || strings.HasPrefix(arg, "s3://") {
		return arg, nil
	}
	if filepath.Base(arg) != arg {
		if filepath.IsAbs(arg) {
			return arg, nil
		}
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return strings.TrimRight(wd, "/") + "/" + arg, nil
	}
	return filepath.Join(snapshotDir(), arg), nil
}

// What is left for the operator is the same whatever the restore restarted.
var restoreStillToDo = etcdsnap.StillToDo(true)

var restoreNextSteps = "\nseed restored. " + etcdsnap.RestoreRestartSet().RestartedProse() +
	"\n\n" + restoreStillToDo

// In-cluster restarts must run on the seed: admin.conf may name the VIP before the rejoin.
var restoreIncompleteNextSteps = `
seed restored, but NOT every component holding a pre-restore watch or start-up
state was restarted (the error below names them). Do not run the restore again. Restart
them by hand — the host-side watchers by killing the process, which restarts it,
and the in-cluster workloads, on this seed node, with
` + etcdsnap.CommandLines(etcdsnap.SeedRolloutRestartCommands()) + `

` + restoreStillToDo

var restoreLocalSeed = etcdsnap.RestoreLocalSeed

// A live download rewrites its temp file continuously, so only stale ones are removed.
const staleTempAge = 15 * time.Minute

// Refuses a dir others could swap files in (snapshots hold every Secret); never chmods one as root.
func openSnapshotDir(dir string) (_ *os.Root, err error) {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("snapshot dir %s is not an absolute path", dir)
	}
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = root.Close()
		}
	}()
	for _, comp := range strings.Split(dir, string(filepath.Separator)) {
		if comp == "" {
			continue
		}
		fi, err := root.Lstat(comp)
		if errors.Is(err, fs.ErrNotExist) {
			if err = root.Mkdir(comp, 0o700); err == nil {
				fi, err = root.Lstat(comp)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("snapshot dir %s: %w", dir, err)
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf("snapshot dir %s has a symlink at %q; refusing to download into it", dir, comp)
		case !fi.IsDir():
			return nil, fmt.Errorf("snapshot dir %s: %q is not a directory", dir, comp)
		}
		next, err := root.OpenRoot(comp)
		if err != nil {
			return nil, fmt.Errorf("snapshot dir %s: %w", dir, err)
		}
		_ = root.Close()
		root = next
		// comp may have been swapped for a symlink between the Lstat and the open.
		if st, err := root.Stat("."); err != nil || !os.SameFile(fi, st) {
			return nil, fmt.Errorf("snapshot dir %s: %q changed while it was being opened", dir, comp)
		}
	}
	fi, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("snapshot dir %s is writable by group or others (%04o); chmod it to 0700 or pick another spec.snapshots.dir", dir, fi.Mode().Perm())
	}
	// The owner can swap files in a dir whatever its mode.
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != geteuid() {
		return nil, fmt.Errorf("snapshot dir %s is owned by uid %d, not by the user running restore (uid %d)", dir, st.Uid, geteuid())
	}
	return root, nil
}

var geteuid = func() uint32 { return uint32(os.Geteuid()) }

// Restore only holds the configured target's credentials: another bucket or prefix is refused.
func s3RestoreName(src string, t *objstore.Target) (string, error) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(src, "s3://"), "/")
	if bucket != t.Bucket {
		return "", fmt.Errorf("%s names bucket %q, but restore uses the configured S3 target, bucket %q", src, bucket, t.Bucket)
	}
	name := path.Base(key)
	if key == "" || strings.HasSuffix(key, "/") || name == "." || name == ".." {
		return "", fmt.Errorf("%s names no snapshot object", src)
	}
	// Compare the literal key: path.Dir cleans dot segments S3 keeps, so ".." would slip past.
	if want := t.Key(name); strings.Contains(key, "/") && key != want {
		return "", fmt.Errorf("%s is outside the configured S3 target's prefix, and restore uses the configured target, which would fetch %q from bucket %q", src, want, t.Bucket)
	}
	return name, nil
}

func newSnapshotRestoreCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "restore <NAME|FILE|s3://bucket/key>",
		Short: "Restore etcd from a snapshot on this seed node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				return fmt.Errorf("stratum snapshot restore is disruptive (resets etcd on this seed node); re-run with --yes")
			}
			src := args[0]
			local, err := resolveSnapshotArg(src)
			if err != nil {
				return fmt.Errorf("resolve snapshot %q: %w", src, err)
			}
			if strings.HasPrefix(src, "s3://") {
				// The defaults' "needs an S3 target" would misname the problem, so wrap the config error.
				cfg, err := loadSnapshotConfig()
				if err != nil {
					return fmt.Errorf("read snapshot config: %w", err)
				}
				t := cfg.S3Target()
				if t == nil {
					return fmt.Errorf("s3:// restore needs an S3 target (configure it with `stratum snapshot schedule --s3-bucket …`)")
				}
				name, err := s3RestoreName(src, t)
				if err != nil {
					return err
				}
				dir := snapshotDir()
				root, err := openSnapshotDir(dir)
				if err != nil {
					return err
				}
				defer root.Close()
				if err := objstore.RemoveStaleTemps(root, name, staleTempAge); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: removing stale download temp files in %s: %v\n", dir, err)
				}
				local = filepath.Join(dir, name)
				fmt.Printf("downloading %s -> %s\n", src, local)
				if err := s3Download(t, context.Background(), root, name); err != nil {
					return err
				}
			}
			if err := restoreLocalSeed(os.Stdout, local); err != nil {
				if errors.Is(err, etcdsnap.ErrWatchersNotRestarted) {
					fmt.Fprint(cmd.OutOrStdout(), restoreIncompleteNextSteps)
				}
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), restoreNextSteps)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "proceed (restore stops apiserver+etcd and resets etcd to a new single-member cluster)")
	return cmd
}

// Default is listing: a pre-restore copy can be the only copy of the pre-restore data.
func newSnapshotPruneRestoresCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "prune-restores",
		Short: "List, or with --yes remove, the etcd data copies snapshot restore keeps",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			copies, err := etcdsnap.RestoreCopies()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(copies) == 0 {
				fmt.Fprintln(out, "no restore copies to remove")
				return nil
			}
			var total int64
			fmt.Fprintf(out, "%-15s %-10s %-17s %s\n", "KIND", "SIZE", "MODIFIED (UTC)", "PATH")
			for _, c := range copies {
				total += c.Bytes
				fmt.Fprintf(out, "%-15s %-10s %-17s %s\n", c.Kind, etcdsnap.HumanBytes(c.Bytes),
					c.Modified.UTC().Format("2006-01-02 15:04"), c.Path)
			}
			if !yes {
				fmt.Fprintf(out, "\n%d restore copies, %s. Nothing was removed; once the cluster is verified, "+
					"re-run with --yes to remove them.\n", len(copies), etcdsnap.HumanBytes(total))
				return nil
			}
			removed, err := etcdsnap.RemoveRestoreCopies(copies)
			for _, c := range removed {
				fmt.Fprintf(out, "removed %s\n", c.Path)
			}
			if kept := len(copies) - len(removed); err == nil && kept > 0 {
				fmt.Fprintf(out, "left %d copies that changed after they were listed; re-run to review them\n", kept)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "remove them (each is a full copy of etcd's data; a pre-restore copy may be the only copy of the data from before a restore)")
	return cmd
}

func newSnapshotRejoinCommand() *cobra.Command {
	var server string
	var yes, force bool
	cmd := &cobra.Command{
		Use:   "rejoin",
		Short: "Rejoin a restored cluster (wipes this node's etcd unless it can resume)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return fmt.Errorf("stratum snapshot rejoin requires --server https://<seed-ip>:6443")
			}
			if !yes {
				return fmt.Errorf("stratum snapshot rejoin is disruptive (wipes this node's etcd data unless it finds a member it can resume); re-run with --yes")
			}
			u, err := url.Parse(server)
			if err != nil || u.Hostname() == "" {
				return fmt.Errorf("invalid --server %q: %v", server, err)
			}
			return ha.RejoinEtcd(cmd.Context(), os.Stdout, u.Hostname(), force)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&server, "server", "", "the restored seed control-plane URL, e.g. https://<cp1-ip>:6443")
	fs.BoolVar(&yes, "yes", false, "proceed (rejoins the restored cluster; wipes this node's etcd unless it can resume an existing member)")
	fs.BoolVar(&force, "force", false, "wipe even though this node's etcd still has quorum with its own cluster (its data is discarded)")
	return cmd
}

// Test seams: the real paths are fixed under /etc/stratum.
var (
	scheduleLoadConfig = cluster.LoadSnapshotConfig
	scheduleSaveConfig = etcdsnap.SaveConfig
)

func newSnapshotScheduleCommand() *cobra.Command {
	var (
		retain    int
		bucket    string
		endpoint  string
		region    string
		prefix    string
		pathStyle bool
		snaps     bool
	)
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Configure the snapshot retention/S3-offload schedule",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Seeding from the defaults would drop config.yaml's S3 target; unreadable conf is an error.
			cfg, err := scheduleLoadConfig()
			if err != nil {
				return fmt.Errorf("read snapshot config (not overwriting it): %w", err)
			}
			// Key on Changed, not values: an explicit --s3-path-style=false must override a configured true.
			var detailFlags []string
			if cmd.Flags().Changed("s3-endpoint") {
				detailFlags = append(detailFlags, "--s3-endpoint")
			}
			if cmd.Flags().Changed("s3-region") {
				detailFlags = append(detailFlags, "--s3-region")
			}
			if cmd.Flags().Changed("s3-prefix") {
				detailFlags = append(detailFlags, "--s3-prefix")
			}
			if cmd.Flags().Changed("s3-path-style") {
				detailFlags = append(detailFlags, "--s3-path-style")
			}
			// Saving an empty bucket would clear the configured one and with it the omitted S3 details.
			if cmd.Flags().Changed("s3-bucket") && strings.TrimSpace(bucket) == "" {
				return fmt.Errorf("snapshot schedule: --s3-bucket was passed empty: an empty bucket is refused; to stop S3 uploads instead, remove the S3_BUCKET line from %s", etcdsnap.ConfigPath)
			}
			// Refuse a detail flag without a bucket rather than silently saving a no-op.
			effectiveBucket := cfg.S3.Bucket
			if cmd.Flags().Changed("s3-bucket") {
				effectiveBucket = bucket
			}
			if len(detailFlags) > 0 && effectiveBucket == "" {
				verb := "needs"
				if len(detailFlags) > 1 {
					verb = "need"
				}
				return fmt.Errorf("snapshot schedule: %s %s an S3 bucket: pass --s3-bucket (no bucket is configured)", strings.Join(detailFlags, ", "), verb)
			}
			if retain > 0 {
				cfg.Retain = retain
			}
			if cmd.Flags().Changed("s3-bucket") {
				cfg.S3.Bucket = bucket
			}
			if cmd.Flags().Changed("s3-endpoint") {
				cfg.S3.Endpoint = endpoint
			}
			if cmd.Flags().Changed("s3-region") {
				cfg.S3.Region = region
			}
			if cmd.Flags().Changed("s3-prefix") {
				cfg.S3.Prefix = prefix
			}
			if cmd.Flags().Changed("s3-path-style") {
				cfg.S3.PathStyle = pathStyle
			}
			// Credentials come from the environment, never argv; SaveConfig does not write them back.
			if err := scheduleSaveConfig(os.Stdout, cfg); err != nil {
				return err
			}
			if cfg.HasPersistedCredentials() {
				fmt.Printf("note: %s holds S3 credentials on disk (left unchanged); to migrate them off disk, first configure STRATUM_S3_*/AWS_* in the snapshot process environment, then remove S3_ACCESS_KEY/S3_SECRET_KEY from this file and verify S3 uploads\n", etcdsnap.ConfigPath)
			}
			// The controller's snapshot loop reads this config every tick, so saving it is the schedule change.
			if snaps {
				fmt.Printf("snapshot schedule updated (retain=%d, s3-bucket=%q); the running controller's snapshot loop picks it up on its next tick\n", cfg.Retain, cfg.S3.Bucket)
			} else {
				fmt.Println("note: to disable scheduled snapshots set spec.snapshots.disabled=true in /etc/stratum/config.yaml and restart the controller (config kept)")
			}
			return nil
		},
	}
	fs := cmd.Flags()
	fs.IntVar(&retain, "retain", 0, "snapshots to keep per node (0 = leave unchanged)")
	fs.StringVar(&bucket, "s3-bucket", "", "S3 bucket for off-box copies (enables S3 offload)")
	fs.StringVar(&endpoint, "s3-endpoint", "", "custom S3 endpoint (for MinIO/Ceph/etc.)")
	fs.StringVar(&region, "s3-region", "", "S3 region (default us-east-1)")
	fs.StringVar(&prefix, "s3-prefix", "", "key prefix within the bucket")
	fs.BoolVar(&pathStyle, "s3-path-style", false, "path-style addressing (typical for non-AWS)")
	fs.BoolVar(&snaps, "snapshots", true, "false prints how to disable scheduled snapshots instead of the update confirmation; it does not disable them, and the config is saved either way")
	return cmd
}

func init() {
	// Import cycle: the MCP package cannot import this one, so the seam is wired here at startup.
	mcpsrv.BackupEvidenceRead = backupEvidenceRows
}

// Serves the same BackupEvidence read an external pinned binary makes; stamps are RFC 3339 UTC.
func backupEvidenceRows(ctx context.Context, in mcpsrv.BackupEvidenceIn) mcpsrv.BackupEvidenceOut {
	res := BackupEvidence(ctx,
		WithBackupEvidenceMaxEntries(in.MaxEntries),
		WithBackupEvidenceMaxBytes(in.MaxBytes),
		WithBackupEvidenceMaxTime(time.Duration(in.MaxSeconds)*time.Second),
	)
	return mcpsrv.BackupEvidenceOut{
		Policy:      backupPolicyRow(res.Policy),
		Node:        res.Node,
		NodeUID:     res.NodeUID,
		Local:       backupLocalRow(res.Local),
		Remote:      backupRemoteRow(res.Remote),
		Backups:     backupCensusRow(res.Backups),
		Maintenance: backupMaintenanceRow(res.Maintenance),
		Complete:    res.Complete,
		Warnings:    backupRowStrings(res.Warnings),
	}
}

// The contract reads an empty stamp as an absent value.
func backupEvidenceStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// A nil list becomes an empty one, never JSON null.
func backupRowStrings(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func backupPolicyRow(p BackupPolicy) mcpsrv.BackupPolicyRow {
	return mcpsrv.BackupPolicyRow{
		State:           string(p.State),
		IntervalSeconds: int64(p.Interval / time.Second),
		IntervalFrom:    string(p.IntervalFrom),
		Retain:          p.Retain,
		RetainFrom:      string(p.RetainFrom),
		Copy:            string(p.Copy),
		CopyFrom:        string(p.CopyFrom),
		Dir:             p.Dir,
		DirFrom:         string(p.DirFrom),
		Warnings:        backupRowStrings(p.Warnings),
	}
}

func backupArtifactRow(a BackupArtifact) mcpsrv.BackupArtifactRow {
	return mcpsrv.BackupArtifactRow{
		ID:         a.ID,
		Location:   a.Location,
		LocationID: a.LocationID,
		CopyID:     a.CopyID,
		Created:    backupEvidenceStamp(a.Created),
		Size:       a.Size,
		Digest:     a.Digest,
	}
}

func backupArtifactRows(as []BackupArtifact) []mcpsrv.BackupArtifactRow {
	out := make([]mcpsrv.BackupArtifactRow, 0, len(as))
	for _, a := range as {
		out = append(out, backupArtifactRow(a))
	}
	return out
}

func backupLocalRow(l BackupLocalCatalog) mcpsrv.BackupLocalRow {
	return mcpsrv.BackupLocalRow{
		Node:      l.Node,
		Artifacts: backupArtifactRows(l.Artifacts),
		Complete:  l.Complete,
		Warnings:  backupRowStrings(l.Warnings),
	}
}

func backupRemoteRow(r BackupRemoteCatalog) mcpsrv.BackupRemoteRow {
	return mcpsrv.BackupRemoteRow{
		Copies:   backupArtifactRows(r.Copies),
		Old:      backupArtifactRows(r.Old),
		Complete: r.Complete,
		Warnings: backupRowStrings(r.Warnings),
	}
}

// Copies each entry's artifact values so no pointer reaches back into the surface's result.
func backupCensusRow(c BackupCensus) mcpsrv.BackupCensusRow {
	entries := make([]mcpsrv.BackupCensusEntryRow, 0, len(c.Entries))
	for _, e := range c.Entries {
		row := mcpsrv.BackupCensusEntryRow{ID: e.ID, OldRemote: e.OldRemote}
		if e.Local != nil {
			local := backupArtifactRow(*e.Local)
			row.Local = &local
		}
		if e.Remote != nil {
			remote := backupArtifactRow(*e.Remote)
			row.Remote = &remote
		}
		entries = append(entries, row)
	}
	return mcpsrv.BackupCensusRow{
		Node:      c.Node,
		Entries:   entries,
		Complete:  c.Complete,
		Warnings:  backupRowStrings(c.Warnings),
		Retention: backupRowStrings(c.Retention),
	}
}

func backupMaintenanceRow(m BackupMaintenance) mcpsrv.BackupMaintenanceRow {
	return mcpsrv.BackupMaintenanceRow{
		Save:              backupAttemptRow(m.Save),
		Upload:            backupAttemptRow(m.Upload),
		Prune:             backupAttemptRow(m.Prune),
		LastSaveSuccess:   backupEvidenceStamp(m.LastSaveSuccess),
		LastUploadSuccess: backupEvidenceStamp(m.LastUploadSuccess),
		Warnings:          backupRowStrings(m.Warnings),
		AttemptsKnown:     m.AttemptsKnown,
	}
}

func backupAttemptRow(a BackupAttempt) mcpsrv.BackupAttemptRow {
	return mcpsrv.BackupAttemptRow{
		AttemptedAt: backupEvidenceStamp(a.AttemptedAt),
		Outcome:     string(a.Outcome),
	}
}

package cli

import (
	"context"
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

// snapshotDir is the directory a bare snapshot name resolves in
// (spec.snapshots.dir), a seam so tests need not touch /etc/stratum. Restore
// resolves names through it; save, list and prune instead take the directory
// from the resolved snapshot settings, so they cannot drift from the scheduled
// loop that owns the place.
var snapshotDir = cluster.LoadSnapshotDir

// loadSnapshotConfig and saveSnapshot are the config read and the etcd save
// behind seams, so tests reach neither the host's snapshot.conf (and its S3
// target) nor etcd. The settings carry the directory (Dir) the save, list and
// prune commands act in.
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
			// The save owns the shared interrupt context: cmd.Context() is
			// Background for every stratum invocation, so on the raw command
			// context a first Ctrl-C killed the etcdctl run mid-snapshot. On
			// this context the first signal stops the save at its next step
			// boundary (the etcdctl run, then the S3 upload) and a second
			// kills, the same contract the scheduled loop and the MCP tool run
			// under. A signal-asked stop exits 0; the partial snapshot the
			// etcdctl run leaves is the shape the scheduled loop already
			// tolerates and prune removes.
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

func newSnapshotListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List local + S3 snapshots",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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
				// An unreadable config would leave the built-in defaults in
				// effect (retain 5, no S3), so pruning on it would delete
				// snapshots the operator's retention kept. Refuse, unless the
				// operator gave an explicit --retain: then prune locally to
				// exactly that, leaving S3 retention untouched.
				if retain <= 0 {
					return fmt.Errorf("read snapshot config (pass --retain N to prune to an explicit retention): %w", err)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "WARN: %v; pruning local snapshots to the --retain %d you passed, leaving S3 retention alone\n", err, retain)
				// No S3 target: the config the S3 settings came from is unreadable.
				// The directory does not come from snapshot.conf, so it still
				// resolves to the one the scheduled loop writes.
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
			// PruneScheduled returns its failures instead of printing them;
			// the command keeps reporting them as WARN lines with exit status
			// 0 — a failed prune is a warning, not a command failure, since a
			// snapshot already succeeded.
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

// resolveSnapshotArg maps the bare NAME `snapshot list` prints to its file
// under the snapshot dir. Anything with a path separator, and s3://
// keys, pass through unchanged, so full paths keep working. A file in the
// working directory needs ./NAME. A relative path is made absolute so one
// that starts with "-" cannot reach etcdutl as a flag. It is prefixed with the
// working directory, not cleaned: Clean folds ".." lexically, which past a
// symlink names a different file.
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

// The restore's own watchers are its restart set (etcdsnap.RestoreRestartSet):
// the host-side watchers and in-cluster workloads the restore restarts because
// they hold watches, or start-up state, from before it. What is left for the
// operator is the same whether or not every one of those restarts worked.
var restoreStillToDo = etcdsnap.StillToDo(true)

var restoreNextSteps = "\nseed restored. " + etcdsnap.RestoreRestartSet().RestartedProse() +
	"\n\n" + restoreStillToDo

// restoreIncompleteNextSteps is for a restore whose data is in place but that
// could not restart every watcher. The error names the ones it could not.
//
// The in-cluster workloads are restarted through the seed's own apiserver, and on
// the seed: before the other control planes rejoin, admin.conf's server may be
// the VIP, which can lead to a member that still holds the old datastore.
var restoreIncompleteNextSteps = `
seed restored, but NOT every component holding a pre-restore watch or start-up
state was restarted (the error below names them). Do not run the restore again. Restart
them by hand — the host-side watchers by killing the process, which restarts it,
and the in-cluster workloads, on this seed node, with
` + etcdsnap.CommandLines(etcdsnap.SeedRolloutRestartCommands()) + `

` + restoreStillToDo

// restoreLocalSeed is etcdsnap.RestoreLocalSeed, indirected for tests.
var restoreLocalSeed = etcdsnap.RestoreLocalSeed

// staleTempAge is how old a download temp file in the snapshot dir must be
// before restore removes it: a live download rewrites its file continuously.
const staleTempAge = 15 * time.Minute

// openSnapshotDir opens dir, creating it 0700 if it is missing, and returns a
// handle to it. It refuses a download dir other users could swap files in: the
// snapshot holds every Secret. Each component is opened without following
// symlinks, so a symlink anywhere in the path is refused, and the checks are
// made on the opened directory, which the caller then writes through: swapping
// the path for a symlink afterwards redirects nothing. An existing dir is
// checked, never chmodded, so a misconfigured spec.snapshots.dir (/) is not
// changed as root.
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
			_ = root.Close() // root is the deepest handle opened so far
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

// geteuid is os.Geteuid, indirected for tests.
var geteuid = func() uint32 { return uint32(os.Geteuid()) }

// s3RestoreName returns the object name src asks for under the configured
// target t. Restore only has t's credentials, so a URL naming another bucket,
// or a key directory other than t's prefix, is refused rather than served from
// t: the admin would restore a different snapshot from the one they named. A
// bare key (no directory part) is the name under t's prefix.
func s3RestoreName(src string, t *objstore.Target) (string, error) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(src, "s3://"), "/")
	if bucket != t.Bucket {
		return "", fmt.Errorf("%s names bucket %q, but restore uses the configured S3 target, bucket %q", src, bucket, t.Bucket)
	}
	name := path.Base(key)
	if key == "" || strings.HasSuffix(key, "/") || name == "." || name == ".." {
		return "", fmt.Errorf("%s names no snapshot object", src)
	}
	// Compare the literal key: path.Dir would clean dot segments and repeated
	// slashes that S3 keeps, so "other/../x.db" would pass as "x.db".
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
				// An unreadable config falls back to the defaults (no S3), whose
				// "needs an S3 target" would misname the problem.
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

// newSnapshotPruneRestoresCommand lists, and with --yes removes, the etcd data
// copies snapshot restore keeps beside the data dir. Listing is the
// default: a pre-restore copy can be the only copy of the data from before a
// restore, so nothing removes one without being asked.
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

// scheduleLoadConfig and scheduleSaveConfig are `snapshot schedule`'s config
// read and write behind seams: the real ones use the fixed /etc/stratum paths,
// which a test must not touch.
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
			// Seed from what is in effect: the existing conf, else config.yaml, plus
			// env creds. Seeding from the defaults would write a conf that drops
			// config.yaml's S3 target, and the file then wins.
			// An unreadable snapshot.conf is an error, not "no file": saving over it
			// from the defaults would silently replace what the operator configured.
			cfg, err := scheduleLoadConfig()
			if err != nil {
				return fmt.Errorf("read snapshot config (not overwriting it): %w", err)
			}
			if retain > 0 {
				cfg.Retain = retain
			}
			if bucket != "" {
				cfg.S3.Bucket = bucket
				cfg.S3.Endpoint = endpoint
				cfg.S3.Region = region
				cfg.S3.Prefix = prefix
				cfg.S3.PathStyle = pathStyle
				// credentials come from the environment (STRATUM_S3_*/AWS_*) — never
				// argv; LoadConfig already overlaid them onto cfg.S3. SaveConfig
				// will not write them back out.
			}
			if err := scheduleSaveConfig(os.Stdout, cfg); err != nil {
				return err
			}
			if cfg.HasPersistedCredentials() {
				fmt.Printf("note: %s holds S3 credentials on disk (left unchanged); to migrate them off disk, first configure STRATUM_S3_*/AWS_* in the snapshot process environment, then remove S3_ACCESS_KEY/S3_SECRET_KEY from this file and verify S3 uploads\n", etcdsnap.ConfigPath)
			}
			// No timer units under the supervisor: the controller's snapshot loop
			// reads this config on every tick, so saving it IS the schedule change.
			// Disabling still goes through the cluster config (spec.snapshots).
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

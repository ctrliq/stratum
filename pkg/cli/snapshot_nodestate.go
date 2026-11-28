package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/nodebackup"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

// The node-state commands' reads of the host, behind seams so tests reach
// neither the node's config nor its name.
var (
	loadNodeStateBackup = cluster.LoadNodeStateBackup
	nodeStateHostname   = host.Hostname
	nodeStateS3Config   = etcdsnap.LoadConfigOr
)

// s3ObjectSize is the size of the object name under target, from a listing
// of its prefix. A var so a test need not serve one.
var s3ObjectSize = func(ctx context.Context, target *objstore.Target, name string) (int64, error) {
	objs, err := target.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list s3://%s/%s: %w", target.Bucket, target.Prefix, err)
	}
	for _, o := range objs {
		if o.Name == name {
			return o.Size, nil
		}
	}
	return 0, fmt.Errorf("no object %s in s3://%s", target.Key(name), target.Bucket)
}

// maxSealedBundle bounds a bundle read from disk or S3: the archive's own
// cap, with room for its header and seal.
const maxSealedBundle = 80 << 20

func newSnapshotKeygenCommand() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Make the key pair node-state backups are sealed to",
		Long: "Makes a key pair for the node-state bundle a control plane takes beside each etcd snapshot. " +
			"The recipient it prints is public: put it in the cluster config under spec.snapshots.nodeState.recipients. " +
			"The identity it writes to --out is the only thing that opens those bundles: keep it off the cluster, " +
			"with the people who would run a recovery, and keep more than one copy. Run this anywhere stratum runs; " +
			"it reads and changes nothing on the node.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return errors.New("--out is required: the file to write the identity to (it is never printed)")
			}
			id, err := nodebackup.GenerateIdentity()
			if err != nil {
				return err
			}
			secret, err := id.String()
			if err != nil {
				return err
			}
			if _, err := host.CreateFileExclusively(out, []byte(secret+"\n"), 0o600); err != nil {
				return fmt.Errorf("write the identity to %s: %w", out, err)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "identity written to %s (mode 0600). It is a secret, and the only thing that opens these backups:\n", out)
			fmt.Fprintln(w, "move it off this machine and keep at least two copies. A lost identity cannot be recovered.")
			fmt.Fprintf(w, "\nrecipient: %s\n", id.Recipient())
			fmt.Fprintf(w, "\nAdd it to the cluster config on every control plane, then restart stratum there:\n\n"+
				"spec:\n  snapshots:\n    nodeState:\n      recipients:\n        - %s\n", id.Recipient())
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "file to write the identity to; it must not exist")
	return cmd
}

// readIdentityFile loads a backup identity. The file must be private to its
// owner: an identity other local users can read protects nothing.
func readIdentityFile(path string) (nodebackup.Identity, error) {
	if path == "" {
		return nodebackup.Identity{}, errors.New("--identity-file is required: the file `stratum snapshot keygen` wrote")
	}
	// O_NONBLOCK so a FIFO with no writer fails the regular-file check
	// instead of hanging the open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nodebackup.Identity{}, fmt.Errorf("read the identity file: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nodebackup.Identity{}, fmt.Errorf("read the identity file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nodebackup.Identity{}, fmt.Errorf("identity file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nodebackup.Identity{}, fmt.Errorf("identity file %s is mode %04o: other users can read it; chmod it to 0600", path, perm)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != geteuid() {
		return nodebackup.Identity{}, fmt.Errorf("identity file %s is not owned by the user running the restore (uid %d)", path, geteuid())
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nodebackup.Identity{}, fmt.Errorf("read the identity file: %w", err)
	}
	id, err := nodebackup.ParseIdentity(string(b))
	if err != nil {
		return nodebackup.Identity{}, fmt.Errorf("%s: %w", path, err)
	}
	return id, nil
}

// nodeStateDestination is where a restore on this machine writes.
func nodeStateDestination(allowExtra string) nodebackup.Destination {
	return nodebackup.Destination{
		PKIDir:       version.PKIDir,
		StatePath:    nodestate.StatePath(),
		ConfDir:      version.ConfDir,
		ManifestsDir: version.ManifestsDir,
		AllowExtra:   allowExtra,
	}
}

// refuseExistingNode refuses a machine that already holds a node's state. A
// restore over one would leave two nodes' keys and identity mixed on disk,
// and a restore onto a running control plane would replace the CA under it.
func refuseExistingNode() error {
	for _, p := range []string{filepath.Join(version.PKIDir, "ca.key"), filepath.Join(version.PKIDir, "ca.crt"), nodestate.StatePath()} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("this machine already holds a Stratum node's state (%s). A node-state restore is for a machine "+
				"that is not a node yet: run `stratum uninstall --yes` here first, or use another machine", p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if entries, err := os.ReadDir(version.EtcdDataDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("this machine already holds etcd data (%s). A node-state restore is for a machine that is not a "+
			"node yet: run `stratum uninstall --yes` here first, or use another machine", version.EtcdDataDir)
	}
	return nil
}

type nodeStateS3Flags struct {
	endpoint, region string
	pathStyle        bool
}

// fetchSealedBundle reads the sealed bundle src names: a file, or an
// s3://bucket/key object. A machine that is not a node yet has no snapshot
// config (it is inside the bundle), so the S3 target is the URL's bucket and
// key with the endpoint from flags and the credentials from the environment,
// as every other S3 use takes them.
func fetchSealedBundle(parent context.Context, src string, s3 nodeStateS3Flags) ([]byte, error) {
	if !strings.HasPrefix(src, "s3://") {
		f, err := os.Open(src)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(io.LimitReader(f, maxSealedBundle+1))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", src, err)
		}
		if len(b) > maxSealedBundle {
			return nil, fmt.Errorf("%s is too large to be a node-state bundle", src)
		}
		return b, nil
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(src, "s3://"), "/")
	name := path.Base(key)
	if bucket == "" || key == "" || strings.HasSuffix(key, "/") || name == "." || name == ".." {
		return nil, fmt.Errorf("%s names no bundle object: want s3://<bucket>/<key>", src)
	}
	prefix := ""
	if dir := path.Dir(key); dir != "." {
		prefix = dir
	}
	// The URL names the object. A snapshot config already on the machine, and
	// the flags, say only how to reach the bucket: its prefix is the
	// snapshots', one level above the bundles, and is not this object's.
	cfg, err := nodeStateS3Config(etcdsnap.Config{})
	if err != nil {
		return nil, err
	}
	target := &objstore.Target{Bucket: bucket, Prefix: prefix,
		Endpoint: cfg.S3.Endpoint, Region: cfg.S3.Region, PathStyle: cfg.S3.PathStyle,
		AccessKey: cfg.S3.AccessKey, SecretKey: cfg.S3.SecretKey}
	if s3.endpoint != "" {
		target.Endpoint = s3.endpoint
	}
	if s3.region != "" {
		target.Region = s3.region
	}
	if s3.pathStyle {
		target.PathStyle = true
	}
	ctx, stop := interrupt.DerivedContext(parent)
	defer stop()
	// The download writes whatever the object holds, so its size is checked
	// first: a bundle is small, and an object that is not must not fill this
	// machine's disk before it is turned away.
	size, err := s3ObjectSize(ctx, target, name)
	if err != nil {
		return nil, err
	}
	if size > maxSealedBundle {
		return nil, fmt.Errorf("%s is %d MiB, too large to be a node-state bundle", src, size>>20)
	}
	dir, err := os.MkdirTemp("", "stratum-node-state-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	if err := s3Download(target, ctx, root, name); err != nil {
		return nil, err
	}
	return fetchSealedBundle(parent, filepath.Join(dir, name), s3)
}

func newSnapshotRestoreNodeStateCommand() *cobra.Command {
	var identityFile, allowPath, caSHA256 string
	var yes, anyHostname, noPin bool
	var s3 nodeStateS3Flags
	cmd := &cobra.Command{
		Use:   "restore-node-state <FILE|s3://bucket/key>",
		Short: "Put a lost control plane's PKI and node state on its replacement",
		Long: "Restores a node-state bundle onto a machine that is to replace a lost control plane and is not a Stratum " +
			"node yet: the cluster's CA keys, the node's own state and its configuration. Run it before the " +
			"machine's first `stratum up`, which would otherwise mint a new CA that nothing trusts. Then start " +
			"Stratum and restore the etcd snapshot taken with the bundle. The replacement must have the lost " +
			"control plane's hostname and IP address.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			id, err := readIdentityFile(identityFile)
			if err != nil {
				return err
			}
			if err := refuseExistingNode(); err != nil {
				return err
			}
			sealed, err := fetchSealedBundle(cmd.Context(), args[0], s3)
			if err != nil {
				return err
			}
			plain, err := nodebackup.Open(id, sealed)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			dst := nodeStateDestination(allowPath)
			m, err := nodebackup.Inspect(plain, dst)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			fmt.Fprintf(w, "node-state bundle of %s (node IP %s), taken %s\n", m.Hostname, m.NodeIP, m.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
			fmt.Fprintf(w, "  stratum %s, Kubernetes %s\n", m.StratumVersion, m.KubernetesVersion)
			if m.Snapshot != "" {
				fmt.Fprintf(w, "  taken with etcd snapshot: %s\n", m.Snapshot)
			}
			if m.ExtraConfig != "" {
				fmt.Fprintf(w, "  cluster config kept outside %s: %s\n", version.ConfDir, m.ExtraConfig)
			}
			// Printed last, after every field the bundle wrote itself.
			fmt.Fprintf(w, "  cluster CA SHA-256: %s\n", m.ClusterCASHA256)
			// A bundle is sealed to a public key: opening it proves who can
			// read it, not who wrote it. The fingerprint is the carried CA
			// certificate's own, and the bundle's CA key is checked to be that
			// certificate's, so a pin holds the restored CA to the cluster the
			// operator means. It does not vouch for the rest of the bundle.
			if caSHA256 != "" && !strings.EqualFold(strings.ReplaceAll(caSHA256, ":", ""), m.ClusterCASHA256) {
				return fmt.Errorf("the bundle's cluster CA is %s, not the --ca-sha256 given; nothing was restored", m.ClusterCASHA256)
			}
			if hn, herr := nodeStateHostname(); !anyHostname && (herr != nil || hn != m.Hostname) {
				return fmt.Errorf("this machine is %q and the bundle is %q's. The replacement must take the lost control plane's "+
					"hostname and IP address; set them, or pass --allow-hostname-mismatch if the names differ only in how "+
					"they are written. Nothing was restored", hn, m.Hostname)
			}
			if m.StratumVersion != version.StratumVersion {
				fmt.Fprintf(w, "  WARN: the bundle was taken on stratum %s and this is %s; install the same release before starting\n",
					m.StratumVersion, version.StratumVersion)
			}
			// The outside config's path comes from the bundle, so it is written
			// only when the operator names that same path, and that is settled
			// before anything is written: a node restored without the config
			// it starts with would have to be wiped to try again.
			if m.ExtraConfig != "" && allowPath != m.ExtraConfig {
				return fmt.Errorf("the bundle says the cluster config its node ran with is %s, which is outside %s, and a "+
					"bundle may not choose where it writes. Check that %s is this node's cluster config, then pass "+
					"--allow-path %s to restore it there. Nothing was restored", m.ExtraConfig, version.ConfDir, m.ExtraConfig,
					shellQuote(m.ExtraConfig))
			}
			if !yes {
				return errors.New("this writes that cluster's CA keys and this node's identity onto this machine; " +
					"check the CA fingerprint above against your record of the cluster, then re-run with --yes " +
					"and --ca-sha256 set to it")
			}
			// Anyone who can write where bundles are kept can seal one to the
			// public recipient, with a CA of their own. The pin is what holds
			// the restore to the cluster the operator means, so it is not
			// skipped without being asked to.
			if caSHA256 == "" && !noPin {
				return errors.New("pass --ca-sha256 with the cluster CA fingerprint you recorded when the bundle was saved " +
					"(each save prints it), so a bundle sealed with another CA is refused. If you have checked the " +
					"fingerprint above against the cluster by other means, pass --no-ca-pin instead. Nothing was restored")
			}
			res, err := nodebackup.Extract(plain, dst)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "\nrestored %d files under %s, %s, %s and %s\n", res.Files, version.PKIDir, version.ConfDir, version.ManifestsDir, filepath.Dir(nodestate.StatePath()))
			config := cluster.DefaultConfigPath
			if m.ConfigPath != "" {
				config = m.ConfigPath
			}
			fmt.Fprintf(w, "\nNext, on this machine:\n"+
				"  1. Start Stratum:        stratum up --config %s\n"+
				"     If it refuses to start an empty member, every other control plane is gone or powered off,\n"+
				"     and you mean to rebuild from this one, add --confirm-etcd-quorum-lost for this one start.\n"+
				"  2. Restore the datastore: stratum snapshot restore <snapshot> --yes\n", shellQuote(config))
			return nil
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&identityFile, "identity-file", "", "file holding the backup identity, as written by stratum snapshot keygen")
	fs.BoolVar(&yes, "yes", false, "confirm: write the bundle's CA keys and node state onto this machine")
	fs.StringVar(&caSHA256, "ca-sha256", "", "refuse a bundle whose cluster CA certificate does not have this SHA-256 (required with --yes unless --no-ca-pin)")
	fs.BoolVar(&noPin, "no-ca-pin", false, "restore without --ca-sha256, having checked the printed CA fingerprint by other means")
	fs.BoolVar(&anyHostname, "allow-hostname-mismatch", false, "restore a bundle taken on a host of another name")
	fs.StringVar(&allowPath, "allow-path", "", "also restore the bundle's cluster config to this path outside the configuration directory")
	fs.StringVar(&s3.endpoint, "s3-endpoint", "", "S3 endpoint for an s3:// bundle (credentials come from the environment)")
	fs.StringVar(&s3.region, "s3-region", "", "S3 region for an s3:// bundle")
	fs.BoolVar(&s3.pathStyle, "s3-path-style", false, "use path-style addressing for an s3:// bundle")
	return cmd
}

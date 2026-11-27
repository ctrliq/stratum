package cli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/nodebackup"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

// nodeStateRoots points the node's state paths at a temp tree.
func nodeStateRoots(t *testing.T) {
	t.Helper()
	oldPKI, oldConf, oldK8s, oldMan, oldEtcd := version.PKIDir, version.ConfDir, version.K8sConfDir, version.ManifestsDir, version.EtcdDataDir
	t.Cleanup(func() {
		version.PKIDir, version.ConfDir, version.K8sConfDir, version.ManifestsDir, version.EtcdDataDir = oldPKI, oldConf, oldK8s, oldMan, oldEtcd
	})
	root := t.TempDir()
	version.PKIDir = filepath.Join(root, "var/lib/stratum/pki")
	version.ConfDir = filepath.Join(root, "etc/stratum")
	version.K8sConfDir = filepath.Join(root, "var/lib/stratum/config")
	version.ManifestsDir = filepath.Join(root, "var/lib/stratum/manifests")
	version.EtcdDataDir = filepath.Join(root, "var/lib/stratum/etcd")
}

func writeNodeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lostNode lays out a control plane called cp-1 at the redirected paths,
// seals its state to a new identity, wipes it, and returns the bundle file,
// the identity file and the bundle's manifest.
func lostNode(t *testing.T) (bundle, identity string, m nodebackup.Manifest) {
	t.Helper()
	nodeStateRoots(t)
	ca, caKey := testCA(t)
	writeNodeFile(t, filepath.Join(version.PKIDir, "ca.crt"), ca)
	writeNodeFile(t, filepath.Join(version.PKIDir, "ca.key"), caKey)
	writeNodeFile(t, nodestate.StatePath(), "role: controller\nhostname: cp-1\n")
	writeNodeFile(t, filepath.Join(version.ConfDir, "config.yaml"), "spec: {}\n")
	id, err := nodebackup.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	plain, m, err := nodebackup.Collect(nodebackup.Sources{PKIDir: version.PKIDir, StatePath: nodestate.StatePath(), ConfDir: version.ConfDir},
		nodebackup.Meta{Hostname: "cp-1", NodeIP: "10.0.0.1", StratumVersion: version.StratumVersion, KubernetesVersion: version.K8sVersion,
			CreatedAt: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC), Snapshot: "etcd-snapshot-cp-1-20261125-120000.db"})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := nodebackup.Seal([]nodebackup.Recipient{id.Recipient()}, plain)
	if err != nil {
		t.Fatal(err)
	}
	keep := t.TempDir()
	bundle, identity = filepath.Join(keep, "cp-1.bundle"), filepath.Join(keep, "identity")
	secret, err := id.String()
	if err != nil {
		t.Fatal(err)
	}
	writeNodeFile(t, bundle, string(sealed))
	writeNodeFile(t, identity, secret+"\n")
	for _, dir := range []string{version.PKIDir, version.ConfDir, version.K8sConfDir} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	oldHost := nodeStateHostname
	t.Cleanup(func() { nodeStateHostname = oldHost })
	nodeStateHostname = func() (string, error) { return "cp-1", nil }
	return bundle, identity, m
}

func runRestoreNodeState(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newSnapshotRestoreNodeStateCommand()
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func caKeyRestored() bool {
	_, err := os.Stat(filepath.Join(version.PKIDir, "ca.key"))
	return err == nil
}

// keygen writes the identity to a private file it never prints, and prints
// the recipient that identity opens bundles for.
func TestSnapshotKeygenWritesTheIdentityAndPrintsTheRecipient(t *testing.T) {
	out := filepath.Join(t.TempDir(), "identity")
	var buf bytes.Buffer
	cmd := newSnapshotKeygenCommand()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--out", out})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(out)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	id, err := readIdentityFile(out)
	if err != nil {
		t.Fatalf("the identity keygen wrote does not read back: %v", err)
	}
	secret, _ := id.String()
	if strings.Contains(buf.String(), secret) {
		t.Error("keygen printed the identity")
	}
	if !strings.Contains(buf.String(), "recipient: "+id.Recipient().String()) {
		t.Errorf("keygen did not print the identity's recipient:\n%s", buf.String())
	}
	again := newSnapshotKeygenCommand()
	again.SetOut(io.Discard)
	again.SetErr(io.Discard)
	again.SilenceUsage, again.SilenceErrors = true, true
	again.SetArgs([]string{"--out", out})
	if err := again.Execute(); err == nil {
		t.Error("keygen overwrote an identity file: every bundle sealed to it would be lost")
	}
	if back, err := readIdentityFile(out); err != nil || back.Recipient().String() != id.Recipient().String() {
		t.Errorf("the refused keygen changed the identity file (%v)", err)
	}
}

// The restore puts a lost control plane's state back on a machine that has
// none, and says what to run next.
func TestRestoreNodeStatePutsTheNodeBack(t *testing.T) {
	bundle, identity, m := lostNode(t)
	out, err := runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes", "--ca-sha256", m.ClusterCASHA256)
	if err != nil {
		t.Fatalf("restore-node-state: %v\n%s", err, out)
	}
	for path, want := range map[string]string{
		nodestate.StatePath():                         "role: controller\nhostname: cp-1\n",
		filepath.Join(version.ConfDir, "config.yaml"): "spec: {}\n",
	} {
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", path, b, err, want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.key")); err != nil || !strings.Contains(string(b), "PRIVATE KEY") {
		t.Errorf("the CA key was not restored (%v)", err)
	}
	for _, want := range []string{m.ClusterCASHA256, "etcd-snapshot-cp-1-20261125-120000.db", "stratum up --config", "stratum snapshot restore"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// Every refusal leaves the machine as it was: the CA keys are written only by
// a restore the operator confirmed, of the bundle they meant, onto a machine
// that is the lost node's replacement and nothing else yet.
func TestRestoreNodeStateRefusals(t *testing.T) {
	t.Run("without --yes it shows the bundle and writes nothing", func(t *testing.T) {
		bundle, identity, m := lostNode(t)
		out, err := runRestoreNodeState(t, bundle, "--identity-file", identity)
		if err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("error = %v, want one asking for --yes", err)
		}
		if !strings.Contains(out, m.ClusterCASHA256) {
			t.Errorf("the unconfirmed run did not show the CA fingerprint to check:\n%s", out)
		}
		if caKeyRestored() {
			t.Error("an unconfirmed restore wrote the CA key")
		}
	})
	t.Run("another cluster's CA", func(t *testing.T) {
		bundle, identity, _ := lostNode(t)
		_, err := runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes", "--ca-sha256", strings.Repeat("ab", 32))
		if err == nil || !strings.Contains(err.Error(), "--ca-sha256") || caKeyRestored() {
			t.Fatalf("error = %v, CA key restored = %v; want a refusal that writes nothing", err, caKeyRestored())
		}
	})
	t.Run("a machine of another name", func(t *testing.T) {
		bundle, identity, _ := lostNode(t)
		nodeStateHostname = func() (string, error) { return "cp-9", nil }
		_, err := runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes")
		if err == nil || !strings.Contains(err.Error(), "cp-9") || caKeyRestored() {
			t.Fatalf("error = %v, CA key restored = %v; want a refusal naming this machine", err, caKeyRestored())
		}
		if _, err := runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes", "--allow-hostname-mismatch", "--no-ca-pin"); err != nil {
			t.Errorf("with --allow-hostname-mismatch: %v", err)
		}
	})
	t.Run("an unpinned --yes", func(t *testing.T) {
		bundle, identity, m := lostNode(t)
		_, err := runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes")
		if err == nil || !strings.Contains(err.Error(), "--ca-sha256") || caKeyRestored() {
			t.Fatalf("error = %v, CA key restored = %v; want a refusal asking for the pin", err, caKeyRestored())
		}
		out, err := runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes", "--no-ca-pin")
		if err != nil || !caKeyRestored() {
			t.Fatalf("with --no-ca-pin: %v, CA key restored = %v", err, caKeyRestored())
		}
		lines := strings.Split(strings.TrimSpace(strings.SplitN(out, "\nrestored", 2)[0]), "\n")
		if last := lines[len(lines)-1]; !strings.Contains(last, m.ClusterCASHA256) {
			t.Errorf("the summary's last line is %q; the CA fingerprint must be printed after every field the bundle wrote", last)
		}
	})
	t.Run("a machine that is already a node", func(t *testing.T) {
		bundle, identity, _ := lostNode(t)
		writeNodeFile(t, filepath.Join(version.PKIDir, "ca.crt"), "another cluster's CA")
		_, err := runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes")
		if err == nil || !strings.Contains(err.Error(), "uninstall") || caKeyRestored() {
			t.Fatalf("error = %v, CA key restored = %v; want a refusal pointing at uninstall", err, caKeyRestored())
		}
	})
	t.Run("a machine that holds etcd data", func(t *testing.T) {
		bundle, identity, _ := lostNode(t)
		writeNodeFile(t, filepath.Join(version.EtcdDataDir, "member", "wal"), "x")
		// --no-ca-pin, so the CA pin's own refusal cannot stand in for this one.
		_, err := runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes", "--no-ca-pin")
		if err == nil || !strings.Contains(err.Error(), version.EtcdDataDir) || !strings.Contains(err.Error(), "uninstall") || caKeyRestored() {
			t.Fatalf("error = %v, CA key restored = %v; want a refusal naming the etcd data directory %s", err, caKeyRestored(), version.EtcdDataDir)
		}
	})
	t.Run("someone else's identity", func(t *testing.T) {
		bundle, _, _ := lostNode(t)
		other, err := nodebackup.GenerateIdentity()
		if err != nil {
			t.Fatal(err)
		}
		secret, _ := other.String()
		wrong := filepath.Join(t.TempDir(), "identity")
		writeNodeFile(t, wrong, secret)
		if _, err := runRestoreNodeState(t, bundle, "--identity-file", wrong, "--yes"); !errors.Is(err, nodebackup.ErrNotForIdentity) || caKeyRestored() {
			t.Fatalf("error = %v, CA key restored = %v; want ErrNotForIdentity", err, caKeyRestored())
		}
	})
	t.Run("an identity file others can read", func(t *testing.T) {
		bundle, identity, _ := lostNode(t)
		if err := os.Chmod(identity, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes"); err == nil || !strings.Contains(err.Error(), "0600") || caKeyRestored() {
			t.Fatalf("error = %v, CA key restored = %v; want a refusal over the file's mode", err, caKeyRestored())
		}
	})
	t.Run("no identity", func(t *testing.T) {
		bundle, _, _ := lostNode(t)
		if _, err := runRestoreNodeState(t, bundle, "--yes"); err == nil || !strings.Contains(err.Error(), "--identity-file") {
			t.Fatalf("error = %v, want one asking for --identity-file", err)
		}
	})
}

// save takes the bundle the node's config asks for, takes none when told not
// to, and reports a snapshot whose bundle failed as saved and incomplete.
func TestSnapshotSaveTakesTheConfiguredNodeStateBundle(t *testing.T) {
	useSnapshotDir(t)
	oldSave, oldLoad := saveSnapshot, loadNodeStateBackup
	t.Cleanup(func() { saveSnapshot, loadNodeStateBackup = oldSave, oldLoad })
	want := &etcdsnap.NodeState{NodeIP: "10.0.0.1"}
	loadNodeStateBackup = func() (*etcdsnap.NodeState, error) { return want, nil }
	var got etcdsnap.SaveOptions
	var saveErr error
	saveSnapshot = func(_ context.Context, _ io.Writer, o etcdsnap.SaveOptions) (string, error) {
		got = o
		return filepath.Join(o.Dir, "x.db"), saveErr
	}
	run := func(args ...string) error {
		cmd := newSnapshotSaveCommand()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		cmd.SetArgs(args)
		return cmd.Execute()
	}
	if err := run(); err != nil || got.NodeState != want {
		t.Errorf("save: err = %v, NodeState = %+v; want the configured bundle", err, got.NodeState)
	}
	if err := run("--no-node-state"); err != nil || got.NodeState != nil {
		t.Errorf("save --no-node-state: err = %v, NodeState = %+v; want none", err, got.NodeState)
	}
	saveErr = etcdsnap.ErrNodeStateIncomplete
	if err := run(); !errors.Is(err, etcdsnap.ErrNodeStateIncomplete) {
		t.Errorf("a save whose bundle failed returned %v, want the failure: a backup set without its bundle is not a success", err)
	}
	// A config that cannot be read for its recipients is not one that asks
	// for no bundle: the snapshot is taken, and the save fails as incomplete.
	saveErr = nil
	loadNodeStateBackup = func() (*etcdsnap.NodeState, error) { return nil, errors.New("config.yaml: unreadable") }
	if err := run(); !errors.Is(err, etcdsnap.ErrNodeStateIncomplete) || !strings.Contains(err.Error(), "unreadable") || got.Dir == "" {
		t.Errorf("save with an unreadable config returned %v, want the snapshot taken and the set reported incomplete", err)
	}
	if err := run("--no-node-state"); err != nil {
		t.Errorf("save --no-node-state with an unreadable config: %v, want success: no bundle was asked for", err)
	}
}

// A save whose upload failed and whose recipients could not be read reports
// both: the failed upload must not hide that the snapshot has no bundle.
func TestSnapshotSaveReportsAFailedUploadAndAMissingBundleTogether(t *testing.T) {
	useSnapshotDir(t)
	oldSave, oldLoad := saveSnapshot, loadNodeStateBackup
	t.Cleanup(func() { saveSnapshot, loadNodeStateBackup = oldSave, oldLoad })
	loadNodeStateBackup = func() (*etcdsnap.NodeState, error) { return nil, errors.New("config.yaml: unreadable") }
	upload := errors.New("S3 upload failed: denied")
	saveSnapshot = func(_ context.Context, _ io.Writer, o etcdsnap.SaveOptions) (string, error) {
		return filepath.Join(o.Dir, "x.db"), upload
	}
	cmd := newSnapshotSaveCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if !errors.Is(err, upload) || !errors.Is(err, etcdsnap.ErrNodeStateIncomplete) || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("save = %v, want the failed upload and the bundle that was not taken, both", err)
	}
}

// A signal that arrives once the snapshot exists, while its S3 upload runs,
// stops the save cleanly: the stop message and exit 0, not an incomplete
// backup set. The bundle the stop left untaken is a consequence of the
// signal, not a failure of its own. A config that could not be read for its
// recipients is a failure of its own: joined with the signal's stop it stays
// visible, and the save does not exit 0. The save runs in a child process, as
// a real signal is the stimulus and the save prints to the process's stdout.
func TestSnapshotSaveStoppedAfterTheSnapshotExitsClean(t *testing.T) {
	if mode := os.Getenv("STRATUM_SNAPSHOT_SAVE_SIGNAL_CHILD"); mode != "" {
		snapshotSaveSignalChild(t, mode == "unreadable-config")
		return
	}
	for _, tc := range []struct {
		mode      string
		want, not string
	}{
		{mode: "clean", want: "snapshot save stopped (signal received)", not: "node-state bundle incomplete"},
		{mode: "unreadable-config", want: "snapshot saved: ", not: "snapshot save stopped"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSnapshotSaveStoppedAfterTheSnapshotExitsClean$")
			cmd.Env = append(os.Environ(), "STRATUM_SNAPSHOT_SAVE_SIGNAL_CHILD="+tc.mode)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("signal child failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.want) || strings.Contains(string(out), tc.not) {
				t.Errorf("a save stopped by a signal after its snapshot printed:\n%s\nwant %q and not %q", out, tc.want, tc.not)
			}
		})
	}
}

// snapshotSaveSignalChild is the child half of
// TestSnapshotSaveStoppedAfterTheSnapshotExitsClean: a real save whose
// snapshot's S3 upload is where the signal lands.
func snapshotSaveSignalChild(t *testing.T, unreadableConfig bool) {
	dir := realTempDir(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "etcdctl"), []byte("#!/bin/sh\nprintf snapshot > \"$3\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldBin, oldCfg, oldSave, oldLoad := version.BinDir, loadSnapshotConfig, saveSnapshot, loadNodeStateBackup
	defer func() {
		version.BinDir, loadSnapshotConfig, saveSnapshot, loadNodeStateBackup = oldBin, oldCfg, oldSave, oldLoad
	}()
	version.BinDir = bin
	saveSnapshot = etcdsnap.SaveContext
	// The bucket signals this process once, on the first request: a second
	// signal would kill it under the interrupt contract, so a retry the
	// client sends before it sees the stop must not send another. Every
	// request is held until the save gives up on it or the test is over.
	release := make(chan struct{})
	var signalOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signalOnce.Do(func() {
			if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
				t.Error(err)
			}
		})
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		srv.Close()
	}()
	loadSnapshotConfig = func() (etcdsnap.Config, error) {
		return etcdsnap.Config{Dir: dir, S3: objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}}, nil
	}
	loadNodeStateBackup = func() (*etcdsnap.NodeState, error) { return &etcdsnap.NodeState{NodeIP: "10.0.0.1"}, nil }
	if unreadableConfig {
		loadNodeStateBackup = func() (*etcdsnap.NodeState, error) { return nil, errors.New("config.yaml: unreadable") }
	}
	cmd := newSnapshotSaveCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(nil)
	err := cmd.Execute()
	var exit *ErrExitCode
	if unreadableConfig {
		if err == nil || errors.As(err, &exit) || !errors.Is(err, etcdsnap.ErrNodeStateIncomplete) ||
			!strings.Contains(err.Error(), "no bundle was taken, because the cluster config could not be read") {
			t.Fatalf("a signal-stopped save whose config could not be read returned %v, want the unread config as a failure, not a clean stop", err)
		}
	} else if !errors.As(err, &exit) || exit.Code != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("a save stopped by a signal after its snapshot returned %v, want a clean stop that exits 0", err)
	}
	if snaps, _ := filepath.Glob(filepath.Join(dir, "*.db")); len(snaps) != 1 {
		t.Fatalf("snapshots = %v, want the one taken before the signal", snaps)
	}
}

// testCA is a self-signed CA certificate and its key, PEM encoded: a restore
// checks that a bundle's CA key is its CA certificate's.
func testCA(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "stratum-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}

// A cluster config the node kept outside its configuration directory is
// named before anything is written, and nothing is written until the operator
// allows its path: a node restored without the config it starts with would
// have to be wiped to try again.
func TestRestoreNodeStateSettlesAnOutsideConfigBeforeItWrites(t *testing.T) {
	nodeStateRoots(t)
	ca, caKey := testCA(t)
	writeNodeFile(t, filepath.Join(version.PKIDir, "ca.crt"), ca)
	writeNodeFile(t, filepath.Join(version.PKIDir, "ca.key"), caKey)
	writeNodeFile(t, nodestate.StatePath(), "role: controller\n")
	extra := filepath.Join(t.TempDir(), "site", "cluster.yaml")
	writeNodeFile(t, extra, "spec: {site: true}\n")
	id, err := nodebackup.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := nodebackup.Collect(nodebackup.Sources{PKIDir: version.PKIDir, StatePath: nodestate.StatePath(),
		ExtraConfig: extra, ConfigPath: extra}, nodebackup.Meta{Hostname: "cp-1", StratumVersion: version.StratumVersion})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := nodebackup.Seal([]nodebackup.Recipient{id.Recipient()}, plain)
	if err != nil {
		t.Fatal(err)
	}
	keep := t.TempDir()
	bundle, identity := filepath.Join(keep, "b"), filepath.Join(keep, "identity")
	secret, _ := id.String()
	writeNodeFile(t, bundle, string(sealed))
	writeNodeFile(t, identity, secret)
	for _, p := range []string{version.PKIDir, version.K8sConfDir, filepath.Dir(extra)} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	oldHost := nodeStateHostname
	t.Cleanup(func() { nodeStateHostname = oldHost })
	nodeStateHostname = func() (string, error) { return "cp-1", nil }

	out, err := runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes")
	if err == nil || !strings.Contains(err.Error(), "--allow-path "+extra) {
		t.Fatalf("error = %v, want one naming --allow-path %s", err, extra)
	}
	if caKeyRestored() {
		t.Fatal("the CA key was written before the outside config's path was settled")
	}
	if !strings.Contains(out, extra) {
		t.Errorf("the bundle's summary does not name the outside config:\n%s", out)
	}
	out, err = runRestoreNodeState(t, bundle, "--identity-file", identity, "--yes", "--allow-path", extra, "--no-ca-pin")
	if err != nil {
		t.Fatalf("with --allow-path: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(extra); err != nil || string(b) != "spec: {site: true}\n" {
		t.Errorf("the outside config = %q, %v", b, err)
	}
	if !strings.Contains(out, "stratum up --config '"+extra+"'") {
		t.Errorf("the next step does not start the node with the config it ran with:\n%s", out)
	}
}

// An s3:// bundle is fetched from the bucket and key the URL names. A
// snapshot config already on the machine says how to reach the bucket and
// nothing about where the bundle is: its prefix is the snapshots', one level
// up.
func TestRestoreNodeStateFetchesTheS3ObjectTheURLNames(t *testing.T) {
	bundle, identity, _ := lostNode(t)
	sealed, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	oldCfg, oldDL, oldSize := nodeStateS3Config, s3Download, s3ObjectSize
	t.Cleanup(func() { nodeStateS3Config, s3Download, s3ObjectSize = oldCfg, oldDL, oldSize })
	size := int64(len(sealed))
	s3ObjectSize = func(context.Context, *objstore.Target, string) (int64, error) { return size, nil }
	nodeStateS3Config = func(etcdsnap.Config) (etcdsnap.Config, error) {
		return etcdsnap.Config{S3: objstore.Target{Bucket: "other", Prefix: "snaps", Endpoint: "https://conf.example", AccessKey: "k", SecretKey: "s"}}, nil
	}
	var got objstore.Target
	var gotName string
	s3Download = func(t *objstore.Target, _ context.Context, root *os.Root, name string) error {
		got, gotName = *t, name
		return root.WriteFile(name, sealed, 0o600)
	}
	out, err := runRestoreNodeState(t, "s3://bkt/snaps/node-state/cp-1.bundle", "--identity-file", identity, "--yes", "--no-ca-pin", "--s3-endpoint", "https://flag.example")
	if err != nil {
		t.Fatalf("restore from s3://: %v\n%s", err, out)
	}
	if got.Bucket != "bkt" || got.Prefix != "snaps/node-state" || gotName != "cp-1.bundle" {
		t.Errorf("fetched bucket %q prefix %q name %q, want the URL's", got.Bucket, got.Prefix, gotName)
	}
	if got.Endpoint != "https://flag.example" || got.AccessKey != "k" {
		t.Errorf("target = %+v, want the flag's endpoint over the config's, and the config's credentials", got)
	}
	if !caKeyRestored() {
		t.Error("the bundle fetched from S3 was not restored")
	}
	// An object too large to be a bundle is turned away before any of it is
	// downloaded onto the replacement machine's disk.
	size, gotName = maxSealedBundle+1, ""
	if _, err := fetchSealedBundle(context.Background(), "s3://bkt/snaps/node-state/huge.bundle", nodeStateS3Flags{}); err == nil || gotName != "" {
		t.Errorf("an oversized object: error = %v, downloaded %q; want a refusal with no download", err, gotName)
	}
	for _, bad := range []string{"s3://bkt", "s3://bkt/", "s3://bkt/dir/"} {
		if _, err := fetchSealedBundle(context.Background(), bad, nodeStateS3Flags{}); err == nil {
			t.Errorf("%s was accepted as naming a bundle", bad)
		}
	}
}

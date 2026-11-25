package etcdsnap_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/backupevidence"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/nodebackup"
	"github.com/ctrliq/stratum/internal/objstore"
	"github.com/ctrliq/stratum/internal/version"
)

// nodeStateFixture is a node's state under a temp root, an identity, and the
// save option that bundles the one to the other.
func nodeStateFixture(t *testing.T) (*etcdsnap.NodeState, nodebackup.Identity) {
	t.Helper()
	root := t.TempDir()
	pki := filepath.Join(root, "pki")
	state := filepath.Join(root, "config", "node.yaml")
	ca, caKey := testCA(t)
	for path, data := range map[string]string{
		filepath.Join(pki, "ca.crt"): ca,
		filepath.Join(pki, "ca.key"): caKey,
		state:                        "role: controller\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldLock := version.NodeUpgradeLockPath
	version.NodeUpgradeLockPath = filepath.Join(root, "node.lock")
	t.Cleanup(func() { version.NodeUpgradeLockPath = oldLock })
	id, err := nodebackup.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return &etcdsnap.NodeState{
		Recipients: []nodebackup.Recipient{id.Recipient()},
		Sources:    nodebackup.Sources{PKIDir: pki, StatePath: state},
		NodeIP:     "10.0.0.1",
	}, id
}

// recordingBucket accepts every upload and delete, lists the keys it was
// given under the request's prefix one level deep, and records what it saw.
type recordingBucket struct {
	mu      sync.Mutex
	puts    []string
	deletes []string
	listed  map[string][]string
}

func (b *recordingBucket) target(t *testing.T, prefix string) *objstore.Target {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/b/")
		switch r.Method {
		case http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			b.puts = append(b.puts, key)
			w.Header().Set("ETag", "\"x\"")
		case http.MethodDelete:
			b.deletes = append(b.deletes, key)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>b</Name><IsTruncated>false</IsTruncated>`)
			for i, k := range b.listed[r.URL.Query().Get("prefix")] {
				fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>1</Size><LastModified>2026-01-%02dT00:00:00.000Z</LastModified></Contents>`, k, i+1)
			}
			fmt.Fprint(w, `</ListBucketResult>`)
		default:
			http.Error(w, "unexpected "+r.Method, http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return &objstore.Target{Endpoint: srv.URL, Bucket: "b", Prefix: prefix, AccessKey: "k", SecretKey: "s", PathStyle: true}
}

// A save with recipients takes the bundle beside its snapshot: sealed, mode
// 0600, in a directory of its own, uploaded one prefix below the snapshot,
// naming the snapshot it belongs with, and recorded in the evidence.
func TestSaveTakesAndUploadsTheNodeStateBundle(t *testing.T) {
	redirectEvidence(t)
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	ns, id := nodeStateFixture(t)
	var bucket recordingBucket
	path, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Evidence: true, S3: bucket.target(t, "clusters/a"), NodeState: ns,
	})
	if err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	snapshot := filepath.Base(path)
	bundles, err := filepath.Glob(filepath.Join(dir, etcdsnap.NodeStateDir, "node-state-cp1-*.bundle"))
	if err != nil || len(bundles) != 1 {
		t.Fatalf("bundles beside the snapshot = %v (%v), want one", bundles, err)
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(snapshot, "etcd-snapshot-cp1-"), ".db")
	if want := etcdsnap.NodeStateName("cp1", stamp); filepath.Base(bundles[0]) != want {
		t.Errorf("bundle is %s, want %s: the stamp its snapshot carries", filepath.Base(bundles[0]), want)
	}
	if fi, err := os.Stat(bundles[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("bundle mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	sealed, err := os.ReadFile(bundles[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "PRIVATE KEY") {
		t.Fatal("the bundle on disk holds the CA key in the clear")
	}
	plain, err := nodebackup.Open(id, sealed)
	if err != nil {
		t.Fatalf("the bundle does not open with the recipient's identity: %v", err)
	}
	m, err := nodebackup.Inspect(plain, nodebackup.Destination{PKIDir: ns.Sources.PKIDir, StatePath: ns.Sources.StatePath})
	if err != nil || m.Snapshot != snapshot || m.Hostname != "cp1" || m.NodeIP != "10.0.0.1" {
		t.Errorf("manifest = %+v (%v), want this host and snapshot %s", m, err, snapshot)
	}
	want := []string{"clusters/a/" + snapshot, "clusters/a/node-state/" + filepath.Base(bundles[0])}
	if got := slices.Sorted(slices.Values(bucket.puts)); !slices.Equal(got, want) {
		t.Errorf("uploaded %v, want %v", got, want)
	}
	rec, err := backupevidence.LoadAttempts()
	if err != nil {
		t.Fatal(err)
	}
	if rec.NodeState.AttemptedAt.IsZero() || rec.NodeState.Result != "" || rec.LastNodeStateSuccess.IsZero() {
		t.Errorf("the bundle's save was not recorded as a success: %+v", rec.NodeState)
	}
	if rec.NodeStateUpload.AttemptedAt.IsZero() || rec.NodeStateUpload.Result != "" || rec.LastNodeStateUploadSuccess.IsZero() {
		t.Errorf("the bundle's upload was not recorded as a success: %+v", rec.NodeStateUpload)
	}
}

// With no recipient there is no bundle in any form, on the node or in S3,
// and no attempt recorded: the node's PKI never leaves its directory.
func TestSaveWithoutRecipientsTakesNoBundle(t *testing.T) {
	redirectEvidence(t)
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	var bucket recordingBucket
	if _, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Evidence: true, S3: bucket.target(t, ""),
	}); err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, etcdsnap.NodeStateDir)); !os.IsNotExist(err) {
		t.Errorf("a save with no recipient made the bundle directory (stat: %v)", err)
	}
	if len(bucket.puts) != 1 || strings.Contains(bucket.puts[0], "node-state") {
		t.Errorf("uploaded %v, want the snapshot alone", bucket.puts)
	}
	rec, err := backupevidence.LoadAttempts()
	if err != nil {
		t.Fatal(err)
	}
	if !rec.NodeState.IsZero() || !rec.NodeStateUpload.IsZero() {
		t.Errorf("a save with no recipient recorded a bundle attempt: %+v %+v", rec.NodeState, rec.NodeStateUpload)
	}
}

// A bundle that cannot be taken leaves the snapshot saved and uploaded, and
// the save says the backup set is incomplete instead of reporting success.
func TestSaveReportsABundleItCouldNotTake(t *testing.T) {
	redirectEvidence(t)
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	ns, _ := nodeStateFixture(t)
	if err := os.RemoveAll(ns.Sources.PKIDir); err != nil {
		t.Fatal(err)
	}
	var bucket recordingBucket
	path, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Evidence: true, S3: bucket.target(t, ""), NodeState: ns,
	})
	if !errors.Is(err, etcdsnap.ErrNodeStateIncomplete) {
		t.Fatalf("error = %v, want ErrNodeStateIncomplete", err)
	}
	if _, serr := os.Stat(path); path == "" || serr != nil {
		t.Fatalf("the snapshot was not kept beside the failed bundle: %q (%v)", path, serr)
	}
	if len(bucket.puts) != 1 {
		t.Errorf("uploaded %v, want the snapshot", bucket.puts)
	}
	rec, lerr := backupevidence.LoadAttempts()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if rec.Save.Result != "" || rec.LastSaveSuccess.IsZero() {
		t.Errorf("the snapshot's own success was disturbed by the bundle's failure: %+v", rec.Save)
	}
	if rec.NodeState.Result == "" || !rec.LastNodeStateSuccess.IsZero() || !rec.NodeStateUpload.IsZero() {
		t.Errorf("the failed bundle was not recorded as a failure with no upload: %+v %+v", rec.NodeState, rec.NodeStateUpload)
	}
}

// Retention keeps the newest bundles of this host, locally and in S3, and
// touches neither another host's nor any snapshot.
func TestSavePrunesOnlyThisHostsBundles(t *testing.T) {
	redirectEvidence(t)
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	ns, _ := nodeStateFixture(t)
	sub := filepath.Join(dir, etcdsnap.NodeStateDir)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{
		etcdsnap.NodeStateName("cp1", "20260101-000000"),
		etcdsnap.NodeStateName("cp1", "20260102-000000"),
		etcdsnap.NodeStateName("cp1-2", "20260101-000000"),
	} {
		p := filepath.Join(sub, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	bucket := recordingBucket{listed: map[string][]string{"node-state/": {
		"node-state/" + etcdsnap.NodeStateName("cp1", "20260101-000000"),
		"node-state/" + etcdsnap.NodeStateName("cp1", "20260102-000000"),
		"node-state/" + etcdsnap.NodeStateName("cp1-2", "20260101-000000"),
		"node-state/etcd-snapshot-cp1-20260101-000000.db",
	}}}
	if _, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Retain: 1, S3: bucket.target(t, ""), NodeState: ns,
	}); err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	left, err := os.ReadDir(sub)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if len(names) != 2 || !slices.Contains(names, etcdsnap.NodeStateName("cp1-2", "20260101-000000")) ||
		slices.Contains(names, etcdsnap.NodeStateName("cp1", "20260101-000000")) ||
		slices.Contains(names, etcdsnap.NodeStateName("cp1", "20260102-000000")) {
		t.Errorf("bundles left on the node = %v, want this save's and the other host's", names)
	}
	wantGone := []string{
		"node-state/" + etcdsnap.NodeStateName("cp1", "20260101-000000"),
	}
	if got := slices.Sorted(slices.Values(bucket.deletes)); !slices.Equal(got, wantGone) {
		t.Errorf("deleted from S3 %v, want %v: the listing's newest of this host stays, and nothing else is this host's bundle", got, wantGone)
	}
}

// The snapshot listing shows snapshots and the bundle listing bundles, each
// blind to the other's files, so nothing takes a bundle for a snapshot.
func TestBundlesAreListedApartFromSnapshots(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, etcdsnap.NodeStateDir)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	bundle := etcdsnap.NodeStateName("cp1", "20260101-000000")
	for _, p := range []string{filepath.Join(dir, "etcd-snapshot-cp1-20260101-000000.db"), filepath.Join(sub, bundle)} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snaps, err := etcdsnap.List(dir, nil)
	if err != nil || len(snaps) != 1 || snaps[0].Name != "etcd-snapshot-cp1-20260101-000000.db" {
		t.Errorf("List = %+v (%v), want the snapshot alone", snaps, err)
	}
	got, err := etcdsnap.ListNodeState(dir, nil)
	if err != nil || len(got) != 1 || got[0].Name != "node-state/"+bundle || got[0].Location != "local" {
		t.Errorf("ListNodeState = %+v (%v), want the bundle alone", got, err)
	}
	if none, err := etcdsnap.ListNodeState(t.TempDir(), nil); err != nil || len(none) != 0 {
		t.Errorf("ListNodeState on a node with no bundle = %+v, %v", none, err)
	}
}

// The bundles' S3 target is the snapshots' bucket one prefix down, with or
// without a prefix of its own, and nothing when there is no target.
func TestNodeStateTargetIsOnePrefixBelowTheSnapshots(t *testing.T) {
	for prefix, want := range map[string]string{"": "node-state", "clusters/a": "clusters/a/node-state", "clusters/a/": "clusters/a/node-state"} {
		got := etcdsnap.NodeStateTarget(&objstore.Target{Bucket: "b", Prefix: prefix})
		if got == nil || got.Prefix != want || got.Bucket != "b" {
			t.Errorf("prefix %q: target = %+v, want prefix %q", prefix, got, want)
		}
	}
	if got := etcdsnap.NodeStateTarget(nil); got != nil {
		t.Errorf("no S3 target gave a bundle target %+v", got)
	}
	if got := etcdsnap.NodeStateTarget(&objstore.Target{}); got != nil {
		t.Errorf("a target with no bucket gave a bundle target %+v", got)
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

// A snapshot saved under a name of its own is never pruned, so its bundle is
// named for it and never pruned either: a kept snapshot with its bundle
// pruned away could not rebuild a control plane.
func TestANamedSnapshotsBundleIsKeptWithIt(t *testing.T) {
	redirectEvidence(t)
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	ns, _ := nodeStateFixture(t)
	save := func(name string) {
		t.Helper()
		if _, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
			Dir: dir, Host: "cp1", Name: name, Retain: 1, NodeState: ns,
		}); err != nil {
			t.Fatalf("SaveContext(%q): %v", name, err)
		}
	}
	save("pre-change.db")
	kept := filepath.Join(dir, etcdsnap.NodeStateDir, "node-state-pre-change.bundle")
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("the named snapshot's bundle is not named for it: %v", err)
	}
	save("")
	time.Sleep(1100 * time.Millisecond)
	save("")
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("scheduled saves pruned the bundle of a named snapshot that is itself kept: %v", err)
	}
	scheduled, _ := filepath.Glob(filepath.Join(dir, etcdsnap.NodeStateDir, "node-state-cp1-*.bundle"))
	if len(scheduled) != 1 {
		t.Errorf("scheduled bundles left = %v, want the newest one", scheduled)
	}
}

// A bundle retention that fails is in the evidence, as a snapshot's is: one
// that keeps failing fills the bucket with bundles nobody is told about.
func TestAFailedBundlePruneIsRecorded(t *testing.T) {
	redirectEvidence(t)
	dir := t.TempDir()
	fakeEtcdctl(t, "printf snapshot > \"$3\"")
	ns, _ := nodeStateFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("ETag", "\"x\"")
		case r.Method == http.MethodGet && r.URL.Query().Get("prefix") == "node-state/":
			http.Error(w, "denied", http.StatusForbidden)
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><Name>b</Name><IsTruncated>false</IsTruncated></ListBucketResult>`)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	target := &objstore.Target{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", PathStyle: true}
	if _, err := etcdsnap.SaveContext(context.Background(), io.Discard, etcdsnap.SaveOptions{
		Dir: dir, Host: "cp1", Retain: 1, Evidence: true, S3: target, NodeState: ns,
	}); err != nil {
		t.Fatalf("SaveContext: %v", err)
	}
	rec, err := backupevidence.LoadAttempts()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rec.Warnings, func(w string) bool { return strings.Contains(w, "node-state prune") }) {
		t.Errorf("the failed bundle prune is not among the recorded warnings: %v", rec.Warnings)
	}
}

package certs

import (
	"io"

	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// The multi-node harness for `certs migrate-etcd-ca --cluster`.
//
// These tests are about the ORCHESTRATION — phase-major ordering, the drop-root
// gate, refusal on a partially-migrated cluster, halting safely, rollback.
// internal/pki already covers the per-node primitives in 939 lines and none of
// that is repeated here.
//
// What makes the assertions mean anything is that the driver below is not a
// mock. version.PKIDir is a package var, so each node gets its own REAL PKI
// directory, and RunPhase calls the production per-node path
// (migrateEtcdCAWith) against real files — so a phase a real node would refuse
// is refused here too, and the cross-node checks verify certificates with
// crypto/x509 rather than trusting a bookkeeping flag. The primitives and the
// per-node sequencing are the fixture; the cluster logic is what is under test.
//
// version.PKIDir is global, so none of these may run in parallel — the same
// constraint internal/pki's own tests already live under.

// fakeCluster is an etcdDriver backed by one real PKI directory per node.
type fakeCluster struct {
	t     *testing.T
	order []string          // node names, initiator first
	dirs  map[string]string // node -> its PKI directory
	calls []string          // every (node, phase) dispatch, in order

	failAt           map[string]bool // "node:phase" -> RunPhase returns an error
	failRestartAt    map[string]bool // "node:phase" -> the phase writes, then its restart fails
	unreachable      map[string]bool // node -> Survey reports unknown
	unreachableAfter map[string]bool // "node:phase" -> node goes unknown AFTER this phase's etcd restart
	// transientFor reports unknown+transient for the first N surveys of the
	// node, then its real running state — what a just-restarted etcd looks
	// like while its listener is up but it is not back in raft yet.
	transientFor  map[string]int
	transientSeen map[string]int
	// notListening makes transientFor's members report the refused-port
	// kind (etcd not listening yet) instead of the stalled one.
	notListening map[string]bool
	// surveys counts Survey calls per node, for assertions that a node was
	// probed exactly once (definite unknowns are never retried).
	surveys map[string]int

	// restarts records the components each node bounced, in order, so a test can
	// assert the per-phase restart SET (not just that etcd moved).
	restarts map[string][]string
	// confirmed counts ConfirmClosed calls; confirmFail forces it to fail for a
	// node, so the closing-verification failure path is testable.
	confirmed   int
	confirmFail map[string]bool

	// running is what each node's etcd has actually ADOPTED — updated only when
	// a restart succeeds. Survey reads this, never the disk.
	//
	// This is the whole premise of the design under test: etcd builds its CA
	// pool once at startup, so a bundle can be written while the process still
	// enforces the previous one. A fake that surveyed the filesystem could never
	// produce that divergence, and gateDropRoot would never be exercised against
	// the failure it exists to catch.
	running map[string]string

	healthErr, quorumErr error

	// published/cleaned track the fetch-key lifetime: the key that decrypts
	// every cluster private key must be removed on every exit path, not only
	// the happy one.
	published, cleaned, reaped int
	interrupt                  error
}

func newFakeCluster(t *testing.T, names ...string) (*fakeCluster, []etcdNode) {
	t.Helper()
	f := &fakeCluster{
		t: t, dirs: map[string]string{},
		failAt: map[string]bool{}, failRestartAt: map[string]bool{},
		unreachable: map[string]bool{}, unreachableAfter: map[string]bool{},
		transientFor: map[string]int{}, transientSeen: map[string]int{}, notListening: map[string]bool{},
		surveys: map[string]int{},
		running: map[string]string{}, restarts: map[string][]string{},
		confirmFail: map[string]bool{},
	}
	saved := version.PKIDir
	t.Cleanup(func() { version.PKIDir = saved })

	// One cluster, one root. The shared CA must be byte-identical on every node,
	// or "is this leaf root-signed" quietly means something different per node
	// and every cross-node assertion below is measuring nothing. Mint it once,
	// distribute it, THEN issue each node's leaves from it — which is the order a
	// real HA join uses (internal/ha, sharedCerts).
	rootDir := t.TempDir()
	version.PKIDir = rootDir
	if err := pki.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := pki.EnsureCA("ca", "stratum-ca"); err != nil {
		t.Fatal(err)
	}

	nodes := make([]etcdNode, 0, len(names))
	for i, name := range names {
		dir := t.TempDir()
		f.dirs[name] = dir
		ip := fmt.Sprintf("10.0.0.%d", i+1)
		nodes = append(nodes, etcdNode{Name: name, IP: ip})
		f.order = append(f.order, name)
		for _, file := range []string{"ca.crt", "ca.key"} {
			copyFile(t, filepath.Join(rootDir, file), filepath.Join(dir, file))
		}
		// Every node starts as a pre-split control plane (etcd trusts the cluster
		// root): all leaves signed by the shared cluster root, no etcd CA on
		// disk. That is the only state from which this migration is ever needed.
		f.in(name, func() { seedUnmigratedNode(t, name, ip) })
		f.running[name] = pki.EtcdStateNotMigrated
	}
	return f, nodes
}

// in runs fn with version.PKIDir pointed at one node's directory.
func (f *fakeCluster) in(node string, fn func()) {
	f.t.Helper()
	saved := version.PKIDir
	version.PKIDir = f.dirs[node]
	defer func() { version.PKIDir = saved }()
	fn()
}

func (f *fakeCluster) initiator() string { return f.order[0] }

// distributeEtcdCA is what ha.DownloadCertsAdmin does for real: hand a follower
// the cluster's etcd CA and its current trust bundle. Without it
// MigrateEtcdTrustBoth(allowMint=false) correctly refuses on a follower.
func (f *fakeCluster) distributeEtcdCA(node string) {
	f.t.Helper()
	init := f.initiator()
	if node == init {
		return
	}
	for _, file := range []string{
		pki.EtcdCAName + ".crt", pki.EtcdCAName + ".key",
		pki.EtcdBundleBase + "-bundle.crt",
	} {
		src := filepath.Join(f.dirs[init], file)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		copyFile(f.t, src, filepath.Join(f.dirs[node], file))
	}
}

// Survey reports what the node's etcd is RUNNING, which is what the real
// driver's TLS handshake reads — not what is on its disk.
func (f *fakeCluster) Survey(n etcdNode) (string, string, bool) {
	f.surveys[n.Name]++
	if budget, transient := f.transientFor[n.Name]; transient {
		f.transientSeen[n.Name]++
		if f.transientSeen[n.Name] <= budget {
			if f.notListening[n.Name] {
				return pki.EtcdStateUnknown,
					"could not complete a TLS handshake with etcd at " + n.IP + ": etcd is not listening: connection refused", true
			}
			return pki.EtcdStateUnknown,
				"could not complete a TLS handshake with etcd at " + n.IP +
					": etcd accepted the connection but did not answer TLS (starting up or not yet back in raft)", true
		}
	}
	if f.unreachable[n.Name] {
		return pki.EtcdStateUnknown, "could not complete a TLS handshake with etcd at " + n.IP, false
	}
	return f.running[n.Name], "", false
}

// diskState is the file-derived state, for assertions that care about what was
// written rather than what was adopted.
func (f *fakeCluster) diskState(node string) string {
	var s string
	f.in(node, func() { s = pki.EtcdMigrationState() })
	return s
}

func (f *fakeCluster) RunPhase(n etcdNode, phase string, fetchShared, force bool) error {
	f.calls = append(f.calls, n.Name+":"+phase)
	if f.failAt[n.Name+":"+phase] {
		return fmt.Errorf("node %s is unreachable", n.Name)
	}
	var err error
	f.in(n.Name, func() {
		err = migrateEtcdCAWith(io.Discard,
			EtcdCAOptions{Phase: phase, FetchShared: fetchShared, Force: force, Quiet: true},
			nodeIdent{Hostname: n.Name, NodeIP: n.IP},
			etcdNodeOps{
				// Stands in for ha.DownloadCertsAdmin: the follower receives the
				// cluster's etcd CA and its current trust bundle. Everything else
				// in the per-node path is the production code.
				fetchShared: func() error { f.distributeEtcdCA(n.Name); return nil },
				// This machine renders no manifests, so the real staticpod.Exists
				// would report every control plane as a worker.
				podExists: func(string) bool { return true },
				restartPod: func(name string) error {
					f.restarts[n.Name] = append(f.restarts[n.Name], name)
					if f.failRestartAt[n.Name+":"+phase] {
						return fmt.Errorf("node %s: %s did not restart", n.Name, name)
					}
					// etcd adopting the new trust material is the ONLY thing that
					// moves a node's running state. Anything that writes a bundle
					// without reaching here leaves the process behind its disk.
					if name == "etcd" {
						f.running[n.Name] = f.diskStateLocked(n.Name)
						// Simulate a node whose etcd flaps unreachable right after
						// adopting this phase — so a later survey reads it as unknown.
						if f.unreachableAfter[n.Name+":"+phase] {
							f.unreachable[n.Name] = true
						}
					}
					return nil
				},
				waitPod: func(string) error { return nil },
			})
	})
	return err
}

// diskStateLocked is diskState for callers already inside f.in.
func (f *fakeCluster) diskStateLocked(string) string { return pki.EtcdMigrationState() }

func (f *fakeCluster) PublishEtcdCA() (func(), error) {
	f.published++
	return func() { f.cleaned++ }, nil
}

func (f *fakeCluster) ReapStaleFetchKey() { f.reaped++ }

func (f *fakeCluster) Interrupted() error { return f.interrupt }

func (f *fakeCluster) ClusterHealthy([]etcdNode) error { return f.healthErr }

func (f *fakeCluster) EtcdQuorumHealthy([]etcdNode) error { return f.quorumErr }

// ConfirmClosed stands in for the TLS probe. On a real node it asks the running
// etcd; here it asks the same question of the trust bundle that etcd would build
// its pool from — a cluster-root-signed client leaf must no longer verify.
func (f *fakeCluster) ConfirmClosed(n etcdNode) error {
	f.confirmed++
	if f.confirmFail[n.Name] {
		return fmt.Errorf("etcd still accepts a cluster-root-signed client certificate")
	}
	var accepted bool
	f.in(n.Name, func() { accepted = etcdAcceptsRootSignedLeaf(f.t, f.dirs[n.Name]) })
	if accepted {
		return fmt.Errorf("etcd still accepts a cluster-root-signed client certificate")
	}
	return nil
}

func (f *fakeCluster) migration(nodes []etcdNode, resume bool) *etcdMigration {
	return &etcdMigration{nodes: nodes, drv: f, resume: resume, w: io.Discard,
		sleep: func(time.Duration) {}, // tests must not wall-clock
	}
}

// state is the surveyed (running) state of one node, for assertions.
func (f *fakeCluster) state(node string) string { return f.running[node] }

// ─── fixtures ────────────────────────────────────────────────────────────────

// seedUnmigratedNode builds a control plane as it looked before the etcd CA split: every
// leaf, etcd's included, signed by the cluster root, and no etcd CA.
func seedUnmigratedNode(t *testing.T, host, ip string) {
	t.Helper()
	if err := pki.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	// EnsureCA here LOADS the shared root already copied in; it never mints one.
	// Deliberately not pki.GenerateAll: that calls EnsureEtcdTrustBundle, which
	// would mint an etcd CA and leave the node at trust-both before the test has
	// run anything — a fixture that starts halfway through the migration under
	// test.
	ca, err := pki.EnsureCA("ca", "stratum-ca")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range pki.LeafSpecs(host, ip, nil) {
		if err := ca.Issue(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := pki.WriteCABundle("ca", "ca"); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
	mode := os.FileMode(0o644)
	if strings.HasSuffix(src, ".key") {
		mode = 0o600
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		t.Fatalf("copy to %s: %v", dst, err)
	}
}

// etcdAcceptsRootSignedLeaf runs the decision etcd makes for an incoming client
// under `--client-cert-auth --trusted-ca-file etcd-ca-bundle.crt`: verify the
// chain against a pool built from that file, with the client-auth EKU. Same
// call path etcd's TLS stack takes, so a leaf this accepts is one etcd accepts.
func etcdAcceptsRootSignedLeaf(t *testing.T, dir string) bool {
	t.Helper()
	return leafVerifiesAgainstEtcdBundle(t, dir, dir, "kubelet")
}

// leafVerifiesAgainstEtcdBundle checks a leaf from leafDir against the etcd
// trust bundle in bundleDir. Cross-node use is the point: it answers "would
// THAT node's etcd accept THIS node's certificate", which is the property that
// decides whether a halted migration left a working cluster.
func leafVerifiesAgainstEtcdBundle(t *testing.T, leafDir, bundleDir, leaf string) bool {
	t.Helper()
	pool := x509.NewCertPool()
	b, err := os.ReadFile(filepath.Join(bundleDir, pki.EtcdBundleBase+"-bundle.crt"))
	if err != nil {
		// No etcd bundle yet: an unmigrated node's etcd trusts the cluster root.
		b, err = os.ReadFile(filepath.Join(bundleDir, "ca-bundle.crt"))
		if err != nil {
			t.Fatalf("no trust material in %s: %v", bundleDir, err)
		}
	}
	if !pool.AppendCertsFromPEM(b) {
		t.Fatalf("no usable anchors in %s", bundleDir)
	}
	c := loadLeaf(t, filepath.Join(leafDir, leaf+".crt"))
	_, err = c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	return err == nil
}

func loadLeaf(t *testing.T, p string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("no PEM block in %s", p)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	return c
}

// assertClusterStillWorks is the assertion that turns "halting is safe" from a
// comment into a test: for EVERY pair of control planes, the certificate one
// serves must verify against the anchors the other enforces. If that holds, no
// member has been locked out of raft, whatever phase the run stopped at.
func assertClusterStillWorks(t *testing.T, f *fakeCluster) {
	t.Helper()
	for _, from := range f.order {
		for _, to := range f.order {
			if !leafVerifiesAgainstEtcdBundle(t, f.dirs[from], f.dirs[to], "etcd") {
				t.Errorf("%s's etcd certificate does not verify against %s's etcd trust bundle — "+
					"%s has been locked out of raft by a halted migration", from, to, from)
			}
		}
	}
}

func phaseCalls(f *fakeCluster, phase string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasSuffix(c, ":"+phase) {
			out = append(out, strings.TrimSuffix(c, ":"+phase))
		}
	}
	return out
}

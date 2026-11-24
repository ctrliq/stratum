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

// Orchestration harness; each node gets a real PKI dir, so nothing runs in parallel.

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
	// unknown+transient for the first N surveys, then the real state, as a just-restarted etcd looks.
	transientFor  map[string]int
	transientSeen map[string]int
	// Makes transientFor's members report the refused-port kind instead of the stalled one.
	notListening map[string]bool
	// Counts Survey calls per node; definite unknowns are never retried.
	surveys map[string]int

	// Records each node's bounced components in order, for the per-phase restart SET.
	restarts map[string][]string
	// Counts ConfirmClosed calls; confirmFail forces a failure for one node.
	confirmed   int
	confirmFail map[string]bool

	// etcd loads its CA pool once at startup; Survey reads what a restart adopted, never the disk.
	running map[string]string

	healthErr, quorumErr error

	// Tracks the fetch-key lifetime: removed on every exit path, not only the happy one.
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

	// The shared CA must be byte-identical on every node; mint once, distribute, then issue leaves.
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
		// Every node starts pre-split: leaves root-signed, no etcd CA on disk.
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

// What ha.DownloadCertsAdmin does for real; without it MigrateEtcdTrustBoth(false) refuses.
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

// Reports what the node's etcd is RUNNING, not what is on its disk.
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

// The file-derived state, for assertions about what was written rather than adopted.
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
				// Stands in for ha.DownloadCertsAdmin; the rest of the per-node path is production code.
				fetchShared: func() error { f.distributeEtcdCA(n.Name); return nil },
				// No manifests render here, so real staticpod.Exists would report every CP a worker.
				podExists: func(string) bool { return true },
				restartPod: func(name string) error {
					f.restarts[n.Name] = append(f.restarts[n.Name], name)
					if f.failRestartAt[n.Name+":"+phase] {
						return fmt.Errorf("node %s: %s did not restart", n.Name, name)
					}
					// Adoption on restart is the only thing that moves a node's running state.
					if name == "etcd" {
						f.running[n.Name] = f.diskStateLocked(n.Name)
						// etcd flaps unreachable right after adopting, so a later survey reads unknown.
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

// Asks of the bundle the question the TLS probe asks the running etcd.
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

// A control plane as before the etcd CA split: leaves root-signed, no etcd CA.
func seedUnmigratedNode(t *testing.T, host, ip string) {
	t.Helper()
	if err := pki.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	// EnsureCA here only loads the copied-in root; pki.GenerateAll would mint an etcd CA.
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

// etcd's own accept decision: verify the chain against the bundle's pool with the client-auth EKU.
func etcdAcceptsRootSignedLeaf(t *testing.T, dir string) bool {
	t.Helper()
	return leafVerifiesAgainstEtcdBundle(t, dir, dir, "kubelet")
}

// Answers "would THAT node's etcd accept THIS node's certificate".
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

// For EVERY node pair, one's served certificate verifies against the other's anchors.
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

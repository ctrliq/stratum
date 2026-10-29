package ha

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// Run with STRATUM_TEST_ETCD_BIN pointing to the shipped etcd binary. Host
// assembly is recorded, but membership, TLS, quorum loss and recovery use two
// real etcd processes. Disabling strict reconfiguration deliberately creates
// the dangerous committed second-voter add, even on servers that refuse it
// by default; recovery must handle an already-committed membership.
func TestLiveJoinResumeRestoresTwoVoterQuorum(t *testing.T) {
	bin := os.Getenv("STRATUM_TEST_ETCD_BIN")
	if bin == "" {
		t.Skip("set STRATUM_TEST_ETCD_BIN to run the live etcd recovery gate")
	}
	joinFixture(t)
	oldPKI, oldData, oldKube := version.PKIDir, version.DataDir, version.KubeconfigDir
	version.DataDir = t.TempDir()
	version.PKIDir, version.KubeconfigDir = filepath.Join(version.DataDir, "pki"), filepath.Join(version.DataDir, "kubeconfig")
	t.Cleanup(func() { version.PKIDir, version.DataDir, version.KubeconfigDir = oldPKI, oldData, oldKube })
	if err := os.MkdirAll(version.KubeconfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := pki.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	const seedIP, joinIP = "127.77.0.1", "127.77.0.2"
	var pin string
	var etcdCA *pki.CA
	for _, name := range []string{"ca", "front-proxy-ca", pki.EtcdCAName} {
		ca, err := pki.NewCA(name, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "ca" {
			pin = clusterinfo.PinFor(ca.Cert)
		}
		if name == pki.EtcdCAName {
			etcdCA = ca
		}
	}
	if err := pki.EnsureSAKeypair(); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []pki.CertSpec{
		{Name: "live-etcd-server", CN: "live-etcd-server", IPs: []string{seedIP, joinIP, "127.0.0.1"}, ServerAuth: true, ClientAuth: true},
		{Name: "apiserver-etcd-client", CN: "apiserver-etcd-client", ClientAuth: true},
	} {
		if err := etcdCA.Issue(spec); err != nil {
			t.Fatal(err)
		}
	}
	if err := pki.WriteCABundle(pki.EtcdBundleBase, pki.EtcdCAName); err != nil {
		t.Fatal(err)
	}
	seedPeer := fmt.Sprintf("https://%s:%d", seedIP, version.EtcdPeerPort)
	joinPeer := fmt.Sprintf("https://%s:%d", joinIP, version.EtcdPeerPort)
	startLiveJoinEtcd(t, bin, "seed", seedIP, "seed="+seedPeer, "new")
	cli, err := etcdClient(seedIP)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	waitLiveJoinQuorum(t, func(ctx context.Context) error { _, err := cli.MemberList(ctx); return err })

	hostname, err := host.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	st := nodestate.NodeState{Role: "controller", Hostname: hostname, EtcdName: hostname,
		NodeIP: joinIP, KubeletNodeIP: joinIP, EtcdState: "existing",
		Server: "https://" + seedIP + ":6443", PodCIDR: netdefault.PodCIDR, ServiceCIDR: netdefault.ServiceCIDR,
		ClusterDomain: "fuzz.internal"}
	j := joinIntent{MemberName: hostname, PeerURL: joinPeer, Phase: intentPhasePrepared,
		Recovery: &joinRecovery{State: st, EtcdIPs: []string{seedIP}, CAHash: pin}}
	if err := persistJoinPreparation(); err != nil {
		t.Fatal(err)
	}
	if err := writeJoinIntent(j); err != nil {
		t.Fatal(err)
	}
	// A voter, added directly: the production join adds a learner, which
	// leaves the seed its quorum, so it cannot build the quorumless two-voter
	// cluster an older join left behind.
	addCtx, addCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = cli.MemberAdd(addCtx, []string{joinPeer})
	addCancel()
	if err != nil {
		t.Fatal(err)
	}
	// The add committed, but the joiner never started and its reply/state
	// could have been lost. Only one of two voters is running now.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, err = cli.MemberList(ctx)
	cancel()
	if err == nil {
		t.Fatal("one of two voters unexpectedly served a quorum-backed membership read")
	}
	if err := joinQuorumHeld([]string{seedIP}); err == nil {
		t.Fatal("production cleanup gate authorised removal without quorum")
	}

	r := &recordedJoin{detectIP: joinIP}
	fx := fakeJoinEffects(r)
	fx.verifyResumeTrust, fx.persistPreparation, fx.clusterMembers, fx.saveState = verifyJoinResumeTrust, persistJoinPreparation, joinMembership, saveJoinState
	fx.discover = func(string, string, string) ([]byte, error) {
		t.Error("quorumless recovery attempted API discovery")
		return nil, errors.New("API unavailable")
	}
	fx.downloadGrant = func(string, string, func(bundleProfile) error) (bundleGrant, error) {
		t.Error("quorumless recovery attempted Secret download")
		return bundleGrant{}, errors.New("API unavailable")
	}
	fx.probeEtcd = func([]string) ([]string, error) {
		t.Error("resume required a linearizable pre-add probe")
		return nil, errors.New("no quorum")
	}
	fx.memberAdd = func([]string, string, string) (string, error) {
		t.Error("resume attempted a second add")
		return "", errors.New("duplicate add")
	}
	o := freshJoinOpts()
	o.Server, o.CAHash, o.VIP = st.Server, pin, nodestate.VIP{}
	resumed, err := prepareControlPlaneJoin(context.Background(), o, t.TempDir(), fx)
	if err != nil {
		t.Fatalf("production membership recovery refused committed member: %v", err)
	}
	// The journal outlives the resumed state save: it ends when a later start
	// confirms this node's local etcd data (runUp behind requireLocalEtcdMember),
	// not at the save — the same rule the unit resume tests pin.
	if _, err := os.Stat(joinIntentPath); err != nil {
		t.Fatalf("completed recovery lost the journal: %v", err)
	}
	startLiveJoinEtcd(t, bin, resumed.EtcdName, joinIP, resumed.EtcdInitialCluster, "existing")
	waitLiveJoinQuorum(t, func(ctx context.Context) error {
		members, err := cli.MemberList(ctx)
		if err != nil {
			return err
		}
		if len(members.Members) != 2 {
			return fmt.Errorf("membership has %d members, want 2", len(members.Members))
		}
		_, err = cli.Put(ctx, "join-recovery-gate", "quorum-restored")
		return err
	})
	// With two live voters, a failed third join still has a majority.
	// Exercise real compensation as well as the quorumless refusal above.
	thirdPeer := fmt.Sprintf("https://127.77.0.3:%d", version.EtcdPeerPort)
	third := joinIntent{MemberName: "never-started-third", PeerURL: thirdPeer, Phase: intentPhasePrepared}
	if err := writeJoinIntent(third); err != nil {
		t.Fatal(err)
	}
	if _, err := MemberAdd([]string{seedIP, joinIP}, third.MemberName, thirdPeer); err != nil {
		t.Fatal(err)
	}
	cleanup := productionJoinEffects()
	cleanup.confirmQuorumHeld = func() error { return joinQuorumHeld([]string{seedIP, joinIP}) }
	cause := errors.New("third join state save failed")
	if err := joinPostAddFailure(cleanup, []string{seedIP, joinIP}, &third, cause); !errors.Is(err, cause) || !strings.Contains(err.Error(), "removed again by ID") {
		t.Fatalf("real quorum-backed compensation did not remove the failed member: %v", err)
	}
	if _, err := os.Stat(joinIntentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful compensation retained journal: %v", err)
	}
	waitLiveJoinQuorum(t, func(ctx context.Context) error {
		members, err := cli.MemberList(ctx)
		if err != nil {
			return err
		}
		if len(members.Members) != 2 {
			return fmt.Errorf("compensation left %d members", len(members.Members))
		}
		_, err = cli.Put(ctx, "join-compensation-gate", "quorum-retained")
		return err
	})
}

func startLiveJoinEtcd(t *testing.T, bin, name, ip, initial, state string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "etcd.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	clientURL := etcdEndpoint(ip)
	peerURL := fmt.Sprintf("https://%s:%d", ip, version.EtcdPeerPort)
	cert, key, ca := filepath.Join(version.PKIDir, "live-etcd-server.crt"), filepath.Join(version.PKIDir, "live-etcd-server.key"), filepath.Join(version.PKIDir, pki.EtcdCAName+".crt")
	cmd := exec.Command(bin, "--name="+name, "--data-dir="+filepath.Join(dir, "data"),
		"--listen-client-urls="+clientURL, "--advertise-client-urls="+clientURL,
		"--listen-peer-urls="+peerURL, "--initial-advertise-peer-urls="+peerURL,
		"--initial-cluster="+initial, "--initial-cluster-state="+state,
		"--strict-reconfig-check=false", "--cert-file="+cert, "--key-file="+key,
		"--trusted-ca-file="+ca, "--client-cert-auth=true", "--peer-cert-file="+cert,
		"--peer-key-file="+key, "--peer-trusted-ca-file="+ca, "--peer-client-cert-auth=true")
	cmd.Stdout, cmd.Stderr = log, log
	// Peer discovery uses HTTP and must stay on these loopback addresses
	// even when the test runner has a corporate proxy configured.
	cmd.Env = append(os.Environ(), "NO_PROXY=127.77.0.1,127.77.0.2,127.0.0.1", "no_proxy=127.77.0.1,127.77.0.2,127.0.0.1")
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		log.Close()
		if t.Failed() {
			b, _ := os.ReadFile(logPath)
			if len(b) > 5000 {
				b = b[len(b)-5000:]
			}
			t.Logf("etcd %s log:\n%s", name, strings.TrimSpace(string(b)))
		}
	})
}

func waitLiveJoinQuorum(t *testing.T, read func(context.Context) error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		last = read(ctx)
		cancel()
		if last == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("live etcd quorum did not become usable: %v", last)
}

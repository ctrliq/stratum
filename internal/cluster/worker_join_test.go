package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/version"
)

// TestWorkerJoinSavesTheStateItsFlagsDescribe pins the join half of
// PrepareWorker (worker.go): with the host and bootstrap calls behind the
// seams this package names, every flag the operator passed must land in the state
// field of its own name, the discovered CA must reach both trust files, the
// bootstrap kubeconfig must be written for the given server and token, and
// the firewall must trust the pod CIDR the flags describe. The existing
// runUp-level tests record only what runUp hands PrepareWorker; none of them
// reaches the state this code saves, which is what the move out of
// pkg/cli had to carry unchanged.

// joinHostStubs records what PrepareWorker asked the host and the bootstrap
// package for while answering from fixed values: the arguments DiscoverJoin
// and WriteBootstrapKubeconfig were given, and whether the node's addresses
// were detected.
type joinHostStubs struct {
	discover        []string // server, token, caHash, as given to DiscoverJoin
	bootstrapServer string
	bootstrapToken  string
	bootstrapCA     []byte
	nodeIPsCalls    int
	// kubeletCA is what the cluster publishes for its kubelets' client trust;
	// nil answers as a cluster that predates the split and publishes none.
	kubeletCA      []byte
	split          bool
	kubeletFetchCA []byte // the trust FetchKubeletClientCA was asked to verify with
}

// stubJoinHost points the host, CA-discovery and bootstrap-kubeconfig seams
// at fixed answers, and the host-preparation seam at a no-op: the join path
// under test, not the host it would prepare.
func stubJoinHost(t *testing.T, hostname, nodeIPs string, ca []byte) *joinHostStubs {
	t.Helper()
	rec := &joinHostStubs{}
	oldHostname, oldNodeIPs, oldDirs := hostHostname, hostDetectNodeIPs, hostDirs
	oldDiscover, oldWrite, oldFetchKubelet := discoverJoin, writeBootstrapKubeconfig, fetchKubeletClientCA
	oldPrereqs := hostPrereqs
	hostHostname = func() (string, error) { return hostname, nil }
	hostDetectNodeIPs = func(bool) (string, error) {
		rec.nodeIPsCalls++
		return nodeIPs, nil
	}
	hostDirs = func() error { return nil }
	hostPrereqs = func(host.PrereqOptions) error { return nil }
	discoverJoin = func(server, token, caHash string) ([]byte, bool, error) {
		rec.discover = []string{server, token, caHash}
		return ca, rec.split, nil
	}
	writeBootstrapKubeconfig = func(server, token string, ca []byte) error {
		rec.bootstrapServer, rec.bootstrapToken, rec.bootstrapCA = server, token, ca
		return nil
	}
	fetchKubeletClientCA = func(_ string, roots []byte) ([]byte, bool, error) {
		rec.kubeletFetchCA = roots
		return rec.kubeletCA, rec.kubeletCA != nil, nil
	}
	t.Cleanup(func() {
		hostHostname, hostDetectNodeIPs, hostDirs = oldHostname, oldNodeIPs, oldDirs
		discoverJoin, writeBootstrapKubeconfig, fetchKubeletClientCA = oldDiscover, oldWrite, oldFetchKubelet
		hostPrereqs = oldPrereqs
	})
	return rec
}

// redirectJoinDirs points the node's config and PKI directories at fresh
// temp dirs, so the state and trust files this test reads are the ones
// PrepareWorker wrote and nothing else's.
func redirectJoinDirs(t *testing.T) (confDir, pkiDir string) {
	t.Helper()
	workerScratchHost(t)
	if err := os.MkdirAll(version.K8sConfDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return version.K8sConfDir, version.PKIDir
}

func mustLoadState(t *testing.T) *nodestate.NodeState {
	t.Helper()
	st, err := nodestate.LoadState()
	if err != nil {
		t.Fatalf("load the saved node state: %v", err)
	}
	return st
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func TestWorkerJoinSavesTheStateItsFlagsDescribe(t *testing.T) {
	registerCISForTest(t)
	const (
		server = "https://cp-7.example.test:6443"
		tok    = "join-token-7"
		caHash = "sha256:77"
	)
	ca := []byte("-----BEGIN CERTIFICATE-----\nthe discovered CA\n-----END CERTIFICATE-----\n")
	const detected = "10.70.0.9,fd70::9" // a dual-stack pair, as detection hands it back

	t.Run("every flag in its own state field", func(t *testing.T) {
		_, pki := redirectJoinDirs(t)
		rec := stubJoinHost(t, "w-7", detected, ca)
		trusted := recordTrust(t)

		ret, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash, ServiceCIDR: "10.71.0.0/16", PodCIDR: "10.72.0.0/16", ClusterDomain: "join.example.test", Profile: "cis", DualStack: true})
		if err != nil {
			t.Fatalf("PrepareWorker = %v", err)
		}

		st := mustLoadState(t)
		for field, check := range map[string]struct{ got, want string }{
			"role":           {st.Role, "worker"},
			"hostname":       {st.Hostname, "w-7"},
			"node IP":        {st.NodeIP, host.Primary(detected)},
			"kubelet IPs":    {st.KubeletNodeIP, detected},
			"server":         {st.Server, server},
			"service CIDR":   {st.ServiceCIDR, "10.71.0.0/16"},
			"pod CIDR":       {st.PodCIDR, "10.72.0.0/16"},
			"cluster domain": {st.ClusterDomain, "join.example.test"},
		} {
			if check.got != check.want {
				t.Errorf("saved state's %s = %q, want %q", field, check.got, check.want)
			}
		}
		if !st.CIS {
			t.Error("saved state's cis is false, want the --cis the join was given")
		}
		// The state this start runs with is the join's return value, not the
		// saved file: RunWorker's kubelet.conf render keys on it, so its
		// profile and CIS flag must agree without a reload.
		if ret.ProfileName() != "cis" || !ret.CIS {
			t.Errorf("returned state profile=%q cis=%t; want the join's own state to agree with the profile it joined with",
				ret.ProfileName(), ret.CIS)
		}
		if !st.DualStack {
			t.Error("saved state's dualStack is false, want the --dual-stack the join was given")
		}

		for _, f := range []string{"ca.crt", "ca-bundle.crt", "kubelet-client-ca-bundle.crt"} {
			if got := mustRead(t, filepath.Join(pki, f)); string(got) != string(ca) {
				t.Errorf("%s holds %q, want the discovered CA bytes", f, got)
			}
		}

		wantDiscover := strings.Join([]string{server, tok, caHash}, "|")
		if got := strings.Join(rec.discover, "|"); got != wantDiscover {
			t.Errorf("DiscoverJoin got %s, want the join's server, token and CA pin (%s)", got, wantDiscover)
		}
		if rec.bootstrapServer != server || rec.bootstrapToken != tok || string(rec.bootstrapCA) != string(ca) {
			t.Errorf("bootstrap kubeconfig written for server %q token %q, want %q and %q with the discovered CA",
				rec.bootstrapServer, rec.bootstrapToken, server, tok)
		}

		if len(*trusted) != 1 || (*trusted)[0] != "10.72.0.0/16" {
			t.Errorf("firewall trusted %q, want the join's pod CIDR once", *trusted)
		}
	})

	// A split cluster's worker trusts the published kubelet-client CA for its
	// kubelet's clients and nothing the cluster root signs; it still trusts the
	// root for everything else.
	t.Run("split cluster", func(t *testing.T) {
		_, pki := redirectJoinDirs(t)
		rec := stubJoinHost(t, "w-7", detected, ca)
		rec.kubeletCA = []byte("-----BEGIN CERTIFICATE-----\nthe kubelet-client CA\n-----END CERTIFICATE-----\n")
		rec.split = true
		recordTrust(t)
		if _, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash, DualStack: true}); err != nil {
			t.Fatalf("PrepareWorker = %v", err)
		}
		if got := mustRead(t, filepath.Join(pki, "kubelet-client-ca-bundle.crt")); string(got) != string(rec.kubeletCA) {
			t.Errorf("kubelet-client-ca-bundle.crt holds %q, want the published kubelet-client CA", got)
		}
		for _, f := range []string{"ca.crt", "ca-bundle.crt"} {
			if got := mustRead(t, filepath.Join(pki, f)); string(got) != string(ca) {
				t.Errorf("%s holds %q, want the discovered CA bytes", f, got)
			}
		}
		if string(rec.kubeletFetchCA) != string(ca) {
			t.Errorf("the kubelet-client CA was fetched over TLS verified by %q, want the discovered CA", rec.kubeletFetchCA)
		}
	})

	t.Run("split cluster with missing published bundle", func(t *testing.T) {
		_, pki := redirectJoinDirs(t)
		rec := stubJoinHost(t, "w-7", detected, ca)
		rec.split = true
		recordTrust(t)
		if _, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash, DualStack: true}); err == nil {
			t.Fatal("split join accepted a missing or forbidden kubelet client-CA ConfigMap")
		}
		if _, err := os.Stat(filepath.Join(pki, "kubelet-client-ca-bundle.crt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("split join wrote kubelet client trust from the root: %v", err)
		}
		if _, err := nodestate.LoadState(); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed split join published worker state: %v", err)
		}
	})

	t.Run("legacy cluster with unexpected published bundle", func(t *testing.T) {
		_, pki := redirectJoinDirs(t)
		rec := stubJoinHost(t, "w-7", detected, ca)
		rec.kubeletCA = []byte("unexpected separate CA")
		recordTrust(t)
		if _, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash, DualStack: true}); err == nil {
			t.Fatal("join accepted a published bundle that contradicts signed legacy mode")
		}
		if _, err := os.Stat(filepath.Join(pki, "kubelet-client-ca-bundle.crt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inconsistent join wrote kubelet client trust: %v", err)
		}
	})

	// A profile and dual-stack changing places when the join assembly moves
	// is caught by pinning each independently.
	for _, tc := range []struct {
		name    string
		profile string
		dual    bool
	}{
		{"a profile without dual-stack", "cis", false},
		{"dual-stack without a profile", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registerCISForTest(t)
			redirectJoinDirs(t)
			nodeIPs := "10.70.0.9"
			if tc.dual {
				nodeIPs = detected
			}
			stubJoinHost(t, "w-7", nodeIPs, ca)
			recordTrust(t)

			ret, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash, Profile: tc.profile, DualStack: tc.dual})
			if err != nil {
				t.Fatalf("PrepareWorker = %v", err)
			}
			st := mustLoadState(t)
			if st.ProfileName() != tc.profile || st.CIS != (tc.profile == "cis") || st.DualStack != tc.dual {
				t.Errorf("saved profile=%q cis=%t, dualStack=%t; want profile=%q, dualStack=%t",
					st.ProfileName(), st.CIS, st.DualStack, tc.profile, tc.dual)
			}
			if ret.ProfileName() != tc.profile || ret.CIS != (tc.profile == "cis") || ret.DualStack != tc.dual {
				t.Errorf("returned profile=%q cis=%t, dualStack=%t; want the returned state to agree with the saved one",
					ret.ProfileName(), ret.CIS, ret.DualStack)
			}
		})
	}

	t.Run("a profile this binary does not register is refused before any write", func(t *testing.T) {
		registerCISForTest(t)
		redirectJoinDirs(t)
		stubJoinHost(t, "w-8", "10.70.0.10", ca)
		recordTrust(t)
		_, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash, Profile: "stig"})
		if !errors.Is(err, profile.ErrUnregistered) {
			t.Fatalf("PrepareWorker with --profile stig = %v, want a refusal", err)
		}
		if _, err := os.Stat(nodestate.StatePath()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused join wrote node state: %v", err)
		}
	})

	// A worker learns nothing of the cluster's pod range at its join, so one
	// joined without --pod-cidr records none: a default saved here would read
	// later as a range the user gave, and refuse the cluster's real one.
	t.Run("no --pod-cidr records no pod range", func(t *testing.T) {
		redirectJoinDirs(t)
		stubJoinHost(t, "w-7", "10.70.0.9", ca)
		trusted := recordTrust(t)

		ret, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash, ServiceCIDR: "10.71.0.0/16"})
		if err != nil {
			t.Fatalf("PrepareWorker = %v", err)
		}

		if st := mustLoadState(t); st.PodCIDR != "" {
			t.Errorf("saved pod CIDR = %q, want none: the join was given no --pod-cidr", st.PodCIDR)
		}
		if ret.PodCIDR != "" {
			t.Errorf("returned pod CIDR = %q, want none, as saved", ret.PodCIDR)
		}
		// The interface-scoped firewall binding still converges, handed no
		// invented range.
		if len(*trusted) != 1 || (*trusted)[0] != "" {
			t.Errorf("firewall trusted %q, want one call with no range", *trusted)
		}
	})

	t.Run("--node-ip is used instead of detecting addresses", func(t *testing.T) {
		redirectJoinDirs(t)
		rec := stubJoinHost(t, "w-7", detected, ca)
		recordTrust(t)

		if _, err := PrepareWorker(context.Background(), WorkerOptions{Server: server, Token: tok, CAHash: caHash, NodeIP: "10.73.0.9"}); err != nil {
			t.Fatalf("PrepareWorker = %v", err)
		}

		if rec.nodeIPsCalls != 0 {
			t.Errorf("address detection ran %d time(s), want none: --node-ip was given", rec.nodeIPsCalls)
		}
		st := mustLoadState(t)
		if st.NodeIP != "10.73.0.9" || st.KubeletNodeIP != "10.73.0.9" {
			t.Errorf("saved node IP %q / kubelet IPs %q, want the --node-ip 10.73.0.9 in both",
				st.NodeIP, st.KubeletNodeIP)
		}
	})
}

// A worker joined without --pod-cidr takes the cluster's range from the first
// restart given one, and from then on refuses a different one: the range the
// user gave wins, and nothing the join assumed stands in its way.
func TestWorkerRestartRecordsThePodCIDRItsJoinWasNotGiven(t *testing.T) {
	redirectJoinDirs(t)
	stubJoinHost(t, "w-7", "10.70.0.9", []byte("discovered CA"))
	trusted := recordTrust(t)

	if _, err := PrepareWorker(context.Background(), WorkerOptions{Server: "https://cp:6443", Token: "token", CAHash: "pin"}); err != nil {
		t.Fatalf("join without --pod-cidr: PrepareWorker = %v", err)
	}

	st, err := PrepareWorker(context.Background(), WorkerOptions{PodCIDR: "10.42.0.0/16"})
	if err != nil {
		t.Fatalf("restart with --pod-cidr 10.42.0.0/16 after a join given none: PrepareWorker = %v, want the range recorded", err)
	}
	if st.PodCIDR != "10.42.0.0/16" {
		t.Errorf("returned pod CIDR = %q, want the restart's 10.42.0.0/16", st.PodCIDR)
	}
	if saved := mustLoadState(t); saved.PodCIDR != "10.42.0.0/16" {
		t.Errorf("saved pod CIDR = %q, want the restart's 10.42.0.0/16", saved.PodCIDR)
	}
	if n := len(*trusted); n != 2 || (*trusted)[1] != "10.42.0.0/16" {
		t.Errorf("firewall trusted %q, want the restart to trust 10.42.0.0/16 after the join", *trusted)
	}

	_, err = PrepareWorker(context.Background(), WorkerOptions{PodCIDR: "10.99.0.0/16"})
	if err == nil || !strings.Contains(err.Error(), "conflicts with this worker's saved pod CIDR") {
		t.Fatalf("restart with --pod-cidr 10.99.0.0/16 over the recorded 10.42.0.0/16: PrepareWorker = %v, want a conflict refusal", err)
	}
	if saved := mustLoadState(t); saved.PodCIDR != "10.42.0.0/16" {
		t.Errorf("saved pod CIDR after the refused restart = %q, want 10.42.0.0/16 kept", saved.PodCIDR)
	}
}

// Earlier releases saved the default pod range for a worker joined without
// --pod-cidr, so a worker they joined holds 10.245.0.0/16 it was never given.
// A restart given the cluster's real range replaces that default rather than
// refusing it, and from then on refuses a different range as for any other.
func TestWorkerRestartReplacesTheDefaultPodCIDRAnEarlierJoinSaved(t *testing.T) {
	trusted := recordTrust(t)
	workerState(t, &nodestate.NodeState{Role: "worker", Hostname: "w1", PodCIDR: "10.245.0.0/16"})

	st, err := PrepareWorker(context.Background(), WorkerOptions{PodCIDR: "10.42.0.0/16"})
	if err != nil {
		t.Fatalf("restart with --pod-cidr 10.42.0.0/16 over the saved default: PrepareWorker = %v, want the range recorded", err)
	}
	if st.PodCIDR != "10.42.0.0/16" {
		t.Errorf("returned pod CIDR = %q, want the restart's 10.42.0.0/16", st.PodCIDR)
	}
	if saved := mustLoadState(t); saved.PodCIDR != "10.42.0.0/16" {
		t.Errorf("saved pod CIDR = %q, want the restart's 10.42.0.0/16", saved.PodCIDR)
	}
	if len(*trusted) != 1 || (*trusted)[0] != "10.42.0.0/16" {
		t.Errorf("firewall trusted %q, want 10.42.0.0/16 once", *trusted)
	}

	_, err = PrepareWorker(context.Background(), WorkerOptions{PodCIDR: "10.99.0.0/16"})
	if err == nil || !strings.Contains(err.Error(), "conflicts with this worker's saved pod CIDR") {
		t.Fatalf("restart with --pod-cidr 10.99.0.0/16 over the recorded 10.42.0.0/16: PrepareWorker = %v, want a conflict refusal", err)
	}
	if saved := mustLoadState(t); saved.PodCIDR != "10.42.0.0/16" {
		t.Errorf("saved pod CIDR after the refused restart = %q, want 10.42.0.0/16 kept", saved.PodCIDR)
	}
}

// The service range and cluster domain a worker joins with are what its
// kubelet hands pods: RunWorker renders kubelet.conf from the state the join
// returns, so the flags must reach clusterDNS and clusterDomain, not the
// defaults.
func TestWorkerJoinFlagsReachTheKubeletConf(t *testing.T) {
	redirectJoinDirs(t)
	stubJoinHost(t, "w-7", "10.70.0.9", []byte("discovered CA"))
	recordTrust(t)

	st, err := PrepareWorker(context.Background(), WorkerOptions{Server: "https://cp:6443", Token: "token", CAHash: "pin",
		ServiceCIDR: "10.71.0.0/16", ClusterDomain: "join.example.test"})
	if err != nil {
		t.Fatalf("PrepareWorker = %v", err)
	}
	if err := runtime.WriteKubeletConfig(st); err != nil {
		t.Fatalf("WriteKubeletConfig = %v", err)
	}

	b := mustRead(t, runtime.KubeletConfigPath())
	var conf struct {
		ClusterDNS    []string `json:"clusterDNS"`
		ClusterDomain string   `json:"clusterDomain"`
	}
	if err := yaml.Unmarshal(b, &conf); err != nil {
		t.Fatalf("kubelet.conf does not parse: %v\n%s", err, b)
	}
	if len(conf.ClusterDNS) != 1 || conf.ClusterDNS[0] != "10.71.0.10" {
		t.Errorf("kubelet.conf clusterDNS = %q, want [10.71.0.10] from --service-cidr 10.71.0.0/16", conf.ClusterDNS)
	}
	if conf.ClusterDomain != "join.example.test" {
		t.Errorf("kubelet.conf clusterDomain = %q, want the --cluster-domain join.example.test", conf.ClusterDomain)
	}
}

func TestWorkerJoinStopsBeforeTheNextPreparationStep(t *testing.T) {
	for _, phase := range []string{"directories", "prerequisites", "firewall", "discovery", "bootstrap"} {
		t.Run(phase, func(t *testing.T) {
			_, pkiDir := redirectJoinDirs(t)
			stubJoinHost(t, "w1", "192.0.2.9", []byte("discovered CA"))
			recordTrust(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var steps []string
			step := func(name string) {
				steps = append(steps, name)
				if name == phase {
					cancel()
				}
			}
			hostDirs = func() error { step("directories"); return nil }
			hostPrereqs = func(host.PrereqOptions) error { step("prerequisites"); return nil }
			hostTrustPodCIDRs = func(string) { step("firewall") }
			discoverJoin = func(string, string, string) ([]byte, bool, error) {
				step("discovery")
				return []byte("discovered CA"), false, nil
			}
			writeBootstrapKubeconfig = func(string, string, []byte) error { step("bootstrap"); return nil }
			if _, err := PrepareWorker(ctx, WorkerOptions{Server: "https://cp:6443", Token: "token", CAHash: "pin"}); !errors.Is(err, context.Canceled) {
				t.Fatalf("worker join after cancellation = %v", err)
			}
			if len(steps) == 0 || steps[len(steps)-1] != phase {
				t.Fatalf("preparation continued after %s: %v", phase, steps)
			}
			if _, err := nodestate.LoadState(); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cancelled join published node state: %v", err)
			}
			if phase != "bootstrap" {
				if _, err := os.Stat(filepath.Join(pkiDir, "ca.crt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cancelled preparation published a trust file: %v", err)
				}
			}
		})
	}
}

func TestWorkerRestartCancellationKeepsTheRecordedState(t *testing.T) {
	redirectJoinDirs(t)
	stubJoinHost(t, "w1", "192.0.2.9", nil)
	trusted := recordTrust(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hostPrereqs = func(host.PrereqOptions) error { cancel(); return nil }
	if _, err := PrepareWorker(ctx, WorkerOptions{PodCIDR: "10.42.0.0/16"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("worker restart = %v, want cancellation", err)
	}
	if st := mustLoadState(t); st.PodCIDR != "" || len(*trusted) != 0 {
		t.Fatalf("cancelled restart changed state/firewall: state=%+v, trust=%v", st, *trusted)
	}
}

// A join without --node-ip finds its address through host.ResolveNodeIP,
// which takes $NODE_IP before it falls back to detection: an operator who
// exported NODE_IP for a multi-NIC worker must see that address registered,
// as before the join moved out of pkg/cli. Interface detection alone would
// pick whichever address the host offers first.
func TestWorkerJoinTakesTheNodeIPEnvironment(t *testing.T) {
	redirectJoinDirs(t)
	rec := stubJoinHost(t, "w-7", "10.70.0.9", []byte("discovered CA"))
	recordTrust(t)
	t.Setenv("NODE_IP", "10.74.0.9")

	if _, err := PrepareWorker(context.Background(), WorkerOptions{Server: "https://cp:6443", Token: "token", CAHash: "pin"}); err != nil {
		t.Fatalf("PrepareWorker = %v", err)
	}
	st := mustLoadState(t)
	if st.NodeIP != "10.74.0.9" || st.KubeletNodeIP != "10.74.0.9" {
		t.Errorf("saved node IP %q / kubelet IPs %q, want $NODE_IP's 10.74.0.9 in both, not detection's 10.70.0.9",
			st.NodeIP, st.KubeletNodeIP)
	}
	if rec.nodeIPsCalls != 0 {
		t.Errorf("address detection ran %d time(s), want none: $NODE_IP names the address", rec.nodeIPsCalls)
	}
}

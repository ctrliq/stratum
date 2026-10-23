package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/version"
)

// joinCall records one invocation of PrepareWorker through the prepareWorkerFn seam.
// called distinguishes "ran with an empty value" from "never ran", which a
// bare string return cannot.
type joinCall struct {
	called       bool
	server       string
	token        string
	caHash       string
	nodeIP       string
	configNodeIP string
	serviceCIDR  string
	podCIDR      string
	domain       string
	profile      string
	dual         bool
	// selinuxOptOut is the opt-out runUp passed; nil when it found no
	// spec.selinux to pass.
	selinuxOptOut *bool
}

// runUpWorker runs runUp on a scratch host with prepareWorkerFn stubbed, and
// returns the call runUp made (nil fields mean it never got that far).
func runUpWorker(t *testing.T, o runUpOptions, config string, saved *cluster.NodeState) (*joinCall, error) {
	t.Helper()
	tmp := t.TempDir()
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.K8sConfDir, filepath.Join(tmp, "config")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	if saved != nil {
		if err := cluster.SaveState(saved); err != nil {
			t.Fatal(err)
		}
	}
	o.configPath = filepath.Join(tmp, "config.yaml")
	if err := os.WriteFile(o.configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	oldInstall, oldRun, oldJoin := installUnit, runWorkerFn, prepareWorkerFn
	t.Cleanup(func() { installUnit, runWorkerFn, prepareWorkerFn = oldInstall, oldRun, oldJoin })
	installUnit = func([]string) error { return nil }
	runWorkerFn = func(context.Context, *cluster.NodeState) error { return nil }
	call := &joinCall{}
	prepareWorkerFn = func(_ context.Context, w cluster.WorkerOptions) (*cluster.NodeState, error) {
		*call = joinCall{called: true, server: w.Server, token: w.Token, caHash: w.CAHash, nodeIP: w.NodeIP, configNodeIP: w.ConfigNodeIP, serviceCIDR: w.ServiceCIDR, podCIDR: w.PodCIDR, domain: w.ClusterDomain, profile: w.Profile, dual: w.DualStack, selinuxOptOut: w.SELinuxOptOut}
		return &cluster.NodeState{Role: "worker", Hostname: "w1", Server: w.Server}, nil
	}

	o.worker = true
	err := runUp(context.Background(), o)
	return call, err
}

const nodeIPConfig = "spec:\n  network:\n    nodeIP: 10.50.0.4\n"

// The connection that was once missing: a worker join started through runUp
// takes spec.network.nodeIP from the config and hands it to PrepareWorker
// apart from --node-ip, which outranks it there, so the join can say which
// one it took; and a field the worker cannot honour stops the join before
// PrepareWorker runs. want is "<--node-ip>|<spec.network.nodeIP>", or "-"
// when PrepareWorker never ran.
func TestRunUpWorkerJoinReadsTheConfig(t *testing.T) {
	join := runUpOptions{server: "https://10.50.0.3:6443", token: "abcdef.0123456789abcdef", caHash: "sha256:aa"}
	for _, tc := range []struct {
		name, config, flag, want, wantErr string
	}{
		{name: "config nodeIP", config: nodeIPConfig, want: "|10.50.0.4"},
		{name: "flag and config both passed", config: nodeIPConfig, flag: "10.9.9.9", want: "10.9.9.9|10.50.0.4"},
		{name: "no nodeIP leaves $NODE_IP and detection to the join", config: "spec: {}\n", want: "|"},
		{name: "zero-valued field refused", config: nodeIPConfig + "  operator: false\n", want: "-", wantErr: "spec.operator"},
		{name: "refused even with the flag", config: nodeIPConfig + "  snapshots:\n    retain: 0\n", flag: "10.9.9.9", want: "-", wantErr: "spec.snapshots.retain"},
		{name: "misspelled key refused", config: "spec:\n  network:\n    nodeAddr: 10.50.0.4\n", want: "-", wantErr: "nodeAddr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := join
			o.nodeIP = tc.flag
			call, err := runUpWorker(t, o, tc.config, nil)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("runUp = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("runUp = %v, want an error naming %s", err, tc.wantErr)
			}
			got := "-"
			if call.called {
				got = call.nodeIP + "|" + call.configNodeIP
			}
			if got != tc.want {
				t.Errorf("PrepareWorker got --node-ip|spec.network.nodeIP %q, want %q", got, tc.want)
			}
		})
	}
}

// A restart resumes from the saved node state: the config is not read, so a
// file a join would refuse cannot stop a reboot, and its nodeIP is not passed on.
func TestRunUpWorkerRestartDoesNotReadTheConfig(t *testing.T) {
	saved := &cluster.NodeState{Role: "worker", Hostname: "w1", NodeIP: "10.50.0.4", Server: "https://10.50.0.3:6443"}
	call, err := runUpWorker(t, runUpOptions{}, nodeIPConfig+"  operator: false\n", saved)
	if err != nil {
		t.Fatalf("runUp(--worker restart) = %v", err)
	}
	if !call.called {
		t.Fatal("restart never reached PrepareWorker to load the saved state")
	}
	if call.nodeIP != "" || call.configNodeIP != "" {
		t.Errorf("restart handed PrepareWorker node IP %q and config node IP %q, want neither", call.nodeIP, call.configNodeIP)
	}
}

// A restart given --node-ip moves the worker to that address, so runUp must
// hand the flag to PrepareWorker, whose restart path resolves and saves it.
// The flag does not make the restart read the config as a join does: the
// file's nodeIP differs from the flag and from the saved address, and it sets
// a field a worker join refuses, so a restart that passed the config's value,
// the saved one, or nothing, or that validated the file, would fail here.
func TestRunUpWorkerRestartHandsOnTheNodeIPFlag(t *testing.T) {
	saved := &cluster.NodeState{Role: "worker", Hostname: "w1", NodeIP: "10.50.0.5", Server: "https://10.50.0.3:6443"}
	call, err := runUpWorker(t, runUpOptions{nodeIP: "10.50.0.9"}, nodeIPConfig+"  operator: false\n", saved)
	if err != nil {
		t.Fatalf("runUp(--worker restart --node-ip) = %v", err)
	}
	if !call.called {
		t.Fatal("restart never reached PrepareWorker")
	}
	if call.server != "" || call.token != "" {
		t.Errorf("restart handed PrepareWorker server %q and token %q, want both empty", call.server, call.token)
	}
	if call.nodeIP != "10.50.0.9" {
		t.Errorf("restart handed PrepareWorker node IP %q, want the --node-ip value 10.50.0.9", call.nodeIP)
	}
}

// TestRunUpHandsTheWorkerJoinEveryFlagInItsPlace.
//
// runUp hands PrepareWorker ten options, seven of them strings
// (pkg/cli/up.go), and nothing checked more than the node IP: a conversion
// that swapped two strings — the service and pod CIDRs look alike — or
// dropped a boolean would pass the suite. This pin gives every parameter a
// distinct value through the flags an operator actually types, and checks
// each lands in the parameter of its own name.
func TestRunUpHandsTheWorkerJoinEveryFlagInItsPlace(t *testing.T) {
	registerCISForTest(t)
	join := runUpOptions{
		server:        "https://10.60.0.3:6443",
		token:         "join-token-1",
		caHash:        "sha256:0f1e",
		nodeIP:        "10.60.0.9",
		serviceCIDR:   "10.61.0.0/16",
		podCIDR:       "10.62.0.0/16",
		clusterDomain: "join.example.test",
		profile:       "cis",
		dual:          true,
	}
	call, err := runUpWorker(t, join, "spec: {}\n", nil)
	if err != nil {
		t.Fatalf("runUp(--worker join) = %v", err)
	}
	if !call.called {
		t.Fatal("runUp never reached PrepareWorker")
	}
	checks := []struct {
		param, got, want string
	}{
		{"server", call.server, join.server},
		{"token", call.token, join.token},
		{"--discovery-token-ca-cert-hash", call.caHash, join.caHash},
		{"--node-ip", call.nodeIP, join.nodeIP},
		{"--service-cidr", call.serviceCIDR, join.serviceCIDR},
		{"--pod-cidr", call.podCIDR, join.podCIDR},
		{"--cluster-domain", call.domain, join.clusterDomain},
		{"--profile", call.profile, join.profile},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("PrepareWorker's %s parameter = %q, want the flag's %q", c.param, c.got, c.want)
		}
	}
	if !call.dual {
		t.Error("PrepareWorker's dual parameter is false, want --dual-stack")
	}

	// Different values catch a positional swap that a matching pair cannot.
	for _, tc := range []struct {
		name    string
		profile string
		cis     bool
		dual    bool
	}{
		{"a profile without dual-stack", "cis", false, false},
		{"dual-stack without a profile", "", false, true},
		{"the deprecated --cis reads as --profile cis", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := join
			opts.profile, opts.cis, opts.dual = tc.profile, tc.cis, tc.dual
			got, err := runUpWorker(t, opts, "spec: {}\n", nil)
			if err != nil {
				t.Fatalf("runUp(--worker join) = %v", err)
			}
			if !got.called {
				t.Fatal("runUp never reached PrepareWorker")
			}
			want := tc.profile
			if tc.cis {
				want = "cis"
			}
			if got.profile != want || got.dual != tc.dual {
				t.Errorf("workerJoin got profile=%q, dualStack=%t; want profile=%q, dualStack=%t",
					got.profile, got.dual, want, tc.dual)
			}
		})
	}

	// A restart (no --server) hands the join neither a server nor a token:
	// the saved state is the restart's material, not one-shot join
	// credentials that may since have expired.
	saved := &cluster.NodeState{Role: "worker", Hostname: "w1", NodeIP: "10.60.0.4", Server: "https://10.60.0.3:6443"}
	call, err = runUpWorker(t, runUpOptions{}, "spec: {}\n", saved)
	if err != nil {
		t.Fatalf("runUp(--worker restart) = %v", err)
	}
	if call.server != "" || call.token != "" {
		t.Errorf("restart handed PrepareWorker server %q and token %q, want both empty", call.server, call.token)
	}
	if !call.called {
		t.Fatal("restart never reached PrepareWorker to load the saved state")
	}
}

// A worker join takes spec.selinux from the config, and so does a restart,
// which reads nothing else from the file; a file a restart cannot read leaves
// the saved setting in place rather than stopping the start.
func TestRunUpWorkerReadsTheSELinuxOptOut(t *testing.T) {
	o := runUpOptions{server: "https://10.50.0.3:6443", token: "abcdef.0123456789abcdef", caHash: "sha256:aa"}
	call, err := runUpWorker(t, o, "spec:\n  selinux: false\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	if call.selinuxOptOut == nil || !*call.selinuxOptOut {
		t.Error("join did not pass spec.selinux: false to PrepareWorker")
	}
	saved := &cluster.NodeState{Role: "worker", Hostname: "w1", NodeIP: "10.50.0.4", Server: "https://10.50.0.3:6443"}
	for _, tc := range []struct {
		name, config string
		want         *bool
	}{
		{"restart with spec.selinux: false", "spec:\n  selinux: false\n  operator: false\n", ptr(true)},
		{"restart with spec.selinux: true", "spec:\n  selinux: true\n", ptr(false)},
		{"restart without spec.selinux", "spec:\n  network:\n    nodeIP: 10.50.0.4\n", nil},
		{"restart with an unreadable file", "spec: [\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call, err := runUpWorker(t, runUpOptions{}, tc.config, saved)
			if err != nil {
				t.Fatalf("runUp(--worker restart) = %v", err)
			}
			if (call.selinuxOptOut == nil) != (tc.want == nil) || (tc.want != nil && *call.selinuxOptOut != *tc.want) {
				t.Errorf("restart passed %v, want %v", call.selinuxOptOut, tc.want)
			}
		})
	}
}

// The opt-out is known before payload staging, which runs before the config
// is read, from a controller's config, a worker's config, or the setting a
// worker saved.
func TestStageOptOut(t *testing.T) {
	dir := t.TempDir()
	old := version.K8sConfDir
	version.K8sConfDir = dir
	t.Cleanup(func() { version.K8sConfDir = old })
	cfg := filepath.Join(dir, "config.yaml")
	write := func(body string) {
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("spec:\n  selinux: false\n")
	if !stageOptOut(runUpOptions{configPath: cfg}) || !stageOptOut(runUpOptions{configPath: cfg, worker: true}) {
		t.Error("spec.selinux: false not seen before staging")
	}
	write("spec: [\n")
	if stageOptOut(runUpOptions{configPath: cfg}) {
		t.Error("an unreadable controller config read as an opt-out")
	}
	if err := cluster.SaveState(&cluster.NodeState{Role: "worker", Hostname: "w1", SELinuxOptOut: true}); err != nil {
		t.Fatal(err)
	}
	if !stageOptOut(runUpOptions{configPath: cfg, worker: true}) {
		t.Error("a worker's saved opt-out not seen when its config cannot be read")
	}
}

// An opt-out under CIS is refused once the config is validated, so staging
// must not act on it first and unlabel a node whose start then fails.
func TestStageOptOutIgnoresARefusedCISOptOut(t *testing.T) {
	registerCISForTest(t)
	dir := t.TempDir()
	old := version.K8sConfDir
	version.K8sConfDir = dir
	t.Cleanup(func() { version.K8sConfDir = old })
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("spec:\n  cis: true\n  selinux: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stageOptOut(runUpOptions{configPath: cfg}) {
		t.Error("a controller config with spec.cis and spec.selinux: false read as an opt-out")
	}
	if err := os.WriteFile(cfg, []byte("spec:\n  selinux: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stageOptOut(runUpOptions{configPath: cfg, profile: "cis"}) {
		t.Error("spec.selinux: false read as an opt-out under --profile cis")
	}
	if stageOptOut(runUpOptions{configPath: cfg, worker: true, cis: true}) {
		t.Error("a joining CIS worker's spec.selinux: false read as an opt-out")
	}
	if err := cluster.SaveState(&cluster.NodeState{Role: "worker", Hostname: "w1", CIS: true}); err != nil {
		t.Fatal(err)
	}
	if stageOptOut(runUpOptions{configPath: cfg, worker: true}) {
		t.Error("a restarted CIS worker's spec.selinux: false read as an opt-out")
	}
	if err := cluster.SaveState(&cluster.NodeState{Role: "controller", Hostname: "cp1", CIS: true}); err != nil {
		t.Fatal(err)
	}
	if stageOptOut(runUpOptions{configPath: cfg}) {
		t.Error("a restarted CIS controller's profile-less spec.selinux: false read as an opt-out")
	}
}

// A join whose config the join itself refuses is not an opt-out: staging
// acting on spec.selinux: false there would unlabel a node whose join then
// fails.
func TestStageOptOutValidatesAJoinsWholeConfig(t *testing.T) {
	dir := t.TempDir()
	old := version.K8sConfDir
	version.K8sConfDir = dir
	t.Cleanup(func() { version.K8sConfDir = old })
	cfg := filepath.Join(dir, "config.yaml")
	join := runUpOptions{configPath: cfg, worker: true, server: "https://10.0.0.1:6443"}
	if err := os.WriteFile(cfg, []byte("spec:\n  selinux: false\n  network:\n    podCIDR: 10.1.0.0/16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stageOptOut(join) {
		t.Error("a join config with a field the join refuses read as an opt-out")
	}
	if err := os.WriteFile(cfg, []byte("spec:\n  selinux: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !stageOptOut(join) {
		t.Error("a valid join config's spec.selinux: false not seen before staging")
	}
}

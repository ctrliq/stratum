package cluster

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/comps"
	stratumpki "github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

func TestWorkerKubeletBootstrapFlagsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	pki := func(name string) string { return filepath.Join(dir, "pki", name) }
	kc := func(name string) string { return filepath.Join(dir, name) }
	worker := &NodeState{Role: "worker"}
	first := kubeletProcess(worker, pki, kc).Args
	if err := os.WriteFile(kc("kubelet.kubeconfig"), []byte("created by bootstrap"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := kubeletProcess(worker, pki, kc).Args
	if !slices.Equal(first, restarted) {
		t.Fatalf("worker launch changed after bootstrap: before=%v after=%v", first, restarted)
	}
	for _, flag := range []string{"--cert-dir=" + pki("kubelet"), "--bootstrap-kubeconfig=" + version.BootstrapKubeconfig, "--kubeconfig=" + kc("kubelet.kubeconfig")} {
		if !slices.Contains(restarted, flag) {
			t.Errorf("restarted worker missing %s", flag)
		}
	}
	controller := kubeletProcess(&NodeState{Role: "controller"}, pki, kc).Args
	if slices.Contains(controller, "--bootstrap-kubeconfig="+version.BootstrapKubeconfig) {
		t.Fatal("pre-provisioned controller unexpectedly uses bootstrap")
	}
}

// TestKubeletServingCertDirFollowsTheLaunchFlags pins where the heal and
// rotate-ca look for the kubelet's bootstrap-issued serving pair. A controller
// launches with no --cert-dir, so its kubelet keeps the pair in the kubelet's
// fixed default, which --root-dir does not move.
func TestKubeletServingCertDirFollowsTheLaunchFlags(t *testing.T) {
	dir := t.TempDir()
	pki := func(name string) string { return filepath.Join(dir, "pki", name) }
	kc := func(name string) string { return filepath.Join(dir, name) }
	if err := os.WriteFile(kc("kubelet.kubeconfig"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := kubeletProcess(&NodeState{Role: "worker"}, pki, kc).Args
	if got := stratumpki.KubeletCertDir(worker); got != pki("kubelet") {
		t.Errorf("worker cert dir = %q, want %q", got, pki("kubelet"))
	}
	controller := kubeletProcess(&NodeState{Role: "controller"}, pki, kc).Args
	if got := stratumpki.KubeletCertDir(controller); got != "/var/lib/kubelet/pki" {
		t.Errorf("controller cert dir = %q, want the kubelet default /var/lib/kubelet/pki (args %v)", got, controller)
	}
}

// The bootstrapsigner and etcd client-CA regression tests live in
// internal/staticpod with the argv renderers, since the control plane is
// static pods. What is left to pin here is the supervisor roster itself:
// kubelet+containerd on every node, the operator only on controllers with the
// operator switch on, and no control-plane component supervised as a host
// process.

func rosterNames(st *NodeState, operator bool) []string {
	var names []string
	for _, p := range Processes(st, operator) {
		names = append(names, p.Name)
	}
	return names
}

func TestRosterIsHostProcessesOnly(t *testing.T) {
	controller := rosterNames(&NodeState{Role: "controller"}, true)
	if !slices.Equal(controller, []string{"containerd", "kubelet", "operator"}) {
		t.Errorf("controller roster = %v, want [containerd kubelet operator]", controller)
	}
	worker := rosterNames(&NodeState{Role: "worker"}, true)
	if !slices.Equal(worker, []string{"containerd", "kubelet"}) {
		t.Errorf("worker roster = %v, want [containerd kubelet]", worker)
	}
	for _, cp := range []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-vip"} {
		if slices.Contains(controller, cp) {
			t.Errorf("%s is supervised as a host process — the control plane is static pods", cp)
		}
	}
}

// TestSupervisorRosterIsTheUnits ties the roster to comps.Units, the canonical
// component list uninstall and the upgrade derive from. TestRosterIsHostProcessesOnly
// names the members in a hand-written literal, so a component added here but not
// there — or the reverse — passes it while the rest of the tree never hears of
// the change. The relation is the pin; a new component must land in both lists
// at once to keep it green. The operator switch decides the operator on a
// controller and nothing on a worker, which never runs it whatever the switch.
func TestSupervisorRosterIsTheUnits(t *testing.T) {
	withoutOperator := slices.DeleteFunc(slices.Clone(comps.Units), func(name string) bool { return name == "operator" })

	if got := rosterNames(&NodeState{Role: "controller"}, true); !slices.Equal(got, comps.Units) {
		t.Errorf("controller roster = %v, want the canonical units %v in order", got, comps.Units)
	}
	for _, tc := range []struct {
		name     string
		st       *NodeState
		operator bool
	}{
		{"a worker with the operator on", &NodeState{Role: "worker"}, true},
		{"a worker with the operator off", &NodeState{Role: "worker"}, false},
		{"a controller with the operator off", &NodeState{Role: "controller"}, false},
	} {
		if got := rosterNames(tc.st, tc.operator); !slices.Equal(got, withoutOperator) {
			t.Errorf("%s: roster = %v, want the units without the operator %v in order", tc.name, got, withoutOperator)
		}
	}
}

// TestContainerdConfigArg pins the containerd process to stratum's own config
// path. Selecting that path (away from the host's /etc/containerd/config.toml)
// is the core behavior of the stratum-owned containerd config; without this
// assertion the roster test above would stay green if the --config arg
// regressed to the host path.
func TestContainerdConfigArg(t *testing.T) {
	for _, role := range []string{"controller", "worker"} {
		var containerd []string
		for _, p := range Processes(&NodeState{Role: role}, true) {
			if p.Name == "containerd" {
				containerd = p.Args
			}
		}
		if containerd == nil {
			t.Fatalf("%s: no containerd process in the roster", role)
		}
		i := slices.Index(containerd, "--config")
		if i < 0 || i+1 >= len(containerd) {
			t.Fatalf("%s: containerd has no --config arg: %v", role, containerd)
		}
		if got := containerd[i+1]; got != version.ContainerdConfig {
			t.Errorf("%s: containerd --config = %q, want the stratum-owned %q (must not regress to the host path)", role, got, version.ContainerdConfig)
		}
	}
}

// namedProcess finds one process in a rendered roster, failing the test when
// the name is missing: a pin that silently skipped a missing process would
// pass on the day the roster loses it.
func namedProcess(t *testing.T, procs []supervisor.Process, name string) supervisor.Process {
	t.Helper()
	for _, p := range procs {
		if p.Name == name {
			return p
		}
	}
	var names []string
	for _, p := range procs {
		names = append(names, p.Name)
	}
	t.Fatalf("no %s process in the roster %v", name, names)
	return supervisor.Process{}
}

// hostArgv is the argv the supervisor starts a process with: ArgsFn's fresh
// render when the component has one, Args otherwise — the same choice
// writeComponentArgs records.
func hostArgv(p supervisor.Process) []string {
	if p.ArgsFn != nil {
		return p.ArgsFn()
	}
	return p.Args
}

// TestHostProcessArgvIgnoresTheOperatorSwitch: the roster reads no cluster
// config, only the node state and the operator switch, and the upgrade's
// reload and the worker start render the host processes with the switch off
// while a controller start passes the config's own. That split rests on the
// containerd and kubelet argv not depending on the switch. The assumption is
// load-bearing: an upgraded node must run exactly the argv its saved node
// state describes, so a switch that starts shaping either argv changes what
// an upgraded node runs while every test that renders an argv with one switch
// stays green. Building both rosters for the same node state — one with the
// operator off, one with it on — makes the disagreement itself the failure.
func TestHostProcessArgvIgnoresTheOperatorSwitch(t *testing.T) {
	for _, st := range []*NodeState{
		{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11", VIP: VIP{Address: "10.0.0.10"}},
		{Role: "worker", Hostname: "w1", NodeIP: "10.0.0.21", KubeletNodeIP: "10.0.0.21"},
	} {
		for _, name := range []string{"containerd", "kubelet"} {
			off := namedProcess(t, Processes(st, false), name)
			on := namedProcess(t, Processes(st, true), name)
			if off.Path != on.Path {
				t.Errorf("%s %s: path %q with the operator off, %q with it on", st.Role, name, off.Path, on.Path)
			}
			if offArgs, onArgs := hostArgv(off), hostArgv(on); !slices.Equal(offArgs, onArgs) {
				t.Errorf("%s %s: argv with the operator off %q differs from the argv with it on %q", st.Role, name, offArgs, onArgs)
			}
			if !slices.Equal(off.Env, on.Env) {
				t.Errorf("%s %s: environment %q with the operator off, %q with it on", st.Role, name, off.Env, on.Env)
			}
		}
	}
}

// TestOperatorKnowsTheVIP: the deployer points Cilium at the VIP when there is
// one, and it can only know there is one from its argv. No VIP, no
// flag, so a VIP-less roster is unchanged.
func TestOperatorKnowsTheVIP(t *testing.T) {
	operatorArgs := func(st *NodeState) []string {
		for _, p := range Processes(st, true) {
			if p.Name == "operator" {
				return p.Args
			}
		}
		t.Fatal("no operator in the controller roster")
		return nil
	}
	with := operatorArgs(&NodeState{Role: "controller", NodeIP: "10.0.0.11", VIP: VIP{Address: "10.0.0.10"}})
	if i := slices.Index(with, "--apiserver-vip"); i < 0 || i+1 >= len(with) || with[i+1] != "10.0.0.10" {
		t.Errorf("operator argv %q lacks --apiserver-vip 10.0.0.10", with)
	}
	if without := operatorArgs(&NodeState{Role: "controller", NodeIP: "10.0.0.11"}); slices.Contains(without, "--apiserver-vip") {
		t.Errorf("operator argv %q carries --apiserver-vip with no VIP", without)
	}
}

// TestOperatorArgvCarriesTheClusterDomain: the operator names the dashboard's
// and the webhook's Services in their serving certs, and it can only know the
// cluster's domain from its argv. A node state without a domain still passes
// the default, so the operator never falls back on its own flag default.
func TestOperatorArgvCarriesTheClusterDomain(t *testing.T) {
	for _, tc := range []struct {
		domain string
		want   string
	}{
		{"fuzz.internal", "fuzz.internal"},
		{"", "cluster.local"},
	} {
		st := &NodeState{Role: "controller", NodeIP: "10.0.0.11", ClusterDomain: tc.domain}
		args := namedProcess(t, Processes(st, true), "operator").Args
		if i := slices.Index(args, "--cluster-domain"); i < 0 || i+1 >= len(args) || args[i+1] != tc.want {
			t.Errorf("node-state domain %q: operator argv %q lacks --cluster-domain %s", tc.domain, args, tc.want)
		}
	}
}

// TestOperatorArgvMatchesTheRestorePattern: the restore's guidance
// restarts the operator with `pkill -f` and comps.OperatorCommandLinePattern, which
// matches flags in the order assembled here. A reordering would make the printed
// command miss a real operator, so the real argv, with and without a VIP and under
// an artifact name, must match it and be accepted by the supervisor's own test.
// A controller roster that holds no operator fails too: otherwise the match is
// never attempted and the test passes having checked nothing.
func TestOperatorArgvMatchesTheRestorePattern(t *testing.T) {
	re := regexp.MustCompile(comps.OperatorCommandLinePattern())
	for _, tc := range []struct {
		name string
		st   *NodeState
	}{
		{"without a VIP", &NodeState{Role: "controller", NodeIP: "10.0.0.11"}},
		{"with a VIP", &NodeState{Role: "controller", NodeIP: "10.0.0.11", VIP: VIP{Address: "10.0.0.10"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := false
			for _, p := range Processes(tc.st, true) {
				if p.Name != "operator" {
					continue
				}
				found = true
				for _, exe := range []string{"/usr/local/bin/stratum", "/tmp/stratum-linux-amd64"} {
					line := strings.Join(append([]string{exe}, p.Args...), " ")
					if !re.MatchString(line) {
						t.Errorf("the restore's operator pattern does not match %q", line)
					}
				}
			}
			if !found {
				t.Fatalf("no operator in the controller roster %s", tc.name)
			}
		})
	}
}

// TestRunControllerRendersBeforeTheSupervisorStarts: upgrade, rollback and
// rejoin treat an answering supervisor as proof that this node's renders
// are done (ensureSupervisor skips every write once the control socket
// answers), so RunController must write the static-pod manifests, the
// add-on bundles and the .args records before it starts the supervisor. The
// test stands where the supervisor stands — runSupervisor replaced by a
// stub that inspects the disk and hands back the context error — so a start
// sequence that reaches the supervisor before a render finishes fails here,
// not on a node whose kubelet boots half-rendered state.
func TestRunControllerRendersBeforeTheSupervisorStarts(t *testing.T) {
	origConf, origManifests, origPKI, origKubeconfig := version.K8sConfDir, version.ManifestsDir, version.PKIDir, version.KubeconfigDir
	t.Cleanup(func() {
		version.K8sConfDir, version.ManifestsDir, version.PKIDir, version.KubeconfigDir = origConf, origManifests, origPKI, origKubeconfig
	})
	oldGrowth, oldRun, oldUpstream := startEtcdGrowthRecorder, runSupervisor, writeCoreDNSUpstream
	t.Cleanup(func() {
		startEtcdGrowthRecorder, runSupervisor, writeCoreDNSUpstream = oldGrowth, oldRun, oldUpstream
	})
	writeCoreDNSUpstream = func(*NodeState) error { return nil }

	seed := func(vip string) *NodeState {
		return &NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11", KubeletNodeIP: "10.0.0.11",
			EtcdName: "cp1", EtcdState: "new", EtcdInitialCluster: "cp1=https://10.0.0.11:2380",
			PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.43.0.0/16", VIP: VIP{Address: vip}}
	}
	for _, tc := range []struct {
		name string
		st   *NodeState
	}{
		{"with a VIP", seed("10.0.0.10")},
		{"without a VIP", seed("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version.K8sConfDir = t.TempDir()
			version.ManifestsDir = t.TempDir()
			version.PKIDir = t.TempDir()
			version.KubeconfigDir = t.TempDir()

			// The recorder parks like a real loop: a stub that returns would
			// have the run group restart it, and the shutdown join would
			// wait out the restart backoff.
			startEtcdGrowthRecorder = func(ctx context.Context) { <-ctx.Done() }

			st := tc.st
			runSupervisor = func(ctx context.Context, procs []supervisor.Process, opts supervisor.Options) error {
				for _, name := range staticpod.Names {
					_, err := os.Stat(staticpod.Path(name))
					if name == "kube-vip" && st.VIP.Address == "" {
						if !errors.Is(err, fs.ErrNotExist) {
							t.Error("a kube-vip manifest exists without a VIP")
						}
						continue
					}
					if err != nil {
						t.Errorf("static pod %s is not on disk before the supervisor starts: %v", name, err)
					}
				}
				for _, rel := range []string{"cilium/cilium.yaml", "coredns/coredns.yaml"} {
					if _, err := os.Stat(filepath.Join(version.ManifestsDir, rel)); err != nil {
						t.Errorf("%s is not staged before the supervisor starts: %v", rel, err)
					}
				}
				for _, p := range procs {
					if _, err := os.Stat(filepath.Join(version.K8sConfDir, "components", p.Name+".args")); err != nil {
						t.Errorf("no .args record for %s before the supervisor starts: %v", p.Name, err)
					}
				}
				return ctx.Err()
			}

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := RunController(ctx, &ClusterConfig{}, st); !errors.Is(err, context.Canceled) {
				t.Fatalf("RunController returned %v; want the supervisor seam's context error", err)
			}
		})
	}
}

// TestBundlesAreTheSameOnEveryControlPlane pins leader-independent bundles at their source: the
// operator applies the leader's staged copy, so any per-node byte in a bundle
// becomes a re-apply, and for Cilium a rollout, every time the lease moves.
func TestBundlesAreTheSameOnEveryControlPlane(t *testing.T) {
	orig, origCilium := version.ManifestsDir, version.CiliumSettingsPath
	version.CiliumSettingsPath = filepath.Join(t.TempDir(), "cilium.json")
	t.Cleanup(func() { version.ManifestsDir, version.CiliumSettingsPath = orig, origCilium })
	stage := func(nodeIP string) []byte {
		version.ManifestsDir = t.TempDir()
		if err := writeBundles(&ClusterConfig{}, &NodeState{Role: "controller", NodeIP: nodeIP}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(version.ManifestsDir, "cilium", "cilium.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if a, b := stage("10.0.0.11"), stage("10.0.0.12"); !slices.Equal(a, b) {
		t.Error("two control planes staged different Cilium bundles; the operator would re-apply it on every leader change")
	}
}

// stagedStacks lists every file writeBundles staged under dir, keyed by path
// relative to it — the deployer's view of the manifests directory.
func stagedStacks(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == pendingStagingName || d.Name() == stagedFingerprintName {
			return nil // local activation bookkeeping is not a manifest
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sortedStacks is stagedStacks' key set in order, for comparing the staged
// set with the set a config asks for.
func sortedStacks(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for rel := range files {
		names = append(names, rel)
	}
	slices.Sort(names)
	return names
}

// TestWriteBundlesStagesEveryStackTheConfigAsksFor: the deployer prunes
// what a stack stops declaring, so the staged set IS the cluster's add-on
// set — the CRDs, the node-mark policy, Cilium and CoreDNS always,
// local-path only while the config asks for it. Nothing pinned the set, so
// a render that drops a stack deletes its objects on the next pass with
// every test green. CoreDNS must be the render for the node's own service
// range and domain (10.43.0.10 is the tenth address of 10.43.0.0/16), and
// an unchanged file must not be rewritten: the deployer re-applies on
// change, so a start that touches every file rolls the add-ons on every
// reboot.
func TestWriteBundlesStagesEveryStackTheConfigAsksFor(t *testing.T) {
	orig := version.ManifestsDir
	t.Cleanup(func() { version.ManifestsDir = orig })

	st := &NodeState{Role: "controller", Hostname: "cp1", NodeIP: "10.0.0.11",
		ServiceCIDR: "10.43.0.0/16", ClusterDomain: "k8s.example.internal"}
	localPath := &ClusterConfig{Spec: Spec{Storage: Storage{Default: addons.StorageLocalPath}}}
	want := []string{
		"cilium/cilium.yaml",
		"coredns/coredns.yaml",
		"crds/stratum-crds.yaml",
		"dashboard/dashboard.yaml",
		"local-path/local-path.yaml",
		"node-mark/node-mark-policy.yaml",
	}

	version.ManifestsDir = t.TempDir()
	if err := writeBundles(localPath, st); err != nil {
		t.Fatal(err)
	}
	staged := stagedStacks(t, version.ManifestsDir)
	if got := sortedStacks(staged); !slices.Equal(got, want) {
		t.Fatalf("a local-path config staged %v, want exactly %v", got, want)
	}
	if got := staged["coredns/coredns.yaml"]; !bytes.Equal(got, addons.RenderCoreDNS()) {
		t.Error("coredns.yaml is not the staged render, whose per-cluster inputs stay as sentinels")
	}

	// Age every staged file back an hour so a rewrite cannot hide inside the
	// filesystem's timestamp granularity: the deployer re-applies a changed
	// file, so an idempotent second run must leave the aged times alone.
	aged := time.Now().Add(-time.Hour)
	for rel := range staged {
		if err := os.Chtimes(filepath.Join(version.ManifestsDir, rel), aged, aged); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeBundles(localPath, st); err != nil {
		t.Fatal(err)
	}
	if staged = stagedStacks(t, version.ManifestsDir); !slices.Equal(sortedStacks(staged), want) {
		t.Fatalf("the second run staged %v, want the same %v", sortedStacks(staged), want)
	}
	for rel := range staged {
		fi, err := os.Stat(filepath.Join(version.ManifestsDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(aged) {
			t.Errorf("the second run rewrote %s; the deployer watches for change and would re-apply it on every start", rel)
		}
	}

	// An unloaded ClusterConfig has no storage default (LoadConfig fills it
	// in), so a render that consults one stages local-path for a cluster
	// that never asked for it.
	version.ManifestsDir = t.TempDir()
	if err := writeBundles(&ClusterConfig{}, st); err != nil {
		t.Fatal(err)
	}
	staged = stagedStacks(t, version.ManifestsDir)
	want = slices.DeleteFunc(slices.Clone(want), func(rel string) bool { return rel == "local-path/local-path.yaml" })
	if got := sortedStacks(staged); !slices.Equal(got, want) {
		t.Errorf("an empty storage default staged %v, want the same set without local-path: %v", got, want)
	}
}

// decodeStack parses a staged multi-document YAML into unstructured objects —
// the same decode path the operator's apply uses — so a pin can ask which
// objects a stack declares.
func decodeStack(t *testing.T, data []byte) []*unstructured.Unstructured {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for {
		obj := &unstructured.Unstructured{}
		err := dec.Decode(obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode a staged stack: %v", err)
		}
		if len(obj.Object) > 0 {
			out = append(out, obj)
		}
	}
	return out
}

// TestWriteBundlesStagesTheSameStacksOnEveryControlPlane: the operator
// applies the lease holder's staged copy, so two control planes on one
// release staging different bytes or names re-apply and roll the add-ons on
// every leader change. TestBundlesAreTheSameOnEveryControlPlane compares
// Cilium only, for two node IPs; this pin compares the whole staged set for
// node states that differ in hostname, node IP and kubelet node IP, and
// holds the stack names the rest of the tree keys off: the CoreDNS spreader
// restarts the Deployment it finds in the coredns stack, and the operator's
// HelmChart controller needs its CRD in the crds stack. A stack renamed or
// an object moved between them deletes the object on the next prune and
// silently switches the spreading off.
func TestWriteBundlesStagesTheSameStacksOnEveryControlPlane(t *testing.T) {
	orig := version.ManifestsDir
	t.Cleanup(func() { version.ManifestsDir = orig })

	cfg := &ClusterConfig{Spec: Spec{Storage: Storage{Default: addons.StorageLocalPath}}}
	stage := func(hostname, nodeIP, kubeletNodeIP string) map[string][]byte {
		version.ManifestsDir = t.TempDir()
		st := &NodeState{Role: "controller", Hostname: hostname, NodeIP: nodeIP, KubeletNodeIP: kubeletNodeIP,
			ServiceCIDR: "10.43.0.0/16", ClusterDomain: "k8s.example.internal"}
		if err := writeBundles(cfg, st); err != nil {
			t.Fatal(err)
		}
		return stagedStacks(t, version.ManifestsDir)
	}
	first := stage("cp1", "10.0.0.11", "10.0.0.11")
	second := stage("cp2", "10.0.0.12", "10.0.0.12,fd00::12")

	want := []string{
		"cilium/cilium.yaml",
		"coredns/coredns.yaml",
		"crds/stratum-crds.yaml",
		"dashboard/dashboard.yaml",
		"local-path/local-path.yaml",
		"node-mark/node-mark-policy.yaml",
	}
	for name, files := range map[string]map[string][]byte{"cp1": first, "cp2": second} {
		if got := sortedStacks(files); !slices.Equal(got, want) {
			t.Errorf("%s staged %v, want exactly %v", name, got, want)
		}
	}
	for rel, b := range first {
		if other, ok := second[rel]; !ok || !bytes.Equal(b, other) {
			t.Errorf("%s differs between control planes; the operator re-applies it on every leader change", rel)
		}
	}

	var corednsDeployment, helmChartCRD bool
	for _, o := range decodeStack(t, first["coredns/coredns.yaml"]) {
		if o.GetKind() == "Deployment" && o.GetName() == "coredns" && o.GetNamespace() == "kube-system" {
			corednsDeployment = true
		}
	}
	if !corednsDeployment {
		t.Error("the coredns stack does not declare the kube-system/coredns Deployment the spreader restarts")
	}
	for _, o := range decodeStack(t, first["crds/stratum-crds.yaml"]) {
		if kind, _, _ := unstructured.NestedString(o.Object, "spec", "names", "kind"); o.GetKind() == "CustomResourceDefinition" && kind == "HelmChart" {
			helmChartCRD = true
		}
	}
	if !helmChartCRD {
		t.Error("the crds stack does not declare the HelmChart CRD the operator reconciles")
	}
}

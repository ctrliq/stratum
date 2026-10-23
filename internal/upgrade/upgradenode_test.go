package upgrade

import (
	"io"

	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/version"
)

// controllerUpgradeFixture is a controller whose UpgradeNode can run to the
// end: every host-bound step is stubbed, the node's files live in temp dirs,
// and the returned slice records, in order, each health-gate wait and each
// stamp. Health gates are called in a fixed order: 1 the apiserver's
// component gate, 2 the node gate before the config refresh, 3 the node gate
// after it. failGate makes that numbered call fail (0 = none). The payload's
// kubelet reports payloadVer; the installed one reports v1.37.8.
func controllerUpgradeFixture(t *testing.T, payloadVer string, failGate int) *[]string {
	t.Helper()
	resumeFixture(t)
	redirect(t, &version.K8sVersion, "v1.37.9")
	fakeKubelet(t, "v1.37.8")
	writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"),
		"role: controller\nhostname: cp-1.registered\nnodeIP: 10.0.0.1\n", 0o600)
	for _, n := range staticpod.Names {
		writeFile(t, staticpod.Path(n), "name: "+n+"\n", 0o600)
	}

	payload := t.TempDir()
	writeFile(t, filepath.Join(payload, "containerd"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(payload, "kubelet"), "#!/bin/sh\necho Kubernetes "+payloadVer+"-stratum\n", 0o755)

	var events []string
	gate := 0
	swap(t, &resolvePayload, func(io.Writer, string, []string) (Source, error) { return Source{Dir: payload}, nil })
	swap(t, &startStaticPod, func(string) error { return nil })
	swap(t, &waitStaticPodReady, func(string, time.Duration) error { return nil })
	swap(t, &componentPIDs, func(string) ([]int, error) { return nil, nil })
	swap(t, &bootClock, func() (staticpod.Mark, error) { return staticpod.Mark{}, nil })
	swap(t, &refreshCerts, func(io.Writer, *nodestate.NodeState, string) (bool, error) { return false, nil })
	swap(t, &writeKubeVipConfig, func() error { events = append(events, "kube-vip kubeconfig"); return nil })
	swap(t, &writeSchedulerConfig, func() error { return nil })
	swap(t, &soak, func(time.Duration) {})
	swap(t, &rollbackEtcdTarget, func(string) (string, error) { return "", nil })
	swap(t, &waitHealthzCtx, func(context.Context) error {
		gate++
		events = append(events, fmt.Sprintf("healthz#%d", gate))
		if gate == failGate {
			return errors.New("apiserver /healthz did not become ok")
		}
		return nil
	})
	// Containerd reads active and reloads cleanly, so applyHostConfigs goes on
	// to the legacy-config heal; stubbed, the test cannot delete the host's
	// /etc/containerd/config.toml. unitActive is already stubbed by nodeUp,
	// restated so the fixture does not depend on it.
	swap(t, &unitActive, func(context.Context, string) bool { return true })
	swap(t, &removeLegacyConfig, func(io.Writer) {})
	swap(t, &legacyConfigPresent, func() (string, bool) { return "", false })
	swap(t, &restartUnit, func(string) error { return nil })
	swap(t, &unitExists, func(string) bool { return false })
	swap(t, &writeNodeConfigs, func(io.Writer, *nodestate.NodeState) error { return nil })
	swap(t, &reloadUnit, func(string, supervisor.ProcSpec) error { return nil })
	swap(t, &stampNode, func(_ context.Context, node string) error { events = append(events, "stamp:"+node); return nil })
	return &events
}

// swap replaces *p with v for the test.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// stampUpgraded is covered alone elsewhere; nothing failed if the
// call left UpgradeNode's success path. Once, with the persisted node name,
// after every health gate and the version check.
func TestUpgradeNodeStampsAControllerOnceItIsVerified(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) }); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	want := []string{"healthz#1", "healthz#2", "healthz#3", "stamp:cp-1.registered"}
	if strings.Join(*events, " ") != strings.Join(want, " ") {
		t.Errorf("events = %v, want %v", *events, want)
	}
}

// The other side of the same contract: an upgrade that did not land verified
// at target does not stamp the node as upgraded.
func TestUpgradeNodeDoesNotStampAnUpgradeThatFailed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		payloadVer string
		failGate   int
		want       string
	}{
		{"rolled back at the node gate", "v1.37.9", 2, "the node was rolled back"},
		{"rolled back at the apiserver gate", "v1.37.9", 1, "the node was rolled back"},
		{"post-reload gate, no rollback", "v1.37.9", 3, "post-upgrade host reload health gate"},
		{"kubelet reports another version", "v1.37.7", 0, "payload/binary mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := controllerUpgradeFixture(t, tc.payloadVer, tc.failGate)
			_, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("UpgradeNode = %v, want an error containing %q", err, tc.want)
			}
			for _, e := range *events {
				if strings.HasPrefix(e, "stamp:") {
					t.Errorf("stamped after a failed upgrade: %v", *events)
				}
			}
		})
	}
}

// The runtime-version checks sit inside the same node
// gate the stamp waits on. An orphaned containerd on the CRI socket, or a
// kubelet whose Node still reports the old version, fails the gate, the node
// rolls back, and nothing is stamped as upgraded.
func TestUpgradeNodeDoesNotStampWhenTheRuntimeIsNotTheInstalledOne(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func()
		want  string
	}{
		{"orphaned containerd", func() {
			servingContainerd = func(context.Context, string) (string, error) { return "v0.9.0", nil }
		}, "orphan"},
		{"kubelet still reports the old version", func() {
			kubeletReports = func(context.Context, string) (string, error) { return "v0.9.0", nil }
		}, "reports kubelet v0.9.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := controllerUpgradeFixture(t, "v1.37.9", 0)
			tc.setup() // after the fixture, which installs matching probes
			_, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) })
			if err == nil || !strings.Contains(err.Error(), "the node was rolled back") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("UpgradeNode = %v, want a rollback error containing %q", err, tc.want)
			}
			for _, e := range *events {
				if strings.HasPrefix(e, "stamp:") {
					t.Errorf("stamped after a failed upgrade: %v", *events)
				}
			}
		})
	}
}

// The upgrade's refreshes sit in a fixed order around its health gates, and
// the shared render that replaces them must keep it. The certificate pass and
// the CIS config write come before the apiserver's re-render, so the pod the
// new image recreates reads this release's Pod Security pin rather than a
// later restart's. Nothing that changes a host process's on-disk config or
// argv runs before the node gate proves the new binaries.
// The stamp follows every host change. A supervisor still running the
// previous build is handed to the installed one after the stamp, and the node
// is gated again once it is back; that restarts the operator too, so it is not
// restarted on its own. Each seam records an event; only the relative order
// is asserted.
func TestUpgradeNodeKeepsItsRefreshOrder(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	// A CIS controller, so the CIS refresh runs at all; the fixture's
	// node.yaml is not one. The CIS paths join the fixture's redirected config
	// dir, the way withCISConfigs redirects them, so nothing outside the test
	// is read.
	writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"),
		"role: controller\nhostname: cp-1.registered\nnodeIP: 10.0.0.1\ncis: true\n", 0o600)
	redirect(t, &version.AdmissionConfigPath, filepath.Join(version.K8sConfDir, "admission-config.yaml"))
	redirect(t, &version.AuditPolicyPath, filepath.Join(version.K8sConfDir, "audit-policy.yaml"))
	rec := func(e string) { *events = append(*events, e) }
	swap(t, &refreshCerts, func(io.Writer, *nodestate.NodeState, string) (bool, error) { rec("certs"); return false, nil })
	swap(t, &writeProfileConfigs, func(profile.Profile) error { rec("cis"); return nil })
	swap(t, &startStaticPod, func(name string) error { rec("start:" + name); return nil })
	swap(t, &writeNodeConfigs, func(io.Writer, *nodestate.NodeState) error { rec("configs"); return nil })
	swap(t, &reloadUnit, func(name string, _ supervisor.ProcSpec) error { rec("reload:" + name); return nil })
	swap(t, &restartUnit, func(name string) error { rec("restart:" + name); return nil })
	swap(t, &unitExists, func(string) bool { return true }) // the operator runs on this node
	handedOver := false
	swap(t, &runningSupervisor, func() (comps.RunningSupervisor, error) {
		return comps.RunningSupervisor{PID: 1, Exe: "/usr/local/bin/stratum (deleted)", Installed: handedOver}, nil
	})
	swap(t, &reexecSupervisor, func() error { rec("handoff"); handedOver = true; return nil })
	swap(t, &supervisorStatus, func() (map[string]supervisor.ProcStatus, error) { return nil, nil })

	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) }); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	at := func(e string) int {
		t.Helper()
		i := slices.Index(*events, e)
		if i < 0 {
			t.Fatalf("event %q never happened; events = %v", e, *events)
		}
		return i
	}
	apiserver := at("start:kube-apiserver")
	if c, cis := at("certs"), at("cis"); c > apiserver || cis > apiserver {
		t.Errorf("the certificate pass (%d) or the CIS config write (%d) came after the apiserver re-render (%d)", c, cis, apiserver)
	}
	for _, n := range []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"} {
		at("start:" + n)
	}
	gate := at("healthz#2") // the node gate before the config refresh
	for i, e := range *events {
		if strings.HasPrefix(e, "start:") && i > gate {
			t.Errorf("%s came after the node gate (%d)", e, gate)
		}
	}
	configs, reloadCtrd, reloadKubelet := at("configs"), at("reload:containerd"), at("reload:kubelet")
	if configs < gate || reloadCtrd < gate || reloadKubelet < gate {
		t.Errorf("the node-config refresh (%d) or a reload (%d, %d) ran before the node gate (%d)", configs, reloadCtrd, reloadKubelet, gate)
	}
	if slices.Contains(*events, "restart:operator") {
		t.Errorf("the operator was restarted on its own as well as by the handoff that replaced the supervisor: %v", *events)
	}
	stamp, handoff := at("stamp:cp-1.registered"), at("handoff")
	for _, e := range (*events)[stamp+1:] {
		if e != "handoff" && !strings.HasPrefix(e, "healthz#") {
			t.Errorf("%s came after the stamp (%d); only the handoff and its gate follow it", e, stamp)
		}
	}
	if handoff < stamp {
		t.Errorf("the supervisor handoff (%d) preceded the stamp (%d)", handoff, stamp)
	}
	if got := *events; len(got) < 2 || got[len(got)-2] != "handoff" || !strings.HasPrefix(got[len(got)-1], "healthz#") {
		t.Errorf("the upgrade ended on %v; want the handoff, then a node gate", got)
	}
}

// A node-gate failure rolls the node back, and the rollback must return the
// static-pod manifest set byte for byte with no stray file: kubelet parses
// every visible file in that directory, so a manifest left on the new render,
// or a leftover, changes pods on a node reported as rolled back. The derived
// admission-webhook.yaml is restored itself, with its own mode, because a
// rollback's manifests are not re-rendered — and a config the upgrade created
// is removed again, because it did not exist before it.
func TestGateRollbackRestoresTheManifestSetByteForByte(t *testing.T) {
	for _, tc := range []struct {
		name string
		cis  bool
	}{
		{"manifests only", false},
		{"with the CIS configs", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controllerUpgradeFixture(t, "v1.37.9", 2) // fail the node gate: the node rolls back
			nodeYAML := "role: controller\nhostname: cp-1.registered\nnodeIP: 10.0.0.1\n"
			if tc.cis {
				nodeYAML += "cis: true\n"
				redirect(t, &version.AdmissionConfigPath, filepath.Join(version.K8sConfDir, "admission-config.yaml"))
				redirect(t, &version.AuditPolicyPath, filepath.Join(version.K8sConfDir, "audit-policy.yaml"))
				writeFile(t, filepath.Join(version.K8sConfDir, "admission-webhook.yaml"), "old webhook", 0o600)
				swap(t, &writeProfileConfigs, func(profile.Profile) error { // this release's refresh overwrites the three
					for _, p := range []string{version.AdmissionConfigPath, version.AuditPolicyPath, filepath.Join(version.K8sConfDir, "admission-webhook.yaml")} {
						writeFile(t, p, "new", 0o644)
					}
					return nil
				})
			}
			writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"), nodeYAML, 0o600)

			// What the node started from, and the visible legacy leftover the
			// old writer could leave between its write and its rename.
			type savedFile struct {
				data string
				mode os.FileMode
			}
			original := map[string]savedFile{}
			for _, n := range staticpod.Names {
				b, err := os.ReadFile(staticpod.Path(n))
				if err != nil {
					t.Fatal(err)
				}
				fi, err := os.Stat(staticpod.Path(n))
				if err != nil {
					t.Fatal(err)
				}
				original[n+".yaml"] = savedFile{string(b), fi.Mode().Perm()}
			}
			writeFile(t, filepath.Join(staticpod.Dir(), "kube-scheduler.yaml.tmp"), "ghost", 0o600)

			// The re-render a gate-failing upgrade still performs.
			swap(t, &startStaticPod, func(name string) error {
				return os.WriteFile(staticpod.Path(name), []byte("rendered "+name+"\n"), 0o600)
			})

			_, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) })
			if err == nil || !strings.Contains(err.Error(), "the node was rolled back") {
				t.Fatalf("UpgradeNode = %v, want a rolled-back node", err)
			}

			for _, n := range staticpod.Names {
				b, err := os.ReadFile(staticpod.Path(n))
				if err != nil {
					t.Fatal(err)
				}
				fi, err := os.Stat(staticpod.Path(n))
				if err != nil {
					t.Fatal(err)
				}
				if got := (savedFile{string(b), fi.Mode().Perm()}); got != original[n+".yaml"] {
					t.Errorf("%s.yaml after the rollback = %q, %o; want the original %q, %o",
						n, got.data, got.mode, original[n+".yaml"].data, original[n+".yaml"].mode)
				}
			}
			if tc.cis {
				for _, p := range []string{version.AdmissionConfigPath, version.AuditPolicyPath} {
					if _, serr := os.Stat(p); !errors.Is(serr, os.ErrNotExist) {
						t.Errorf("%s survived the rollback, but it did not exist before the upgrade: %v", p, serr)
					}
				}
				b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "admission-webhook.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				fi, err := os.Stat(filepath.Join(version.K8sConfDir, "admission-webhook.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != "old webhook" || fi.Mode().Perm() != 0o600 {
					t.Errorf("admission-webhook.yaml after the rollback = %q, %o; want the original, 0600", b, fi.Mode().Perm())
				}
			}
			entries, err := os.ReadDir(staticpod.Dir())
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range entries {
				got = append(got, e.Name())
			}
			want := make([]string, 0, len(staticpod.Names))
			for _, n := range staticpod.Names {
				want = append(want, n+".yaml")
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("the static-pod directory holds %v after the rollback; want exactly %v", got, want)
			}
		})
	}
}

// The supervisor that survives an in-place upgrade keeps starting the host
// processes, so the upgrade hands it THIS release's definitions for them: it
// renders containerd's and the kubelet's argv the way `stratum up` does — from
// the node state, with the operator switch off since neither argv depends on
// it — and pushes one spec each through the reload op. Any other process, or
// another release's argv, leaves a node running an old definition while the
// upgrade claims it landed.
func TestUpgradeHandsTheSupervisorThisReleasesHostArgv(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	// The kubelet render picks its flags by whether kubelet.kubeconfig exists;
	// an empty dir makes that deterministic on every machine.
	redirect(t, &version.KubeconfigDir, t.TempDir())

	type handed struct {
		name string
		spec supervisor.ProcSpec
	}
	var reloads []handed
	swap(t, &reloadUnit, func(name string, spec supervisor.ProcSpec) error {
		reloads = append(reloads, handed{name, spec})
		return nil
	})

	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) }); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	if got := *events; len(got) == 0 || got[len(got)-1] != "stamp:cp-1.registered" {
		t.Fatalf("the upgrade did not run to its stamp; events = %v", got)
	}

	st, err := nodestate.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]supervisor.ProcSpec{}
	for _, p := range cluster.Processes(st, false) {
		args := p.Args
		if p.ArgsFn != nil {
			args = p.ArgsFn()
		}
		want[p.Name] = supervisor.ProcSpec{Path: p.Path, Args: args, Env: p.Env}
	}
	if len(reloads) != 2 || reloads[0].name != "containerd" || reloads[1].name != "kubelet" {
		t.Fatalf("the upgrade reloaded %v; want exactly containerd then kubelet", reloads)
	}
	for _, r := range reloads {
		w := want[r.name]
		if r.spec.Path != w.Path || !slices.Equal(r.spec.Args, w.Args) || !slices.Equal(r.spec.Env, w.Env) {
			t.Errorf("%s was handed path %q, args %q, env %q; want this release's %q, %q, %q",
				r.name, r.spec.Path, r.spec.Args, r.spec.Env, w.Path, w.Args, w.Env)
		}
	}
}

// The supervisor that survives the upgrade can be one release older than the
// binary running it, and answer the reload op with the refusal that predates
// it. Only that exact case — ErrReloadUnsupported — defers the new host
// process argv: the upgrade still succeeds, still stamps the node, and says
// the new argv applies on the next `stratum up` or reboot rather than
// claiming it landed or failing the node.
func TestUpgradeAgainstAnOldSupervisorDefersTheNewArgv(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	swap(t, &reloadUnit, func(name string, _ supervisor.ProcSpec) error {
		return fmt.Errorf("reload %s: %w", name, supervisor.ErrReloadUnsupported)
	})

	out, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) })
	if err != nil {
		t.Fatalf("UpgradeNode against an old supervisor = %v\n%s", err, out)
	}
	if !strings.Contains(out, "new host-process args apply on the next `stratum up` or reboot") {
		t.Errorf("the output never defers the new argv:\n%s", out)
	}
	if got := *events; len(got) == 0 || got[len(got)-1] != "stamp:cp-1.registered" {
		t.Errorf("the node was not stamped as upgraded; events = %v", got)
	}
}

// Exercise the successful upgrade's prune wiring, rather than supplying the
// prune patterns ourselves: dropping a kind from that wiring must leave an
// old backup behind and fail this guard.
func TestSuccessfulUpgradePrunesEveryBackupKind(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	const currentStamp = "20260404-000000"
	swap(t, &nowStamp, func() string { return currentStamp })
	writeFile(t, stratumInstallPath, "old stratum", 0o755)
	writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"),
		"role: controller\nhostname: cp-1.registered\nnodeIP: 10.0.0.1\ncis: true\n", 0o600)
	redirect(t, &version.AdmissionConfigPath, filepath.Join(version.K8sConfDir, "admission-config.yaml"))
	redirect(t, &version.AuditPolicyPath, filepath.Join(version.K8sConfDir, "audit-policy.yaml"))
	swap(t, &writeProfileConfigs, func(profile.Profile) error { return nil })

	kinds := []struct {
		name string
		path func(string) string
		dir  bool
	}{
		{"stratum", func(ts string) string { return stratumInstallPath + ".bak-" + ts }, false},
		{"kubelet", func(ts string) string { return filepath.Join(version.BinDir, "kubelet.bak-"+ts) }, false},
		{"manifests", func(ts string) string { return filepath.Join(version.DataDir, "upgrade", "manifests-bak-"+ts) }, true},
		{"profile configs", func(ts string) string { return filepath.Join(version.DataDir, "upgrade", "cisconfig-bak-"+ts) }, true},
	}
	oldStamps := []string{"20260101-000000", "20260202-000000", "20260303-000000"}
	for _, kind := range kinds {
		for _, ts := range oldStamps {
			path := kind.path(ts)
			if kind.dir {
				path = filepath.Join(path, "saved")
			}
			writeFile(t, path, "old backup", 0o600)
		}
	}

	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) }); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	if got := *events; len(got) == 0 || got[len(got)-1] != "stamp:cp-1.registered" {
		t.Fatalf("the upgrade did not finish; events = %v", got)
	}
	for _, kind := range kinds {
		for _, ts := range append(slices.Clone(oldStamps), currentStamp) {
			_, err := os.Stat(kind.path(ts))
			if ts == oldStamps[2] || ts == currentStamp {
				if err != nil {
					t.Errorf("%s backup %s must survive successful upgrade pruning: %v", kind.name, ts, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("old %s backup %s was not pruned: %v", kind.name, ts, err)
			}
		}
	}
}

// UpgradeNode's gates read the Node kubelet registered under,
// the persisted hostname, which is what the stamp uses too. The fixture's
// node.yaml names it "cp-1.registered", which no OS hostname matches.
func TestUpgradeNodeReadsTheKubeletNodeItRegisteredUnder(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	var asked []string
	kubeletReports = func(_ context.Context, node string) (string, error) {
		asked = append(asked, node)
		return "v1.0.0", nil // what runtimeFixture's installed kubelet prints
	}
	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) }); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	if len(asked) == 0 {
		t.Fatal("the kubelet version check never read a Node")
	}
	for _, n := range asked {
		if n != "cp-1.registered" {
			t.Errorf("the kubelet check read node %q, want the registered name %q (reads: %v)", n, "cp-1.registered", asked)
		}
	}
	if got := *events; len(got) == 0 || got[len(got)-1] != "stamp:cp-1.registered" {
		t.Errorf("events = %v; the upgrade should end by stamping the same node", got)
	}
}

// TestUpgradeNodeWaitsForTheReplacedProcess: a component whose manifest
// changed is gated only once the process kubelet is replacing has exited,
// so its gate cannot be answered by the old one, and nothing restarts
// kubelet or containerd while kubelet is still replacing it.
func TestUpgradeNodeWaitsForTheReplacedProcess(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	swap(t, &startStaticPod, func(name string) error {
		return os.WriteFile(staticpod.Path(name), []byte("name: "+name+"\nimage: new\n"), 0o600)
	})
	swap(t, &waitComponentReplace, func(name string, _ staticpod.Mark, _ time.Duration) error {
		*events = append(*events, "replaced:"+name)
		return nil
	})
	if err := UpgradeNode(io.Discard, NodeOptions{}); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	if got := strings.Join(*events, " "); !strings.HasPrefix(got, "replaced:etcd replaced:kube-apiserver") {
		t.Errorf("events = %q, want each changed component waited for, etcd first, before any gate", got)
	}
}

// TestUpgradeNodeWaitsForTheApiserverRestartedOntoItsCert: the restart onto a
// reissued certificate changes the manifest again, so its gate also waits for
// the apiserver it replaces to be gone.
func TestUpgradeNodeWaitsForTheApiserverRestartedOntoItsCert(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	swap(t, &refreshCerts, func(io.Writer, *nodestate.NodeState, string) (bool, error) { return true, nil })
	swap(t, &restartStaticPod, func(name string) error {
		*events = append(*events, "restart:"+name)
		return nil
	})
	swap(t, &waitComponentReplace, func(name string, _ staticpod.Mark, _ time.Duration) error {
		*events = append(*events, "replaced:"+name)
		return nil
	})
	if err := UpgradeNode(io.Discard, NodeOptions{}); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	if got := strings.Join(*events, " "); !strings.Contains(got, "restart:kube-apiserver replaced:kube-apiserver healthz#") {
		t.Errorf("events = %q, want the restarted apiserver waited for before its gate", got)
	}
}

func TestUpgradeNodeWritesKubeVipConfigBeforeKubeVip(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	writeFile(t, filepath.Join(version.K8sConfDir, "node.yaml"),
		"role: controller\nhostname: cp-1.registered\nnodeIP: 10.0.0.1\nvip:\n  address: 10.0.0.100\n", 0o600)
	swap(t, &startStaticPod, func(name string) error { *events = append(*events, "start:"+name); return nil })

	if err := UpgradeNode(io.Discard, NodeOptions{SkipSnapshot: true}); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	cfg, kv := slices.Index(*events, "kube-vip kubeconfig"), slices.Index(*events, "start:kube-vip")
	if cfg < 0 || kv < 0 || cfg > kv {
		t.Fatalf("kube-vip kubeconfig at %d, kube-vip re-render at %d; want the kubeconfig first. events = %v", cfg, kv, *events)
	}
}

func TestUpgradeNodeWithoutAVIPWritesNoKubeVipConfig(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	if err := UpgradeNode(io.Discard, NodeOptions{SkipSnapshot: true}); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	if slices.Contains(*events, "kube-vip kubeconfig") {
		t.Error("a node with no VIP wrote kube-vip's kubeconfig")
	}
}

func TestUpgradeNodeWritesSchedulerConfigBeforeScheduler(t *testing.T) {
	events := controllerUpgradeFixture(t, "v1.37.9", 0)
	swap(t, &writeSchedulerConfig, func() error { *events = append(*events, "scheduler kubeconfig"); return nil })
	swap(t, &startStaticPod, func(name string) error { *events = append(*events, "start:"+name); return nil })

	if _, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) }); err != nil {
		t.Fatalf("UpgradeNode: %v", err)
	}
	cfg, sched := slices.Index(*events, "scheduler kubeconfig"), slices.Index(*events, "start:kube-scheduler")
	if cfg < 0 || sched < 0 || cfg > sched {
		t.Fatalf("scheduler kubeconfig at %d, kube-scheduler re-render at %d; want the kubeconfig first. events = %v", cfg, sched, *events)
	}
}

func TestUpgradeNodeFailsWhenTheSchedulerConfigCannotBeWritten(t *testing.T) {
	controllerUpgradeFixture(t, "v1.37.9", 0)
	swap(t, &writeSchedulerConfig, func() error { return errors.New("no server") })

	_, err := runWriter(t, func(w io.Writer) error { return UpgradeNode(w, NodeOptions{SkipSnapshot: true}) })
	if err == nil || !strings.Contains(err.Error(), "kube-scheduler's kubeconfig") {
		t.Fatalf("UpgradeNode = %v, want the scheduler kubeconfig failure, rolled back", err)
	}
}

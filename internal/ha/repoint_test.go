package ha

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
)

func TestNodeRepointIPv6Kubeconfig(t *testing.T) {
	servingVIP(t)
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: cluster\n  cluster:\n    server: https://127.0.0.1:6443\ncontexts:\n- name: local\n  context:\n    cluster: cluster\ncurrent-context: local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop before restarting host services")
	warnings, err := nodeRepoint(io.Discard, "fd00::1", func(server string) error {
		if err := kubeconfig.SetServer(path, server); err != nil {
			t.Fatal(err)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none: the repoint stopped before any consumer restart", warnings)
	}
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters["cluster"].Server; got != "https://[fd00::1]:6443" {
		t.Fatalf("server = %q", got)
	}
}

func TestRepointKubeconfigsCoversBootstrapConf(t *testing.T) {
	dir := t.TempDir()
	oldDir, oldAdmin, oldBoot := version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig
	t.Cleanup(func() {
		version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig = oldDir, oldAdmin, oldBoot
	})
	version.KubeconfigDir = dir
	version.AdminConf = filepath.Join(dir, "admin.conf")
	version.BootstrapKubeconfig = filepath.Join(dir, "bootstrap.conf")

	const body = "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: https://127.0.0.1:6443\nusers:\n- name: u\n  user:\n    token: tok\ncontexts:\n- name: x\n  context:\n    cluster: c\n    user: u\ncurrent-context: x\n"
	// Only some files exist, as on a worker; absent ones must be skipped.
	for _, f := range []string{version.BootstrapKubeconfig, filepath.Join(dir, "kubelet.kubeconfig")} {
		if err := os.WriteFile(f, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const server = "https://10.0.0.5:6443"
	if err := RepointKubeconfigs(server); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{version.BootstrapKubeconfig, filepath.Join(dir, "kubelet.kubeconfig")} {
		cfg, err := clientcmd.LoadFromFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Clusters["c"].Server; got != server {
			t.Errorf("%s server = %q, want %q", filepath.Base(f), got, server)
		}
		if got := cfg.AuthInfos["u"].Token; got != "tok" {
			t.Errorf("%s token = %q, credentials not preserved", filepath.Base(f), got)
		}
	}
	if _, err := os.Stat(version.AdminConf); !os.IsNotExist(err) {
		t.Errorf("absent admin.conf must not be created: %v", err)
	}
}

// TestNodeRepoint_ReturnsTheKubeletRestartWarning pins the outcome contract
// for the node repoint: a local consumer that fails to restart after the
// kubeconfigs were rewritten comes back as a warning beside a nil error —
// data a caller can report, not only a printed line and not a repoint
// failure — because a kubelet that does not come back leaves the node NotReady
// while the repoint itself landed. The supervisor's control socket points at
// an absent path so the kubelet restart fails, and no static-pod manifests
// exist, so the kubelet is the only consumer that can warn.
func TestNodeRepoint_ReturnsTheKubeletRestartWarning(t *testing.T) {
	servingVIP(t)
	root := t.TempDir()
	oldConf, oldSock := version.K8sConfDir, version.ControlSocket
	t.Cleanup(func() { version.K8sConfDir, version.ControlSocket = oldConf, oldSock })
	version.K8sConfDir = filepath.Join(root, "config")         // no manifests: controller-manager and scheduler are skipped
	version.ControlSocket = filepath.Join(root, "absent.sock") // the kubelet restart fails

	repointed := ""
	warnings, err := nodeRepoint(io.Discard, "10.0.0.5", func(server string) error {
		repointed = server
		return nil
	})
	if err != nil {
		t.Fatalf("nodeRepoint = %v; a failed best-effort restart must not fail the repoint", err)
	}
	if repointed != "https://10.0.0.5:6443" {
		t.Errorf("the kubeconfigs were repointed to %q, want the VIP", repointed)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "restart kubelet") {
		t.Errorf("warnings = %q, want one warning naming the kubelet restart", warnings)
	}
}

// TestRepointCiliumWith_ReturnsTheStuckRolloutAsAWarning pins the Cilium
// repoint's outcome contract: the env change is applied first, and a rollout
// that does not finish in time is reported as a warning beside a nil error —
// the change is already live, so the repoint is not a failure, and the caller
// reads what happened from the warnings list instead of a sentinel error.
func TestRepointCiliumWith_ReturnsTheStuckRolloutAsAWarning(t *testing.T) {
	servingVIP(t)
	oldPoll := rolloutPoll
	t.Cleanup(func() { rolloutPoll = oldPoll })
	rolloutPoll = 10 * time.Millisecond

	ds, dep := ciliumObjects(true)
	dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing,
		Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
	cs := fake.NewSimpleClientset(ds, dep)

	warnings, err := RepointCiliumWith(t.Context(), io.Discard, cs, "10.0.0.10")
	if err != nil {
		t.Fatalf("RepointCiliumWith = %v; a stuck rollout is a warning, the env change is already applied", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "deploy/cilium-operator") {
		t.Errorf("warnings = %q, want one warning naming deploy/cilium-operator", warnings)
	}
	updated := false
	for _, a := range cs.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "deployments" {
			updated = true
		}
	}
	if !updated {
		t.Error("the operator env change was not applied although the repoint reported only a warning")
	}
}

// TestRepointCiliumWith_CleanRolloutCarriesNoWarnings pins the other side of
// the outcome contract: a rollout that finishes leaves the repoint's success
// carrying no warnings beside its nil error.
func TestRepointCiliumWith_CleanRolloutCarriesNoWarnings(t *testing.T) {
	servingVIP(t)
	oldPoll := rolloutPoll
	t.Cleanup(func() { rolloutPoll = oldPoll })
	rolloutPoll = 10 * time.Millisecond

	ds, dep := ciliumObjects(true)
	warnings, err := RepointCiliumWith(t.Context(), io.Discard, fake.NewSimpleClientset(ds, dep), "10.0.0.10")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none on a finished rollout", warnings)
	}
}

// ── the stdout-guard retirement pins ─────────────────────────────────────────
//
// The wrapped ha operations take an io.Writer FIRST (after any ctx) and
// report their progress and warnings through it, so internal/mcp needs no
// captureRun/captureWarns stdout bridge around them. ha.Join's prints are
// outside the retirement surface.

// captureRealStdout captures the process's ACTUAL stdout while fn runs, so a
// test can prove an operation left it alone.
func captureRealStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(&buf, r) }()

	fn()

	os.Stdout = saved
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String()
}

// TestNodeRepoint_WritesItsProgressToTheWriter pins the node repoint's half of
// the convention: NodeRepoint takes the writer first, its step headers, the
// kubelet-restart warning and the closing "clients now target" line are
// written there, and os.Stdout never sees them.
func TestNodeRepoint_WritesItsProgressToTheWriter(t *testing.T) {
	servingVIP(t)
	root := t.TempDir()
	oldConf, oldSock := version.K8sConfDir, version.ControlSocket
	t.Cleanup(func() { version.K8sConfDir, version.ControlSocket = oldConf, oldSock })
	version.K8sConfDir = filepath.Join(root, "config")         // no manifests: controller-manager and scheduler are skipped
	version.ControlSocket = filepath.Join(root, "absent.sock") // the kubelet restart fails

	var buf bytes.Buffer
	var (
		warnings []string
		err      error
	)
	onStdout := captureRealStdout(t, func() {
		warnings, err = NodeRepoint(&buf, "10.0.0.5")
	})
	if err != nil {
		t.Fatalf("NodeRepoint = %v; a failed best-effort restart must not fail the repoint", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "restart kubelet") {
		t.Errorf("warnings = %q, want one warning naming the kubelet restart", warnings)
	}
	for _, want := range []string{"ha-repoint: this node's clients now target https://10.0.0.5:6443", "WARN: restart kubelet"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the repoint's progress is missing %q; the writer it was handed has:\n%s", want, buf.String())
		}
	}
	if strings.Contains(onStdout, "ha-repoint") {
		t.Errorf("the repoint printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

// TestRepointCiliumWith_WritesItsProgressToTheWriter pins the Cilium repoint's
// half: the writer comes right after the context, the VIP check and the
// closing "Cilium now targets the VIP" line reach it, and a clean rollout
// leaves os.Stdout untouched.
func TestRepointCiliumWith_WritesItsProgressToTheWriter(t *testing.T) {
	servingVIP(t)
	oldPoll := rolloutPoll
	t.Cleanup(func() { rolloutPoll = oldPoll })
	rolloutPoll = 10 * time.Millisecond

	ds, dep := ciliumObjects(true)

	var buf bytes.Buffer
	var (
		warnings []string
		err      error
	)
	onStdout := captureRealStdout(t, func() {
		warnings, err = RepointCiliumWith(t.Context(), &buf, fake.NewSimpleClientset(ds, dep), "10.0.0.10")
	})
	if err != nil {
		t.Fatalf("RepointCiliumWith: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none on a finished rollout", warnings)
	}
	for _, want := range []string{"Check that the VIP serves this cluster's apiserver", "ha-repoint --cilium: Cilium now targets the VIP 10.0.0.10"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the repoint's progress is missing %q; the writer it was handed has:\n%s", want, buf.String())
		}
	}
	if strings.Contains(onStdout, "ha-repoint --cilium") {
		t.Errorf("the repoint printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

// TestRejoinEtcd_WritesItsWaitingProgressToTheWriter pins the rejoin wait's
// half (the restore.go report loop): RejoinEtcd takes the writer after its
// context, and the periodic "still waiting for etcd member" report is written
// there, never to os.Stdout. The fake clock advances a member that answers
// after four minutes, so the report loop fires several times on the way.
func TestRejoinEtcd_WritesItsWaitingProgressToTheWriter(t *testing.T) {
	f := newFakeRejoin(t, seedWith(), controller())
	f.local = servingAfter(4*time.Minute, addedID, true)

	var buf bytes.Buffer
	var err error
	onStdout := captureRealStdout(t, func() {
		err = RejoinEtcd(t.Context(), &buf, "10.0.0.1", false)
	})
	if err != nil {
		t.Fatalf("RejoinEtcd: %v", err)
	}
	if !strings.Contains(buf.String(), "still waiting for etcd member") {
		t.Errorf("the rejoin's waiting progress never reached the writer it was handed; it has:\n%s", buf.String())
	}
	if strings.Contains(onStdout, "still waiting for etcd member") {
		t.Errorf("the rejoin printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

// TestRepointRecordsTheServer pins that `stratum ha-repoint` survives the next
// `stratum up`, because the repoint records the server it wrote in the node
// state's Server — the record every kubeconfig writer reads — instead of only
// rewriting the files from its own list. The state here is what a single
// controller carries before it grows into HA (clients on loopback, no VIP in
// the record, per nodeRepoint's documented use): the drift today is that the
// repointed kubeconfigs are rewritten from the stale record at the next start.
func TestRepointRecordsTheServer(t *testing.T) {
	oldData := version.DataDir
	version.DataDir = t.TempDir()
	t.Cleanup(func() { version.DataDir = oldData })
	const server = "https://192.168.99.240:6443"
	const before = "https://127.0.0.1:6443"
	dir := t.TempDir()
	oldDir, oldAdmin, oldBoot := version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig
	t.Cleanup(func() {
		version.KubeconfigDir, version.AdminConf, version.BootstrapKubeconfig = oldDir, oldAdmin, oldBoot
	})
	version.KubeconfigDir = dir
	version.AdminConf = filepath.Join(dir, "admin.conf")
	version.BootstrapKubeconfig = filepath.Join(dir, "bootstrap.conf")
	oldConf := version.K8sConfDir
	version.K8sConfDir = filepath.Join(dir, "kubernetes")
	t.Cleanup(func() { version.K8sConfDir = oldConf })

	// The kubeconfig the repoint rewrites, and the node state it must record
	// into: the directory setup of TestRepointKubeconfigs plus the saved
	// state a controller has.
	in := strings.Join([]string{
		"apiVersion: v1",
		"kind: Config",
		"clusters:",
		"- cluster:",
		"    certificate-authority-data: QUFBQQ==",
		"    server: " + before,
		"  name: stratum",
		"contexts:",
		"- context: {cluster: stratum, user: admin}",
		"  name: stratum",
		"current-context: stratum",
		"users:",
		"- name: admin",
		"  user: {}",
	}, "\n")
	if err := os.WriteFile(version.AdminConf, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", Hostname: "cp1",
		NodeIP: "192.168.99.5", Server: before}); err != nil {
		t.Fatal(err)
	}

	if err := RepointKubeconfigs(server); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(version.AdminConf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "server: "+server) {
		t.Errorf("admin.conf was not repointed:\n%s", out)
	}
	saved, err := nodestate.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Server != server {
		t.Errorf("node state Server = %q, want the repointed %q — the next `stratum up` writes kubeconfigs from the record, so the repoint does not survive it",
			saved.Server, server)
	}

	// A growth recorder holds this same lock while persisting EtcdHasPeers.
	// Repoint must refuse rather than save an independently loaded snapshot —
	// and since the rewrites and the record land under one lock acquisition,
	// the refused repoint leaves the kubeconfigs unchanged too: a repoint
	// that rewrote the files and then lost the state write would leave files
	// targeting one endpoint under a state recording another, and the next
	// start's recorded server would discard the deliberate endpoint.
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		t.Fatal(err)
	}
	err = RepointKubeconfigs(before)
	release()
	if err == nil {
		t.Fatal("repoint overwrote state while another state writer held its lock")
	}
	out, err = os.ReadFile(version.AdminConf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "server: "+server) {
		t.Errorf("the refused repoint rewrote admin.conf:\n%s", out)
	}
	if err := nodestate.Mutate(func(st *nodestate.NodeState) error { st.EtcdHasPeers = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := RepointKubeconfigs(before); err != nil {
		t.Fatal(err)
	}
	saved, err = nodestate.LoadState()
	if err != nil || saved.Server != before || !saved.EtcdHasPeers {
		t.Fatalf("repoint lost the growth record: state=%+v err=%v", saved, err)
	}
}

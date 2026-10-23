package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/etcdsnap"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
	"github.com/ctrliq/stratum/internal/supervisor"
	"github.com/ctrliq/stratum/internal/uninstall"
	"github.com/ctrliq/stratum/internal/upgrade"
	"github.com/ctrliq/stratum/internal/version"
)

// fakeStratumHost points the host probe at a directory that exists, so the
// tools take the "this is a node" branch.
func fakeStratumHost(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	saved := stratumHostDirs
	stratumHostDirs = []string{dir}
	t.Cleanup(func() { stratumHostDirs = saved })
	return dir
}

// notStratumHost points the probe at a path that cannot exist.
func notStratumHost(t *testing.T) {
	t.Helper()
	saved := stratumHostDirs
	stratumHostDirs = []string{filepath.Join(t.TempDir(), "absent")}
	t.Cleanup(func() { stratumHostDirs = saved })
}

func TestOnStratumHost(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		fakeStratumHost(t)
		if !onStratumHost() {
			t.Error("a present directory was not recognised")
		}
	})
	t.Run("absent", func(t *testing.T) {
		notStratumHost(t)
		if onStratumHost() {
			t.Error("an absent directory was recognised as a node")
		}
		if err := requireStratumHost(); err == nil {
			t.Fatal("requireStratumHost accepted a machine with no Stratum directories")
		}
	})
}

// The message is the product here.
func TestNotStratumHostErrorExplainsItself(t *testing.T) {
	msg := errNotStratumHost.Error()
	for _, want := range []string{"not a Stratum node", "/etc/stratum", "/var/lib/stratum", "generic Kubernetes tools"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q:\n%s", want, msg)
		}
	}
}

// planKubeadmCPLabel is the kubeadm role label, spelled out because the
// orchestrator's decision under test is that its presence alone must not make
// a node a control plane.
const planKubeadmCPLabel = "node-role.kubernetes.io/control-plane"

// planRow is one node's place in a rendered plan: name and role, in order.
type planRow struct{ name, role string }

// nodeForPlan is one Node object as both front ends read it: Ready, with the
// given labels, kubelet version and in-progress record.
func nodeForPlan(name string, labels map[string]string, kubeletVersion string, annotations map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: kubeletVersion},
		},
	}
}

// servePlanNodes serves nodes as the API would, so one node list feeds both
// front ends in the same order: the dry run's client and upgrade_plan's
// connector read the same server.
func servePlanNodes(t *testing.T, nodes []corev1.Node) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(&corev1.NodeList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"},
		Items:    nodes,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captureUpgradeStdout captures what UpgradeCluster prints, so the test can
// read the dry run's rendered plan.
func captureUpgradeStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	runErr := fn()
	os.Stdout = saved
	_ = w.Close()
	out, _ := io.ReadAll(r)
	_ = r.Close()
	return string(out), runErr
}

// planRowsFromDryRun reads the dry run's node rows: one line per node, whose
// second field is the role and third the current version.
func planRowsFromDryRun(out string) []planRow {
	var rows []planRow
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.HasPrefix(f[2], "current=") {
			continue
		}
		if f[1] == "control-plane" || f[1] == "worker" {
			rows = append(rows, planRow{f[0], f[1]})
		}
	}
	return rows
}

// TestUpgradePlanParityWithTheClusterDryRun: upgrade_plan renders the same
// classification and order as UpgradeCluster's dry run for the same node set —
// the orchestrator's semantics are canonical, so the tool cannot rebuild the
// plan by hand and drift. The node set is built to catch both ways the two
// copies disagreed: a node with only the kubeadm role label (the orchestrator
// plans it as a worker), a node whose Stratum label says "false", and a served
// order that is not the alphabetical one.
func TestUpgradePlanParityWithTheClusterDryRun(t *testing.T) {
	fakeStratumHost(t)
	nodes := []corev1.Node{
		nodeForPlan("cp-2", map[string]string{k8sutil.ControlPlaneLabel: "true"}, "v1.37.9", nil),
		nodeForPlan("cp-1", map[string]string{planKubeadmCPLabel: ""}, "v1.37.9", nil),
		nodeForPlan("w-2", nil, "v1.37.9", nil),
		nodeForPlan("w-1", map[string]string{k8sutil.ControlPlaneLabel: "false"}, "v1.37.9", nil),
	}
	srv := servePlanNodes(t, nodes)

	// The CLI side: the dry run prints its plan rows to stdout.
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	var plan bytes.Buffer
	err = upgrade.UpgradeCluster(&plan, upgrade.ClusterOptions{
		Target: "v1.37.10", From: "staged-stratum", Client: cs, DryRun: true,
	})
	dryRun := plan.String()
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, dryRun)
	}
	want := planRowsFromDryRun(dryRun)
	if len(want) != len(nodes) {
		t.Fatalf("the dry run rendered %d plan rows, want one per node (%d):\n%s", len(want), len(nodes), dryRun)
	}

	// The MCP side: the same node list, through the tool's connector.
	conn := kube.NewConnector(kube.Options{Kubeconfig: kubetest.Kubeconfig(t, &rest.Config{Host: srv.URL})})
	_, out, err := UpgradePlanHandler(conn)(context.Background(), nil, UpgradePlanIn{Target: "v1.37.10", WorkersMaxUnavailable: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Allowed || out.Blocker != "" {
		t.Fatalf("upgrade_plan refused the upgrade the dry run allows: blocker %q", out.Blocker)
	}
	var got []planRow
	for _, s := range out.Steps {
		for _, n := range s.Nodes {
			got = append(got, planRow{n, s.Role})
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upgrade_plan renders %v; the dry run renders %v — the two disagree about which nodes are control planes or the order they roll in", got, want)
	}
	if out.NodeCount != len(nodes) {
		t.Errorf("upgrade_plan counted %d nodes, want %d", out.NodeCount, len(nodes))
	}
}

func TestSummarizeCerts(t *testing.T) {
	now := time.Now()
	got := summarizeCerts([]certs.CertStatus{
		{Name: "healthy", NotAfter: now.Add(300 * 24 * time.Hour), DaysLeft: 300, Rotatable: true},
		{Name: "expiring", NotAfter: now.Add(10 * 24 * time.Hour), DaysLeft: 10, Expiring: true, Rotatable: true},
		{Name: "dead", NotAfter: now.Add(-24 * time.Hour), DaysLeft: -1, Expiring: true, Rotatable: true},
		{Name: "ca", NotAfter: now.Add(3000 * 24 * time.Hour), DaysLeft: 3000, IsCA: true},
	})

	if got.Expired != 1 {
		t.Errorf("expired = %d, want 1", got.Expired)
	}
	// The expired one must not also be counted as expiring, or the two
	// numbers double-count and a summary reads as worse than it is.
	if got.Expiring != 1 {
		t.Errorf("expiring = %d, want 1 (the already-expired one is counted as expired, not both)", got.Expiring)
	}

	// Soonest first: the thing about to break is what should be read first.
	if got.Items[0].Name != "dead" || got.Items[len(got.Items)-1].Name != "ca" {
		var order []string
		for _, i := range got.Items {
			order = append(order, i.Name)
		}
		t.Errorf("order = %v, want soonest expiry first", order)
	}

	if !strings.Contains(got.Note, "expired") {
		t.Errorf("note does not lead with the expired certificate: %q", got.Note)
	}
	if !strings.Contains(got.Note, "Stratum-managed leaves can be renewed with certs_rotate") {
		t.Errorf("note does not identify the due managed leaf's renewal command: %q", got.Note)
	}
	if strings.Contains(got.Note, "certs_rotate_ca") {
		t.Errorf("healthy CA produced CA rotation advice: %q", got.Note)
	}
}

func TestSummarizeCertsCleanCluster(t *testing.T) {
	got := summarizeCerts([]certs.CertStatus{
		{Name: "fine", NotAfter: time.Now().Add(300 * 24 * time.Hour), DaysLeft: 300},
	})
	if got.Note != "" {
		t.Errorf("a healthy cluster produced a note: %q", got.Note)
	}
	if got.Expiring != 0 || got.Expired != 0 {
		t.Errorf("healthy cluster counted %d expiring / %d expired", got.Expiring, got.Expired)
	}
}

// TestSummarizeCertsKubeletClient keeps the warning window, overdue renewal,
// and expiry distinct and gives remediation for the certificate actually due.
func TestSummarizeCertsKubeletClient(t *testing.T) {
	got := summarizeCerts([]certs.CertStatus{
		{Name: certs.KubeletClientName, NotAfter: time.Now().Add(20 * 24 * time.Hour), DaysLeft: 20, Expiring: true, Kubelet: true, RenewalOverdue: true},
	})
	if len(got.Items) != 1 || !got.Items[0].Kubelet || got.Items[0].Rotatable || !got.Items[0].RenewalOverdue {
		t.Fatalf("items = %+v, want one overdue non-rotatable kubelet client row", got.Items)
	}
	if !strings.Contains(got.Note, "renewed by the kubelet itself, not by certs_rotate") {
		t.Errorf("note does not say certs_rotate cannot renew the kubelet client cert: %q", got.Note)
	}
	if !strings.Contains(got.Note, "past its latest expected renewal point") {
		t.Errorf("overdue kubelet renewal is missing from note: %q", got.Note)
	}
	if strings.Contains(got.Note, "Stratum-managed leaves") {
		t.Errorf("kubelet-only note recommends managed-leaf rotation: %q", got.Note)
	}

	insideWindow := summarizeCerts([]certs.CertStatus{
		{Name: certs.KubeletClientName, NotAfter: time.Now().Add(40 * 24 * time.Hour), DaysLeft: 40, Expiring: true, Kubelet: true},
	})
	if insideWindow.Expiring != 1 || !strings.Contains(insideWindow.Note, "renewed by the kubelet itself") || strings.Contains(insideWindow.Note, "past its latest expected renewal point") {
		t.Errorf("healthy kubelet inside warning window has incorrect status: %+v", insideWindow)
	}

	expired := summarizeCerts([]certs.CertStatus{
		{Name: certs.KubeletClientName, NotAfter: time.Now().Add(-time.Hour), DaysLeft: -1, Expiring: true, Kubelet: true, RenewalOverdue: true},
	})
	if expired.Expired != 1 || !strings.Contains(expired.Note, "remove and rejoin") || strings.Contains(expired.Note, "Stratum-managed leaves") || strings.Contains(expired.Note, "rotation is the fix") {
		t.Errorf("expired kubelet-only note gives contradictory remediation: %+v", expired)
	}

	overdueOutsideWindow := summarizeCerts([]certs.CertStatus{
		{Name: certs.KubeletClientName, NotAfter: time.Now().Add(20 * 24 * time.Hour), DaysLeft: 20, Kubelet: true, RenewalOverdue: true},
	})
	if overdueOutsideWindow.Expiring != 0 || !strings.HasPrefix(overdueOutsideWindow.Note, "The kubelet-client") || !strings.Contains(overdueOutsideWindow.Note, "past its latest expected renewal point") {
		t.Errorf("overdue renewal outside a short warning window was omitted: %+v", overdueOutsideWindow)
	}

	healthy := summarizeCerts([]certs.CertStatus{
		{Name: certs.KubeletClientName, NotAfter: time.Now().Add(200 * 24 * time.Hour), DaysLeft: 200, Kubelet: true},
	})
	if healthy.Note != "" {
		t.Errorf("a healthy kubelet client cert produced a note: %q", healthy.Note)
	}
}

// TestSummarizeProcs: a component whose socket another process serves makes
// the node unhealthy even though it has a pid; a wanted component
// that is not running still does; a stopped one that is not wanted does not.
func TestSummarizeProcs(t *testing.T) {
	for _, tc := range []struct {
		name        string
		procs       []supervisor.ProcStatus
		healthy     bool
		noteHas     []string
		foreignRows []string
	}{
		{name: "all running", procs: []supervisor.ProcStatus{
			{Name: "kubelet", Pid: 2, Running: true, Wanted: true},
			{Name: "containerd", Pid: 1, Running: true, Wanted: true},
		}, healthy: true},
		{name: "stopped on purpose", procs: []supervisor.ProcStatus{{Name: "operator"}}, healthy: true},
		{name: "wanted but down", procs: []supervisor.ProcStatus{{Name: "kubelet", Wanted: true}},
			noteHas: []string{"Wanted but not running: kubelet"}},
		{name: "foreign socket owner", procs: []supervisor.ProcStatus{
			{Name: "containerd", Pid: 9, Running: true, Wanted: true, Foreign: true},
			{Name: "kubelet", Pid: 2, Running: true, Wanted: true},
		}, noteHas: []string{"another process", "containerd"}, foreignRows: []string{"containerd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, healthy, note := summarizeProcs(tc.procs)
			if healthy != tc.healthy {
				t.Errorf("healthy = %v, want %v (note %q)", healthy, tc.healthy, note)
			}
			if !healthy && note == "" {
				t.Error("unhealthy with no note")
			}
			for _, w := range tc.noteHas {
				if !strings.Contains(note, w) {
					t.Errorf("note %q lacks %q", note, w)
				}
			}
			var foreign []string
			for _, r := range rows {
				if r.Foreign {
					foreign = append(foreign, r.Name)
				}
			}
			if strings.Join(foreign, ",") != strings.Join(tc.foreignRows, ",") {
				t.Errorf("foreign rows = %v, want %v", foreign, tc.foreignRows)
			}
			for i := 1; i < len(rows); i++ {
				if rows[i-1].Name > rows[i].Name {
					t.Errorf("rows not sorted: %v", rows)
				}
			}
		})
	}
}

// stubSnapshotDir points the snapshot directory at a fresh directory: the
// stand-in for a custom spec.snapshots.dir. Anything still using the default
// dir misses it. It also stubs loadSnapshotConfig to the S3-less default.
// The directory is part of the resolved snapshot settings, so the config
// stub carries it, and snapshotDir is stubbed to the same directory for the
// readers that resolve a bare snapshot name.
func stubSnapshotDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "snaps")
	old := snapshotDir
	t.Cleanup(func() { snapshotDir = old })
	snapshotDir = func() string { return dir }

	// The handlers also read the host's snapshot config; without this a run on
	// a node with an S3 target would prune and upload to the real bucket.
	oldCfg := loadSnapshotConfig
	t.Cleanup(func() { loadSnapshotConfig = oldCfg })
	cfg := etcdsnap.DefaultConfig()
	cfg.Dir = dir
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return cfg, nil }
	return dir
}

func TestSnapshotListHonoursSnapshotDir(t *testing.T) {
	fakeStratumHost(t)
	dir := stubSnapshotDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "etcd-cp1-1.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out SnapshotListOut
	var err error
	captureStderrDiscard(func() {
		_, out, err = SnapshotListHandler()(context.Background(), nil, SnapshotListIn{})
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Dir != dir {
		t.Errorf("Dir = %q, want %q", out.Dir, dir)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "etcd-cp1-1.db" {
		t.Errorf("items = %+v, want the snapshot in %s", out.Items, dir)
	}
}

func TestUninstallPreviewNamesSnapshotDir(t *testing.T) {
	fakeStratumHost(t)
	dir := stubSnapshotDir(t)

	_, out, err := UninstallPreviewHandler()(context.Background(), nil, UninstallPreviewIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Note, dir) || !strings.Contains(out.Note, "NOT part of this set") {
		t.Errorf("note %q does not say the snapshots in %s are kept", out.Note, dir)
	}
}

// A snapshot dir inside a removal target is deleted with it, so the note must
// not promise otherwise. Paths are resolved, because spec.snapshots.dir may be
// a symlink in either direction.
func TestSnapshotDirNote(t *testing.T) {
	root := t.TempDir() // a symlinked parent on macOS (/var -> /private/var), on purpose
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	link := func(oldname, name string) string {
		p := filepath.Join(root, name)
		if err := os.Symlink(oldname, p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	target := mk("var-lib-stratum")
	inside := mk("var-lib-stratum", "snapshots")
	outside := mk("backups")
	targets := []string{target, filepath.Join(root, "etc-stratum")}
	realLib := mk("real-lib")
	linkLib := link("real-lib", "lib")
	elsewhere := mk("elsewhere")

	const (
		removed = "WOULD be removed"
		kept    = "NOT part of this set"
		unknown = "is unknown"
	)
	for _, tc := range []struct {
		name string
		dir  string
		want string
		// targets overrides the shared set.
		targets []string
	}{
		{"a dir outside every target", outside, kept, nil},
		{"a dir sharing a prefix, not a parent", mk("var-lib-stratum-snaps"), kept, nil},
		{"a dir inside a target", inside, removed, nil},
		{"the target itself", target, removed, nil},
		{"a dir not created yet, inside a target", filepath.Join(target, "later", "snaps"), removed, nil},
		{"a dir not created yet, outside the targets", filepath.Join(outside, "later"), kept, nil},
		{"a symlink outside that points into a target", link(inside, "into"), removed, nil},
		{"a symlink inside a target that points outside", link(outside, filepath.Join("var-lib-stratum", "out")), kept, nil},
		// Its target can appear later, inside a removal target, so no verdict.
		{"a dangling symlink", link(filepath.Join(target, "later"), "dangling"), unknown, nil},
		{"a path through a file", filepath.Join(mkFile(t, root, "plain"), "snaps"), unknown, nil},
		// RemoveAll follows every parent of a target, but not its last component.
		{"a target reached through a symlinked parent", filepath.Join(realLib, "stratum", "snaps"), removed,
			[]string{filepath.Join(linkLib, "stratum")}},
		{"a target that is itself a symlink", filepath.Join(elsewhere, "snaps"), kept,
			[]string{link(elsewhere, "tgt")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := targets
			if tc.targets != nil {
				ts = tc.targets
			}
			note := snapshotDirNote(tc.dir, ts)
			if !strings.Contains(note, tc.want) {
				t.Errorf("snapshotDirNote(%q) = %q, want it to say %q", tc.dir, note, tc.want)
			}
			if tc.want != removed && strings.Contains(note, removed) || tc.want != kept && strings.Contains(note, kept) {
				t.Errorf("snapshotDirNote(%q) = %q, says more than %q", tc.dir, note, tc.want)
			}
			if !strings.Contains(note, tc.dir) {
				t.Errorf("snapshotDirNote(%q) does not name the dir: %q", tc.dir, note)
			}
		})
	}
}

func mkFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUninstallPreviewWarnsWhenSnapshotDirIsRemoved(t *testing.T) {
	fakeStratumHost(t)
	stubSnapshotDir(t)
	dir := filepath.Join(version.DataDir, "snapshots")
	// The note reports on the directory the resolved snapshot settings
	// name, so the dir inside the removal target reaches the handler
	// through loadSnapshotConfig as well as through snapshotDir.
	cfg := etcdsnap.DefaultConfig()
	cfg.Dir = dir
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return cfg, nil }
	snapshotDir = func() string { return dir }

	_, out, err := UninstallPreviewHandler()(context.Background(), nil, UninstallPreviewIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Note, "WOULD be removed") || strings.Contains(out.Note, "NOT part of this set") {
		t.Errorf("note %q must warn that %s is deleted by an uninstall", out.Note, dir)
	}
}

// The preview's snapshot note is about the directory the resolved snapshot
// settings name — the same one snapshot_list lists — not about a directory the
// handler resolved on its own.
func TestUninstallPreview_NoteNamesTheResolvedDir(t *testing.T) {
	fakeStratumHost(t)

	resolved := t.TempDir()
	oldCfg, oldDir := loadSnapshotConfig, snapshotDir
	t.Cleanup(func() { loadSnapshotConfig, snapshotDir = oldCfg, oldDir })
	loadSnapshotConfig = func() (etcdsnap.Config, error) { return etcdsnap.Config{Retain: 5, Dir: resolved}, nil }
	snapshotDir = func() string { return filepath.Join(resolved, "unread") }

	_, out, err := UninstallPreviewHandler()(context.Background(), nil, UninstallPreviewIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Note, resolved) {
		t.Errorf("note %q does not name the resolved dir %s", out.Note, resolved)
	}
	if strings.Contains(out.Note, filepath.Join(resolved, "unread")) {
		t.Errorf("note %q names a directory the resolved settings do not", out.Note)
	}
}

// TestUninstallPreviewReturnsTheSharedConditionals pins the MCP side of the one
// preview: uninstall_preview's structured result carries the
// conditional effects — the legacy containerd config, the firewalld bindings,
// the registered SELinux contexts and the netns/cilium/cgroup/legacy-kubelet
// layout — as uninstall.Preview() returns them, so the tool and the CLI dry run
// enumerate the same effects because both render the same function's value.
// The probes are stubbed through uninstall's PreviewProbes seam; the handler
// must render what that seam answers, not a second gathering of its own.
func TestUninstallPreviewReturnsTheSharedConditionals(t *testing.T) {
	fakeStratumHost(t)
	stubSnapshotDir(t)

	saved := uninstall.PreviewProbes
	t.Cleanup(func() { uninstall.PreviewProbes = saved })
	uninstall.PreviewProbes.LegacyHostConfig = func() (string, bool) { return "/etc/containerd/config.toml", true }
	uninstall.PreviewProbes.TrustedPodCIDRs = func() ([]string, error) { return []string{"10.245.0.0/16"}, nil }
	uninstall.PreviewProbes.OwnedFirewalldInterfaces = func() ([]string, error) { return []string{"lxc+"}, nil }
	uninstall.PreviewProbes.SELinuxRegistered = func() bool { return true }

	_, out, err := UninstallPreviewHandler()(context.Background(), nil, UninstallPreviewIn{})
	if err != nil {
		t.Fatal(err)
	}
	if want := uninstall.Preview().Items; !reflect.DeepEqual(out.Conditionals, want) {
		t.Errorf("uninstall_preview conditionals = %+v, want exactly uninstall.Preview()'s items %+v; the tool must report the shared preview's effects, not gather its own", out.Conditionals, want)
	}
	// Behaviorally: what the stubbed host carries is in the structured output.
	for _, want := range []uninstall.PreviewItem{
		{Kind: "legacy-containerd-config", Path: "/etc/containerd/config.toml"},
		{Kind: "firewalld-source", Path: "10.245.0.0/16"},
		{Kind: "firewalld-interface", Path: "lxc+"},
		{Kind: "selinux-contexts"},
	} {
		found := false
		for _, got := range out.Conditionals {
			if got.Kind == want.Kind && got.Path == want.Path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("uninstall_preview conditionals %+v lack the %q item for %q the CLI dry run discloses", out.Conditionals, want.Kind, want.Path)
		}
	}
}

// TestCredentialSources_ReadTools pins the separate credential domains:
// token_list uses the shared node connector pinned to the node's own admin
// credential — super-admin.conf where the node has one, admin.conf until its
// first new-release `stratum up` writes it — while upgrade_plan continues to
// use the configurable connector. Selecting a remote cluster must never
// redirect a node-local token read, and the node credential must never be the
// server's user-facing one.
func TestCredentialSources_ReadTools(t *testing.T) {
	fakeStratumHost(t)

	// recordingServer answers every request with body and records the path
	// it was requested under.
	recordingServer := func(body string) (*httptest.Server, func() []string) {
		var mu sync.Mutex
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), paths...)
		}
	}
	secrets := `{"apiVersion":"v1","kind":"SecretList","metadata":{},"items":[]}`
	nodes := `{"apiVersion":"v1","kind":"NodeList","metadata":{},"items":[` +
		`{"metadata":{"name":"cp1","labels":{"stratum.ciq.com/control-plane":"true"}},` +
		`"status":{"nodeInfo":{"kubeletVersion":"v1.30.0"},` +
		`"conditions":[{"type":"Ready","status":"True"}]}}]}`
	serverA, aPaths := recordingServer(secrets)
	serverB, bPaths := recordingServer(nodes)
	serverC, cPaths := recordingServer(secrets)

	// A is the node connector's explicit admin.conf, independent of the
	// generic connector's selected remote cluster.
	oldConf := version.AdminConf
	version.AdminConf = kubetest.Kubeconfig(t, &rest.Config{Host: serverA.URL})
	t.Cleanup(func() { version.AdminConf = oldConf })
	// B is the server's --kubeconfig. $KUBECONFIG points there too, so a
	// connector built the way pkg/cli builds the server's resolves B, not A.
	bConf := kubetest.Kubeconfig(t, &rest.Config{Host: serverB.URL})
	t.Setenv("KUBECONFIG", bConf)
	// C is super-admin.conf: the credential Stratum's own clients use when
	// the node has one. The directory redirect keeps the test off the real
	// node's paths.
	oldKubeconfigDir := version.KubeconfigDir
	version.KubeconfigDir = t.TempDir()
	t.Cleanup(func() { version.KubeconfigDir = oldKubeconfigDir })

	// serverConnector mirrors pkg/cli's generic connector: no explicit
	// kubeconfig, so resolution follows $KUBECONFIG and falls back to
	// admin.conf. nodeConnector mirrors its node connector: the node's own
	// admin credential, super-admin.conf where the node has one, admin.conf
	// until then.
	serverConnector := func() *kube.Connector {
		return kube.NewConnector(kube.Options{FallbackKubeconfig: version.AdminConf})
	}
	nodeConnector := func() *kube.Connector {
		return kube.NewConnector(kube.Options{KubeconfigPath: k8sutil.InternalAdminConf})
	}

	t.Run("token_list stays on this node despite the selected remote cluster", func(t *testing.T) {
		// The selector resolves against the redirected node paths per call.
		// No super-admin.conf in the redirected
		// directory, so the node credential is admin.conf — A, never B.
		before := len(bPaths())
		handler := TokenListHandler(nodeConnector())
		if _, _, err := handler(context.Background(), nil, TokenListIn{}); err != nil {
			t.Fatal(err)
		}
		var saw bool
		for _, p := range aPaths() {
			if p == "/api/v1/namespaces/kube-system/secrets" {
				saw = true
			}
		}
		if !saw {
			t.Errorf("token_list never reached this node's credential; A saw %v", aPaths())
		}
		if got := bPaths(); len(got) != before {
			t.Errorf("token_list followed the selected remote cluster: %v", got[before:])
		}
	})

	// Stratum's own clients use super-admin.conf once the node has one, and
	// token_list is one of them: a revocable admin.conf must not be the
	// credential that reads kube-system on a node that holds the real one.
	t.Run("token_list notices super-admin.conf on the same connector", func(t *testing.T) {
		handler := TokenListHandler(nodeConnector())
		if _, _, err := handler(t.Context(), nil, TokenListIn{}); err != nil {
			t.Fatal(err)
		}
		superConf := filepath.Join(version.KubeconfigDir, "super-admin.conf")
		superData, err := os.ReadFile(kubetest.Kubeconfig(t, &rest.Config{Host: serverC.URL}))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(superConf, superData, 0o600); err != nil {
			t.Fatal(err)
		}
		beforeA, beforeB := len(aPaths()), len(bPaths())
		if _, _, err := handler(context.Background(), nil, TokenListIn{}); err != nil {
			t.Fatal(err)
		}
		var saw bool
		for _, p := range cPaths() {
			if p == "/api/v1/namespaces/kube-system/secrets" {
				saw = true
			}
		}
		if !saw {
			t.Errorf("token_list never reached super-admin.conf; C saw %v", cPaths())
		}
		if got := aPaths(); len(got) != beforeA {
			t.Errorf("token_list fell back to admin.conf beside a super-admin.conf; A saw %v", got[beforeA:])
		}
		if got := bPaths(); len(got) != beforeB {
			t.Errorf("token_list resolved through the server's credential; B saw %v", got[beforeB:])
		}
	})

	t.Run("missing node credentials never fall through to the remote cluster", func(t *testing.T) {
		before := len(bPaths())
		handler := TokenListHandler(kube.NewConnector(kube.Options{
			Kubeconfig: filepath.Join(t.TempDir(), "missing-admin.conf"),
		}))
		if _, _, err := handler(t.Context(), nil, TokenListIn{}); err == nil {
			t.Error("token_list succeeded without this node's credentials")
		}
		if got := bPaths(); len(got) != before {
			t.Errorf("missing admin.conf redirected token_list to the remote cluster: %v", got[before:])
		}
	})

	// The node credential also stays local when no generic credential was selected.
	t.Run("token_list uses admin.conf with no --kubeconfig or $KUBECONFIG", func(t *testing.T) {
		kubetest.IsolateHome(t) // clears $KUBECONFIG and any ~/.kube/config
		before := len(bPaths())
		handler := TokenListHandler(kube.NewConnector(kube.Options{Kubeconfig: version.AdminConf}))
		if _, _, err := handler(context.Background(), nil, TokenListIn{}); err != nil {
			t.Fatal(err)
		}
		var saw bool
		for _, p := range aPaths() {
			if p == "/api/v1/namespaces/kube-system/secrets" {
				saw = true
			}
		}
		if !saw {
			t.Errorf("token_list never reached the node credential; A saw %v", aPaths())
		}
		if got := bPaths(); len(got) != before {
			t.Errorf("token_list skipped the node credential: %v", got[before:])
		}
	})

	t.Run("upgrade_plan reads the connector it is given", func(t *testing.T) {
		before := aPaths()
		conn := serverConnector()
		_, out, err := UpgradePlanHandler(conn)(context.Background(), nil, UpgradePlanIn{Target: "v1.31.0"})
		if err != nil {
			t.Fatal(err)
		}
		if out.NodeCount != 1 {
			t.Errorf("nodeCount = %d, want the one node B served; out %+v", out.NodeCount, out)
		}
		var saw bool
		for _, p := range bPaths() {
			if p == "/api/v1/nodes" {
				saw = true
			}
		}
		if !saw {
			t.Errorf("upgrade_plan never listed nodes through the connector; B saw %v", bPaths())
		}
		if got := aPaths(); len(got) != len(before) {
			t.Errorf("upgrade_plan used the node's admin credential: %v", got)
		}
	})
}

// TestCertsCheckSchemaStatesTheRenewalWindow: the certs_check schema tells a
// model the default warning window in prose. A window change that misses this
// string leaves the tool advertising the old default, and callers that read
// the schema instead of the code plan around the wrong expiry.
func TestCertsCheckSchemaStatesTheRenewalWindow(t *testing.T) {
	f, ok := reflect.TypeOf(CertsCheckIn{}).FieldByName("WarnDays")
	if !ok {
		t.Fatal("CertsCheckIn has no WarnDays field")
	}
	want := strconv.Itoa(version.CertRenewWindowDays)
	got := f.Tag.Get("jsonschema")
	match := regexp.MustCompile(`\bdefaults to ([0-9]+)\b`).FindStringSubmatch(got)
	if len(match) != 2 || match[1] != want {
		t.Errorf("jsonschema tag %q does not state the renewal window %s", got, want)
	}
}

// TestUninstallPreviewSchemaNamesTheLocalPathDir: the uninstall preview
// schema tells a model which fixed layout items its conditionals carry. The
// local-path directory is one of them, so a schema that leaves it out has the
// tool describe a preview without the item it returns.
func TestUninstallPreviewSchemaNamesTheLocalPathDir(t *testing.T) {
	f, ok := reflect.TypeOf(UninstallPreviewOut{}).FieldByName("Conditionals")
	if !ok {
		t.Fatal("UninstallPreviewOut has no Conditionals field")
	}
	const want = "the netns, Cilium, cgroup, legacy-kubelet and local-path directory layout"
	if got := f.Tag.Get("jsonschema"); !strings.Contains(got, want) {
		t.Errorf("jsonschema tag %q does not name the fixed layout items %q", got, want)
	}
}

// TestCertsCheckServedSchemaStatesTheRenewalWindow pins the schema text a
// client's tools/list actually shows for warn_days. The tag-level pin above
// reads the struct tag; this pins the description inferred from it at
// registration — the artifact a model reads when it decides how to call the
// tool — so no registration-side change can drop or rewrite the window while
// the tag still holds it.
func TestCertsCheckServedSchemaStatesTheRenewalWindow(t *testing.T) {
	cs := fencedSession(t, func(s *sdk.Server) {
		sdk.AddTool(s, &sdk.Tool{Name: "certs_check", Description: "Report certificate expiry."}, CertsCheckHandler())
	})
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var schema *struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	for i := range res.Tools {
		if res.Tools[i].Name != "certs_check" {
			continue
		}
		b, err := json.Marshal(res.Tools[i].InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatalf("certs_check's served input schema does not parse: %v:\n%s", err, b)
		}
		break
	}
	if schema == nil {
		t.Fatalf("tools/list has no certs_check tool: %+v", res.Tools)
	}
	warnDays, ok := schema.Properties["warn_days"]
	if !ok {
		t.Fatal("certs_check's served input schema has no warn_days property")
	}
	want := "defaults to " + strconv.Itoa(version.CertRenewWindowDays)
	if !strings.Contains(warnDays.Description, want) {
		t.Errorf("served warn_days description %q does not state the renewal window (%q)", warnDays.Description, want)
	}
}

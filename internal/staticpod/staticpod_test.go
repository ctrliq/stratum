package staticpod

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/version"
)

// testNode redirects the version paths to a temp dir and persists a controller
// node state there, so LoadState-based verbs (Start, Restart) see it.
func testNode(t *testing.T) *nodestate.NodeState {
	t.Helper()
	version.K8sConfDir = t.TempDir()
	st := &nodestate.NodeState{
		Role: "controller", Hostname: "cp1", NodeIP: "192.0.2.10",
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12",
		EtcdName: "cp1", EtcdInitialCluster: "cp1=https://192.0.2.10:2380", EtcdState: "new",
	}
	if err := nodestate.SaveState(st); err != nil {
		t.Fatal(err)
	}
	return st
}

func readPod(t *testing.T, name string) (*corev1.Pod, []byte) {
	t.Helper()
	b, err := os.ReadFile(Path(name))
	if err != nil {
		t.Fatalf("manifest %s: %v", name, err)
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal(b, &pod); err != nil {
		t.Fatalf("manifest %s does not parse as a Pod: %v", name, err)
	}
	return &pod, b
}

// TestWriteAllIdempotent: repeated renders are byte-identical (an unnecessary
// content change would make kubelet restart the pod on every controller start),
// and a restarted-at annotation left by Restart survives the next WriteAll.
func TestWriteAllIdempotent(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	_, first := readPod(t, "kube-apiserver")
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	_, second := readPod(t, "kube-apiserver")
	if string(first) != string(second) {
		t.Error("re-render of an unchanged node produced different manifest bytes — kubelet would restart the pod on every controller start")
	}

	if err := Restart("kube-apiserver"); err != nil {
		t.Fatal(err)
	}
	restarted, _ := readPod(t, "kube-apiserver")
	stamp := restarted.Annotations[RestartedAtAnnotation]
	if stamp == "" {
		t.Fatal("Restart left no restarted-at annotation")
	}
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	after, _ := readPod(t, "kube-apiserver")
	if after.Annotations[RestartedAtAnnotation] != stamp {
		t.Errorf("WriteAll erased the restarted-at annotation (%q -> %q) — that content change bounces the pod on every controller start",
			stamp, after.Annotations[RestartedAtAnnotation])
	}

	// No VIP in the state: no kube-vip manifest.
	if Exists("kube-vip") {
		t.Error("kube-vip manifest rendered without a VIP in the node state")
	}
}

// TestRestartChangesOnlyAnnotation: the restart verb is an annotation bump,
// nothing else — same command, same mounts, same everything.
func TestRestartChangesOnlyAnnotation(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	before, _ := readPod(t, "kube-scheduler")
	if err := Restart("kube-scheduler"); err != nil {
		t.Fatal(err)
	}
	after, _ := readPod(t, "kube-scheduler")

	if before.Annotations[RestartedAtAnnotation] == after.Annotations[RestartedAtAnnotation] {
		t.Error("Restart did not change the restarted-at annotation — kubelet sees no content change and never restarts the pod")
	}
	delete(after.Annotations, RestartedAtAnnotation)
	if len(after.Annotations) == 0 {
		after.Annotations = nil
	}
	ab, _ := yaml.Marshal(after)
	bb, _ := yaml.Marshal(before)
	if string(ab) != string(bb) {
		t.Errorf("Restart changed more than the annotation:\nbefore:\n%s\nafter:\n%s", bb, ab)
	}
}

// TestEtcdMembershipRerenders: `stratum snapshot rejoin/restore` patches the
// node state and Starts etcd; the manifest must carry the PATCHED membership
// flags (the static-pod equivalent of the supervisor's ArgsFn semantics).
func TestEtcdMembershipRerenders(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	st.EtcdInitialCluster = "cp1=https://192.0.2.10:2380,cp2=https://192.0.2.11:2380"
	st.EtcdState = "existing"
	if err := nodestate.SaveState(st); err != nil {
		t.Fatal(err)
	}
	if err := Start("etcd"); err != nil {
		t.Fatal(err)
	}
	pod, _ := readPod(t, "etcd")
	cmd := strings.Join(pod.Spec.Containers[0].Command, " ")
	if !strings.Contains(cmd, "cp2=https://192.0.2.11:2380") {
		t.Errorf("etcd manifest does not carry the patched membership:\n%s", cmd)
	}
	if !strings.Contains(cmd, "--initial-cluster-state existing") {
		t.Errorf("etcd manifest does not carry the patched cluster state:\n%s", cmd)
	}
}

// TestControllerManagerEnablesBootstrapSigner is the regression test for the bootstrapsigner.
//
// bootstrapsigner is disabled by default in kube-controller-manager, so
// unless an explicit --controllers value turns it on
// the control plane never publishes the detached JWS over cluster-info — and
// bootstrap.DiscoverCA then hard-fails every join rather than install a CA it
// cannot authenticate. Asserted against the RENDERED manifest, not the constant:
// the constant holding the right value while nothing renders it is exactly the
// failure this exists to catch.
func TestControllerManagerEnablesBootstrapSigner(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	pod, _ := readPod(t, "kube-controller-manager")

	var got string
	for _, a := range pod.Spec.Containers[0].Command {
		if strings.HasPrefix(a, "--controllers=") {
			got = a
		}
	}
	if got == "" {
		t.Fatalf("kube-controller-manager renders no --controllers flag, so bootstrapsigner stays "+
			"off, cluster-info is never signed, and every join fails closed.\ncommand: %v", pod.Spec.Containers[0].Command)
	}
	if got != version.ControllerManagerControllersFlag {
		t.Errorf("--controllers = %q, want %q", got, version.ControllerManagerControllersFlag)
	}
	if !strings.Contains(got, "bootstrapsigner") {
		t.Errorf("--controllers does not enable bootstrapsigner: %q", got)
	}
}

// TestControllerManagerSignsForTheCAValidity pins the controller-manager's CSR
// signing duration to the CA lifetime, so the kubelet client certs it signs do
// not expire after the one-year default.
func TestControllerManagerSignsForTheCAValidity(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	pod, _ := readPod(t, "kube-controller-manager")

	var got []string
	for _, a := range pod.Spec.Containers[0].Command {
		if strings.HasPrefix(a, "--cluster-signing-duration=") {
			got = append(got, a)
		}
	}
	if len(got) != 1 {
		t.Fatalf("--cluster-signing-duration rendered %d times, want once: %v", len(got), got)
	}
	d, err := time.ParseDuration(strings.TrimPrefix(got[0], "--cluster-signing-duration="))
	if err != nil {
		t.Fatalf("%s: %v", got[0], err)
	}
	if d != pki.CAValidity {
		t.Errorf("%s signs for %v, want pki.CAValidity (%v)", got[0], d, pki.CAValidity)
	}
}

// TestEtcdTrustsOnlyItsOwnCA is the regression test for the etcd trust hole (etcd accepting
// any client certificate signed by the cluster root, which also signs every
// kubelet's certificate): the flags that actually decide the vulnerability.
//
// etcd authorizes a client by chaining its certificate to
// --trusted-ca-file. Pointed at the shared cluster bundle, EVERY
// certificate the cluster root signed is a valid etcd client — including every
// kubelet's — so any node with a kubelet cert has full read/write on cluster
// state. --peer-trusted-ca-file is the same authority for the raft peer port,
// and the apiserver's --etcd-cafile is the client half of the split. Asserted
// on the rendered manifests, negatively as well as positively: pointing these
// back at ca-bundle.crt is a three-character edit that reopens it in full.
func TestEtcdTrustsOnlyItsOwnCA(t *testing.T) {
	const etcdBundle = "etcd-ca-bundle.crt"
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	etcdPod, _ := readPod(t, "etcd")
	etcdCmd := etcdPod.Spec.Containers[0].Command

	valueOf := func(args []string, flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, flag+"="); ok {
				return v
			}
		}
		return ""
	}

	for _, flag := range []string{"--trusted-ca-file", "--peer-trusted-ca-file"} {
		got := valueOf(etcdCmd, flag)
		if got == "" {
			t.Errorf("etcd renders no %s at all — it would authorize clients against its compiled-in default", flag)
			continue
		}
		if filepath.Base(got) != etcdBundle {
			t.Errorf("etcd %s = %q, want a path ending in %q; any CA in that file is full etcd read/write",
				flag, got, etcdBundle)
		}
		if filepath.Base(got) == "ca-bundle.crt" {
			t.Errorf("etcd %s points at the shared cluster trust bundle — every kubelet certificate "+
				"is a valid etcd client again", flag)
		}
	}

	apiPod, _ := readPod(t, "kube-apiserver")
	apiCA := valueOf(apiPod.Spec.Containers[0].Command, "--etcd-cafile")
	if filepath.Base(apiCA) != etcdBundle {
		t.Errorf("apiserver --etcd-cafile = %q, want a path ending in %q — the apiserver must verify "+
			"etcd against etcd's own CA, not the Kubernetes root", apiCA, etcdBundle)
	}
}

// TestEtcdPeerPortAcceptsOnlyTheEtcdLeaf: the peer port trusts the same
// bundle as the client port, so without --peer-cert-allowed-cn the
// apiserver-etcd-client leaf, or any leaf the bundle's CAs sign, is a raft
// peer credential.
func TestEtcdPeerPortAcceptsOnlyTheEtcdLeaf(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	etcdPod, _ := readPod(t, "etcd")
	var got []string
	cmd := etcdPod.Spec.Containers[0].Command
	for i, a := range cmd {
		if a == "--peer-cert-allowed-cn" && i+1 < len(cmd) {
			got = append(got, cmd[i+1])
		}
		if v, ok := strings.CutPrefix(a, "--peer-cert-allowed-cn="); ok {
			got = append(got, v)
		}
	}
	if len(got) != 1 || got[0] != pki.EtcdCN {
		t.Fatalf("etcd --peer-cert-allowed-cn = %q, want exactly [%q]", got, pki.EtcdCN)
	}
	for _, sp := range pki.LeafSpecs("cp1", "10.0.0.1", nil) {
		if sp.Name == "etcd" && sp.CN != pki.EtcdCN {
			t.Errorf("the etcd leaf is minted with CN %q, but its peers accept only %q", sp.CN, pki.EtcdCN)
		}
		if sp.Name != "etcd" && sp.CN == pki.EtcdCN {
			t.Errorf("leaf %s carries CN %q, so etcd's peer port accepts it", sp.Name, sp.CN)
		}
	}
}

// TestApiserverVerifiesKubeletServingCertsInEveryProfile: without
// --kubelet-certificate-authority the apiserver accepts any certificate a
// kubelet presents, so a compromised node can forge one and intercept the
// apiserver's exec and log streams to it. It must not depend on --cis.
func TestApiserverVerifiesKubeletServingCertsInEveryProfile(t *testing.T) {
	for _, cis := range []bool{false, true} {
		st := &nodestate.NodeState{Role: "controller", NodeIP: "10.0.0.1", CIS: cis}
		var got []string
		for _, a := range apiserverArgs(st, profiletest.For(cis), func(f string) string { return f }) {
			if v, ok := strings.CutPrefix(a, "--kubelet-certificate-authority="); ok {
				got = append(got, v)
			}
		}
		if len(got) != 1 || got[0] != "ca-bundle.crt" {
			t.Errorf("CIS=%v: --kubelet-certificate-authority = %q, want exactly one, ca-bundle.crt", cis, got)
		}
	}
}

// TestKubeVipArgs is a golden test on the kube-vip argv: every flag below was
// verified against the pinned image's `manager --help` (build/images.txt). A
// generated flag the pinned binary does not know is a crashloop, not a warning
// — kube-vip exits on "unknown flag" and the VIP never comes up.
// If this fails after a kube-vip bump, re-check the args against the new
// --help before updating the expectation.
func TestKubeVipArgs(t *testing.T) {
	base := []string{
		"manager",
		"--address", "192.0.2.40",
		"--port", "6443",
		"--controlplane",
		"--leaderElection",
		"--prometheusHTTPServer", "",
		"--k8sConfigPath", version.KubeVipKubeconfig(),
	}
	for _, tc := range []struct {
		name string
		vip  nodestate.VIP
		want []string
	}{
		// No --interface at all: kube-vip auto-detects the default-gateway
		// interface. There is no flag that asks for that.
		{"auto interface", nodestate.VIP{Address: "192.0.2.40"}, append(base, "--arp")},
		{"explicit interface", nodestate.VIP{Address: "192.0.2.40", Interface: "eth0"},
			append(base, "--interface", "eth0", "--arp")},
		{"bgp", nodestate.VIP{Address: "192.0.2.40", BGP: true}, append(base, "--bgp")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := kubeVipArgs(&nodestate.NodeState{VIP: tc.vip})
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("kube-vip argv\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestPutAndRead: Put writes a manifest byte for byte, atomically (no temp file
// is left for kubelet to parse), and Read returns it; Read of a component with
// no manifest is nil with no error.
func TestPutAndRead(t *testing.T) {
	testNode(t)
	if b, err := Read("etcd"); b != nil || err != nil {
		t.Fatalf("Read of a missing manifest = %q, %v; want nil, nil", b, err)
	}
	want := []byte("kind: Pod\nmetadata:\n  name: etcd\n")
	if err := Put("etcd", want); err != nil {
		t.Fatal(err)
	}
	got, err := Read("etcd")
	if err != nil || string(got) != string(want) {
		t.Fatalf("Read after Put = %q, %v; want %q", got, err, want)
	}
	entries, err := os.ReadDir(Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "etcd.yaml" {
			t.Errorf("Put left %s in the manifest dir", e.Name())
		}
	}
}

// TestApiserverKeepsTheDefaultEtcdPrefix pins one half of ha.JoinBundleEtcdKey's
// coupling to the apiserver's storage layout: that key is spelled under
// kube-apiserver's default --etcd-prefix, /registry, so an apiserver given
// another prefix would store the stratum-certs bundle where the CIS key gate
// never looks, and the gate would read every cluster as having none.
func TestApiserverKeepsTheDefaultEtcdPrefix(t *testing.T) {
	for _, cis := range []bool{false, true} {
		st := &nodestate.NodeState{Role: "controller", NodeIP: "10.0.0.1", CIS: cis}
		for _, a := range apiserverArgs(st, profiletest.For(cis), func(f string) string { return f }) {
			if strings.HasPrefix(a, "--etcd-prefix") {
				t.Fatalf("the apiserver sets %s; ha.JoinBundleEtcdKey assumes the default /registry", a)
			}
		}
	}
}

// TestRestartAsIsKeepsTheManifestOnDisk: an upgrade rollback restores
// older manifests and runs the newer binary, so a restart that re-rendered
// them would put the newer images back. Only the annotation may change, and a
// field this binary's Pod type does not know must survive.
func TestRestartAsIsKeepsTheManifestOnDisk(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kube-controller-manager", "kube-scheduler"} {
		b, err := os.ReadFile(Path(name))
		if err != nil {
			t.Fatal(err)
		}
		current := readImage(t, name)
		old := strings.ReplaceAll(string(b), current, "registry.invalid/old-"+name+":v1.36.0")
		old += "futureField: kept\n"
		if err := os.WriteFile(Path(name), []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := RestartAsIs(name); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(Path(name))
		if err != nil {
			t.Fatal(err)
		}
		if got := readImage(t, name); got != "registry.invalid/old-"+name+":v1.36.0" {
			t.Errorf("%s: image = %q, the restored manifest's image was replaced", name, got)
		}
		if !strings.Contains(string(after), "futureField: kept") {
			t.Errorf("%s: a field this binary does not know was dropped:\n%s", name, after)
		}
		pod, _ := readPod(t, name)
		if pod.Annotations[RestartedAtAnnotation] == "" {
			t.Errorf("%s: no restarted-at annotation, so kubelet sees no change and never restarts the pod", name)
		}
	}
}

func TestRestartAsIsWithoutAManifestFails(t *testing.T) {
	testNode(t)
	if err := RestartAsIs("kube-scheduler"); err == nil {
		t.Error("want an error for a component with no manifest")
	}
}

// Every etcd listen and advertise URL must bracket IPv6 addresses, matching
// the peer URL recorded in cluster membership.
func TestEtcdIPv6ListenAndAdvertiseURLs(t *testing.T) {
	st := &nodestate.NodeState{NodeIP: "fd00::9", EtcdName: "cp1"}
	args := etcdArgs(st)
	for flag, want := range map[string]string{
		"--listen-client-urls":          "https://127.0.0.1:2379,https://[fd00::9]:2379",
		"--advertise-client-urls":       "https://[fd00::9]:2379",
		"--listen-peer-urls":            "https://[fd00::9]:2380",
		"--initial-advertise-peer-urls": "https://[fd00::9]:2380",
	} {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("etcd args missing %s", flag)
		}
		if got := args[i+1]; got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
}

// TestEtcdAdvertisesTheNodeAddress pins etcd's IPv4 peer and client URLs for
// the node's own address. MemberRemove compares the peer URL byte for byte
// with the URL the member was added under, so a changed format here strands
// every rollback of a failed join. Derived from the same state testNode
// persists, so the rendered manifest and these flags cannot drift apart.
func TestEtcdAdvertisesTheNodeAddress(t *testing.T) {
	st := testNode(t)
	want := map[string]string{
		"--listen-client-urls":          "https://127.0.0.1:2379,https://192.0.2.10:2379",
		"--advertise-client-urls":       "https://192.0.2.10:2379",
		"--listen-peer-urls":            "https://192.0.2.10:2380",
		"--initial-advertise-peer-urls": "https://192.0.2.10:2380",
	}
	valueOf := func(args []string, flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	got := etcdArgs(st)
	for flag, wantURL := range want {
		if gotURL := valueOf(got, flag); gotURL != wantURL {
			t.Errorf("etcd %s = %q, want %q", flag, gotURL, wantURL)
		}
	}
}

// TestManifestWriteStagesUnderAHiddenName pins the staging rule of
// writeFile: kubelet parses EVERY non-hidden file in the manifest directory,
// so a visible staging name would let it read a half-written manifest as a
// pod. A directory at the destination makes the final rename fail, and the
// staging path is read back from the returned *os.LinkError.
func TestManifestWriteStagesUnderAHiddenName(t *testing.T) {
	testNode(t)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(Path("etcd"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Put("etcd", []byte("kind: Pod\n"))
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) {
		t.Fatalf("Put with a blocked destination = %v, want an *os.LinkError from the failed rename", err)
	}
	staging := linkErr.Old // the rename's source is the staging path
	if !strings.HasPrefix(filepath.Base(staging), ".") {
		t.Errorf("staging file %q is not hidden — kubelet would parse the half-written manifest as a pod", staging)
	}
	if filepath.Dir(staging) != Dir() {
		t.Errorf("staging file %q is not in the manifest dir %q", staging, Dir())
	}
}

// TestEveryStaticPodHasAListenerOrIsDeclaredListenerless pins the relation
// between Names and localPort: the upgrade's gate waits on Active, which
// reports false for a component with no listener entry, so a new static pod
// without one would make every upgrade wait three minutes and roll back.
// kube-vip is the one declared listenerless component today.
func TestEveryStaticPodHasAListenerOrIsDeclaredListenerless(t *testing.T) {
	for _, n := range Names {
		if _, ok := localPort[n]; !ok && n != "kube-vip" {
			t.Errorf("static pod %q has no entry in localPort and is not declared listenerless — the upgrade gate would wait on a listener it can never see", n)
		}
	}
	for n := range localPort {
		if !slices.Contains(Names, n) {
			t.Errorf("localPort names %q, which is not in Names — no manifest renders that listener", n)
		}
	}
}

// TestNamesForAppliesTheKubeVipRuleOnce pins the one place the VIP rule
// lives: kube-vip is in the roster exactly when the node state carries a VIP,
// every other component unconditionally, in Names' order. The upgrade's
// re-render order and the restore's restart set derive from this, so a copy
// of the rule restated beside it fails here first.
func TestNamesForAppliesTheKubeVipRuleOnce(t *testing.T) {
	withVIP := &nodestate.NodeState{Role: "controller", VIP: nodestate.VIP{Address: "192.0.2.100"}}
	if got := NamesFor(withVIP); !slices.Equal(got, Names) {
		t.Errorf("NamesFor(with a VIP) = %q, want the full roster %q — kube-vip renders wherever a VIP is configured", got, Names)
	}
	withoutVIP := &nodestate.NodeState{Role: "controller"}
	var want []string
	for _, n := range Names {
		if n != "kube-vip" {
			want = append(want, n)
		}
	}
	if got := NamesFor(withoutVIP); !slices.Equal(got, want) {
		t.Errorf("NamesFor(no VIP) = %q, want %q — kube-vip is excluded exactly when the state carries no VIP", got, want)
	}
}

// TestNamesForGivesTheCallerItsOwnSlice: callers append to and reorder the
// roster they are handed, so a result aliasing the Names var would let one
// caller corrupt the roster every other reader sees.
func TestNamesForGivesTheCallerItsOwnSlice(t *testing.T) {
	st := &nodestate.NodeState{Role: "controller", VIP: nodestate.VIP{Address: "192.0.2.100"}}
	got := NamesFor(st)
	got[0] = "mutated"
	if again := NamesFor(st); again[0] != "etcd" {
		t.Errorf("mutating one NamesFor result changed the next one (%q) — the result aliases a shared roster", again)
	}
}

// TestWriteAllRendersExactlyNamesFor ties the roster to the render: after
// WriteAll, the manifest directory holds exactly the components NamesFor
// names, with and without a VIP. The rule cannot drift between what is
// rendered and the roster other code derives from it.
func TestWriteAllRendersExactlyNamesFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		vip  string
	}{
		{"with a VIP", "192.0.2.100"},
		{"without a VIP", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testNode(t)
			st.VIP.Address = tc.vip
			if err := WriteAll(st); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(Dir())
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range entries {
				if component, ok := ManifestName(e.Name()); ok {
					got = append(got, component)
				}
			}
			if !slices.Equal(got, NamesFor(st)) {
				t.Errorf("after WriteAll (%s) the manifest directory holds %q, want exactly NamesFor's %q", tc.name, got, NamesFor(st))
			}
		})
	}
}

// TestHasListener reports which components own a localhost listener: every
// roster component but kube-vip (its metrics server is disabled), and no name
// outside the roster. The upgrade's gate waits on exactly the components
// HasListener accepts, so a listenerless component reported as having a
// listener would make every upgrade wait out its gate timeout and roll back.
func TestHasListener(t *testing.T) {
	for _, n := range Names {
		want := n != "kube-vip"
		if got := HasListener(n); got != want {
			t.Errorf("HasListener(%q) = %v, want %v — kube-vip is the one component with no localhost listener", n, got, want)
		}
	}
	if HasListener("no-such-component") {
		t.Error(`HasListener("no-such-component") = true, want false for a name outside the roster`)
	}
}

func readImage(t *testing.T, name string) string {
	t.Helper()
	pod, _ := readPod(t, name)
	return pod.Spec.Containers[0].Image
}

// TestRestartAsIsRefusesAnEmptyManifest: an empty or null YAML document
// unmarshals without error, and writing into the nil map it leaves would panic
// after etcd has already been restored. It is an error the restore can report.
func TestRestartAsIsRefusesAnEmptyManifest(t *testing.T) {
	testNode(t)
	for _, content := range []string{"", "\n", "null\n", "# nothing\n"} {
		if err := os.MkdirAll(Dir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(Path("kube-scheduler"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		err := RestartAsIs("kube-scheduler")
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Errorf("manifest %q: err = %v, want an empty-manifest error", content, err)
		}
		if b, _ := os.ReadFile(Path("kube-scheduler")); string(b) != content {
			t.Errorf("manifest %q was rewritten to %q", content, b)
		}
	}
}

// TestWaitReplacedTellsProcessesByStartTime: a process started before the
// mark holds the wait, and one started after it does not.
func TestWaitReplacedTellsProcessesByStartTime(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "stratum-replaced-test"
	start := func() *exec.Cmd {
		// The test binary stands in for the component under a renamed argv[0],
		// which is what PIDs matches. A symlinked /bin/sleep does not: a
		// multi-call coreutils (Ubuntu 26.04's) refuses to run under another name.
		cmd := exec.Command(self, "-test.run=^TestHelperSleep$")
		cmd.Args[0] = filepath.Join(t.TempDir(), name)
		cmd.Env = append(os.Environ(), "STRATUM_TEST_HELPER_SLEEP=1")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		return cmd
	}
	old := start()
	time.Sleep(50 * time.Millisecond)
	mark, err := Now()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := WaitReplaced(name, mark, time.Second); err == nil || !strings.Contains(err.Error(), "still runs") {
		t.Fatalf("WaitReplaced with the old process running = %v, want it held", err)
	}
	_ = old.Process.Kill()
	_ = old.Wait()
	start()
	if err := WaitReplaced(name, mark, 5*time.Second); err != nil {
		t.Fatalf("WaitReplaced with only a process started after the mark = %v, want nil", err)
	}
}

// TestHelperSleep is the long-running process TestWaitReplacedTellsProcessesByStartTime
// starts; run directly, it does nothing.
func TestHelperSleep(t *testing.T) {
	if os.Getenv("STRATUM_TEST_HELPER_SLEEP") == "" {
		t.Skip("helper process")
	}
	time.Sleep(60 * time.Second)
}

// TestWaitReplacedCountsTheMarksTickAsBefore: start times have a tick's
// resolution, so a process that started in the mark's own tick holds the wait.
func TestWaitReplacedCountsTheMarksTickAsBefore(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	start, alive, err := StartTicks(os.Getpid())
	if err != nil || !alive {
		t.Fatalf("StartTicks(self) = %d, %v, %v", start, alive, err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	err = WaitReplaced(filepath.Base(exe), Mark{ticks: start}, 0)
	if err == nil || !strings.Contains(err.Error(), "still runs") {
		t.Fatalf("WaitReplaced with a process started in the mark's tick = %v, want it held", err)
	}
}

// TestAuditPolicyChangeRestartsTheAPIServer: the apiserver reads its audit
// policy only at start, so under CIS its manifest carries the policy's sum: a
// changed policy changes the manifest, an unchanged one leaves it as it was,
// and a missing one is an error rather than an apiserver without its policy.
func TestAuditPolicyChangeRestartsTheAPIServer(t *testing.T) {
	st := testNode(t)
	profiletest.Register(t)
	oldAudit, oldAdmission := version.AuditPolicyPath, version.AdmissionConfigPath
	t.Cleanup(func() { version.AuditPolicyPath, version.AdmissionConfigPath = oldAudit, oldAdmission })
	version.AuditPolicyPath = filepath.Join(version.K8sConfDir, "audit-policy.yaml")
	version.AdmissionConfigPath = filepath.Join(version.K8sConfDir, "admission-config.yaml")
	put := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(version.AdmissionConfigPath, "apiVersion: apiserver.config.k8s.io/v1\nkind: AdmissionConfiguration\nplugins: []\n")
	put(version.AuditPolicyPath, "rules:\n- level: Metadata\n")
	annotation := func() string {
		pod, _ := readPod(t, "kube-apiserver")
		return pod.Annotations[AuditPolicyAnnotation]
	}

	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	if annotation() != "" {
		t.Error("an apiserver without CIS carries an audit policy sum")
	}
	st.CIS = true
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	first := annotation()
	if len(first) != 64 {
		t.Fatalf("CIS apiserver audit policy sum = %q, want a sha256", first)
	}
	_, before := readPod(t, "kube-apiserver")
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	if _, after := readPod(t, "kube-apiserver"); string(after) != string(before) {
		t.Error("an unchanged policy re-rendered the apiserver differently, restarting it")
	}
	put(version.AuditPolicyPath, "rules:\n- level: RequestResponse\n")
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	if annotation() == first {
		t.Error("a changed audit policy left the apiserver manifest unchanged, so it keeps logging by the old one")
	}
	if err := os.Remove(version.AuditPolicyPath); err != nil {
		t.Fatal(err)
	}
	if err := WriteAll(st); err == nil || !strings.Contains(err.Error(), "audit-policy.yaml") {
		t.Errorf("render with the policy missing: %v, want an error naming it", err)
	}
}

// TestAdmissionConfigChangeRestartsTheAPIServer: the apiserver reads the
// admission config merged from the profile's only at start, so the manifest
// carries that body's sum in an annotation of its own: a changed body changes
// the manifest, an unchanged one leaves it byte-identical, the audit policy's
// annotation keeps the value it had before this one existed, and a missing
// body is an error.
func TestAdmissionConfigChangeRestartsTheAPIServer(t *testing.T) {
	st := testNode(t)
	profiletest.Register(t)
	oldAudit, oldAdmission := version.AuditPolicyPath, version.AdmissionConfigPath
	t.Cleanup(func() { version.AuditPolicyPath, version.AdmissionConfigPath = oldAudit, oldAdmission })
	version.AuditPolicyPath = filepath.Join(version.K8sConfDir, "audit-policy.yaml")
	version.AdmissionConfigPath = filepath.Join(version.K8sConfDir, "admission-config.yaml")
	put := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const audit = "rules:\n- level: Metadata\n"
	put(version.AuditPolicyPath, audit)
	put(version.AdmissionConfigPath, "apiVersion: apiserver.config.k8s.io/v1\nkind: AdmissionConfiguration\nplugins: []\n")
	annotations := func() map[string]string {
		pod, _ := readPod(t, "kube-apiserver")
		return pod.Annotations
	}

	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	if v, ok := annotations()[AdmissionConfigAnnotation]; ok {
		t.Errorf("an apiserver without a profile carries an admission config sum %q", v)
	}
	st.Profile = profile.CISName
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	first := annotations()
	if len(first[AdmissionConfigAnnotation]) != 64 {
		t.Fatalf("cis apiserver admission config sum = %q, want a sha256", first[AdmissionConfigAnnotation])
	}
	auditSum := sha256.Sum256([]byte(audit))
	if first[AuditPolicyAnnotation] != hex.EncodeToString(auditSum[:]) {
		t.Errorf("audit policy annotation = %q, want the policy's own sha256, as before the admission annotation", first[AuditPolicyAnnotation])
	}
	_, before := readPod(t, "kube-apiserver")
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	if _, after := readPod(t, "kube-apiserver"); string(after) != string(before) {
		t.Error("an unchanged admission config re-rendered the apiserver differently, restarting it")
	}
	put(version.AdmissionConfigPath, "apiVersion: apiserver.config.k8s.io/v1\nkind: AdmissionConfiguration\nplugins:\n- name: EventRateLimit\n")
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	second := annotations()
	if second[AdmissionConfigAnnotation] == first[AdmissionConfigAnnotation] {
		t.Error("a changed admission config left the apiserver manifest unchanged, so it keeps the old admission policy")
	}
	if second[AuditPolicyAnnotation] != first[AuditPolicyAnnotation] {
		t.Error("a changed admission config changed the audit policy annotation")
	}
	if err := os.Remove(version.AdmissionConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := WriteAll(st); err == nil || !strings.Contains(err.Error(), "admission") {
		t.Errorf("render with the admission config missing: %v, want an error naming it", err)
	}
}

// Every static pod runs as spc_t: with containerd's enable_selinux on, a
// control-plane pod running as container_t is denied its PKI, kubeconfigs and
// etcd data.
func TestStaticPodsRunAsSPCT(t *testing.T) {
	st := testNode(t)
	st.VIP.Address = "192.0.2.100"
	for _, name := range []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler", "kube-vip"} {
		pod, err := render(name, st)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sc := pod.Spec.SecurityContext
		if sc == nil || sc.SELinuxOptions == nil || sc.SELinuxOptions.Type != "spc_t" {
			t.Errorf("%s: pod securityContext = %+v, want seLinuxOptions.type spc_t", name, sc)
		}
	}
}

// TestKubeVipMountsOnlyItsKubeconfig pins that kube-vip's pod sees its own
// kubeconfig and no other: the shared directory holds admin.kubeconfig.
func TestKubeVipMountsOnlyItsKubeconfig(t *testing.T) {
	st := testNode(t)
	st.VIP.Address = "192.0.2.100"
	pod, err := render("kube-vip", st)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Dir(version.KubeVipKubeconfig())
	if want == version.KubeconfigDir {
		t.Fatalf("kube-vip's kubeconfig sits in %s beside the others", want)
	}
	var paths []string
	for _, v := range pod.Spec.Volumes {
		paths = append(paths, v.HostPath.Path)
	}
	if len(paths) != 1 || paths[0] != want {
		t.Errorf("kube-vip mounts %v, want only %s", paths, want)
	}
	if got := pod.Spec.Containers[0].Args; !slices.Contains(got, version.KubeVipKubeconfig()) {
		t.Errorf("kube-vip args %v do not name %s", got, version.KubeVipKubeconfig())
	}
}

// TestSchedulerMountsOnlyItsKubeconfig pins that kube-scheduler's pod sees its
// own kubeconfig and no other, and no PKI: the shared directories hold the
// admin and super-admin credentials and the CA key.
func TestSchedulerMountsOnlyItsKubeconfig(t *testing.T) {
	pod, err := render("kube-scheduler", testNode(t))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Dir(version.SchedulerKubeconfig())
	if want == version.KubeconfigDir {
		t.Fatalf("the scheduler's kubeconfig sits in %s beside the others", want)
	}
	var paths []string
	for _, v := range pod.Spec.Volumes {
		paths = append(paths, v.HostPath.Path)
	}
	if len(paths) != 1 || paths[0] != want {
		t.Errorf("kube-scheduler mounts %v, want only %s", paths, want)
	}
	for _, a := range pod.Spec.Containers[0].Command {
		if strings.Contains(a, "kubeconfig=") && !strings.HasSuffix(a, "="+version.SchedulerKubeconfig()) {
			t.Errorf("kube-scheduler arg %q names a kubeconfig other than %s", a, version.SchedulerKubeconfig())
		}
	}
}

// TestSigningFlagsFollowTheSigner: the controller-manager's CSR signer and
// both service-account signing keys name the pair pki.Signer picks, so a
// forced rotation that went ahead without a lost key still has a signer from
// trust-new to sign-new. The trust flags never move with it.
func TestSigningFlagsFollowTheSigner(t *testing.T) {
	orig := signer
	t.Cleanup(func() { signer = orig })
	signer = func(base string) string { return base + "-new" }
	st := &nodestate.NodeState{Role: "controller", NodeIP: "10.0.0.1", PodCIDR: "10.244.0.0/16"}
	id := func(f string) string { return f }
	args := append(apiserverArgs(st, profiletest.For(st.CIS), id), cmArgs(st, id, id)...)
	for _, want := range []string{
		"--service-account-signing-key-file=sa-new.key",
		"--cluster-signing-cert-file=ca-new.crt",
		"--cluster-signing-key-file=ca-new.key",
		"--service-account-private-key-file=sa-new.key",
		"--client-ca-file=ca-bundle.crt",
		"--service-account-key-file=sa-bundle.pub",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("no %s in the control-plane args", want)
		}
	}
}

// TestAPIServerCISFlagsAreExactlyTheProfile pins the CIS half of the apiserver
// argv by rendering one node state with CIS off and on and comparing the two
// renders with each other: the CIS argv must be the non-CIS one with exactly
// the profile's flags spliced in, in this order, immediately before
// --kubelet-certificate-authority, the profile flag every render carries. A CIS flag that is dropped, renamed,
// revalued, reordered or moved fails; a flag added to both renders by an
// unrelated change does not, which a whole-argv golden file would not
// tolerate. Nothing here touches the filesystem — apiserverArgs is pure argv
// building — so the paths are fixed literals, redirected to keep the render
// independent of what earlier tests left in the version vars.
func TestAPIServerCISFlagsAreExactlyTheProfile(t *testing.T) {
	oldAudit, oldLog, oldEncryption, oldConf, oldLogDir := version.AuditPolicyPath, version.AuditLogPath, version.EncryptionConfigPath, version.K8sConfDir, version.AuditLogDir
	t.Cleanup(func() {
		version.AuditPolicyPath, version.AuditLogPath, version.EncryptionConfigPath, version.K8sConfDir, version.AuditLogDir = oldAudit, oldLog, oldEncryption, oldConf, oldLogDir
	})
	version.AuditPolicyPath = "/cfg/audit-policy.yaml"
	version.AuditLogPath = "/log/audit.log"
	version.AuditLogDir = "/log"
	version.EncryptionConfigPath = "/pki/encryption-config.yaml"
	version.K8sConfDir = "/cfg"
	pki := func(f string) string { return "/pki/" + f }

	// The two node states differ only in the CIS flag.
	base := &nodestate.NodeState{
		Role: "controller", Hostname: "cp1", NodeIP: "192.0.2.10",
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12",
	}
	plain := apiserverArgs(base, profiletest.For(false), pki)
	base.CIS = true
	hardened := apiserverArgs(base, profiletest.For(true), pki)

	cisFlags := []string{
		"--audit-policy-file=" + version.AuditPolicyPath,
		"--audit-log-path=" + version.AuditLogPath,
		"--audit-log-maxage=30",
		"--audit-log-maxbackup=10",
		"--audit-log-maxsize=100",
		"--encryption-provider-config=" + version.EncryptionConfigPath,
	}
	kca := slices.Index(plain, "--kubelet-certificate-authority="+pki("ca-bundle.crt"))
	if kca < 0 {
		t.Fatal("the non-CIS render carries no --kubelet-certificate-authority to splice the CIS flags before")
	}
	want := slices.Insert(slices.Clone(plain), kca, cisFlags...)
	if !slices.Equal(hardened, want) {
		t.Errorf("the CIS argv is not the non-CIS one with exactly the profile's flags, in order, before --kubelet-certificate-authority:\ngot:  %q\nwant: %q", hardened, want)
	}
}

// TestReadContainerSpecReadsTheFileKubeletReads owns the reader the hardening
// audit's manifest controls rest on. It reads the FILE, not a re-render: an
// out-of-band edit must be visible, or the audit reports what stratum intended
// rather than what kubelet was handed. The argv is Command followed by Args —
// what the container actually execs — and the declared env is the container's
// whole environment, so a rendered container that carries none must read as
// none. The fail-closed cases are the contract's other half: valueFrom and
// envFrom name environment the manifest does not carry, and a manifest without
// the named container says nothing about it — each must error rather than hand
// back a spec that reads as complete.
func TestReadContainerSpecReadsTheFileKubeletReads(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	spec, err := ReadContainerSpec("etcd")
	if err != nil {
		t.Fatal(err)
	}
	pod, _ := readPod(t, "etcd")
	c := &pod.Spec.Containers[0]
	if !slices.Equal(spec.Command, c.Command) {
		t.Errorf("ReadContainerSpec command = %q, want the manifest's Command: %q", spec.Command, c.Command)
	}
	want := append(append([]string{}, c.Command...), c.Args...)
	if !slices.Equal(spec.Argv, want) {
		t.Errorf("ReadContainerSpec argv = %q, want the manifest's Command followed by Args: %q", spec.Argv, want)
	}
	if len(spec.Env) != 0 {
		t.Errorf("the rendered etcd declares env %v — the audit treats the manifest as the container's whole environment, so an unrendered source would read as absent", spec.Env)
	}

	// An out-of-band edit is exactly what the audit exists to see: the spec
	// must follow the file, not the node state this manifest was rendered from.
	edited := append(slices.Clone(c.Command), "--client-cert-auth=false")
	c.Command = edited
	if err := os.WriteFile(Path("etcd"), marshal(t, pod), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := ReadContainerSpec("etcd")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.Argv, edited) {
		t.Errorf("after an out-of-band edit, ReadContainerSpec argv = %q, want the edited file's %q — a re-render would hide the drift the audit is for", after.Argv, edited)
	}

	// Fail-closed cases over hand-written manifests, written through Put the
	// way any manifest lands on disk.
	for _, tc := range []struct {
		name, body, wantErr string
	}{
		{"valueFrom hides the value", `apiVersion: v1
kind: Pod
spec:
  containers:
  - name: etcd
    command: [etcd]
    env:
    - name: ETCD_AUTO_TLS
      valueFrom:
        fieldRef: {fieldPath: spec.nodeName}
`, "valueFrom"},
		{"envFrom is a second source", `apiVersion: v1
kind: Pod
spec:
  containers:
  - name: etcd
    command: [etcd]
    envFrom:
    - configMapRef: {name: etcd-env}
`, "envFrom"},
		{"no container of that name", `apiVersion: v1
kind: Pod
spec:
  containers:
  - name: other
    command: [etcd]
`, "declares no container named"},
		{"unparseable manifest", "not a manifest: [unclosed", "parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Put("etcd", []byte(tc.body)); err != nil {
				t.Fatal(err)
			}
			_, err := ReadContainerSpec("etcd")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ReadContainerSpec = err %v, want an error naming %q — the spec must fail closed, never read as complete", err, tc.wantErr)
			}
		})
	}
}

// TestSnapshotRestoreRoundTrip: Snapshot captures the manifest set the render
// owns — every static pod manifest plus the two render-derived files an
// apiserver manifest write puts beside it (admission-webhook.yaml and
// webhook-client.kubeconfig) — and RestoreSnapshot puts exactly that set back
// through the package's one staging path: manifests into the manifest dir,
// derived files into the config directory where the render writes them, each
// with its own mode, and no visible stray file for kubelet to parse. A
// rollback runs its restored manifests without re-rendering them, so nothing
// else puts the derived files back.
func TestSnapshotRestoreRoundTrip(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}
	// Files the CIS config writer owns rather than the render: they ride the
	// CIS config backup, never the manifest set.
	for name, body := range map[string]string{
		"admission-config.yaml": "admission config\n",
		"audit-policy.yaml":     "audit policy\n",
	} {
		if err := os.WriteFile(filepath.Join(version.K8sConfDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// No VIP in the state: no kube-vip manifest. The render wrote both derived
	// files 0600; one is given its own mode so the round trip pins that each
	// file keeps the mode the node had, not the writer's default.
	manifests := []string{"etcd", "kube-apiserver", "kube-controller-manager", "kube-scheduler"}
	derived := map[string]os.FileMode{
		"admission-webhook.yaml":    0o644,
		"webhook-client.kubeconfig": 0o600,
	}
	if err := os.Chmod(version.AdmissionWebhookConfig(), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := t.TempDir()
	if err := Snapshot(snap); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(snap)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := []string{"admission-webhook.yaml", "webhook-client.kubeconfig"}
	for _, n := range manifests {
		want = append(want, n+".yaml")
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the snapshot holds %q, want exactly the manifests and the render-derived files %q — the CIS writer's files and stray entries must stay out", got, want)
	}
	for _, n := range manifests {
		snapBytes, err := os.ReadFile(filepath.Join(snap, n+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		liveBytes, err := os.ReadFile(Path(n))
		if err != nil {
			t.Fatal(err)
		}
		if string(snapBytes) != string(liveBytes) {
			t.Errorf("the snapshot's %s does not hold the live manifest's bytes", n)
		}
	}
	for name, mode := range derived {
		fi, err := os.Stat(filepath.Join(snap, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode {
			t.Errorf("the snapshot's %s mode = %o, want the live file's %o", name, fi.Mode().Perm(), mode)
		}
	}

	// The state the upgrade leaves behind: re-rendered manifests, a rewritten
	// derived pair, and a manifest the newer render dropped.
	for _, n := range manifests {
		if err := os.WriteFile(Path(n), []byte("new "+n+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(Path("kube-scheduler")); err != nil {
		t.Fatal(err)
	}
	for name := range derived {
		if err := os.WriteFile(filepath.Join(version.K8sConfDir, name), []byte("new "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := RestoreSnapshot(snap); err != nil {
		t.Fatal(err)
	}

	for _, n := range manifests {
		snapBytes, err := os.ReadFile(filepath.Join(snap, n+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(Path(n)); err != nil || string(b) != string(snapBytes) {
			t.Errorf("%s after the restore = %q, %v; want the snapshotted bytes back, the dropped manifest included", n, b, err)
		}
	}
	for name, mode := range derived {
		b, err := os.ReadFile(filepath.Join(version.K8sConfDir, name))
		if err != nil {
			t.Fatal(err)
		}
		wantBytes, err := os.ReadFile(filepath.Join(snap, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != string(wantBytes) {
			t.Errorf("%s after the restore = %q, want the snapshotted bytes — the restored manifests are not re-rendered, so nothing else puts this file back", name, b)
		}
		if fi, err := os.Stat(filepath.Join(version.K8sConfDir, name)); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s mode after the restore = %v, %v; want %o", name, fi, err, mode)
		}
	}

	// kubelet parses every non-hidden file in the manifest dir: after the
	// restore it must hold exactly the manifests.
	entries, err = os.ReadDir(Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !slices.Contains(manifests, strings.TrimSuffix(e.Name(), ".yaml")) {
			t.Errorf("the restore left %s in the manifest dir; kubelet parses it as a pod", e.Name())
		}
	}
}

func TestRestoreSnapshotRestoresDerivedFileModeAndType(t *testing.T) {
	for _, kind := range []string{"mode drift", "symlink", "unchanged"} {
		t.Run(kind, func(t *testing.T) {
			st := testNode(t)
			if err := WriteAll(st); err != nil {
				t.Fatal(err)
			}
			snap := filepath.Join(t.TempDir(), "snapshot")
			if err := Snapshot(snap); err != nil {
				t.Fatal(err)
			}
			path := version.WebhookClientKubeconfig()
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			var target string
			switch kind {
			case "mode drift":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target = filepath.Join(t.TempDir(), "foreign.kubeconfig")
				if err := os.WriteFile(target, contents, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := RestoreSnapshot(snap); err != nil {
				t.Fatal(err)
			}
			after, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !after.Mode().IsRegular() || after.Mode().Perm() != 0o600 {
				t.Errorf("restored kubeconfig mode = %v, want a regular file with mode 0600", after.Mode())
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != string(contents) {
				t.Errorf("restored kubeconfig = %q, %v; want %q", got, err, contents)
			}
			if kind == "unchanged" && !os.SameFile(before, after) {
				t.Error("an unchanged kubeconfig was rewritten")
			}
			if target != "" {
				fi, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode().Perm() != 0o600 {
					t.Errorf("foreign symlink target mode = %v, want 0600", fi.Mode())
				}
				if got, err := os.ReadFile(target); err != nil || string(got) != string(contents) {
					t.Errorf("foreign symlink target = %q, %v; want %q", got, err, contents)
				}
			}
		})
	}
}

// TestSnapshotSkipsStagingLeftoversAndNonManifests: a staging file that joined
// the snapshot would be restored on every later rollback — a visible one is a
// pod kubelet parses, a hidden one is a torn write — and the manifest dir holds
// no non-manifest files worth copying.
func TestSnapshotSkipsStagingLeftoversAndNonManifests(t *testing.T) {
	testNode(t)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"etcd.yaml":            "real",
		"etcd.yaml.tmp":        "the old visible staging leftover",
		".etcd.yaml.tmp-98765": "a hidden in-flight write",
		".etcd.yaml":           "hidden is never a manifest",
		"notes.txt":            "not a manifest at all",
	} {
		if err := os.WriteFile(filepath.Join(Dir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(Dir(), "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	snap := filepath.Join(t.TempDir(), "manifests-bak") // Snapshot creates it
	if err := Snapshot(snap); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(snap)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, []string{"etcd.yaml"}) {
		t.Errorf("the snapshot holds %q, want [etcd.yaml] — a staging file in the set is restored as a pod on every later rollback", got)
	}
}

// TestSnapshotSyncsEachCopiedFileThenTheParentDirectory: a backup set the
// upgrade resumes against must survive a power loss as a set. Every manifest
// and render-derived file is fsynced at the mode the node had before it lands
// under the hidden staging name, and the directory holding the snapshot is
// fsynced once the rename puts the set in place — until that sync a power
// loss can undo the rename after the upgrade went on believing its backup was
// taken.
func TestSnapshotSyncsEachCopiedFileThenTheParentDirectory(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}

	oldSyncFile, oldSyncDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldSyncFile, oldSyncDir })

	snap := filepath.Join(t.TempDir(), "manifests-bak") // Snapshot creates it
	parent := filepath.Dir(snap)

	var syncedModes []os.FileMode
	syncFile = func(f *os.File) error {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		syncedModes = append(syncedModes, fi.Mode().Perm())
		return oldSyncFile(f)
	}
	syncedDirs := map[string]int{}
	parentSyncedWithSnapInPlace := false
	syncDir = func(d string) error {
		syncedDirs[d]++
		if d == parent {
			if _, err := os.Stat(snap); err == nil {
				parentSyncedWithSnapInPlace = true
			}
		}
		return oldSyncDir(d)
	}

	if err := Snapshot(snap); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(snap)
	if err != nil {
		t.Fatal(err)
	}
	var wantModes []os.FileMode
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		wantModes = append(wantModes, fi.Mode().Perm())
	}
	slices.Sort(syncedModes)
	slices.Sort(wantModes)
	if !slices.Equal(syncedModes, wantModes) {
		t.Errorf("the copies were synced at modes %v, want the snapshot's file modes %v — a mode pinned after the fsync is not covered by it", syncedModes, wantModes)
	}

	var otherDirs []string
	for d, n := range syncedDirs {
		if d == parent {
			if n != 1 {
				t.Errorf("the snapshot's parent %s was synced %d times, want once after the rename", parent, n)
			}
			continue
		}
		if filepath.Base(d) != filepath.Base(snap) && !strings.HasPrefix(filepath.Base(d), "."+filepath.Base(snap)+".staging-") {
			otherDirs = append(otherDirs, d)
		}
	}
	if otherDirs != nil {
		t.Errorf("Snapshot fsynced directories beside the staging copy and the snapshot's parent: %v", otherDirs)
	}
	if !parentSyncedWithSnapInPlace {
		t.Errorf("the snapshot's parent %s was never synced with the snapshot in place: a power loss can undo the rename", parent)
	}
}

// TestSnapshotKeepsNoPartialSetWhenASyncFails: a manifest whose fsync fails
// stops the snapshot. Nothing is left at dir for the upgrade to resume
// against, and no staging copy survives beside it — a partial set is one a
// later rollback would restore half-whole.
func TestSnapshotKeepsNoPartialSetWhenASyncFails(t *testing.T) {
	st := testNode(t)
	if err := WriteAll(st); err != nil {
		t.Fatal(err)
	}

	oldSyncFile := syncFile
	t.Cleanup(func() { syncFile = oldSyncFile })
	syncFile = func(*os.File) error { return errors.New("injected fsync failure") }

	snap := filepath.Join(t.TempDir(), "manifests-bak")
	if err := Snapshot(snap); err == nil {
		t.Fatal("Snapshot succeeded with a failing file sync: a torn copy must not be presented as a taken backup")
	}
	if _, err := os.Lstat(snap); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a snapshot exists at %s despite the failed sync (lstat err = %v): the upgrade would resume against it", snap, err)
	}
	entries, err := os.ReadDir(filepath.Dir(snap))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "."+filepath.Base(snap)+".staging-") {
			t.Errorf("staging copy %s survived the failed snapshot", e.Name())
		}
	}
}

// TestRestoreSnapshotSweepsVisibleLeftovers: nodes upgraded before the atomic
// writer still carry a visible "<name>.yaml.tmp" in the manifest dir, where
// kubelet parses it as a pod and it sorts after the real manifest in the file
// source's glob, so it holds that pod's store key permanently. The restore
// evicts it. A hidden temp is a live write by a concurrent Start and is never
// ours to remove; a snapshot holding an unchanged manifest must not rewrite it
// (a fresh inode and an inotify event buy kubelet's pod churn for nothing).
func TestRestoreSnapshotSweepsVisibleLeftovers(t *testing.T) {
	testNode(t)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	snap := t.TempDir()
	if err := os.WriteFile(filepath.Join(snap, "etcd.yaml"), []byte("restored etcd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "kube-apiserver.yaml"), []byte("restored apiserver\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file that is neither a manifest nor a derived one must not land in the
	// manifest dir, whatever a broken snapshot holds.
	if err := os.WriteFile(filepath.Join(snap, "notes.txt"), []byte("not a manifest"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(Path("kube-apiserver.yaml"), []byte("restored apiserver\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.Stat(Path("kube-apiserver.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(), "etcd.yaml.tmp"), []byte("a pod kubelet should never have seen\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(), ".kube-scheduler.yaml.tmp-1"), []byte("an in-flight render\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RestoreSnapshot(snap); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(Dir(), "etcd.yaml.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the visible staging ghost survived the restore (%v); kubelet keeps parsing it as a pod", err)
	}
	if b, err := os.ReadFile(filepath.Join(Dir(), ".kube-scheduler.yaml.tmp-1")); err != nil || string(b) != "an in-flight render\n" {
		t.Errorf("the restore touched a hidden in-flight staging write (%q, %v) — that is a concurrent Start's temp, and deleting it fails that writer's rename with ENOENT", b, err)
	}
	if b, err := os.ReadFile(Path("etcd")); err != nil || string(b) != "restored etcd\n" {
		t.Errorf("etcd manifest after the restore = %q, %v; want the snapshotted bytes", b, err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "notes.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the restore copied %s into the manifest dir; kubelet parses it as a pod", "notes.txt")
	}
	after, err := os.Stat(Path("kube-apiserver.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(unchanged, after) {
		t.Error("a manifest the snapshot held byte-for-byte was rewritten — kubelet sees the new inode and recreates the pod for nothing")
	}
}

// TestRestoreSnapshotFailsWhenAGhostSurvives: the honesty contract. If the
// visible staging file cannot be removed, the restore must report failure
// rather than claim success while kubelet still parses the ghost as a pod.
func TestRestoreSnapshotFailsWhenAGhostSurvives(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes from a directory its mode denies")
	}
	testNode(t)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	snap := t.TempDir()
	if err := os.WriteFile(filepath.Join(snap, "etcd.yaml"), []byte("restored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(), "etcd.yaml.tmp"), []byte("ghost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(Dir(), 0o755) })

	if err := RestoreSnapshot(snap); err == nil {
		t.Fatal("RestoreSnapshot reported success while a staging ghost it could not remove is still in the manifest dir")
	}
}

// TestManifestWriteDoesNotStageAtAFixedSharedName: the fixed
// ".<name>.yaml.tmp" staging name is one path shared by every writer of the
// manifest — a rollback's Put and a concurrent Start both staging there tear
// one manifest, one truncating the file the other is about to rename into
// place. Staging is unique per write: a write must succeed even with the old
// fixed staging name already held — here by a directory standing in for
// another writer's occupation.
func TestManifestWriteDoesNotStageAtAFixedSharedName(t *testing.T) {
	testNode(t)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(Dir(), ".etcd.yaml.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("kind: Pod\nmetadata:\n  name: etcd\n")
	if err := Put("etcd", want); err != nil {
		t.Fatalf("Put with the old fixed staging name already held: %v — the manifest writer still shares one staging path", err)
	}
	if b, err := os.ReadFile(Path("etcd")); err != nil || string(b) != string(want) {
		t.Errorf("manifest after Put = %q, %v; want %q", b, err, want)
	}
}

// TestManifestWriteSkipsAnUnchangedManifest: a write whose bytes are already on
// disk must not touch the file — an idempotent re-render or a restore of an
// identical manifest would otherwise hand kubelet an inotify event and a fresh
// inode, and kubelet answers with a pod restart nothing needed.
func TestManifestWriteSkipsAnUnchangedManifest(t *testing.T) {
	testNode(t)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Put("etcd", []byte("kind: Pod\n")); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(Path("etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Put("etcd", []byte("kind: Pod\n")); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(Path("etcd"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("an unchanged manifest was rewritten — every idempotent render and identical restore would restart the pod")
	}
}

// marshal writes a pod back the way the renderer does, so an edited manifest
// is a manifest kubelet would still run.
func marshal(t *testing.T, pod *corev1.Pod) []byte {
	t.Helper()
	b, err := yaml.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAdmissionFilesLandWhereTheArgvNamesThem pins the three-way resolution
// version.AdmissionWebhookConfig exists for: the render WRITES the admission
// file there, the apiserver's rendered ARGV names it there, and the webhook
// plugin inside it names the client kubeconfig the same render wrote beside it.
// A writer and an argv that resolve the path differently hand the apiserver an
// admission file that does not exist — or leave a stale one for it to load —
// and the audit's 1.2.11 reads exactly the file that argv names.
func TestAdmissionFilesLandWhereTheArgvNamesThem(t *testing.T) {
	st := testNode(t)
	profiletest.Register(t)
	oldAudit, oldAdmission := version.AuditPolicyPath, version.AdmissionConfigPath
	t.Cleanup(func() { version.AuditPolicyPath, version.AdmissionConfigPath = oldAudit, oldAdmission })
	version.AuditPolicyPath = filepath.Join(version.K8sConfDir, "audit-policy.yaml")
	version.AdmissionConfigPath = filepath.Join(version.K8sConfDir, "admission-config.yaml")
	put := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put(version.AuditPolicyPath, "rules:\n- level: Metadata\n")
	put(version.AdmissionConfigPath, "apiVersion: apiserver.config.k8s.io/v1\nkind: AdmissionConfiguration\nplugins: []\n")

	for _, cis := range []bool{false, true} {
		name := "non-CIS"
		if cis {
			name = "CIS"
		}
		t.Run(name, func(t *testing.T) {
			st.CIS = cis
			if err := WriteAll(st); err != nil {
				t.Fatal(err)
			}
			spec, err := ReadContainerSpec("kube-apiserver")
			if err != nil {
				t.Fatal(err)
			}
			named := ""
			for _, tok := range spec.Argv {
				if v, ok := strings.CutPrefix(tok, "--admission-control-config-file="); ok {
					named = v
				}
			}
			if named == "" {
				t.Fatal("the rendered apiserver argv names no admission control config file")
			}
			if named != version.AdmissionWebhookConfig() {
				t.Errorf("the argv names %s, want the render's own %s — writer and argv resolve the admission file through different calls", named, version.AdmissionWebhookConfig())
			}
			b, err := os.ReadFile(named)
			if err != nil {
				t.Fatalf("the admission file the argv names does not exist where the render wrote it: %v", err)
			}
			var admission struct {
				Plugins []struct {
					Name          string `json:"name"`
					Configuration struct {
						KubeConfigFile string `json:"kubeConfigFile"`
					} `json:"configuration"`
				} `json:"plugins"`
			}
			if err := yaml.Unmarshal(b, &admission); err != nil {
				t.Fatalf("the admission file the argv names does not parse: %v", err)
			}
			found := 0
			for _, p := range admission.Plugins {
				if p.Name != "MutatingAdmissionWebhook" {
					continue
				}
				found++
				if p.Configuration.KubeConfigFile != version.WebhookClientKubeconfig() {
					t.Errorf("the webhook plugin names %q, want the render's own %q — the plugin and the render resolve the client kubeconfig differently", p.Configuration.KubeConfigFile, version.WebhookClientKubeconfig())
				}
				if _, err := os.ReadFile(p.Configuration.KubeConfigFile); err != nil {
					t.Errorf("the webhook kubeconfig the plugin names does not exist where the render wrote it: %v", err)
				}
			}
			if found != 1 {
				t.Errorf("the admission file carries %d MutatingAdmissionWebhook plugins, want exactly the one the render appends", found)
			}
		})
	}
}

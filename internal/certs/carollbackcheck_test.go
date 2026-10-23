package certs

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// The cluster-wide check asks every node, control planes first, and refuses
// when one cannot roll back, naming it; it never asks a node to change
// anything, so the seam below is the only thing it runs per node.
func TestCheckRollbackClusterRefusesWhenOneNodeCannotRollBack(t *testing.T) {
	prep(t)
	objs := append(thisCPNode(t), certNodeFixture("w1", false), certNodeFixture("w2", false))
	cs := fakeAdminClientset(t, objs...)
	build := func() (kubernetes.Interface, error) { return cs, nil }

	old := runRollbackCheckJob
	t.Cleanup(func() { runRollbackCheckJob = old })
	var asked []string
	answers := map[string]int{}
	runRollbackCheckJob = func(_ context.Context, _ io.Writer, _ kubernetes.Interface, n cnode) (int, string, error) {
		asked = append(asked, n.Name)
		code := answers[n.Name]
		log := rollbackState{trustsNew: true}.String() + "\n"
		if n.IsCP {
			log = rollbackState{trustsNew: true, target: "ca=old"}.String() + "\n"
		}
		if code != 0 {
			log += "stratum certs rotate-ca: pki: refusing to roll back (nothing has been changed): the restored ca would not be a matching certificate and key\n"
		}
		return code, log, nil
	}

	var out bytes.Buffer
	if err := CheckRollbackCluster(context.Background(), &out, build); err != nil {
		t.Fatalf("every node can roll back, CheckRollbackCluster = %v", err)
	}
	if len(asked) != 4 || asked[2] != "w1" || asked[3] != "w2" {
		t.Errorf("asked %v, want both control planes and then w1, w2", asked)
	}
	if !strings.Contains(out.String(), "every node can roll back") {
		t.Errorf("output lacks the all-clear:\n%s", out.String())
	}

	asked, out = nil, bytes.Buffer{}
	answers["cp2"] = ExitRollbackUnsafe
	err := CheckRollbackCluster(context.Background(), &out, build)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "cp2") || !strings.Contains(err.Error(), "do not roll back any node") {
		t.Fatalf("one node cannot roll back, CheckRollbackCluster = %v; want a refusal naming cp2 and every node", err)
	}
	if len(asked) != 4 {
		t.Errorf("asked %v, want every node checked even after a refusal, so the operator sees them all", asked)
	}
	if !strings.Contains(out.String(), "cp2 (control-plane): cannot roll back: stratum certs rotate-ca: pki: refusing") {
		t.Errorf("output lacks cp2's reason:\n%s", out.String())
	}

	asked, out = nil, bytes.Buffer{}
	delete(answers, "cp2")
	answers["w1"] = 2
	err = CheckRollbackCluster(context.Background(), &out, build)
	if err == nil || errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "w1") || !strings.Contains(err.Error(), "exited 2") {
		t.Fatalf("a check Job that failed = %v; want an error about w1, not a rollback verdict", err)
	}

	boom := errors.New("job did not start")
	runRollbackCheckJob = func(context.Context, io.Writer, kubernetes.Interface, cnode) (int, string, error) { return 0, "", boom }
	if err := CheckRollbackCluster(context.Background(), &out, build); !errors.Is(err, boom) || errors.Is(err, pki.ErrRollbackUnsafe) {
		t.Fatalf("a check that did not run = %v; want its error and no verdict", err)
	}
}

// stubRollbackStates answers each node's check with the state given for it,
// refusing where refuse names the node.
func stubRollbackStates(t *testing.T, states map[string]rollbackState, refuse ...string) {
	t.Helper()
	old := runRollbackCheckJob
	t.Cleanup(func() { runRollbackCheckJob = old })
	runRollbackCheckJob = func(_ context.Context, _ io.Writer, _ kubernetes.Interface, n cnode) (int, string, error) {
		log := states[n.Name].String() + "\n"
		if slices.Contains(refuse, n.Name) {
			return ExitRollbackUnsafe, log + "stratum certs rotate-ca: pki: refusing to roll back: the restored sa would not be a matching certificate and key\n", nil
		}
		return 0, log + "rollback: possible on this node\n", nil
	}
}

func rollbackCheckClusterNodes(t *testing.T) (string, func() (kubernetes.Interface, error)) {
	t.Helper()
	prep(t)
	objs := append(thisCPNode(t), certNodeFixture("w1", false))
	cs := fakeAdminClientset(t, objs...)
	me, _ := os.Hostname()
	return me, func() (kubernetes.Interface, error) { return cs, nil }
}

// A control plane that finalized holds no backups, so its own check passes
// against the new signers while one still at sign-new passes against the old:
// the cluster check must see that they would not return to the same root.
func TestCheckRollbackClusterRefusesAfterAPartialFinalize(t *testing.T) {
	me, build := rollbackCheckClusterNodes(t)
	stubRollbackStates(t, map[string]rollbackState{
		me:    {trustsNew: false, target: "ca=new"},
		"cp2": {trustsNew: true, target: "ca=old"},
		"w1":  {trustsNew: true},
	})
	err := CheckRollbackCluster(context.Background(), io.Discard, build)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "would not roll back to the same signers") ||
		!strings.Contains(err.Error(), "cp2 to ca=old") || !strings.Contains(err.Error(), me+" to ca=new") {
		t.Fatalf("CheckRollbackCluster = %v; want a refusal naming both targets", err)
	}
	if strings.Contains(err.Error(), "Do not run sign-new yet") {
		t.Errorf("a partial finalize means every node trusts the new CA; the refusal should not send the operator back to trust-new: %v", err)
	}
}

// A lost key while a node has not completed trust-new: promoting sign-new would
// sign with a CA that node does not trust. A worker can refresh its trust from
// the control planes by hand; a control plane cannot fetch the staged CA once
// the run has ended, so the way out is the lost key back from a backup.
func TestCheckRollbackClusterSendsANodeWithoutTheNewCAToTrustNewFirst(t *testing.T) {
	me, build := rollbackCheckClusterNodes(t)
	stubRollbackStates(t, map[string]rollbackState{
		me:    {trustsNew: true, target: "ca=old"},
		"cp2": {trustsNew: false, target: "ca=old"},
		"w1":  {trustsNew: false},
	}, me)
	err := CheckRollbackCluster(context.Background(), io.Discard, build)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "Do not run sign-new either: cp2 do not trust the new CA") ||
		!strings.Contains(err.Error(), "Restore the missing key on "+me) || strings.Contains(err.Error(), "--fetch-shared") || strings.Contains(err.Error(), "--phase=sign-new") {
		t.Fatalf("CheckRollbackCluster = %v; want the lost key restored, and neither --fetch-shared nor sign-new", err)
	}

	stubRollbackStates(t, map[string]rollbackState{
		me:    {trustsNew: true, target: "ca=old"},
		"cp2": {trustsNew: true, target: "ca=old"},
		"w1":  {trustsNew: false},
	}, me)
	err = CheckRollbackCluster(context.Background(), io.Discard, build)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "the workers w1 do not trust the new CA") ||
		!strings.Contains(err.Error(), "--phase=trust-new --yes") || strings.Contains(err.Error(), "--fetch-shared") {
		t.Fatalf("CheckRollbackCluster = %v; want trust-new on w1 before sign-new", err)
	}

	stubRollbackStates(t, map[string]rollbackState{
		me:    {trustsNew: true, target: "ca=old"},
		"cp2": {trustsNew: true, target: "ca=old"},
		"w1":  {trustsNew: true},
	}, me)
	err = CheckRollbackCluster(context.Background(), io.Discard, build)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || strings.Contains(err.Error(), "Do not run sign-new") || !strings.Contains(err.Error(), "--phase=sign-new --yes") {
		t.Fatalf("every node trusts the new CA, CheckRollbackCluster = %v; want the sign-new forward path", err)
	}
}

// A succeeded node Job is deleted as soon as it is seen, so the node's state
// line must come back with its exit status, read before the deletion.
func TestRunRollbackCheckJobReturnsTheLogOfASucceededJob(t *testing.T) {
	old := runExitCodeLogs
	t.Cleanup(func() { runExitCodeLogs = old })
	want := rollbackState{trustsNew: true, target: "ca=old"}.String() + "\nrollback: possible on this node\n"
	runExitCodeLogs = func(_ context.Context, _ kubernetes.Interface, s nodejob.Spec) (int, string, error) {
		if !slices.Contains(s.Argv, "--check") {
			t.Errorf("argv = %q, want the check", s.Argv)
		}
		return 0, want, nil
	}
	code, log, err := runRollbackCheckJob(context.Background(), io.Discard, nil, cnode{Name: "cp2", IsCP: true})
	if err != nil || code != 0 || log != want {
		t.Fatalf("runRollbackCheckJob = %d, %q, %v; want 0 and the node's log", code, log, err)
	}
}

// A forced rotation keeps its resume path: a halt before sign-new resumes with
// --cluster --force, and no node is sent to sign-new while one lacks the new CA.
func TestCheckRollbackClusterKeepsTheForcedResumePath(t *testing.T) {
	me, build := rollbackCheckClusterNodes(t)
	stubRollbackStates(t, map[string]rollbackState{
		me:    {forced: true, trustsNew: true, target: "ca=old"},
		"cp2": {forced: true, trustsNew: false, target: "ca=old"},
		"w1":  {trustsNew: false},
	}, me, "cp2")
	err := CheckRollbackCluster(context.Background(), io.Discard, build)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "--cluster --force --yes") || !strings.Contains(err.Error(), "halted before sign-new") {
		t.Fatalf("CheckRollbackCluster = %v; want the forced resume path", err)
	}
}

// A node whose check printed no state, an older stratum, gives no verdict.
func TestCheckRollbackClusterNeedsEveryNodesState(t *testing.T) {
	_, build := rollbackCheckClusterNodes(t)
	old := runRollbackCheckJob
	t.Cleanup(func() { runRollbackCheckJob = old })
	runRollbackCheckJob = func(context.Context, io.Writer, kubernetes.Interface, cnode) (int, string, error) {
		return 0, "rollback: possible on this node\n", nil
	}
	err := CheckRollbackCluster(context.Background(), io.Discard, build)
	if err == nil || errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "did not report") {
		t.Fatalf("CheckRollbackCluster = %v; want an error, not a verdict", err)
	}
}

func TestRollbackStateRoundTrips(t *testing.T) {
	for _, st := range []rollbackState{{}, {forced: true, trustsNew: true, target: "ca=0a,sa=0b"}, {trustsNew: true}} {
		got, ok := parseRollbackState("Waiting for the Job\n" + st.String() + "\nrollback: possible\n")
		if !ok || got != st {
			t.Errorf("parse(%q) = %+v, %v; want %+v", st.String(), got, ok, st)
		}
	}
	if _, ok := parseRollbackState("rollback: possible\n"); ok {
		t.Error("a log with no state line parsed")
	}
}

func TestCheckRollbackClusterRefusesACancelledContextBeforeAnyJob(t *testing.T) {
	old := runRollbackCheckJob
	t.Cleanup(func() { runRollbackCheckJob = old })
	runRollbackCheckJob = func(context.Context, io.Writer, kubernetes.Interface, cnode) (int, string, error) {
		t.Fatal("a Job ran on a cancelled context")
		return 0, "", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckRollbackCluster(ctx, io.Discard, func() (kubernetes.Interface, error) { return nil, errors.New("not built") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckRollbackCluster = %v, want the cancellation", err)
	}
}

// This machine renders no static pods, so CheckRollback answers as a worker
// does, and the node check's argv is the shell-free form every node Job uses.
func TestCheckRollbackOnAWorkerAndTheNodeArgv(t *testing.T) {
	prep(t)
	workerKubeconfigs(t)
	var out bytes.Buffer
	if err := CheckRollback(&out); err != nil || !strings.Contains(out.String(), "rollback: possible (a worker") || !strings.HasPrefix(out.String(), rollbackStatePrefix+" forced=no trusts-new=no\n") {
		t.Fatalf("CheckRollback = %v, %q", err, out.String())
	}
	argv := rollbackCheckNodeArgv()
	if i := slices.Index(argv, "--"); i < 0 || !slices.Equal(argv[i+1:], []string{argv[i+1], "certs", "rotate-ca", "--rollback", "--check"}) {
		t.Errorf("node argv = %q, want nsenter -- <stratum> certs rotate-ca --rollback --check", argv)
	}
	if slices.Contains(argv, "--yes") {
		t.Errorf("node argv %q carries --yes; the check is read-only and must not", argv)
	}
}

// `certs check` names the active signer of each pair and whether this node can
// roll back, alongside the forced line, so the damaged node is obvious.
func TestRotationDetailNamesTheRollbackVerdict(t *testing.T) {
	prep(t)
	workerKubeconfigs(t)
	rotateCAMaterialAfterPrep(t)
	got := RotationDetail()
	if len(got) != 3 || got[0] != "forced: no" || got[1] != "signer: ca/sa/front-proxy-ca (live)" || !strings.HasPrefix(got[2], "rollback: possible") {
		t.Errorf("detail = %q, want the forced line, the three signers and a rollback verdict", got)
	}
}

var _ runtime.Object = certNodeFixture("x", false)

// rotateCAMaterialAfterPrep stages a rotation's new CAs a second after prep's.
// The rollback check tells the new CA from the old by NotBefore, which has
// one-second resolution, so a rotation staged in prep's second leaves two CAs
// it cannot tell apart and the check refuses.
func rotateCAMaterialAfterPrep(t *testing.T) {
	t.Helper()
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(1100 * time.Millisecond)))
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
}

// workerKubeconfigs gives this test a kubelet kubeconfig and a
// bootstrap-kubeconfig a worker's trust refresh can read.
func workerKubeconfigs(t *testing.T) {
	t.Helper()
	redirectVar(t, &version.KubeconfigDir, t.TempDir())
	redirectVar(t, &version.BootstrapKubeconfig, filepath.Join(t.TempDir(), "bootstrap-kubeconfig"))
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[version.ClusterName] = &clientcmdapi.Cluster{Server: "https://10.0.0.9:6443", CertificateAuthorityData: []byte("ca")}
	for _, kc := range []string{workerTrustPaths().kubeletKubeconfig, version.BootstrapKubeconfig} {
		if err := clientcmd.WriteToFile(*cfg, kc); err != nil {
			t.Fatal(err)
		}
	}
}

// workerAfterSignNew lays out a worker whose trust holds old‖new and whose
// kubelet client certificate was renewed by signer.
func workerAfterSignNew(t *testing.T, signer func(old, newer *testCA) *testCA) {
	t.Helper()
	prep(t)
	workerKubeconfigs(t)
	old := caStarting(t, "old", time.Now().Add(-2*time.Hour))
	newer := caStarting(t, "new", time.Now().Add(-time.Hour))
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-bundle.crt"), bundle(old, newer), 0o644); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := nodeClientCert(t, signer(old, newer), &key.PublicKey, pkix.Name{CommonName: "system:node:w1", Organization: []string{nodesGroup}})
	if err := os.MkdirAll(filepath.Dir(kubeletClientCertPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kubeletClientCertPath(), leaf, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A worker sign-new renewed under the new CA cannot roll back: the rollback
// refreshes its trust only, and the kubelet's certificate would not be trusted
// by control planes back on the old CA. The check, `certs check` and the
// rollback itself all refuse, and the rollback changes nothing.
func TestWorkerRollbackRefusesAClientCertSignedByTheNewCA(t *testing.T) {
	workerAfterSignNew(t, func(_, newer *testCA) *testCA { return newer })
	var out bytes.Buffer
	err := CheckRollback(&out)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "kubelet client certificate under the new CA") ||
		!strings.Contains(err.Error(), "--phase=sign-new --yes") || !strings.Contains(err.Error(), "stratum uninstall") {
		t.Fatalf("CheckRollback = %v; want the refusal, the forward path and the rejoin", err)
	}
	if _, ok := parseRollbackState(out.String()); !ok {
		t.Errorf("the refusing check printed no state line: %q", out.String())
	}
	if got := RotationDetail(); len(got) != 3 || !strings.HasPrefix(got[2], "rollback: refused on this worker") {
		t.Errorf("detail = %q, want the worker's refusal", got)
	}
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(t.TempDir(), "upgrade.lock"))
	before := readAll(t, filepath.Join(version.PKIDir, "ca-bundle.crt"))
	if rbErr := RollbackCA(io.Discard, CARotateOptions{}); !errors.Is(rbErr, pki.ErrRollbackUnsafe) || rbErr.Error() != err.Error() {
		t.Fatalf("RollbackCA = %v, want the check's refusal word for word", rbErr)
	}
	if after := readAll(t, filepath.Join(version.PKIDir, "ca-bundle.crt")); !bytes.Equal(before, after) {
		t.Error("the refused rollback changed the trust bundle")
	}
}

// Before sign-new the kubelet's certificate is the old CA's, which the
// rollback keeps, so the worker can roll back.
func TestWorkerRollbackAllowsAClientCertSignedByTheOldCA(t *testing.T) {
	workerAfterSignNew(t, func(old, _ *testCA) *testCA { return old })
	var out bytes.Buffer
	if err := CheckRollback(&out); err != nil || !strings.Contains(out.String(), "rollback: possible (a worker") {
		t.Fatalf("CheckRollback = %v, %q; want possible", err, out.String())
	}
}

// `certs check` reports the verdict RollbackCheck gives: with every pair
// matching, a kubelet client-CA bundle RollbackCA cannot read still refuses.
func TestRotationDetailReportsWhatRollbackCheckRefuses(t *testing.T) {
	prep(t)
	redirectVar(t, &version.K8sConfDir, t.TempDir())
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staticpod.Path("kube-apiserver"), []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateCAMaterialAfterPrep(t)
	if err := os.WriteFile(pki.KubeletClientBundlePath(), []byte("not PEM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := RotationDetail()
	if len(got) != 3 || !strings.HasPrefix(got[2], "rollback: refused on this node") || !strings.Contains(got[2], pki.KubeletClientBundlePath()) {
		t.Errorf("detail = %q, want the refusal naming the kubelet client-CA bundle", got)
	}
}

// Only a control plane that refused is sent to restore a lost key: a worker
// that refused has no key to restore.
func TestCheckRollbackClusterSendsOnlyARefusingControlPlaneToABackup(t *testing.T) {
	me, build := rollbackCheckClusterNodes(t)
	stubRollbackStates(t, map[string]rollbackState{
		me:    {trustsNew: true, target: "ca=old"},
		"cp2": {trustsNew: false, target: "ca=old"},
		"w1":  {trustsNew: true},
	}, "w1")
	err := CheckRollbackCluster(context.Background(), io.Discard, build)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || strings.Contains(err.Error(), "Restore the missing key") ||
		strings.Contains(err.Error(), "--phase=sign-new") || !strings.Contains(err.Error(), "Do not run sign-new: cp2") ||
		!strings.Contains(err.Error(), "stratum uninstall") {
		t.Fatalf("CheckRollbackCluster = %v; want no backup and no sign-new for w1, and its rejoin", err)
	}
}

// A worker whose trust refresh would refuse cannot roll back: the check names
// what the refresh reads before it fetches anything, so the cluster check
// does not clear a worker whose rollback would then fail.
func TestWorkerRollbackRefusesWhatItsTrustRefreshWouldRefuse(t *testing.T) {
	for name, damage := range map[string]func(t *testing.T){
		"a corrupt trust bundle": func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-bundle.crt"), []byte("not PEM\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"a malformed kubelet kubeconfig": func(t *testing.T) {
			if err := os.WriteFile(workerTrustPaths().kubeletKubeconfig, []byte("{not yaml"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a malformed bootstrap-kubeconfig": func(t *testing.T) {
			if err := os.WriteFile(version.BootstrapKubeconfig, []byte("{not yaml"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"no kubelet client-CA bundle": func(t *testing.T) {
			if err := os.Remove(pki.KubeletClientBundlePath()); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			workerAfterSignNew(t, func(old, _ *testCA) *testCA { return old })
			damage(t)
			err := CheckRollback(io.Discard)
			if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "its trust refresh would refuse") {
				t.Fatalf("CheckRollback = %v; want the refresh's refusal", err)
			}
		})
	}
}

// Two CAs that start in the same second cannot be told apart, so the worker
// cannot tell whether its kubelet client certificate survives the rollback,
// and refuses rather than clear it.
func TestWorkerRollbackRefusesWhenTheNewCACannotBeTold(t *testing.T) {
	workerAfterSignNew(t, func(old, _ *testCA) *testCA { return old })
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	a, b := caStarting(t, "a", start), caStarting(t, "b", start)
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-bundle.crt"), bundle(a, b), 0o644); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kubeletClientCertPath(), nodeClientCert(t, b, &key.PublicKey, pkix.Name{CommonName: "system:node:w1", Organization: []string{nodesGroup}}), 0o600); err != nil {
		t.Fatal(err)
	}
	err = CheckRollback(io.Discard)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "cannot tell which CA") {
		t.Fatalf("CheckRollback = %v; want a refusal naming the ambiguity", err)
	}
}

// A split kubelet client-CA bundle is parsed as strictly as the refresh
// parses it, so a non-certificate block refuses the check, not the rollback.
func TestWorkerRollbackRefusesASplitBundleTheRefreshWouldRefuse(t *testing.T) {
	workerAfterSignNew(t, func(old, _ *testCA) *testCA { return old })
	split := newTestCA(t, "kubelet-client")
	der, err := x509.MarshalECPrivateKey(split.key)
	if err != nil {
		t.Fatal(err)
	}
	b := append(append([]byte{}, split.pem...), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})...)
	if err := os.WriteFile(pki.KubeletClientBundlePath(), b, 0o644); err != nil {
		t.Fatal(err)
	}
	err = CheckRollback(io.Discard)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "its trust refresh would refuse") {
		t.Fatalf("CheckRollback = %v; want the refresh's refusal of the split bundle", err)
	}
}

// A control plane's rollback reads its certificate inputs before it restores
// anything, so node state with no recorded address refuses the check too.
func TestCheckRollbackRefusesAControlPlaneWithoutItsCertificateInputs(t *testing.T) {
	prep(t)
	redirectVar(t, &version.K8sConfDir, t.TempDir())
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staticpod.Path("kube-apiserver"), []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateCAMaterialAfterPrep(t)
	if err := CheckRollback(io.Discard); err != nil {
		t.Fatalf("setup: CheckRollback = %v, want possible", err)
	}
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller"}); err != nil {
		t.Fatal(err)
	}
	err := CheckRollback(io.Discard)
	if !errors.Is(err, pki.ErrRollbackUnsafe) || !strings.Contains(err.Error(), "certificate inputs cannot be read") {
		t.Fatalf("CheckRollback = %v; want the refusal over the missing address", err)
	}
	if got := RotationDetail(); len(got) != 3 || !strings.HasPrefix(got[2], "rollback: refused on this node") {
		t.Errorf("detail = %q, want the same refusal", got)
	}
}

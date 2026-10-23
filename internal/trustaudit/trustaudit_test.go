package trustaudit

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

func (r Result) String() string {
	switch r {
	case Pass:
		return "PASS"
	case Fail:
		return "FAIL"
	case Exception:
		return "EXCEPTION"
	}
	return "unknown"
}

func testCAPEM(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testLeafPEM(t *testing.T, cn string, o ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn, Organization: o},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func tempRoots(t *testing.T) {
	t.Helper()
	data, conf, run := version.DataDir, version.ConfDir, version.RunDir
	t.Cleanup(func() { version.SetRoots(data, conf, run) })
	version.SetRoots(t.TempDir(), t.TempDir(), t.TempDir())
	for _, d := range []string{version.PKIDir, version.K8sConfDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeKubeletNode(t *testing.T, role string) {
	t.Helper()
	tempRoots(t)
	hostname := "w1"
	if role == "controller" {
		hostname = "cp1"
		if err := os.WriteFile(filepath.Join(version.PKIDir, "kubelet.crt"), testLeafPEM(t, "system:node:cp1", "system:nodes"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(version.PKIDir, "kubelet.key"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-bundle.crt"), testCAPEM(t, "kubernetes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pki.KubeletClientBundlePath(), testCAPEM(t, "stratum-kubelet-client-ca"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &nodestate.NodeState{
		Role: role, Hostname: hostname, NodeIP: "192.0.2.10",
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12",
	}
	if err := runtime.WriteKubeletConfig(st); err != nil {
		t.Fatal(err)
	}
}

func TestKubeletClientTrust(t *testing.T) {
	bundle := func() string { return pki.KubeletClientBundlePath() }
	root := func(t *testing.T) []byte {
		b, err := os.ReadFile(filepath.Join(version.PKIDir, "ca-bundle.crt"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		want  Result
		has   string
	}{
		{"split from birth", func(*testing.T) {}, Pass, "1 kubelet-client CA"},
		{"earlier cluster", func(t *testing.T) {
			if err := os.WriteFile(bundle(), root(t), 0o644); err != nil {
				t.Fatal(err)
			}
		}, Fail, pki.KubeletMigrateCommand},
		{"part way through the migration", func(t *testing.T) {
			b := append(testCAPEM(t, "stratum-kubelet-client-ca"), root(t)...)
			if err := os.WriteFile(bundle(), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}, Fail, pki.KubeletStateTrustBoth},
		{"a foreign CA", func(t *testing.T) {
			b := append(testCAPEM(t, "stratum-kubelet-client-ca"), testCAPEM(t, "someone-else")...)
			if err := os.WriteFile(bundle(), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}, Fail, "CN=someone-else"},
		{"only a foreign CA", func(t *testing.T) {
			if err := os.WriteFile(bundle(), testCAPEM(t, "someone-else"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, Fail, "neither"},
		{"missing bundle", func(t *testing.T) {
			if err := os.Remove(bundle()); err != nil {
				t.Fatal(err)
			}
		}, Fail, "cannot read"},
		{"another clientCAFile", func(t *testing.T) {
			p := runtime.KubeletConfigPath()
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			b = bytes.ReplaceAll(b, []byte(bundle()), []byte(filepath.Join(version.PKIDir, "ca-bundle.crt")))
			if err := os.WriteFile(p, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}, Fail, "clientCAFile"},
		{"no kubelet.conf", func(t *testing.T) {
			if err := os.Remove(runtime.KubeletConfigPath()); err != nil {
				t.Fatal(err)
			}
		}, Exception, "kubelet.conf not found"},
	} {
		for _, role := range []string{"controller", "worker"} {
			t.Run(tc.name+"/"+role, func(t *testing.T) {
				writeKubeletNode(t, role)
				tc.setup(t)
				got := KubeletClientTrust()
				if got.Result != tc.want || !strings.Contains(got.Detail, tc.has) || strings.Contains(got.Detail, "stratum stratum") {
					t.Errorf("got %s (%s), want %s containing %q", got.Result, got.Detail, tc.want, tc.has)
				}
			})
		}
	}
}

func TestKubeletsRefuseRootVerdict(t *testing.T) {
	rootCert := tls.Certificate{Certificate: [][]byte{[]byte("root")}}
	apiCert := tls.Certificate{Certificate: [][]byte{[]byte("apiserver")}}
	type answer struct {
		root, api int
		err       error
		expire    bool
	}
	node := func(name, ip string) corev1.Node {
		n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if ip != "" {
			n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}
		}
		return n
	}
	closed := answer{root: 401, api: 200}
	for _, tc := range []struct {
		name    string
		expired bool
		nodes   []corev1.Node
		answers map[string]answer
		want    Result
		has     string
	}{
		{"every kubelet closed", false, []corev1.Node{node("cp1", "10.0.0.1"), node("w1", "10.0.0.2")},
			map[string]answer{"10.0.0.1": closed, "10.0.0.2": {root: 401, api: 403}}, Pass, "all 2"},
		{"a kubelet accepts the root", false, []corev1.Node{node("cp1", "10.0.0.1"), node("w1", "10.0.0.2")},
			map[string]answer{"10.0.0.1": closed, "10.0.0.2": {root: 403, api: 200}}, Fail, "w1 (HTTP 403); remediate with `stratum certs"},
		{"a kubelet refuses the apiservers", false, []corev1.Node{node("w1", "10.0.0.2")},
			map[string]answer{"10.0.0.2": {root: 401, api: 401}}, Fail, "kubectl logs"},
		{"an unreachable kubelet", false, []corev1.Node{node("cp1", "10.0.0.1"), node("w1", "10.0.0.2")},
			map[string]answer{"10.0.0.1": closed, "10.0.0.2": {err: errors.New("dial")}}, Exception, "w1 (dial)"},
		{"an open kubelet outranks an unreachable one", false, []corev1.Node{node("cp1", "10.0.0.1"), node("w1", "10.0.0.2")},
			map[string]answer{"10.0.0.1": {root: 200, api: 200}, "10.0.0.2": {err: errors.New("dial")}}, Fail, "cp1"},
		{"no InternalIP", false, []corev1.Node{node("w1", "")}, nil, Exception, "no InternalIP"},
		{"no nodes", false, nil, nil, Exception, "no nodes"},
		{"the deadline passed before any probe", true, []corev1.Node{node("w1", "10.0.0.2")},
			map[string]answer{"10.0.0.2": closed}, Exception, "w1 (context canceled)"},
		{"an open kubelet outranks the deadline", false, []corev1.Node{node("cp1", "10.0.0.1"), node("w1", "10.0.0.2")},
			map[string]answer{"10.0.0.1": {root: 200, api: 200, expire: true}, "10.0.0.2": closed}, Fail, "cp1 (HTTP 200)"},
		{"nodes left once the deadline passes", false, []corev1.Node{node("cp1", "10.0.0.1"), node("w1", "10.0.0.2")},
			map[string]answer{"10.0.0.1": {root: 401, api: 200, expire: true}, "10.0.0.2": closed}, Exception, "w1 (context canceled)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.expired {
				cancel()
			}
			old := kubeletProbe
			t.Cleanup(func() { kubeletProbe = old })
			kubeletProbe = func(_ context.Context, addr string, _ []*x509.Certificate, c tls.Certificate) (int, error) {
				a := tc.answers[strings.TrimSuffix(addr, ":10250")]
				if a.expire {
					cancel()
				}
				if a.err != nil {
					return 0, a.err
				}
				if bytes.Equal(c.Certificate[0], rootCert.Certificate[0]) {
					return a.root, nil
				}
				return a.api, nil
			}
			got := kubeletsRefuseRootVerdict(ctx, tc.nodes, nil, rootCert, apiCert)
			if got.Result != tc.want || !strings.Contains(got.Detail, tc.has) || strings.Contains(got.Detail, "stratum stratum") {
				t.Errorf("got %s (%s), want %s containing %q", got.Result, got.Detail, tc.want, tc.has)
			}
		})
	}
}

func TestEtcdTrustVerdictNamesARealCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		trust    pki.EtcdTrust
		rejected bool
	}{
		{"etcd accepts a root-signed client", pki.EtcdTrust{}, false},
		{"a rotation left a stale root anchor", pki.EtcdTrust{HasEtcdCA: true, BundleExists: true, HasEtcdAnchor: true, StaleAnchors: 1, LeavesMigrated: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict := etcdTrustVerdict(tc.trust, tc.rejected, nil)
			if verdict.Result != Fail {
				t.Fatalf("precondition: expected a FAIL verdict, got %s (%s)", verdict.Result, verdict.Detail)
			}
			if strings.Contains(verdict.Detail, "KUB-") {
				t.Errorf("remediation still points at a ticket instead of a command: %s", verdict.Detail)
			}
			if !strings.Contains(verdict.Detail, pki.EtcdMigrateCommand) {
				t.Errorf("remediation does not name the command that fixes it: %s", verdict.Detail)
			}
		})
	}
}

func TestEtcdTrustVerdict(t *testing.T) {
	migrated := pki.EtcdTrust{HasEtcdCA: true, BundleExists: true, Anchors: 1, HasEtcdAnchor: true, LeavesMigrated: true}
	for _, tc := range []struct {
		name     string
		trust    pki.EtcdTrust
		rejected bool
		probeErr error
		want     Result
		has      string
	}{
		{"migrated and rejected", migrated, true, nil, Pass, "rejected"},
		{"unreadable trust outranks the probe", pki.EtcdTrust{Err: errors.New("bad bundle")}, true, errors.New("dial"), Fail, "bad bundle"},
		{"probe failed", migrated, false, errors.New("dial"), Exception, "UNVERIFIED"},
		{"live root still trusted", pki.EtcdTrust{TrustsLiveRoot: true}, true, nil, Fail, "1 Kubernetes root anchor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := etcdTrustVerdict(tc.trust, tc.rejected, tc.probeErr)
			if got.Result != tc.want || !strings.Contains(got.Detail, tc.has) {
				t.Errorf("got %s (%s), want %s containing %q", got.Result, got.Detail, tc.want, tc.has)
			}
		})
	}
}

func TestEtcdTrustOnANodeWithoutEtcd(t *testing.T) {
	tempRoots(t)
	old := etcdProbe
	t.Cleanup(func() { etcdProbe = old })
	etcdProbe = func() (bool, error) {
		t.Error("probed etcd on a node that runs none")
		return true, nil
	}
	got := EtcdTrust(context.Background())
	if got.Result != Exception || !strings.Contains(got.Detail, "etcd.yaml not found") {
		t.Errorf("got %s (%s), want EXCEPTION naming the missing manifest", got.Result, got.Detail)
	}
}

func withEtcdManifest(t *testing.T) {
	t.Helper()
	tempRoots(t)
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staticpod.Path("etcd"), []byte("kind: Pod\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEtcdTrustRunsTheProbe(t *testing.T) {
	withEtcdManifest(t)
	old := etcdProbe
	t.Cleanup(func() { etcdProbe = old })
	etcdProbe = func() (bool, error) { return false, errors.New("connection refused") }
	got := EtcdTrust(context.Background())
	if got.Result != Exception || !strings.Contains(got.Detail, "connection refused") {
		t.Errorf("got %s (%s), want the probe's EXCEPTION", got.Result, got.Detail)
	}
}

func TestEtcdTrustStopsWhenTheContextEnds(t *testing.T) {
	withEtcdManifest(t)
	old := etcdProbe
	t.Cleanup(func() { etcdProbe = old })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	etcdProbe = func() (bool, error) {
		<-release
		return true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got := EtcdTrust(ctx)
	if got.Result != Exception || !strings.HasPrefix(got.Detail, "unverified: ") {
		t.Errorf("got %s (%s), want an unverified EXCEPTION", got.Result, got.Detail)
	}
}

func TestCancelledEtcdTrustIsUnverified(t *testing.T) {
	tempRoots(t)
	withEtcdManifest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := EtcdTrust(ctx)
	if got.Result != Exception || !strings.HasPrefix(got.Detail, "unverified: ") || !strings.Contains(got.Detail, context.Canceled.Error()) {
		t.Errorf("EtcdTrust on a cancelled context = %s (%s), want an unverified EXCEPTION", got.Result, got.Detail)
	}
}

func TestKubeletsRefuseRootFailsWhenNodesCannotBeListed(t *testing.T) {
	tempRoots(t)
	if err := pki.GenerateAll("cp1", "192.0.2.10", nil); err != nil {
		t.Fatal(err)
	}
	cs := k8sfake.NewClientset()
	cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := KubeletsRefuseRoot(ctx, cs)
	if got.Result != Fail || !strings.Contains(got.Detail, "could not list nodes") {
		t.Errorf("KubeletsRefuseRoot with an unlistable cluster = %s (%s), want FAIL: a check that never ran must not pass", got.Result, got.Detail)
	}
}

func TestEmptyKubeletClientTrustFails(t *testing.T) {
	got := kubeletClientTrustVerdict(pki.KubeletClientTrust{})
	if got.Result != Fail || !strings.Contains(got.Detail, "trusts no client") {
		t.Errorf("an empty kubelet-client bundle = %s (%s), want FAIL naming that it trusts no client", got.Result, got.Detail)
	}
}

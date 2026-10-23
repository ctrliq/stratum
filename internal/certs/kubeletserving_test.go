package certs

import (
	"io"

	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

var (
	servingDNS = []string{"w1"}
	servingIPs = []net.IP{net.ParseIP("192.0.2.20").To4()}
)

type servingSpec struct {
	subject             pkix.Name
	dns                 []string
	ips                 []net.IP
	eku                 x509.ExtKeyUsage
	notBefore, notAfter time.Time
}

func defaultServing() servingSpec {
	return servingSpec{subject: nodeW1, dns: servingDNS, ips: servingIPs, eku: x509.ExtKeyUsageServerAuth}
}

func servingCertFor(t *testing.T, ca *testCA, pub crypto.PublicKey, s servingSpec) []byte {
	t.Helper()
	if s.notBefore.IsZero() {
		s.notBefore, s.notAfter = time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      s.subject,
		NotBefore:    s.notBefore,
		NotAfter:     s.notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{s.eku},
		DNSNames:     s.dns,
		IPAddresses:  s.ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// writeServingPair lays out a kubelet cert dir the way the kubelet's
// certificate manager does: a timestamped pair and the current symlink.
func writeServingPair(t *testing.T, dir string, certPEM []byte) (current, target string) {
	t.Helper()
	current = filepath.Join(dir, pki.KubeletServingCurrent)
	target = filepath.Join(dir, "kubelet-server-2026-01-01-00-00-00.pem")
	content := append(slices.Clone(certPEM), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{0}})...)
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, current); err != nil {
		t.Fatal(err)
	}
	return current, target
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRenewKubeletServingCert(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	oldCA := caStarting(t, "ca", start.Add(-48*time.Hour))
	newCA := caStarting(t, "ca", start.Add(-time.Hour))
	strangerCA := caStarting(t, "stranger", start.Add(-2*time.Hour))
	signWith := func(ca *testCA, mutate func(*servingSpec), otherKey bool) func(*testing.T, []byte) []byte {
		return func(t *testing.T, csrPEM []byte) []byte {
			req := parseCSRPEM(t, csrPEM)
			s := servingSpec{subject: req.Subject, dns: req.DNSNames, ips: req.IPAddresses, eku: x509.ExtKeyUsageServerAuth}
			if mutate != nil {
				mutate(&s)
			}
			var pub crypto.PublicKey = req.PublicKey
			if otherKey {
				pub = &newKey(t).PublicKey
			}
			return servingCertFor(t, ca, pub, s)
		}
	}
	for _, tc := range []struct {
		name        string
		current     *servingSpec
		currentCA   *testCA
		noCurrent   bool
		sign        func(*testing.T, []byte) []byte
		requestErr  error
		storeErr    error
		wantErr     string
		wantRenewed bool
	}{
		{name: "renewed under the new CA", sign: signWith(newCA, nil, false), wantRenewed: true},
		{name: "already signed by the new CA", currentCA: newCA},
		{name: "no serving cert yet", noCurrent: true, wantErr: "has no serving cert yet"},
		{name: "current cert carries no SANs", current: &servingSpec{subject: nodeW1, eku: x509.ExtKeyUsageServerAuth}, wantErr: "carries no SANs"},
		{name: "current cert not a node", current: &servingSpec{subject: pkix.Name{CommonName: "admin"}, dns: servingDNS, eku: x509.ExtKeyUsageServerAuth}, wantErr: "not a node identity"},
		{name: "request not approved", requestErr: errors.New("CSR csr-x was not approved and signed: context deadline exceeded"), wantErr: "not approved"},
		{name: "signed by the replaced CA", sign: signWith(oldCA, nil, false), wantErr: "the CA this rotation replaces"},
		{name: "signed by an untrusted CA", sign: signWith(strangerCA, nil, false), wantErr: "does not trust"},
		{name: "issued for other SANs", sign: signWith(newCA, func(s *servingSpec) { s.ips = []net.IP{net.ParseIP("192.0.2.99")} }, false), wantErr: "not the current cert's"},
		{name: "issued for another DNS name", sign: signWith(newCA, func(s *servingSpec) { s.dns = []string{"w2"} }, false), wantErr: "not the current cert's"},
		{name: "issued already expired", sign: signWith(newCA, func(s *servingSpec) {
			s.notBefore, s.notAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
		}, false), wantErr: "not valid now"},
		{name: "issued not yet valid", sign: signWith(newCA, func(s *servingSpec) {
			s.notBefore, s.notAfter = time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
		}, false), wantErr: "not valid now"},
		{name: "issued for another node", sign: signWith(newCA, func(s *servingSpec) { s.subject.CommonName = "system:node:w2" }, false), wantErr: `not "system:node:w1"`},
		{name: "issued without server auth", sign: signWith(newCA, func(s *servingSpec) { s.eku = x509.ExtKeyUsageClientAuth }, false), wantErr: "server auth"},
		{name: "issued for another key", sign: signWith(newCA, nil, true), wantErr: "does not carry the key"},
		{name: "store fails", sign: signWith(newCA, nil, false), storeErr: errors.New("disk full"), wantErr: "disk full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			certPath := filepath.Join(dir, pki.KubeletServingCurrent)
			if !tc.noCurrent {
				spec := defaultServing()
				if tc.current != nil {
					spec = *tc.current
				}
				signer := oldCA
				if tc.currentCA != nil {
					signer = tc.currentCA
				}
				writeServingPair(t, dir, servingCertFor(t, signer, &newKey(t).PublicKey, spec))
			}
			var requested, stored bool
			var storedCert []byte
			renewed, err := renewKubeletServingCert(t.Context(), certPath, []*x509.Certificate{oldCA.cert, newCA.cert}, kubeletRenewOps{
				request: func(_ context.Context, csrPEM []byte, key crypto.Signer) ([]byte, error) {
					requested = true
					req := parseCSRPEM(t, csrPEM)
					if req.Subject.CommonName != "system:node:w1" || !slices.Equal(req.Subject.Organization, []string{"system:nodes"}) {
						t.Errorf("CSR subject = %v, want the current node identity", req.Subject)
					}
					if !slices.Equal(req.DNSNames, servingDNS) || !slices.EqualFunc(req.IPAddresses, servingIPs, net.IP.Equal) {
						t.Errorf("CSR SANs = %v %v, want the current cert's %v %v", req.DNSNames, req.IPAddresses, servingDNS, servingIPs)
					}
					if !key.Public().(*ecdsa.PublicKey).Equal(req.PublicKey) {
						t.Errorf("CSR key differs from the signer handed to request")
					}
					if tc.requestErr != nil {
						return nil, tc.requestErr
					}
					return tc.sign(t, csrPEM), nil
				},
				store: func(c, _ []byte) error {
					stored, storedCert = true, c
					return tc.storeErr
				},
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("renew: %v", err)
				}
				if renewed != tc.wantRenewed {
					t.Fatalf("renewed = %v, want %v", renewed, tc.wantRenewed)
				}
				if !tc.wantRenewed {
					if requested || stored {
						t.Fatalf("requested=%v stored=%v for a cert the new CA already signed; want neither", requested, stored)
					}
					return
				}
				leaf, err := pki.FirstCertificate(storedCert)
				if err != nil || leaf.CheckSignatureFrom(newCA.cert) != nil {
					t.Fatalf("stored cert not signed by the new CA: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			}
			if tc.storeErr == nil && stored {
				t.Fatal("stored a certificate the checks refused")
			}
			if renewed {
				t.Fatal("reported renewed alongside an error")
			}
			if errors.Is(err, errNoKubeletServingCert) != tc.noCurrent {
				t.Fatalf("errors.Is(err, errNoKubeletServingCert) = %v for %v", !tc.noCurrent, err)
			}
			if (tc.noCurrent || tc.current != nil) && requested {
				t.Fatal("sent a CSR before the current cert was checked")
			}
		})
	}
}

// TestKubeletServingSwapOnlyOnSuccess runs the renewal against a real cert dir
// and the real store: a CSR that is never approved leaves the kubelet's current
// serving pair exactly as it was, and an approved one swaps the current link
// to a new pair while keeping the previous file.
func TestKubeletServingSwapOnlyOnSuccess(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	oldCA := caStarting(t, "ca", start.Add(-48*time.Hour))
	newCA := caStarting(t, "ca", start.Add(-time.Hour))
	anchor := []*x509.Certificate{oldCA.cert, newCA.cert}
	approve := func(_ context.Context, csrPEM []byte, _ crypto.Signer) ([]byte, error) {
		req := parseCSRPEM(t, csrPEM)
		return servingCertFor(t, newCA, req.PublicKey, servingSpec{subject: req.Subject, dns: req.DNSNames, ips: req.IPAddresses, eku: x509.ExtKeyUsageServerAuth}), nil
	}
	pending := func(context.Context, []byte, crypto.Signer) ([]byte, error) {
		return nil, errors.New("CSR csr-x was not approved and signed: context deadline exceeded")
	}

	dir := t.TempDir()
	current, previous := writeServingPair(t, dir, servingCertFor(t, oldCA, &newKey(t).PublicKey, defaultServing()))
	before, err := os.ReadFile(previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := renewKubeletServingCert(t.Context(), current, anchor, kubeletRenewOps{request: pending, store: storeKubeletServingCert(current)}); err == nil {
		t.Fatal("renewal with no approval reported success")
	}
	if target, err := os.Readlink(current); err != nil || target != previous {
		t.Fatalf("after a failed renewal current -> %q (%v), want it still on %q", target, err, previous)
	}

	renewed, err := renewKubeletServingCert(t.Context(), current, anchor, kubeletRenewOps{request: approve, store: storeKubeletServingCert(current)})
	if err != nil || !renewed {
		t.Fatalf("renew: renewed=%v err=%v", renewed, err)
	}
	if target, err := os.Readlink(current); err != nil || target == previous {
		t.Fatalf("current still points at the previous pair: %q, %v", target, err)
	}
	leaf, err := pki.ReadKubeletServingLeaf(current)
	if err != nil || leaf.CheckSignatureFrom(newCA.cert) != nil {
		t.Fatalf("current does not hold a new-CA serving cert: %v", err)
	}
	if after, err := os.ReadFile(previous); err != nil || string(after) != string(before) {
		t.Fatalf("previous pair changed: %v", err)
	}
}

func TestCheckServingCertSurvives(t *testing.T) {
	oldCA, newCA := newTestCA(t, "ca"), newTestCA(t, "ca")
	for _, tc := range []struct {
		name    string
		signer  *testCA
		garbage bool
		dropped []*x509.Certificate
		wantErr bool
	}{
		{name: "still signed by the dropped CA", signer: oldCA, dropped: []*x509.Certificate{oldCA.cert}, wantErr: true},
		{name: "renewed under the new CA", signer: newCA, dropped: []*x509.Certificate{oldCA.cert}},
		{name: "nothing dropped", signer: oldCA},
		{name: "missing serving cert refuses finalize", dropped: []*x509.Certificate{oldCA.cert}, wantErr: true},
		{name: "unreadable serving cert refuses finalize", garbage: true, dropped: []*x509.Certificate{oldCA.cert}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, pki.KubeletServingCurrent)
			switch {
			case tc.signer != nil:
				writeServingPair(t, dir, servingCertFor(t, tc.signer, &newKey(t).PublicKey, defaultServing()))
			case tc.garbage:
				if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := checkServingCertSurvives(tc.dropped, path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "--phase="+PhaseSignNew) {
				t.Errorf("refusal does not name sign-new: %v", err)
			}
		})
	}
}

// The finalize call site: a worker whose client cert was renewed but whose
// serving cert is still old-signed must abort before any trust file changes.
func TestWorkerFinalizeRefusesOldSignedServingCert(t *testing.T) {
	oldCA, newCA := newTestCA(t, "ca"), newTestCA(t, "ca")
	url := clusterInfo(t, newCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA))
	p := node(t, bundle(oldCA, newCA), url)
	savedPKI, savedKC, savedBootstrap := version.PKIDir, version.KubeconfigDir, version.BootstrapKubeconfig
	t.Cleanup(func() {
		version.PKIDir, version.KubeconfigDir, version.BootstrapKubeconfig = savedPKI, savedKC, savedBootstrap
	})
	version.PKIDir, version.KubeconfigDir, version.BootstrapKubeconfig = filepath.Dir(p.caBundle), filepath.Dir(p.kubeletKubeconfig), p.bootstrapKubeconfig
	certDir := filepath.Dir(kubeletClientCertPath())
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kubeletClientCertPath(), clientCertSignedBy(t, newCA), 0o600); err != nil {
		t.Fatal(err)
	}
	writeServingPair(t, certDir, servingCertFor(t, oldCA, &newKey(t).PublicKey, defaultServing()))
	before := snapshot(t, p)
	err := workerRefreshTrust(io.Discard, CARotateOptions{Phase: PhaseFinalize, Quiet: true}, trustNarrow)
	if err == nil || !strings.Contains(err.Error(), "serving cert") || !strings.Contains(err.Error(), "still signed by a CA this finalize drops") {
		t.Fatalf("got %v", err)
	}
	assertCleanAbort(t, p, before)
	if err := os.Remove(kubeletServingCertPath()); err != nil {
		t.Fatal(err)
	}
	err = workerRefreshTrust(io.Discard, CARotateOptions{Phase: PhaseFinalize, Quiet: true}, trustNarrow)
	if err == nil || !strings.Contains(err.Error(), "cannot verify the kubelet serving cert") {
		t.Fatalf("missing serving cert must also refuse finalize, got %v", err)
	}
	assertCleanAbort(t, p, before)
}

func TestRenewServingCertSkipsAStaticServingLeaf(t *testing.T) {
	savedConf, savedPKI := version.K8sConfDir, version.PKIDir
	t.Cleanup(func() { version.K8sConfDir, version.PKIDir = savedConf, savedPKI })
	start := time.Now().Truncate(time.Second)
	oldCA := caStarting(t, "ca", start.Add(-48*time.Hour))
	newCA := caStarting(t, "ca", start.Add(-time.Hour))
	dir := t.TempDir()
	current, _ := writeServingPair(t, dir, servingCertFor(t, oldCA, &newKey(t).PublicKey, defaultServing()))
	renew := func() error {
		_, err := renewServingCert(io.Discard, CARotateOptions{Quiet: true}, PhaseFinalize, filepath.Join(dir, "no-such.kubeconfig"),
			current, []*x509.Certificate{oldCA.cert, newCA.cert})
		return err
	}
	for _, tc := range []struct {
		name      string
		state     *nodestate.NodeState
		kubeletCA bool
		wantErr   string
	}{
		{name: "a controller pinning the static kubelet.crt leaf", state: &nodestate.NodeState{Role: "controller", Hostname: "cp1"}, kubeletCA: true},
		{name: "a token-joined worker bootstraps", state: &nodestate.NodeState{Role: "worker", Hostname: "w1"}, wantErr: "request a kubelet serving cert"},
		{name: "a CIS controller bootstraps", state: &nodestate.NodeState{Role: "controller", Hostname: "cp1", CIS: true}, kubeletCA: true, wantErr: "request a kubelet serving cert"},
		{name: "no node state is refused, not read as a static leaf", wantErr: "read the node state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version.K8sConfDir, version.PKIDir = t.TempDir(), t.TempDir()
			if tc.state != nil {
				if err := nodestate.SaveState(tc.state); err != nil {
					t.Fatal(err)
				}
			}
			if tc.kubeletCA {
				if err := os.WriteFile(filepath.Join(version.PKIDir, "kubelet.crt"), []byte("leaf"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := renew()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("a static serving leaf has nothing to renew, got %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestRenewServingCertRefusesMissingBootstrapCert(t *testing.T) {
	saved := kubeletBootstrapsServing
	t.Cleanup(func() { kubeletBootstrapsServing = saved })
	kubeletBootstrapsServing = func() (bool, error) { return true, nil }
	for _, phase := range []string{PhaseSignNew, PhaseFinalize} {
		t.Run(phase, func(t *testing.T) {
			_, err := renewServingCert(io.Discard, CARotateOptions{Quiet: true}, phase, "unused.kubeconfig",
				filepath.Join(t.TempDir(), pki.KubeletServingCurrent), nil)
			if !errors.Is(err, errNoKubeletServingCert) || !strings.Contains(err.Error(), "CSR must be approved") ||
				!strings.Contains(err.Error(), phase) {
				t.Fatalf("renewServingCert = %v; missing serving cert must refuse %s", err, phase)
			}
		})
	}
}

func TestCheckKubeletServingTrusted(t *testing.T) {
	newCA := newTestCA(t, "ca")
	mk := func(name, ip string, cp bool) corev1.Node {
		n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
		if cp {
			n.Labels[k8sutil.ControlPlaneLabel] = "true"
		}
		if ip != "" {
			n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: name}, {Type: corev1.NodeInternalIP, Address: ip}}
		}
		return n
	}
	nodes := []corev1.Node{mk("cp1", "192.0.2.10", true), mk("w1", "192.0.2.20", false), mk("w2", "", false)}
	probeWith := func(results map[string]error, seen *[]string) pki.KubeletProbe {
		var mu sync.Mutex
		return func(_ context.Context, addr string, roots []*x509.Certificate) error {
			mu.Lock()
			*seen = append(*seen, addr)
			mu.Unlock()
			if len(roots) != 1 || !roots[0].Equal(newCA.cert) {
				t.Errorf("probe of %s verified against %d roots, want only the new CA", addr, len(roots))
			}
			return results[addr]
		}
	}
	untrusted := errors.Join(pki.ErrKubeletServingUntrusted, errors.New("x509: certificate signed by unknown authority"))
	for _, tc := range []struct {
		name       string
		cisCP      bool
		noCPIP     bool
		results    map[string]error
		wantErr    string
		wantRemedy string
		wantProbe  []string
	}{
		{name: "every kubelet on the new CA", wantProbe: []string{"192.0.2.10:10250", "192.0.2.20:10250"}},
		{name: "a worker still on the old CA", results: map[string]error{"192.0.2.20:10250": untrusted}, wantErr: "node w1", wantRemedy: "--phase=sign-new --yes"},
		{name: "a control plane still on the old CA", results: map[string]error{"192.0.2.10:10250": untrusted}, wantErr: "node cp1", wantRemedy: "systemctl restart stratum"},
		{name: "a control plane without an InternalIP refuses finalize", noCPIP: true, wantErr: "control plane cp1", wantRemedy: "systemctl restart stratum", wantProbe: []string{"192.0.2.20:10250"}},
		{name: "CIS control planes renew in their own finalize", cisCP: true, results: map[string]error{"192.0.2.10:10250": untrusted}, wantProbe: []string{"192.0.2.20:10250"}},
		{name: "an unreachable worker kubelet is not proof either way", results: map[string]error{"192.0.2.20:10250": errors.New("connection refused")}},
		{name: "an unreachable control-plane kubelet refuses", results: map[string]error{"192.0.2.10:10250": errors.New("connection refused")}, wantErr: "control plane cp1", wantRemedy: "systemctl restart stratum"},
		{name: "a CIS control plane is not probed, reachable or not", cisCP: true, results: map[string]error{"192.0.2.10:10250": errors.New("connection refused")}, wantProbe: []string{"192.0.2.20:10250"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			checkedNodes := slices.Clone(nodes)
			if tc.noCPIP {
				checkedNodes[0].Status.Addresses = nil
			}
			err := checkKubeletServingTrusted(t.Context(), io.Discard, checkedNodes, tc.cisCP, newCA.cert, probeWith(tc.results, &seen))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "logs/exec") ||
				!strings.Contains(err.Error(), "new CA") || !strings.Contains(err.Error(), tc.wantRemedy) {
				t.Fatalf("err = %v, want it to name %q, the new CA and the remedy %q", err, tc.wantErr, tc.wantRemedy)
			}
			slices.Sort(seen)
			if tc.wantProbe != nil && !slices.Equal(seen, tc.wantProbe) {
				t.Errorf("probed %v, want %v", seen, tc.wantProbe)
			}
		})
	}
}

// promotedControlPlane lays out a control plane past sign-new: the new root
// signs and ca-bundle.crt still holds the old one too.
func promotedControlPlane(t *testing.T) *pki.CA {
	t.Helper()
	prep(t)
	// newestCA tells the roots apart by NotBefore, which has one-second
	// resolution.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(1100 * time.Millisecond)))
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if err := pki.PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := pki.PromoteSA(); err != nil {
		t.Fatal(err)
	}
	if err := pki.MarkCAPromoted(); err != nil {
		t.Fatal(err)
	}
	newRoot, err := pki.LoadCA("ca")
	if err != nil {
		t.Fatal(err)
	}
	return newRoot
}

// kubeletRestarts counts the kubelet restarts a stubbed finalize renewal asks
// for, and fails them with err when it is set.
type kubeletRestarts struct {
	n   int
	err error
	// servingNew is whether the running kubelet serves a new-CA cert: it
	// changes only when a restart succeeds, to whatever pair is on disk then.
	servingNew bool
}

// stubFinalizeRenewal points the finalize renewal at certDir and at request,
// records kubelet restarts, and writes kubelet.conf.
func stubFinalizeRenewal(t *testing.T, certDir, kubeletConf string, request kubeletRequest) *kubeletRestarts {
	t.Helper()
	savedArgs, savedRequest, savedRestart, savedConf, savedKC := kubeletArgs, requestServingCert, restartKubelet, version.K8sConfDir, version.KubeconfigDir
	savedBootstraps, savedServes := kubeletBootstrapsServing, localKubeletServes
	t.Cleanup(func() {
		kubeletArgs, requestServingCert, restartKubelet, version.K8sConfDir, version.KubeconfigDir = savedArgs, savedRequest, savedRestart, savedConf, savedKC
		kubeletBootstrapsServing, localKubeletServes = savedBootstraps, savedServes
	})
	restarts := &kubeletRestarts{}
	current := filepath.Join(certDir, pki.KubeletServingCurrent)
	onDiskNew := func() bool {
		root, err := pki.LoadCA("ca")
		if err != nil {
			return false
		}
		leaf, err := pki.ReadKubeletServingLeaf(current)
		return err == nil && leaf.CheckSignatureFrom(root.Cert) == nil
	}
	restartKubelet = func() error {
		restarts.n++
		if restarts.err != nil {
			return restarts.err
		}
		restarts.servingNew = onDiskNew()
		return nil
	}
	localKubeletServes = func(*x509.Certificate) error {
		if restarts.servingNew {
			return nil
		}
		return pki.ErrKubeletServingUntrusted
	}
	bootstraps := strings.Contains(kubeletConf, "serverTLSBootstrap: true")
	kubeletBootstrapsServing = func() (bool, error) { return bootstraps, nil }
	kubeletArgs = func() ([]string, error) { return []string{"--root-dir=/elsewhere", "--cert-dir=" + certDir}, nil }
	requestServingCert = func(string) kubeletRequest { return request }
	version.K8sConfDir, version.KubeconfigDir = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(version.K8sConfDir, "kubelet.conf"), []byte(kubeletConf), 0o600); err != nil {
		t.Fatal(err)
	}
	return restarts
}

func signedByRoot(t *testing.T, root *pki.CA) kubeletRequest {
	return func(_ context.Context, csrPEM []byte, _ crypto.Signer) ([]byte, error) {
		req := parseCSRPEM(t, csrPEM)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: req.Subject,
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			DNSNames: req.DNSNames, IPAddresses: req.IPAddresses,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, root.Cert, req.PublicKey, root.Key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
	}
}

func TestCPRenewServingCert(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kubeletConf string
		wantRenewed bool
	}{
		{name: "a CIS control plane bootstraps its serving cert: renewed under the new root", kubeletConf: "serverTLSBootstrap: true\n", wantRenewed: true},
		{name: "a static serving leaf: nothing to renew", kubeletConf: "serverTLSBootstrap: false\ntlsCertFile: /pki/kubelet.crt\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newRoot := promotedControlPlane(t)
			certDir := t.TempDir()
			current, previous := writeServingPair(t, certDir, servingCertFor(t, newTestCA(t, "ca"), &newKey(t).PublicKey, defaultServing()))
			requested := false
			sign := signedByRoot(t, newRoot)
			restarts := stubFinalizeRenewal(t, certDir, tc.kubeletConf, func(ctx context.Context, csrPEM []byte, key crypto.Signer) ([]byte, error) {
				requested = true
				return sign(ctx, csrPEM, key)
			})
			if !tc.wantRenewed {
				kubeletArgs = func() ([]string, error) { return nil, errors.New("no running supervised kubelet") }
			}
			if err := cpRenewServingCert(io.Discard, CARotateOptions{Quiet: true}); err != nil {
				t.Fatalf("cpRenewServingCert: %v", err)
			}
			if requested != tc.wantRenewed {
				t.Fatalf("requested = %v, want %v", requested, tc.wantRenewed)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.wantRenewed]; restarts.n != want {
				t.Fatalf("kubelet restarted %d times, want %d: a renewed serving cert is served only once the kubelet restarts", restarts.n, want)
			}
			target, err := os.Readlink(current)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantRenewed {
				if target != previous {
					t.Fatalf("a static-leaf node's serving pair moved to %s", target)
				}
				return
			}
			leaf, err := pki.ReadKubeletServingLeaf(current)
			if err != nil || leaf.CheckSignatureFrom(newRoot.Cert) != nil {
				t.Fatalf("the serving pair in the kubelet's --cert-dir is not signed by the new root: %v", err)
			}
			requested = false
			if err := cpRenewServingCert(io.Discard, CARotateOptions{Quiet: true}); err != nil {
				t.Fatalf("cpRenewServingCert again: %v", err)
			}
			if requested || restarts.n != 1 {
				t.Fatalf("a serving cert already on the new root was requested again (%v) or the kubelet restarted again (%d restarts)", requested, restarts.n)
			}
		})
	}
}

// A renewed serving cert the kubelet could not be restarted onto must refuse
// finalize: until the restart the kubelet still serves the old-CA cert.
func TestCPFinalizeRefusesWhenTheKubeletCannotRestart(t *testing.T) {
	newRoot := promotedControlPlane(t)
	certDir := t.TempDir()
	writeServingPair(t, certDir, servingCertFor(t, newTestCA(t, "ca"), &newKey(t).PublicKey, defaultServing()))
	restarts := stubFinalizeRenewal(t, certDir, "serverTLSBootstrap: true\n", signedByRoot(t, newRoot))
	restarts.err = errors.New("kubelet did not come back within 1m30s of restart")
	err := cpFinalize(io.Discard, CARotateOptions{Phase: PhaseFinalize, Quiet: true, SkipTokenCheck: true})
	if err == nil || !strings.Contains(err.Error(), "finalize refused") || !strings.Contains(err.Error(), "restart the kubelet") {
		t.Fatalf("cpFinalize = %v, want a refusal naming the kubelet restart", err)
	}
	if after, err := pki.CABundleCertCount("ca"); err != nil || after != 2 {
		t.Fatalf("a refused finalize left ca-bundle.crt with %d CAs (%v), want both still trusted", after, err)
	}
	if restarts.n != 1 {
		t.Fatalf("the kubelet restart was attempted %d times, want once", restarts.n)
	}
}

// A CIS control plane whose serving cert cannot be renewed must not narrow its
// trust: the apiservers would then refuse its kubelet.
func TestCPFinalizeRefusesWhenServingRenewalFails(t *testing.T) {
	promotedControlPlane(t)
	certDir := t.TempDir()
	writeServingPair(t, certDir, servingCertFor(t, newTestCA(t, "ca"), &newKey(t).PublicKey, defaultServing()))
	restarts := stubFinalizeRenewal(t, certDir, "serverTLSBootstrap: true\n", func(context.Context, []byte, crypto.Signer) ([]byte, error) {
		return nil, errors.New("CSR csr-x was not approved and signed: context deadline exceeded")
	})
	before, err := pki.CABundleCertCount("ca")
	if err != nil || before != 2 {
		t.Fatalf("fixture: ca-bundle.crt holds %d CAs (%v), want 2", before, err)
	}
	err = cpFinalize(io.Discard, CARotateOptions{Phase: PhaseFinalize, Quiet: true, SkipTokenCheck: true})
	if err == nil || !strings.Contains(err.Error(), "finalize refused") || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("cpFinalize = %v, want a refusal naming the unapproved CSR", err)
	}
	if after, err := pki.CABundleCertCount("ca"); err != nil || after != 2 {
		t.Fatalf("a refused finalize left ca-bundle.crt with %d CAs (%v), want both still trusted", after, err)
	}
	if restarts.n != 0 {
		t.Fatalf("the kubelet restarted %d times with nothing renewed", restarts.n)
	}
}

// A finalize whose kubelet restart failed after the renewal must restart the
// kubelet when it is retried: the renewal is then a no-op, but the running
// kubelet still serves the old-CA cert it loaded at its last start.
func TestCPRenewServingCertRestartsOnARetryAfterAFailedRestart(t *testing.T) {
	newRoot := promotedControlPlane(t)
	certDir := t.TempDir()
	writeServingPair(t, certDir, servingCertFor(t, newTestCA(t, "ca"), &newKey(t).PublicKey, defaultServing()))
	restarts := stubFinalizeRenewal(t, certDir, "serverTLSBootstrap: true\n", signedByRoot(t, newRoot))
	restarts.err = errors.New("kubelet did not come back within 1m30s of restart")
	if err := cpRenewServingCert(io.Discard, CARotateOptions{Quiet: true}); err == nil {
		t.Fatal("the first attempt succeeded although the kubelet restart failed")
	}
	restarts.err = nil
	if err := cpRenewServingCert(io.Discard, CARotateOptions{Quiet: true}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if restarts.n != 2 || !restarts.servingNew {
		t.Fatalf("restarted %d times (serving the new cert: %v), want a second restart onto the renewed cert", restarts.n, restarts.servingNew)
	}
	if err := cpRenewServingCert(io.Discard, CARotateOptions{Quiet: true}); err != nil || restarts.n != 2 {
		t.Fatalf("a kubelet already serving the new cert was restarted again (%d restarts, %v)", restarts.n, err)
	}
}

func TestFinalizeKubeletCheck(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	oldCA := caStarting(t, "ca", start.Add(-48*time.Hour))
	newCA := caStarting(t, "ca", start.Add(-time.Hour))
	savedRoots, savedBootstrap, savedProbe := kubeletServingRoots, controlPlanesBootstrapServing, probeKubelet
	t.Cleanup(func() {
		kubeletServingRoots, controlPlanesBootstrapServing, probeKubelet = savedRoots, savedBootstrap, savedProbe
	})
	kubeletServingRoots = func() ([]*x509.Certificate, error) { return []*x509.Certificate{oldCA.cert, newCA.cert}, nil }
	cp := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp1", Labels: map[string]string{k8sutil.ControlPlaneLabel: "true"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.10"}}}}
	for _, tc := range []struct {
		name      string
		bootstrap bool
		wantErr   string
		wantProbe bool
	}{
		{name: "a default-profile control plane is probed and refused", wantErr: "node cp1", wantProbe: true},
		{name: "a bootstrapping profile's control plane renews in its own finalize", bootstrap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controlPlanesBootstrapServing = func() (bool, error) { return tc.bootstrap, nil }
			probed := false
			probeKubelet = func(_ context.Context, _ string, roots []*x509.Certificate) error {
				probed = true
				if len(roots) != 1 || !roots[0].Equal(newCA.cert) {
					t.Errorf("probed against %d roots, want only the newest CA in the bundle", len(roots))
				}
				return pki.ErrKubeletServingUntrusted
			}
			err := finalizeKubeletCheck(t.Context(), io.Discard, []corev1.Node{cp})
			if probed != tc.wantProbe {
				t.Fatalf("probed = %v, want %v", probed, tc.wantProbe)
			}
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	controlPlanesBootstrapServing = func() (bool, error) { return false, errors.New("no node state") }
	if err := finalizeKubeletCheck(t.Context(), io.Discard, []corev1.Node{cp}); err == nil {
		t.Fatal("an unreadable profile let the check pass")
	}
}

func TestAbandonCSR(t *testing.T) {
	cs := fake.NewSimpleClientset(&certificatesv1.CertificateSigningRequest{ObjectMeta: metav1.ObjectMeta{Name: "csr-x"}})
	if got := abandonCSR(cs, "csr-x"); !strings.Contains(got, "deleted") {
		t.Fatalf("abandonCSR = %q, want it to report the deletion", got)
	}
	if _, err := cs.CertificatesV1().CertificateSigningRequests().Get(t.Context(), "csr-x", metav1.GetOptions{}); err == nil {
		t.Fatal("the abandoned CSR is still there")
	}
	if got := abandonCSR(cs, "csr-y"); !strings.Contains(got, "kubectl delete csr csr-y") {
		t.Fatalf("abandonCSR of a CSR it could not delete = %q, want the command that deletes it", got)
	}
}

// A restart proves only that a new kubelet process exists. One that still
// serves no new-CA cert afterwards must refuse finalize, since the cluster
// preflight skips CIS control planes and nothing else would catch it.
func TestCPFinalizeRefusesWhenTheRestartedKubeletServesNoNewCACert(t *testing.T) {
	newRoot := promotedControlPlane(t)
	certDir := t.TempDir()
	writeServingPair(t, certDir, servingCertFor(t, newTestCA(t, "ca"), &newKey(t).PublicKey, defaultServing()))
	restarts := stubFinalizeRenewal(t, certDir, "serverTLSBootstrap: true\n", signedByRoot(t, newRoot))
	restartKubelet = func() error { restarts.n++; return nil }
	savedWait, savedPoll := kubeletServesWait, kubeletServesPoll
	t.Cleanup(func() { kubeletServesWait, kubeletServesPoll = savedWait, savedPoll })
	kubeletServesWait, kubeletServesPoll = 50*time.Millisecond, 10*time.Millisecond
	err := cpRenewServingCert(io.Discard, CARotateOptions{Quiet: true})
	if err == nil || !errors.Is(err, pki.ErrKubeletServingUntrusted) || !strings.Contains(err.Error(), "restarted kubelet does not serve") {
		t.Fatalf("cpRenewServingCert = %v, want a refusal that the restarted kubelet serves no new-CA cert", err)
	}
	if restarts.n != 1 {
		t.Fatalf("kubelet restarted %d times, want 1", restarts.n)
	}
}

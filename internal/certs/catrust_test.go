package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/pki"
)

// This file stands its own fixtures up rather than using prep(t) from
// helper_test.go: the trust refresh reads only the paths it is handed, so a
// trustPaths built here needs no version.* redirection, and keeping it local
// means a change to the shared helper cannot reach these tests.

// ── fixtures ─────────────────────────────────────────────────────────────────

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	return caStarting(t, cn, time.Now().Add(-time.Hour))
}

// caStarting is a CA whose validity starts at notBefore, so a test can say which
// of two CAs a rotation introduced.
func caStarting(t *testing.T, cn string, notBefore time.Time) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// serving issues an apiserver-style serving cert. ips nil means "no IP SANs",
// which is how the SAN-mismatch case is built.
func (ca *testCA) serving(t *testing.T, ips []net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "kube-apiserver"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"kubernetes", "kubernetes.default"},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}
}

func bundle(cas ...*testCA) []byte {
	var out []byte
	for _, ca := range cas {
		out = append(out, ca.pem...)
	}
	return out
}

// clusterInfo serves the kube-public cluster-info ConfigMap over TLS, publishing
// publish verbatim as certificate-authority-data.
func clusterInfo(t *testing.T, serving tls.Certificate, publish []byte) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/kube-public/configmaps/cluster-info" {
			http.NotFound(w, r)
			return
		}
		kc := clientcmdapi.NewConfig()
		kc.Clusters[""] = &clientcmdapi.Cluster{Server: "https://apiserver", CertificateAuthorityData: publish}
		raw, err := clientcmd.Write(*kc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"kubeconfig": string(raw)},
		}); err != nil {
			t.Errorf("encode cluster-info: %v", err)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serving}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL
}

// node lays out a joined worker's five trust files and returns their paths.
func node(t *testing.T, onDisk []byte, server string) trustPaths {
	t.Helper()
	base := t.TempDir()
	pkiDir := filepath.Join(base, "pki")
	kcDir := filepath.Join(base, "kubeconfig")
	for _, d := range []string{pkiDir, kcDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p := trustPaths{
		caBundle:            filepath.Join(pkiDir, "ca-bundle.crt"),
		kubeletClientCA:     filepath.Join(pkiDir, "kubelet-client-ca-bundle.crt"),
		caCert:              filepath.Join(pkiDir, "ca.crt"),
		kubeletKubeconfig:   filepath.Join(kcDir, "kubelet.kubeconfig"),
		bootstrapKubeconfig: filepath.Join(base, "bootstrap-kubeconfig"),
	}
	for _, f := range []string{p.caBundle, p.kubeletClientCA, p.caCert} {
		if err := os.WriteFile(f, onDisk, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{p.kubeletKubeconfig, p.bootstrapKubeconfig} {
		cfg := clientcmdapi.NewConfig()
		cfg.Clusters["oss"] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: onDisk}
		cfg.AuthInfos["kubelet"] = &clientcmdapi.AuthInfo{Token: "x"}
		cfg.Contexts["default"] = &clientcmdapi.Context{Cluster: "oss", AuthInfo: "kubelet"}
		cfg.CurrentContext = "default"
		raw, err := clientcmd.Write(*cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func (p trustPaths) all() []string {
	return []string{p.caBundle, p.kubeletClientCA, p.caCert, p.kubeletKubeconfig, p.bootstrapKubeconfig}
}

func snapshot(t *testing.T, p trustPaths) map[string]string {
	t.Helper()
	out := make(map[string]string, 5)
	for _, f := range p.all() {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

// assertCleanAbort is the assertion this whole change exists for: a refused
// refresh must leave every file byte-identical and no staged residue behind.
func assertCleanAbort(t *testing.T, p trustPaths, before map[string]string) {
	t.Helper()
	for f, want := range before {
		got, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s unreadable after a refusal: %v", f, err)
		}
		if string(got) != want {
			t.Errorf("%s was modified by a refused refresh", f)
		}
	}
	assertNoStagedResidue(t, p)
}

// assertNoStagedResidue checks every directory the refresh stages into, including
// the one holding bootstrap-kubeconfig, whose temp lands beside it rather than in
// the PKI or kubeconfig directory.
func assertNoStagedResidue(t *testing.T, p trustPaths) {
	t.Helper()
	for _, dir := range []string{filepath.Dir(p.caBundle), filepath.Dir(p.kubeletKubeconfig), filepath.Dir(p.bootstrapKubeconfig)} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if strings.Contains(e.Name(), ".tmp-") {
				t.Errorf("staged residue left behind in %s: %s", dir, e.Name())
			}
		}
	}
}

func embeddedCA(t *testing.T, file string) string {
	t.Helper()
	cfg, err := clientcmd.LoadFromFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cfg.Clusters {
		return string(c.CertificateAuthorityData)
	}
	t.Fatalf("%s has no cluster entry", file)
	return ""
}

// ── rejection cases ──────────────────────────────────────────────────────────

// TestRefreshRejectsAttackerServingCert is the L1 case, run for all three
// intents: an on-path attacker answering with a self-signed cert is what an
// InsecureSkipVerify transport accepts without complaint.
func TestRefreshRejectsAttackerServingCert(t *testing.T) {
	genuine := newTestCA(t, "stratum-ca")
	attacker := newTestCA(t, "attacker-ca")
	for _, tc := range []struct {
		name   string
		intent trustIntent
	}{
		{"trust-new", trustWiden},
		{"finalize", trustNarrow},
		{"rollback", trustEither},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := clusterInfo(t, attacker.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(attacker))
			p := node(t, bundle(genuine), url)
			before := snapshot(t, p)
			_, err := planTrustRefresh(url, tc.intent, p)
			if err == nil {
				t.Fatal("attacker-signed serving cert accepted")
			}
			if !strings.Contains(err.Error(), "does not trust") {
				t.Errorf("error should name the trust failure, got: %v", err)
			}
			assertCleanAbort(t, p, before)
		})
	}
}

// TestRefreshRejectsSANMismatch covers the population that hostname
// verification alongside chain verification catches: a worker whose
// endpoint is not in the apiserver cert's SANs. It must fail closed, and the
// message must be distinguishable from a trust failure — under
// `rotate-ca --cluster` it is the operator's only signal.
func TestRefreshRejectsSANMismatch(t *testing.T) {
	genuine := newTestCA(t, "stratum-ca")
	// Signed by the CA this node trusts, but carrying no 127.0.0.1 IP SAN.
	url := clusterInfo(t, genuine.serving(t, nil), bundle(genuine))
	p := node(t, bundle(genuine), url)
	before := snapshot(t, p)
	_, err := planTrustRefresh(url, trustWiden, p)
	if err == nil {
		t.Fatal("serving cert with no matching SAN accepted")
	}
	// Both branches of classifyFetchError mention "SAN mismatch" — the trust branch
	// ends "...not as a SAN mismatch" — so pin the phrase unique to this one.
	if !strings.Contains(err.Error(), "presented a TRUSTED certificate that is not valid for that endpoint") {
		t.Errorf("SAN failure must be distinguishable from a trust failure, got: %v", err)
	}
	if strings.Contains(err.Error(), "neither trusted") {
		t.Errorf("a trusted cert was misreported as a trust failure: %v", err)
	}
	assertCleanAbort(t, p, before)
}

// TestRefreshRejectsDisjointBundle: the fetched bundle shares no certificate
// with the node's current one. The message says not to trust the bundle, since
// no rotation, --force included, produces one, and names the rejoin path,
// because this is the one refusal whose cause is not inferable from the fact
// that the phase halted.
func TestRefreshRejectsDisjointBundle(t *testing.T) {
	genuine := newTestCA(t, "stratum-ca")
	other := newTestCA(t, "unrelated-ca")
	for _, tc := range []struct {
		name   string
		intent trustIntent
	}{
		{"trust-new", trustWiden},
		{"finalize", trustNarrow},
		{"rollback", trustEither},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := clusterInfo(t, genuine.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(other))
			p := node(t, bundle(genuine), url)
			before := snapshot(t, p)
			_, err := planTrustRefresh(url, tc.intent, p)
			if err == nil {
				t.Fatal("wholesale trust pivot accepted")
			}
			if !strings.Contains(err.Error(), "shares no certificate") {
				t.Errorf("error should name the containment predicate, got: %v", err)
			}
			if !strings.Contains(err.Error(), "do not trust this bundle") {
				t.Errorf("disjoint-bundle error must say not to trust the bundle, got: %v", err)
			}
			for _, want := range []string{"`sudo /usr/local/bin/stratum uninstall --yes`", "stratum token create"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("disjoint-bundle error must name the rejoin (%s), got: %v", want, err)
				}
			}
			assertCleanAbort(t, p, before)
		})
	}
}

// TestRefreshRejectsShrinkAtTrustNew: trust-new may only widen. A stale
// cluster-info republish that has lost the new CA looks exactly like this.
func TestRefreshRejectsShrinkAtTrustNew(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(oldCA))
	p := node(t, bundle(oldCA, newCA), url)
	before := snapshot(t, p)
	_, err := planTrustRefresh(url, trustWiden, p)
	if err == nil {
		t.Fatal("trust-new accepted a bundle that drops a trusted CA")
	}
	if !strings.Contains(err.Error(), "may only widen") {
		t.Errorf("error should name the widen predicate, got: %v", err)
	}
	assertCleanAbort(t, p, before)
}

// TestRefreshRejectsIntroductionAtFinalize: finalize may only drop CAs. A phase
// whose entire job is removal must not be the one that adds an anchor.
func TestRefreshRejectsIntroductionAtFinalize(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(oldCA, newCA))
	p := node(t, bundle(oldCA), url)
	before := snapshot(t, p)
	_, err := planTrustRefresh(url, trustNarrow, p)
	if err == nil {
		t.Fatal("finalize accepted a bundle that introduces a new CA")
	}
	if !strings.Contains(err.Error(), "may only drop") {
		t.Errorf("error should name the narrow predicate, got: %v", err)
	}
	assertCleanAbort(t, p, before)
}

// TestRefreshRejectsRegressedBundleAtFinalize is the case that shows containment
// alone is not sufficient, and the reason L2 exists.
//
// A stale RepinCA republishes the pre-rotation bundle while the control plane is
// already serving a new-CA cert. At finalize the worker holds old‖new, so the
// fetched {old} is a legal subset, shares a certificate, and passes containment
// — yet adopting it drops the only anchor that validates the apiserver the
// worker is talking to. L2 refuses it because the bundle does not validate the
// connection it arrived on.
func TestRefreshRejectsRegressedBundleAtFinalize(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	// Post sign-new: the apiserver serves a NEW-CA-signed cert.
	url := clusterInfo(t, newCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(oldCA))
	p := node(t, bundle(oldCA, newCA), url)

	// Containment alone accepts it — assert that directly, so the test states the
	// gap rather than implying it.
	onDisk, err := parseTrustBundle("on-disk", bundle(oldCA, newCA))
	if err != nil {
		t.Fatal(err)
	}
	regressed, err := parseTrustBundle("fetched", bundle(oldCA))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkTrustTransition(onDisk, regressed, trustNarrow); err != nil {
		t.Fatalf("premise broken: containment was expected to accept the regressed bundle, got %v", err)
	}

	before := snapshot(t, p)
	if _, err := planTrustRefresh(url, trustNarrow, p); err == nil {
		t.Fatal("regressed bundle accepted; the worker would have cut itself off from the apiserver")
	} else if !strings.Contains(err.Error(), "does not validate the apiserver serving cert") {
		t.Errorf("error should name the peer-coverage predicate, got: %v", err)
	}
	assertCleanAbort(t, p, before)
}

// TestRefreshRejectsMalformedBundle is L4: caFromClusterInfo returns
// certificate-authority-data on length alone (as does its initial-join twin
// fetchClusterCA), so without this, arbitrary bytes reach the kubelet's
// clientCAFile.
func TestRefreshRejectsMalformedBundle(t *testing.T) {
	genuine := newTestCA(t, "stratum-ca")
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "not-a-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}, genuine.cert, &leafKey.PublicKey, genuine.key)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		publish []byte
		want    string
	}{
		{"not pem at all", []byte("hello"), "not a PEM block"},
		{"empty", []byte{}, "no CA"}, // fails earlier, in caFromClusterInfo
		// Whitespace-only survives the length check and does reach parseTrustBundle.
		{"whitespace only", []byte("   \n\t\n"), "holds no certificate"},
		{"wrong block type", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}), "CERTIFICATE blocks only"},
		{"unparseable cert", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("nope")}), "unparseable certificate"},
		{"non-CA leaf", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), "non-CA certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := clusterInfo(t, genuine.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), tc.publish)
			p := node(t, bundle(genuine), url)
			before := snapshot(t, p)
			_, err := planTrustRefresh(url, trustWiden, p)
			if err == nil {
				t.Fatal("malformed bundle accepted into the kubelet clientCAFile")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			assertCleanAbort(t, p, before)
		})
	}
}

// ── accept cases ─────────────────────────────────────────────────────────────

// TestRefreshAcceptsPerPhase walks the real rotation, one accept per phase,
// asserting every file lands with the fetched bundle and the right mode.
//
// The rollback re-widen subtest pins that trustEither ACCEPTS a re-widen, which is
// what a Phase-derived intent would get wrong for `--rollback --phase=finalize`.
// It does not pin the mapping itself — nothing here calls RotateCA or RollbackCA,
// so a regression in which intent those pass would not surface. That wiring is
// covered only by deploy/stratum-pve-lab.sh.
func TestRefreshAcceptsPerPhase(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	for _, tc := range []struct {
		name    string
		intent  trustIntent
		onDisk  []byte
		signer  *testCA
		publish []byte
	}{
		{"trust-new widens to old+new", trustWiden, bundle(oldCA), oldCA, bundle(newCA, oldCA)},
		{"finalize narrows to new", trustNarrow, bundle(oldCA, newCA), newCA, bundle(newCA)},
		{"rollback narrows back to old", trustEither, bundle(oldCA, newCA), oldCA, bundle(oldCA)},
		{"rollback re-widens to old+new", trustEither, bundle(oldCA), oldCA, bundle(newCA, oldCA)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := clusterInfo(t, tc.signer.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), tc.publish)
			p := node(t, tc.onDisk, url)
			u, err := planTrustRefresh(url, tc.intent, p)
			if err != nil {
				t.Fatalf("legitimate refresh refused: %v", err)
			}
			// Staging alone must not have touched anything yet.
			for _, f := range []string{p.caBundle, p.kubeletClientCA, p.caCert} {
				if b, err := os.ReadFile(f); err != nil || string(b) != string(tc.onDisk) {
					t.Errorf("%s changed before commit", f)
				}
			}
			if err := u.commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if err := u.abort(); err != nil {
				t.Errorf("abort after a successful commit must be a no-op, got %v", err)
			}
			for _, f := range []string{p.caBundle, p.kubeletClientCA, p.caCert} {
				b, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				if string(b) != string(tc.publish) {
					t.Errorf("%s does not carry the fetched bundle", f)
				}
			}
			for _, f := range []string{p.kubeletKubeconfig, p.bootstrapKubeconfig} {
				if got := embeddedCA(t, f); got != string(tc.publish) {
					t.Errorf("%s does not embed the fetched bundle", f)
				}
			}
			for f, want := range map[string]os.FileMode{
				p.caBundle: 0o644, p.kubeletClientCA: 0o644, p.caCert: 0o644,
				p.kubeletKubeconfig: 0o600, p.bootstrapKubeconfig: 0o600,
			} {
				fi, err := os.Stat(f)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode().Perm() != want {
					t.Errorf("%s mode = %o, want %o", f, fi.Mode().Perm(), want)
				}
			}
			assertNoStagedResidue(t, p)
		})
	}
}

// TestRefreshSkipsAbsentKubeconfig: a worker may have no bootstrap-kubeconfig.
// Absent files are skipped, never created.
func TestRefreshSkipsAbsentKubeconfig(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA, oldCA))
	p := node(t, bundle(oldCA), url)
	if err := os.Remove(p.bootstrapKubeconfig); err != nil {
		t.Fatal(err)
	}
	u, err := planTrustRefresh(url, trustWiden, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.bootstrapKubeconfig); !os.IsNotExist(err) {
		t.Errorf("an absent bootstrap-kubeconfig must be skipped, not created")
	}
}

// A missing kubelet bundle is ambiguous: it may be a legacy worker, or a split
// worker whose dedicated bundle was deleted. A root refresh must not repair it
// with the root and thereby widen access to the kubelet.
func TestRefreshRefusesAnAbsentKubeletClientCA(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA, oldCA))
	p := node(t, bundle(oldCA), url)
	if err := os.Remove(p.kubeletClientCA); err != nil {
		t.Fatal(err)
	}
	if _, err := planTrustRefresh(url, trustWiden, p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refresh error = %v, want the missing kubelet client-CA bundle", err)
	}
	if _, err := os.Stat(p.kubeletClientCA); !os.IsNotExist(err) {
		t.Errorf("refresh recreated the missing kubelet bundle from the root: %v", err)
	}
	if b, err := os.ReadFile(p.caBundle); err != nil || string(b) != string(bundle(oldCA)) {
		t.Errorf("refused refresh changed the root trust bundle: %v", err)
	}
}

// TestSignNewChecksTheKubeletClientCA: sign-new reissues apiserver-kubelet-client
// under the new root, so a worker whose clientCAFile a stopped trust-new left on
// the old root alone would reject the apiserver's logs and exec. A worker with no
// clientCAFile still runs its kubelet on ca-bundle.crt, which is checked anyway.
func TestSignNewChecksTheKubeletClientCA(t *testing.T) {
	oldCA, newCA := newTestCA(t, "old"), newTestCA(t, "new")

	p := node(t, bundle(oldCA, newCA), "https://127.0.0.1:6443")
	if err := os.WriteFile(p.kubeletClientCA, bundle(oldCA), 0o644); err != nil {
		t.Fatal(err)
	}
	err := checkWorkerTrustWidened(p)
	if err == nil || !strings.Contains(err.Error(), p.kubeletClientCA) || !strings.Contains(err.Error(), "--phase="+PhaseTrustNew) {
		t.Fatalf("err = %v, want a refusal naming %s and offering --phase=%s", err, p.kubeletClientCA, PhaseTrustNew)
	}

	p = node(t, bundle(oldCA, newCA), "https://127.0.0.1:6443")
	if err := os.Remove(p.kubeletClientCA); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkerTrustWidened(p); err != nil {
		t.Fatalf("a worker with no clientCAFile and a widened ca-bundle.crt was refused: %v", err)
	}
}

// TestSignNewRefusesAStaleTwoCATrustInput: a finalize that stopped after
// ca-bundle.crt shrank leaves the other trust inputs on {previous, current}, and a
// later trust-new that stops after ca-bundle.crt widens to {current, next} leaves
// them there. Two CAs each, but not the CA sign-new is about to sign under.
func TestSignNewRefusesAStaleTwoCATrustInput(t *testing.T) {
	prevCA, curCA, nextCA := newTestCA(t, "prev"), newTestCA(t, "cur"), newTestCA(t, "next")
	stale := bundle(prevCA, curCA)
	for _, tc := range []struct {
		name  string
		file  func(trustPaths) string
		write func(*testing.T, string)
	}{
		{name: "kubelet client-CA bundle", file: func(p trustPaths) string { return p.kubeletClientCA },
			write: func(t *testing.T, f string) {
				if err := os.WriteFile(f, stale, 0o644); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "kubelet.kubeconfig", file: func(p trustPaths) string { return p.kubeletKubeconfig },
			write: func(t *testing.T, f string) {
				raw, err := kubeconfig.ReadWithClusterCA(f, stale)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := node(t, bundle(curCA, nextCA), "https://127.0.0.1:6443")
			if err := checkWorkerTrustWidened(p); err != nil {
				t.Fatalf("a fully widened worker was refused: %v", err)
			}
			tc.write(t, tc.file(p))
			err := checkWorkerTrustWidened(p)
			if err == nil || !strings.Contains(err.Error(), tc.file(p)) || !strings.Contains(err.Error(), "--phase="+PhaseTrustNew) {
				t.Fatalf("err = %v, want a refusal naming %s and offering --phase=%s", err, tc.file(p), PhaseTrustNew)
			}
		})
	}
}

// ── unit-level predicate coverage ────────────────────────────────────────────

// TestCheckTrustTransition pins the predicate table directly, including the
// at-most-one-addition bound that the end-to-end cases do not reach.
func TestCheckTrustTransition(t *testing.T) {
	a, b, c := newTestCA(t, "a"), newTestCA(t, "b"), newTestCA(t, "c")
	// Takes the subtest's own t: calling Fatal on the parent from inside t.Run
	// would FailNow the wrong goroutine.
	parse := func(t *testing.T, pemBytes []byte) []*x509.Certificate {
		t.Helper()
		got, err := parseTrustBundle("x", pemBytes)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, tc := range []struct {
		name    string
		onDisk  []byte
		fetched []byte
		intent  trustIntent
		wantErr string
	}{
		{"widen by one", bundle(a), bundle(a, b), trustWiden, ""},
		{"widen by two", bundle(a), bundle(a, b, c), trustWiden, "introduces 2 new CAs"},
		// A single-CA node whose fetch adds nothing means cluster-info was never
		// republished; proceeding strands the node at sign-new.
		{"widen that widens nothing", bundle(a), bundle(a), trustWiden, "would widen nothing"},
		// The same shape on an already-widened node is a legitimate phase re-run.
		{"widen re-run after trust-new", bundle(a, b), bundle(a, b), trustWiden, ""},
		{"widen that shrinks", bundle(a, b), bundle(a), trustWiden, "may only widen"},
		{"narrow by one", bundle(a, b), bundle(b), trustNarrow, ""},
		{"narrow unchanged", bundle(a), bundle(a), trustNarrow, ""},
		{"narrow that adds", bundle(a), bundle(a, b), trustNarrow, "may only drop"},
		// finalize that drops nothing while the node still holds two CAs means
		// cluster-info was never republished; accepting it leaves the old CA in the
		// kubelet's clientCAFile, which is the anchor finalize exists to remove.
		{"narrow that drops nothing", bundle(a, b), bundle(a, b), trustNarrow, "would drop nothing"},
		// A bundle listing the same CA twice holds ONE anchor. Both no-op guards
		// count distinct anchors for that reason, and both rows die if either
		// guard's len(dedupe(onDisk)) is weakened to len(onDisk) — the widen one
		// fails OPEN there, which is the failure the widen guard exists to prevent.
		{"narrow unchanged, on-disk listed twice", bundle(a, a), bundle(a), trustNarrow, ""},
		{"widen that widens nothing, on-disk listed twice", bundle(a, a), bundle(a), trustWiden, "would widen nothing"},
		{"rollback narrows", bundle(a, b), bundle(a), trustEither, ""},
		{"rollback re-widens", bundle(a), bundle(a, b), trustEither, ""},
		{"rollback adds two", bundle(a), bundle(a, b, c), trustEither, "introduces 2"},
		{"disjoint", bundle(a), bundle(b), trustWiden, "shares no certificate"},
		{"disjoint keeps nothing but adds one", bundle(a), bundle(b), trustEither, "shares no certificate"},
		// A rollback may widen or narrow, never both — commit's partial-apply
		// argument depends on every file moving the same direction.
		{"rollback adds and drops at once", bundle(a, b), bundle(a, c), trustEither, "not both at once"},
		// A bundle listing the same CA twice is one addition, not two.
		{"widen by one, listed twice", bundle(a), bundle(a, b, b), trustWiden, ""},
		{"unknown intent", bundle(a), bundle(a, b), trustIntent(99), "unknown trust intent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := checkTrustTransition(parse(t, tc.onDisk), parse(t, tc.fetched), tc.intent)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected refusal: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("accepted, want refusal mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestContainmentDoesNotStopAnAppend records, as an executable fact, that
// containment is not an anti-MITM control: the cluster CA certificate is public,
// so a peer that defeats the transport can append its own CA to a bundle that
// also carries the genuine one and satisfy every containment predicate. If this
// test ever fails, someone has mistaken containment for a security boundary.
func TestContainmentDoesNotStopAnAppend(t *testing.T) {
	genuine, attacker := newTestCA(t, "stratum-ca"), newTestCA(t, "attacker-ca")
	onDisk, err := parseTrustBundle("on-disk", bundle(genuine))
	if err != nil {
		t.Fatal(err)
	}
	appended, err := parseTrustBundle("fetched", bundle(genuine, attacker))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkTrustTransition(onDisk, appended, trustWiden); err != nil {
		t.Fatalf("premise changed: containment now rejects an append (%v) — re-read the L1/L3 split in catrust.go", err)
	}
}

// TestCommitStopsAtTheFirstFailedRename asserts the residue contract: files
// already renamed stay, un-renamed temps are cleaned up, and nothing is left
// half-written.
func TestCommitStopsAtTheFirstFailedRename(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "child"), 0o755); err != nil {
		t.Fatal(err) // a non-empty dir cannot be replaced by a rename, for any uid
	}
	u := &trustUpdate{}
	for _, f := range []string{good, blocked, filepath.Join(dir, "never")} {
		s, err := stageFile(f, []byte("data"), 0o644)
		if err != nil {
			t.Fatal(err)
		}
		u.staged = append(u.staged, s)
	}
	err := u.commit()
	if err == nil {
		t.Fatal("commit over a non-renameable path reported success")
	}
	if !strings.Contains(err.Error(), blocked) {
		t.Errorf("error should name the file it could not write: %v", err)
	}
	if b, readErr := os.ReadFile(good); readErr != nil || string(b) != "data" {
		t.Errorf("the already-renamed file should stay in place")
	}
	u.abort()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("abort left staged residue: %s", e.Name())
		}
	}
}

// ── staging-failure cases ────────────────────────────────────────────────────
//
// Everything above rejects before the first stageFile call, so none of it
// exercises the rollback that discards already-staged temps. These do: they fail
// partway through staging, which is the only way to reach the deferred abort in
// planTrustRefresh. Deleting that defer must not leave this suite green.

// TestRefreshCleansUpWhenStagingFails corrupts kubelet.kubeconfig so staging
// fails on the third file, after ca-bundle.crt and ca.crt are already staged.
func TestRefreshCleansUpWhenStagingFails(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA, oldCA))
	p := node(t, bundle(oldCA), url)
	if err := os.WriteFile(p.kubeletKubeconfig, []byte("not a kubeconfig at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, p)
	_, err := planTrustRefresh(url, trustWiden, p)
	if err == nil {
		t.Fatal("staging over an unreadable kubeconfig reported success")
	}
	if !strings.Contains(err.Error(), "rewrite CA in") || !strings.Contains(err.Error(), p.kubeletKubeconfig) {
		t.Errorf("error should name the file it could not rewrite, got: %v", err)
	}
	// The two PKI temps staged before the failure must be gone.
	assertCleanAbort(t, p, before)
}

// TestRefreshCleansUpWhenTempCannotBeCreated fails at the very first stageFile,
// covering stageFile's own error path rather than the caller's.
func TestRefreshCleansUpWhenTempCannotBeCreated(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA, oldCA))
	p := node(t, bundle(oldCA), url)
	before := snapshot(t, p)
	// Point ca.crt at a directory that does not exist, so CreateTemp fails there
	// while ca-bundle.crt has already been staged successfully.
	staged := p
	staged.caCert = filepath.Join(filepath.Dir(p.caBundle), "no-such-dir", "ca.crt")
	_, err := planTrustRefresh(url, trustWiden, staged)
	if err == nil {
		t.Fatal("staging into a missing directory reported success")
	}
	if !strings.Contains(err.Error(), "stage ") {
		t.Errorf("error should name the staging step, got: %v", err)
	}
	assertCleanAbort(t, p, before)
}

// ── fallback and regression cases ────────────────────────────────────────────

// TestRefreshFallsBackToCACert: a worker joined by the installer has
// only ca.crt — no ca-bundle.crt. Every other bundle read in the tree falls back;
// without it, CA rotation halts on those nodes at the first read.
func TestRefreshFallsBackToCACert(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA, oldCA))
	p := node(t, bundle(oldCA), url)
	if err := os.Remove(p.caBundle); err != nil {
		t.Fatal(err)
	}
	u, err := planTrustRefresh(url, trustWiden, p)
	if err != nil {
		t.Fatalf("refresh refused on a node that has only ca.crt: %v", err)
	}
	if err := u.commit(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p.caBundle, p.kubeletClientCA, p.caCert} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if string(b) != string(bundle(newCA, oldCA)) {
			t.Errorf("%s does not carry the fetched bundle", f)
		}
	}
}

// TestRefreshRejectsUntrustedServerWithSANMismatch pins the one combination where
// reporting on the x509 error type alone gets it backwards: Verify checks the
// hostname before it builds chains, so an attacker-signed cert that also fails
// SAN validation surfaces as a HostnameError and would otherwise be announced as
// "TRUSTED ... not a trust failure".
func TestRefreshRejectsUntrustedServerWithSANMismatch(t *testing.T) {
	genuine := newTestCA(t, "stratum-ca")
	attacker := newTestCA(t, "attacker-ca")
	url := clusterInfo(t, attacker.serving(t, nil), bundle(attacker)) // untrusted AND no IP SAN
	p := node(t, bundle(genuine), url)
	before := snapshot(t, p)
	_, err := planTrustRefresh(url, trustWiden, p)
	if err == nil {
		t.Fatal("untrusted serving cert accepted")
	}
	if !strings.Contains(err.Error(), "neither trusted by this node nor valid") {
		t.Errorf("an untrusted cert that also fails SAN validation must be reported as a trust failure, got: %v", err)
	}
	if strings.Contains(err.Error(), "TRUSTED") {
		t.Errorf("untrusted cert announced as TRUSTED: %v", err)
	}
	assertCleanAbort(t, p, before)
}

// TestDescribeDistinguishesSameNamedCAs: pki mints both the live root and the
// rotation root with CommonName "stratum-ca", so a refusal naming only the CN tells
// the operator nothing about which anchor moved.
func TestDescribeDistinguishesSameNamedCAs(t *testing.T) {
	a, b := newTestCA(t, "stratum-ca"), newTestCA(t, "stratum-ca")
	certs, err := parseTrustBundle("x", bundle(a, b))
	if err != nil {
		t.Fatal(err)
	}
	got := describe(certs)
	if !strings.Contains(got, "sha256:") {
		t.Errorf("describe must pin each CA, got %q", got)
	}
	parts := strings.Split(got, "; ")
	if len(parts) != 2 || parts[0] == parts[1] {
		t.Errorf("two distinct CAs sharing a CommonName must render distinguishably, got %q", got)
	}
}

// TestSummaryNamesTheChange: the summary is what an operator reads to confirm the
// phase did what the phase is for.
func TestSummaryNamesTheChange(t *testing.T) {
	a, b := newTestCA(t, "old"), newTestCA(t, "new")
	certs := func(pemBytes []byte) []*x509.Certificate {
		t.Helper()
		got, err := parseTrustBundle("x", pemBytes)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, tc := range []struct {
		name string
		u    *trustUpdate
		want string
	}{
		{"widen", &trustUpdate{added: certs(bundle(b))}, "add "},
		{"narrow", &trustUpdate{dropped: certs(bundle(a))}, "drop "},
		{"both", &trustUpdate{added: certs(bundle(b)), dropped: certs(bundle(a))}, "add "},
		{"no change", &trustUpdate{}, "no change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Assert on the leading verb, not a substring anywhere: describe embeds a
			// hex fingerprint, and a random nibble run spells "add" often enough to
			// make a Contains assertion flake roughly once in 25 runs.
			got := tc.u.summary()
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("summary = %q, want it to start with %q", got, tc.want)
			}
		})
	}
	both := (&trustUpdate{added: certs(bundle(b)), dropped: certs(bundle(a))}).summary()
	if !strings.Contains(both, " / drop ") {
		t.Errorf("a summary that both adds and drops must name both, got %q", both)
	}
}

// TestParseTrustBundleRejectsSmuggledBytes: the fetched bytes are written
// verbatim, so a region pem.Decode skips would land in the kubelet's clientCAFile
// without anything having validated it.
func TestParseTrustBundleRejectsSmuggledBytes(t *testing.T) {
	ca, other := newTestCA(t, "stratum-ca"), newTestCA(t, "stratum-ca-2")
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"trailing garbage", append(append([]byte{}, ca.pem...), []byte("\nowned-by-nobody\n")...)},
		{"leading garbage", append([]byte("owned-by-nobody\n"), ca.pem...)},
		{"undecodable block after a valid one", append(append([]byte{}, ca.pem...),
			[]byte("-----BEGIN CERTIFICATE-----\n!!!!not base64!!!!\n-----END CERTIFICATE-----\n")...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTrustBundle("x", tc.in); err == nil {
				t.Error("accepted; those bytes would be written verbatim to the kubelet clientCAFile")
			}
		})
	}
	// Whitespace that is not a newline, immediately before a BEGIN line: pem.Decode
	// anchors only at offset 0 or after '\n', so these validate here while
	// client-go's cert.ParseCertsPEM — what the kubelet loads clientCAFile with —
	// silently skips the block. Verified against both parsers: before the anchoring
	// check these produced stratum=2/client-go=1 and stratum=1/client-go=0.
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"space between blocks", concat(ca.pem, []byte(" "), other.pem)},
		{"tab between blocks", concat(ca.pem, []byte("\t"), other.pem)},
		{"lone CR between blocks", concat(ca.pem, []byte("\r"), other.pem)},
		{"newline then space between blocks", concat(ca.pem, []byte("\n "), other.pem)},
		{"space before the only block", concat([]byte(" "), ca.pem)},
		{"tab before the only block", concat([]byte("\t"), ca.pem)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTrustBundle("x", tc.in); err == nil {
				t.Error("accepted; the kubelet's parser would load fewer anchors than this validated")
			} else if !strings.Contains(err.Error(), "immediately before a PEM block") {
				t.Errorf("err = %v, want it to name the misplaced whitespace", err)
			}
		})
	}
	// A separator ending in a newline is legitimate and must still parse.
	for _, sep := range []string{"\n", "\n\n", "\r\n", " \n"} {
		if got, err := parseTrustBundle("x", concat(ca.pem, []byte(sep), other.pem)); err != nil {
			t.Errorf("separator %q rejected: %v", sep, err)
		} else if len(got) != 2 {
			t.Errorf("separator %q parsed %d certs, want 2", sep, len(got))
		}
	}
	// Trailing whitespace is not smuggling.
	if _, err := parseTrustBundle("x", append(append([]byte{}, ca.pem...), '\n', '\n')); err != nil {
		t.Errorf("trailing newlines rejected: %v", err)
	}
}

// TestParseTrustBundleRejectsFakeBeginLine: pem.Decode scans forward internally,
// so a "-----BEGIN " line it cannot complete makes it skip everything up to the
// next block. A prefix check at offset 0 does not catch that, and the skipped
// bytes are written out verbatim.
func TestParseTrustBundleRejectsFakeBeginLine(t *testing.T) {
	ca, other := newTestCA(t, "stratum-ca"), newTestCA(t, "stratum-ca-2")
	fake := []byte("-----BEGIN ZZZ\n#!/bin/sh\nrm -rf /\n")
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"fake BEGIN before a valid block", append(append([]byte{}, fake...), ca.pem...)},
		{"fake BEGIN between two valid blocks", concat(ca.pem, fake, other.pem)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTrustBundle("x", tc.in); err == nil {
				t.Error("accepted; pem.Decode skipped the region and it would be written verbatim")
			} else if !strings.Contains(err.Error(), "skips over") {
				t.Errorf("err = %v, want it to name the skipped region", err)
			}
		})
	}
}

// TestParseTrustBundleRejectsPEMHeaders: a CERTIFICATE block carrying PEM headers
// parses fine here but is dropped outright by client-go's cert.ParseCertsPEM, the
// parser the kubelet loads clientCAFile with — so accepting one would leave the
// kubelet with no anchors at all while this code reported success.
func TestParseTrustBundleRejectsPEMHeaders(t *testing.T) {
	ca := newTestCA(t, "stratum-ca")
	blk, _ := pem.Decode(ca.pem)
	if blk == nil {
		t.Fatal("fixture CA is not PEM")
	}
	withHeaders := pem.EncodeToMemory(&pem.Block{
		Type:    "CERTIFICATE",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"},
		Bytes:   blk.Bytes,
	})
	if _, err := parseTrustBundle("x", withHeaders); err == nil {
		t.Error("accepted; the kubelet's parser would drop it and load no anchors")
	} else if !strings.Contains(err.Error(), "PEM headers") {
		t.Errorf("err = %v, want it to name the headers", err)
	}
}

// TestBundleReadersAgreedInputs is the certs half of the pin internal/pki
// holds: the same rows go to CABundleCertCount and bundleCerts there and to
// parseTrustBundle here, and every parser must give the same answer — the
// count, or the refusal — for inputs they all see today. The three readers
// share one file, so agreement is the contract a single shared parser must
// keep.
func TestBundleReadersAgreedInputs(t *testing.T) {
	ca, other := newTestCA(t, "stratum-ca"), newTestCA(t, "stratum-ca-2")
	badDER := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a DER certificate")})
	for _, row := range []struct {
		name string
		in   []byte
		want int // 0: the parser must refuse
	}{
		{"a clean bundle of two CA certificates", bundle(ca, other), 2},
		{"a single CA certificate", ca.pem, 1},
		{"a block whose DER does not parse", badDER, 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			certs, err := parseTrustBundle("x", row.in)
			if row.want == 0 {
				if err == nil {
					t.Error("accepted; the pki readers refuse this input, so the parsers would disagree")
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if len(certs) != row.want {
				t.Errorf("parsed %d certificates, want %d", len(certs), row.want)
			}
		})
	}
}

// TestOneBundleParser is the certs half of a shared pin: internal/pki's
// TestOneBundleParser holds the same rows against CABundleCertCount and
// bundleCerts there. The worker refresh writes the fetched bytes verbatim into
// the kubelet's clientCAFile, so parseTrustBundle must keep the kubelet's
// verbatim rules: newline-ended leading whitespace is the only leading form it
// accepts, and space/tab/lone-CR before BEGIN, PEM headers, non-certificate
// blocks and non-CA certificates are all refused. Its answer must be exactly
// the answer of pki's one shared decoder under pki.KubeletPolicy, so the
// difference between the three readers stays a declared policy rather than
// ad-hoc parsing.
func TestOneBundleParser(t *testing.T) {
	ca, other := newTestCA(t, "stratum-ca"), newTestCA(t, "stratum-ca-2")
	leaf := leafPEM(t, "kube-apiserver")
	blk, _ := pem.Decode(ca.pem)
	if blk == nil {
		t.Fatal("fixture CA is not PEM")
	}
	// The block keeps its DER under the headers: the kubelet's parser drops it
	// outright, which is why the worker must refuse it.
	headers := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"}, Bytes: blk.Bytes})
	foreign := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not a certificate")})
	two := bundle(ca, other)
	for _, row := range []struct {
		name string
		in   []byte
		want int // 0: the parser must refuse
	}{
		{"a clean bundle of two CA certificates", two, 2},
		{"a single CA certificate", ca.pem, 1},
		{"newline-ended leading whitespace before the first block", concat([]byte(" \n"), two), 2},
		{"a space immediately before BEGIN", concat([]byte(" "), ca.pem), 0},
		{"a tab immediately before BEGIN", concat([]byte("\t"), ca.pem), 0},
		{"a lone CR immediately before BEGIN", concat([]byte("\r"), ca.pem), 0},
		{"a CERTIFICATE block carrying PEM headers", headers, 0},
		{"a non-certificate PEM block after the CA", concat(ca.pem, foreign), 0},
		{"a non-CA certificate", leaf, 0},
		{"embedded text before a block", concat([]byte("owned-by-nobody\n"), ca.pem), 0},
	} {
		t.Run(row.name, func(t *testing.T) {
			certs, err := parseTrustBundle("x", row.in)
			if row.want == 0 {
				if err == nil {
					t.Error("accepted; the worker writes these bytes verbatim to the kubelet's clientCAFile")
				}
			} else {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if len(certs) != row.want {
					t.Errorf("parsed %d certificates, want %d", len(certs), row.want)
				}
			}
			// The kubelet-verbatim policy is declared in pki, where the other
			// two readers declare theirs: parseTrustBundle must answer exactly
			// as the shared decoder answers under it.
			shared, sharedErr := pki.DecodePEMBundle(row.in, pki.KubeletPolicy)
			if (err == nil) != (sharedErr == nil) {
				t.Errorf("parseTrustBundle and pki.DecodePEMBundle disagree on accept/refuse: parseTrustBundle err = %v, decoder err = %v", err, sharedErr)
				return
			}
			if err == nil && len(certs) != len(shared) {
				t.Errorf("parseTrustBundle parsed %d certificates, pki.DecodePEMBundle decoded %d under pki.KubeletPolicy", len(certs), len(shared))
			}
		})
	}
}

// leafPEM mints a self-signed non-CA certificate, for the rows that test the
// IsCA policy.
func leafPEM(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestSyncDirsReportsTheChangeIsApplied: syncDirs runs after every rename has
// landed, so a failure there means the refresh IS in effect and only its survival
// of a power loss is unconfirmed. The message has to say so — an operator who
// reads it as a failed apply will roll back a node that does not need it.
func TestSyncDirsReportsTheChangeIsApplied(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "no-such-dir", "ca-bundle.crt")
	err := syncDirs([]stagedFile{{final: gone}})
	if err == nil {
		t.Fatal("syncDirs over a missing directory reported success")
	}
	if !strings.Contains(err.Error(), "IS applied to disk") {
		t.Errorf("err = %v, want it to say the files are in effect but the kubelet was not restarted", err)
	}
	if !strings.Contains(err.Error(), filepath.Dir(gone)) {
		t.Errorf("err = %v, want it to name the directory", err)
	}
	// The restart is skipped on this path, so the message must not leave an operator
	// believing the running kubelet picked the change up.
	if !strings.Contains(err.Error(), "kubelet was not restarted") {
		t.Errorf("err = %v, want it to say the kubelet was not restarted", err)
	}
}

// TestStageFileSetsModeBeforeRename pins that the mode lands on the temp file
// rather than after the rename, so there is no window in which a live
// kubeconfig is briefly 0644.
//
// It deliberately does NOT claim to pin the Chmod-before-Sync ordering that makes
// the mode crash-durable: both orders leave the same mode visible to Stat, and
// distinguishing them needs fault injection between the two syscalls. That
// ordering is correct by inspection — fsync flushes the inode, so the chmod has
// to precede it — and is unverified here rather than falsely pinned.
func TestStageFileSetsModeBeforeRename(t *testing.T) {
	dir := t.TempDir()
	for _, mode := range []os.FileMode{0o600, 0o644} {
		final := filepath.Join(dir, fmt.Sprintf("f-%o", mode))
		s, err := stageFile(final, []byte("data"), mode)
		if err != nil {
			t.Fatal(err)
		}
		// Mode is checked on the TEMP file: os.CreateTemp opens at 0600, so a 0644
		// bundle proves the chmod ran during staging and not after the rename.
		fi, err := os.Stat(s.tmp)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode {
			t.Errorf("staged temp mode = %o, want %o (set during staging)", fi.Mode().Perm(), mode)
		}
	}
}

// TestAbortReportsWhatItCouldNotRemove: a swallowed failure here reports a clean
// abort while leaving credential-bearing kubeconfig temps on disk — the exact
// residue deploy/stratum-pve-lab.sh asserts is absent.
func TestAbortReportsWhatItCouldNotRemove(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory cannot be removed by os.Remove, for any uid including
	// root — a chmod-based denial would be a no-op under root.
	undeletable := filepath.Join(dir, ".bootstrap-kubeconfig.tmp-x")
	if err := os.MkdirAll(filepath.Join(undeletable, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok, err := stageFile(filepath.Join(dir, "removable"), []byte("x"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Undeletable FIRST: with it second, an abort that gave up on the first failure
	// would still have removed the other one and this assertion could never fail.
	u := &trustUpdate{staged: []stagedFile{{tmp: undeletable, final: "b"}, {tmp: ok.tmp, final: "a"}}}

	cerr := u.abort()
	if cerr == nil {
		t.Fatal("abort reported success while leaving a staged file on disk")
	}
	if !strings.Contains(cerr.Error(), undeletable) {
		t.Errorf("err = %v, want it to name the file left behind", cerr)
	}
	// It must still remove everything it can, and must not report the same problem twice.
	if _, err := os.Stat(ok.tmp); !os.IsNotExist(err) {
		t.Error("abort stopped at the first failure instead of clearing what it could")
	}
	if u.abort() != nil {
		t.Error("abort is not idempotent: the second call still reported a failure")
	}
}

// TestCommitOnFirstRenameSaysNothingWasApplied: a failure on the FIRST rename is
// a clean abort, and the message must not tell the operator otherwise. This
// string reaches them through a retained failed-Job pod log and is the only
// signal they get, so an all-caps partial-refresh warning here would push them
// toward rolling back a node whose files are byte-identical, mid-rotation.
func TestCommitOnFirstRenameSaysNothingWasApplied(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	if err := os.MkdirAll(filepath.Join(blocked, "child"), 0o755); err != nil {
		t.Fatal(err) // a non-empty dir cannot be replaced by a rename, for any uid
	}
	u := &trustUpdate{}
	for _, f := range []string{blocked, filepath.Join(dir, "never")} {
		s, err := stageFile(f, []byte("data"), 0o644)
		if err != nil {
			t.Fatal(err)
		}
		u.staged = append(u.staged, s)
	}
	err := u.commit()
	if err == nil {
		t.Fatal("commit over a non-renameable first path reported success")
	}
	if !strings.Contains(err.Error(), "nothing was applied") {
		t.Errorf("err = %v, want it to say nothing was applied", err)
	}
	if strings.Contains(err.Error(), "PARTIALLY REFRESHED") {
		t.Errorf("a clean abort was announced as a partial refresh: %v", err)
	}
	if err := u.abort(); err != nil {
		t.Errorf("abort after the failed commit: %v", err)
	}
}

// A split cluster's worker trusts the kubelet-client CA, not the root, for its
// kubelet's clients. A refresh of the root's trust moves ca-bundle.crt, ca.crt
// and the kubeconfigs and leaves that bundle alone in every direction, so a
// rotation of the root never hands the root back the kubelet trust.
func TestRefreshRotatesASplitKubeletBundle(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	oldKCA, newKCA := newTestCA(t, "stratum-kubelet-client-ca-old"), newTestCA(t, "stratum-kubelet-client-ca-new")
	for _, tc := range []struct {
		name                  string
		onDisk, fetched       []byte
		kubeletOn, kubeletPub []byte
		intent                trustIntent
	}{
		{"trust-new", bundle(oldCA), bundle(newCA, oldCA), bundle(oldKCA), bundle(newKCA, oldKCA), trustWiden},
		{"finalize", bundle(newCA, oldCA), bundle(newCA), bundle(newKCA, oldKCA), bundle(newKCA), trustNarrow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubKubeletClientCA(t, tc.kubeletPub, true)
			url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), tc.fetched)
			if tc.intent == trustNarrow {
				url = clusterInfo(t, newCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), tc.fetched)
			}
			p := node(t, tc.onDisk, url)
			if err := os.WriteFile(p.kubeletClientCA, tc.kubeletOn, 0o644); err != nil {
				t.Fatal(err)
			}
			u, err := planTrustRefresh(url, tc.intent, p)
			if err != nil {
				t.Fatalf("refresh refused on a split worker: %v", err)
			}
			if err := u.commit(); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(p.kubeletClientCA); err != nil || string(b) != string(tc.kubeletPub) {
				t.Errorf("the split kubelet bundle is %q (%v); want the published one", b, err)
			}
			if b, err := os.ReadFile(p.caBundle); err != nil || string(b) != string(tc.fetched) {
				t.Errorf("ca-bundle.crt does not carry the fetched bundle (%v)", err)
			}
		})
	}
}

// An unreadable kubelet bundle refuses the refresh rather than being replaced
// by the root's bundle: on a split worker that would undo the split.
func TestRefreshRefusesAnUnreadableKubeletBundle(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA, oldCA))
	p := node(t, bundle(oldCA), url)
	if err := os.WriteFile(p.kubeletClientCA, []byte("not PEM"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, p)
	if _, err := planTrustRefresh(url, trustWiden, p); err == nil {
		t.Fatal("refresh went ahead over an unreadable kubelet bundle")
	}
	assertCleanAbort(t, p, before)
}

func stubKubeletClientCA(t *testing.T, published []byte, found bool) {
	t.Helper()
	old := fetchKubeletClientCA
	fetchKubeletClientCA = func(string, []byte) ([]byte, bool, error) { return published, found, nil }
	t.Cleanup(func() { fetchKubeletClientCA = old })
}

// A split worker's kubelet bundle moves only as the rotation's phase allows: a
// trust-new whose published bundle drops a CA, a finalize that adds one, or a
// control plane that published nothing refuses the whole refresh unchanged.
func TestRefreshRefusesAKubeletBundleOutOfPhase(t *testing.T) {
	oldCA, newCA := newTestCA(t, "stratum-ca-old"), newTestCA(t, "stratum-ca-new")
	oldKCA, newKCA := newTestCA(t, "stratum-kubelet-client-ca-old"), newTestCA(t, "stratum-kubelet-client-ca-new")
	for _, tc := range []struct {
		name                  string
		kubeletOn, kubeletPub []byte
		found                 bool
	}{
		{"published bundle unchanged", bundle(oldKCA), bundle(oldKCA), true},
		{"published bundle drops the live CA", bundle(oldKCA), bundle(newKCA), true},
		{"nothing published", bundle(oldKCA), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubKubeletClientCA(t, tc.kubeletPub, tc.found)
			url := clusterInfo(t, oldCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA, oldCA))
			p := node(t, bundle(oldCA), url)
			if err := os.WriteFile(p.kubeletClientCA, tc.kubeletOn, 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, p)
			if _, err := planTrustRefresh(url, trustWiden, p); err == nil {
				t.Fatal("trust-new went ahead")
			}
			assertCleanAbort(t, p, before)
		})
	}
}

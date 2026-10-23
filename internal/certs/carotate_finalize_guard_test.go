package certs

import (
	"io"

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
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestCheckCPFinalize(t *testing.T) {
	stage := func(t *testing.T) {
		t.Helper()
		if err := pki.RotateCAMaterial(false); err != nil {
			t.Fatal(err)
		}
	}
	promote := func(t *testing.T) {
		t.Helper()
		if err := pki.PromoteCA(); err != nil {
			t.Fatal(err)
		}
		if err := pki.PromoteSA(); err != nil {
			t.Fatal(err)
		}
		if err := pki.MarkCAPromoted(); err != nil { // cpSignNew does this once the leaves are reissued
			t.Fatal(err)
		}
	}
	finalize := func(t *testing.T) {
		t.Helper()
		if err := pki.FinalizeCA(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T)
		wantErr bool
	}{
		{name: "sign-new refused: staged, never promoted", setup: stage, wantErr: true},
		{name: "never rotated: idle, never promoted", setup: func(*testing.T) {}, wantErr: true},
		{name: "crash mid-staging: ca-new.crt without its key", wantErr: true, setup: func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-new.crt"), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "sign-new half done: sa-new left behind", wantErr: true, setup: func(t *testing.T) {
			stage(t)
			if err := pki.PromoteCA(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "sign-new reached", setup: func(t *testing.T) { stage(t); promote(t) }},
		{name: "--force promotion with no old root key to back up", setup: func(t *testing.T) {
			if err := os.Remove(filepath.Join(version.PKIDir, "ca.key")); err != nil {
				t.Fatal(err)
			}
			if err := pki.RotateCAMaterial(true); err != nil {
				t.Fatal(err)
			}
			promote(t)
		}},
		{name: "promoted but never marked: older release, or sign-new failed before the leaves were reissued", wantErr: true, setup: func(t *testing.T) {
			stage(t)
			if err := pki.PromoteCA(); err != nil {
				t.Fatal(err)
			}
			if err := pki.PromoteSA(); err != nil {
				t.Fatal(err)
			}
			if !signerPromoted() {
				t.Fatal("fixture: the .pre-carotate backup should exist, and must not be proof")
			}
		}},
		{name: "retry after a finalize whose restart failed", setup: func(t *testing.T) { stage(t); promote(t); finalize(t) }},
		{name: "finalized, then rolled back", wantErr: true, setup: func(t *testing.T) {
			stage(t)
			promote(t)
			finalize(t)
			if err := pki.RollbackCA(); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "finalized, then a new rotation staged", wantErr: true, setup: func(t *testing.T) {
			stage(t)
			promote(t)
			finalize(t)
			stage(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prep(t)
			tc.setup(t)
			err := checkCPFinalize()
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkCPFinalize = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), "--rollback") {
				t.Errorf("refusal does not name --rollback: %v", err)
			}
			// cpFinalize must refuse before FinalizeCA discards anything staged.
			staged := pki.CAStagedFiles()
			bundleBefore, _ := os.ReadFile(filepath.Join(version.PKIDir, "ca-bundle.crt"))
			if ferr := cpFinalize(io.Discard, CARotateOptions{Phase: PhaseFinalize, Quiet: true}); ferr == nil {
				t.Fatal("cpFinalize ran after a refusal")
			}
			if got := pki.CAStagedFiles(); strings.Join(got, ",") != strings.Join(staged, ",") {
				t.Errorf("refused finalize changed the staged files: %v -> %v", staged, got)
			}
			if bundleAfter, _ := os.ReadFile(filepath.Join(version.PKIDir, "ca-bundle.crt")); string(bundleAfter) != string(bundleBefore) {
				t.Error("refused finalize rewrote the bundle")
			}
		})
	}
}

// A marker that cannot be removed must fail the phase, not leave finalize
// believing a stale record.
func TestMarkerRemovalFailureIsReported(t *testing.T) {
	for _, marker := range []string{"carotate-finalized", "carotate-promoted"} {
		for name, run := range map[string]func() error{
			"trust-new": func() error { return pki.RotateCAMaterial(false) },
			"rollback":  pki.RollbackCA,
		} {
			t.Run(marker+"/"+name, func(t *testing.T) {
				prep(t)
				dir := filepath.Join(version.PKIDir, marker)
				if err := os.MkdirAll(filepath.Join(dir, "keep"), 0o700); err != nil { // non-empty: removal fails even as root
					t.Fatal(err)
				}
				err := run()
				if err == nil || !strings.Contains(err.Error(), marker) {
					t.Fatalf("err = %v, want a failure naming %s", err, marker)
				}
				if pki.CARotationStaged() {
					t.Error("trust-new staged a rotation despite the failed marker removal")
				}
			})
		}
	}
}

func TestPromotedMarkerClearedWhenRotationEnds(t *testing.T) {
	for _, end := range []string{"finalize", "rollback"} {
		t.Run(end, func(t *testing.T) {
			prep(t)
			if err := pki.RotateCAMaterial(false); err != nil {
				t.Fatal(err)
			}
			for _, step := range []func() error{pki.PromoteCA, pki.PromoteSA} {
				if err := step(); err != nil {
					t.Fatal(err)
				}
			}
			if pki.CAPromoted() {
				t.Fatal("promoting the files must not record promotion; sign-new does that after the reissue")
			}
			if err := pki.MarkCAPromoted(); err != nil {
				t.Fatal(err)
			}
			if end == "finalize" {
				if err := pki.FinalizeCA(); err != nil {
					t.Fatal(err)
				}
			} else if err := pki.RollbackCA(); err != nil {
				t.Fatal(err)
			}
			if pki.CAPromoted() {
				t.Errorf("%s left the promotion record behind", end)
			}
		})
	}
}

// A promote that stops partway must neither consume anything nor
// leave a state finalize accepts.
func TestPartialPromotionIsRefused(t *testing.T) {
	prep(t)
	// trust-new stopped after the front-proxy certificate, before its key and
	// before any SA staging.
	if _, err := pki.NewCA("ca-new", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	if _, err := pki.NewCA("front-proxy-ca-new", "front-proxy-ca"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(version.PKIDir, "front-proxy-ca-new.key")); err != nil {
		t.Fatal(err)
	}
	liveCA, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	err = cpSignNew(io.Discard, CARotateOptions{Phase: PhaseSignNew, Quiet: true, SkipWorkloadCheck: true})
	if err == nil || !strings.Contains(err.Error(), "incomplete") || !strings.Contains(err.Error(), "front-proxy-ca-new.key") {
		t.Fatalf("expected an incomplete-staging refusal naming the missing key, got %v", err)
	}
	if now, _ := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt")); string(now) != string(liveCA) {
		t.Error("the live root was replaced by a refused promotion")
	}
	for _, f := range []string{"ca-new.crt", "ca-new.key", "front-proxy-ca-new.crt"} {
		if !fileExists(filepath.Join(version.PKIDir, f)) {
			t.Errorf("refused promotion consumed %s", f)
		}
	}
	if pki.CAPromoted() {
		t.Error("a refused promotion was recorded as done")
	}
	if err := checkCPFinalize(); err == nil {
		t.Fatal("finalize accepted a node whose promotion was refused")
	}
}

// The backup is not proof of promotion. A failed cleanup can leave one behind
// after a rollback; the old signer is live again and finalize must still refuse.
func TestStaleBackupAfterRollbackIsRefused(t *testing.T) {
	prep(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{pki.PromoteCA, pki.PromoteSA, pki.MarkCAPromoted, pki.RollbackCA} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	// Put back what a failed unlink would have left.
	old, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca.pre-carotate.crt"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	if !signerPromoted() {
		t.Fatal("fixture: the stale backup should be present")
	}
	if err := checkCPFinalize(); err == nil {
		t.Fatal("finalize accepted a stale backup after rollback restored the old signer")
	}
}

// A rollback that fails after restoring the root must not read as promoted.
func TestRollbackFailureAfterRootRestoreIsRefused(t *testing.T) {
	prep(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{pki.PromoteCA, pki.PromoteSA, pki.MarkCAPromoted} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	bundle := filepath.Join(version.PKIDir, "sa-bundle.pub")
	if err := os.Remove(bundle); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := pki.RollbackCA(); err == nil {
		t.Fatal("expected rollback failure after root restore")
	}
	if pki.CAPromoted() || pki.CAFinalized() {
		t.Fatal("markers should have been removed")
	}
	if err := checkCPFinalize(); err == nil {
		t.Fatal("finalize accepted a node whose rollback failed partway")
	}
}

// Finalize and rollback must report a rotation file they cannot delete. A
// non-empty directory makes os.Remove fail on every platform and as root.
func TestScratchRemovalFailureIsReported(t *testing.T) {
	for _, tc := range []struct{ phase, file string }{
		{"finalize", "ca.pre-carotate.crt"},
		{"rollback", "ca-new.key"},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			prep(t)
			if err := pki.RotateCAMaterial(false); err != nil {
				t.Fatal(err)
			}
			for _, step := range []func() error{pki.PromoteCA, pki.PromoteSA} {
				if err := step(); err != nil {
					t.Fatal(err)
				}
			}
			stuck := filepath.Join(version.PKIDir, tc.file)
			if err := os.RemoveAll(stuck); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(stuck, "keep"), 0o700); err != nil {
				t.Fatal(err)
			}
			run := pki.FinalizeCA
			if tc.phase == "rollback" {
				run = func() error {
					if err := pki.RollbackCA(); err != nil {
						return err
					}
					return pki.RemoveRotationScratch()
				}
			}
			err := run()
			if err == nil || !strings.Contains(err.Error(), tc.file) {
				t.Fatalf("err = %v, want a failure naming %s", err, tc.file)
			}
		})
	}
}

func TestCheckWorkerTrustWidened(t *testing.T) {
	oldCA, newCA := newTestCA(t, "old"), newTestCA(t, "new")
	// narrow rewrites one file's CA to the old CA only, as a trust-new that stopped
	// partway leaves it.
	narrow := func(files func(trustPaths) []string) func(*testing.T, trustPaths) {
		return func(t *testing.T, p trustPaths) {
			t.Helper()
			for _, f := range files(p) {
				if strings.HasSuffix(f, ".crt") {
					if err := os.WriteFile(f, bundle(oldCA), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				raw, err := kubeconfig.ReadWithClusterCA(f, bundle(oldCA))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	remove := func(file func(trustPaths) string) func(*testing.T, trustPaths) {
		return func(t *testing.T, p trustPaths) {
			t.Helper()
			if err := os.Remove(file(p)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name    string
		onDisk  []byte
		mutate  func(*testing.T, trustPaths)
		wantErr string // substring; "" means accepted
		noHints bool   // a refusal that is not about trust-new stopping partway
	}{
		{name: "trust-new never ran", onDisk: bundle(oldCA), wantErr: "trust-new has not finished"},
		{name: "same CA twice", onDisk: bundle(oldCA, oldCA), wantErr: "trust-new has not finished"},
		{name: "trust-new applied everywhere", onDisk: bundle(oldCA, newCA)},
		{name: "only the bootstrap kubeconfig not refreshed", onDisk: bundle(oldCA, newCA), wantErr: "bootstrap-kubeconfig",
			mutate: narrow(func(p trustPaths) []string { return []string{p.bootstrapKubeconfig} })},
		{name: "only kubelet.kubeconfig not refreshed", onDisk: bundle(oldCA, newCA), wantErr: "kubelet.kubeconfig",
			mutate: narrow(func(p trustPaths) []string { return []string{p.kubeletKubeconfig} })},
		{name: "bundle refreshed, both kubeconfigs not", onDisk: bundle(oldCA, newCA), wantErr: "kubelet.kubeconfig",
			mutate: narrow(func(p trustPaths) []string { return []string{p.kubeletKubeconfig, p.bootstrapKubeconfig} })},
		{name: "no bootstrap-kubeconfig: trust-new skips it too", onDisk: bundle(oldCA, newCA),
			mutate: remove(func(p trustPaths) string { return p.bootstrapKubeconfig })},
		{name: "no kubelet.kubeconfig", onDisk: bundle(oldCA, newCA), wantErr: "kubelet.kubeconfig", noHints: true,
			mutate: remove(func(p trustPaths) string { return p.kubeletKubeconfig })},
		{name: "only the ca.crt copy not refreshed: not a trust input of the kubelet", onDisk: bundle(oldCA, newCA),
			mutate: narrow(func(p trustPaths) []string { return []string{p.caCert} })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := node(t, tc.onDisk, "https://127.0.0.1:6443")
			if tc.mutate != nil {
				tc.mutate(t, p)
			}
			err := checkWorkerTrustWidened(p)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want a refusal naming %q", err, tc.wantErr)
			}
			if tc.noHints {
				return
			}
			for _, want := range []string{"--rollback", "--phase=" + PhaseTrustNew} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not offer %s: %v", want, err)
				}
			}
		})
	}
}

func clientCertSignedBy(t *testing.T, ca *testCA) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "system:node:w1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestCheckClientCertSurvives(t *testing.T) {
	oldCA, newCA := newTestCA(t, "ca"), newTestCA(t, "ca") // same subject: only the signature tells them apart
	for _, tc := range []struct {
		name    string
		cert    []byte // nil: no file
		raw     bool   // write cert as given, without the key block kubelet appends
		dropped []*x509.Certificate
		wantErr bool
		rejoin  bool
	}{
		{name: "sign-new refused: still old-signed", cert: clientCertSignedBy(t, oldCA), dropped: []*x509.Certificate{oldCA.cert}, wantErr: true},
		{name: "no client cert", dropped: []*x509.Certificate{oldCA.cert}, wantErr: true, rejoin: true},
		{name: "reissued under the new CA", cert: clientCertSignedBy(t, newCA), dropped: []*x509.Certificate{oldCA.cert}},
		{name: "nothing dropped (finalize re-run), cert present", cert: clientCertSignedBy(t, oldCA)},
		{name: "nothing dropped, no client cert: nothing for finalize to break"},
		{name: "something dropped, malformed client cert", cert: []byte("not a pem"), dropped: []*x509.Certificate{oldCA.cert}, wantErr: true, raw: true, rejoin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "kubelet-client-current.pem")
			if tc.cert != nil {
				// kubelet writes cert then key into the same file.
				content := tc.cert
				if !tc.raw {
					content = append(content, []byte("-----BEGIN EC PRIVATE KEY-----\nAA==\n-----END EC PRIVATE KEY-----\n")...)
				}
				if err := os.WriteFile(path, content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := checkClientCertSurvives(tc.dropped, path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			hint := "--rollback"
			if tc.rejoin {
				hint = "stratum uninstall"
			}
			if err != nil && !strings.Contains(err.Error(), hint) {
				t.Errorf("refusal does not name %s: %v", hint, err)
			}
		})
	}
}

// workerRefreshTrust is the finalize call site: a kubelet cert still signed by the
// dropped CA must abort before any of the four trust files is touched.
func TestWorkerFinalizeRefusesOldSignedClientCert(t *testing.T) {
	oldCA, newCA := newTestCA(t, "ca"), newTestCA(t, "ca")
	url := clusterInfo(t, newCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA))
	p := node(t, bundle(oldCA, newCA), url)
	savedPKI, savedKC, savedBootstrap := version.PKIDir, version.KubeconfigDir, version.BootstrapKubeconfig
	t.Cleanup(func() {
		version.PKIDir, version.KubeconfigDir, version.BootstrapKubeconfig = savedPKI, savedKC, savedBootstrap
	})
	version.PKIDir, version.KubeconfigDir, version.BootstrapKubeconfig = filepath.Dir(p.caBundle), filepath.Dir(p.kubeletKubeconfig), p.bootstrapKubeconfig
	if err := os.MkdirAll(filepath.Dir(kubeletClientCertPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kubeletClientCertPath(), clientCertSignedBy(t, oldCA), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, p)
	err := workerRefreshTrust(io.Discard, CARotateOptions{Phase: PhaseFinalize, Quiet: true}, trustNarrow)
	if err == nil || !strings.Contains(err.Error(), "still signed by a CA this finalize drops") {
		t.Fatalf("got %v", err)
	}
	assertCleanAbort(t, p, before)
}

// rollbackCPMaterial must keep the backups until the leaves and
// kubeconfigs are repaired, so a rollback whose cleanup fails, then retried,
// still ends with leaves signed by the restored root.
func promoteAndReissue(t *testing.T) {
	t.Helper()
	for _, step := range []func() error{pki.PromoteCA, pki.PromoteSA} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	hostname, nodeIP, extra, _, err := hostNodeIP()
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.RotateLeaves(hostname, nodeIP, extra); err != nil {
		t.Fatal(err)
	}
	if err := regenFromNodeState(); err != nil {
		t.Fatal(err)
	}
}

func assertLeafSignedByLiveRoot(t *testing.T) {
	t.Helper()
	ca, err := pki.LoadCA("ca")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(version.PKIDir, "apiserver.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatal("apiserver.crt holds no PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.CheckSignatureFrom(ca.Cert); err != nil {
		t.Fatalf("apiserver leaf is not signed by the restored live root: %v", err)
	}
}

func TestRollbackAfterUnmarkedPromotionRepairsLeaves(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	promoteAndReissue(t) // sign-new that never reached MarkCAPromoted
	if err := checkCPFinalize(); err == nil {
		t.Fatal("finalize accepted an unmarked promotion")
	}
	if err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	assertLeafSignedByLiveRoot(t)
}

func TestRollbackRetryAfterCleanupFailureRepairsLeaves(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	promoteAndReissue(t)
	if err := pki.MarkCAPromoted(); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(version.PKIDir, "ca-new.key") // a non-empty directory: unlink fails everywhere
	if err := os.MkdirAll(filepath.Join(stuck, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true})
	if err == nil || !strings.Contains(err.Error(), "remove rotation scratch") {
		t.Fatalf("expected the cleanup failure, got %v", err)
	}
	assertLeafSignedByLiveRoot(t) // repaired before the cleanup was attempted
	if pki.CAPromoted() || pki.CAFinalized() {
		t.Fatal("a failed rollback retained a marker")
	}
	if err := checkCPFinalize(); err == nil {
		t.Fatal("finalize passed after a failed rollback")
	}
	if err := os.RemoveAll(stuck); err != nil {
		t.Fatal(err)
	}
	if err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	assertLeafSignedByLiveRoot(t)
}

// If the repair itself fails, nothing is deleted, so the retry still knows the
// leaves need reissuing.
func TestRollbackKeepsBackupsWhenRepairFails(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	promoteAndReissue(t)
	if err := pki.MarkCAPromoted(); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(version.PKIDir, "apiserver.crt") // a non-empty directory: the reissue cannot write it
	if err := os.Remove(leaf); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(leaf, "keep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true}); err == nil {
		t.Fatal("expected the repair to fail")
	}
	if !signerPromoted() {
		t.Fatal("the backups went before the leaves were repaired")
	}
	if err := os.RemoveAll(leaf); err != nil {
		t.Fatal(err)
	}
	if err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true}); err != nil {
		t.Fatal(err)
	}
	assertLeafSignedByLiveRoot(t)
}

// Promotion counts only once the control plane restarted. After
// a failed restart finalize is refused and --rollback is a way out.
func TestSignNewRestartFailureIsNotPromotion(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	promoteAndReissue(t)
	boom := errors.New("etcd gate timed out")
	err := signNewRestartThen(func() error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the restart failure", err)
	}
	if pki.CAPromoted() {
		t.Fatal("a failed restart was recorded as a completed promotion")
	}
	if err := checkCPFinalize(); err == nil {
		t.Fatal("finalize accepted a node whose sign-new restart failed")
	}
	if err := rollbackCPMaterial(io.Discard, CARotateOptions{Quiet: true}); err != nil {
		t.Fatalf("rollback after the failed restart: %v", err)
	}
	assertLeafSignedByLiveRoot(t)
	if err := checkCPFinalize(); err == nil {
		t.Fatal("finalize accepted a rolled-back node")
	}
}

// After a failed sign-new restart, running sign-new again finishes it: the
// staging is consumed, so it skips the promotion, reissues, restarts and
// records it, and finalize then accepts the node. This is the way out of a
// forced rotation, which cannot be rolled back.
func TestSignNewRestartFailureResumes(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	promoteAndReissue(t)
	if err := signNewRestartThen(func() error { return errors.New("etcd gate timed out") }); err == nil {
		t.Fatal("fixture: the restart should fail")
	}
	if err := cpSignNew(io.Discard, CARotateOptions{Phase: PhaseSignNew, Quiet: true, SkipWorkloadCheck: true}); err != nil {
		t.Fatalf("re-running sign-new after a failed restart: %v", err)
	}
	if !pki.CAPromoted() {
		t.Fatal("the resumed sign-new did not record the promotion")
	}
	if err := checkCPFinalize(); err != nil {
		t.Fatalf("finalize refused a node whose sign-new resumed: %v", err)
	}
	assertLeafSignedByLiveRoot(t)
}

func TestSignNewRestartSuccessRecordsPromotion(t *testing.T) {
	kubeVIPNode(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	promoteAndReissue(t)
	if err := signNewRestartThen(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := checkCPFinalize(); err != nil {
		t.Fatalf("finalize refused after a completed sign-new: %v", err)
	}
}

// Every cluster entry of both kubeconfigs is a
// trust input the refresh rewrites, so each must hold two CAs.
func TestCheckWorkerTrustWidenedMultipleClusters(t *testing.T) {
	oldCA, newCA := newTestCA(t, "old"), newTestCA(t, "new")
	widened := bundle(oldCA, newCA)
	for _, target := range []string{"kubelet", "bootstrap"} {
		for _, narrow := range []bool{false, true} {
			t.Run(target+map[bool]string{false: "/all-widened", true: "/extra-old-only"}[narrow], func(t *testing.T) {
				p := node(t, widened, "https://127.0.0.1:6443")
				for _, path := range []string{p.kubeletKubeconfig, p.bootstrapKubeconfig} {
					cfg, err := clientcmd.LoadFromFile(path)
					if err != nil {
						t.Fatal(err)
					}
					ca := widened
					if narrow && ((target == "kubelet" && path == p.kubeletKubeconfig) || (target == "bootstrap" && path == p.bootstrapKubeconfig)) {
						ca = bundle(oldCA)
					}
					cfg.Clusters["extra"] = &clientcmdapi.Cluster{Server: "https://127.0.0.2:6443", CertificateAuthorityData: ca}
					if err := clientcmd.WriteToFile(*cfg, path); err != nil {
						t.Fatal(err)
					}
				}
				err := checkWorkerTrustWidened(p)
				if !narrow {
					if err != nil {
						t.Fatalf("all clusters widened: %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), `cluster "extra"`) || !strings.Contains(err.Error(), "--phase=trust-new") || !strings.Contains(err.Error(), "--rollback --yes") {
					t.Fatalf("an extra old-only cluster must be refused with recovery guidance: %v", err)
				}
			})
		}
	}
}

// A finalized worker on a pre-upgrade launch holds one CA and has no rotating
// client cert. Finalize drops nothing there, so it passes and can be re-run.
func TestFinalizedLegacyWorkerPassesFinalizeWithoutClientCert(t *testing.T) {
	newCA := newTestCA(t, "new")
	url := clusterInfo(t, newCA.serving(t, []net.IP{net.ParseIP("127.0.0.1")}), bundle(newCA))
	p := node(t, bundle(newCA), url)
	u, err := planTrustRefresh(url, trustNarrow, p)
	if err != nil {
		t.Fatalf("a finalized one-CA worker must plan a finalize: %v", err)
	}
	defer func() {
		if err := u.abort(); err != nil {
			t.Fatal(err)
		}
	}()
	if len(u.dropped) != 0 {
		t.Fatalf("fixture: nothing should be dropped, got %d", len(u.dropped))
	}
	missing := filepath.Join(t.TempDir(), "kubelet-client-current.pem")
	if err := checkClientCertSurvives(u.dropped, missing); err != nil {
		t.Fatalf("finalize refused a finalized legacy worker with no client cert: %v", err)
	}
}

// unreadableNodeState redirects node state to a malformed node.yaml, so any
// read of it fails.
func unreadableNodeState(t *testing.T) {
	t.Helper()
	oldConf, oldNet := version.K8sConfDir, version.NetConfigPath
	version.K8sConfDir = t.TempDir()
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath = oldConf, oldNet })
	if err := os.WriteFile(nodestate.StatePath(), []byte("vip: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// sign-new reads the node state before it promotes anything: on a node whose
// node.yaml cannot be read it refuses with the live CA still the signer and
// the staged one still staged, rather than leaving a promoted node whose
// leaves were never reissued.
func TestSignNewRefusesBeforePromotingOnUnreadableNodeState(t *testing.T) {
	prep(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	liveCA, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	unreadableNodeState(t)
	err = cpSignNew(io.Discard, CARotateOptions{Phase: PhaseSignNew, Quiet: true, SkipWorkloadCheck: true})
	if err == nil || !strings.Contains(err.Error(), "sign-new refused") || !strings.Contains(err.Error(), "node.yaml") {
		t.Fatalf("cpSignNew = %v, want a refusal naming node.yaml", err)
	}
	if now, _ := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt")); string(now) != string(liveCA) {
		t.Error("the live CA was replaced before the refusal")
	}
	if !fileExists(filepath.Join(version.PKIDir, "ca-new.crt")) {
		t.Error("the staged CA was consumed before the refusal")
	}
	if pki.CAPromoted() {
		t.Error("the refused promotion was recorded")
	}
}

// trust-new reads the node state before it fetches or stages anything, on the
// follower path too: on a node whose node.yaml cannot be read nothing is
// staged.
func TestTrustNewRefusesBeforeStagingOnUnreadableNodeState(t *testing.T) {
	for _, fetch := range []bool{false, true} {
		prep(t)
		unreadableNodeState(t)
		err := cpTrustNew(io.Discard, CARotateOptions{Phase: PhaseTrustNew, Quiet: true, FetchShared: fetch})
		if err == nil || !strings.Contains(err.Error(), "node.yaml") {
			t.Fatalf("fetch=%v: cpTrustNew = %v, want a refusal naming node.yaml", fetch, err)
		}
		if fileExists(filepath.Join(version.PKIDir, "ca-new.crt")) {
			t.Errorf("fetch=%v: CA material was staged before the refusal", fetch)
		}
	}
}

// On a split cluster the worker's clientCAFile holds the kubelet-client CA,
// which a rotation of the root never widens: sign-new must not refuse over it.
func TestCheckWorkerTrustWidenedSkipsASplitKubeletBundle(t *testing.T) {
	oldCA, newCA, kubeletCA := newTestCA(t, "old"), newTestCA(t, "new"), newTestCA(t, "kubelet-client-ca")
	p := node(t, bundle(newCA, oldCA), "https://127.0.0.1:6443")
	if err := os.WriteFile(p.kubeletClientCA, bundle(kubeletCA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkerTrustWidened(p); err != nil {
		t.Fatalf("sign-new refused a widened split worker: %v", err)
	}
}

// A rollback that lands between finalize's first guard and FinalizeCA must stop
// the finalize: collapsing trust then would pair the restored old root with new
// leaves and write a finalized marker.
func TestCPFinalizeRechecksGuardBeforeFinalizeCA(t *testing.T) {
	prep(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{pki.PromoteCA, pki.PromoteSA, pki.MarkCAPromoted} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	saved := kubeletBootstrapsServing
	t.Cleanup(func() { kubeletBootstrapsServing = saved })
	kubeletBootstrapsServing = func() (bool, error) { return false, pki.RollbackCA() }

	err := cpFinalize(io.Discard, CARotateOptions{Phase: PhaseFinalize, Quiet: true, SkipTokenCheck: true})
	if err == nil || !strings.Contains(err.Error(), "--rollback") {
		t.Fatalf("cpFinalize after an interleaved rollback = %v, want the finalize refusal", err)
	}
	if pki.CAFinalized() {
		t.Error("a refused finalize wrote the finalized marker")
	}
}

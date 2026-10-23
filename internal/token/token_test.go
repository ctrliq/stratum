package token

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// TestCACertHash checks that CACertHash computes sha256(SubjectPublicKeyInfo) in
// the "sha256:<hex>" form — the exact value clusterinfo.PinFor recomputes
// on the joining node, so the two ends of the discovery handshake agree.
func TestCACertHash(t *testing.T) {
	version.PKIDir = t.TempDir()
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}

	got, err := CACertHash()
	if err != nil {
		t.Fatal(err)
	}

	// independent recomputation of the expected hash
	raw, _ := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
	blk, _ := pem.Decode(raw)
	cert, _ := x509.ParseCertificate(blk.Bytes)
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	want := "sha256:" + hex.EncodeToString(sum[:])

	if got != want {
		t.Errorf("CACertHash = %q, want %q", got, want)
	}
}

func TestRandStr(t *testing.T) {
	a, b := randStr(6), randStr(6)
	if len(a) != 6 {
		t.Errorf("len = %d, want 6", len(a))
	}
	if a == b {
		t.Error("randStr returned identical values (not random)")
	}
	for _, c := range a {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			t.Errorf("unexpected char %q in token id", c)
		}
	}
}

// TestCreate_UsesTheClientItIsGiven pins that token.Create mints its
// bootstrap-token Secret through the client it is given — the CLI hands it the
// admin client, the MCP token_create handler the node connector's client — and
// this test fails if Create builds its own client from admin.conf.
// version.AdminConf points at a missing file, so a client built from it fails
// before the first API call; the fake clientset is the only API the mint can
// reach, and the fake's recorded actions are the proof the mint went through
// it.
func TestCreate_UsesTheClientItIsGiven(t *testing.T) {
	version.PKIDir = t.TempDir()
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	oldConf := version.AdminConf
	version.AdminConf = filepath.Join(t.TempDir(), "absent-admin.conf")
	t.Cleanup(func() { version.AdminConf = oldConf })

	// Nothing signs cluster-info on a fake cluster, so Create's signing
	// post-condition cannot pass. Shorten the wait the vars exist to shorten,
	// and expect that failure: the mint and every step before the wait still
	// run, and the mint is what this pin asserts on.
	oldTimeout, oldInterval := waitForSigningTimeout, waitForSigningInterval
	waitForSigningTimeout = 30 * time.Millisecond
	waitForSigningInterval = time.Millisecond
	t.Cleanup(func() { waitForSigningTimeout, waitForSigningInterval = oldTimeout, oldInterval })

	cs := fake.NewSimpleClientset()
	_, _, _, err := Create(io.Discard, cs, "https://10.0.0.5:6443", DefaultTokenTTL, false, nil)
	if err == nil || !strings.Contains(err.Error(), "cluster-info was not signed") {
		t.Fatalf("Create = %v; want the signing post-condition failure (a fake cluster has no bootstrapsigner)", err)
	}

	var minted *corev1.Secret
	for _, a := range cs.Actions() {
		if a.GetVerb() != "create" || a.GetResource().Resource != "secrets" || a.GetNamespace() != "kube-system" {
			continue
		}
		ca, ok := a.(k8stesting.CreateAction)
		if !ok {
			continue
		}
		if s, ok := ca.GetObject().(*corev1.Secret); ok && s.Type == corev1.SecretTypeBootstrapToken {
			minted = s
		}
	}
	if minted == nil {
		t.Fatal("Create created no bootstrap-token Secret in kube-system through the client it was given")
	}
	id := minted.StringData["token-id"]
	if len(id) != 6 || minted.Name != "bootstrap-token-"+id {
		t.Errorf("minted Secret %q carries token-id %q; want the six-character id half named in the Secret", minted.Name, id)
	}
	if strings.Contains(minted.StringData["auth-extra-groups"], k8sutil.ControlPlaneBootstrapGroup) {
		t.Error("a worker token must not carry the control-plane bootstrap group")
	}
}

// ── the stdout-guard retirement pins ─────────────────────────────────────────
//
// The mint's one print — the cluster-info signing wait on the control-plane
// path — goes to the writer the front ends hand down, never to os.Stdout,
// which on the MCP stdio transport is the JSON-RPC stream.

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

// TestCreate_TakesTheWriterTheFrontEndsHandIt pins the mint's signature at the
// seam both front ends' deps.Create wrap: the writer comes first, so the CLI
// can pass os.Stdout byte-identically and the MCP handler its capture buffer,
// and the signing wait inside reaches one or the other. Compile-level: a
// behavioural run needs a live signer budget this package deliberately has no
// fake for.
func TestCreate_TakesTheWriterTheFrontEndsHandIt(t *testing.T) {
	var _ func(io.Writer, kubernetes.Interface, string, time.Duration, bool, func(context.Context) error) (string, string, *metav1.OwnerReference, error) = Create
}

// TestWaitForClusterInfoSignature_WritesItsWaitingProgressToTheWriter pins the
// wait's half of the convention: the writer comes right after the context, and
// each "waiting for the control plane to sign cluster-info" report is written
// there while os.Stdout stays clean. The fake signer publishes on its third
// read, so the loop reports twice before the signature lands.
func TestWaitForClusterInfoSignature_WritesItsWaitingProgressToTheWriter(t *testing.T) {
	restoreInterval, restoreTimeout := waitForSigningInterval, waitForSigningTimeout
	waitForSigningInterval, waitForSigningTimeout = 5*time.Millisecond, 40*time.Millisecond
	defer func() { waitForSigningInterval, waitForSigningTimeout = restoreInterval, restoreTimeout }()

	f := existing(map[string]string{kcKey: "kc"})
	f.signAfterGets = 3

	var buf bytes.Buffer
	var err error
	onStdout := captureRealStdout(t, func() {
		err = waitForClusterInfoSignature(t.Context(), &buf, f, "abcdef")
	})
	if err != nil {
		t.Fatalf("waitForClusterInfoSignature: %v", err)
	}
	if !strings.Contains(buf.String(), "waiting for the control plane to sign cluster-info (attempt 1)") {
		t.Errorf("the signing wait's progress never reached the writer it was handed; it has:\n%s", buf.String())
	}
	if strings.Contains(onStdout, "waiting for the control plane") {
		t.Errorf("the signing wait printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

// hashFor produces the reference pin through CACertHashFor, the same producer
// CACertHash consumes, so the rows compare WHICH root is pinned, not a hash
// spelling.
func hashFor(t *testing.T, name string) string {
	t.Helper()
	h, err := CACertHashFor(name)
	if err != nil {
		t.Fatalf("CACertHashFor(%q): %v", name, err)
	}
	return h
}

// TestCACertHashPinsTheStagedRootThroughEveryRotationPhase pins which root a
// minted token is pinned against at each phase of a CA rotation. The pin must
// name the root the cluster still trusts after finalize: from trust-new on,
// that is the staged root (ca-new), which finalize renames to the live ca.crt,
// while idle and a completed rollback pin the live old root. A token pinned to
// the old root during the rotation would fail its join after finalize; the
// boundary states are built with real pki calls so the pins break if pki's
// staged names or phase functions move.
func TestCACertHashPinsTheStagedRootThroughEveryRotationPhase(t *testing.T) {
	fresh := func(t *testing.T) map[string]string {
		t.Helper()
		old := version.PKIDir
		t.Cleanup(func() { version.PKIDir = old })
		version.PKIDir = t.TempDir()
		if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
			t.Fatal(err)
		}
		return map[string]string{"old": hashFor(t, "ca")}
	}
	// staged reaches trust-new and records both roots' pins while each still has
	// its own file: the old root in ca.crt, the new in ca-new.crt.
	staged := func(t *testing.T) map[string]string {
		refs := fresh(t)
		if err := pki.RotateCAMaterial(false); err != nil {
			t.Fatal(err)
		}
		refs["new"] = hashFor(t, "ca-new")
		return refs
	}
	promote := func(t *testing.T) {
		t.Helper()
		if err := pki.PromoteCA(); err != nil {
			t.Fatal(err)
		}
		if err := pki.PromoteSA(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name     string
		reach    func(t *testing.T) map[string]string
		wantRoot string // key into refs: the root CACertHash() must pin
	}{
		{
			name: "idle pins the live root", reach: fresh, wantRoot: "old",
		},
		{
			name: "trust-new pins the staged root", reach: staged, wantRoot: "new",
		},
		{
			name: "sign-new still pins the staged root",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				promote(t)
				return refs
			},
			wantRoot: "new",
		},
		{
			name: "finalized pins the new root under its live name",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				promote(t)
				if err := pki.FinalizeCA(); err != nil {
					t.Fatal(err)
				}
				return refs
			},
			wantRoot: "new",
		},
		{
			name: "a completed rollback pins the restored old root",
			reach: func(t *testing.T) map[string]string {
				refs := staged(t)
				promote(t)
				if err := pki.RollbackCA(); err != nil {
					t.Fatal(err)
				}
				if err := pki.RemoveRotationScratch(); err != nil {
					t.Fatal(err)
				}
				return refs
			},
			wantRoot: "old",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := tc.reach(t)
			got, err := CACertHash()
			if err != nil {
				t.Fatalf("CACertHash: %v", err)
			}
			if want := refs[tc.wantRoot]; got != want {
				t.Errorf("CACertHash() = %s, want the %s root's pin %s", got, tc.wantRoot, want)
			}
		})
	}
}

var errMaintenanceLost = errors.New("maintenance lease lost")

// TestCreateRefusesPublicationAfterLosingTheLease is the mid-helper regression
// for the mint path: the fence's recheck rides inside the publication helpers,
// so a lease lost while an intermediate call stalled refuses the next
// ConfigMap write instead of publishing this node's trust snapshot over the
// successor's. The recheck passes for the mint, then refuses; the
// cluster-info kubeconfig write must never start, and the token Secret the
// mint did create is revoked with the failure.
func TestCreateRefusesPublicationAfterLosingTheLease(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var checks atomic.Int32
	confirm := func(context.Context) error {
		if checks.Add(1) > 1 {
			return errMaintenanceLost
		}
		return nil
	}
	_, _, _, err := Create(io.Discard, cs, "https://10.0.0.5:6443", DefaultTokenTTL, false, confirm)
	if !errors.Is(err, errMaintenanceLost) {
		t.Fatalf("Create = %v, want the lost-lease refusal from the publication step", err)
	}
	// The token the mint created is gone: the run reports failure, so the
	// credential-lifecycle guarantee revokes it.
	secrets, serr := cs.CoreV1().Secrets("kube-system").List(context.Background(), metav1.ListOptions{})
	if serr != nil || len(secrets.Items) != 0 {
		t.Fatalf("bootstrap secrets after the refusal = %d (err %v); a failed run leaves no credential behind", len(secrets.Items), serr)
	}
	// And the published trust is untouched: the refused write never happened.
	cms, cerr := cs.CoreV1().ConfigMaps("kube-public").List(context.Background(), metav1.ListOptions{})
	if cerr != nil {
		t.Fatal(cerr)
	}
	for _, cm := range cms.Items {
		if cm.Name == "cluster-info" {
			t.Error("cluster-info was published although the maintenance lease was lost before the write")
		}
	}
}

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

// Checks the value clusterinfo.PinFor recomputes on the joining node agrees.
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

// The fake clientset is the only API the mint can reach, so its recorded actions prove the client.
func TestCreate_UsesTheClientItIsGiven(t *testing.T) {
	version.PKIDir = t.TempDir()
	if err := pki.GenerateAll("cp1", "10.0.0.5", nil); err != nil {
		t.Fatal(err)
	}
	oldConf := version.AdminConf
	version.AdminConf = filepath.Join(t.TempDir(), "absent-admin.conf")
	t.Cleanup(func() { version.AdminConf = oldConf })

	// No fake signer exists, so expect the signing post-condition failure.
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

// The mint's one print goes to the writer handed in, never to os.Stdout (the MCP stdio stream).

// captureRealStdout captures the process's actual stdout while fn runs.
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

// Compile-level: a behavioural run needs a live signer this package has no fake for.
func TestCreate_TakesTheWriterTheFrontEndsHandIt(t *testing.T) {
	var _ func(io.Writer, kubernetes.Interface, string, time.Duration, bool, func(context.Context) error) (string, string, *metav1.OwnerReference, error) = Create
}

// The writer comes right after the context; os.Stdout stays clean.
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

// Rows compare which root is pinned, not a hash spelling.
func hashFor(t *testing.T, name string) string {
	t.Helper()
	h, err := CACertHashFor(name)
	if err != nil {
		t.Fatalf("CACertHashFor(%q): %v", name, err)
	}
	return h
}

// The pin must name the root still trusted after finalize; states use real pki calls.
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
	// Records both roots' pins while each still has its own file.
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

// The recheck rides inside the publication helpers, so a stalled call cannot publish stale trust.
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
	// A failed run leaves no credential behind.
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

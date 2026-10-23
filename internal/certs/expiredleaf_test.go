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
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/version"
)

// expireLeaf replaces the kube-vip leaf with one that expired two days ago.
func expireLeaf(t *testing.T) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kube-vip"},
		NotBefore:    time.Now().Add(-400 * 24 * time.Hour),
		NotAfter:     time.Now().Add(-48 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(version.PKIDir, "kube-vip.crt"), pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
}

// An expired leaf is what keeps the apiserver from answering, so the cluster
// fence cannot be taken through it. Renewal then rotates under the node lock
// alone instead of refusing forever; a fence another run holds still refuses,
// and so does an unreachable API while nothing has expired.
func TestLeafRenewalOfExpiredLeavesNeedsNoFence(t *testing.T) {
	unauthorized := func(t *testing.T) {
		cs := fakeAdminClientset(t)
		cs.PrependReactor("*", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewUnauthorized("x509: certificate has expired")
		})
	}

	t.Run("expired leaf, API rejects admin.conf: rotates", func(t *testing.T) {
		kubeVIPNode(t)
		unauthorized(t)
		expireLeaf(t)
		before := readLeaf(t, "kube-vip.crt")
		var out bytes.Buffer
		if _, err := Rotate(&out, Options{Quiet: true, IfExpiringDays: 45}); err != nil {
			t.Fatalf("renewal of an expired leaf refused: %v", err)
		}
		if bytes.Equal(before, readLeaf(t, "kube-vip.crt")) {
			t.Fatal("the expired leaf was not reissued")
		}
		if !strings.Contains(out.String(), "node lock alone") {
			t.Errorf("no warning that the fence was skipped: %q", out.String())
		}
	})

	t.Run("expired leaf, fence held by another run: refuses", func(t *testing.T) {
		kubeVIPNode(t)
		fakeAdminClientset(t, liveMaintenanceLease("rotation-on-other-node"))
		expireLeaf(t)
		before := readLeaf(t, "kube-vip.crt")
		_, err := Rotate(&bytes.Buffer{}, Options{Quiet: true, IfExpiringDays: 45})
		if err == nil || !strings.Contains(err.Error(), "rotation-on-other-node") {
			t.Fatalf("renewal must refuse a fence another run holds: %v", err)
		}
		if !bytes.Equal(before, readLeaf(t, "kube-vip.crt")) {
			t.Fatal("renewal changed leaves under another cluster holder")
		}
	})

	t.Run("expired leaf, run cancelled while taking the fence: changes nothing", func(t *testing.T) {
		kubeVIPNode(t)
		cs := fakeAdminClientset(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cs.PrependReactor("*", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
			cancel() // shutdown lands while the lease call is in flight
			return true, nil, context.Canceled
		})
		expireLeaf(t)
		before := readLeaf(t, "kube-vip.crt")
		if _, err := RotateContext(ctx, &bytes.Buffer{}, Options{Quiet: true, IfExpiringDays: 45}); err == nil {
			t.Fatal("a cancelled run reported success")
		}
		if !bytes.Equal(before, readLeaf(t, "kube-vip.crt")) {
			t.Fatal("a cancelled run reissued leaves it could not finish rolling out")
		}
	})

	t.Run("nothing expired, API rejects admin.conf: refuses", func(t *testing.T) {
		kubeVIPNode(t)
		unauthorized(t)
		before := readLeaf(t, "kube-vip.crt")
		if _, err := Rotate(&bytes.Buffer{}, Options{Quiet: true}); err == nil {
			t.Fatal("a forced rotation with live leaves ran without the fence")
		}
		if !bytes.Equal(before, readLeaf(t, "kube-vip.crt")) {
			t.Fatal("renewal changed leaves without the fence")
		}
	})
}

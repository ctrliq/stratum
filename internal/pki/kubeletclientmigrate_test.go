package pki

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func assertKubeletState(t *testing.T, when, want string) KubeletClientTrust {
	t.Helper()
	tr, err := ReadKubeletClientTrust()
	if err != nil {
		t.Fatalf("%s: ReadKubeletClientTrust: %v", when, err)
	}
	if got := tr.State(); got != want {
		t.Fatalf("%s: state = %s, want %s", when, got, want)
	}
	return tr
}

func assertLeafTrustedByKubelet(t *testing.T, when string) {
	t.Helper()
	if _, err := loadCert(t, "apiserver-kubelet-client").Verify(x509.VerifyOptions{
		Roots: poolFromBundle(t, KubeletClientBundleBase), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("%s: apiserver-kubelet-client does not verify against the kubelet clientCAFile: %v", when, err)
	}
}

func assertPKIUnchanged(t *testing.T, when string, before map[string]string) {
	t.Helper()
	after := pkiSnapshot(t)
	for f, b := range before {
		if after[f] != b {
			t.Errorf("%s changed %s", when, f)
		}
	}
	for f := range after {
		if _, ok := before[f]; !ok {
			t.Errorf("%s wrote %s", when, f)
		}
	}
}

// Each phase moves a legacy control plane one step, and the apiserver's kubelet
// client certificate stays trusted by its kubelet at every step. Until
// drop-root the kubelet still trusts the root, and the root-bundle sync that
// a restart or a rotation runs leaves the widened bundle alone.
func TestKubeletClientMigrationMovesALegacyNodeOntoItsOwnCA(t *testing.T) {
	genLegacy(t)
	assertKubeletState(t, "legacy", KubeletStateNotMigrated)

	if err := KubeletClientTrustBoth(true); err != nil {
		t.Fatal(err)
	}
	tr := assertKubeletState(t, KubeletPhaseTrustBoth, KubeletStateTrustBoth)
	if len(tr.Anchors) != 1 || !tr.Anchors[0].Equal(loadCert(t, KubeletClientCAName)) || len(tr.Roots) != 1 {
		t.Fatalf("trust-both: kubelet bundle holds %d anchors and %d roots, want %s and the root", len(tr.Anchors), len(tr.Roots), KubeletClientCAName)
	}
	assertLeafTrustedByKubelet(t, KubeletPhaseTrustBoth)
	before := readPKI(t, KubeletClientBundleBase+"-bundle.crt")
	if err := SyncKubeletClientBundle(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readPKI(t, KubeletClientBundleBase+"-bundle.crt")) {
		t.Fatal("SyncKubeletClientBundle dropped the kubelet-client CA from a migrating node's kubelet bundle")
	}
	if inUse, err := KubeletClientCAInUse(); err != nil || !inUse {
		t.Errorf("KubeletClientCAInUse = %v, %v at trust-both, want true", inUse, err)
	}

	if err := KubeletClientIssueNew(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	assertKubeletState(t, KubeletPhaseIssueNew, KubeletStateIssueNew)
	assertLeafTrustedByKubelet(t, KubeletPhaseIssueNew)

	if err := KubeletClientDropRoot(); err != nil {
		t.Fatal(err)
	}
	assertKubeletState(t, KubeletPhaseDropRoot, KubeletStateMigrated)
	assertSplit(t, KubeletPhaseDropRoot)
}

// The rollback phases take a migrated control plane back to the root, its
// kubelet trusting the apiserver's client certificate at every step.
func TestKubeletClientRollbackReturnsAMigratedNodeToTheRoot(t *testing.T) {
	genInto(t)
	assertKubeletState(t, "fresh install", KubeletStateMigrated)

	if err := KubeletClientReAddRoot(); err != nil {
		t.Fatal(err)
	}
	assertKubeletState(t, KubeletPhaseReAddRoot, KubeletStateIssueNew)
	assertLeafTrustedByKubelet(t, KubeletPhaseReAddRoot)

	if err := KubeletClientReissueRoot(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	assertKubeletState(t, KubeletPhaseReissueRoot, KubeletStateTrustBoth)
	assertLeafTrustedByKubelet(t, KubeletPhaseReissueRoot)

	if err := KubeletClientRemoveCA(); err != nil {
		t.Fatal(err)
	}
	assertKubeletState(t, KubeletPhaseRemoveCA, KubeletStateNotMigrated)
	for _, ext := range []string{"crt", "key"} {
		if fileExists(path(KubeletClientCAName, ext)) {
			t.Errorf("remove-ca left %s.%s", KubeletClientCAName, ext)
		}
	}
	assertKubeletTrustsAPIServer(t, KubeletPhaseRemoveCA)
}

// A phase run out of order, or on a node it does not fit, is refused before it
// writes anything.
func TestKubeletClientMigrationRefusesAPhaseOutOfOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		run   func() error
		want  string
	}{
		{"a follower that did not fetch the CA", genLegacy, func() error { return KubeletClientTrustBoth(false) }, "refusing to mint"},
		{"trust-both on a migrated node", genInto, func() error { return KubeletClientTrustBoth(true) }, "already trusts kubelet-client-ca alone"},
		{"issue-new before trust-both", genLegacy, func() error { return KubeletClientIssueNew(tHost, tIP, nil) }, "run --phase=trust-both"},
		{"drop-root before trust-both", genLegacy, KubeletClientDropRoot, "trusts no kubelet-client CA"},
		{"drop-root before issue-new", func(t *testing.T) {
			genLegacy(t)
			if err := KubeletClientTrustBoth(true); err != nil {
				t.Fatal(err)
			}
		}, KubeletClientDropRoot, "run --phase=issue-new"},
		{"reissue-root before re-add-root", genInto, func() error { return KubeletClientReissueRoot(tHost, tIP, nil) }, "run --phase=re-add-root"},
		{"remove-ca before reissue-root", func(t *testing.T) {
			genInto(t)
			if err := KubeletClientReAddRoot(); err != nil {
				t.Fatal(err)
			}
		}, KubeletClientRemoveCA, "run --phase=reissue-root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			before := pkiSnapshot(t)
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal containing %q", err, tc.want)
			}
			assertPKIUnchanged(t, tc.name, before)
		})
	}
}

// A CA rotation would rewrite the kubelet bundle from ca-bundle.crt and drop
// the kubelet-client CA, so it is refused part way through the migration.
func TestCARotationRefusesAMigratingNode(t *testing.T) {
	genLegacy(t)
	if err := KubeletClientTrustBoth(true); err != nil {
		t.Fatal(err)
	}
	before := pkiSnapshot(t)
	err := RotateCAMaterial(false)
	if err == nil || !strings.Contains(err.Error(), "part way through the kubelet client-CA migration") {
		t.Fatalf("RotateCAMaterial = %v, want a refusal naming the migration", err)
	}
	assertPKIUnchanged(t, "the refused rotation", before)
}

// A legacy rotation's finalize leaves the kubelet bundle old‖new beside a new
// ca-bundle.crt before the sync. No kubelet-client CA is in it, so it is not
// read as a migration and the sync narrows it.
func TestALegacyRotationIsNotAMigration(t *testing.T) {
	genLegacy(t)
	for _, phase := range []func() error{func() error { return RotateCAMaterial(false) }, PromoteCA} {
		if err := phase(); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteCABundle("ca", "ca"); err != nil {
		t.Fatal(err)
	}
	if m, err := KubeletClientMigratingPEM(KubeletClientBundlePath(), readPKI(t, "ca-bundle.crt"), "ca-bundle.crt"); err != nil || m {
		t.Fatalf("KubeletClientMigratingPEM = %v, %v, want false", m, err)
	}
	if err := SyncKubeletClientBundle(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readPKI(t, KubeletClientBundleBase+"-bundle.crt"), readPKI(t, "ca-bundle.crt")) {
		t.Error("the sync did not narrow a legacy kubelet bundle to ca-bundle.crt")
	}
}

func TestAddKubeletClientAnchorTakesOnlyAKubeletClientCA(t *testing.T) {
	genLegacy(t)
	before := pkiSnapshot(t)
	if err := AddKubeletClientAnchor(loadCert(t, "front-proxy-ca")); err == nil {
		t.Fatal("AddKubeletClientAnchor took the front-proxy CA")
	}
	assertPKIUnchanged(t, "the refused anchor", before)

	other := t.TempDir()
	version.PKIDir, other = other, version.PKIDir
	ca, err := NewCA(KubeletClientCAName, kubeletClientCACommonName)
	version.PKIDir = other
	if err != nil {
		t.Fatal(err)
	}
	if err := AddKubeletClientAnchor(ca.Cert); err != nil {
		t.Fatal(err)
	}
	assertKubeletState(t, "worker after trust-both", KubeletStateTrustBoth)
	if err := AddKubeletClientAnchor(ca.Cert); err != nil {
		t.Fatal(err)
	}
	if tr, _ := ReadKubeletClientTrust(); len(tr.Anchors) != 1 {
		t.Errorf("a second AddKubeletClientAnchor left %d anchors, want 1", len(tr.Anchors))
	}
}

// ProbeKubeletClientAuth reports the kubelet's answer to the client
// certificate it presents.
func TestProbeKubeletClientAuthReportsTheKubeletsAnswer(t *testing.T) {
	genInto(t)
	pool := poolFromBundle(t, KubeletClientBundleBase)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inter := x509.NewCertPool()
		for _, c := range r.TLS.PeerCertificates[1:] {
			inter.AddCert(c)
		}
		if _, err := r.TLS.PeerCertificates[0].Verify(x509.VerifyOptions{
			Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	roots := []*x509.Certificate{srv.Certificate()}

	for leaf, want := range map[string]int{"apiserver-kubelet-client": 200, "admin": 401} {
		client, err := LeafKeyPair(leaf)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ProbeKubeletClientAuth(context.Background(), addr, roots, client)
		if err != nil || got != want {
			t.Errorf("%s: ProbeKubeletClientAuth = %d, %v, want %d", leaf, got, err, want)
		}
	}
	client, err := LeafKeyPair("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeKubeletClientAuth(context.Background(), addr, []*x509.Certificate{loadCert(t, "ca")}, client); err == nil {
		t.Error("ProbeKubeletClientAuth trusted a kubelet serving certificate its roots did not sign")
	}
}

// A legacy node's unreadable kubelet bundle does not stop a rotation, which
// re-syncs it from ca-bundle.crt.
func TestCARotationStillRepairsALegacyKubeletBundle(t *testing.T) {
	genLegacy(t)
	if err := os.WriteFile(KubeletClientBundlePath(), []byte("not PEM"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RotateCAMaterial(false); err != nil {
		t.Fatalf("RotateCAMaterial = %v, want the rotation to go ahead and repair the bundle", err)
	}
	assertKubeletTrustsAPIServer(t, "trust-new over an unreadable legacy bundle")
}

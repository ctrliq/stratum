package pki

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"

	"github.com/ctrliq/stratum/internal/host"
)

// Migrates an existing cluster to KubeletClientCAName.
const KubeletMigrateCommand = "stratum certs migrate-kubelet-client-ca"

// The first three phases move onto KubeletClientCAName, the last three back to the root.
const (
	KubeletPhaseTrustBoth   = "trust-both"
	KubeletPhaseIssueNew    = "issue-new"
	KubeletPhaseDropRoot    = "drop-root"
	KubeletPhaseReAddRoot   = "re-add-root"
	KubeletPhaseReissueRoot = "reissue-root"
	KubeletPhaseRemoveCA    = "remove-ca"
)

// Client-trust states reported by KubeletClientTrust.State.
const (
	KubeletStateNotMigrated = "not-migrated"
	KubeletStateTrustBoth   = "trust-both"
	KubeletStateIssueNew    = "issue-new"
	KubeletStateMigrated    = "migrated"
)

// KubeletClientTrust is this node's kubelet client trust.
type KubeletClientTrust struct {
	// In the kubelet bundle: Anchors are kubelet-client CAs absent from ca-bundle.crt; Roots shared.
	Anchors []*x509.Certificate
	Roots   []*x509.Certificate
	Other   []*x509.Certificate
	// Only a control plane holds kubelet-client-ca.crt.
	HasCA bool
	// apiserver-kubelet-client signed by kubelet-client-ca.crt.
	LeafOnCA bool
}

// ReadKubeletClientTrust classifies this node's kubelet client trust.
func ReadKubeletClientTrust() (KubeletClientTrust, error) {
	t := KubeletClientTrust{HasCA: fileExists(path(KubeletClientCAName, "crt"))}
	if t.HasCA {
		on, err := leafChainsTo("apiserver-kubelet-client", KubeletClientCAName)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return t, fmt.Errorf("pki: tell which CA signed apiserver-kubelet-client: %w", err)
		}
		t.LeafOnCA = on
	}
	if !bundleExists(KubeletClientBundleBase) {
		return t, nil
	}
	have, err := bundleCerts(KubeletClientBundlePath())
	if err != nil {
		return t, err
	}
	roots, err := bundleCerts(bundleCrtPath("ca"))
	if err != nil {
		return t, err
	}
	t.classify(have, roots)
	return t, nil
}

// Reports whether kubeletBundle trusts both a kubelet-client CA and a CA from rootSource.
func KubeletClientMigratingPEM(kubeletBundle string, rootPEM []byte, rootSource string) (bool, error) {
	if _, err := os.Stat(kubeletBundle); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	have, err := bundleCerts(kubeletBundle)
	if err != nil {
		return false, err
	}
	roots, err := parseBundle(rootPEM, rootSource)
	if err != nil {
		return false, err
	}
	var t KubeletClientTrust
	t.classify(have, roots)
	return len(t.Anchors) > 0 && len(t.Roots) > 0, nil
}

func (t *KubeletClientTrust) classify(have, roots []*x509.Certificate) {
	for _, c := range have {
		switch {
		case slices.ContainsFunc(roots, c.Equal):
			t.Roots = append(t.Roots, c)
		case isKubeletClientCA(c):
			t.Anchors = append(t.Anchors, c)
		default:
			t.Other = append(t.Other, c)
		}
	}
}

func isKubeletClientCA(c *x509.Certificate) bool {
	return c.IsCA && c.Subject.CommonName == kubeletClientCACommonName
}

func (t KubeletClientTrust) State() string {
	switch {
	case len(t.Roots) == 0 && (len(t.Anchors) > 0 || t.HasCA):
		return KubeletStateMigrated
	case len(t.Anchors) == 0:
		return KubeletStateNotMigrated
	case t.LeafOnCA:
		return KubeletStateIssueNew
	}
	return KubeletStateTrustBoth
}

// A node whose kubelet trusts both a kubelet-client CA and the cluster root.
func (t KubeletClientTrust) Migrating() bool {
	s := t.State()
	return s == KubeletStateTrustBoth || s == KubeletStateIssueNew
}

// True part way through the migration, which a CA rotation must not interleave with.
func KubeletClientMigrating() (bool, error) {
	t, err := ReadKubeletClientTrust()
	if err != nil {
		return false, err
	}
	return t.Migrating(), nil
}

// True whether this node's kubelet trusts a kubelet-client CA, split or migrating.
func KubeletClientCAInUse() (bool, error) {
	split, err := KubeletClientSplit()
	if err != nil || split {
		return split, err
	}
	return KubeletClientMigrating()
}

// Refuses mid-migration; without KubeletClientCAName an unreadable bundle is left to the caller.
func refuseWhileKubeletMigrating(what string) error {
	t, err := ReadKubeletClientTrust()
	if err != nil && !t.HasCA {
		return nil
	}
	if err != nil {
		return err
	}
	if t.Migrating() {
		return fmt.Errorf("pki: cannot %s: this node is part way through the kubelet client-CA migration (%s); finish it with `%s --cluster --yes` or undo it with `%s --cluster --rollback --yes` first",
			what, t.State(), KubeletMigrateCommand, KubeletMigrateCommand)
	}
	return nil
}

// Trusts KubeletClientCAName beside the root; only the initiating node may mint the CA.
func KubeletClientTrustBoth(allowMint bool) error {
	t, err := ReadKubeletClientTrust()
	if err != nil {
		return err
	}
	if t.State() == KubeletStateMigrated {
		return fmt.Errorf("pki: refusing %s: this node's kubelet already trusts %s alone, and re-adding the cluster root would let every root-signed client certificate reach it again; run `%s --cluster --yes` to finish the migration",
			KubeletPhaseTrustBoth, KubeletClientCAName, KubeletMigrateCommand)
	}
	if !fileExists(path("ca", "crt")) {
		return fmt.Errorf("pki: %s needs the cluster root certificate %s", KubeletPhaseTrustBoth, path("ca", "crt"))
	}
	if !exists(KubeletClientCAName) {
		if !allowMint {
			return fmt.Errorf("pki: refusing to mint %s on a follower: this node did not receive the cluster's %s.crt and .key, and a CA of its own would sign a client certificate no other kubelet trusts",
				KubeletClientCAName, KubeletClientCAName)
		}
		if _, err := NewCA(KubeletClientCAName, kubeletClientCACommonName); err != nil {
			return fmt.Errorf("pki: mint %s: %w", KubeletClientCAName, err)
		}
	}
	ca, err := readCert(KubeletClientCAName)
	if err != nil {
		return err
	}
	roots, err := bundleCerts(bundleCrtPath("ca"))
	if err != nil {
		return err
	}
	return writeKubeletBundle(append([]*x509.Certificate{ca}, roots...))
}

// Reissues apiserver-kubelet-client under KubeletClientCAName; every kubelet must trust it.
func KubeletClientIssueNew(host, nodeIP string, sans *APISANs) error {
	t, err := ReadKubeletClientTrust()
	if err != nil {
		return err
	}
	if !t.HasCA || len(t.Anchors) == 0 {
		return fmt.Errorf("pki: %s needs this node's kubelet to trust %s first; run --phase=%s", KubeletPhaseIssueNew, KubeletClientCAName, KubeletPhaseTrustBoth)
	}
	ca, err := loadCA(KubeletClientCAName)
	if err != nil {
		return fmt.Errorf("pki: load %s: %w", KubeletClientCAName, err)
	}
	return ca.Reissue(kubeletClientLeafSpec(host, nodeIP, sans))
}

// Trusts the held kubelet-client CAs alone; the leaf must already be signed by KubeletClientCAName.
func KubeletClientDropRoot() error {
	t, err := ReadKubeletClientTrust()
	if err != nil {
		return err
	}
	if len(t.Anchors) == 0 {
		return fmt.Errorf("pki: refusing %s: this node's kubelet trusts no kubelet-client CA, so dropping the root would leave it trusting nothing; run --phase=%s first",
			KubeletPhaseDropRoot, KubeletPhaseTrustBoth)
	}
	if t.HasCA && !t.LeafOnCA {
		return fmt.Errorf("pki: refusing %s: apiserver-kubelet-client here is not signed by %s yet, so its apiserver would lose every kubelet; run --phase=%s first",
			KubeletPhaseDropRoot, KubeletClientCAName, KubeletPhaseIssueNew)
	}
	if t.HasCA {
		ca, err := readCert(KubeletClientCAName)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(t.Anchors, ca.Equal) {
			return fmt.Errorf("pki: refusing %s: this node's kubelet does not trust %s, which signs apiserver-kubelet-client here; run --phase=%s first",
				KubeletPhaseDropRoot, path(KubeletClientCAName, "crt"), KubeletPhaseTrustBoth)
		}
	}
	return writeKubeletBundle(t.Anchors)
}

// Puts the root back beside the kubelet-client CAs; a node that never migrated is left alone.
func KubeletClientReAddRoot() error {
	t, err := ReadKubeletClientTrust()
	if err != nil {
		return err
	}
	if len(t.Anchors) == 0 {
		return nil
	}
	roots, err := bundleCerts(bundleCrtPath("ca"))
	if err != nil {
		return err
	}
	return writeKubeletBundle(append(slices.Clone(t.Anchors), roots...))
}

// Reissues apiserver-kubelet-client under the root; every kubelet must trust it again first.
func KubeletClientReissueRoot(host, nodeIP string, sans *APISANs) error {
	t, err := ReadKubeletClientTrust()
	if err != nil {
		return err
	}
	if len(t.Roots) == 0 {
		return fmt.Errorf("pki: refusing %s: this node's kubelet does not trust the cluster root; run --phase=%s first", KubeletPhaseReissueRoot, KubeletPhaseReAddRoot)
	}
	ca, err := loadCA("ca")
	if err != nil {
		return fmt.Errorf("pki: load the cluster root: %w", err)
	}
	return ca.Reissue(kubeletClientLeafSpec(host, nodeIP, sans))
}

// Trusts ca-bundle.crt again and removes KubeletClientCAName; the leaf must be root-signed.
func KubeletClientRemoveCA() error {
	t, err := ReadKubeletClientTrust()
	if err != nil {
		return err
	}
	if t.LeafOnCA {
		return fmt.Errorf("pki: refusing %s: apiserver-kubelet-client here is still signed by %s; run --phase=%s first", KubeletPhaseRemoveCA, KubeletClientCAName, KubeletPhaseReissueRoot)
	}
	if len(t.Anchors) > 0 && len(t.Roots) == 0 {
		return fmt.Errorf("pki: refusing %s: this node's kubelet does not trust the cluster root yet; run --phase=%s first", KubeletPhaseRemoveCA, KubeletPhaseReAddRoot)
	}
	b, err := os.ReadFile(bundleCrtPath("ca"))
	if err == nil {
		err = host.WriteFileAtomically(KubeletClientBundlePath(), b, 0o644)
	}
	if err != nil {
		return fmt.Errorf("pki: write %s from %s: %w", KubeletClientBundlePath(), bundleCrtPath("ca"), err)
	}
	for _, ext := range []string{"key", "crt"} {
		if err := os.Remove(path(KubeletClientCAName, ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("pki: remove %s: %w", path(KubeletClientCAName, ext), err)
		}
	}
	return nil
}

// Trusts ca beside what this kubelet trusts now; ca must be a kubelet-client CA.
func AddKubeletClientAnchor(ca *x509.Certificate) error {
	if !isKubeletClientCA(ca) {
		return fmt.Errorf("pki: %s is not a kubelet-client CA", ca.Subject)
	}
	have, err := bundleCerts(KubeletClientBundlePath())
	if err != nil {
		return err
	}
	if slices.ContainsFunc(have, ca.Equal) {
		return nil
	}
	return writeKubeletBundle(append([]*x509.Certificate{ca}, have...))
}

func kubeletClientLeafSpec(host, nodeIP string, sans *APISANs) CertSpec {
	for _, s := range LeafSpecs(host, nodeIP, sans) {
		if s.Name == "apiserver-kubelet-client" {
			return s
		}
	}
	panic("pki: no apiserver-kubelet-client leaf spec")
}

func writeKubeletBundle(certs []*x509.Certificate) error {
	var b bytes.Buffer
	for _, c := range certs {
		if err := pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}); err != nil {
			return err
		}
	}
	if err := host.WriteFileAtomically(KubeletClientBundlePath(), b.Bytes(), 0o644); err != nil {
		return fmt.Errorf("pki: write %s: %w", KubeletClientBundlePath(), err)
	}
	return nil
}

// Loads the {name}.crt/.key pair from this node's PKI directory.
func LeafKeyPair(name string) (tls.Certificate, error) {
	return tls.LoadX509KeyPair(path(name, "crt"), path(name, "key"))
}

// GET /healthz at addr as client, verifying its serving cert against roots; 401 means untrusted.
func ProbeKubeletClientAuth(ctx context.Context, addr string, roots []*x509.Certificate, client tls.Certificate) (int, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	pool := x509.NewCertPool()
	for _, ca := range roots {
		pool.AddCert(ca)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{client},
	}}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/healthz", nil)
	if err != nil {
		return 0, err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("probe the kubelet at %s: %w", addr, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

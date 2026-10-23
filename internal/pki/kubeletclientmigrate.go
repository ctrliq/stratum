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

// KubeletMigrateCommand migrates an existing cluster to KubeletClientCAName.
const KubeletMigrateCommand = "stratum certs migrate-kubelet-client-ca"

// Kubelet client-CA migration phases. The first three move a cluster onto
// KubeletClientCAName; the last three take it back to the cluster root.
const (
	KubeletPhaseTrustBoth   = "trust-both"
	KubeletPhaseIssueNew    = "issue-new"
	KubeletPhaseDropRoot    = "drop-root"
	KubeletPhaseReAddRoot   = "re-add-root"
	KubeletPhaseReissueRoot = "reissue-root"
	KubeletPhaseRemoveCA    = "remove-ca"
)

// Kubelet client-trust states reported by KubeletClientTrust.State.
const (
	KubeletStateNotMigrated = "not-migrated"
	KubeletStateTrustBoth   = "trust-both"
	KubeletStateIssueNew    = "issue-new"
	KubeletStateMigrated    = "migrated"
)

// KubeletClientTrust is this node's kubelet client trust.
type KubeletClientTrust struct {
	// Anchors are the certificates in kubelet-client-ca-bundle.crt that
	// ca-bundle.crt does not hold and whose subject is a kubelet-client CA's.
	Anchors []*x509.Certificate
	// Roots are the certificates it shares with ca-bundle.crt.
	Roots []*x509.Certificate
	// Other are the certificates that are neither.
	Other []*x509.Certificate
	// HasCA reports kubelet-client-ca.crt on disk; only a control plane holds it.
	HasCA bool
	// LeafOnCA reports apiserver-kubelet-client signed by kubelet-client-ca.crt.
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

// KubeletClientMigratingPEM reports whether kubeletBundle trusts both a
// kubelet-client CA and a CA from the root trust already read from rootSource.
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

// State names where this node is in the migration.
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

// Migrating reports a node whose kubelet trusts both a kubelet-client CA and
// the cluster root.
func (t KubeletClientTrust) Migrating() bool {
	s := t.State()
	return s == KubeletStateTrustBoth || s == KubeletStateIssueNew
}

// KubeletClientMigrating reports whether this node is part way through the
// migration, which a CA rotation must not interleave with.
func KubeletClientMigrating() (bool, error) {
	t, err := ReadKubeletClientTrust()
	if err != nil {
		return false, err
	}
	return t.Migrating(), nil
}

// KubeletClientCAInUse reports whether this node's kubelet trusts a
// kubelet-client CA, on a split or a migrating node.
func KubeletClientCAInUse() (bool, error) {
	split, err := KubeletClientSplit()
	if err != nil || split {
		return split, err
	}
	return KubeletClientMigrating()
}

// refuseWhileKubeletMigrating refuses what while this node is part way
// through the migration. Without KubeletClientCAName an unreadable bundle is
// left for the caller to repair, as KubeletClientSplit leaves it.
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

// KubeletClientTrustBoth makes this control plane's kubelet trust
// KubeletClientCAName beside the cluster root. Only the node that starts the
// migration may mint the CA; the others must have fetched it.
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

// KubeletClientIssueNew reissues apiserver-kubelet-client under
// KubeletClientCAName. Every kubelet in the cluster must already trust it.
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

// KubeletClientDropRoot makes this node's kubelet trust the kubelet-client CAs
// it holds and nothing else. On a control plane, apiserver-kubelet-client must
// already be signed by KubeletClientCAName.
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

// KubeletClientReAddRoot puts the cluster root back beside the kubelet-client
// CAs this node's kubelet trusts. A node that never migrated is left alone.
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

// KubeletClientReissueRoot reissues apiserver-kubelet-client under the cluster
// root. Every kubelet in the cluster must trust the root again first.
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

// KubeletClientRemoveCA makes this node's kubelet trust ca-bundle.crt again
// and removes KubeletClientCAName. On a control plane, apiserver-kubelet-client
// must already be signed by the root.
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

// AddKubeletClientAnchor makes this node's kubelet trust ca beside what it
// trusts now. ca must be a kubelet-client CA.
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

// LeafKeyPair loads the {name}.crt/.key pair from this node's PKI directory.
func LeafKeyPair(name string) (tls.Certificate, error) {
	return tls.LoadX509KeyPair(path(name, "crt"), path(name, "key"))
}

// ProbeKubeletClientAuth sends GET /healthz to the kubelet at addr as client,
// verifying its serving certificate against roots, and returns the status. A
// kubelet answers 401 to a client certificate it does not trust.
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

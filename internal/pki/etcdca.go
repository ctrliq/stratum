// Package pki stages the etcd CA split; only drop-root closes the hole, mid-way stays exploitable.
package pki

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ctrliq/stratum/internal/version"
)

// Names the file etcd's --trusted-ca-file and the apiserver's --etcd-cafile point at.
const (
	EtcdCAName       = "etcd-ca"
	etcdCACommonName = "stratum-etcd-ca"
	EtcdBundleBase   = "etcd-ca"
)

// Refusal messages name this command because its reader is an operator at a shell.
const EtcdMigrateCommand = "stratum certs migrate-etcd-ca"

const (
	EtcdPhaseTrustBoth = "trust-both"
	EtcdPhaseIssueNew  = "issue-new"
	EtcdPhaseDropRoot  = "drop-root"
	// Not a forward phase; the recovery path for a failed issue-new.
	EtcdPhaseRollback = "rollback"
)

const (
	EtcdStateNotMigrated = "not-migrated"
	EtcdStateTrustBoth   = "trust-both"
	EtcdStateIssueNew    = "issue-new"
	EtcdStateMigrated    = "migrated"
	// Never a resting state; no guard may treat it as safe.
	EtcdStateUnknown = "unknown"
)

// A missing leaf is (false, os.ErrNotExist), so callers can tell no-leaf from not-signed.
func leafChainsTo(leaf, caName string) (bool, error) {
	lc, err := readCert(leaf)
	if err != nil {
		return false, err
	}
	ca, err := readCert(caName)
	if err != nil {
		return false, err
	}
	return lc.CheckSignatureFrom(ca) == nil, nil
}

func readCert(name string) (*x509.Certificate, error) {
	return readCertFile(name, path(name, "crt"))
}

func readCertFile(name, file string) (*x509.Certificate, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("pki: bad PEM for %s", name)
	}
	return x509.ParseCertificate(blk.Bytes)
}

// File-derived: describes intent, not the process; for closure use EtcdRejectsRootSignedClient.
type EtcdTrust struct {
	HasEtcdCA      bool
	BundleExists   bool
	Anchors        int
	HasEtcdAnchor  bool
	TrustsLiveRoot bool
	StaleAnchors   int
	LeavesMigrated bool
	// Set when a trust artifact could not be read or parsed; other fields are then meaningless.
	Err error
}

// Inspects this node's PKI directory; a read or parse failure sets Err, not a plausible state.
func ReadEtcdTrust() EtcdTrust { return readEtcdTrust(path("ca", "crt")) }

// ReadEtcdTrust with the cluster root read from rootFile, e.g. the root a rollback would restore.
func readEtcdTrust(rootFile string) EtcdTrust {
	t := EtcdTrust{HasEtcdCA: exists(EtcdCAName), BundleExists: bundleExists(EtcdBundleBase)}
	if t.HasEtcdCA {
		migrated, err := etcdLeavesUnder(EtcdCAName)
		if err != nil {
			return EtcdTrust{Err: err}
		}
		t.LeavesMigrated = migrated
	}
	if !t.BundleExists {
		return t
	}
	// An unreadable anchor is not an absent one; without it every bundle cert classifies as stale.
	etcdCA, err := readCertOptional(EtcdCAName)
	if err != nil {
		return EtcdTrust{Err: err}
	}
	liveRoot, err := readCertFile("ca", rootFile)
	if errors.Is(err, os.ErrNotExist) {
		liveRoot, err = nil, nil
	}
	if err != nil {
		return EtcdTrust{Err: err}
	}
	certs, err := bundleCerts(bundleCrtPath(EtcdBundleBase))
	if err != nil {
		return EtcdTrust{Err: err}
	}
	for _, c := range certs {
		t.Anchors++
		switch {
		case etcdCA != nil && c.Equal(etcdCA):
			t.HasEtcdAnchor = true
		case liveRoot != nil && c.Equal(liveRoot):
			t.TrustsLiveRoot = true
		default:
			// A root a rotation has since replaced; still a valid client-auth anchor.
			t.StaleAnchors++
		}
	}
	return t
}

// Returns (nil, nil) only when the file is genuinely absent.
func readCertOptional(name string) (*x509.Certificate, error) {
	c, err := readCert(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return c, err
}

func (t EtcdTrust) State() string {
	if t.Err != nil {
		return EtcdStateUnknown
	}
	switch {
	case !t.HasEtcdCA || !t.BundleExists || !t.HasEtcdAnchor:
		return EtcdStateNotMigrated
	case t.TrustsRoot() && !t.LeavesMigrated:
		return EtcdStateTrustBoth
	case t.TrustsRoot() && t.LeavesMigrated:
		return EtcdStateIssueNew
	case !t.TrustsRoot() && t.LeavesMigrated:
		// Not "Anchors == 1": mid root-rotation an unmigrated cluster legitimately holds three certs.
		return EtcdStateMigrated
	default:
		// A truncated bundle, or an etcd anchor with root-signed leaves.
		return EtcdStateNotMigrated
	}
}

// Deliberately not a count: a stale root in the bundle grants the same access as the live one.
func (t EtcdTrust) TrustsRoot() bool { return t.TrustsLiveRoot || t.StaleAnchors > 0 }

// Broader policy than kubelet's: the bundle must not look narrower than what etcd actually loads.
func bundleCerts(p string) ([]*x509.Certificate, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("pki: read trust bundle %s: %w", p, err)
	}
	return parseBundle(b, p)
}

func parseBundle(b []byte, p string) ([]*x509.Certificate, error) {
	certs, err := DecodePEMBundle(b, EtcdPolicy)
	if err != nil {
		return nil, fmt.Errorf("pki: trust bundle %s: %w", p, err)
	}
	return certs, nil
}

// Missing leaf is false; other errors propagate, else a corrupt leaf could read as unmigrated.
func etcdLeavesUnder(caName string) (bool, error) {
	for name := range etcdLeafNames {
		ok, err := leafChainsTo(name, caName)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// A missing or unreadable file is a mismatch.
func etcdLeafPairsMatch() bool {
	for name := range etcdLeafNames {
		if !keyMatches(path(name, "crt"), path(name, "key")) {
			return false
		}
	}
	return true
}

// False on any read failure; callers that must distinguish use ReadEtcdTrust().Err.
func EtcdLeavesMigrated() bool { t := ReadEtcdTrust(); return t.Err == nil && t.LeavesMigrated }

// Root is the safe signer while any root is an anchor; a caller avoiding a guess checks Err first.
func (t EtcdTrust) EtcdSignerName() string {
	if t.Err != nil || !t.HasEtcdAnchor || t.TrustsRoot() {
		return "ca"
	}
	return EtcdCAName
}

// Widens to etcd-ca‖root on existing clusters so re-rendering never locks out root-signed leaves.
func EnsureEtcdTrustBundle() error {
	if bundleExists(EtcdBundleBase) {
		return nil
	}
	if _, err := EnsureCA(EtcdCAName, etcdCACommonName); err != nil {
		return err
	}
	if exists("etcd") && !EtcdLeavesMigrated() && exists("ca") {
		return WriteCABundle(EtcdBundleBase, EtcdCAName, "ca")
	}
	return WriteCABundle(EtcdBundleBase, EtcdCAName)
}

// The caller must restart etcd after every phase: its CA pools are built once and never reload.
func MigrateEtcdTrustBoth(allowMint bool) error {
	if err := EnsureDir(); err != nil {
		return err
	}
	if !exists("ca") {
		return fmt.Errorf("pki: cannot migrate — cluster root CA not present at %s", version.PKIDir)
	}
	// Replayed past drop-root, this re-opens the etcd trust hole while reporting success.
	t := ReadEtcdTrust()
	if t.Err != nil {
		return fmt.Errorf("pki: refusing %s — cannot determine this node's etcd trust state: %w. "+
			"Proceeding would risk re-adding the cluster root to a bundle that has already dropped it, which re-opens the etcd trust hole on a node that is already fixed",
			EtcdPhaseTrustBoth, t.Err)
	}
	// State(), not a shape test: fetching a narrow bundle makes a non-migrated node read migrated.
	if t.State() == EtcdStateMigrated {
		return fmt.Errorf("pki: refusing %s — this node has already completed %s and etcd no longer trusts any cluster root. "+
			"Re-adding it would re-open the etcd trust hole on a node that is already fixed. "+
			"To continue a halted cluster migration run `%s --cluster --resume --yes`, which skips the phases a node is already past; "+
			"to deliberately un-migrate, `%s --cluster --rollback --force --yes`",
			EtcdPhaseTrustBoth, EtcdPhaseDropRoot, EtcdMigrateCommand, EtcdMigrateCommand)
	}
	// A follower minting its own CA looks healthy until issue-new breaks peer TLS; fail here instead.
	if !allowMint && !exists(EtcdCAName) {
		return fmt.Errorf("pki: refusing to mint an etcd CA on a follower — this node did not receive the cluster's %s.crt/.key. "+
			"Every control plane must share one anchor; minting here would break peer TLS at %s",
			EtcdCAName, EtcdPhaseIssueNew)
	}
	if _, err := EnsureCA(EtcdCAName, etcdCACommonName); err != nil {
		return fmt.Errorf("stage etcd CA: %w", err)
	}
	return WriteCABundle(EtcdBundleBase, EtcdCAName, "ca")
}

// Reissues etcd leaves under the etcd CA; safe only once every control plane completed trust-both.
func MigrateEtcdIssueNew(host, nodeIP string, sans *APISANs) error {
	if !exists(EtcdCAName) {
		return fmt.Errorf("pki: %s requires the etcd CA — run --phase=%s first", EtcdPhaseIssueNew, EtcdPhaseTrustBoth)
	}
	n, err := CABundleCertCount(EtcdBundleBase)
	if err != nil {
		return fmt.Errorf("pki: read %s bundle: %w", EtcdBundleBase, err)
	}
	if n < 2 && !EtcdLeavesMigrated() {
		return fmt.Errorf("pki: %s requires the etcd trust bundle to carry both CAs (found %d) — run --phase=%s first",
			EtcdPhaseIssueNew, n, EtcdPhaseTrustBoth)
	}
	etcdCA, err := loadCA(EtcdCAName)
	if err != nil {
		return fmt.Errorf("load etcd CA: %w", err)
	}
	_, etcdLeaves := SplitLeafSpecs(host, nodeIP, sans)
	for _, l := range etcdLeaves {
		if err := etcdCA.Reissue(l); err != nil {
			return fmt.Errorf("reissue %s: %w", l.Name, err)
		}
	}
	return nil
}

// Closes the trust hole; local gate only (the --cluster run surveys every member); restart etcd.
func MigrateEtcdDropRoot() error {
	if !exists(EtcdCAName) {
		return fmt.Errorf("pki: %s requires the etcd CA — run --phase=%s first", EtcdPhaseDropRoot, EtcdPhaseTrustBoth)
	}
	if dt := ReadEtcdTrust(); dt.Err != nil {
		return fmt.Errorf("pki: refusing %s — cannot determine this node's etcd trust state: %w", EtcdPhaseDropRoot, dt.Err)
	}
	if !EtcdLeavesMigrated() {
		return fmt.Errorf("pki: refusing %s — this node's etcd leaves are not signed by %s yet (run --phase=%s first); dropping the root now would lock the apiserver out of its own etcd",
			EtcdPhaseDropRoot, EtcdCAName, EtcdPhaseIssueNew)
	}
	return WriteCABundle(EtcdBundleBase, EtcdCAName)
}

// Recovery by re-derivation, not backup: no leaf is backed up anywhere; refuses on migrated nodes.
func MigrateEtcdRollback(host, nodeIP string, sans *APISANs) error {
	// The only path back to a root-trusting etcd bundle, so an ungated one re-opens the trust hole.
	t := ReadEtcdTrust()
	if t.Err != nil {
		return fmt.Errorf("pki: refusing %s — cannot determine this node's etcd trust state: %w. "+
			"Proceeding would risk re-deriving the etcd leaves under the cluster root on a node that has already completed %s, which re-opens the etcd trust hole and costs the node quorum",
			EtcdPhaseRollback, t.Err, EtcdPhaseDropRoot)
	}
	if t.State() == EtcdStateMigrated {
		return fmt.Errorf("pki: refusing %s — this node has already completed %s and etcd no longer trusts any cluster root. "+
			"Rolling back would re-open the etcd trust hole on a node that is already fixed, and would put its etcd leaves back on the cluster root while its peers enforce %s-only peer trust, costing it quorum. "+
			"This phase undoes a FAILED %s. To deliberately un-migrate, run `%s --cluster --rollback --force --yes`, which does it as a ROLLING operation across every control plane",
			EtcdPhaseRollback, EtcdPhaseDropRoot, EtcdCAName, EtcdPhaseIssueNew, EtcdMigrateCommand)
	}
	return migrateEtcdRollback(host, nodeIP, sans)
}

// Re-opens the hole on this node by design; only rolling across every control plane keeps quorum.
func MigrateEtcdRollbackFromMigrated(host, nodeIP string, sans *APISANs) error {
	return migrateEtcdRollback(host, nodeIP, sans)
}

// Widening must land on every node before any leaf re-derives, or that node loses quorum.
func MigrateEtcdReAddRoot() error {
	if !exists("ca") {
		return fmt.Errorf("pki: cannot re-add the cluster root — it is not present at %s", version.PKIDir)
	}
	if !exists(EtcdCAName) {
		return fmt.Errorf("pki: nothing to un-migrate — no etcd CA at %s", version.PKIDir)
	}
	t := ReadEtcdTrust()
	if t.Err != nil {
		return fmt.Errorf("pki: refusing to re-add the cluster root — cannot determine this node's etcd trust state: %w", t.Err)
	}
	return WriteCABundle(EtcdBundleBase, EtcdCAName, "ca")
}

// Unexported so reaching it always means passing one of the two rollback guards.
func migrateEtcdRollback(host, nodeIP string, sans *APISANs) error {
	if !exists("ca") {
		return fmt.Errorf("pki: cannot roll back — cluster root CA not present at %s", version.PKIDir)
	}
	if !exists(EtcdCAName) {
		return fmt.Errorf("pki: nothing to roll back — no etcd CA at %s", version.PKIDir)
	}
	ca, err := loadCA("ca")
	if err != nil {
		return fmt.Errorf("load root CA: %w", err)
	}
	// Widen first: etcd must trust the root before receiving a root-signed leaf.
	if err := WriteCABundle(EtcdBundleBase, EtcdCAName, "ca"); err != nil {
		return fmt.Errorf("widen etcd trust before rollback: %w", err)
	}
	_, etcdLeaves := SplitLeafSpecs(host, nodeIP, sans)
	for _, l := range etcdLeaves {
		if err := ca.Reissue(l); err != nil {
			return fmt.Errorf("re-derive %s under the cluster root: %w", l.Name, err)
		}
	}
	return nil
}

func EtcdMigrationState() string { return ReadEtcdTrust().State() }

// Derived at every ca-bundle writer so a root rotation stays safe mid-migration.
func syncEtcdBundleToRoot(caCertNames ...string) error {
	t := ReadEtcdTrust()
	if t.Err != nil {
		return fmt.Errorf("cannot determine etcd's trust state, so the rotation cannot safely re-derive its bundle: %w "+
			"(leaving it would keep a root trusted by etcd that Kubernetes has stopped trusting)", t.Err)
	}
	if !t.HasEtcdAnchor || !t.TrustsRoot() {
		return nil // no etcd bundle yet, or a migrated cluster: not ours to touch
	}
	return WriteCABundle(EtcdBundleBase, append([]string{EtcdCAName}, caCertNames...)...)
}

// Applied means strictly past: the on-disk file is not the running process.
func EtcdPhaseApplied(phase string) (bool, error) {
	t := ReadEtcdTrust()
	if t.Err != nil {
		return false, t.Err
	}
	switch phase {
	case EtcdPhaseTrustBoth:
		// Past it once the leaves have moved, which only issue-new does.
		return t.HasEtcdAnchor && t.LeavesMigrated, nil
	case EtcdPhaseIssueNew:
		// Past it once the root is gone, and only while each leaf still pairs with its key.
		return t.HasEtcdAnchor && t.LeavesMigrated && !t.TrustsRoot() && etcdLeafPairsMatch(), nil
	case EtcdPhaseDropRoot:
		// Nothing is past drop-root, so it is never skipped; re-running re-verifies.
		return false, nil
	}
	return false, fmt.Errorf("pki: unknown phase %q", phase)
}

// The trust file the etcd unit and the apiserver point at.
func EtcdBundlePath() string { return bundleCrtPath(EtcdBundleBase) }

// Never unions the Kubernetes root in: a root-signed ServerAuth leaf could then impersonate etcd.
func EtcdTrustFile() (string, error) {
	for _, p := range []string{
		bundleCrtPath(EtcdBundleBase),
		bundleCrtPath("ca"),
		filepath.Join(version.PKIDir, "ca.crt"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("pki: no etcd trust material in %s (looked for %s-bundle.crt, ca-bundle.crt, ca.crt)",
		version.PKIDir, EtcdBundleBase)
}

// EtcdTrustPEM returns the PEM of EtcdTrustFile.
func EtcdTrustPEM() ([]byte, error) {
	p, err := EtcdTrustFile()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read etcd trust material %s: %w", p, err)
	}
	return b, nil
}

// etcd CA migration — splitting etcd out of the Kubernetes trust domain.
//
// Without it, etcd's entire client authorization decision is "signed by the
// cluster root CA" — the same root that signs every kubelet client certificate,
// so root on any worker is unrestricted read/write of the whole keyspace (the
// etcd trust hole). The migration issues etcd's leaves from a dedicated "etcd-ca"
// and drops the cluster root from etcd's trust.
//
// Dropping a trust anchor under a live raft cannot be done in one step, so the
// migration is staged like the existing CA rotation. Each phase must complete on
// EVERY control plane before any node starts the next — peer TLS is a
// cluster-wide agreement, and a member presenting a leaf its peers do not yet
// trust loses quorum:
//
//	trust-both  etcd trusts etcd-ca ‖ root; every leaf still root-signed
//	issue-new   etcd + apiserver-etcd-client reissued under etcd-ca
//	drop-root   etcd trusts etcd-ca only              <- only this closes the hole
//
// Only drop-root closes the vulnerability. trust-both and issue-new are security
// no-ops that exist to make drop-root survivable, and a cluster parked in either
// looks completely healthy while remaining fully exploitable — see
// EtcdMigrationState and the process-derived probe in etcdprobe.go.
//
// Order of operations, and which side moves first: etcd's TRUST widens before any
// client's LEAF narrows, and etcd's trust narrows only after every client's leaf
// has moved. So etcd restarts first on the way in (trust-both) and last on the
// way out (drop-root). At no point does the apiserver hold a leaf its etcd will
// reject.
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

// EtcdCAName is the dedicated etcd trust anchor; EtcdBundleBase names the file
// etcd's --trusted-ca-file / --peer-trusted-ca-file and the apiserver's
// --etcd-cafile point at ({base}-bundle.crt).
const (
	EtcdCAName = "etcd-ca"
	// etcdCACommonName is the Subject CN minted for it.
	etcdCACommonName = "stratum-etcd-ca"
	EtcdBundleBase   = "etcd-ca"
)

// EtcdMigrateCommand is the operator-facing command that drives these phases.
// Every refusal below that tells an operator there is a way through names THIS
// constant rather than an exported Go function: the reader of that message is
// mid-incident at a shell, not in a debugger.
const EtcdMigrateCommand = "stratum certs migrate-etcd-ca"

// Migration phase names, in order.
const (
	EtcdPhaseTrustBoth = "trust-both"
	EtcdPhaseIssueNew  = "issue-new"
	EtcdPhaseDropRoot  = "drop-root"
	// EtcdPhaseRollback re-derives the etcd leaves under the cluster root. Not a
	// forward phase: the recovery path for a failed issue-new.
	EtcdPhaseRollback = "rollback"
)

// Migration states reported by EtcdMigrationState.
const (
	EtcdStateNotMigrated = "not-migrated"
	EtcdStateTrustBoth   = "trust-both"
	EtcdStateIssueNew    = "issue-new"
	EtcdStateMigrated    = "migrated"
	// EtcdStateUnknown means a trust artifact could not be read or parsed. It is
	// never a resting state and no guard may treat it as safe.
	EtcdStateUnknown = "unknown"
)

// leafChainsTo reports whether {leaf}.crt on disk was signed by {caName}.crt.
// A missing leaf is (false, os.ErrNotExist) so callers can tell "not signed by
// this CA" apart from "no leaf to judge".
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

// EtcdTrust is the single answer to "what etcd trust state is this cluster in".
//
// Three readers need that answer: the cluster orchestrator (does the anchor it
// needs already exist elsewhere?), HA join (which CA will the live members
// accept?), and root rotation (does it own an anchor in etcd's bundle?).
// Anything that touches etcd trust reads this, so those answers cannot drift
// apart.
//
// It is derived from the trust BUNDLE rather than from a phase counter, because
// the bundle is what etcd actually enforces and it is shared material — a joining
// node reads the cluster's bundle and gets the same answer every member has.
//
// It is still FILE-derived and therefore describes intent, not the running
// process: etcd never reloads its CA pool. Anything asserting the vulnerability
// is CLOSED must use EtcdRejectsRootSignedClient instead.
type EtcdTrust struct {
	HasEtcdCA      bool // etcd-ca.crt + .key are on disk (this node can sign)
	BundleExists   bool
	Anchors        int  // CERTIFICATE blocks in the trust bundle
	HasEtcdAnchor  bool // the etcd CA is one of them
	TrustsLiveRoot bool // the CURRENT ca.crt is one of them
	StaleAnchors   int  // anchors that are neither the etcd CA nor the live root
	LeavesMigrated bool // this node's etcd leaves chain to the etcd CA
	// Err is set when a trust artifact could not be read or parsed. When it is
	// set every other field is meaningless and State() reports "unknown".
	//
	// This exists because the alternative — swallowing the error and returning
	// zero values — makes an unreadable bundle indistinguishable from a genuinely
	// unmigrated one, and every guard in this package keys off that distinction.
	// A corrupt bundle on a migrated node would read as "not migrated", and
	// trust-both would then re-add the cluster root it had just dropped.
	Err error
}

// ReadEtcdTrust inspects this node's PKI directory. A read or parse failure sets
// Err rather than degrading to a plausible-looking state.
func ReadEtcdTrust() EtcdTrust { return readEtcdTrust(path("ca", "crt")) }

// readEtcdTrust is ReadEtcdTrust with the cluster root read from rootFile, so a
// rollback can ask about the root it would restore rather than the live one.
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
	// A present-but-unreadable anchor is not the same as an absent one: without
	// it every cert in the bundle classifies as "stale", which inverts the
	// answer. Only os.ErrNotExist is a legitimate nil here.
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
			// An anchor that is neither. In practice this is a root that
			// `rotate-ca` has since replaced — still a valid etcd client-auth
			// anchor, and invisible to any check that only counts.
			t.StaleAnchors++
		}
	}
	return t
}

// readCertOptional returns (nil, nil) only when the file is genuinely absent.
func readCertOptional(name string) (*x509.Certificate, error) {
	c, err := readCert(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return c, err
}

// State names the migration phase this node is in.
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
		// Deliberately NOT "Anchors == 1": mid root-rotation an unmigrated cluster
		// legitimately holds three certs, so a raw count falls off the end of the
		// mapping. What closes the etcd trust hole is that no
		// cluster root remains an anchor.
		return EtcdStateMigrated
	default:
		// A truncated bundle (etcd trusts nothing), or an etcd anchor with
		// root-signed leaves — a node that never migrated.
		return EtcdStateNotMigrated
	}
}

// TrustsRoot reports whether ANY Kubernetes root — the live one or one a rotation
// left behind — is still a valid etcd client-auth anchor. This is the question
// the etcd trust hole is about, and it is deliberately not "Anchors > 1": a
// stale root left in the bundle grants exactly the same access as the live one.
func (t EtcdTrust) TrustsRoot() bool { return t.TrustsLiveRoot || t.StaleAnchors > 0 }

// bundleCerts parses the trust bundle at p under EtcdPolicy, the declared etcd
// trust policy. It is deliberately broader than the kubelet's: etcd loads this
// file through x509.CertPool.AppendCertsFromPEM, which skips non-certificate
// blocks and never checks IsCA, so a caller deciding trust must see the
// anchors etcd itself would load plus the fact that something else is there —
// refusing one would make the bundle look narrower than it is.
func bundleCerts(p string) ([]*x509.Certificate, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("pki: read trust bundle %s: %w", p, err)
	}
	return parseBundle(b, p)
}

// parseBundle is bundleCerts on bytes already read. Etcd accepts the broader
// policy; kubelet verbatim trust parsing retains its stricter policy.
func parseBundle(b []byte, p string) ([]*x509.Certificate, error) {
	certs, err := DecodePEMBundle(b, EtcdPolicy)
	if err != nil {
		return nil, fmt.Errorf("pki: trust bundle %s: %w", p, err)
	}
	return certs, nil
}

// etcdLeavesUnder reports whether every etcd leaf chains to caName. A missing
// leaf is "no"; anything else is an error, because a corrupt leaf read as "not
// under the etcd CA" is what lets trust-both re-add a dropped root.
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

// etcdLeafPairsMatch reports whether every etcd leaf's key belongs to its
// certificate. A missing or unreadable file is a mismatch.
func etcdLeafPairsMatch() bool {
	for name := range etcdLeafNames {
		if !keyMatches(path(name, "crt"), path(name, "key")) {
			return false
		}
	}
	return true
}

// EtcdLeavesMigrated reports whether this node's etcd leaves are issued by the
// etcd CA. False on any read failure — callers that must distinguish use
// ReadEtcdTrust().Err.
func EtcdLeavesMigrated() bool { t := ReadEtcdTrust(); return t.Err == nil && t.LeavesMigrated }

// EtcdSignerName returns the CA a NEW etcd leaf must be signed by so that every
// live member accepts it, given the cluster's current trust bundle.
//
// This is the join-time and reissue-time question. While any root is still an
// anchor the cluster is mid-migration and every existing member's leaf is
// root-signed, so a new member must match them — minting under the etcd CA there
// is a unilateral issue-new, and its peers reject it. Only once the bundle
// carries the etcd CA alone is etcd-ca the right signer.
// A caller that cannot tolerate a guess must check Err first; the conservative
// answer here is the cluster root, which every member accepts while any root is
// still an anchor.
func (t EtcdTrust) EtcdSignerName() string {
	if t.Err != nil || !t.HasEtcdAnchor || t.TrustsRoot() {
		return "ca"
	}
	return EtcdCAName
}

// EnsureEtcdTrustBundle writes etcd-ca-bundle.crt if it is absent, choosing the
// contents from what is already on disk:
//
//   - fresh install (no pre-existing root-signed etcd leaves) → etcd-ca only.
//     Fully split in one pass; no migration needed, the etcd trust hole never applies.
//   - existing cluster (etcd leaves still root-signed) → etcd-ca ‖ ca, i.e. phase
//     trust-both. This is a SAFETY NET, not the migration path: it exists so that
//     re-rendering the etcd unit on an already-installed node cannot point
//     --trusted-ca-file at an anchor that signed none of the live certificates.
//     Actually migrating a cluster drives the Migrate* primitives below across
//     every control plane; the command that does so is
//     `stratum certs migrate-etcd-ca --cluster`. New clusters are split from
//     birth and never need it.
//
// Idempotent and never narrows an existing bundle — collapsing to etcd-ca only is
// exclusively MigrateEtcdDropRoot's job, which is gated on cluster-wide health.
func EnsureEtcdTrustBundle() error {
	if bundleExists(EtcdBundleBase) {
		return nil
	}
	if _, err := EnsureCA(EtcdCAName, etcdCACommonName); err != nil {
		return err
	}
	// The root stays an anchor only when there are EXISTING etcd leaves this
	// bundle has to keep accepting. No etcd leaf at all means a fresh install (or
	// a worker, which has no root key either) — split cleanly, never vulnerable.
	if exists("etcd") && !EtcdLeavesMigrated() && exists("ca") {
		return WriteCABundle(EtcdBundleBase, EtcdCAName, "ca")
	}
	return WriteCABundle(EtcdBundleBase, EtcdCAName)
}

// MigrateEtcdTrustBoth (phase 1) makes etcd trust the etcd CA *in addition to*
// the cluster root. Every leaf stays root-signed and nothing is locked out.
//
// The caller MUST restart etcd afterward. etcd builds its client/peer CA pools
// once at listener construction (v3.5.16 transport/listener.go — ServerConfig
// sets ClientCAs once, with no GetConfigForClient callback) and never reloads
// them, so writing this file without a restart leaves the process trusting the
// root only, and phase issue-new then breaks peer TLS.
//
// These are per-node primitives and do not own that restart: the caller does.
// internal/certs (`stratum certs migrate-etcd-ca`) is that caller and restarts
// etcd after every phase; a test in that package holds the obligation in place.
// Anything else wiring these directly inherits it.
func MigrateEtcdTrustBoth(allowMint bool) error {
	if err := EnsureDir(); err != nil {
		return err
	}
	if !exists("ca") {
		return fmt.Errorf("pki: cannot migrate — cluster root CA not present at %s", version.PKIDir)
	}
	// REPLAY GUARD. trust-both re-adds the cluster root as an etcd client-auth
	// anchor. Replayed against a node that has already completed drop-root that
	// re-opens the etcd trust hole — reported as success, by the command named for
	// closing it, on a cluster the operator believes is fixed. A complete re-run would narrow
	// again at drop-root; the hazard is one that stops early.
	//
	// rotate-ca solves the same problem the same way, at the start (cacluster.go:
	// "a naive re-run would mint a DIVERGENT CA on nodes that already promoted.
	// Make the operator finish or roll back first").
	t := ReadEtcdTrust()
	if t.Err != nil {
		return fmt.Errorf("pki: refusing %s — cannot determine this node's etcd trust state: %w. "+
			"Proceeding would risk re-adding the cluster root to a bundle that has already dropped it, which re-opens the etcd trust hole on a node that is already fixed",
			EtcdPhaseTrustBoth, t.Err)
	}
	// State(), not a hand-rolled shape test. "has the etcd anchor and no root"
	// ALSO matches a node carrying an etcd-ca-only bundle whose leaves are still
	// root-signed — a node that has migrated NOTHING. A follower reaches exactly
	// that shape through `--phase=trust-both --fetch-shared` against a cluster
	// whose initiator has already finished: certs.migrateEtcdCAWith fetches first, and
	// ha.DownloadCertsAdmin writes the cluster's current (narrow) etcd-ca-bundle.crt
	// over this node's before MigrateEtcdTrustBoth is reached. Telling that node it
	// "already completed drop-root" strands it — it never migrates, and its own
	// etcd would reject its own apiserver on the next restart. State() already
	// classifies that shape as not-migrated (the default: branch), so ask it.
	if t.State() == EtcdStateMigrated {
		return fmt.Errorf("pki: refusing %s — this node has already completed %s and etcd no longer trusts any cluster root. "+
			"Re-adding it would re-open the etcd trust hole on a node that is already fixed. "+
			"To continue a halted cluster migration run `%s --cluster --resume --yes`, which skips the phases a node is already past; "+
			"to deliberately un-migrate, `%s --cluster --rollback --force --yes`",
			EtcdPhaseTrustBoth, EtcdPhaseDropRoot, EtcdMigrateCommand, EtcdMigrateCommand)
	}
	// allowMint is false on every node except the one that starts the migration.
	// The etcd CA is cluster-wide material: if a follower mints its own, phase
	// trust-both still looks healthy — the leaves are all root-signed, so nothing
	// is observably wrong — and the cluster only comes apart at issue-new, when
	// each member presents a leaf its peers cannot verify. Failing here instead
	// turns a silent quorum loss into a distribution error.
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

// MigrateEtcdIssueNew (phase 2) reissues the etcd + apiserver-etcd-client leaves
// under the etcd CA. Safe only once EVERY control plane has completed trust-both:
// the peer cert and the server cert are the same file, so this moves both roles
// at once and any member still trusting the root alone would reject this one.
//
// Refuses to run unless the local trust bundle already carries both anchors —
// otherwise this node would present a leaf its own etcd cannot verify.
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

// MigrateEtcdDropRoot (phase 3) collapses etcd's trust to the etcd CA alone. This
// is the step that actually closes the etcd trust hole — after it, a
// cluster-root-signed client certificate no longer authenticates to etcd.
//
// Refuses to run while this node's own etcd leaves are still root-signed, which
// would lock its apiserver out of its own etcd. This is the LOCAL gate only;
// cluster-wide readiness is `stratum certs migrate-etcd-ca --cluster`'s, which
// surveys every member's running etcd before dispatching this phase anywhere.
//
// As with trust-both the caller MUST restart etcd — the CA pool does not reload,
// so without it this writes a file that claims drop-root landed while the running
// process still accepts every kubelet certificate in the cluster.
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

// MigrateEtcdRollback undoes a failed or unwanted issue-new by RE-DERIVING the
// etcd leaves under the cluster root and widening the bundle back to trust-both.
//
// Recovery by re-derivation, not by backup — the same shape rotate-ca uses
// (`--rollback` detects sign-new and reissues the leaves under the restored
// anchor). No leaf is backed up anywhere in this codebase, so a backup-based
// rollback would be the deviation. Without this, once Reissue has overwritten
// etcd.crt under the etcd CA nothing in the tree puts it back on the root, and
// a failed issue-new would be unrecoverable.
//
// It refuses on a node that has already completed drop-root. Deliberately
// un-migrating a cluster is MigrateEtcdRollbackFromMigrated, which the CLI
// exposes as `--phase=rollback --force`.
func MigrateEtcdRollback(host, nodeIP string, sans *APISANs) error {
	// STATE GUARD, the counterpart to the trust-both replay guard. Every
	// other writer of etcd trust here is gated on STATE — trust-both on the replay
	// guard, drop-root on EtcdLeavesMigrated, syncEtcdBundleToRoot on TrustsRoot —
	// and rollback is the ONLY path in this package back to a root-trusting etcd
	// bundle, so an ungated one is the way back into the etcd trust hole.
	//
	// It exists to undo a FAILED issue-new. On a migrated node there is no failed
	// issue-new to undo, and running it there does two things at once: it re-opens
	// the etcd trust hole on a node the operator believes is fixed, and it puts that node's
	// etcd.crt back on the cluster root while its peers enforce etcd-CA-only peer
	// trust — so the node also falls out of raft. Presence checks (a root CA is
	// here, an etcd CA is here) pass on exactly that node.
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

// MigrateEtcdRollbackFromMigrated is MigrateEtcdRollback with the migrated-node
// guard waived: it re-adds the cluster root to etcd's trust and re-derives this
// node's etcd leaves under it even where drop-root has already run. It backs
// `stratum certs migrate-etcd-ca --rollback --force`.
//
// This RE-OPENS the etcd trust hole on this node — after it, any kubelet client
// certificate in the cluster authenticates to this etcd again. It exists because a cluster-wide
// un-migration is a legitimate, if unhappy, operation (a defect found in the etcd
// CA after the fact, an anchor that has to be replaced), and the alternative to a
// named opt-in is an operator concatenating bundles and re-signing leaves by hand
// under pressure.
//
// Only meaningful as a ROLLING operation across every control plane. Each node
// widens its own trust before its own leaf moves, so at every step a majority
// still trusts each other and quorum holds; a node left rolled back on its own is
// rejected by its un-rolled-back peers and drops out of raft. As with every phase
// here the caller MUST restart etcd — the CA pool is built once at listener
// construction and never reloads.
func MigrateEtcdRollbackFromMigrated(host, nodeIP string, sans *APISANs) error {
	return migrateEtcdRollback(host, nodeIP, sans)
}

// MigrateEtcdReAddRoot is the FIRST half of an un-migration: it re-adds the
// cluster root to etcd's trust bundle and touches no leaf.
//
// It exists because rolling a fully migrated cluster back one node at a time
// does not work, and the one-step form (widen + reissue together, which is what
// migrateEtcdRollback does) cannot be made to. On a migrated cluster every peer
// enforces the etcd CA alone, so the moment one node's leaf is re-derived under
// the root its peers reject it and it leaves raft — before any other node has
// been told to follow. A run that halts there leaves a split cluster, and on a
// two-member cluster it loses quorum outright.
//
// Splitting the operation restores the same property the forward phases have:
// trust widens EVERYWHERE before any leaf moves. After this has run on every
// control plane the cluster is at issue-new — etcd-ca ‖ root trusted, leaves
// still etcd-CA-signed — which every member verifies, so the caller can then
// re-derive leaves node by node with no member ever presenting a certificate
// its peers do not already accept.
//
// As with every phase here the caller MUST restart etcd: the CA pool is built
// once at listener construction and never reloads, so without it this widens a
// file while the process still enforces the narrow set.
//
// Idempotent, and it never narrows: re-running on a node already carrying both
// anchors rewrites the same bundle.
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

// migrateEtcdRollback is the unguarded body both entry points share: widen etcd's
// trust, then re-derive its leaves under the cluster root. Unexported so that
// reaching it always means passing one of the two guards above.
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
	// Widen FIRST. etcd must trust the root before it is handed a root-signed
	// leaf, and it must keep trusting the etcd CA because peers that already
	// completed issue-new are still presenting etcd-CA-signed certs.
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

// EtcdMigrationState reports this node's on-disk migration state. Thin wrapper
// over ReadEtcdTrust so every surface reports the same thing.
func EtcdMigrationState() string { return ReadEtcdTrust().State() }

// syncEtcdBundleToRoot re-derives etcd-ca-bundle.crt whenever the Kubernetes
// trust bundle is rewritten, so the etcd anchor set tracks the rotation instead
// of going stale.
//
// The rule: preserve the WIDTH, refresh the CONTENTS. If the etcd bundle
// currently carries a cluster root it becomes etcd-ca ‖ <the same roots
// ca-bundle.crt was just given>. If it carries only the etcd CA — a migrated
// cluster — it is left completely alone, which is the point of splitting the
// trust domain.
//
// This is why the etcd bundle must be DERIVED at every ca-bundle writer rather
// than patched at selected phase transitions: derivation is what makes a root
// rotation safe to interleave with an etcd migration. At sign-new, migrated
// nodes reissue from etcd-ca and unmigrated ones from the new root, and every
// one of those signers is in every node's bundle, so peer TLS holds across the
// mixed set.
//
// It is a no-op before the etcd CA exists at all; GenerateAll seeds the bundle.
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

// EtcdPhaseApplied reports whether a phase's effect is already present on this
// node, so a resumed run can skip it instead of tripping a guard written for a
// deliberate re-invocation.
//
// A failed node has per-node recovery; this is what gives the ORCHESTRATOR
// recovery. Its loop always restarts at trust-both, so after a follower failed at
// drop-root, re-running --cluster would run trust-both against an initiator that
// has already dropped the root — and the replay guard would (correctly)
// refuse, leaving no way to resume. Skipping an applied phase is not weakening
// the guard: the guard exists to stop a root being re-added, and a phase that is
// already applied adds nothing.
func EtcdPhaseApplied(phase string) (bool, error) {
	t := ReadEtcdTrust()
	if t.Err != nil {
		return false, t.Err
	}
	// A phase counts as applied only when the node is STRICTLY PAST it — i.e. a
	// LATER phase's effect is visible. "At" the phase is not enough.
	//
	// Every phase is a file write followed by a mandatory restart, and the
	// central premise is that the file is not the process. A node whose
	// trust-both wrote the bundle and then failed its restart looks "at
	// trust-both" on disk while the running etcd never adopted it — skipping on
	// that reading would let issue-new proceed and hand etcd leaves it cannot
	// verify, which is the exact failure the restart exists to prevent.
	//
	// Re-running a phase the node is merely AT is idempotent: it rewrites the
	// same file and restarts again. Cheap, and it re-establishes the process
	// state the skip could not confirm.
	switch phase {
	case EtcdPhaseTrustBoth:
		// Past it once the leaves have moved, which only issue-new does.
		return t.HasEtcdAnchor && t.LeavesMigrated, nil
	case EtcdPhaseIssueNew:
		// Past it once the root is gone, which only drop-root does -- and only
		// while each leaf it issued still pairs with its key. A pair that does
		// not leaves etcd unable to start, and issue-new is what rewrites it
		// under the etcd CA without etcd serving.
		return t.HasEtcdAnchor && t.LeavesMigrated && !t.TrustsRoot() && etcdLeafPairsMatch(), nil
	case EtcdPhaseDropRoot:
		// Nothing is past drop-root, so it is never skipped. It is idempotent and
		// it ends in the process-derived probe, so re-running it on an
		// already-migrated node re-verifies rather than re-does.
		return false, nil
	}
	return false, fmt.Errorf("pki: unknown phase %q", phase)
}

// EtcdBundlePath is the trust file the etcd unit and the apiserver point at.
func EtcdBundlePath() string { return bundleCrtPath(EtcdBundleBase) }

// EtcdTrustFile returns the file a CLIENT should verify an etcd SERVER against,
// preferring the etcd trust bundle. First readable file wins.
//
// It deliberately does NOT union the Kubernetes bundle in. etcd-ca-bundle.crt is
// already the cluster-wide anchor set in every migration state — etcd-ca‖root
// while any member is still root-signed, etcd-ca alone once drop-root has run
// cluster-wide — so it covers unmigrated peers on its own. Adding the Kubernetes
// root on top would keep it trusted for etcd server identity forever, which means
// any root-signed ServerAuth leaf carrying that node's IP SAN (its kubelet.crt,
// its apiserver.crt) could impersonate an etcd endpoint to MemberAdd/MemberList
// during HA join and snapshot rejoin.
//
// The fallbacks exist only for a node that predates the etcd CA.
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

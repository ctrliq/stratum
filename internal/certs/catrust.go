// Worker CA-trust refresh: the fetch → validate → stage → commit prefix of
// workerRefreshTrust, kept out of carotate.go so it can be exercised without a
// live supervisor. Nothing here mutates a live file until commit is called.
//
// The refresh rewrites ca-bundle.crt and, with the same bytes, the kubelet's
// clientCAFile (kubelet-client-ca-bundle.crt) — any CA in that file can present
// an O=system:masters client cert to the kubelet's :10250.
// Four layers guard that write, and only the first defends against an on-path
// attacker:
//
//	L1 transport   the fetch runs over TLS verified against the on-disk bundle
//	               (bootstrap.FetchClusterCAVerified). THIS is the anti-MITM control.
//	L4 structure   every block must be a parseable CA certificate.
//	L3 containment the transition must match the caller's intent (widen / narrow).
//	L2 self-check  the fetched bundle must validate the serving cert of the
//	               connection it arrived on — never adopt trust that would not
//	               have permitted the connection you just made.
//
// They are numbered by how much each assumes and applied L4 → L3 → L2, the order
// that yields the most actionable message when more than one would fire.
//
// L2-L4 bound what a LEGITIMATE BUT WRONG peer can do — a stale cluster-info
// republish, a half-orchestrated rotation, a control plane that published the
// wrong bundle. None is an anti-MITM control and none should ever be described as
// one: the cluster CA certificate is public data served to anonymous callers, so
// an attacker who defeats L1 simply appends their own CA to a bundle that also
// contains the genuine one and satisfies every containment predicate that can be
// written. L1 stops the attack; L2-L4 stop the accidents.

package certs

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ctrliq/stratum/internal/bootstrap"
	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// trustIntent is the direction a caller is allowed to move this node's trust
// store. It is passed explicitly rather than derived from CARotateOptions.Phase:
// `certs rotate-ca --rollback --phase=finalize` is accepted by the CLI (main.go
// evaluates the rollback case first and never rejects --phase), so a
// Phase-derived intent would apply finalize's narrow-only rule to a rollback that
// legitimately re-widens.
type trustIntent int

const (
	// trustWiden (phase trust-new) — the cluster is adding a CA. Trust may grow by
	// the one staged CA and must not shrink.
	trustWiden trustIntent = iota
	// trustNarrow (phase finalize) — the cluster is dropping the old CA. Trust may
	// only fall back to anchors already held; nothing new may be introduced by a
	// phase whose entire job is removal.
	trustNarrow
	// trustEither (RollbackCA) — a rollback legitimately narrows or re-widens
	// depending on how far the rotation got, and the worker has no phase context to
	// tell which. It may do one or the other, never both at once: no rotation state
	// both adds and drops an anchor, and forbidding it keeps commit's partial-apply
	// argument true on this path too.
	trustEither
)

func (i trustIntent) String() string {
	switch i {
	case trustWiden:
		return "trust-new"
	case trustNarrow:
		return "finalize"
	case trustEither:
		return "rollback"
	default:
		return fmt.Sprintf("trustIntent(%d)", int(i))
	}
}

// trustPaths are the five files a trust refresh rewrites, in commit order.
type trustPaths struct {
	caBundle            string // the cluster trust bundle
	kubeletClientCA     string // kubelet clientCAFile — inbound trust
	caCert              string // read by other tooling; kept in step with the bundle
	kubeletKubeconfig   string // outbound trust: how kubelet verifies the apiserver
	bootstrapKubeconfig string // outbound trust: the join's CA, kept in step with kubelet.kubeconfig
}

func workerTrustPaths() trustPaths {
	return trustPaths{
		caBundle:            pki.RootTrustBundlePath(),
		kubeletClientCA:     pki.KubeletClientBundlePath(),
		caCert:              filepath.Join(version.PKIDir, pki.RootCAName+".crt"),
		kubeletKubeconfig:   filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig"),
		bootstrapKubeconfig: version.BootstrapKubeconfig,
	}
}

// planTrustRefresh fetches the cluster-info bundle over verified TLS, runs L2-L4,
// and stages every resulting file next to its destination. It performs NO
// mutation of a live file — every rejection above leaves the node byte-identical,
// which is what makes a refused refresh a clean abort rather than a half-applied
// one. Call commit to apply, or abort to discard.
func planTrustRefresh(server string, intent trustIntent, p trustPaths) (_ *trustUpdate, err error) {
	onDiskPEM, source, onDisk, err := readRefreshAnchor(p)
	if err != nil {
		return nil, err
	}

	fetchedPEM, peer, err := bootstrap.FetchClusterCAVerified(server, onDiskPEM)
	if err != nil {
		return nil, err
	}
	fetched, err := parseTrustBundle("cluster-info bundle from "+server, fetchedPEM)
	if err != nil {
		return nil, err
	}
	// Containment runs before the peer check purely for the error an operator sees:
	// a wholesale pivot fails both, and "shares no certificate with your bundle" is
	// the actionable message where "does not validate the peer" is a symptom.
	added, dropped, err := checkTrustTransition(onDisk, fetched, intent)
	if err != nil {
		return nil, err
	}
	if err := bundleCoversPeer(fetched, peer); err != nil {
		return nil, err
	}

	u := &trustUpdate{added: added, dropped: dropped}
	planned := false
	defer func() {
		if planned {
			return
		}
		if cerr := u.abort(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("staged temp files could not be removed and remain on disk: %w", cerr))
		}
	}()
	// ca-bundle.crt, the kubelet's clientCAFile (unless split) and ca.crt carry
	// the fetched bundle verbatim.
	split, err := checkKubeletRefreshInputs(p, onDiskPEM, source)
	if err != nil {
		return nil, err
	}
	files := []string{p.caBundle, p.kubeletClientCA, p.caCert}
	if split {
		files = []string{p.caBundle, p.caCert}
	}
	for _, f := range files {
		s, err := stageFile(f, fetchedPEM, 0o644)
		if err != nil {
			return nil, err
		}
		u.staged = append(u.staged, s)
	}
	if split {
		kubeletPEM, err := planKubeletClientRefresh(server, onDiskPEM, intent, p)
		if err != nil {
			return nil, err
		}
		s, err := stageFile(p.kubeletClientCA, kubeletPEM, 0o644)
		if err != nil {
			return nil, err
		}
		u.staged = append(u.staged, s)
	}
	// The kubeconfigs embed the CA as data. Absent ones are skipped, not created —
	// a worker may have no bootstrap-kubeconfig.
	for _, kc := range refreshKubeconfigs(p) {
		data, err := refreshKubeconfig(kc, fetchedPEM)
		if err != nil {
			return nil, err
		}
		s, err := stageFile(kc, data, 0o600)
		if err != nil {
			return nil, err
		}
		u.staged = append(u.staged, s)
	}
	planned = true
	return u, nil
}

// refreshServer is the endpoint a worker refresh fetches cluster-info from.
func refreshServer(p trustPaths) (string, error) {
	server, err := kubeconfig.CurrentServer(p.kubeletKubeconfig)
	if err != nil {
		return "", fmt.Errorf("worker: read kubelet kubeconfig endpoint: %w", err)
	}
	return server, nil
}

// refreshKubeconfigs are the kubeconfigs a refresh rewrites: absent ones are
// skipped, not created, since a worker may have no bootstrap-kubeconfig.
func refreshKubeconfigs(p trustPaths) []string {
	var out []string
	for _, kc := range []string{p.kubeletKubeconfig, p.bootstrapKubeconfig} {
		if fileExists(kc) {
			out = append(out, kc)
		}
	}
	return out
}

// refreshKubeconfig is kc with its embedded CA replaced by ca.
func refreshKubeconfig(kc string, ca []byte) ([]byte, error) {
	data, err := kubeconfig.ReadWithClusterCA(kc, ca)
	if err != nil {
		return nil, fmt.Errorf("rewrite CA in %s: %w", kc, err)
	}
	return data, nil
}

// readRefreshAnchor reads and parses the on-disk trust a refresh starts from.
func readRefreshAnchor(p trustPaths) ([]byte, string, []*x509.Certificate, error) {
	onDiskPEM, source, err := readTrustAnchor(p)
	if err != nil {
		return nil, "", nil, err
	}
	onDisk, err := parseTrustBundle("on-disk trust bundle "+source, onDiskPEM)
	if err != nil {
		return nil, "", nil, err
	}
	return onDiskPEM, source, onDisk, nil
}

// checkKubeletRefreshInputs makes a refresh's local checks of the kubelet
// client-CA bundle and reports whether it is split from the root. A missing
// bundle cannot tell whether this worker joined a split cluster, and recreating
// it from the root would grant root-signed clients access to the kubelet, so
// the operator must restore the original.
func checkKubeletRefreshInputs(p trustPaths, onDiskPEM []byte, source string) (bool, error) {
	if _, err := os.Stat(p.kubeletClientCA); err != nil {
		return false, fmt.Errorf("cannot refresh root trust: restore the original kubelet client-CA bundle %s before retrying: %w", p.kubeletClientCA, err)
	}
	split, err := pki.KubeletClientSplitPEM(p.kubeletClientCA, onDiskPEM, source)
	if err != nil {
		return false, fmt.Errorf("tell whether %s follows the cluster root: %w", p.kubeletClientCA, err)
	}
	migrating, err := pki.KubeletClientMigratingPEM(p.kubeletClientCA, onDiskPEM, source)
	if err != nil {
		return false, fmt.Errorf("tell whether %s is part way through the kubelet client-CA migration: %w", p.kubeletClientCA, err)
	}
	if migrating {
		return false, fmt.Errorf("refusing to refresh trust: this node's kubelet is part way through the kubelet client-CA migration; finish it with `%s --cluster --yes` or undo it with `%s --cluster --rollback --yes` on a control plane first",
			pki.KubeletMigrateCommand, pki.KubeletMigrateCommand)
	}
	if split {
		b, err := os.ReadFile(p.kubeletClientCA)
		if err != nil {
			return false, err
		}
		if _, err := parseTrustBundle("kubelet client-CA bundle "+p.kubeletClientCA, b); err != nil {
			return false, err
		}
	}
	return split, nil
}

// readTrustAnchor returns the node's existing trust material through pki's one
// "trust the bundle, else ca.crt" reader — the same order kubeconfig.Write and
// token.ensureClusterInfo embed, over the files pki's manifest classifies. The
// fallback is what keeps a node that only ever had ca.crt written refreshable.
// The refresh's own paths name the files it rewrites; they live in one PKI
// directory, and the anchor read goes through that directory.
func readTrustAnchor(p trustPaths) ([]byte, string, error) {
	b, source, err := pki.ReadRootTrustFrom(filepath.Dir(p.caBundle))
	if err != nil {
		return nil, "", fmt.Errorf("read this node's trust anchor — a trust refresh needs an existing anchor to verify the apiserver against: %w", err)
	}
	return b, source, nil
}

// parseTrustBundle decodes a CA trust bundle under pki.KubeletPolicy, the
// kubelet-verbatim policy. caFromClusterInfo returns the ConfigMap's
// certificate-authority-data on len(...) > 0 alone — never PEM-decoded, never
// parsed, as does its initial-join twin fetchClusterCA — so without this,
// arbitrary bytes reach the kubelet's clientCAFile. The policy is the strict
// one on purpose: the fetched bytes are written verbatim, so every byte must
// be a CERTIFICATE block that client-go's cert.ParseCertsPEM — the parser the
// kubelet loads clientCAFile with — will honour. The rule each refusal
// enforces is declared (and its etcd counterpart documented) at
// pki.KubeletPolicy and pki.DecodePEMBundle, which does the decoding.
func parseTrustBundle(what string, pemBytes []byte) ([]*x509.Certificate, error) {
	certs, err := pki.DecodePEMBundle(pemBytes, pki.KubeletPolicy)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return certs, nil
}

// bundleCoversPeer (L2) requires the fetched bundle to validate the serving
// certificate of the connection it arrived on. It deliberately does not re-check
// the hostname: the TLS handshake already did that against the on-disk anchor
// (L1), and repeating it would only re-verify what L1 established. This is what
// catches a stale or regressed cluster-info republish — the case containment
// alone passes, because a narrower bundle intersects the on-disk one perfectly
// well.
func bundleCoversPeer(fetched, peer []*x509.Certificate) error {
	if len(peer) == 0 {
		return fmt.Errorf("refusing to apply the fetched trust bundle: the connection presented no certificate to check it against")
	}
	roots := x509.NewCertPool()
	for _, c := range fetched {
		roots.AddCert(c)
	}
	intermediates := x509.NewCertPool()
	for _, c := range peer[1:] {
		intermediates.AddCert(c)
	}
	_, err := peer[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("refusing to apply the fetched trust bundle: it does not validate the apiserver serving cert of the connection it arrived on, so adopting it would immediately cut this kubelet off from the apiserver — a stale or regressed cluster-info republish looks exactly like this: %w", err)
	}
	return nil
}

// checkTrustTransition (L3) bounds how far a single refresh may move this node's
// trust store, per the caller's intent, and returns what the accepted transition
// adds and drops so the caller can report it. See the trustIntent constants for
// the rule each phase gets and why intent is a parameter, not a phase lookup.
func checkTrustTransition(onDisk, fetched []*x509.Certificate, intent trustIntent) (added, dropped []*x509.Certificate, err error) {
	have := fingerprints(onDisk)
	want := fingerprints(fetched)
	added = dedupe(notIn(fetched, have))
	dropped = dedupe(notIn(onDisk, want))

	// Disjoint is a wholesale pivot in any phase, and is the one refusal whose
	// cause an operator cannot infer from the fact that the phase halted.
	if len(notIn(fetched, have)) == len(fetched) {
		return nil, nil, fmt.Errorf("refusing to apply the fetched trust bundle: it shares no certificate with this node's on-disk bundle (%s), which is a wholesale trust pivot rather than a rotation: every rotation, --force included, keeps the old root in the bundle until finalize, so do not trust this bundle. If the cluster's control planes were rebuilt on a new root, remove this node (`sudo %s uninstall --yes`) and join it again with a token from `stratum token create` on a control plane", describe(onDisk), version.StratumInstallPath)
	}
	switch intent {
	case trustWiden:
		if len(dropped) > 0 {
			return nil, nil, fmt.Errorf("refusing to apply the fetched trust bundle: this phase may only widen this node's trust, but the bundle drops %d CA(s) still trusted here (%s) — a stale cluster-info republish looks exactly like this", len(dropped), describe(dropped))
		}
		if len(added) > 1 {
			return nil, nil, fmt.Errorf("refusing to apply the fetched trust bundle: it introduces %d new CAs (%s) but a CA rotation stages exactly one", len(added), describe(added))
		}
		if len(added) == 0 && len(dedupe(onDisk)) == 1 {
			return nil, nil, fmt.Errorf("refusing to apply the fetched trust bundle: cluster-info published the one CA this node already trusts (%s), so trust-new would widen nothing — the control planes have not republished cluster-info (RepinCA is best-effort and its failure is only a warning). Proceeding would strand this node once sign-new promotes the new signer; re-run trust-new on the control planes first", describe(dedupe(onDisk)))
		}
	case trustNarrow:
		if len(added) > 0 {
			return nil, nil, fmt.Errorf("refusing to apply the fetched trust bundle: this phase may only drop CAs, but the bundle introduces %d not currently trusted here (%s)", len(added), describe(added))
		}
		if len(dropped) == 0 && len(dedupe(onDisk)) > 1 {
			return nil, nil, fmt.Errorf("refusing to apply the fetched trust bundle: this node still trusts %d CAs (%s) and cluster-info published all of them, so finalize would drop nothing — the control planes have not republished cluster-info (RepinCA is best-effort and its failure is only a warning). Finalize on the control planes first, then retry this node", len(dedupe(onDisk)), describe(dedupe(onDisk)))
		}
	case trustEither:
		if len(added) > 1 {
			return nil, nil, fmt.Errorf("refusing to apply the fetched trust bundle: a rollback may re-widen by the one staged CA, but this bundle introduces %d (%s)", len(added), describe(added))
		}
		if len(added) > 0 && len(dropped) > 0 {
			return nil, nil, fmt.Errorf("refusing to apply the fetched trust bundle: a rollback may widen or narrow this node's trust, not both at once, but this bundle adds %s and drops %s — no rotation state produces that", describe(added), describe(dropped))
		}
	default:
		return nil, nil, fmt.Errorf("refusing to apply the fetched trust bundle: unknown trust intent %d, so there is no rule to judge it by", int(intent))
	}
	return added, dropped, nil
}

// fingerprints keys certificates by SHA-256 over the full DER. "Already trusted"
// means this exact certificate, so full DER is the comparison unit; subject+SKI
// is forgeable outright and SPKI would accept a re-issued look-alike. Note
// bootstrap.verifyCAHash pins SPKI instead, correctly — a discovery pin is over a key, not a
// certificate. The two are not interchangeable and are deliberately not unified.
func fingerprints(certs []*x509.Certificate) map[[sha256.Size]byte]struct{} {
	m := make(map[[sha256.Size]byte]struct{}, len(certs))
	for _, c := range certs {
		m[sha256.Sum256(c.Raw)] = struct{}{}
	}
	return m
}

func notIn(certs []*x509.Certificate, set map[[sha256.Size]byte]struct{}) []*x509.Certificate {
	var out []*x509.Certificate
	for _, c := range certs {
		if _, ok := set[sha256.Sum256(c.Raw)]; !ok {
			out = append(out, c)
		}
	}
	return out
}

// dedupe collapses repeated certificates so a bundle that lists the same CA twice
// is not counted as two additions and refused.
func dedupe(certs []*x509.Certificate) []*x509.Certificate {
	seen := make(map[[sha256.Size]byte]struct{}, len(certs))
	var out []*x509.Certificate
	for _, c := range certs {
		k := sha256.Sum256(c.Raw)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, c)
	}
	return out
}

// describe names CAs for an operator. The CommonName alone does not identify one:
// pki mints both the live root and the rotation root as "stratum-ca", so a refusal
// that said only that would leave the reader unable to tell which anchor moved.
// The pin is the same "sha256:<hex>" over SubjectPublicKeyInfo that token.CACertHash
// prints in every join command, so it can be compared directly against those.
func describe(certs []*x509.Certificate) string {
	out := make([]string, 0, len(certs))
	for _, c := range certs {
		sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
		out = append(out, fmt.Sprintf("%s sha256:%s… expires %s",
			c.Subject.CommonName, hex.EncodeToString(sum[:8]), c.NotAfter.Format("2006-01-02")))
	}
	return strings.Join(out, "; ")
}

// stagedFile is a fully-written temp file waiting to be renamed over final.
type stagedFile struct {
	tmp   string
	final string
}

// trustUpdate is a staged, not-yet-applied trust refresh, plus what applying it
// would change.
type trustUpdate struct {
	added   []*x509.Certificate
	dropped []*x509.Certificate
	staged  []stagedFile
}

// summary describes the pending change for an operator.
func (u *trustUpdate) summary() string {
	switch {
	case len(u.added) == 0 && len(u.dropped) == 0:
		return "no change (cluster-info matches this node's trust)"
	case len(u.dropped) == 0:
		return "add " + describe(u.added)
	case len(u.added) == 0:
		return "drop " + describe(u.dropped)
	default:
		return "add " + describe(u.added) + " / drop " + describe(u.dropped)
	}
}

// stageFile writes data to a temp file in final's own directory (so the later
// rename stays within one filesystem and is therefore atomic), sets its mode, and
// only then fsyncs — so the flush covers the data AND the mode, and a crash
// cannot leave a zero-length file that a successful WriteFile appeared to have
// written, nor a correct file still at CreateTemp's 0600.
func stageFile(final string, data []byte, mode os.FileMode) (stagedFile, error) {
	f, err := os.CreateTemp(filepath.Dir(final), "."+filepath.Base(final)+".tmp-*")
	if err != nil {
		return stagedFile{}, fmt.Errorf("stage %s: %w", final, err)
	}
	tmp := f.Name()
	fail := func(err error) (stagedFile, error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return stagedFile{}, fmt.Errorf("stage %s: %w", final, err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	// Chmod before Sync: fsync flushes the inode too, so doing it in this order is
	// what makes the mode durable alongside the bytes.
	if err := f.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return stagedFile{}, fmt.Errorf("stage %s: %w", final, err)
	}
	return stagedFile{tmp: tmp, final: final}, nil
}

// commit renames every staged file into place, back to back with no intervening
// I/O.
//
// This is NOT cross-file atomic and does not pretend to be: five files are five
// renames, and a crash between two leaves some files refreshed and some not. Two
// things make that residue survivable. Each file is individually complete and
// valid, because renaming a fully-written temp file cannot produce a truncated
// one. And a refresh only ever moves every file the same direction — a superset
// under trustWiden, a subset under trustNarrow, and one or the other but never
// both under trustEither (checkTrustTransition enforces exactly that) — so a
// partial application leaves the node in a state the phase already passed
// through, and re-running the phase converges.
func (u *trustUpdate) commit() error {
	for i, s := range u.staged {
		if err := os.Rename(s.tmp, s.final); err != nil {
			done := describeFinals(u.staged[:i])
			u.staged = u.staged[i:] // the un-renamed remainder still needs cleaning up
			if i == 0 {
				return fmt.Errorf("apply refreshed trust: rename into %s: %w — nothing was applied; this node's trust files are unchanged", s.final, err)
			}
			return fmt.Errorf("apply refreshed trust: rename into %s: %w — THIS NODE IS PARTIALLY REFRESHED: %d file(s) were already replaced (%s); re-run the same phase to converge", s.final, err, i, done)
		}
	}
	renamed := u.staged
	u.staged = nil
	return syncDirs(renamed)
}

// abort discards anything still staged and reports what it could not remove.
// Safe to call after commit, and safe to call twice.
//
// The failure is worth surfacing rather than swallowing: two of the five staged
// files are kubeconfigs, and bootstrap-kubeconfig carries the join's bootstrap
// token, a credential until it expires. A silent failure here would report a clean abort while leaving
// that behind, so the error is returned rather than swallowed.
func (u *trustUpdate) abort() error {
	var errs []error
	for _, s := range u.staged {
		if err := os.Remove(s.tmp); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	u.staged = nil
	return errors.Join(errs...)
}

// syncDirs fsyncs each directory a rename landed in, so the rename itself is
// durable and not just the file contents. A failure here means every file WAS
// replaced and the refresh is in effect — only its survival of a power loss is
// unconfirmed.
func syncDirs(files []stagedFile) error {
	seen := make(map[string]struct{}, len(files))
	for _, s := range files {
		dir := filepath.Dir(s.final)
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		d, err := os.Open(dir)
		if err != nil {
			return fmt.Errorf("refreshed trust IS applied to disk, but %s could not be opened to make the change crash-durable, AND the kubelet was not restarted — it is still using its previous trust; re-run the same phase: %w", dir, err)
		}
		err = d.Sync()
		_ = d.Close()
		if err != nil {
			return fmt.Errorf("refreshed trust IS applied to disk, but %s could not be synced to make the change crash-durable, AND the kubelet was not restarted — it is still using its previous trust; re-run the same phase: %w", dir, err)
		}
	}
	return nil
}

func describeFinals(files []stagedFile) string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.final)
	}
	return strings.Join(names, ", ")
}

// fetchKubeletClientCA is bootstrap.FetchKubeletClientCA, a var so a test can
// stand in for the control planes.
var fetchKubeletClientCA = bootstrap.FetchKubeletClientCA

// planKubeletClientRefresh fetches a split cluster's published kubelet
// client-CA bundle over TLS verified by roots and holds it to the same
// containment rules for intent as the cluster trust bundle.
func planKubeletClientRefresh(server string, roots []byte, intent trustIntent, p trustPaths) ([]byte, error) {
	fetched, found, err := fetchKubeletClientCA(server, roots)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("refusing to refresh trust: this node's kubelet trusts a client CA split from the cluster root, but %s publishes no %s ConfigMap to refresh it from — run this phase on the control planes first", server, clusterinfo.KubeletClientCAConfigMap)
	}
	onDiskPEM, err := os.ReadFile(p.kubeletClientCA)
	if err != nil {
		return nil, err
	}
	onDisk, err := parseTrustBundle("kubelet client-CA bundle "+p.kubeletClientCA, onDiskPEM)
	if err != nil {
		return nil, err
	}
	want, err := parseTrustBundle("published kubelet client-CA bundle from "+server, fetched)
	if err != nil {
		return nil, err
	}
	if _, _, err := checkTrustTransition(onDisk, want, intent); err != nil {
		return nil, fmt.Errorf("kubelet client-CA bundle: %w", err)
	}
	return fetched, nil
}

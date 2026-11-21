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

type trustIntent int

const (
	trustWiden trustIntent = iota
	trustNarrow
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

type trustPaths struct {
	caBundle        string
	kubeletClientCA string
	// read by other tooling; kept in step with the bundle
	caCert              string
	kubeletKubeconfig   string
	bootstrapKubeconfig string
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
	// Containment runs before the peer check purely for the more actionable operator-facing error.
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

func refreshServer(p trustPaths) (string, error) {
	server, err := kubeconfig.CurrentServer(p.kubeletKubeconfig)
	if err != nil {
		return "", fmt.Errorf("worker: read kubelet kubeconfig endpoint: %w", err)
	}
	return server, nil
}

func refreshKubeconfigs(p trustPaths) []string {
	var out []string
	for _, kc := range []string{p.kubeletKubeconfig, p.bootstrapKubeconfig} {
		if fileExists(kc) {
			out = append(out, kc)
		}
	}
	return out
}

func refreshKubeconfig(kc string, ca []byte) ([]byte, error) {
	data, err := kubeconfig.ReadWithClusterCA(kc, ca)
	if err != nil {
		return nil, fmt.Errorf("rewrite CA in %s: %w", kc, err)
	}
	return data, nil
}

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

func readTrustAnchor(p trustPaths) ([]byte, string, error) {
	b, source, err := pki.ReadRootTrustFrom(filepath.Dir(p.caBundle))
	if err != nil {
		return nil, "", fmt.Errorf("read this node's trust anchor — a trust refresh needs an existing anchor to verify the apiserver against: %w", err)
	}
	return b, source, nil
}

func parseTrustBundle(what string, pemBytes []byte) ([]*x509.Certificate, error) {
	certs, err := pki.DecodePEMBundle(pemBytes, pki.KubeletPolicy)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return certs, nil
}

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

// checkTrustTransition bounds how far one refresh may move this node's trust store, per intent.
func checkTrustTransition(onDisk, fetched []*x509.Certificate, intent trustIntent) (added, dropped []*x509.Certificate, err error) {
	have := fingerprints(onDisk)
	want := fingerprints(fetched)
	added = dedupe(notIn(fetched, have))
	dropped = dedupe(notIn(onDisk, want))

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

// dedupe collapses repeats so a bundle listing one CA twice is not refused as two additions.
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

func describe(certs []*x509.Certificate) string {
	out := make([]string, 0, len(certs))
	for _, c := range certs {
		sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
		out = append(out, fmt.Sprintf("%s sha256:%s… expires %s",
			c.Subject.CommonName, hex.EncodeToString(sum[:8]), c.NotAfter.Format("2006-01-02")))
	}
	return strings.Join(out, "; ")
}

type stagedFile struct {
	tmp   string
	final string
}

type trustUpdate struct {
	added   []*x509.Certificate
	dropped []*x509.Certificate
	staged  []stagedFile
}

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
	// Chmod before Sync: fsync flushes the inode too, so the mode is durable with the bytes.
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

// fetchKubeletClientCA is a var so a test can stand in for the control planes.
var fetchKubeletClientCA = bootstrap.FetchKubeletClientCA

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

package pki

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/ctrliq/stratum/internal/host"
)

// KubeletClientBundleBase names kubelet-client-ca-bundle.crt, the kubelet's
// clientCAFile: exactly the CAs that sign apiserver-kubelet-client. A split
// cluster holds KubeletClientCAName there; an earlier one, the cluster root.
const KubeletClientBundleBase = "kubelet-client-ca"

// KubeletClientCAName signs apiserver-kubelet-client on a split cluster.
const (
	KubeletClientCAName       = "kubelet-client-ca"
	kubeletClientCACommonName = "stratum-kubelet-client-ca"
)

// KubeletClientBundlePath is the kubelet's clientCAFile.
func KubeletClientBundlePath() string { return bundleCrtPath(KubeletClientBundleBase) }

// KubeletClientSplit reports whether this node's kubelet bundle shares no CA
// with ca-bundle.crt. Where KubeletClientCAName is on disk a missing bundle is
// split and an unreadable one an error; elsewhere both read as not split.
func KubeletClientSplit() (bool, error) {
	if _, err := os.Stat(KubeletClientBundlePath()); errors.Is(err, os.ErrNotExist) {
		return fileExists(path(KubeletClientCAName, "crt")), nil
	}
	split, err := KubeletClientSplitFiles(KubeletClientBundlePath(), bundleCrtPath("ca"))
	if errors.Is(err, errUnreadableKubeletBundle) && !fileExists(path(KubeletClientCAName, "crt")) {
		return false, nil
	}
	return split, err
}

var errUnreadableKubeletBundle = errors.New("unreadable kubelet client-CA bundle")

// KubeletClientSplitFiles reports whether kubeletBundle shares no CA with
// rootBundle. A missing kubelet bundle is not split; an unreadable one is an error.
func KubeletClientSplitFiles(kubeletBundle, rootBundle string) (bool, error) {
	if _, err := os.Stat(kubeletBundle); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	root, err := os.ReadFile(rootBundle)
	if err != nil {
		return false, fmt.Errorf("pki: read trust bundle %s: %w", rootBundle, err)
	}
	return KubeletClientSplitPEM(kubeletBundle, root, rootBundle)
}

// KubeletClientSplitPEM is KubeletClientSplitFiles against root trust already
// read from rootSource.
func KubeletClientSplitPEM(kubeletBundle string, rootPEM []byte, rootSource string) (bool, error) {
	if _, err := os.Stat(kubeletBundle); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	kubelet, err := bundleCerts(kubeletBundle)
	if err == nil && len(kubelet) == 0 {
		err = fmt.Errorf("pki: %s holds no certificate", kubeletBundle)
	}
	if err != nil {
		return false, fmt.Errorf("%w: %w", errUnreadableKubeletBundle, err)
	}
	root, err := parseBundle(rootPEM, rootSource)
	if err != nil {
		return false, err
	}
	for _, c := range kubelet {
		if slices.ContainsFunc(root, c.Equal) {
			return false, nil
		}
	}
	return true, nil
}

// ensureKubeletClientTrust writes a missing kubelet bundle: from the root on a
// node whose apiserver-kubelet-client predates the split, from a new
// KubeletClientCAName otherwise.
func ensureKubeletClientTrust() error {
	if bundleExists(KubeletClientBundleBase) {
		return nil
	}
	if fileExists(path("apiserver-kubelet-client", "crt")) && !fileExists(path(KubeletClientCAName, "crt")) {
		return SyncKubeletClientBundle()
	}
	if _, err := EnsureCA(KubeletClientCAName, kubeletClientCACommonName); err != nil {
		return err
	}
	return WriteCABundle(KubeletClientBundleBase, KubeletClientCAName)
}

// kubeletClientSigner returns KubeletClientCAName on a split node, root otherwise.
func kubeletClientSigner(root *CA) (*CA, error) {
	split, err := KubeletClientSplit()
	if err != nil || !split {
		return root, err
	}
	ca, err := loadCA(KubeletClientCAName)
	if err != nil {
		return nil, fmt.Errorf("pki: the kubelets trust only %s for clients, but it cannot sign apiserver-kubelet-client here: %w", KubeletClientCAName, err)
	}
	return ca, nil
}

// kubeletClientReissuer returns KubeletClientCAName on a split node, else the
// CA that signed the live apiserver-kubelet-client, or kubeletClientSigner's
// choice when there is none.
func kubeletClientReissuer(root *CA) (*CA, error) {
	if split, err := KubeletClientSplit(); err != nil || split {
		if err != nil {
			return nil, err
		}
		return kubeletClientSigner(root)
	}
	signed, err := leafChainsTo("apiserver-kubelet-client", KubeletClientCAName)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return kubeletClientSigner(root)
	case err != nil:
		return nil, fmt.Errorf("pki: tell which CA signed apiserver-kubelet-client: %w", err)
	case signed:
		return loadCA(KubeletClientCAName)
	}
	return root, nil
}

// SyncKubeletClientBundle rewrites kubelet-client-ca-bundle.crt from
// ca-bundle.crt by rename, so a failed write leaves the previous bundle or none,
// never a truncated one that EnsureKubeletClientBundle would keep. It does
// nothing on a split or migrating node.
func SyncKubeletClientBundle() error {
	split, err := KubeletClientSplit()
	if err != nil {
		return fmt.Errorf("pki: decide whether %s follows %s: %w", KubeletClientBundlePath(), bundleCrtPath("ca"), err)
	}
	if split {
		return nil
	}
	if t, err := ReadKubeletClientTrust(); err == nil && t.Migrating() {
		return nil
	}
	b, err := os.ReadFile(bundleCrtPath("ca"))
	if err == nil {
		err = host.WriteFileAtomically(KubeletClientBundlePath(), b, 0o644)
	}
	if err != nil {
		return fmt.Errorf("pki: write %s from %s: %w", KubeletClientBundlePath(), bundleCrtPath("ca"), err)
	}
	return nil
}

// EnsureKubeletClientBundle writes a missing kubelet-client-ca-bundle.crt from
// KubeletClientCAName when this node holds it, from ca-bundle.crt otherwise.
func EnsureKubeletClientBundle() error {
	if bundleExists(KubeletClientBundleBase) {
		return nil
	}
	if fileExists(path(KubeletClientCAName, "crt")) {
		return WriteCABundle(KubeletClientBundleBase, KubeletClientCAName)
	}
	return SyncKubeletClientBundle()
}

// EnsureKubeletClientTrusts makes kubelet-client-ca-bundle.crt carry the
// certificate of every CA in names. ca-bundle.crt is written first and the
// kubelet's bundle second, so a trust-new that stopped between the two leaves
// a rotation staged while the kubelet still trusts only the old root; a missing
// root, or a bundle that cannot be read, is re-synced from ca-bundle.crt, and a
// root that ca-bundle.crt lacks too is an error, since the kubelet would refuse
// a client certificate it signs.
func EnsureKubeletClientTrusts(names ...string) error {
	if split, err := KubeletClientSplit(); err == nil && split {
		return nil
	}
	if missing, err := kubeletClientMissing(names); err == nil && len(missing) == 0 {
		return nil
	}
	if err := SyncKubeletClientBundle(); err != nil {
		return err
	}
	missing, err := kubeletClientMissing(names)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("pki: %s does not carry %v even after it was re-synced from %s: the kubelet would refuse a client certificate signed by it",
			KubeletClientBundlePath(), missing, bundleCrtPath("ca"))
	}
	return nil
}

func kubeletClientMissing(names []string) ([]string, error) {
	have, err := bundleCerts(KubeletClientBundlePath())
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, name := range names {
		c, err := readCert(name)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(have, c.Equal) {
			missing = append(missing, name+".crt")
		}
	}
	return missing, nil
}

// writeRootBundle writes ca-bundle.crt from certNames and keeps the kubelet's
// clientCAFile in step with it.
func writeRootBundle(certNames ...string) error {
	if err := WriteCABundle("ca", certNames...); err != nil {
		return err
	}
	return SyncKubeletClientBundle()
}

// ensureKubeletClientStaged rewrites the kubelet bundle as the staged and live
// kubelet-client CAs when it lacks either, as a trust-new that stopped before
// writing it leaves it.
func ensureKubeletClientStaged() error {
	have, err := bundleCerts(KubeletClientBundlePath())
	if err != nil {
		return err
	}
	names := []string{KubeletClientCAName + "-new"}
	if fileExists(path(KubeletClientCAName, "crt")) {
		names = append(names, KubeletClientCAName)
	}
	for _, n := range names {
		c, err := readCert(n)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(have, c.Equal) {
			return WriteCABundle(KubeletClientBundleBase, names...)
		}
	}
	return nil
}

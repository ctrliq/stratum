package pki

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/ctrliq/stratum/internal/host"
)

// Names kubelet-client-ca-bundle.crt, the kubelet's clientCAFile.
const KubeletClientBundleBase = "kubelet-client-ca"

// KubeletClientCAName signs apiserver-kubelet-client on a split cluster.
const (
	KubeletClientCAName       = "kubelet-client-ca"
	kubeletClientCACommonName = "stratum-kubelet-client-ca"
)

// The kubelet's clientCAFile.
func KubeletClientBundlePath() string { return bundleCrtPath(KubeletClientBundleBase) }

// A missing bundle is split where KubeletClientCAName is on disk; unreadable there is an error.
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

// A missing kubelet bundle is not split; an unreadable one is an error.
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

// KubeletClientSplitFiles against root trust already read from rootSource.
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

// From the root when apiserver-kubelet-client predates the split, else the new CA.
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

// Prefers the CA that signed the live apiserver-kubelet-client when not split.
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

// By rename: a failed write leaves the previous bundle or none, never a truncated one.
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

// From KubeletClientCAName when this node holds it, from ca-bundle.crt otherwise.
func EnsureKubeletClientBundle() error {
	if bundleExists(KubeletClientBundleBase) {
		return nil
	}
	if fileExists(path(KubeletClientCAName, "crt")) {
		return WriteCABundle(KubeletClientBundleBase, KubeletClientCAName)
	}
	return SyncKubeletClientBundle()
}

// Root bundle first, kubelet bundle second, so a stopped trust-new never locks kubelets out.
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

// Writes ca-bundle.crt and keeps the kubelet's clientCAFile in step.
func writeRootBundle(certNames ...string) error {
	if err := WriteCABundle("ca", certNames...); err != nil {
		return err
	}
	return SyncKubeletClientBundle()
}

// Add whichever of the staged and live kubelet-client CAs the bundle lacks.
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

package runtime

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/enginesettings"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// PrepareNodeConfigs writes what kubelet+containerd need: the kubelet config
// (driver-matched to containerd) and containerd's config.toml. Lives here (not
// in internal/cluster) so the HA join path can call it without an import cycle
// through certs — every node runs kubelet.
//
// The render's change flag is dropped here on purpose: a join (and a
// controller's prepare) run before any supervisor exists, so nothing is
// running to restart and the supervisor brings containerd up on the config
// just written.
func PrepareNodeConfigs(st *nodestate.NodeState) error {
	if _, err := EnsureConfig(); err != nil {
		return fmt.Errorf("containerd config: %w", err)
	}
	return writeKubeletConfig(st)
}

// WriteNodeConfigs is PrepareNodeConfigs without the legacy-config heal
// (see WriteConfig): the in-place upgrade's entry point, which reaps the legacy
// file only after the supervisor has containerd on the new config path.
//
// It reports whether the render changed containerd's config, so its caller can
// fold that change into its own single host reload instead of restarting
// containerd a second time: the render restarts nothing, and the reload the
// upgrade runs after it is the one restart an upgrade gives containerd.
func WriteNodeConfigs(st *nodestate.NodeState) (bool, error) {
	return WriteNodeConfigsWithWriter(os.Stdout, st)
}

// WriteNodeConfigsWithWriter writes node configs and reports progress to w.
func WriteNodeConfigsWithWriter(w io.Writer, st *nodestate.NodeState) (bool, error) {
	changed, err := WriteConfigWithWriter(w)
	if err != nil {
		return changed, fmt.Errorf("containerd config: %w", err)
	}
	if err := writeKubeletConfig(st); err != nil {
		return changed, err
	}
	return changed, nil
}

// writeKubeletConfig renders the kubelet.conf template. The node profile's
// settings render first, in its order; then the serving certificate: a node
// whose profile bootstraps it, or that has no local serving-cert leaf (a
// token-joined worker), bootstraps it through a CSR, and any other pins the
// static kubelet pair; the lines are internal/enginesettings' serving lines,
// which carry each branch's reason.
func writeKubeletConfig(st *nodestate.NodeState) error {
	pkiDir := version.PKIDir
	prof, err := profile.ForNode(st.ProfileName())
	if err != nil {
		return fmt.Errorf("render kubelet.conf: %w", err)
	}
	dnsIP, err := ClusterDNSIP(st)
	if err != nil {
		return err
	}
	if err := pki.EnsureKubeletClientBundle(); err != nil {
		return err
	}
	pullPolicy, err := payloadImagePullPolicy()
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
authentication:
  anonymous: {enabled: false}
  webhook: {enabled: true}
  x509: {clientCAFile: %s}
authorization: {mode: Webhook}
cgroupDriver: %s
clusterDNS: [%s]
clusterDomain: %q
containerRuntimeEndpoint: %s
failSwapOn: false
readOnlyPort: 0
streamingConnectionIdleTimeout: 5m
makeIPTablesUtilChains: true
eventRecordQPS: 5
tlsCipherSuites: [%s]
%s`, pki.KubeletClientBundlePath(), CgroupDriver(), dnsIP, st.Domain(),
		version.ContainerdSock, version.TLSCipherSuites, pullPolicy)
	pkiFile := func(f string) string { return pkiDir + "/" + f }
	b.WriteString(prof.KubeletLines(version.ProfileLocations(pkiFile)))
	b.WriteString(enginesettings.ServingLines(servingBootstrapped(prof, pkiDir), pkiFile))
	// A worker's client certificate comes from TLS bootstrap and the
	// controller-manager signs it for a year. Nothing renews it unless the
	// kubelet asks: without rotateCertificates (default false) every worker went
	// NotReady a year after its join and could not recover, since its bootstrap
	// token had long expired. The renewal CSR is approved by the
	// stratum-csr-renewal binding (internal/token). A controller's client
	// certificate is a Stratum PKI leaf the certificate-renewal loop rotates,
	// so it does not get a second renewer here.
	if st.Role == "worker" {
		b.WriteString("rotateCertificates: true\n")
	}
	fmt.Fprintf(&b, "staticPodPath: %s\n", staticpod.Dir())
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		return err
	}
	// 0600 as CIS 4.1.1 asks. The atomic writer replaces the file whole: the
	// rename swaps in a freshly created inode at the given mode, so a
	// kubelet.conf an older release wrote 0644 comes back 0600 without a
	// separate chmod, and a crash mid-write leaves the previous config, not
	// a torn one the kubelet cannot start from.
	return atomicfile.WriteFile(KubeletConfigPath(), []byte(b.String()), 0o600)
}

// payloadImagePullPolicy is the kubelet.conf lines that exempt every payload
// image from kubelet's pull-credential check, so a payload image is always used
// from the node. The exemption covers the payload registry namespace and the
// repository of every image the staged archives carry: an importer's images
// sit outside the namespace, and without their own entries kubelet asks the
// registry for them, which an airgapped node cannot reach. A build with
// neither renders none, leaving kubelet on its default policy rather than
// exempting a namespace that is not the payload's.
func payloadImagePullPolicy() (string, error) {
	var allow []string
	r := addons.ImageRegistry
	stamped := r != "" && r != "unknown"
	if stamped {
		allow = append(allow, r+"/*")
	}
	archives, _ := filepath.Glob(filepath.Join(version.DataDir, assets.ImagesDir, "*.tar"))
	names, err := assets.ImageNames(archives)
	if err != nil {
		return "", fmt.Errorf("list the payload images kubelet must not re-verify: %w", err)
	}
	repos := map[string]bool{}
	for n := range names {
		repo := assets.Repository(n)
		if !stamped || !strings.HasPrefix(repo, r+"/") {
			repos[repo] = true
		}
	}
	allow = append(allow, slices.Sorted(maps.Keys(repos))...)
	if len(allow) == 0 {
		return "", nil
	}
	quoted := make([]string, len(allow))
	for i, a := range allow {
		quoted[i] = strconv.Quote(a)
	}
	return "imagePullCredentialsVerificationPolicy: NeverVerifyAllowlistedImages\n" +
		"preloadedImagesVerificationAllowlist: [" + strings.Join(quoted, ", ") + "]\n", nil
}

// ClusterDNSIP is the cluster DNS Service IP this node's resolver hands its
// pods, and the one derivation for every renderer of that address — the
// kubelet's clusterDNS (writeKubeletConfig) and the CoreDNS stack writeBundles
// stages: the tenth address of the cluster's service CIDR (netcfg.DNSIPFor).
// Every controller records the CIDR in its state; a worker records it from
// --service-cidr, which `stratum token create` prints on a cluster with a
// non-default range. A worker that joined before that records none, and
// learns the address from the cluster's kube-dns Service instead; the recorded
// address stands in only where no range is recorded, because the range is the
// authority where it exists. With neither it is the default 10.96.0.10, as it
// always was.
func ClusterDNSIP(st *nodestate.NodeState) (string, error) {
	switch {
	case st.ServiceCIDR != "":
		return netcfg.DNSIPFor(st.ServiceCIDR)
	case st.ClusterDNSIP != "":
		return st.ClusterDNSIP, nil
	}
	return netcfg.Default().DNSIPv4, nil
}

// WriteKubeletConfig renders kubelet.conf alone.
func WriteKubeletConfig(st *nodestate.NodeState) error { return writeKubeletConfig(st) }

// KubeletConfigPath is the kubelet.conf writeKubeletConfig renders.
func KubeletConfigPath() string { return filepath.Join(version.K8sConfDir, "kubelet.conf") }

// ServingBootstrapped reports whether the kubelet writeKubeletConfig configures
// for st gets its serving cert through a kubelet-serving CSR (serverTLSBootstrap)
// rather than the static kubelet.crt leaf in pkiDir: when the node's profile
// bootstraps it, and on a node with no kubelet.crt, which is a token-joined
// worker. It resolves the profile as the renderer does, and refuses a profile
// the renderer would refuse.
func ServingBootstrapped(st *nodestate.NodeState, pkiDir string) (bool, error) {
	prof, err := profile.ForNode(st.ProfileName())
	if err != nil {
		return false, err
	}
	return servingBootstrapped(prof, pkiDir), nil
}

// servingBootstrapped is the serving-certificate decision the renderer and
// ServingBootstrapped share.
func servingBootstrapped(p profile.Profile, pkiDir string) bool {
	return p.KubeletServerTLSBootstrap || !hasServingLeaf(pkiDir)
}

// hasServingLeaf reports whether the node holds the local kubelet serving-cert
// leaf a control plane carries.
func hasServingLeaf(pkiDir string) bool {
	_, err := os.Stat(filepath.Join(pkiDir, "kubelet.crt"))
	return err == nil
}

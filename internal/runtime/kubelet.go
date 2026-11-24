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

	"k8s.io/apimachinery/pkg/util/validation"

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

// PrepareNodeConfigs drops the change flag on purpose: a join runs before any supervisor exists.
func PrepareNodeConfigs(st *nodestate.NodeState) error {
	if _, err := EnsureContainerdConfigs(os.Stdout, st); err != nil {
		return err
	}
	return writeKubeletConfig(st)
}

// EnsureContainerdConfigs is EnsureConfig plus the drop-in, before containerd launches.
func EnsureContainerdConfigs(w io.Writer, st *nodestate.NodeState) (bool, error) {
	changed, err := EnsureConfig()
	if err != nil {
		return changed, fmt.Errorf("containerd config: %w", err)
	}
	handlersChanged, err := writeHandlers(w, st.RuntimeHandlers)
	if err != nil {
		return changed || handlersChanged, fmt.Errorf("containerd runtime handlers: %w", err)
	}
	return changed || handlersChanged, nil
}

// WriteNodeConfigs reports the change so its caller folds it into its one reload.
func WriteNodeConfigs(st *nodestate.NodeState) (bool, error) {
	return WriteNodeConfigsWithWriter(os.Stdout, st)
}

func WriteNodeConfigsWithWriter(w io.Writer, st *nodestate.NodeState) (bool, error) {
	changed, err := WriteConfigWithWriter(w)
	if err != nil {
		return changed, fmt.Errorf("containerd config: %w", err)
	}
	handlersChanged, err := writeHandlers(w, st.RuntimeHandlers)
	changed = changed || handlersChanged
	if err != nil {
		return changed, fmt.Errorf("containerd runtime handlers: %w", err)
	}
	if err := writeKubeletConfig(st); err != nil {
		return changed, err
	}
	return changed, nil
}

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
	// A worker's client cert renews only when the kubelet asks; internal/certs rotates a controller's.
	if st.Role == "worker" {
		b.WriteString("rotateCertificates: true\n")
	}
	taints, err := registerWithTaintsLine(st.NodeTaints)
	if err != nil {
		return fmt.Errorf("render kubelet.conf: %w; repair nodeTaints in %s", err, nodestate.StatePath())
	}
	b.WriteString(taints)
	fmt.Fprintf(&b, "staticPodPath: %s\n", staticpod.Dir())
	if err := os.MkdirAll(staticpod.Dir(), 0o755); err != nil {
		return err
	}
	// 0600 as CIS 4.1.1 asks; the atomic rename sets the mode, so no separate chmod.
	return atomicfile.WriteFile(KubeletConfigPath(), []byte(b.String()), 0o600)
}

// DropInImportStamp is kept apart from a payload archive's stamp of the same name.
func DropInImportStamp(tar string) string {
	return filepath.Join(version.K8sConfDir, "images.d", filepath.Base(tar)+".imported")
}

// ImportStampValue records size and mtime, so a replaced archive is imported again.
func ImportStampValue(fi os.FileInfo) string {
	return fmt.Sprintf("%d %d", fi.Size(), fi.ModTime().UnixNano())
}

func DropInImported(tar string) bool {
	fi, err := os.Stat(tar)
	if err != nil {
		return false
	}
	b, err := os.ReadFile(DropInImportStamp(tar))
	return err == nil && string(b) == ImportStampValue(fi)
}

// The exemption covers a whole repository; an unimported archive would exempt unpinned images.
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
	dropIns, _ := filepath.Glob(filepath.Join(version.ImagesDropInDir, "*.tar"))
	for _, archive := range dropIns {
		if !DropInImported(archive) {
			continue
		}
		got, err := assets.ArchiveImageNames(archive)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stratum: warning: %s is not exempt from kubelet's pull-credential check: read its image names: %v\n", archive, err)
			continue
		}
		if !DropInImported(archive) { // replaced while its names were read
			continue
		}
		for _, n := range got {
			names[n] = true
		}
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

// ClusterDNSIP is the one derivation for every renderer; the recorded ServiceCIDR is the authority.
func ClusterDNSIP(st *nodestate.NodeState) (string, error) {
	switch {
	case st.ServiceCIDR != "":
		return netcfg.DNSIPFor(st.ServiceCIDR)
	case st.ClusterDNSIP != "":
		return st.ClusterDNSIP, nil
	}
	return netcfg.Default().DNSIPv4, nil
}

func WriteKubeletConfig(st *nodestate.NodeState) error { return writeKubeletConfig(st) }

func KubeletConfigPath() string { return filepath.Join(version.K8sConfDir, "kubelet.conf") }

// True when the kubelet gets its serving cert by CSR; no kubelet.crt means a token-joined worker.
func ServingBootstrapped(st *nodestate.NodeState, pkiDir string) (bool, error) {
	prof, err := profile.ForNode(st.ProfileName())
	if err != nil {
		return false, err
	}
	return servingBootstrapped(prof, pkiDir), nil
}

func servingBootstrapped(p profile.Profile, pkiDir string) bool {
	return p.KubeletServerTLSBootstrap || !hasServingLeaf(pkiDir)
}

func hasServingLeaf(pkiDir string) bool {
	_, err := os.Stat(filepath.Join(pkiDir, "kubelet.crt"))
	return err == nil
}

// SplitTaint is the one reading of the spelling, for the validation, merge and rendering alike.
func SplitTaint(t string) (key, value, effect string, ok bool) {
	kv, effect, ok := strings.Cut(t, ":")
	if !ok || strings.Contains(effect, ":") {
		return "", "", "", false
	}
	key, value, _ = strings.Cut(kv, "=")
	return key, value, effect, true
}

var TaintEffects = []string{"NoSchedule", "PreferNoSchedule", "NoExecute"}

func ParseTaint(t string) (key, value, effect string, err error) {
	key, value, effect, ok := SplitTaint(t)
	if !ok {
		return "", "", "", fmt.Errorf("node taint %q is not key[=value]:Effect", t)
	}
	if !slices.Contains(TaintEffects, effect) {
		return "", "", "", fmt.Errorf("node taint %q: effect %q is not one of %s", t, effect, strings.Join(TaintEffects, ", "))
	}
	if errs := validation.IsQualifiedName(key); len(errs) > 0 {
		return "", "", "", fmt.Errorf("node taint %q: key: %s", t, strings.Join(errs, "; "))
	}
	if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
		return "", "", "", fmt.Errorf("node taint %q: value: %s", t, strings.Join(errs, "; "))
	}
	return key, value, effect, nil
}

// registerWithTaintsLine refuses a bad taint by name instead of failing every kubelet start.
func registerWithTaintsLine(taints []string) (string, error) {
	if len(taints) == 0 {
		return "", nil
	}
	entries := make([]string, 0, len(taints))
	for _, t := range taints {
		key, value, effect, err := ParseTaint(t)
		if err != nil {
			return "", err
		}
		entries = append(entries, fmt.Sprintf("{key: %q, value: %q, effect: %s}", key, value, effect))
	}
	return "registerWithTaints: [" + strings.Join(entries, ", ") + "]\n", nil
}

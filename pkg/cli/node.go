package cli

import (
	"path/filepath"
	"strings"

	"k8s.io/client-go/kubernetes"

	"github.com/ctrliq/stratum/internal/enginesettings"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/selinux"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// NodeLayout is where this node's Stratum files live.
type NodeLayout struct {
	ConfDir                      string
	DataDir                      string
	PKIDir                       string
	KubeconfigDir                string
	K8sConfigDir                 string
	StaticPodDir                 string
	KubeletConfig                string
	AdminConf                    string
	SuperAdminConf               string
	BinDir                       string
	ContainerdConfig             string
	ContainerdConfinedSinceStamp string
	SELinuxLabelStamp            string
	EtcdDataDir                  string
}

// Layout returns this node's file layout as the engine resolves it.
func Layout() NodeLayout {
	return NodeLayout{
		ConfDir:                      version.ConfDir,
		DataDir:                      version.DataDir,
		PKIDir:                       version.PKIDir,
		KubeconfigDir:                version.KubeconfigDir,
		K8sConfigDir:                 version.K8sConfDir,
		StaticPodDir:                 staticpod.Dir(),
		KubeletConfig:                runtime.KubeletConfigPath(),
		AdminConf:                    version.AdminConf,
		SuperAdminConf:               version.SuperAdminConf(),
		BinDir:                       version.BinDir,
		ContainerdConfig:             version.ContainerdConfig,
		ContainerdConfinedSinceStamp: runtime.ConfinedSincePath(),
		SELinuxLabelStamp:            selinux.LabelStampPath(),
		EtcdDataDir:                  version.EtcdDataDir,
	}
}

// PKIRotationCopies returns the staged and pre-rotation names a CA rotation keeps beside the PKI file name, or nil when it keeps none.
func PKIRotationCopies(name string) []string {
	staged := pki.StagedTrustName(name)
	if staged == "" {
		return nil
	}
	return []string{staged, pki.PreRotationBackupName(name)}
}

// TLSCipherSuites returns the strong TLS cipher suites the apiserver and kubelet are rendered with.
func TLSCipherSuites() []string {
	return strings.Split(version.TLSCipherSuites, ",")
}

// EngineAPIServerArgs returns the apiserver argv entries every control plane renders whatever its profile.
func EngineAPIServerArgs() []string {
	return enginesettings.APIServerFlags(pkiFile)
}

// ProfileAPIServerArgs returns the apiserver argv entries p contributes, as a control plane renders them.
func ProfileAPIServerArgs(p Profile) ([]string, error) {
	pp := toProfile(p)
	if err := pp.Validate(); err != nil {
		return nil, err
	}
	return pp.APIServerArgs(version.ProfileLocations(pkiFile)), nil
}

func pkiFile(name string) string { return filepath.Join(version.PKIDir, name) }

// NodeAdminClientset returns a clientset on this node's internal admin kubeconfig: super-admin.conf, else admin.conf.
func NodeAdminClientset() (kubernetes.Interface, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return nil, err
	}
	return cs, nil
}

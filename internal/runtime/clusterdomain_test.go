package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// kubelet.conf carries the configured domain, not the constant.
func TestKubeletConfigFollowsTheClusterDomain(t *testing.T) {
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })
	for _, tc := range []struct{ domain, want string }{
		{"", `clusterDomain: "cluster.local"` + "\n"},
		{"fuzz.internal", `clusterDomain: "fuzz.internal"` + "\n"},
	} {
		version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()
		st := &nodestate.NodeState{Role: "worker", Hostname: "w1", ClusterDomain: tc.domain}
		if err := writeKubeletConfig(st); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "kubelet.conf"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Errorf("clusterDomain %q: kubelet.conf lacks %q:\n%s", tc.domain, tc.want, b)
		}
	}
}

// A cluster domain that validates as a DNS name can still be a YAML scalar of
// another type when written bare: `on` and `yes` are booleans, `1e3` a number,
// `null` null. The kubelet would then read a different domain than the one
// configured, so kubelet.conf quotes it and a parse gives back the literal string.
func TestKubeletConfigClusterDomainIsALiteralString(t *testing.T) {
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })
	for _, domain := range []string{"on", "yes", "1e3", "null", "123", "fuzz.internal"} {
		t.Run(domain, func(t *testing.T) {
			if err := netcfg.ValidateDomain(domain); err != nil {
				t.Fatalf("%q should be a valid domain: %v", domain, err)
			}
			version.PKIDir, version.K8sConfDir = joinedPKIDir(t), t.TempDir()
			if err := writeKubeletConfig(&nodestate.NodeState{Role: "worker", Hostname: "w1", ClusterDomain: domain}); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "kubelet.conf"))
			if err != nil {
				t.Fatal(err)
			}
			var conf map[string]any
			if err := yaml.Unmarshal(b, &conf); err != nil {
				t.Fatalf("kubelet.conf does not parse: %v\n%s", err, b)
			}
			if got, ok := conf["clusterDomain"].(string); !ok || got != domain {
				t.Errorf("clusterDomain parses as %#v, want the string %q", conf["clusterDomain"], domain)
			}
		})
	}
}

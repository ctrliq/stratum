package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/version"
)

func kubeletProfileFixture(t *testing.T) {
	t.Helper()
	oldPKI, oldConf := version.PKIDir, version.K8sConfDir
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir = oldPKI, oldConf })
	version.PKIDir = joinedPKIDir(t)
	version.K8sConfDir = t.TempDir()
}

func setServingLeaf(t *testing.T, on bool) {
	t.Helper()
	for _, f := range []string{"kubelet.crt", "kubelet.key"} {
		p := filepath.Join(version.PKIDir, f)
		if on {
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func renderKubelet(t *testing.T, st *nodestate.NodeState) string {
	t.Helper()
	if err := WriteKubeletConfig(st); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(version.K8sConfDir, "kubelet.conf"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func registerForTest(t *testing.T, ps ...profile.Profile) {
	t.Helper()
	prev := profile.All()
	if err := profile.SetRegistered(ps); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

func controller(profileName string) *nodestate.NodeState {
	return &nodestate.NodeState{
		Role: "controller", Hostname: "cp1", NodeIP: "192.0.2.10",
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12", Profile: profileName,
	}
}

func TestKubeletConfRendersOnlyTheNodesProfile(t *testing.T) {
	kubeletProfileFixture(t)
	edge := profile.Profile{
		Name:            "edge",
		KubeletSettings: []profile.Setting{{Key: "registryPullQPS", Value: "5", From: profile.Literal}},
	}
	registerForTest(t, edge)
	setServingLeaf(t, true)

	plain := renderKubelet(t, controller(""))
	got := renderKubelet(t, controller("edge"))
	want := strings.Replace(plain, "serverTLSBootstrap: false\n", "registryPullQPS: 5\nserverTLSBootstrap: false\n", 1)
	if got == plain || got != want {
		t.Errorf("the edge kubelet.conf is not the plain one with exactly the edge profile's setting before the serving lines:\ngot:\n%s\nwant:\n%s", got, want)
	}

	edge.KubeletServerTLSBootstrap = true
	registerForTest(t, edge)
	got = renderKubelet(t, controller("edge"))
	if !strings.Contains(got, "serverTLSBootstrap: true\n") || strings.Contains(got, "tlsCertFile:") {
		t.Errorf("a profile with KubeletServerTLSBootstrap must bootstrap even beside the leaf:\n%s", got)
	}
}

func TestKubeletConfRefusesAnUnregisteredProfile(t *testing.T) {
	kubeletProfileFixture(t)
	registerForTest(t)
	err := WriteKubeletConfig(controller("edge"))
	if !errors.Is(err, profile.ErrUnregistered) {
		t.Fatalf("WriteKubeletConfig for a node whose profile is not registered = %v, want ErrUnregistered", err)
	}
	if _, serr := os.Stat(filepath.Join(version.K8sConfDir, "kubelet.conf")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("kubelet.conf was written for a node whose profile could not be resolved (stat: %v)", serr)
	}
}

func TestServingBootstrappedAgreesWithTheRenderedKubeletConf(t *testing.T) {
	kubeletProfileFixture(t)
	registerForTest(t, profiletest.Profile(), profile.Profile{Name: "edge", KubeletServerTLSBootstrap: true})
	for _, role := range []string{"controller", "worker"} {
		for _, name := range []string{"", "cis", "edge"} {
			for _, leaf := range []bool{false, true} {
				setServingLeaf(t, leaf)
				st := controller(name)
				st.Role = role
				rendered := strings.Contains(renderKubelet(t, st), "serverTLSBootstrap: true\n")
				got, err := ServingBootstrapped(st, version.PKIDir)
				if err != nil {
					t.Fatalf("%s, profile %q, leaf=%v: ServingBootstrapped = %v", role, name, leaf, err)
				}
				if got != rendered {
					t.Errorf("%s, profile %q, leaf=%v: ServingBootstrapped = %v, but kubelet.conf bootstraps = %v", role, name, leaf, got, rendered)
				}
			}
		}
	}

	if _, err := ServingBootstrapped(controller("unknown"), version.PKIDir); !errors.Is(err, profile.ErrUnregistered) {
		t.Errorf("ServingBootstrapped for a node whose profile is not registered = %v, want ErrUnregistered", err)
	}
}

func TestTheEngineKubeletKeysAreReservedFromProfiles(t *testing.T) {
	kubeletProfileFixture(t)
	registerForTest(t)
	for _, role := range []string{"controller", "worker"} {
		for _, leaf := range []bool{false, true} {
			setServingLeaf(t, leaf)
			st := controller("")
			st.Role = role
			st.NodeTaints = []string{"stratum.ciq.com/unverified=true:NoSchedule"}
			var conf map[string]any
			if err := yaml.Unmarshal([]byte(renderKubelet(t, st)), &conf); err != nil {
				t.Fatal(err)
			}
			for key := range conf {
				if !profile.EngineOwnsKubeletKey(key) {
					t.Errorf("%s, leaf=%v: kubelet.conf carries %q, which a profile may still set", role, leaf, key)
				}
			}
		}
	}
	for _, key := range []string{"imagePullCredentialsVerificationPolicy", "preloadedImagesVerificationAllowlist"} {
		if !profile.EngineOwnsKubeletKey(key) {
			t.Errorf("the payload pull-policy key %q is not reserved", key)
		}
	}
}

package staticpod

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

func edgeProfile() profile.Profile {
	return profile.Profile{
		Name: "edge",
		APIServerFlags: []profile.Flag{
			{Name: "--event-ttl", Value: "2h", From: profile.Literal},
			{Name: "--tracing-config-file", Value: "tracing.yaml", From: profile.ConfigFile},
		},
		ConfigFiles: []profile.File{{Name: "tracing.yaml", Kind: profile.PlainFile,
			Render: func(profile.Context) ([]byte, error) { return []byte("tracing"), nil }}},
	}
}

func setRegistered(t *testing.T, ps ...profile.Profile) {
	t.Helper()
	prev := profile.All()
	if err := profile.SetRegistered(ps); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

func TestAPIServerRendersOnlyTheNodesProfile(t *testing.T) {
	oldConf := version.K8sConfDir
	t.Cleanup(func() { version.K8sConfDir = oldConf })
	version.K8sConfDir = "/cfg"
	pki := func(f string) string { return "/pki/" + f }
	st := &nodestate.NodeState{
		Role: "controller", Hostname: "cp1", NodeIP: "192.0.2.10",
		PodCIDR: "10.245.0.0/16", ServiceCIDR: "10.96.0.0/12",
	}

	plain := apiserverArgs(st, profile.Profile{}, pki)
	st.Profile = "edge"
	edge := apiserverArgs(st, edgeProfile(), pki)

	kca := slices.Index(plain, "--kubelet-certificate-authority="+pki("ca-bundle.crt"))
	if kca < 0 {
		t.Fatal("the render without a profile carries no --kubelet-certificate-authority")
	}
	want := slices.Insert(slices.Clone(plain), kca, "--event-ttl=2h", "--tracing-config-file=/cfg/tracing.yaml")
	if !slices.Equal(edge, want) {
		t.Errorf("the edge argv is not the plain one with exactly the edge profile's flags before --kubelet-certificate-authority:\ngot:  %q\nwant: %q", edge, want)
	}
	for _, a := range edge {
		if strings.HasPrefix(a, "--audit-") || strings.HasPrefix(a, "--encryption-provider-config") {
			t.Errorf("the edge argv carries %q, a cis flag the edge profile does not declare", a)
		}
	}
}

func TestAPIServerRefusesAnUnregisteredProfile(t *testing.T) {
	setRegistered(t)
	st := testNode(t)
	st.Profile = "edge"
	err := WriteAll(st)
	if !errors.Is(err, profile.ErrUnregistered) {
		t.Fatalf("WriteAll for a node whose profile is not registered = %v, want ErrUnregistered", err)
	}
	if _, serr := os.Stat(Path("kube-apiserver")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("a kube-apiserver manifest was written for a node whose profile could not be resolved (stat: %v)", serr)
	}
}

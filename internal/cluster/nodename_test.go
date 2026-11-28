package cluster

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/nodestate"
	stratumpki "github.com/ctrliq/stratum/internal/pki"
)

// A control plane on a mixed-case hostname registers its Node lowercased, as
// every kubelet does. The name preparation derives, and everything built from
// it, must be that same lowercased name: the kubelet flag, the kubelet
// certificate's CN and SAN, the etcd member name and the saved node state.
// With the hostname's case kept, the kubelet's certificate named
// system:node:HK-k8s-ctrl-01 while it registered hk-k8s-ctrl-01, the Node
// authorizer refused it, and `stratum up` waited ten minutes for a Node that
// never appeared.
func TestPrepareControllerUsesTheLowercaseNodeName(t *testing.T) {
	const hostname, want = "HK-k8s-ctrl-01.Corp.Example", "hk-k8s-ctrl-01"
	prepareFixture(t)
	hostHostname = func() (string, error) { return host.NodeNameOf(hostname) }
	saved := assembleNode
	t.Cleanup(func() { assembleNode = saved })
	var req ha.AssembleRequest
	choice := host.NodeIPChoice{IP: "10.0.0.11", Source: "detect"}
	assembleNode = func(_ context.Context, a ha.NodeAssembly, r ha.AssembleRequest) (host.NodeIPChoice, error) {
		req = r
		if _, err := a.ResolveNet(); err != nil {
			return host.NodeIPChoice{}, err
		}
		if err := a.BeforeCerts(choice); err != nil {
			return host.NodeIPChoice{}, err
		}
		return choice, nil
	}
	st, err := PrepareController(&ClusterConfig{}, "", holdDataLock(t))
	if err != nil {
		t.Fatalf("PrepareController: %v", err)
	}
	if req.Hostname != want || st.Hostname != want || st.EtcdName != want {
		t.Errorf("hostname %q prepared as assembly %q, node %q, etcd member %q; want %q for all three",
			hostname, req.Hostname, st.Hostname, st.EtcdName, want)
	}
	onDisk, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Hostname != want {
		t.Errorf("node.yaml saved node name %q, want %q", onDisk.Hostname, want)
	}

	dir := t.TempDir()
	args := kubeletProcess(st, func(n string) string { return filepath.Join(dir, "pki", n) },
		func(n string) string { return filepath.Join(dir, n) }).Args
	if !slices.Contains(args, "--hostname-override="+want) {
		t.Errorf("kubelet args %v; want --hostname-override=%s", args, want)
	}

	// The certificate inputs the assembly issues from are the request's.
	var kubelet stratumpki.CertSpec
	for _, s := range stratumpki.LeafSpecs(req.Hostname, choice.IP, nil) {
		if s.Name == "kubelet" {
			kubelet = s
		}
	}
	if kubelet.CN != "system:node:"+want || !slices.Contains(kubelet.DNS, want) {
		t.Errorf("kubelet leaf CN %q DNS %v; want CN system:node:%s with DNS SAN %s", kubelet.CN, kubelet.DNS, want, want)
	}
}

// A worker join derives its node name the same way: lowercased, saved so, and
// what the kubelet flag and the label and taint checks name.
func TestWorkerJoinUsesTheLowercaseNodeName(t *testing.T) {
	const hostname, want = "HK-k8s-wrk-01", "hk-k8s-wrk-01"
	redirectJoinDirs(t)
	stubJoinHost(t, "", "10.70.0.9", []byte("-----BEGIN CERTIFICATE-----\nthe discovered CA\n-----END CERTIFICATE-----\n"))
	hostHostname = func() (string, error) { return host.NodeNameOf(hostname) }
	recordTrust(t)
	st, err := PrepareWorker(context.Background(), WorkerOptions{Server: "https://cp-1.example.test:6443", Token: "tok", CAHash: "sha256:11"})
	if err != nil {
		t.Fatalf("PrepareWorker = %v", err)
	}
	if st.Hostname != want || mustLoadState(t).Hostname != want {
		t.Errorf("hostname %q joined as %q (saved %q), want %q", hostname, st.Hostname, mustLoadState(t).Hostname, want)
	}
}

// A worker an earlier release joined under a mixed-case hostname saved that
// name, while its kubelet registered the Node lowercased. A restart, which
// runs under the saved name rather than re-reading the hostname, saves it as
// the kubelet registered it, so the label and taint checks find the Node.
func TestWorkerRestartSavesAMixedCaseNodeNameAsRegistered(t *testing.T) {
	recordTrust(t)
	workerState(t, &nodestate.NodeState{Role: "worker", Hostname: "HK-k8s-wrk-01", NodeIP: "192.0.2.8",
		KubeletNodeIP: "192.0.2.8", Server: "https://10.0.0.1:6443", ClusterDNSIP: "10.96.0.10"})
	st, err := PrepareWorker(context.Background(), WorkerOptions{})
	if err != nil {
		t.Fatalf("PrepareWorker = %v", err)
	}
	saved := mustLoadState(t)
	if st.Hostname != "hk-k8s-wrk-01" || saved.Hostname != "hk-k8s-wrk-01" {
		t.Errorf("restart runs as %q and saved %q, want hk-k8s-wrk-01 for both", st.Hostname, saved.Hostname)
	}
	if saved.ClusterDNSIP != "10.96.0.10" || saved.KubeletNodeIP != "192.0.2.8" {
		t.Errorf("the rename save dropped other fields: %+v", saved)
	}
}

// The preparation seams read the node name, not some other spelling of the
// hostname: the tests above swap hostHostname, so this pins its default.
func TestHostHostnameIsTheNodeName(t *testing.T) {
	if reflect.ValueOf(hostHostname).Pointer() != reflect.ValueOf(host.NodeName).Pointer() {
		t.Error("hostHostname is not host.NodeName")
	}
}

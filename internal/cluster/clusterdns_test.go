package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/operator"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/version"
)

var upstreams int

func stubClusterDNS(t *testing.T, ip string, fetchErr error) (saved, rewrote *[]string, fetched *int) {
	t.Helper()
	redirectNodeStateAndLock(t)
	oldFetch, oldSave, oldWrite, oldUpstream := fetchClusterDNSIP, saveState, writeKubeletConf, writeCoreDNSUpstream
	t.Cleanup(func() {
		fetchClusterDNSIP, saveState, writeKubeletConf, writeCoreDNSUpstream = oldFetch, oldSave, oldWrite, oldUpstream
	})
	saved, rewrote, fetched = &[]string{}, &[]string{}, new(int)
	upstreams = 0
	writeCoreDNSUpstream = func(*NodeState) error { upstreams++; return nil }
	fetchClusterDNSIP = func() (string, error) { *fetched++; return ip, fetchErr }
	saveState = func(st *NodeState) error { *saved = append(*saved, st.ClusterDNSIP); return nil }
	writeKubeletConf = func(st *NodeState) error { *rewrote = append(*rewrote, st.ClusterDNSIP); return nil }
	return saved, rewrote, fetched
}

func TestPrepareWorkerKubelet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ip      string
		fetch   error
		st      *NodeState
		want    string
		fetches int
		saves   int
	}{
		{"a worker on a range without 10.96.0.10", "10.43.0.10", nil,
			&NodeState{Role: "worker", Hostname: "w1"}, "10.43.0.10", 1, 1},
		{"already right: the default range", netdefault.DNSClusterIP, nil,
			&NodeState{Role: "worker"}, "", 1, 0},
		{"already learned", "10.43.0.10", nil,
			&NodeState{Role: "worker", ClusterDNSIP: "10.43.0.10"}, "10.43.0.10", 1, 0},
		{"the cluster cannot be reached", "", errors.New("connection refused"),
			&NodeState{Role: "worker", ClusterDNSIP: "10.43.0.10"}, "10.43.0.10", 1, 0},
		{"a recorded range is not asked", "10.99.0.10", nil,
			&NodeState{Role: "worker", ServiceCIDR: "10.43.0.0/16"}, "", 0, 0},
		{"a control plane is not asked", "10.99.0.10", nil,
			&NodeState{Role: "controller"}, "", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved, rewrote, fetched := stubClusterDNS(t, tc.ip, tc.fetch)
			if err := prepareWorkerKubelet(tc.st); err != nil {
				t.Fatal(err)
			}
			if tc.st.ClusterDNSIP != tc.want {
				t.Errorf("ClusterDNSIP = %q, want %q", tc.st.ClusterDNSIP, tc.want)
			}
			if len(*rewrote) != 1 || (*rewrote)[0] != tc.want {
				t.Errorf("kubelet.conf rendered %q, want once with %q", *rewrote, tc.want)
			}
			if upstreams != 1 {
				t.Errorf("the CoreDNS upstream was written %d times, want once per start", upstreams)
			}
			if *fetched != tc.fetches || len(*saved) != tc.saves {
				t.Errorf("read the cluster %d times and saved %q, want %d reads and %d saves", *fetched, *saved, tc.fetches, tc.saves)
			}
		})
	}
}

func TestLearnClusterDNSStartsFromTheKubeletsAddress(t *testing.T) {
	st := &NodeState{Role: "worker"}
	kubelet, err := runtime.ClusterDNSIP(st)
	if err != nil {
		t.Fatal(err)
	}
	_, _, fetched := stubClusterDNS(t, kubelet, nil)
	if learnClusterDNS(st) {
		t.Errorf("the kube-dns Service at %s, the address the kubelet already renders, read as a change to %q", kubelet, st.ClusterDNSIP)
	}
	if st.ClusterDNSIP != "" {
		t.Errorf("ClusterDNSIP = %q, want none recorded: the kubelet already renders %s", st.ClusterDNSIP, kubelet)
	}
	if *fetched != 1 {
		t.Errorf("read the cluster %d times, want once", *fetched)
	}
}

func TestPrepareWorkerKubeletWritesKubeletConfBeforeTheState(t *testing.T) {
	saved, _, _ := stubClusterDNS(t, "10.43.0.10", nil)
	writeKubeletConf = func(*NodeState) error { return errors.New("read-only filesystem") }
	if err := prepareWorkerKubelet(&NodeState{Role: "worker"}); err == nil {
		t.Fatal("the worker start went on with kubelet.conf unwritten")
	}
	if len(*saved) != 0 {
		t.Errorf("the state recorded %q although kubelet.conf was not rewritten", *saved)
	}
}

func TestPrepareWorkerKubeletStopsOnAnUnwrittenUpstream(t *testing.T) {
	_, rewrote, _ := stubClusterDNS(t, netdefault.DNSClusterIP, nil)
	writeCoreDNSUpstream = func(*NodeState) error { return errors.New("read-only filesystem") }
	if err := prepareWorkerKubelet(&NodeState{Role: "worker"}); err == nil {
		t.Error("the worker start went on with the CoreDNS upstream unwritten")
	}
	if len(*rewrote) != 0 {
		t.Errorf("kubelet.conf rendered %q after the upstream failed", *rewrote)
	}
}

func TestCoreDNSServesTheKubeletDNSAddress(t *testing.T) {
	orig := version.ManifestsDir
	t.Cleanup(func() { version.ManifestsDir = orig })
	for _, tc := range []struct {
		name, serviceCIDR, want string
	}{
		{"unset", "", "10.96.0.10"},
		{"the default range", "10.96.0.0/12", "10.96.0.10"},
		{"a custom range", "10.43.0.0/16", "10.43.0.10"},
		{"a dual-stack range", "10.43.0.0/16,fd00:43::/108", "10.43.0.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version.ManifestsDir = t.TempDir()
			var recorded *operator.ClusterInputs
			saved := operatorRecordStaged
			operatorRecordStaged = func(_ context.Context, staged operator.StagedGeneration, previous string) (operator.StagingPublication, error) {
				cp := staged.Inputs
				recorded = &cp
				return operator.StagingPublication{Generation: 1, ID: staged.PublicationID}, nil
			}
			t.Cleanup(func() { operatorRecordStaged = saved })
			st := &NodeState{Role: "controller", Hostname: "cp1",
				ServiceCIDR: tc.serviceCIDR, ClusterDomain: "k8s.example.internal"}
			if err := writeBundles(&ClusterConfig{}, st); err != nil {
				t.Fatal(err)
			}
			if recorded == nil || recorded.DNSAddress != tc.want {
				got := ""
				if recorded != nil {
					got = recorded.DNSAddress
				}
				t.Errorf("the cluster-inputs record holds DNSAddress %q, want the kubelet's clusterDNS %q", got, tc.want)
			}
			// The Corefile serves the record's domain, not the default.
			if recorded != nil && recorded.ClusterDomain != "k8s.example.internal" {
				t.Errorf("the cluster-inputs record holds ClusterDomain %q, want the node's k8s.example.internal, not the default %q", recorded.ClusterDomain, netdefault.ClusterDomain)
			}
			b, err := os.ReadFile(filepath.Join(version.ManifestsDir, "coredns", "coredns.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			served := false
			for _, o := range decodeStack(t, b) {
				if o.GetKind() == "Service" && o.GetName() == "kube-dns" {
					served = true
					if got, _, _ := unstructured.NestedString(o.Object, "spec", "clusterIP"); got != addons.DNSAddressSentinel {
						t.Errorf("the kube-dns Service is staged at %q, want the %s sentinel the deployer fills at apply time", got, addons.DNSAddressSentinel)
					}
				}
			}
			if !served {
				t.Error("the coredns stack does not declare the kube-dns Service")
			}
		})
	}
}

func TestCoreDNSServesTheRecordedDNSAddress(t *testing.T) {
	orig := version.ManifestsDir
	t.Cleanup(func() { version.ManifestsDir = orig })
	for _, tc := range []struct {
		name               string
		serviceCIDR, dnsIP string
		want               string
	}{
		{"a recorded address does not move a recorded range", "10.43.0.0/16", "10.99.0.10", "10.43.0.10"},
		{"a recorded address without a range is served", "", "10.43.0.10", "10.43.0.10"},
		{"neither is the default", "", "", "10.96.0.10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version.ManifestsDir = t.TempDir()
			var recorded *operator.ClusterInputs
			oldRecord := operatorRecordStaged
			operatorRecordStaged = func(_ context.Context, staged operator.StagedGeneration, previous string) (operator.StagingPublication, error) {
				inputs := staged.Inputs
				recorded = &inputs
				return operator.StagingPublication{Generation: 1, ID: staged.PublicationID}, nil
			}
			t.Cleanup(func() { operatorRecordStaged = oldRecord })
			st := &NodeState{Role: "controller", Hostname: "cp1", ServiceCIDR: tc.serviceCIDR,
				ClusterDNSIP: tc.dnsIP, ClusterDomain: "k8s.example.internal"}
			if err := writeBundles(&ClusterConfig{}, st); err != nil {
				t.Fatal(err)
			}
			if recorded == nil || recorded.DNSAddress != tc.want {
				t.Fatalf("recorded DNS = %v, want %s", recorded, tc.want)
			}
			// The Corefile serves the record's domain, not the default.
			if recorded.ClusterDomain != "k8s.example.internal" {
				t.Errorf("the cluster-inputs record holds ClusterDomain %q, want the node's k8s.example.internal, not the default %q", recorded.ClusterDomain, netdefault.ClusterDomain)
			}
			b, err := os.ReadFile(filepath.Join(version.ManifestsDir, "coredns", "coredns.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			served := false
			for _, o := range decodeStack(t, b) {
				if o.GetKind() == "Service" && o.GetName() == "kube-dns" {
					served = true
					if got, _, _ := unstructured.NestedString(o.Object, "spec", "clusterIP"); got != addons.DNSAddressSentinel {
						t.Errorf("the kube-dns Service is staged at %q, want the cluster DNS sentinel %q", got, addons.DNSAddressSentinel)
					}
				}
			}
			if !served {
				t.Error("the coredns stack does not declare the kube-dns Service")
			}
		})
	}
}

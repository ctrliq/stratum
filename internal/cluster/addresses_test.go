package cluster

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// TestBringUpAddressesFromNodeState pins the addresses a bring-up derives from
// node state alone: the endpoint every kubeconfig targets (serverURL), and the
// certificate inputs (the netconfig's apiserver SANs and domain over
// pki.LeafSpecs). One table for the node
// shapes that differ in address facts — a single-stack node without a VIP, one
// with a VIP, a dual-stack node with that VIP, and a node on a custom range —
// so a constructor that reorders or drops a SAN, moves the endpoint, or puts
// the default service range's 10.96.0.1 in place of the node's own, fails for
// the shape it breaks.
// The apiserver IP SAN list is written out in the row rather than recomputed,
// so a change in netcfg or pki shows as a disagreement, not as a tautology.
// The subtests after the table pin which Server records ride across a restart:
// only one the kubeconfig files on disk already target — a deliberate
// ha-repoint's record, written with the files — and never the previous
// release's join URL, whose clients never targeted it.
func TestBringUpAddressesFromNodeState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		podCIDR     string
		serviceCIDR string
		vip         string
		nodeIP      string
		wantSANs    []string
	}{
		{"single-stack without a VIP", "10.245.0.0/16", "10.96.0.0/12", "", "10.0.0.11",
			[]string{"10.96.0.1", "127.0.0.1", "10.0.0.11"}},
		{"single-stack with a VIP", "10.245.0.0/16", "10.96.0.0/12", "10.0.0.10", "10.0.0.11",
			[]string{"10.96.0.1", "127.0.0.1", "10.0.0.11", "10.0.0.10"}},
		{"single-stack on a custom range", "10.42.0.0/16", "10.43.0.0/16", "", "10.0.0.11",
			[]string{"10.43.0.1", "127.0.0.1", "10.0.0.11"}},
		{"dual-stack with a VIP", "10.245.0.0/16,fd00:245::/64", "10.96.0.0/12,fd00:96::/108", "10.0.0.10", "10.0.0.11,fd00::11",
			[]string{"10.96.0.1", "127.0.0.1", "10.0.0.11", "fd00::11", "fd00:96::1", "::1", "10.0.0.10"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			netc, err := netcfg.Parse(tc.podCIDR, tc.serviceCIDR)
			if err != nil {
				t.Fatal(err)
			}
			st := &NodeState{Hostname: "cp1", NodeIP: host.Primary(tc.nodeIP),
				KubeletNodeIP: tc.nodeIP, VIP: VIP{Address: tc.vip}, ClusterDomain: netc.ClusterDomain}

			wantServer := fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)
			if tc.vip != "" {
				wantServer = fmt.Sprintf("https://%s:%d", tc.vip, version.APIServerPort)
			}
			if got := serverURL(st); got != wantServer {
				t.Errorf("serverURL() = %q, want %q", got, wantServer)
			}

			// The SAN composition the shared node assembly issues certificates
			// with: the netconfig's apiserver SANs and cluster domain.
			sans := &pki.APISANs{IPs: netc.APIServerSANs(tc.vip), Domain: netc.ClusterDomain}

			var api *pki.CertSpec
			specs := pki.LeafSpecs(st.Hostname, tc.nodeIP, sans)
			for i := range specs {
				if specs[i].Name == "apiserver" {
					api = &specs[i]
				}
			}
			if api == nil {
				t.Fatal("LeafSpecs has no apiserver leaf")
			}
			if !slices.Equal(api.IPs, tc.wantSANs) {
				t.Errorf("apiserver cert IP SANs = %v, want %v", api.IPs, tc.wantSANs)
			}
		})
	}

	// The carry across a restart. prev is the saved state, st the state this
	// start builds; the carry decides what prev.Server contributes to
	// serverURL's recorded-endpoint rule. The kubeconfig paths are redirected
	// so the fixtures below are the only kubeconfigs a run sees.
	origKubeconfigDir, origAdminConf := version.KubeconfigDir, version.AdminConf
	t.Cleanup(func() { version.KubeconfigDir, version.AdminConf = origKubeconfigDir, origAdminConf })
	version.KubeconfigDir = t.TempDir()
	version.AdminConf = filepath.Join(version.KubeconfigDir, "admin.conf")
	writeKubeconfigs := func(server string) {
		t.Helper()
		const tpl = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
  name: %s
contexts:
- context:
    cluster: %s
    user: admin
  name: default
current-context: default
preferences: {}
users:
- name: admin
  user: {}
`
		data := []byte(fmt.Sprintf(tpl, server, version.ClusterName, version.ClusterName))
		for _, f := range []string{"admin.kubeconfig", "controller-manager.kubeconfig", "scheduler.kubeconfig", "kubelet.kubeconfig"} {
			if err := os.WriteFile(filepath.Join(version.KubeconfigDir, f), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(version.AdminConf, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	joinURL := fmt.Sprintf("https://10.0.0.3:%d", version.APIServerPort)
	vipURL := fmt.Sprintf("https://10.0.0.10:%d", version.APIServerPort)
	localURL := fmt.Sprintf("https://127.0.0.1:%d", version.APIServerPort)

	t.Run("a legacy join record the kubeconfigs do not target is not carried", func(t *testing.T) {
		writeKubeconfigs(vipURL)
		prev := &NodeState{Server: joinURL, VIP: VIP{Address: "10.0.0.10"}}
		st := &NodeState{VIP: VIP{Address: "10.0.0.10"}}
		st.Server = recordedServer(prev, st)
		st.Server = serverURL(st)
		if st.Server != vipURL {
			t.Errorf("endpoint = %q, want the derived VIP %q: the stale join record %q must not reach the kubeconfigs", st.Server, vipURL, joinURL)
		}
	})

	t.Run("a legacy join record on a node without a VIP falls back to the local apiserver", func(t *testing.T) {
		writeKubeconfigs(localURL)
		prev := &NodeState{Server: joinURL}
		st := &NodeState{}
		st.Server = recordedServer(prev, st)
		st.Server = serverURL(st)
		if st.Server != localURL {
			t.Errorf("endpoint = %q, want the derived local apiserver %q", st.Server, localURL)
		}
	})

	t.Run("a ha-repoint record the kubeconfigs target is carried", func(t *testing.T) {
		writeKubeconfigs(vipURL)
		prev := &NodeState{Server: vipURL, VIP: VIP{Address: "10.0.0.10"}}
		st := &NodeState{VIP: VIP{Address: "10.0.0.10"}}
		st.Server = recordedServer(prev, st)
		st.Server = serverURL(st)
		if st.Server != vipURL {
			t.Errorf("endpoint = %q, want the recorded %q: ha-repoint wrote the files and the record together, so the restart keeps it", st.Server, vipURL)
		}
	})

	t.Run("a changed spec.vip re-derives instead of carrying the record", func(t *testing.T) {
		writeKubeconfigs(vipURL)
		prev := &NodeState{Server: vipURL, VIP: VIP{Address: "10.0.0.10"}}
		st := &NodeState{VIP: VIP{Address: "10.0.0.20"}}
		st.Server = recordedServer(prev, st)
		st.Server = serverURL(st)
		want := fmt.Sprintf("https://10.0.0.20:%d", version.APIServerPort)
		if st.Server != want {
			t.Errorf("endpoint = %q, want the new VIP %q", st.Server, want)
		}
	})

	t.Run("a record with no kubeconfig on disk to back it is not carried", func(t *testing.T) {
		// Its own empty directory: the subtests above leave kubeconfigs in the
		// shared one, and a file naming another endpoint refuses the record
		// before the no-file case is ever reached.
		shared, sharedAdmin := version.KubeconfigDir, version.AdminConf
		t.Cleanup(func() { version.KubeconfigDir, version.AdminConf = shared, sharedAdmin })
		version.KubeconfigDir = t.TempDir()
		version.AdminConf = filepath.Join(version.KubeconfigDir, "admin.conf")

		prev := &NodeState{Server: joinURL, VIP: VIP{Address: "10.0.0.10"}}
		st := &NodeState{VIP: VIP{Address: "10.0.0.10"}}
		if got := recordedServer(prev, st); got != "" {
			t.Errorf("recordedServer = %q, want \"\": no kubeconfig on disk corroborates the record", got)
		}
		st.Server = recordedServer(prev, st)
		st.Server = serverURL(st)
		if st.Server != vipURL {
			t.Errorf("endpoint = %q, want the derived VIP %q: with no kubeconfigs on disk the record is uncorroborated", st.Server, vipURL)
		}
	})
}

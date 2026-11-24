package cluster

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/version"
)

func TestResolveNetConfigKeepsPersistedCIDRs(t *testing.T) {
	const pod, svc = "10.42.0.0/16", "10.43.0.0/16"
	for _, tc := range []struct {
		name, yaml string
	}{
		{"no config file", ""},
		{"config without spec.network", "spec:\n  cis: false\n"},
		{"config with only a node IP", "spec:\n  network:\n    nodeIP: 192.168.99.12\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			orig := version.NetConfigPath
			version.NetConfigPath = filepath.Join(dir, "netconfig")
			t.Cleanup(func() { version.NetConfigPath = orig })
			isolateNodeFiles(t)
			if err := os.WriteFile(version.NetConfigPath, []byte(pod+"\n"+svc+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "config.yaml")
			if tc.yaml != "" {
				if err := os.WriteFile(path, []byte(tc.yaml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for i := 1; i <= 2; i++ {
				cfg, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.Spec.Network.PodCIDR != "" || cfg.Spec.Network.ServiceCIDR != "" {
					t.Fatalf("start %d: defaults() filled spec.network: %+v", i, cfg.Spec.Network)
				}
				netc, err := resolveNetConfig(cfg.Spec.Network, nil)
				if err != nil {
					t.Fatal(err)
				}
				if netc.PodCIDR != pod || netc.ServiceCIDR != svc {
					t.Fatalf("start %d: CIDRs = %s, %s; want %s, %s", i, netc.PodCIDR, netc.ServiceCIDR, pod, svc)
				}
				b, _ := os.ReadFile(version.NetConfigPath)
				if got := string(b); got != pod+"\n"+svc+"\n" {
					t.Fatalf("start %d: netconfig = %q, want the cluster's CIDRs kept", i, got)
				}
			}
		})
	}
}

func TestResolveNetConfigFirstBootAndLaterEdit(t *testing.T) {
	orig := version.NetConfigPath
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.NetConfigPath = orig })
	isolateNodeFiles(t)

	netc, err := resolveNetConfig(Network{PodCIDR: "10.42.0.0/16"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if netc.PodCIDR != "10.42.0.0/16" || netc.ServiceCIDR != netdefault.ServiceCIDR {
		t.Fatalf("first boot = %s, %s; want 10.42.0.0/16 and the default service CIDR", netc.PodCIDR, netc.ServiceCIDR)
	}

	netc, err = resolveNetConfig(Network{PodCIDR: "10.99.0.0/16", ServiceCIDR: "10.98.0.0/16"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if netc.PodCIDR != "10.42.0.0/16" || netc.ServiceCIDR != netdefault.ServiceCIDR {
		t.Fatalf("after an edit = %s, %s; want the recorded CIDRs", netc.PodCIDR, netc.ServiceCIDR)
	}
}

func TestResolveNetConfigWarnsOnlyOnADifferingConfig(t *testing.T) {
	const pod, svc = "10.42.0.0/16", "10.43.0.0/16"
	for _, tc := range []struct {
		name string
		n    Network
		warn bool
	}{
		{"unset", Network{}, false},
		{"same as recorded", Network{PodCIDR: pod, ServiceCIDR: svc}, false},
		{"only the pod CIDR, same", Network{PodCIDR: pod}, false},
		{"explicit defaults", Network{PodCIDR: netdefault.PodCIDR, ServiceCIDR: netdefault.ServiceCIDR}, true},
		{"only the service CIDR, different", Network{ServiceCIDR: "10.98.0.0/16"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := version.NetConfigPath
			version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
			t.Cleanup(func() { version.NetConfigPath = orig })
			isolateNodeFiles(t)
			if err := os.WriteFile(version.NetConfigPath, []byte(pod+"\n"+svc+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			origErr := os.Stderr
			os.Stderr = w
			netc, rerr := resolveNetConfig(tc.n, nil)
			os.Stderr = origErr
			w.Close()
			out, _ := io.ReadAll(r)
			r.Close()

			if rerr != nil {
				t.Fatal(rerr)
			}
			if netc.PodCIDR != pod || netc.ServiceCIDR != svc {
				t.Errorf("CIDRs = %s, %s; want the recorded %s, %s", netc.PodCIDR, netc.ServiceCIDR, pod, svc)
			}
			got := string(out)
			if tc.warn {
				for _, want := range []string{"WARNING", "spec.network", "recorded CIDRs", "needs a cluster rebuild", "from another control plane", version.NetConfigPath, pod, svc} {
					if !strings.Contains(got, want) {
						t.Errorf("warning %q lacks %q", got, want)
					}
				}
			} else if got != "" {
				t.Errorf("unexpected stderr: %q", got)
			}
		})
	}
}

func TestResolveNetConfigFallsBackToNodeState(t *testing.T) {
	const pod, svc = "10.42.0.0/16", "10.43.0.0/16"
	for _, tc := range []struct {
		name string
		n    Network
	}{
		{"no spec.network", Network{}},
		{"a spec.network that differs", Network{PodCIDR: "10.99.0.0/16", ServiceCIDR: "10.98.0.0/16"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := version.NetConfigPath
			version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
			t.Cleanup(func() { version.NetConfigPath = orig })
			isolateNodeFiles(t)

			netc, err := resolveNetConfig(tc.n, &NodeState{PodCIDR: pod, ServiceCIDR: svc})
			if err != nil {
				t.Fatal(err)
			}
			if netc.PodCIDR != pod || netc.ServiceCIDR != svc {
				t.Fatalf("CIDRs = %s, %s; want the node state's %s, %s", netc.PodCIDR, netc.ServiceCIDR, pod, svc)
			}
			if b, _ := os.ReadFile(version.NetConfigPath); string(b) != pod+"\n"+svc+"\n" {
				t.Errorf("netconfig = %q, want the state's CIDRs persisted", b)
			}
		})
	}

	t.Run("state without CIDRs is a first boot", func(t *testing.T) {
		orig := version.NetConfigPath
		version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
		t.Cleanup(func() { version.NetConfigPath = orig })
		isolateNodeFiles(t)
		netc, err := resolveNetConfig(Network{PodCIDR: "10.42.0.0/16"}, &NodeState{Role: "controller"})
		if err != nil {
			t.Fatal(err)
		}
		if netc.PodCIDR != "10.42.0.0/16" || netc.ServiceCIDR != netdefault.ServiceCIDR {
			t.Fatalf("CIDRs = %s, %s; want spec.network and the default service CIDR", netc.PodCIDR, netc.ServiceCIDR)
		}
	})
}

func TestRetryAfterAFailedFirstStartKeepsTheFirstNetConfig(t *testing.T) {
	const podA, svcA = "10.42.0.0/16", "10.43.0.0/16"
	const podB, svcB = "10.99.0.0/16", "10.98.0.0/16"
	orig := version.NetConfigPath
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.NetConfigPath = orig })
	isolateNodeFiles(t)

	// The failed first start: netconfig saved, no node state.
	netc, err := resolveNetConfig(Network{PodCIDR: podA, ServiceCIDR: svcA, ClusterDomain: "a.internal"}, nil)
	if err != nil || netc.PodCIDR != podA || netc.ServiceCIDR != svcA {
		t.Fatalf("first start = %+v, %v; want CIDRs %s, %s", netc, err, podA, svcA)
	}
	if _, err := LoadState(); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("node state read = %v; a failed first start must have saved none", err)
	}

	netc, err = resolveNetConfig(Network{PodCIDR: podB, ServiceCIDR: svcB, ClusterDomain: "a.internal"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if netc.PodCIDR != podA || netc.ServiceCIDR != svcA {
		t.Fatalf("retry = %s, %s; want the first start's %s, %s", netc.PodCIDR, netc.ServiceCIDR, podA, svcA)
	}
	want := podA + "\n" + svcA + "\na.internal\n"
	if b, err := os.ReadFile(version.NetConfigPath); err != nil {
		t.Fatal(err)
	} else if got := string(b); got != want {
		t.Fatalf("netconfig = %q, want the first start's %q", got, want)
	}

	_, err = resolveNetConfig(Network{ClusterDomain: "b.internal"}, nil)
	if err == nil {
		t.Fatal("a retry naming another cluster domain was accepted")
	}
	for _, name := range []string{"a.internal", "b.internal"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal %q does not name %q", err, name)
		}
	}
	if b, err := os.ReadFile(version.NetConfigPath); err != nil {
		t.Fatal(err)
	} else if got := string(b); got != want {
		t.Errorf("netconfig = %q after the refusal, want it untouched %q", got, want)
	}
}

// resolveNetConfig reads the state and PKI dirs to tell a first start from an established node.
func isolateNodeFiles(t *testing.T) {
	t.Helper()
	origConf, origPKI := version.K8sConfDir, version.PKIDir
	version.K8sConfDir, version.PKIDir = t.TempDir(), t.TempDir()
	t.Cleanup(func() { version.K8sConfDir, version.PKIDir = origConf, origPKI })
}

// The domain is a third line only when custom, so a default-domain file stays rollback-safe.
func TestResolveNetConfigDomain(t *testing.T) {
	const pod, svc = "10.42.0.0/16", "10.43.0.0/16"
	read := func(t *testing.T) string {
		t.Helper()
		b, err := os.ReadFile(version.NetConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	setup := func(t *testing.T) {
		t.Helper()
		orig := version.NetConfigPath
		version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
		t.Cleanup(func() { version.NetConfigPath = orig })
		isolateNodeFiles(t)
	}

	t.Run("first boot takes and persists a custom domain", func(t *testing.T) {
		setup(t)
		netc, err := resolveNetConfig(Network{PodCIDR: pod, ServiceCIDR: svc, ClusterDomain: "fuzz.internal"}, nil)
		if err != nil || netc.ClusterDomain != "fuzz.internal" {
			t.Fatalf("resolveNetConfig = %+v, %v; want fuzz.internal", netc, err)
		}
		if got, want := read(t), pod+"\n"+svc+"\nfuzz.internal\n"; got != want {
			t.Errorf("netconfig = %q, want %q", got, want)
		}
	})
	t.Run("first boot with no domain is the default, in two lines", func(t *testing.T) {
		setup(t)
		netc, err := resolveNetConfig(Network{}, nil)
		if err != nil || netc.ClusterDomain != netdefault.ClusterDomain {
			t.Fatalf("resolveNetConfig = %+v, %v; want the default domain", netc, err)
		}
		if got, want := read(t), netdefault.PodCIDR+"\n"+netdefault.ServiceCIDR+"\n"; got != want {
			t.Errorf("netconfig = %q, want the two-line default %q", got, want)
		}
	})
	t.Run("a restart with no domain keeps the recorded one", func(t *testing.T) {
		setup(t)
		if err := os.WriteFile(version.NetConfigPath, []byte(pod+"\n"+svc+"\nfuzz.internal\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		netc, err := resolveNetConfig(Network{}, nil)
		if err != nil || netc.ClusterDomain != "fuzz.internal" {
			t.Fatalf("resolveNetConfig = %+v, %v; want the recorded fuzz.internal", netc, err)
		}
	})
	t.Run("a different configured domain is refused and nothing is rewritten", func(t *testing.T) {
		setup(t)
		before := pod + "\n" + svc + "\nfuzz.internal\n"
		if err := os.WriteFile(version.NetConfigPath, []byte(before), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveNetConfig(Network{ClusterDomain: "other.internal"}, nil); err == nil {
			t.Fatal("resolveNetConfig accepted a different domain")
		}
		if got := read(t); got != before {
			t.Errorf("netconfig = %q after the refusal, want it untouched %q", got, before)
		}
	})
	t.Run("a node that lost netconfig recovers the CIDRs and domain from its state", func(t *testing.T) {
		setup(t)
		st := &NodeState{Role: "controller", PodCIDR: pod, ServiceCIDR: svc, ClusterDomain: "fuzz.internal"}
		if err := SaveState(st); err != nil {
			t.Fatal(err)
		}
		netc, err := resolveNetConfig(Network{}, st)
		if err != nil || netc.PodCIDR != pod || netc.ServiceCIDR != svc || netc.ClusterDomain != "fuzz.internal" {
			t.Fatalf("resolveNetConfig = %+v, %v; want the recorded CIDRs and domain", netc, err)
		}
		if got, want := read(t), pod+"\n"+svc+"\nfuzz.internal\n"; got != want {
			t.Errorf("netconfig = %q, want it restored as %q", got, want)
		}
	})
}

package netcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

func withNetConfig(t *testing.T) {
	t.Helper()
	origNet, origConf, origPKI := version.NetConfigPath, version.K8sConfDir, version.PKIDir
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	version.K8sConfDir, version.PKIDir = t.TempDir(), t.TempDir()
	t.Cleanup(func() { version.NetConfigPath, version.K8sConfDir, version.PKIDir = origNet, origConf, origPKI })
}

// The cluster domain rides in netconfig, so HA joins, `token create` and cert
// rotation see the domain the cluster was initialised with.
func TestClusterDomainPersists(t *testing.T) {
	withNetConfig(t)
	if c, err := Load(); err != nil || c.ClusterDomain != netdefault.ClusterDomain {
		t.Fatalf("no file: domain = %q, %v; want the default", c.ClusterDomain, err)
	}
	if err := Save(netdefault.PodCIDR, netdefault.ServiceCIDR, "fuzz.internal"); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(); err != nil || c.ClusterDomain != "fuzz.internal" {
		t.Errorf("Load domain = %q, %v; want fuzz.internal", c.ClusterDomain, err)
	}
	// An HA join passes explicit CIDR overrides but still inherits the domain.
	if c, err := Resolve("10.42.0.0/16", "10.43.0.0/16"); err != nil || c.ClusterDomain != "fuzz.internal" || c.PodCIDR != "10.42.0.0/16" {
		t.Errorf("Resolve with overrides = %+v, %v; want domain fuzz.internal and the overridden CIDRs", c, err)
	}
}

// A netconfig from before the domain was recorded has two lines: a default domain.
func TestLoadTwoLineNetConfigIsTheDefaultDomain(t *testing.T) {
	withNetConfig(t)
	if err := os.WriteFile(version.NetConfigPath, []byte(netdefault.PodCIDR+"\n"+netdefault.ServiceCIDR+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(); err != nil || c.ClusterDomain != netdefault.ClusterDomain {
		t.Errorf("domain = %q, %v; want the default", c.ClusterDomain, err)
	}
}

func TestLoadRejectsABadPersistedDomain(t *testing.T) {
	withNetConfig(t)
	if err := os.WriteFile(version.NetConfigPath, []byte(netdefault.PodCIDR+"\n"+netdefault.ServiceCIDR+"\nBad Domain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "Bad Domain") {
		t.Errorf("Load = %v, want an error naming the domain", err)
	}
}

func TestValidateDomain(t *testing.T) {
	for _, d := range []string{"cluster.local", "fuzz.internal", "k8s"} {
		if err := ValidateDomain(d); err != nil {
			t.Errorf("ValidateDomain(%q) = %v, want nil", d, err)
		}
	}
	// The domain is written into kubelet.conf, so a newline or space is an injection.
	for _, d := range []string{"", "Fuzz.Internal", "a b", "a\nb: c", "-x.local", "x..local", ".local"} {
		if err := ValidateDomain(d); err == nil {
			t.Errorf("ValidateDomain(%q) = nil, want an error", d)
		}
	}
}

func writeNetConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(version.NetConfigPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A control plane that joined a cluster and restarts with no
// spec.network.clusterDomain keeps the domain it joined with.
func TestResolveDomainKeepsThePersistedDomain(t *testing.T) {
	withNetConfig(t)
	writeNetConfig(t, netdefault.PodCIDR+"\n"+netdefault.ServiceCIDR+"\nfuzz.internal\n")
	for _, configured := range []string{"", "fuzz.internal"} {
		if got, err := ResolveDomain(configured); err != nil || got != "fuzz.internal" {
			t.Errorf("ResolveDomain(%q) = %q, %v; want fuzz.internal", configured, got, err)
		}
	}
}

// A different configured domain is refused rather than half-applied, including
// cluster.local on a cluster initialised with another domain, and a custom
// domain on a cluster that predates the recorded one (a two-line netconfig).
func TestResolveDomainRefusesAMismatch(t *testing.T) {
	withNetConfig(t)
	for _, tc := range []struct{ body, configured, persisted string }{
		{netdefault.PodCIDR + "\n" + netdefault.ServiceCIDR + "\nfuzz.internal\n", "other.internal", "fuzz.internal"},
		{netdefault.PodCIDR + "\n" + netdefault.ServiceCIDR + "\nfuzz.internal\n", "cluster.local", "fuzz.internal"},
		{netdefault.PodCIDR + "\n" + netdefault.ServiceCIDR + "\n", "fuzz.internal", "cluster.local"},
	} {
		writeNetConfig(t, tc.body)
		_, err := ResolveDomain(tc.configured)
		if err == nil || !strings.Contains(err.Error(), tc.configured) || !strings.Contains(err.Error(), tc.persisted) {
			t.Errorf("ResolveDomain(%q) over %q = %v; want an error naming both domains", tc.configured, tc.persisted, err)
		}
	}
}

// With no netconfig this is a first start: the configured domain, else the default.
func TestResolveDomainFirstStart(t *testing.T) {
	withNetConfig(t)
	for configured, want := range map[string]string{"": netdefault.ClusterDomain, "fuzz.internal": "fuzz.internal"} {
		if got, err := ResolveDomain(configured); err != nil || got != want {
			t.Errorf("ResolveDomain(%q) = %q, %v; want %q", configured, got, err, want)
		}
	}
}

// An established node with no netconfig (a legacy cluster, or a lost
// file) is on the domain its node state records, else cluster.local. An unset
// or matching clusterDomain is accepted and a different one refused, rather
// than adopted as a first start.
func TestResolveDomainEstablishedNodeWithoutNetConfig(t *testing.T) {
	establish := map[string]func(t *testing.T){
		"node state records a domain": func(t *testing.T) {
			if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", ClusterDomain: "fuzz.internal"}); err != nil {
				t.Fatal(err)
			}
		},
		"node state records none": func(t *testing.T) {
			if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller"}); err != nil {
				t.Fatal(err)
			}
		},
		"only the cluster CA is on disk": func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(version.PKIDir, "ca.crt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	want := map[string]string{
		"node state records a domain":    "fuzz.internal",
		"node state records none":        netdefault.ClusterDomain,
		"only the cluster CA is on disk": netdefault.ClusterDomain,
	}
	for name, setup := range establish {
		t.Run(name, func(t *testing.T) {
			withNetConfig(t)
			setup(t)
			for _, configured := range []string{"", want[name]} { // unset, matching
				if got, err := ResolveDomain(configured); err != nil || got != want[name] {
					t.Errorf("ResolveDomain(%q) = %q, %v; want %q", configured, got, err, want[name])
				}
			}
			other := "other.internal"
			if _, err := ResolveDomain(other); err == nil || !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), want[name]) {
				t.Errorf("ResolveDomain(%q) = %v; want a refusal naming both domains", other, err)
			}
		})
	}
}

// A node state that cannot be read is not guessed at.
func TestResolveDomainRefusesAnUnreadableNodeState(t *testing.T) {
	withNetConfig(t)
	if err := os.WriteFile(nodestate.StatePath(), []byte("{not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveDomain(""); err == nil {
		t.Error("ResolveDomain over an unreadable node state = nil, want an error")
	}
}

// A netconfig that exists but is unreadable or invalid is an error from
// Resolve, with explicit CIDRs or without, rather than a silent default domain.
func TestResolveFailsOnACorruptNetConfig(t *testing.T) {
	for name, body := range map[string]string{
		"wrong line count": "just one line\n",
		"invalid domain":   netdefault.PodCIDR + "\n" + netdefault.ServiceCIDR + "\nBad Domain\n",
		"invalid CIDR":     "not-a-cidr\n" + netdefault.ServiceCIDR + "\nfuzz.internal\n",
	} {
		t.Run(name, func(t *testing.T) {
			withNetConfig(t)
			writeNetConfig(t, body)
			if _, err := Resolve("", ""); err == nil {
				t.Error("Resolve with no overrides = nil error, want one")
			}
			if c, err := Resolve("10.42.0.0/16", "10.43.0.0/16"); err == nil {
				t.Errorf("Resolve with CIDR overrides = %+v, nil error; want one, not the default domain", c)
			}
		})
	}
}

// Save never writes a netconfig whose domain Load would read back
// differently. Empty is the default, written in the two-line format (see
// TestDefaultDomainNetConfigStaysTwoLines), and an invalid domain is refused
// with nothing written.
func TestSaveNormalisesAndValidatesTheDomain(t *testing.T) {
	withNetConfig(t)
	if err := Save(netdefault.PodCIDR, netdefault.ServiceCIDR, ""); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(version.NetConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := netdefault.PodCIDR + "\n" + netdefault.ServiceCIDR + "\n"; string(b) != want {
		t.Errorf("Save with an empty domain wrote %q, want %q", b, want)
	}
	if c, err := Load(); err != nil || c.ClusterDomain != netdefault.ClusterDomain {
		t.Errorf("Load after an empty-domain Save = %q, %v", c.ClusterDomain, err)
	}

	withNetConfig(t)
	for _, bad := range []string{"Bad Domain", "a\nb", "-x"} {
		if err := Save(netdefault.PodCIDR, netdefault.ServiceCIDR, bad); err == nil || !strings.Contains(err.Error(), "cluster domain") {
			t.Errorf("Save with domain %q = %v, want a cluster domain error", bad, err)
		}
		if _, err := os.Stat(version.NetConfigPath); err == nil {
			t.Errorf("Save with domain %q wrote a netconfig", bad)
			os.Remove(version.NetConfigPath)
		}
	}
}

// Save replaces netconfig atomically. PrepareController saves on every
// start, and a write that failed partway through an in-place WriteFile left an
// empty file that every later start rejected. A failed replace keeps the
// previous valid file; a failed first write leaves the live path absent; and
// neither leaves a temp file behind. The failure is a directory that denies
// creating the temp file, as in host's InstallUnit test.
func TestSaveFailureKeepsThePreviousNetConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions this test relies on")
	}
	denyWrites := func(t *testing.T) {
		t.Helper()
		dir := filepath.Dir(version.NetConfigPath)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
	}
	allowWrites := func(t *testing.T) {
		t.Helper()
		if err := os.Chmod(filepath.Dir(version.NetConfigPath), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a failed replace keeps the previous valid file", func(t *testing.T) {
		withNetConfig(t)
		version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
		if err := Save(netdefault.PodCIDR, netdefault.ServiceCIDR, "fuzz.internal"); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(version.NetConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		denyWrites(t)
		if err := Save("10.42.0.0/16", "10.43.0.0/16", "other.internal"); err == nil {
			t.Fatal("Save succeeded in a directory that denies writes")
		}
		allowWrites(t)
		after, err := os.ReadFile(version.NetConfigPath)
		if err != nil || string(after) != string(before) {
			t.Fatalf("netconfig after the failed Save = %q, %v; want the previous %q", after, err, before)
		}
		if c, err := Load(); err != nil || c.ClusterDomain != "fuzz.internal" {
			t.Errorf("Load after the failed Save = %q, %v; want fuzz.internal", c.ClusterDomain, err)
		}
		assertNoTempFiles(t)
	})

	t.Run("a failed first write leaves the live path absent", func(t *testing.T) {
		withNetConfig(t)
		version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
		denyWrites(t)
		if err := Save(netdefault.PodCIDR, netdefault.ServiceCIDR, "fuzz.internal"); err == nil {
			t.Fatal("Save succeeded in a directory that denies writes")
		}
		allowWrites(t)
		if _, err := os.Stat(version.NetConfigPath); !os.IsNotExist(err) {
			t.Errorf("netconfig exists after a failed first Save: %v", err)
		}
		// Absent is the defaults, not an error, so the next start can recover.
		if c, err := Load(); err != nil || c.ClusterDomain != netdefault.ClusterDomain {
			t.Errorf("Load after the failed first Save = %q, %v", c.ClusterDomain, err)
		}
		assertNoTempFiles(t)
	})
}

func assertNoTempFiles(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(version.NetConfigPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(version.NetConfigPath) {
			t.Errorf("stray file %q left beside netconfig", e.Name())
		}
	}
}

// Load recovers the domain of an established node whose netconfig is
// absent, so callers that read the domain through it (rotation, the upgrade
// reissue, `token create`) never see cluster.local on a custom-domain cluster.
// A first start, with neither node state nor the CA, is still the defaults.
func TestLoadWithoutNetConfigRecoversTheDomain(t *testing.T) {
	t.Run("node state records a domain", func(t *testing.T) {
		withNetConfig(t)
		if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", ClusterDomain: "fuzz.internal"}); err != nil {
			t.Fatal(err)
		}
		if c, err := Load(); err != nil || c.ClusterDomain != "fuzz.internal" || c.PodCIDR != netdefault.PodCIDR {
			t.Errorf("Load = %+v, %v; want the default CIDRs and fuzz.internal", c, err)
		}
	})
	t.Run("node state records none, or only the CA is on disk", func(t *testing.T) {
		withNetConfig(t)
		if err := os.WriteFile(filepath.Join(version.PKIDir, "ca.crt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if c, err := Load(); err != nil || c.ClusterDomain != netdefault.ClusterDomain {
			t.Errorf("Load = %q, %v; want the default domain", c.ClusterDomain, err)
		}
	})
	t.Run("first start is the defaults", func(t *testing.T) {
		withNetConfig(t)
		c, err := Load()
		if err != nil || c.ClusterDomain != netdefault.ClusterDomain || c.PodCIDR != netdefault.PodCIDR || c.ServiceCIDR != netdefault.ServiceCIDR {
			t.Errorf("Load = %+v, %v; want the defaults", c, err)
		}
	})
	t.Run("an unreadable node state is an error, not a guess", func(t *testing.T) {
		withNetConfig(t)
		if err := os.WriteFile(nodestate.StatePath(), []byte("{not yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Error("Load over an unreadable node state = nil, want an error")
		}
	})
	t.Run("an existing netconfig stays authoritative over node state", func(t *testing.T) {
		withNetConfig(t)
		if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", ClusterDomain: "state.internal"}); err != nil {
			t.Fatal(err)
		}
		if err := Save(netdefault.PodCIDR, netdefault.ServiceCIDR, "file.internal"); err != nil {
			t.Fatal(err)
		}
		if c, err := Load(); err != nil || c.ClusterDomain != "file.internal" {
			t.Errorf("Load = %q, %v; want file.internal", c.ClusterDomain, err)
		}
	})
}

// The binary before the cluster domain was persisted reads exactly two
// lines, and PrepareController rewrites netconfig through Save on every start.
// A default-domain cluster must therefore keep the two-line file, or a rollback
// after an upgrade leaves it unable to start. Only a custom domain adds a third
// line.
func TestDefaultDomainNetConfigStaysTwoLines(t *testing.T) {
	withNetConfig(t)
	read := func() string {
		b, err := os.ReadFile(version.NetConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, domain := range []string{"", netdefault.ClusterDomain} {
		if err := Save("10.42.0.0/16", "10.43.0.0/16", domain); err != nil {
			t.Fatal(err)
		}
		if got, want := read(), "10.42.0.0/16\n10.43.0.0/16\n"; got != want || strings.Count(got, "\n") != 2 {
			t.Errorf("Save with domain %q wrote %q, want exactly two lines %q", domain, got, want)
		}
	}
	if err := Save("10.42.0.0/16", "10.43.0.0/16", "fuzz.internal"); err != nil {
		t.Fatal(err)
	}
	if got, want := read(), "10.42.0.0/16\n10.43.0.0/16\nfuzz.internal\n"; got != want {
		t.Errorf("Save with a custom domain wrote %q, want %q", got, want)
	}

	// A legacy two-line file round-trips unchanged: Load then Encode (what Save
	// and the shared-cert bundle write) gives the same bytes.
	legacy := "10.42.0.0/16\n10.43.0.0/16\n"
	writeNetConfig(t, legacy)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(c.Encode()); got != legacy {
		t.Errorf("legacy file round-trips as %q, want %q", got, legacy)
	}
	if err := Save(c.PodCIDR, c.ServiceCIDR, c.ClusterDomain); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != legacy {
		t.Errorf("Save of a loaded legacy file wrote %q, want it unchanged", got)
	}
}

// With netconfig lost, Load recovers the pod and
// service ranges from node state along with the domain, validated through Parse,
// and falls back to the default field by field only where the state has none.
func TestLoadWithoutNetConfigRecoversTheRanges(t *testing.T) {
	save := func(t *testing.T, st *nodestate.NodeState) {
		t.Helper()
		withNetConfig(t)
		if err := nodestate.SaveState(st); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("custom ranges and domain", func(t *testing.T) {
		save(t, &nodestate.NodeState{Role: "controller", PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16", ClusterDomain: "fuzz.internal"})
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.PodCIDR != "10.42.0.0/16" || c.ServiceCIDR != "10.43.0.0/16" || c.ClusterDomain != "fuzz.internal" {
			t.Errorf("Load = %q %q %q, want the recorded ranges and domain", c.PodCIDR, c.ServiceCIDR, c.ClusterDomain)
		}
		if c.ServiceIPv4 != "10.43.0.1" || c.DNSIPv4 != "10.43.0.10" {
			t.Errorf("derived service IPs = %s, %s; want them from the recorded service range", c.ServiceIPv4, c.DNSIPv4)
		}
	})
	t.Run("dual-stack ranges", func(t *testing.T) {
		save(t, &nodestate.NodeState{Role: "controller", PodCIDR: "10.42.0.0/16,fd00:42::/56", ServiceCIDR: "10.43.0.0/16,fd00:43::/112"})
		c, err := Load()
		if err != nil || !c.DualStack || c.ServiceIPv6 != "fd00:43::1" {
			t.Errorf("Load = %+v, %v; want dual-stack from the recorded ranges", c, err)
		}
	})
	t.Run("legacy state with no ranges falls back to the defaults", func(t *testing.T) {
		save(t, &nodestate.NodeState{Role: "controller", ClusterDomain: "fuzz.internal"})
		c, err := Load()
		if err != nil || c.PodCIDR != netdefault.PodCIDR || c.ServiceCIDR != netdefault.ServiceCIDR || c.ClusterDomain != "fuzz.internal" {
			t.Errorf("Load = %+v, %v; want the default ranges and the recorded domain", c, err)
		}
	})
	t.Run("one recorded range, the other defaulted field by field", func(t *testing.T) {
		save(t, &nodestate.NodeState{Role: "controller", ServiceCIDR: "10.43.0.0/16"})
		c, err := Load()
		if err != nil || c.PodCIDR != netdefault.PodCIDR || c.ServiceCIDR != "10.43.0.0/16" {
			t.Errorf("Load = %q %q, %v; want the default pod range and the recorded service range", c.PodCIDR, c.ServiceCIDR, err)
		}
	})
	t.Run("an invalid recorded range is an error, not the default", func(t *testing.T) {
		save(t, &nodestate.NodeState{Role: "controller", PodCIDR: "not-a-cidr", ServiceCIDR: "10.43.0.0/16"})
		if c, err := Load(); err == nil {
			t.Errorf("Load = %+v, nil error; want the recorded range refused", c)
		}
	})
	t.Run("an existing netconfig stays authoritative over node state", func(t *testing.T) {
		save(t, &nodestate.NodeState{Role: "controller", PodCIDR: "10.1.0.0/16", ServiceCIDR: "10.2.0.0/16"})
		if err := Save("10.42.0.0/16", "10.43.0.0/16", ""); err != nil {
			t.Fatal(err)
		}
		if c, err := Load(); err != nil || c.PodCIDR != "10.42.0.0/16" || c.ServiceCIDR != "10.43.0.0/16" {
			t.Errorf("Load = %q %q, %v; want the file's ranges", c.PodCIDR, c.ServiceCIDR, err)
		}
	})
}

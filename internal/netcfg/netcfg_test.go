package netcfg

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/version"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name        string
		pod, svc    string
		wantDual    bool
		wantV6SvcIP string
		wantErr     string // substring; "" = no error
	}{
		{
			name: "single-stack ipv4 (default)",
			pod:  "10.245.0.0/16", svc: "10.96.0.0/12",
			wantDual: false,
		},
		{
			name: "dual-stack ipv4-primary",
			pod:  "10.245.0.0/16,fd00:245::/56", svc: "10.96.0.0/12,fd00:96::/108",
			wantDual: true, wantV6SvcIP: "fd00:96::1",
		},
		{
			name: "mismatched family count",
			pod:  "10.245.0.0/16,fd00:245::/56", svc: "10.96.0.0/12",
			wantErr: "same number of families",
		},
		{
			name: "ipv6 listed first is rejected",
			pod:  "fd00:245::/56,10.245.0.0/16", svc: "fd00:96::/108,10.96.0.0/12",
			wantErr: "IPv4 CIDR first",
		},
		{
			name: "invalid cidr",
			pod:  "10.245.0.0/16", svc: "not-a-cidr",
			wantErr: "invalid CIDR",
		},
		{
			name: "too many families",
			pod:  "10.245.0.0/16,fd00:245::/56,fd00:246::/56", svc: "10.96.0.0/12,fd00:96::/108",
			wantErr: "1 or 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse(tt.pod, tt.svc)
			if tt.wantErr != "" {
				if err == nil || !contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.DualStack != tt.wantDual {
				t.Errorf("DualStack = %v, want %v", c.DualStack, tt.wantDual)
			}
			if c.ServiceIPv6 != tt.wantV6SvcIP {
				t.Errorf("ServiceIPv6 = %q, want %q", c.ServiceIPv6, tt.wantV6SvcIP)
			}
			if c.PodCIDR != tt.pod || c.ServiceCIDR != tt.svc {
				t.Errorf("verbatim CIDRs not preserved: %+v", c)
			}
		})
	}
}

func TestFirstHost(t *testing.T) {
	for cidr, want := range map[string]string{
		"fd00:96::/108": "fd00:96::1",
		"10.96.0.0/12":  "10.96.0.1",
		"2001:db8::/64": "2001:db8::1",
	} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		if got := firstHost(n); got != want {
			t.Errorf("firstHost(%s) = %q, want %q", cidr, got, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Install, join and renewal share this list, so the VIP is in every cert or none.
func TestAPIServerSANsCarryTheVIP(t *testing.T) {
	for _, cidrs := range [][2]string{
		{"10.245.0.0/16", "10.96.0.0/12"},
		{"10.245.0.0/16,fd00:10:245::/56", "10.96.0.0/12,fd00:10:96::/112"},
	} {
		c, err := Parse(cidrs[0], cidrs[1])
		if err != nil {
			t.Fatal(err)
		}
		base := c.APIServerExtraIPs()
		if got := c.APIServerSANs(""); !slices.Equal(got, base) {
			t.Errorf("%v, no VIP = %q, want exactly %q", cidrs, got, base)
		}
		if got := c.APIServerSANs("192.168.99.10"); !slices.Equal(got, append(slices.Clone(base), "192.168.99.10")) {
			t.Errorf("%v with VIP = %q, want %q then the VIP", cidrs, got, base)
		}
	}
}

// The file is authoritative once it exists: every Save still leaves 0644.
func TestSaveReplacesKeepsModeAndAuthority(t *testing.T) {
	path := useTempNetConfig(t)
	if err := Save("10.42.0.0/16", "10.43.0.0/16", ""); err != nil {
		t.Fatal(err)
	}
	if err := Save("10.52.0.0/16", "10.53.0.0/16", ""); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("netconfig mode after replacing it = %v, want 0644", fi.Mode().Perm())
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PodCIDR != "10.52.0.0/16" || c.ServiceCIDR != "10.53.0.0/16" {
		t.Errorf("Load = %s / %s, want the saved values — the file is authoritative", c.PodCIDR, c.ServiceCIDR)
	}
}

// Points the etcd data dir at an empty temporary one, instead of the host's.
func withEtcdDataDir(t *testing.T) {
	t.Helper()
	orig := version.EtcdDataDir
	version.EtcdDataDir = filepath.Join(t.TempDir(), "etcd")
	t.Cleanup(func() { version.EtcdDataDir = orig })
}

// Only its presence is establishment; no network value is read from it.
func establishEtcdData(t *testing.T) {
	t.Helper()
	path := filepath.Join(version.EtcdDataDir, "member", "snap", "db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stratum test: retained etcd member data"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Retained member data is establishment even with node.yaml and the CA lost.
func TestRetainedEtcdDataIsEstablishmentForTheDomain(t *testing.T) {
	withNetConfig(t)
	withEtcdDataDir(t)
	establishEtcdData(t)

	if c, err := Load(); err != nil || c.PodCIDR != netdefault.PodCIDR || c.ServiceCIDR != netdefault.ServiceCIDR || c.ClusterDomain != netdefault.ClusterDomain {
		t.Errorf("Load over retained etcd data = %+v, %v; want the defaults without error", c, err)
	}
	for _, configured := range []string{"", netdefault.ClusterDomain} {
		if got, err := ResolveDomain(configured); err != nil || got != netdefault.ClusterDomain {
			t.Errorf("ResolveDomain(%q) = %q, %v; want the default domain", configured, got, err)
		}
	}
	if _, err := ResolveDomain("other.internal"); err == nil || !strings.Contains(err.Error(), "other.internal") || !strings.Contains(err.Error(), netdefault.ClusterDomain) {
		t.Errorf("ResolveDomain(%q) over retained etcd data = %v; want a refusal naming both %q and %q",
			"other.internal", err, "other.internal", netdefault.ClusterDomain)
	}
}

// A WAL without the database is still history, so a changed domain is still refused.
func TestWALOnlyMemberDataIsEstablishmentForTheDomain(t *testing.T) {
	withNetConfig(t)
	withEtcdDataDir(t)
	establishWALOnly(t)

	if c, err := Load(); err != nil || c.ClusterDomain != netdefault.ClusterDomain {
		t.Errorf("Load over WAL-only member data = %+v, %v; want the defaults without error", c, err)
	}
	if _, err := ResolveDomain("other.internal"); err == nil || !strings.Contains(err.Error(), netdefault.ClusterDomain) {
		t.Errorf("ResolveDomain(%q) over WAL-only member data = %v; want a refusal naming %q",
			"other.internal", err, netdefault.ClusterDomain)
	}
}

// The shape of a member that lost its database: only a WAL segment remains.
func establishWALOnly(t *testing.T) {
	t.Helper()
	wal := filepath.Join(version.EtcdDataDir, "member", "wal")
	if err := os.MkdirAll(wal, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wal, "0000000000000000-0000000000000000.wal"), []byte("w"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An evidence directory that cannot be inspected is an error, never a fresh answer.
func TestRetainedMemberDataRefusesWhatItCannotDecide(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "member", "snap"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A file where the check must ReadDir, so the read is refused, not answered.
	if err := os.WriteFile(filepath.Join(dir, "member", "wal"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RetainedMemberData(dir); err == nil {
		t.Error("RetainedMemberData answered a WAL directory it could not inspect; it must refuse")
	}
}

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/token"
	"github.com/ctrliq/stratum/internal/version"
)

// TestWorkerJoinLineCarriesANonDefaultServiceCIDR pins the worker half of the service
// range: a worker learns the cluster's service range only from its join line,
// so on a custom range `token create` must print --service-cidr — after the CA
// pin, so the partial-write accounting of the token stays where it was — and on
// a default cluster the line must be exactly what it always was.
func TestWorkerJoinLineCarriesANonDefaultServiceCIDR(t *testing.T) {
	orig := version.NetConfigPath
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.NetConfigPath = orig })

	if got, err := workerJoinExtra(); err != nil || got != "" {
		t.Errorf("no netconfig (a default cluster): extra = %q, %v; want none", got, err)
	}
	write := func(pod, svc string) {
		t.Helper()
		if err := os.WriteFile(version.NetConfigPath, []byte(pod+"\n"+svc+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(netdefault.PodCIDR, netdefault.ServiceCIDR)
	if got, err := workerJoinExtra(); err != nil || got != "" {
		t.Errorf("default range recorded: extra = %q, %v; want none", got, err)
	}
	write(netdefault.PodCIDR, "10.43.0.0/16")
	if got, err := workerJoinExtra(); err != nil || got != " --service-cidr '10.43.0.0/16'" {
		t.Errorf("custom service range only: extra = %q, %v", got, err)
	}
	// A custom pod range reaches the worker too and remains in its state for
	// compatibility with earlier builds.
	write("10.42.0.0/16", netdefault.ServiceCIDR)
	if got, err := workerJoinExtra(); err != nil || got != " --pod-cidr '10.42.0.0/16'" {
		t.Errorf("custom pod range only: extra = %q, %v", got, err)
	}
	write("10.42.0.0/16", "10.43.0.0/16")
	extra, err := workerJoinExtra()
	if err != nil || extra != " --pod-cidr '10.42.0.0/16' --service-cidr '10.43.0.0/16'" {
		t.Fatalf("custom ranges: extra = %q", extra)
	}

	var out bytes.Buffer
	var revoked []string
	c := okCmd(&out, &revoked)
	c.workerExtra = extra
	if err := createToken("https://cp:6443", false, token.DefaultTokenTTL, c); err != nil {
		t.Fatal(err)
	}
	line := ""
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "stratum up --worker") {
			line = l
		}
	}
	if !strings.HasSuffix(line, "--discovery-token-ca-cert-hash sha256:aa --pod-cidr '10.42.0.0/16' --service-cidr '10.43.0.0/16'") {
		t.Errorf("worker join line = %q; want --pod-cidr and --service-cidr right after the CA pin", line)
	}
}

// TestWorkerJoinExtraRefusesAnUnreadableNetconfig: a netconfig
// that exists but cannot be read or parsed is not a default cluster, and
// treating it as one printed a worker join line without --service-cidr.
func TestWorkerJoinExtraRefusesAnUnreadableNetconfig(t *testing.T) {
	orig := version.NetConfigPath
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.NetConfigPath = orig })
	if err := os.WriteFile(version.NetConfigPath, []byte("10.42.0.0/16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := workerJoinExtra(); err == nil {
		t.Errorf("malformed netconfig: extra = %q with no error; the join line would silently omit --service-cidr", got)
	}
}

// A worker learns a non-default cluster domain only from its join line.
func TestWorkerJoinLineCarriesANonDefaultClusterDomain(t *testing.T) {
	orig := version.NetConfigPath
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	t.Cleanup(func() { version.NetConfigPath = orig })
	body := netdefault.PodCIDR + "\n" + netdefault.ServiceCIDR + "\n"
	for domain, want := range map[string]string{
		netdefault.ClusterDomain: "",
		"fuzz.internal":          " --cluster-domain 'fuzz.internal'",
	} {
		if err := os.WriteFile(version.NetConfigPath, []byte(body+domain+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := workerJoinExtra(); err != nil || got != want {
			t.Errorf("domain %q: extra = %q, %v; want %q", domain, got, err, want)
		}
	}
}

// `token create` on a custom-domain control plane that lost netconfig
// still prints --cluster-domain, recovered from node state.
func TestWorkerJoinLineKeepsTheClusterDomainWithoutNetConfig(t *testing.T) {
	origNet, origConf, origPKI := version.NetConfigPath, version.K8sConfDir, version.PKIDir
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig") // never written: lost
	version.K8sConfDir, version.PKIDir = t.TempDir(), t.TempDir()
	t.Cleanup(func() { version.NetConfigPath, version.K8sConfDir, version.PKIDir = origNet, origConf, origPKI })
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", ClusterDomain: "fuzz.internal"}); err != nil {
		t.Fatal(err)
	}
	if got, err := workerJoinExtra(); err != nil || got != " --cluster-domain 'fuzz.internal'" {
		t.Errorf("extra = %q, %v; want --cluster-domain 'fuzz.internal'", got, err)
	}
}

// `token create` on a custom-range, custom-domain
// control plane that lost netconfig still prints the recorded ranges, not the
// defaults.
func TestWorkerJoinLineKeepsTheRangesWithoutNetConfig(t *testing.T) {
	origNet, origConf, origPKI := version.NetConfigPath, version.K8sConfDir, version.PKIDir
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig") // never written: lost
	version.K8sConfDir, version.PKIDir = t.TempDir(), t.TempDir()
	t.Cleanup(func() { version.NetConfigPath, version.K8sConfDir, version.PKIDir = origNet, origConf, origPKI })
	if err := nodestate.SaveState(&nodestate.NodeState{
		Role: "controller", PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16", ClusterDomain: "fuzz.internal",
	}); err != nil {
		t.Fatal(err)
	}
	want := " --pod-cidr '10.42.0.0/16' --service-cidr '10.43.0.0/16' --cluster-domain 'fuzz.internal'"
	if got, err := workerJoinExtra(); err != nil || got != want {
		t.Errorf("extra = %q, %v; want %q", got, err, want)
	}
}

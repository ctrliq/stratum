package cluster

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/version"
)

// spec.network.cilium is parsed strictly, and every bad value is refused
// naming the field.
func TestLoadConfigValidatesCilium(t *testing.T) {
	for _, tc := range []struct {
		name, block string
		wantErr     string
	}{
		{name: "absent"},
		{name: "every tunnel field", block: "mtu: 9216\n      devices: [fe-nic+, fblo]\n      bgpControlPlane: true\n      routingMode: tunnel\n"},
		{name: "native", block: "routingMode: native\n      ipv4NativeRoutingCIDR: 10.245.0.0/16\n      autoDirectNodeRoutes: false\n"},
		{name: "mtu too small", block: "mtu: 1000\n", wantErr: "spec.network.cilium.mtu 1000 is out of range"},
		{name: "mtu too large", block: "mtu: 70000\n", wantErr: "spec.network.cilium.mtu 70000"},
		{name: "device with a space", block: "devices: [\"eth 0\"]\n", wantErr: "spec.network.cilium.devices: \"eth 0\""},
		{name: "device too long", block: "devices: [abcdefghijklmnop]\n", wantErr: "spec.network.cilium.devices"},
		{name: "wildcard not at the end", block: "devices: [\"e+th\"]\n", wantErr: "spec.network.cilium.devices"},
		{name: "device twice", block: "devices: [eth0, eth0]\n", wantErr: "listed twice"},
		{name: "dot device", block: "devices: [\".\"]\n", wantErr: "spec.network.cilium.devices"},
		{name: "dot-dot device", block: "devices: [\"..\"]\n", wantErr: "spec.network.cilium.devices"},
		{name: "unknown routing mode", block: "routingMode: geneve\n", wantErr: "spec.network.cilium.routingMode \"geneve\""},
		{name: "native without a CIDR", block: "routingMode: native\n", wantErr: "ipv4NativeRoutingCIDR is required"},
		{name: "native with a bad CIDR", block: "routingMode: native\n      ipv4NativeRoutingCIDR: 10.0.0.0\n", wantErr: "not a CIDR"},
		{name: "native with an IPv6 CIDR as IPv4", block: "routingMode: native\n      ipv4NativeRoutingCIDR: fd00::/48\n", wantErr: "not an IPv4 CIDR"},
		{name: "native with an IPv4 CIDR as IPv6", block: "routingMode: native\n      ipv4NativeRoutingCIDR: 10.0.0.0/8\n      ipv6NativeRoutingCIDR: 10.0.0.0/8\n", wantErr: "not an IPv6 CIDR"},
		{name: "CIDR in tunnel mode", block: "ipv4NativeRoutingCIDR: 10.0.0.0/8\n", wantErr: "ipv4NativeRoutingCIDR applies only to routingMode: native"},
		{name: "direct routes in tunnel mode", block: "autoDirectNodeRoutes: true\n", wantErr: "autoDirectNodeRoutes applies only"},
		{name: "misspelled key", block: "mtus: 9000\n", wantErr: "mtus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := "spec:\n  network:\n    podCIDR: 10.245.0.0/16\n"
			if tc.block != "" {
				doc += "    cilium:\n      " + tc.block
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("LoadConfig: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// Native routing on a dual-stack cluster needs the IPv6 range, and a
// single-stack cluster has no use for one.
func TestCiliumCheckStack(t *testing.T) {
	native := func(v6 string) *Cilium {
		return &Cilium{RoutingMode: "native", IPv4NativeRoutingCIDR: "10.0.0.0/8", IPv6NativeRoutingCIDR: v6}
	}
	for _, tc := range []struct {
		name    string
		c       *Cilium
		dual    bool
		wantErr string
	}{
		{name: "no block", dual: true},
		{name: "tunnel on dual-stack", c: &Cilium{MTU: 9000}, dual: true},
		{name: "native single-stack", c: native("")},
		{name: "native dual-stack", c: native("fd00::/48"), dual: true},
		{name: "native dual-stack without IPv6", c: native(""), dual: true, wantErr: "required with routingMode: native on a dual-stack"},
		{name: "native single-stack with IPv6", c: native("fd00::/48"), wantErr: "single-stack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.CheckStack(tc.dual)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("CheckStack: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// A worker's file cannot carry the block: it is a cluster setting.
func TestWorkerConfigRefusesCilium(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("spec:\n  network:\n    cilium:\n      mtu: 9000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWorkerConfig(path); err == nil || !strings.Contains(err.Error(), "spec.network.cilium.mtu") {
		t.Fatalf("err = %v, want the cilium field named", err)
	}
}

func TestCiliumSettingsFromTheBlock(t *testing.T) {
	yes, no := true, false
	if s := (*Cilium)(nil).Settings(); !s.IsDefault() {
		t.Errorf("a nil block = %+v, want the shipped settings", s)
	}
	if s := (&Cilium{RoutingMode: "tunnel", BGPControlPlane: &no}).Settings(); !s.IsDefault() {
		t.Errorf("explicit defaults = %+v, want the shipped settings", s)
	}
	full := &Cilium{MTU: 9000, Devices: []string{"fe-nic+", "fblo"}, BGPControlPlane: &yes, RoutingMode: "native",
		IPv4NativeRoutingCIDR: "10.0.0.0/8", IPv6NativeRoutingCIDR: "fd00::/48", AutoDirectNodeRoutes: &yes}
	want := addons.CiliumSettings{MTU: 9000, Devices: []string{"fe-nic+", "fblo"}, BGPControlPlane: true, NativeRouting: true,
		IPv4NativeRoutingCIDR: "10.0.0.0/8", IPv6NativeRoutingCIDR: "fd00::/48", AutoDirectNodeRoutes: true}
	if got := full.Settings(); !reflect.DeepEqual(got, want) {
		t.Errorf("settings = %+v, want every field mapped: %+v", got, want)
	}
}

// writeBundles stages the configured settings in the Cilium bundle and records
// them for a joining control plane; the shipped settings stage the shipped
// bundle and leave no record.
func TestWriteBundlesStagesTheCiliumSettings(t *testing.T) {
	origDir, origCilium := version.ManifestsDir, version.CiliumSettingsPath
	version.ManifestsDir = t.TempDir()
	version.CiliumSettingsPath = filepath.Join(t.TempDir(), "cilium.json")
	t.Cleanup(func() { version.ManifestsDir, version.CiliumSettingsPath = origDir, origCilium })
	st := &NodeState{Role: "controller", NodeIP: "10.0.0.11"}
	block := &Cilium{MTU: 9000}

	cfg := &ClusterConfig{}
	cfg.Spec.Network.Cilium = block
	if err := writeBundles(cfg, st); err != nil {
		t.Fatal(err)
	}
	staged, err := os.ReadFile(filepath.Join(version.ManifestsDir, "cilium", "cilium.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(staged), "\n  mtu: \"9000\"\n") {
		t.Error("the staged Cilium bundle does not carry the configured MTU")
	}
	if b, err := os.ReadFile(version.CiliumSettingsPath); err != nil || string(b) != string(block.Settings().Encode()) {
		t.Errorf("recorded settings = %q, %v; want %q", b, err, block.Settings().Encode())
	}

	if err := writeBundles(&ClusterConfig{}, st); err != nil {
		t.Fatal(err)
	}
	if staged, err = os.ReadFile(filepath.Join(version.ManifestsDir, "cilium", "cilium.yaml")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(staged), "\n  mtu: ") {
		t.Error("the shipped settings still staged an MTU")
	}
	if _, err := os.Stat(version.CiliumSettingsPath); !os.IsNotExist(err) {
		t.Errorf("the shipped settings left the record behind (%v)", err)
	}
}

// A control plane records the settings it renders, and the shipped ones leave
// no file behind.
func TestSaveCiliumSettings(t *testing.T) {
	orig := version.CiliumSettingsPath
	version.CiliumSettingsPath = filepath.Join(t.TempDir(), "cilium.json")
	t.Cleanup(func() { version.CiliumSettingsPath = orig })
	set := addons.CiliumSettings{MTU: 9216}
	if err := saveCiliumSettings(set); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(version.CiliumSettingsPath); err != nil || string(b) != string(set.Encode()) {
		t.Fatalf("saved %q, %v; want %q", b, err, set.Encode())
	}
	if err := saveCiliumSettings(addons.CiliumSettings{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(version.CiliumSettingsPath); !os.IsNotExist(err) {
		t.Errorf("the shipped settings left %s behind (%v)", version.CiliumSettingsPath, err)
	}
	if err := saveCiliumSettings(addons.CiliumSettings{}); err != nil {
		t.Errorf("no file and the shipped settings: %v", err)
	}
}

// The record says what this control plane staged: a bundle it could not stage
// leaves the previous record, so a join is not checked against settings the
// cluster never ran.
func TestWriteBundlesRecordsOnlyWhatItStaged(t *testing.T) {
	origDir, origCilium := version.ManifestsDir, version.CiliumSettingsPath
	version.ManifestsDir = t.TempDir()
	version.CiliumSettingsPath = filepath.Join(t.TempDir(), "cilium.json")
	t.Cleanup(func() { version.ManifestsDir, version.CiliumSettingsPath = origDir, origCilium })
	st := &NodeState{Role: "controller", NodeIP: "10.0.0.11"}
	before := addons.CiliumSettings{MTU: 1400}
	writeFile := func(p string, b []byte) {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(version.CiliumSettingsPath, before.Encode())
	if err := os.MkdirAll(filepath.Join(version.ManifestsDir, "cilium", "cilium.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &ClusterConfig{}
	cfg.Spec.Network.Cilium = &Cilium{MTU: 9000}
	if err := writeBundles(cfg, st); err == nil {
		t.Fatal("staging over a directory succeeded")
	}
	if b, _ := os.ReadFile(version.CiliumSettingsPath); string(b) != string(before.Encode()) {
		t.Errorf("record = %q after a failed stage, want the previous %q", b, before.Encode())
	}
}

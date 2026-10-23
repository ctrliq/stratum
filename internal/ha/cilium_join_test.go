package ha

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/version"
)

// A cluster whose control planes saved Cilium settings shares them, and one
// that never set any shares no entry, so an older binary can still join it.
func TestCollectBundleSharesTheCiliumSettingsOnlyWhenSet(t *testing.T) {
	bundleHost(t)
	orig := version.CiliumSettingsPath
	version.CiliumSettingsPath = filepath.Join(version.PKIDir, ciliumSettingsEntry)
	t.Cleanup(func() { version.CiliumSettingsPath = orig })

	files, err := collectBundle(false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[ciliumSettingsEntry]; ok {
		t.Error("a cluster with the shipped Cilium settings shared a settings entry")
	}

	set := addons.CiliumSettings{MTU: 9216, BGPControlPlane: true}
	if err := os.WriteFile(version.CiliumSettingsPath, set.Encode(), 0o644); err != nil {
		t.Fatal(err)
	}
	if files, err = collectBundle(false); err != nil {
		t.Fatal(err)
	}
	if got := string(files[ciliumSettingsEntry]); got != string(set.Encode()) {
		t.Errorf("bundle %s = %q, want %q", ciliumSettingsEntry, got, set.Encode())
	}
	joiner := t.TempDir()
	if err := checkCertNames(joiner, files); err != nil {
		t.Errorf("a bundle with the Cilium settings is refused: %v", err)
	}
}

// A joining control plane must carry the cluster's spec.network.cilium.
func TestCheckJoinCilium(t *testing.T) {
	orig := version.CiliumSettingsPath
	version.CiliumSettingsPath = filepath.Join(t.TempDir(), ciliumSettingsEntry)
	t.Cleanup(func() { version.CiliumSettingsPath = orig })
	set := addons.CiliumSettings{MTU: 9216, Devices: []string{"fe-nic+"}}

	if err := checkJoinCilium(addons.CiliumSettings{}); err != nil {
		t.Errorf("no settings on either side: %v", err)
	}
	err := checkJoinCilium(set)
	if err == nil || !strings.Contains(err.Error(), "unset (the shipped settings)") {
		t.Errorf("a joiner with settings into a cluster without them: %v, want a refusal naming both", err)
	}

	if err := os.WriteFile(version.CiliumSettingsPath, set.Encode(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkJoinCilium(set); err != nil {
		t.Errorf("matching settings: %v", err)
	}
	if err := checkJoinCilium(addons.CiliumSettings{}); err == nil || !strings.Contains(err.Error(), `"mtu":9216`) {
		t.Errorf("a joiner without the cluster's settings: %v, want a refusal naming them", err)
	}
	if err := checkJoinCilium(addons.CiliumSettings{MTU: 1500}); err == nil {
		t.Error("a joiner with other settings joined")
	}

	if err := os.WriteFile(version.CiliumSettingsPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkJoinCilium(set); err == nil {
		t.Error("an unreadable settings entry was taken as a match")
	}
}

// A settings file that exists but cannot be read is an error, not "no
// settings": a joiner would take the missing entry as the shipped ones.
func TestCollectBundleRefusesAnUnreadableCiliumSettings(t *testing.T) {
	bundleHost(t)
	orig := version.CiliumSettingsPath
	version.CiliumSettingsPath = filepath.Join(version.PKIDir, ciliumSettingsEntry)
	t.Cleanup(func() { version.CiliumSettingsPath = orig })
	if err := os.Mkdir(version.CiliumSettingsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := collectBundle(false); err == nil || !strings.Contains(err.Error(), "Cilium settings") {
		t.Fatalf("err = %v, want the unreadable settings reported", err)
	}
}

package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

func loadDoc(t *testing.T, doc string) (*ClusterConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestLoadConfigDashboard(t *testing.T) {
	for _, tc := range []struct {
		name, doc   string
		wantErr     string
		wantEnabled bool
		want        addons.DashboardSettings
	}{
		{name: "absent", doc: "spec: {}\n", wantEnabled: true, want: addons.DashboardSettings{NodePort: 30443, NodeShell: true, TokenSecrets: true}},
		{name: "disabled", doc: "spec:\n  dashboard:\n    enabled: false\n", want: addons.DashboardSettings{NodePort: 30443, NodeShell: true, TokenSecrets: true}},
		{name: "operator off", doc: "spec:\n  operator: false\n", want: addons.DashboardSettings{NodePort: 30443, NodeShell: true, TokenSecrets: true}},
		{name: "enabled without the operator", doc: "spec:\n  operator: false\n  dashboard:\n    enabled: true\n", wantErr: "spec.dashboard.enabled needs the operator"},
		{name: "every field", doc: "spec:\n  dashboard:\n    nodePort: 31000\n    publicURL: https://dash.example.com\n    tlsSecretName: my-cert\n    enableNodeShell: true\n",
			wantEnabled: true, want: addons.DashboardSettings{NodePort: 31000, PublicURL: "https://dash.example.com", TLSSecretName: "my-cert", NodeShell: true, TokenSecrets: true}},
		{name: "cis withholds the node shell", doc: "spec:\n  cis: true\n", wantEnabled: true, want: addons.DashboardSettings{NodePort: 30443, TokenSecrets: true}},
		{name: "cis with the node shell asked for", doc: "spec:\n  cis: true\n  dashboard:\n    enableNodeShell: true\n", wantEnabled: true, want: addons.DashboardSettings{NodePort: 30443, NodeShell: true, TokenSecrets: true}},
		{name: "no node shell", doc: "spec:\n  dashboard:\n    enableNodeShell: false\n", wantEnabled: true, want: addons.DashboardSettings{NodePort: 30443, TokenSecrets: true}},
		{name: "no token Secrets", doc: "spec:\n  dashboard:\n    tokenSecrets: false\n", wantEnabled: true, want: addons.DashboardSettings{NodePort: 30443, NodeShell: true}},
		{name: "cluster IP only", doc: "spec:\n  dashboard:\n    nodePort: 0\n", wantEnabled: true, want: addons.DashboardSettings{NodeShell: true, TokenSecrets: true}},
		{name: "node port out of range", doc: "spec:\n  dashboard:\n    nodePort: 8443\n", wantErr: "spec.dashboard.nodePort 8443 is out of the NodePort range"},
		{name: "public URL with a path", doc: "spec:\n  dashboard:\n    publicURL: https://dash.example.com/ui\n", wantErr: "spec.dashboard.publicURL"},
		{name: "public URL not http", doc: "spec:\n  dashboard:\n    publicURL: ftp://dash.example.com\n", wantErr: "spec.dashboard.publicURL"},
		{name: "public URL over plain http", doc: "spec:\n  dashboard:\n    publicURL: http://dash.example.com\n", wantErr: "always serves TLS"},
		{name: "bad secret name", doc: "spec:\n  dashboard:\n    tlsSecretName: My_Cert\n", wantErr: "spec.dashboard.tlsSecretName"},
		{name: "misspelled key", doc: "spec:\n  dashboard:\n    nodePorts: 31000\n", wantErr: "nodePorts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadDoc(t, tc.doc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := c.DashboardEnabled(); got != tc.wantEnabled {
				t.Errorf("DashboardEnabled() = %v, want %v", got, tc.wantEnabled)
			}
			if got := c.DashboardSettings(); got != tc.want {
				t.Errorf("DashboardSettings() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestWriteBundlesStagesAndRemovesTheDashboard(t *testing.T) {
	withImporterStacks(t, nil)
	st := &NodeState{Role: "controller", NodeIP: "10.0.0.11"}
	if err := writeBundles(&ClusterConfig{}, st); err != nil {
		t.Fatal(err)
	}
	staged := stagedStacks(t, version.ManifestsDir)
	if _, ok := staged[dashboardFile]; !ok {
		t.Fatalf("the dashboard was not staged by default: %v", sortedStacks(staged))
	}

	off := false
	cfg := &ClusterConfig{Spec: Spec{Dashboard: &Dashboard{Enabled: &off}, Storage: Storage{Default: addons.StorageNone}}}
	if err := writeBundles(cfg, st); err != nil {
		t.Fatal(err)
	}
	staged = stagedStacks(t, version.ManifestsDir)
	for _, rel := range []string{dashboardFile, localPathFile} {
		if _, ok := staged[rel]; ok {
			t.Errorf("%s is still staged after the config turned it off", rel)
		}
		if _, err := os.Stat(filepath.Join(version.ManifestsDir, filepath.Dir(rel))); !os.IsNotExist(err) {
			t.Errorf("the %s stack directory is left behind: %v", filepath.Dir(rel), err)
		}
	}
}

func TestWriteBundlesRefusesTurningTheOperatorOffUnderTheDashboard(t *testing.T) {
	withImporterStacks(t, nil)
	st := &NodeState{Role: "controller", NodeIP: "10.0.0.11"}
	if err := writeBundles(&ClusterConfig{}, st); err != nil {
		t.Fatal(err)
	}
	off := false
	noOperator := &ClusterConfig{Spec: Spec{Operator: &off}}
	err := writeBundles(noOperator, st)
	if err == nil || !strings.Contains(err.Error(), "spec.dashboard.enabled: false") {
		t.Fatalf("err = %v, want a refusal naming the safe order", err)
	}
	if _, statErr := os.Stat(filepath.Join(version.ManifestsDir, dashboardFile)); statErr != nil {
		t.Error("the refused start removed the dashboard stack anyway")
	}

	if err := os.Remove(filepath.Join(version.ManifestsDir, dashboardFile)); err != nil {
		t.Fatal(err)
	}
	if err := writeBundles(noOperator, st); err != nil {
		t.Errorf("with the dashboard already gone, turning the operator off was refused: %v", err)
	}
}

func TestTheNodeShellDefaultIsTheProfiles(t *testing.T) {
	prev := profile.All()
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
	if err := profile.SetRegistered(append(prev,
		profile.Profile{Name: "open"},
		profile.Profile{Name: "locked", DisableNodeShell: true},
	)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, profile string
		want          bool
	}{
		{"no profile", "", true},
		{"a profile that leaves the shell on", "open", true},
		{"a profile other than cis that disables it", "locked", false},
		{"the cis profile", "cis", false},
		{"a profile this binary does not register", "ghost", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &ClusterConfig{}
			c.Spec.Profile = tc.profile
			if got := c.nodeShellDefault(); got != tc.want {
				t.Errorf("node shell default = %v, want %v", got, tc.want)
			}
		})
	}
}

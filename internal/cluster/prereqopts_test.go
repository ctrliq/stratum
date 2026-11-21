package cluster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/version"
)

// Any node.yaml, even an unreadable one, means an existing node.
func TestPrereqOptionsExisting(t *testing.T) {
	off := false
	cfg := &ClusterConfig{}
	cfg.Spec.CIS = true
	cfg.Spec.SELinux = &off
	for _, tc := range []struct {
		name  string
		state *string
		want  bool
	}{
		{"no node.yaml", nil, false},
		{"node.yaml", strp("role: controller\nhostname: cp1\n"), true},
		{"malformed node.yaml", strp("role: [\n"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := version.K8sConfDir
			version.K8sConfDir = t.TempDir()
			t.Cleanup(func() { version.K8sConfDir = old })
			if tc.state != nil {
				if err := os.WriteFile(StatePath(), []byte(*tc.state), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := prereqOptions(cfg, profiletest.Profile())
			if got.Existing != tc.want || got.Profile.Name != nodestate.CISProfile || !got.SELinuxOptOut {
				t.Errorf("prereqOptions = %+v, want Existing %v with CIS and the opt-out carried", got, tc.want)
			}
		})
	}
}

func strp(s string) *string { return &s }

func TestNodeSELinuxOptions(t *testing.T) {
	registerCISForTest(t)
	dir := t.TempDir()
	recorded := filepath.Join(dir, "recorded.yaml")
	missing := filepath.Join(dir, "missing.yaml")
	writeState := func(t *testing.T, state string) {
		t.Helper()
		if err := os.WriteFile(StatePath(), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// LoadConfig resolves configPath against the working directory, so it is absolute.
	withRecorded := "role: controller\nhostname: cp1\nconfigPath: " + recorded + "\n"
	for _, tc := range []struct {
		name       string
		state      string
		config     string // written where the node reads its config from
		wantCIS    bool
		wantOptOut bool
	}{
		{"controller opt-out from a recorded non-default config path",
			withRecorded,
			"spec:\n  selinux: false\n", false, true},
		{"controller without spec.selinux",
			withRecorded,
			"spec: {}\n", false, false},
		{"controller CIS from the saved state",
			"role: controller\nhostname: cp1\ncis: true\nconfigPath: " + missing + "\n",
			"", true, false},
		{"controller CIS from the config file",
			withRecorded,
			"spec:\n  cis: true\n", true, false},
		{"worker file setting spec.selinux overrides the saved opt-out",
			"role: worker\nhostname: w1\nselinuxOptOut: false\n",
			"spec:\n  selinux: false\n", false, true},
		{"worker file omitting spec.selinux keeps the saved opt-out",
			"role: worker\nhostname: w1\nselinuxOptOut: true\n",
			"spec:\n  network:\n    nodeIP: 10.0.0.5\n", false, true},
		{"worker file turning the opt-out off",
			"role: worker\nhostname: w1\nselinuxOptOut: true\n",
			"spec:\n  selinux: true\n", false, false},
		{"worker CIS from the saved state",
			"role: worker\nhostname: w1\ncis: true\n",
			"", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := version.K8sConfDir
			version.K8sConfDir = dir
			t.Cleanup(func() { version.K8sConfDir = old })
			writeState(t, tc.state)

			oldWorkerPath := workerSELinuxPath
			workerSELinuxPath = filepath.Join(dir, "worker.yaml")
			t.Cleanup(func() { workerSELinuxPath = oldWorkerPath })
			cfgPath := recorded
			if strings.HasPrefix(tc.state, "role: worker") {
				cfgPath = workerSELinuxPath
			}
			if tc.config != "" {
				if err := os.WriteFile(cfgPath, []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			got, err := NodeSELinuxOptions()
			if err != nil {
				t.Fatal(err)
			}
			if got.RequiresSELinux != tc.wantCIS || (tc.wantCIS && got.Profile != "cis") || got.OptOut != tc.wantOptOut {
				t.Errorf("NodeSELinuxOptions = %+v, want the cis profile %v, OptOut %v", got, tc.wantCIS, tc.wantOptOut)
			}
		})
	}
}

func TestNodeSELinuxOptionsUnreadable(t *testing.T) {
	dir := t.TempDir()
	old := version.K8sConfDir
	version.K8sConfDir = dir
	t.Cleanup(func() { version.K8sConfDir = old })
	oldWorkerPath := workerSELinuxPath
	workerSELinuxPath = filepath.Join(dir, "worker.yaml")
	t.Cleanup(func() { workerSELinuxPath = oldWorkerPath })
	recorded := filepath.Join(dir, "recorded.yaml")
	broken := "spec: [\n"

	for _, tc := range []struct {
		name  string
		state string
		conf  string // written to the config file this node reads, if any
	}{
		{"malformed node state", "role: [\n", ""},
		{"controller with a malformed config",
			"role: controller\nconfigPath: " + recorded + "\n", broken},
		{"worker with a malformed config", "role: worker\n", broken},
		{"worker with an unknown profile", "role: worker\nprofile: does-not-exist\n", ""},
		{"controller config naming an unknown profile",
			"role: controller\nconfigPath: " + recorded + "\n", "spec:\n  profile: does-not-exist\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(StatePath(), []byte(tc.state), 0o600); err != nil {
				t.Fatal(err)
			}
			os.Remove(recorded)
			os.Remove(workerSELinuxPath)
			if tc.conf != "" {
				path := workerSELinuxPath
				if strings.HasPrefix(tc.state, "role: controller") {
					path = recorded
				}
				if err := os.WriteFile(path, []byte(tc.conf), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := NodeSELinuxOptions(); err == nil {
				t.Errorf("NodeSELinuxOptions = nil error, want an error")
			}
		})
	}
}

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

// Only a missing node.yaml is a first start; an unreadable one belongs to a
// node brought up before, which a missing SELinux policy must not stop.
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

// NodeSELinuxOptions feeds the upgrade's refusal, so it must read what this
// node actually runs with: the controller its recorded config file, the
// worker its config file over its saved opt-out.
func TestNodeSELinuxOptions(t *testing.T) {
	registerCISForTest(t)
	dir := t.TempDir()
	recorded := filepath.Join(dir, "recorded.yaml")
	// Never written: a state with no configPath would read the host's
	// /etc/stratum/config.yaml instead.
	missing := filepath.Join(dir, "missing.yaml")
	writeState := func(t *testing.T, state string) {
		t.Helper()
		if err := os.WriteFile(StatePath(), []byte(state), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// configPath is the file the controller's state recorded; absolute, since
	// LoadConfig resolves it against the working directory.
	withRecorded := "role: controller\nhostname: cp1\nconfigPath: " + recorded + "\n"
	for _, tc := range []struct {
		name       string
		state      string
		config     string // written where the node reads its config from
		wantCIS    bool
		wantOptOut bool
	}{
		// The controller reads the config its start recorded, not the default
		// path.
		{"controller opt-out from a recorded non-default config path",
			withRecorded,
			"spec:\n  selinux: false\n", false, true},
		{"controller without spec.selinux",
			withRecorded,
			"spec: {}\n", false, false},
		// CIS on either side counts: a restart reads the file, a state predating
		// the field keeps the saved one.
		{"controller CIS from the saved state",
			"role: controller\nhostname: cp1\ncis: true\nconfigPath: " + missing + "\n",
			"", true, false},
		{"controller CIS from the config file",
			withRecorded,
			"spec:\n  cis: true\n", true, false},
		// A worker's config file wins over the opt-out it saved at join.
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

			// What the node reads: the recorded file for a controller, the
			// worker config path for a worker.
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

// An unreadable node state or config, or an unknown profile, cannot say what
// the node runs with, and
// the refusal must not decide from a guess: the caller warns and proceeds.
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
		// A profile this binary does not register: read as the default, a CIS
		// node would pass without its requirements.
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

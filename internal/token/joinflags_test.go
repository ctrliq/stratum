package token

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profile/profiletest"
	"github.com/ctrliq/stratum/internal/version"
)

// Registers the cis test profile and puts back the registry it replaced.
func registerCIS(t *testing.T) {
	t.Helper()
	prev := profile.All()
	if err := profile.SetRegistered([]profile.Profile{profiletest.Profile()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

// Mirrors what the pkg/cli join-flag tests do for their copies of the derivation.
func redirectJoinFlagState(t *testing.T) {
	t.Helper()
	oldConf, oldNet, oldPKI := version.K8sConfDir, version.NetConfigPath, version.PKIDir
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath, version.PKIDir = oldConf, oldNet, oldPKI })
	version.K8sConfDir = t.TempDir()
	// Separate dir: saving node state must not create the netconfig file.
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	version.PKIDir = t.TempDir()
}

// The ranges and domain cannot be discovered after a join; the join line is the source.
func TestJoinFlags_CarryNetworkAndCIS(t *testing.T) {
	registerCIS(t)
	for _, tc := range []struct {
		name      string
		netconfig []string // the 2-3 netconfig lines; nil = lost (never written)
		state     nodestate.NodeState
		want      JoinFlags
	}{
		{
			name:  "a default cluster carries no flags",
			state: nodestate.NodeState{Role: "controller"},
			want:  JoinFlags{},
		},
		{
			name:  "a CIS node carries --cis",
			state: nodestate.NodeState{Role: "controller", CIS: true},
			want:  JoinFlags{Profile: "cis"},
		},
		{
			name:      "a custom service range is carried for the worker",
			netconfig: []string{netdefault.PodCIDR, "10.43.0.0/16"},
			state:     nodestate.NodeState{Role: "controller"},
			want:      JoinFlags{ServiceCIDR: "10.43.0.0/16"},
		},
		{
			name:      "a custom pod range is carried for the worker",
			netconfig: []string{"10.42.0.0/16", netdefault.ServiceCIDR},
			state:     nodestate.NodeState{Role: "controller"},
			want:      JoinFlags{PodCIDR: "10.42.0.0/16"},
		},
		{
			name:      "a custom cluster domain is carried for the worker",
			netconfig: []string{netdefault.PodCIDR, netdefault.ServiceCIDR, "fuzz.internal"},
			state:     nodestate.NodeState{Role: "controller"},
			want:      JoinFlags{ClusterDomain: "fuzz.internal"},
		},
		{
			name:      "a custom-network CIS cluster carries every flag",
			netconfig: []string{"10.42.0.0/16", "10.43.0.0/16", "fuzz.internal"},
			state:     nodestate.NodeState{Role: "controller", CIS: true},
			want: JoinFlags{
				PodCIDR:       "10.42.0.0/16",
				ServiceCIDR:   "10.43.0.0/16",
				ClusterDomain: "fuzz.internal",
				Profile:       "cis",
			},
		},
		{
			name: "a lost netconfig is recovered from node state",
			// Ranges come from recorded node state, never the built-in defaults.
			state: nodestate.NodeState{Role: "controller",
				PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16", ClusterDomain: "fuzz.internal"},
			want: JoinFlags{
				PodCIDR:       "10.42.0.0/16",
				ServiceCIDR:   "10.43.0.0/16",
				ClusterDomain: "fuzz.internal",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectJoinFlagState(t)
			if tc.netconfig != nil {
				if err := os.WriteFile(version.NetConfigPath, []byte(strings.Join(tc.netconfig, "\n")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := nodestate.SaveState(&tc.state); err != nil {
				t.Fatal(err)
			}

			got, err := LoadJoinFlags()
			if err != nil {
				t.Fatalf("LoadJoinFlags: %v", err)
			}
			if got != tc.want {
				t.Errorf("LoadJoinFlags = %+v, want %+v", got, tc.want)
			}
		})
	}

	// Treating a malformed netconfig as a default cluster omits the real ranges silently.
	t.Run("a malformed netconfig fails instead of joining a wrong-range worker", func(t *testing.T) {
		redirectJoinFlagState(t)
		if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller"}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(version.NetConfigPath, []byte("10.42.0.0/16\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadJoinFlags(); err == nil {
			t.Error("a malformed netconfig was treated as a default cluster; the join line would silently omit the real ranges")
		}
	})
}

// A --profile line naming an unregistered profile is a grant no joiner could use.
func TestJoinFlags_RefuseAProfileThisBinaryDoesNotRegister(t *testing.T) {
	prev := profile.All()
	if err := profile.SetRegistered(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
	redirectJoinFlagState(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", Profile: profile.CISName}); err != nil {
		t.Fatal(err)
	}
	_, err := LoadJoinFlags()
	if !errors.Is(err, profile.ErrUnregistered) {
		t.Fatalf("LoadJoinFlags = %v, want the refusal wrapping profile.ErrUnregistered", err)
	}
	for _, want := range []string{profile.CISUnregisteredNote, `profile "cis"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q: %v", want, err)
		}
	}
}

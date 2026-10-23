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

// registerCIS registers the cis test profile for the test, so a cis node's
// state resolves, and puts back the registry it replaced.
func registerCIS(t *testing.T) {
	t.Helper()
	prev := profile.All()
	if err := profile.SetRegistered([]profile.Profile{profiletest.Profile()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
}

// redirectJoinFlagState points the join-flag inputs at the test's own
// directories: the persisted network config (version.NetConfigPath) and the
// node state (version.K8sConfDir), with PKIDir redirected too because a lost
// netconfig falls back to the published CA there. This mirrors what
// pkg/cli/token_cis_test.go and pkg/cli/token_servicecidr_test.go do for the
// pkg/cli copies of the derivation.
func redirectJoinFlagState(t *testing.T) {
	t.Helper()
	oldConf, oldNet, oldPKI := version.K8sConfDir, version.NetConfigPath, version.PKIDir
	t.Cleanup(func() { version.K8sConfDir, version.NetConfigPath, version.PKIDir = oldConf, oldNet, oldPKI })
	version.K8sConfDir = t.TempDir()
	// A separate directory, so saving node state cannot create the netconfig
	// file a "lost netconfig" case must not see.
	version.NetConfigPath = filepath.Join(t.TempDir(), "netconfig")
	version.PKIDir = t.TempDir()
}

// TestJoinFlags_CarryNetworkAndCIS pins that the join flags a joining node
// needs are derived in one place, here, and returned as data. The pod and
// service ranges and the cluster domain cannot be discovered by a node after
// it joins — a worker learns them only from its join line — and --cis is the
// only way a worker learns the cluster runs the CIS profile (with it, the
// kubelet's seccompDefault and the rest of the profile). pkg/cli's
// workerJoinExtra and cisJoinFlag held these rules and rendered them into
// pre-quoted strings, and the MCP token_create tool derived none of them, so a
// worker joined through it ran the default ranges and the non-CIS profile with
// nothing saying why.
//
// The function returns data, not a rendered line: a field is empty when the
// cluster sits at the default for it, so each front end renders --pod-cidr,
// --service-cidr and --cluster-domain only on a non-default cluster and --cis
// only on a CIS one, and no front end re-derives the rule to drift from it.
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
			// netconfig absent; the ranges and domain come from the state the
			// control plane recorded, never from the built-in defaults.
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

	// A netconfig that exists but cannot be parsed is not a default cluster.
	// Treating it as one printed a worker join line without --service-cidr:
	// the join would succeed and the worker's kubelet would point pods at the
	// default DNS address on a cluster whose DNS is elsewhere, with nothing
	// saying why. Fail instead.
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

// TestJoinFlags_RefuseAProfileThisBinaryDoesNotRegister: on a cis control
// plane run by an engine binary the derived join line said --profile cis
// while the bundle the node publishes carried no at-rest encryption key, a
// grant no joiner could use. The derivation refuses instead, with the
// cis note, and both front ends derive before they mint.
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

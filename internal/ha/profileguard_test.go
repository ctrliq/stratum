package ha

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// unregisteredCISController is a control plane whose node state names the cis
// profile on a binary that registers no profile at all: the node a binary
// that does not carry the cis profile meets. Its state and etcd data dir
// live under a private root, and it returns the state file's bytes.
func unregisteredCISController(t *testing.T) []byte {
	t.Helper()
	root := t.TempDir()
	oldConf, oldEtcd := version.K8sConfDir, version.EtcdDataDir
	version.K8sConfDir = filepath.Join(root, "kubernetes")
	version.EtcdDataDir = filepath.Join(root, "etcd")
	t.Cleanup(func() { version.K8sConfDir, version.EtcdDataDir = oldConf, oldEtcd })
	st := controller()
	st.Profile = profile.CISName
	if err := nodestate.SaveState(st); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(version.EtcdDataDir, "member"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version.EtcdDataDir, "member", "wal"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := profile.All()
	if err := profile.SetRegistered(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })
	b, err := os.ReadFile(nodestate.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// requireUnregisteredRefusal is the refusal every guarded entry gives: the
// cis note, wrapping profile.ErrUnregistered, in the entry's voice.
func requireUnregisteredRefusal(t *testing.T, err error, op string) {
	t.Helper()
	if !errors.Is(err, profile.ErrUnregistered) {
		t.Fatalf("err = %v, want the refusal wrapping profile.ErrUnregistered", err)
	}
	for _, want := range []string{op + ": this node runs profile", `profile "cis"`, profile.CISUnregisteredNote,
		"nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q: %v", want, err)
		}
	}
}

// requireNodeUntouched: the state file is byte-identical and the etcd data
// dir still holds its member data.
func requireNodeUntouched(t *testing.T, before []byte) {
	t.Helper()
	after, err := os.ReadFile(nodestate.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("node state was rewritten under a refused operation:\n%s", after)
	}
	if _, err := os.Stat(filepath.Join(version.EtcdDataDir, "member", "wal")); err != nil {
		t.Errorf("etcd data dir was touched under a refused operation: %v", err)
	}
}

// TestRejoinRefusesAnUnregisteredProfileBeforeAnything: a rejoin on a cis
// control plane with nothing registered must refuse before anything changes —
// stopping the apiserver, wiping the etcd data dir, changing the membership
// and saving node state, and then failing in rejoinStartPod("kube-apiserver")
// rendering the profile would leave the apiserver down. The profile is
// resolved straight after the state is read,
// before the seed is reached, whichever path the seed would have chosen.
func TestRejoinRefusesAnUnregisteredProfileBeforeAnything(t *testing.T) {
	member := etcdMember{ID: 2, Name: "cp2", PeerURLs: []string{selfPeer}, ClientURLs: []string{"https://10.0.0.2:2379"}}
	for _, tc := range []struct {
		name string
		seed etcdMembership
	}{
		{"not a member: the fresh path", seedWith()},
		{"already a member: the resume path", seedWith(member)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := unregisteredCISController(t)
			f := newFakeRejoin(t, tc.seed, nil)
			rejoinLoadState = nodestate.LoadState
			touched := []string{}
			rejoinSupervisorRunning = func() bool { touched = append(touched, "supervisor"); return true }
			rejoinPKIPreflight = func() error { touched = append(touched, "pki"); return nil }
			rejoinSeedMembers = func(string) (etcdMembership, error) { touched = append(touched, "seed"); return tc.seed, nil }
			rejoinWipeLocalState = func(io.Writer) error { touched = append(touched, "wipe"); return nil }

			err := RejoinEtcd(context.Background(), io.Discard, "10.0.0.1", true)
			requireUnregisteredRefusal(t, err, "snapshot rejoin")
			if len(touched) != 0 {
				t.Errorf("work ran before the profile check: %v", touched)
			}
			if got := f.steps(); got != "" {
				t.Errorf("steps = %q, want none: nothing is stopped, wiped, added or saved", got)
			}
			requireNodeUntouched(t, before)
		})
	}
}

// TestNodeRepointRefusesAnUnregisteredProfileBeforeAnything: ha-repoint
// rewrites the kubeconfigs and the node state and then restarts the static
// pods, which render from the profile; on an unregistered one the rewrite
// landed and the restarts came back as warnings.
func TestNodeRepointRefusesAnUnregisteredProfileBeforeAnything(t *testing.T) {
	before := unregisteredCISController(t)
	touched := []string{}
	old := VIPServes
	VIPServes = func(string) error { touched = append(touched, "vip"); return nil }
	t.Cleanup(func() { VIPServes = old })

	warnings, err := nodeRepoint(io.Discard, "10.0.0.5", func(string) error {
		touched = append(touched, "repoint")
		return nil
	})
	requireUnregisteredRefusal(t, err, "ha-repoint")
	if len(warnings) != 0 {
		t.Errorf("warnings = %q, want none from a refused repoint", warnings)
	}
	if len(touched) != 0 {
		t.Errorf("work ran before the profile check: %v", touched)
	}
	requireNodeUntouched(t, before)
}

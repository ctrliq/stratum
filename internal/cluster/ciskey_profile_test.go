package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

func otherProfile() profile.Profile {
	return profile.Profile{Name: "tenant", SharedSecrets: []profile.Secret{
		{Name: "tenant-key.yaml", Mint: func() ([]byte, error) { return []byte("K"), nil }},
	}}
}

// cisOnly are the words of the cis profile's refusals that mean nothing for
// another profile: its flag and config spellings, its documentation pointer
// and the re-encryption it names, which distributes no secret of another
// profile.
var cisOnly = []string{"--profile cis", "spec.profile: cis", "--cis", "spec.cis", "re-encrypts", "Stratum PRO", profile.ProDocsURL, "Turn CIS on", "at-rest encryption key"}

func wantProfileRefusal(t *testing.T, what string, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no refusal", what)
	}
	for _, w := range append([]string{"--profile tenant", "tenant-key.yaml"}, wants...) {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("%s: refusal lacks %q: %v", what, w, err)
		}
	}
	for _, w := range cisOnly {
		if strings.Contains(err.Error(), w) {
			t.Errorf("%s: refusal for another profile says %q: %v", what, w, err)
		}
	}
}

// The gate and the second look refuse another profile's shared secret in that
// profile's terms: the profile and the secret by name, --profile and
// spec.profile as the thing to select or deselect, and the secret put on every
// control plane by hand as the way to select it on a running cluster.
func TestSharedSecretRefusalsNameTheProfileAndItsSecrets(t *testing.T) {
	dir := t.TempDir()
	at := func(name string) string { return filepath.Join(dir, name) }
	read := func(st etcdJoinState, err error) func() (etcdJoinState, error) {
		return func() (etcdJoinState, error) { return st, err }
	}

	_, err := sharedSecretGate(otherProfile(), at, read(etcdJoinState{Members: 3}, nil))
	wantProfileRefusal(t, "members", err, "etcd has 3 members", "--profile tenant (or spec.profile: tenant)", "on every control plane", "`sudo /usr/local/bin/stratum kubectl -n kube-system delete secret stratum-certs`", "stratum token create --control-plane")

	_, err = sharedSecretGate(otherProfile(), at, read(etcdJoinState{Members: 1, JoinBundle: true}, nil))
	wantProfileRefusal(t, "bundle", err, "kube-system/stratum-certs", "without --profile tenant", "remove spec.profile: tenant", "start it again with --profile tenant")

	boom := errors.New("etcd unreachable")
	_, err = sharedSecretGate(otherProfile(), at, read(etcdJoinState{}, boom))
	wantProfileRefusal(t, "unread", err, "could not be read")
	if !errors.Is(err, boom) {
		t.Errorf("unread: refusal does not wrap the etcd error: %v", err)
	}

	loop := filepath.Join(dir, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	_, err = sharedSecretGate(profile.Profile{Name: "tenant", SharedSecrets: []profile.Secret{{Name: "loop", Mint: otherProfile().SharedSecrets[0].Mint}}}, at, read(etcdJoinState{Members: 1}, nil))
	if err == nil || !strings.Contains(err.Error(), "--profile tenant: cannot tell whether the shared secret loop exists") {
		t.Errorf("stat error: refusal = %v", err)
	}

	mint, err := sharedSecretGate(otherProfile(), at, read(etcdJoinState{Members: 1}, nil))
	if err != nil || len(mint) != 1 {
		t.Fatalf("a sole control plane: gate = %v, %v; want the secret to mint", mint, err)
	}

	err = mintSharedSecrets(otherProfile(), mint, at, func() error { return nil }, read(etcdJoinState{Members: 1, JoinBundle: true}, nil))
	wantProfileRefusal(t, "second look", err, "a second check of etcd, made after the shared secret tenant-key.yaml was written")
	if _, statErr := os.Stat(at("tenant-key.yaml")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the refused secret was left on disk: %v", statErr)
	}

	if err := mintSharedSecrets(otherProfile(), mint, at, func() error { return nil }, read(etcdJoinState{Members: 1}, nil)); err != nil {
		t.Fatalf("a sole control plane with no bundle: mint = %v", err)
	}
	if b, err := os.ReadFile(at("tenant-key.yaml")); err != nil || string(b) != "K" {
		t.Errorf("minted secret = %q, %v", b, err)
	}
}

package nodestate

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// TestRequireNodeProfileRefusesInTheCallersVoice: one guard for every
// destructive entry, so each refusal names its own command, the profile, the
// cis note and that nothing was changed; a node without state runs
// no profile and is let through.
func TestRequireNodeProfileRefusesInTheCallersVoice(t *testing.T) {
	oldConf := version.K8sConfDir
	version.K8sConfDir = filepath.Join(t.TempDir(), "kubernetes")
	t.Cleanup(func() { version.K8sConfDir = oldConf })
	prev := profile.All()
	if err := profile.SetRegistered(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = profile.SetRegistered(prev) })

	if err := RequireNodeProfile("snapshot rejoin"); err != nil {
		t.Fatalf("RequireNodeProfile with no node state = %v, want nil", err)
	}
	if err := SaveState(&NodeState{Role: "controller", Profile: profile.CISName}); err != nil {
		t.Fatal(err)
	}
	err := RequireNodeProfile("snapshot rejoin")
	if !errors.Is(err, profile.ErrUnregistered) {
		t.Fatalf("err = %v, want the refusal wrapping profile.ErrUnregistered", err)
	}
	for _, want := range []string{`snapshot rejoin: this node runs profile "cis"`, profile.CISUnregisteredNote, "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q: %v", want, err)
		}
	}
	if err := (&NodeState{Role: "controller"}).RequireRegisteredProfile("ha-repoint"); err != nil {
		t.Errorf("a node with no profile was refused: %v", err)
	}
}

package cli

import (
	"testing"

	"github.com/ctrliq/stratum/internal/profile"
)

func TestTheRootHookRegistersProfilesForEveryCommand(t *testing.T) {
	isolateProfileRegistryForTest(t)

	root := NewRootCommand(WithProfiles(cisLike()))
	root.SetArgs([]string{"version", "--build"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version = %v", err)
	}
	if _, err := profile.ForNode("cis"); err != nil {
		t.Errorf("after a command other than up, ForNode(cis) = %v; the root hook must register the binary's profiles for every command", err)
	}

	broken := cisLike()
	broken.Name = "CIS"
	root = NewRootCommand(WithProfiles(broken))
	root.SetArgs([]string{"version", "--build"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version with a profile that does not validate = %v; a command that renders nothing must still run", err)
	}
	if _, err := profile.ForNode("cis"); err == nil {
		t.Error("ForNode(cis) succeeded after registration failed; a renderer must get the reason, not render the node without its profile")
	}
	if p, err := profile.ForNode(""); err != nil || !p.None() {
		t.Errorf("ForNode(\"\") = %+v, %v after registration failed; a node with no profile must still render", p, err)
	}
}

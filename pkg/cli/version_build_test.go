package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

// `version --build` prints the build ID alone: the cluster upgrade reads it
// from the staged binary to pin every node to one build, and the release
// stamp cannot do that -- every untagged commit before a release shares it.
func TestVersionBuildPrintsTheBuildID(t *testing.T) {
	old := version.BuildID
	version.BuildID = "0123abcd-dirty-feedbeef"
	t.Cleanup(func() { version.BuildID = old })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = stdout }()
	cmd := newVersionCommand(rootConfig{displayVersion: "v1.37.1-stratum.0"})
	cmd.SetArgs([]string{"--build"})
	runErr := cmd.Execute()
	w.Close()
	os.Stdout = stdout
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if got := strings.TrimSpace(string(out)); got != "0123abcd-dirty-feedbeef" {
		t.Errorf("version --build printed %q, want the build ID alone", got)
	}
}

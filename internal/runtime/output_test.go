package runtime

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

func TestNodeConfigProgressUsesOperationWriter(t *testing.T) {
	nodeConfigsFixture(t)
	old := selinuxLabelled
	t.Cleanup(func() { selinuxLabelled = old })
	selinuxLabelled = func() bool { return false }
	if _, err := WriteNodeConfigs(&nodestate.NodeState{Role: "worker", Hostname: "w1"}); err != nil {
		t.Fatal(err)
	}
	selinuxLabelled = func() bool { return true }
	if err := os.WriteFile(version.RegistriesConfig, []byte("mirrors:\n  docker.io:\n    endpoint: [https://mirror.example, https://mirror.example]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var progress bytes.Buffer
	var renderErr error
	stdout := captureProcessStdout(t, func() {
		_, renderErr = WriteNodeConfigsWithWriter(&progress, &nodestate.NodeState{Role: "worker", Hostname: "w1"})
	})
	if renderErr != nil {
		t.Fatal(renderErr)
	}
	if stdout != "" {
		t.Errorf("config render leaked to process stdout: %q", stdout)
	}
	for _, want := range []string{"SELinux: containerd now confines containers", "WARN: registry: duplicate endpoint"} {
		if !strings.Contains(progress.String(), want) {
			t.Errorf("operation progress missing %q: %q", want, progress.String())
		}
	}
}

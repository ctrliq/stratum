package upgrade

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/version"
)

// File writes nested under upgrade and rollback may report a missing SELinux
// policy. Those reports belong to the operation, including backups and markers.
func TestUpgradeFileProgressUsesOperationWriter(t *testing.T) {
	for _, which := range []string{"install and backup", "rollback", "marker"} {
		t.Run(which, func(t *testing.T) {
			var progress bytes.Buffer
			oldLabel := labelLike
			t.Cleanup(func() { labelLike = oldLabel })
			labelLike = func(w io.Writer, _, dst string) error { fmt.Fprintf(w, "label warning for %s\n", dst); return nil }
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "next"), filepath.Join(dir, "live")
			writeFile(t, src, "new", 0755)
			writeFile(t, dst, "old", 0755)
			var opErr error
			stdout := captureRealStdout(t, func() {
				switch which {
				case "install and backup":
					_, opErr = backupAndInstall(&progress, dst, src, resumeTS)
				case "rollback":
					if failed := restoreAll(&progress, []backupItem{{name: "binary", bak: src, dst: dst}}); len(failed) > 0 {
						opErr = fmt.Errorf("restore failed: %v", failed)
					}
				case "marker":
					oldData := version.DataDir
					version.DataDir = dir
					t.Cleanup(func() { version.DataDir = oldData })
					opErr = writeUpgradeMarker(&progress, upgradeMarker{TS: resumeTS, Target: "v1.37.9"})
				}
			})
			if opErr != nil {
				t.Fatal(opErr)
			}
			if stdout != "" {
				t.Errorf("nested file progress leaked to stdout: %q", stdout)
			}
			if !strings.Contains(progress.String(), "label warning for ") {
				t.Errorf("nested label warning absent from operation progress: %q", progress.String())
			}
		})
	}
}

func TestUpgradeConfigProgressUsesOperationWriter(t *testing.T) {
	hostConfigFixture(t, nil, nil, func(string) error { return nil })
	var progress bytes.Buffer
	writeNodeConfigs = func(w io.Writer, _ *nodestate.NodeState) error { fmt.Fprintln(w, "rendered node configs"); return nil }
	var opErr error
	stdout := captureRealStdout(t, func() { opErr = applyHostConfigs(&progress, &nodestate.NodeState{Role: "worker", Hostname: "w1"}) })
	if opErr != nil {
		t.Fatal(opErr)
	}
	if stdout != "" {
		t.Errorf("nested config progress leaked to stdout: %q", stdout)
	}
	if !strings.Contains(progress.String(), "rendered node configs") {
		t.Errorf("config progress absent from operation progress: %q", progress.String())
	}
}

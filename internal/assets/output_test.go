package assets

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestPayloadStageProgressUsesOperationWriter(t *testing.T) {
	exe := appendZip(t, map[string]string{"bin/kubelet": "kubelet"})
	var progress bytes.Buffer
	oldLabel := labelLike
	t.Cleanup(func() { labelLike = oldLabel })
	labelLike = func(w io.Writer, _, dst string) error {
		fmt.Fprintf(w, "WARN: %s is not labelled for SELinux\n", dst)
		return nil
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = saved; r.Close(); w.Close() })
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	_, err = StageFromWithWriter(&progress, exe, t.TempDir())
	os.Stdout = saved
	w.Close()
	stdout := <-done
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Errorf("payload staging leaked to process stdout: %q", stdout)
	}
	if !strings.Contains(progress.String(), "is not labelled for SELinux") {
		t.Errorf("payload label warning absent from progress: %q", progress.String())
	}
}

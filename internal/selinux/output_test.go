package selinux

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func captureCleanupStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old; r.Close(); w.Close() })
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	os.Stdout = old
	w.Close()
	return <-done
}

func TestSELinuxProgressUsesOperationWriter(t *testing.T) {
	t.Run("remove contexts", func(t *testing.T) {
		fakeHost(t, Enforcing)
		realRunPaths(t)
		newFakePolicy(t, el9)
		if err := Ensure(Options{}); err != nil {
			t.Fatal(err)
		}
		var progress bytes.Buffer
		var cleanupErr error
		stdout := captureCleanupStdout(t, func() { cleanupErr = RemoveContextsWithWriter(&progress) })
		if cleanupErr != nil {
			t.Fatal(cleanupErr)
		}
		if stdout != "" {
			t.Errorf("context cleanup leaked to process stdout: %q", stdout)
		}
		for _, want := range []string{"removed the file context ", "removed the file context equivalence "} {
			if !strings.Contains(progress.String(), want) {
				t.Errorf("progress missing %q: %q", want, progress.String())
			}
		}
	})
	t.Run("missing policy warning", func(t *testing.T) {
		fakeHost(t, Enforcing)
		writeStamp(t)
		p := newFakePolicy(t, el9)
		p.module = false
		p.override = func(name string, _ []string) (string, error, bool) {
			return "", errors.New("chcon: Invalid argument"), name == "chcon"
		}
		var progress bytes.Buffer
		var labelErr error
		stdout := captureCleanupStdout(t, func() { labelErr = LabelLikeWithWriter(&progress, "/x/.f.tmp-1", "/x/f") })
		if labelErr != nil {
			t.Fatal(labelErr)
		}
		if stdout != "" {
			t.Errorf("label warning leaked to process stdout: %q", stdout)
		}
		if !strings.Contains(progress.String(), "WARN: /x/f is not labelled for SELinux") {
			t.Errorf("warning lost from progress: %q", progress.String())
		}
	})
}

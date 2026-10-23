//go:build unix

package kube

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Replace an input after its reader opens it, but before that read completes.
// The open reader receives the old bytes; every later read sees the replacement.
// This pins rotations during resolution without depending on a scheduler delay.
func clusterAcrossReplacement(t *testing.T, conn *Connector, path, old, replacement string) *Cluster {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	staged := path + ".new"
	if err := os.WriteFile(staged, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		cluster *Cluster
		err     error
	}
	done := make(chan result, 1)
	go func() {
		cl, err := conn.Cluster("")
		done <- result{cl, err}
	}()

	var writer *os.File
	deadline := time.Now().Add(5 * time.Second)
	for writer == nil {
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			writer = os.NewFile(uintptr(fd), path)
			break
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatal(err)
		}
		select {
		case r := <-done:
			t.Fatalf("resolution returned before reading the input: %v", r.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("resolution did not open the input")
		}
		time.Sleep(time.Millisecond)
	}
	defer writer.Close()
	if err := os.Rename(staged, path); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(old); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Cluster across replacement: %v", r.err)
		}
		return r.cluster
	case <-time.After(5 * time.Second):
		t.Fatal("resolution did not finish after the input was replaced")
		return nil
	}
}

func TestCluster_ReplacementDuringResolutionUsesMatchingSnapshot(t *testing.T) {
	t.Run("kubeconfig", func(t *testing.T) {
		isolateHome(t)
		path := filepath.Join(t.TempDir(), "config")
		conn := NewConnector(Options{Kubeconfig: path})
		cl := clusterAcrossReplacement(t, conn, path,
			kubeconfigWithServer("https://192.0.2.1:6443"),
			kubeconfigWithServer("https://192.0.2.2:6443"))
		if cl.Host != "https://192.0.2.2:6443" {
			t.Errorf("Host = %q, want replacement server", cl.Host)
		}
		again, err := conn.Cluster("")
		if err != nil || again != cl || again.Host != "https://192.0.2.2:6443" {
			t.Errorf("stable resolution = %p, %v; want reuse of the replacement", again, err)
		}
	})
	t.Run("token file", func(t *testing.T) {
		isolateHome(t)
		dir := t.TempDir()
		token := filepath.Join(dir, "token")
		path := writeReferencingKubeconfig(t, dir, "config", "https://192.0.2.1:6443", "", "", "", token)
		conn := NewConnector(Options{Kubeconfig: path})
		cl := clusterAcrossReplacement(t, conn, token, "old-token", "new-token")
		if cl.Config.BearerToken != "new-token" {
			t.Errorf("BearerToken = %q, want replacement token", cl.Config.BearerToken)
		}
		again, err := conn.Cluster("")
		if err != nil || again != cl || again.Config.BearerToken != "new-token" {
			t.Errorf("stable resolution = %p, %v; want reuse of the replacement", again, err)
		}
	})
}

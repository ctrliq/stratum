package pki

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/version"
)

func TestCertificateReissueProgressUsesOperationWriter(t *testing.T) {
	old := version.PKIDir
	t.Cleanup(func() { version.PKIDir = old })
	version.PKIDir = t.TempDir()
	if err := GenerateAll(tHost, tIP, nil); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldOut; r.Close(); w.Close() })
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	var progress bytes.Buffer
	err = GenerateAllWithWriter(&progress, tHost, tIP, &APISANs{IPs: []string{"192.0.2.100"}})
	os.Stdout = oldOut
	w.Close()
	stdout := <-done
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Errorf("PKI reissue leaked to process stdout: %q", stdout)
	}
	if !strings.Contains(progress.String(), "apiserver.crt does not cover IP:192.0.2.100; reissuing it") {
		t.Errorf("reissue progress lost: %q", progress.String())
	}
}

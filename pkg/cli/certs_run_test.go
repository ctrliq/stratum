package cli

import (
	"io"
	"testing"

	"github.com/ctrliq/stratum/internal/certs"
)

func TestCARotateNodeJobCarriesRunIdentity(t *testing.T) {
	saved := rotateCA
	t.Cleanup(func() { rotateCA = saved })
	var got string
	rotateCA = func(_ io.Writer, o certs.CARotateOptions) error { got = o.RunID; return nil }
	cmd := newCertsRotateCACommand()
	cmd.SetArgs([]string{"--phase=finalize", "--tokens-checked", "--rotation-run=run-42", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got != "run-42" {
		t.Fatalf("run identity = %q", got)
	}
}

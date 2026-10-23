package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/certs"
	"github.com/ctrliq/stratum/internal/version"
)

func TestScheduledRenewalPreservesWarnings(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			capture, err := os.CreateTemp(t.TempDir(), "renewal-log")
			if err != nil {
				t.Fatal(err)
			}
			oldStdout, oldRenew := os.Stdout, renewCertificates
			t.Cleanup(func() {
				os.Stdout, renewCertificates = oldStdout, oldRenew
				capture.Close()
			})
			os.Stdout = capture
			failure := errors.New("rotation failed")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			const warning = "  WARN: restart kubelet: supervisor unavailable (continuing)\n"
			renewCertificates = func(got context.Context, w io.Writer, opts certs.Options) (bool, error) {
				if got != ctx {
					t.Error("renewal did not pass its context to rotation")
				}
				if !opts.Quiet || opts.IfExpiringDays != version.CertRenewWindowDays {
					t.Errorf("renewal options = %+v", opts)
				}
				fmt.Fprint(w, warning)
				if failed {
					return false, failure
				}
				return true, nil
			}
			err = certRenewRotate(ctx)
			if failed && !errors.Is(err, failure) || !failed && err != nil {
				t.Fatalf("renewal error = %v, failed = %t", err, failed)
			}
			if _, err := capture.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			logs, err := io.ReadAll(capture)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(logs), warning) {
				t.Fatalf("scheduled renewal discarded its warning: %q", logs)
			}
		})
	}
}

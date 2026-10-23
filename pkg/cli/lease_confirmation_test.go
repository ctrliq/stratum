package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestControlPlaneMintStopsAfterLosingMaintenanceLease(t *testing.T) {
	for _, loseAt := range []string{"mint", "guard", "key"} {
		t.Run(loseAt, func(t *testing.T) {
			var out bytes.Buffer
			var revoked []string
			c := okCmd(&out, &revoked)
			guard := &maintlease.Guard{}
			released, uploaded, minted := false, false, false
			c.maintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
				return &k8sutil.MaintenanceLease{Guard: guard, Release: func() { released = true }}, nil
			}
			lose := func(at string) {
				if at == loseAt {
					guard.Observe(maintlease.ErrTaken, time.Now())
				}
			}
			create := c.create
			c.create = func(w io.Writer, url string, ttl time.Duration, cp bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
				minted = true
				lose("mint")
				return create(w, url, ttl, cp, nil)
			}
			c.rekeyGuard = func() error { lose("guard"); return nil }
			c.newCertKey = func() (string, error) { lose("key"); return testCertKey, nil }
			c.uploadCerts = func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
				uploaded = true
				return nil
			}
			err := createToken("https://cp:6443", true, time.Hour, c)
			wantRevoked := 1
			if loseAt == "guard" {
				wantRevoked = 0 // guard and its lease confirmation precede publication
			}
			if !errors.Is(err, maintlease.ErrTaken) || uploaded || len(revoked) != wantRevoked || minted != (loseAt != "guard") || out.Len() != 0 || !released {
				t.Fatalf("lease loss: err=%v minted=%v uploaded=%v revoked=%v output=%q released=%v", err, minted, uploaded, revoked, out.String(), released)
			}
		})
	}
}

func TestExplicitRekeyStopsAfterLosingMaintenanceLease(t *testing.T) {
	for _, loseAt := range []string{"guard", "key", "upload"} {
		t.Run(loseAt, func(t *testing.T) {
			oldFence, oldGuard, oldKey, oldUpload := rekeyMaintenanceFence, rekeyGuard, rekeyNewCertKey, rekeySharedCerts
			oldWrite := rekeyWriteKeyFile
			t.Cleanup(func() {
				rekeyMaintenanceFence, rekeyGuard, rekeyNewCertKey, rekeySharedCerts = oldFence, oldGuard, oldKey, oldUpload
				rekeyWriteKeyFile = oldWrite
			})
			guard := &maintlease.Guard{}
			released, uploaded, wrote := false, false, false
			var out bytes.Buffer
			rekeyMaintenanceFence = func() (*k8sutil.MaintenanceLease, error) {
				return &k8sutil.MaintenanceLease{Guard: guard, Release: func() { released = true }}, nil
			}
			lose := func(at string) {
				if at == loseAt {
					guard.Observe(maintlease.ErrTaken, time.Now())
				}
			}
			rekeyGuard = func() error { lose("guard"); return nil }
			rekeyNewCertKey = func() (string, error) { lose("key"); return testCertKey, nil }
			rekeySharedCerts = func(_ string, _ func(context.Context) error) error { uploaded = true; lose("upload"); return nil }
			rekeyWriteKeyFile = func(string) (string, error) { wrote = true; return "/tmp/unused-certificate-key", nil }
			cmd := newCertsRotateCommand()
			cmd.SetArgs([]string{"--rekey-shared", "--yes"})
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			err := cmd.ExecuteContext(context.Background())
			if !errors.Is(err, maintlease.ErrTaken) || uploaded != (loseAt == "upload") || wrote || out.Len() != 0 || !released {
				t.Fatalf("lease loss: err=%v uploaded=%v released=%v", err, uploaded, released)
			}
		})
	}
}

package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTokenCreateStopsAfterLosingMaintenanceLease(t *testing.T) {
	// A worker mint holds the fence too (every mint republishes cluster-info
	// from local PKI), so its fence Confirm is checked at the same seams —
	// minus the certificate phase, which only a control-plane mint runs.
	for _, cp := range []bool{false, true} {
		losePoints := []string{"mint", "guard"}
		if cp {
			losePoints = append(losePoints, "key")
		}
		for _, loseAt := range losePoints {
			t.Run(fmt.Sprintf("controlPlane=%v/%s", cp, loseAt), func(t *testing.T) {
				fakeStratumHost(t)
				guard := &maintlease.Guard{}
				uploaded, released, minted := false, false, false
				var revoked string
				lose := func(at string) {
					if at == loseAt {
						guard.Observe(maintlease.ErrTaken, time.Now())
					}
				}
				deps := tokenDeps{
					maintenanceFence: func() (*k8sutil.MaintenanceLease, error) {
						return &k8sutil.MaintenanceLease{Guard: guard, Release: func() { released = true }}, nil
					},
					create: func(_ io.Writer, _ string, _ time.Duration, _ bool, _ func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
						minted = true
						lose("mint")
						return "abcdef.0123456789abcdef", "sha256:aa", nil, nil
					},
					rekeyGuard: func() error { lose("guard"); return nil },
					newCertKey: func() (string, error) { lose("key"); return "certificate-key", nil },
					uploadCerts: func(_ string, _ *metav1.OwnerReference, _ func(context.Context) error) error {
						uploaded = true
						return nil
					},
					nodeIP: func() (string, error) { return "10.0.0.1", nil },
					revoke: func(tok string, err error) error { revoked = tok; return err },
				}
				_, out, err := tokenCreateWith(deps)(context.Background(), nil, TokenCreateIn{ControlPlane: cp})
				wantRevoked := "abcdef.0123456789abcdef"
				if loseAt == "guard" {
					wantRevoked = "" // guard and its lease confirmation precede publication
				}
				if !errors.Is(err, maintlease.ErrTaken) || uploaded || revoked != wantRevoked || minted != (loseAt != "guard") || out.Token != "" || !released {
					t.Fatalf("lease loss: err=%v minted=%v uploaded=%v revoked=%q token=%q released=%v", err, minted, uploaded, revoked, out.Token, released)
				}
			})
		}
	}
}

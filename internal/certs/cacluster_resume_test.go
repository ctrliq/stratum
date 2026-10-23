package certs

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// workerResumeCluster uses the real worker trust-transition rules against the
// initiator's published bundle. Unlike the control-plane-only fixture, its
// worker cannot replay trust-new after a control plane publishes new-only trust.
type workerResumeCluster struct {
	*fakeCACluster
	workerTrust []*x509.Certificate
	workerCalls []string
}

func (f *workerResumeCluster) Survey() ([]cnode, error) {
	nodes, err := f.fakeCACluster.Survey()
	return append(nodes, cnode{Name: "worker1"}), err
}

func (f *workerResumeCluster) publishedTrust() []*x509.Certificate {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dirs[f.initiator()], "ca-bundle.crt"))
	if err != nil {
		f.t.Fatal(err)
	}
	ca, err := parseTrustBundle("published cluster-info", b)
	if err != nil {
		f.t.Fatal(err)
	}
	return ca
}

func (f *workerResumeCluster) RunPhase(n cnode, phase string, fetchShared, force bool) error {
	if n.Name != "worker1" {
		return f.fakeCACluster.RunPhase(n, phase, fetchShared, force)
	}
	f.workerCalls = append(f.workerCalls, phase)
	if phase == PhaseSignNew {
		if len(f.workerTrust) != 2 {
			return errors.New("worker sign-new requires widened trust")
		}
		return nil
	}
	intent := trustWiden
	if phase == PhaseFinalize {
		intent = trustNarrow
	}
	fetched := f.publishedTrust()
	if _, _, err := checkTrustTransition(f.workerTrust, fetched, intent); err != nil {
		return err
	}
	f.workerTrust = fetched
	return nil
}

func TestPartialFinalizeResumeKeepsCompletedWorkerPhases(t *testing.T) {
	f := &workerResumeCluster{fakeCACluster: newFakeCACluster(t, "cp1", "cp2")}
	f.workerTrust = f.publishedTrust()
	f.failAt["cp2:"+PhaseFinalize] = true
	if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err == nil {
		t.Fatal("rotation completed despite the failed follower finalize")
	}
	if len(f.workerTrust) != 2 || len(f.publishedTrust()) != 1 {
		t.Fatalf("partial-finalize fixture: worker trusts %d roots, published bundle holds %d", len(f.workerTrust), len(f.publishedTrust()))
	}
	// This is the real rejection a phase-major replay would hit. A resume must
	// retain the global checkpoint rather than try that trust-new again.
	if _, _, err := checkTrustTransition(f.workerTrust, f.publishedTrust(), trustWiden); err == nil {
		t.Fatal("worker accepted narrowing its trust during trust-new")
	}
	f.workerCalls = nil
	delete(f.failAt, "cp2:"+PhaseFinalize)
	if err := (&caCluster{drv: f, o: CARotateOptions{Quiet: true}}).run(); err != nil {
		t.Fatalf("resume with a worker still trusting old and new roots: %v", err)
	}
	if got := strings.Join(f.workerCalls, ","); got != PhaseFinalize {
		t.Errorf("worker resume dispatched %q, want only finalize", got)
	}
	if len(f.workerTrust) != 1 || len(notIn(f.workerTrust, fingerprints(f.publishedTrust()))) != 0 {
		t.Fatal("worker did not finish on the control planes' new root")
	}
}

func TestResumedTokenGateScansAfterLocalOldKeyWasRemoved(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "clean mounted tokens"
		if stale {
			name = "old mounted token still blocks finalize"
		}
		t.Run(name, func(t *testing.T) {
			prepPinned(t)
			savedPKI, savedRoot := pkiDir, kubeletRoot
			t.Cleanup(func() { pkiDir, kubeletRoot = savedPKI, savedRoot })
			pkiDir, kubeletRoot = version.PKIDir, t.TempDir()
			fastGate(t)
			if err := pki.RotateCAMaterial(false); err != nil {
				t.Fatal(err)
			}
			if err := pki.PromoteCA(); err != nil {
				t.Fatal(err)
			}
			if err := pki.PromoteSA(); err != nil {
				t.Fatal(err)
			}
			kid, err := oldSAKeyID()
			if err != nil {
				t.Fatal(err)
			}
			cs := fake.NewSimpleClientset()
			d := &prodCADriver{w: io.Discard, ctx: context.Background(), cs: cs, confirm: func(string) error { return nil }}
			if _, _, err := d.ResolveRotation(); err != nil {
				t.Fatal(err)
			}
			if err := d.RecordPhase(PhaseSignNew); err != nil {
				t.Fatal(err)
			}
			if err := pki.FinalizeCA(); err != nil {
				t.Fatal(err)
			}
			// Production finalization records which shared run this node
			// finished, so another run's finalized marker cannot authorize it.
			markCarotateFinishedRun(io.Discard, d.rotationRun)
			if _, err := oldSAKeyID(); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("finalize did not remove local old public key: %v", err)
			}
			cm, err := cs.CoreV1().ConfigMaps(caNamespace).Get(context.Background(), caRotationStateName, metav1.GetOptions{})
			if err != nil || cm.Data["old-sa-kid"] != kid {
				t.Fatalf("checkpoint did not retain the old key identity: %v, %v", cm, err)
			}
			if stale {
				mount(t, kubeletRoot, "old-token-pod", "kube-api-access-x", jwtWithKid(kid))
				k := &kubelet{root: kubeletRoot, failure: errors.New("restart denied by fixture")}
				k.install(t)
			}
			// A new driver resolves the persisted run after the first control
			// plane finalized. Exercise production TokensChecked, not its fake.
			resumed := &prodCADriver{w: io.Discard, ctx: context.Background(), cs: cs, confirm: func(string) error { return nil }}
			if _, resume, err := resumed.ResolveRotation(); err != nil || !resume {
				t.Fatalf("resolve partial-finalize resume: resumed=%v, err=%v", resume, err)
			}
			err = resumed.TokensChecked([]cnode{{Name: "cp1", IsCP: true}}, "cp1")
			if stale {
				if err == nil || !strings.Contains(err.Error(), "restart denied by fixture") {
					t.Fatalf("old token was not scanned and blocked: %v", err)
				}
			} else if err != nil {
				t.Fatalf("clean token scan after old public key removal: %v", err)
			}
		})
	}
}

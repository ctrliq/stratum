package certs

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/pki"
)

func stubStagedKey(t *testing.T, staged func() (bool, error)) {
	t.Helper()
	old, oldMigration := carotateKeyStaged, carotateKeyMigration
	carotateKeyStaged = staged
	carotateKeyMigration = func() (string, error) { return "", nil }
	t.Cleanup(func() { carotateKeyStaged, carotateKeyMigration = old, oldMigration })
}

func TestGuardRekeyRefusesAMidRotationNodeBeforeAnyClusterRead(t *testing.T) {
	prep(t)
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	stubStagedKey(t, func() (bool, error) {
		t.Error("the cluster-wide staged-key read ran before the node-local phase read")
		return false, nil
	})

	err := GuardRekey()
	if err == nil || !strings.Contains(err.Error(), "CA rotation is in flight") {
		t.Fatalf("GuardRekey() = %v, want a refusal naming the in-flight CA rotation", err)
	}
	if !strings.Contains(err.Error(), "trust-new") {
		t.Errorf("refusal %q does not name the phase this node is in", err)
	}
}

func TestGuardRekeyRefusesAStagedRotationKey(t *testing.T) {
	prep(t)
	if got := RotationPhase(); got != "idle" {
		t.Fatalf("fixture phase = %q, want idle; the test would prove nothing", got)
	}
	stubStagedKey(t, func() (bool, error) { return true, nil })

	err := GuardRekey()
	if err == nil || !strings.Contains(err.Error(), "stratum-carotate") {
		t.Fatalf("GuardRekey() = %v, want a refusal naming the staged stratum-carotate key", err)
	}
}

func TestGuardRekeyFailsClosedWhenTheStagedKeyReadFails(t *testing.T) {
	prep(t)
	boom := errors.New("apiserver unreachable")
	stubStagedKey(t, func() (bool, error) { return false, boom })

	if err := GuardRekey(); !errors.Is(err, boom) {
		t.Fatalf("GuardRekey() = %v, want the staged-key read failure refused (fail closed)", err)
	}
}

func TestGuardRekeyAllowsAnIdleNodeWithNothingStaged(t *testing.T) {
	prep(t)
	stubStagedKey(t, func() (bool, error) { return false, nil })

	if err := GuardRekey(); err != nil {
		t.Fatalf("GuardRekey() = %v, want nil on an idle node with nothing staged", err)
	}
}

func TestGuardRekeyNamesTheForcedResumeForAForcedRotation(t *testing.T) {
	prepPinned(t)
	if err := pki.RotateCAMaterial(true); err != nil {
		t.Fatal(err)
	}
	stubStagedKey(t, func() (bool, error) { return false, nil })
	err := GuardRekey()
	if err == nil {
		t.Fatal("a mint went ahead on a node mid forced rotation")
	}
	if !strings.Contains(err.Error(), "rotate-ca --cluster --force --yes") {
		t.Errorf("refusal %q does not name the forced resume", err)
	}
	if strings.Contains(err.Error(), "roll it back") {
		t.Errorf("refusal %q offers a rollback, which refuses every forced rotation", err)
	}
}

func TestGuardRekeyNamesTheMigrationThatStagedTheKey(t *testing.T) {
	for _, tc := range []struct {
		operation, resume string
	}{
		{carotateOpMigrateEtcdCA, pki.EtcdMigrateCommand + " --cluster --resume --yes"},
		{carotateOpMigrateKubeletCA, pki.KubeletMigrateCommand + " --cluster --yes"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			prepPinned(t)
			key := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: caRotateKeySecret, Namespace: caNamespace},
				Data:       map[string][]byte{"certificate-key": []byte("k")},
			}
			key.Annotations = stagedKeyOwner{operation: tc.operation, run: "r"}.annotations()
			err := GuardRekeyWithClient(fake.NewSimpleClientset(key))
			if err == nil {
				t.Fatal("a mint went ahead while a migration's key is staged")
			}
			for _, want := range []string{tc.operation, tc.resume, "kubectl -n kube-system delete secret stratum-carotate"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal missing %q: %v", want, err)
				}
			}
			for _, wrong := range []string{"rotate-ca --cluster", "stratum-ca-rotation"} {
				if strings.Contains(err.Error(), wrong) {
					t.Errorf("refusal names %q, which does not apply to a migration's key: %v", wrong, err)
				}
			}
		})
	}
}

package certs

// The one re-key entry point, next to the write it protects: every path
// that mints a new certificate-key and re-encrypts stratum-certs — `certs
// rotate --rekey-shared`, `token create --control-plane`, the MCP
// token_create tool — consults GuardRekey first,
// because a re-key while a cluster CA rotation or etcd-CA migration is in
// flight strands the key staged in stratum-carotate that the flow's follower
// control planes decrypt stratum-certs with. The guard answers from two
// signals, the cheap one first: this node's rotation phase (local PKI files)
// and the cluster-wide stratum-carotate Secret. The orchestrators' own
// re-uploads do not consult it — they run under the maintenance lease and
// re-encrypt deliberately.

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/pki"
)

// stubStagedKey swaps the guard's cluster-wide staged-key seam and restores it.
// The staged key's owner reads as recording no migration, so a staged result
// gets the refusal that names both kinds of operation.
func stubStagedKey(t *testing.T, staged func() (bool, error)) {
	t.Helper()
	old, oldMigration := carotateKeyStaged, carotateKeyMigration
	carotateKeyStaged = staged
	carotateKeyMigration = func() (string, error) { return "", nil }
	t.Cleanup(func() { carotateKeyStaged, carotateKeyMigration = old, oldMigration })
}

// TestGuardRekeyRefusesAMidRotationNodeBeforeAnyClusterRead pins the cheap,
// node-local leg and its order: a rotation staged on THIS node refuses the
// re-key from the local phase read alone, before any apiserver call — the
// same ordering the --rekey-shared command's pin
// (TestRekeySharedRefusesWithARotationStagedOnDisk) observes end-to-end.
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

// TestGuardRekeyRefusesAStagedRotationKey pins the cluster-wide leg: an idle
// node must still refuse while the stratum-carotate Secret is staged, because
// RotationPhase reads only local PKI files and answers "idle" on a follower
// the initiator has not reached yet.
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

// TestGuardRekeyFailsClosedWhenTheStagedKeyReadFails: an apiserver failure
// must refuse the mint, never wave a re-key through on an unreadable guard.
func TestGuardRekeyFailsClosedWhenTheStagedKeyReadFails(t *testing.T) {
	prep(t)
	boom := errors.New("apiserver unreachable")
	stubStagedKey(t, func() (bool, error) { return false, boom })

	if err := GuardRekey(); !errors.Is(err, boom) {
		t.Fatalf("GuardRekey() = %v, want the staged-key read failure refused (fail closed)", err)
	}
}

// TestGuardRekeyAllowsAnIdleNodeWithNothingStaged: the guard is a gate, not a
// toll — an idle node with nothing staged re-keys.
func TestGuardRekeyAllowsAnIdleNodeWithNothingStaged(t *testing.T) {
	prep(t)
	stubStagedKey(t, func() (bool, error) { return false, nil })

	if err := GuardRekey(); err != nil {
		t.Fatalf("GuardRekey() = %v, want nil on an idle node with nothing staged", err)
	}
}

// TestGuardRekeyNamesTheForcedResumeForAForcedRotation: the node-local leg
// tells the operator how to finish the rotation it found, and for one started
// with --force that is the forced resume: without the flag the re-run refuses,
// and --rollback refuses every forced rotation.
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

// TestGuardRekeyNamesTheMigrationThatStagedTheKey: the staged key Secret is
// shared by rotate-ca and the two CA migrations, and its owner is recorded on
// it. A refusal for a key a migration staged must send the operator to that
// migration's resume, and to the delete of the Secret that is actually there
// if it was abandoned — not to `rotate-ca --cluster`, which refuses while
// another operation's key is staged, nor to a rotation record that does not
// exist.
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

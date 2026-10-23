package certs

import (
	"context"
	"fmt"
	"time"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/pki"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// carotateKeyStaged is the guard's cluster-wide seam: whether the
// stratum-carotate Secret or an unfinished rotation checkpoint is present. A package var so tests can stub
// the apiserver read; production reads the live cluster.
var carotateKeyStaged = func() (bool, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return rekeyMaterialInFlight(ctx, cs)
}

// carotateKeyMigration is the guard's seam for which CA migration, if any,
// staged the stratum-carotate Secret: its refusal then names that
// migration's way out instead of the rotation's. A package var for the same
// reason as carotateKeyStaged.
var carotateKeyMigration = func() (string, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return stagedKeyMigration(ctx, cs)
}

// stagedKeyMigration returns the operation recorded on the staged key Secret
// when it is a CA migration's — any owner other than rotate-ca — and "" when
// the Secret is absent, a rotation's, or records no owner.
func stagedKeyMigration(ctx context.Context, cs kubernetes.Interface) (string, error) {
	sec, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, caRotateKeySecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	op, _ := stagedKeyForeign(stagedKeyOwner{operation: carotateOpRotateCA}, sec)
	return op, nil
}

// A finalized initiator is locally idle and may already have removed its key,
// while other nodes still need finalize. Its shared checkpoint keeps new grants
// from admitting nodes outside that unfinished rotation's phase ordering.
func rekeyMaterialInFlight(ctx context.Context, cs kubernetes.Interface) (bool, error) {
	_, err := cs.CoreV1().Secrets(caNamespace).Get(ctx, caRotateKeySecret, metav1.GetOptions{})
	if err == nil {
		return true, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	return unfinishedCARotation(ctx, cs)
}

// unfinishedCARotation also catches a partially finalized run whose fetch key is gone.
func unfinishedCARotation(ctx context.Context, cs kubernetes.Interface) (bool, error) {
	cm, err := cs.CoreV1().ConfigMaps(caNamespace).Get(ctx, caRotationStateName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cm.Data["complete"] != "true", nil
}

// GuardRekey is the one re-key entry point for every path that mints a new
// certificate-key and re-encrypts stratum-certs — `certs rotate
// --rekey-shared`, `token create --control-plane` and the MCP token_create
// tool. A re-key while a cluster CA rotation or etcd-CA migration is in flight
// strands the key staged in stratum-carotate that the flow's follower control
// planes decrypt stratum-certs with, so the mint must refuse.
//
// The guard answers from two signals, the cheap one first: this node's
// rotation phase (local PKI files, which catch a rotation staged on THIS
// node), and — because RotationPhase() reads "idle" on a follower the
// initiator has not reached yet — the cluster-wide stratum-carotate Secret or
// an unfinished stratum-ca-rotation record, visible from any control plane:
// the record also covers a halted run whose key is gone, which no one is
// waiting out, so that refusal names the resume and the record to clear after
// a completed rollback. An unreadable staged-key check refuses the
// mint rather than wave it through: an apiserver failure is not evidence that
// nothing is staged.
//
// The orchestrators' own re-uploads (rotate-ca --cluster, migrate-etcd-ca) do
// not consult this guard: they run under the shared maintenance lease, stage
// the key themselves and re-encrypt deliberately.
func GuardRekey() error { return guardRekey(carotateKeyStaged, carotateKeyMigration) }

// GuardRekeyWithClient uses the caller's credentials for cluster-wide checks.
func GuardRekeyWithClient(cs kubernetes.Interface) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return guardRekey(func() (bool, error) { return rekeyMaterialInFlight(ctx, cs) },
		func() (string, error) { return stagedKeyMigration(ctx, cs) })
}

func guardRekey(stagedMaterial func() (bool, error), migration func() (string, error)) error {
	if phase := RotationPhase(); phase != "idle" {
		finish := "Finish it (`stratum certs rotate-ca --cluster --yes`) or roll it back first, then retry"
		if pki.ForcedRotation() {
			finish = "It was started with --force, so it cannot be rolled back: finish it with " +
				"`stratum certs rotate-ca --cluster --force --yes`, with the --restart-workloads or --skip-workload-check the first run had, then retry"
		}
		return fmt.Errorf("refusing while a cluster CA rotation is in flight (%s) — "+
			"that flow stages a key in stratum-carotate for its follower jobs to decrypt stratum-certs, "+
			"and re-encrypting the shared material now would lock them out. %s", phase, finish)
	}
	staged, err := stagedMaterial()
	if err != nil {
		return fmt.Errorf("cannot confirm no cluster CA rotation / etcd-CA migration is staging shared material: %w", err)
	}
	if staged {
		// The key's recorded owner picks the way out. One that cannot be read
		// still refuses, with the text that names both kinds of operation.
		if op, err := migration(); err == nil && op != "" {
			return migrationRekeyRefusal(op)
		}
		return fmt.Errorf("refusing — a cluster-wide CA rotation or etcd-CA migration has not finished: the staged key Secret "+
			"kube-system/stratum-carotate or an unfinished rotation record kube-system/stratum-ca-rotation exists, and re-encrypting "+
			"stratum-certs now would lock its follower control planes out. A run that is still going finishes and removes both itself. %s",
			unfinishedRotationExits)
	}
	return nil
}

// migrationRekeyRefusal is guardRekey's refusal for a key a CA migration
// staged: rotate-ca --cluster refuses while another operation's key is
// staged, so the way out is that migration's own resume, or — once it was
// abandoned — removing its key.
func migrationRekeyRefusal(op string) error {
	resume := "stratum certs " + op + " --cluster --yes"
	switch op {
	case carotateOpMigrateEtcdCA:
		resume = pki.EtcdMigrateCommand + " --cluster --resume --yes"
	case carotateOpMigrateKubeletCA:
		resume = pki.KubeletMigrateCommand + " --cluster --yes"
	}
	return fmt.Errorf("refusing — a cluster-wide %s run has not finished: it staged the key Secret %s/%s, and re-encrypting "+
		"stratum-certs now would lock its follower control planes out. A run that is still going finishes and removes it itself. "+
		"A halted run does not finish on its own: resume it with `%s` from a control plane. Only if it was abandoned, remove its staged key: "+
		"`kubectl -n %s delete secret %s`", op, caNamespace, caRotateKeySecret, resume, caNamespace, caRotateKeySecret)
}

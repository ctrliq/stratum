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

// A package var so tests can stub the apiserver read.
var carotateKeyStaged = func() (bool, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return rekeyMaterialInFlight(ctx, cs)
}

// A package var for the same reason as carotateKeyStaged.
var carotateKeyMigration = func() (string, error) {
	cs, err := k8sutil.AdminClientset()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return stagedKeyMigration(ctx, cs)
}

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

// A finalized initiator is idle with its key gone while other nodes still need finalize.
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

// Catches a partially finalized run whose staged key is already gone.
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

// Refuses a re-key while material is staged; rotate-ca --cluster and migrate-etcd-ca bypass it.
func GuardRekey() error { return guardRekey(carotateKeyStaged, carotateKeyMigration) }

// GuardRekey with the caller's clientset.
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
		// The staged key's owner picks the way out; an unreadable owner still refuses.
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

// rotate-ca refuses while another key is staged, so the refusal names that operation's resume.
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

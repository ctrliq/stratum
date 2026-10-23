package certs

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/version"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCAMigrationsRefuseUnfinishedRootRotationBeforeLocalChanges(t *testing.T) {
	for _, operation := range []string{"kubelet", "etcd"} {
		t.Run(operation, func(t *testing.T) {
			for _, unreadable := range []bool{false, true} {
				t.Run(map[bool]string{false: "active", true: "unreadable"}[unreadable], func(t *testing.T) {
					old := version.PKIDir
					version.PKIDir = t.TempDir()
					t.Cleanup(func() { version.PKIDir = old })
					me, err := host.Hostname()
					if err != nil {
						t.Fatal(err)
					}
					cs := fake.NewSimpleClientset(
						&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: me, Labels: map[string]string{k8sutil.ControlPlaneLabel: "true"}}},
						&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: caRotationStateName, Namespace: caNamespace}, Data: map[string]string{"run": "root-run", "phase": PhaseSignNew}},
					)
					boom := errors.New("cannot read shared rotation checkpoint")
					if unreadable {
						cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, boom })
					}
					build := func() (kubernetes.Interface, error) { return cs, nil }
					if operation == "kubelet" {
						err = MigrateKubeletClientCAClusterContext(context.Background(), io.Discard, KubeletCAOptions{}, build)
					} else {
						err = MigrateEtcdCAClusterContext(context.Background(), io.Discard, EtcdCAOptions{}, build)
					}
					if err == nil || (!unreadable && !strings.Contains(err.Error(), "rotation")) || (unreadable && !errors.Is(err, boom)) {
						t.Fatalf("expected rotation guard refusal, got %v", err)
					}
					if !unreadable {
						for _, want := range []string{"rotate-ca --cluster --yes", "delete configmap stratum-ca-rotation"} {
							if !strings.Contains(err.Error(), want) {
								t.Errorf("refusal %q does not name %q", err, want)
							}
						}
					}
					entries, err := os.ReadDir(version.PKIDir)
					if err != nil {
						t.Fatal(err)
					}
					if len(entries) != 0 {
						t.Fatalf("migration changed local PKI before refusal: %v", entries)
					}
				})
			}
		})
	}
}

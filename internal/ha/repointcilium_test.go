package ha

import (
	"context"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/version"
)

// The VIP reaches every Cilium container through the API, replacing the
// address the manifest rendered and leaving other variables alone.
func TestSetCiliumAPIServerPointsEveryContainerAtTheVIP(t *testing.T) {
	spec := func() corev1.PodSpec {
		return corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "config", Env: []corev1.EnvVar{
				{Name: "KUBERNETES_SERVICE_HOST", Value: "10.0.0.5"},
				{Name: "KEEP", Value: "me"},
			}}},
			Containers: []corev1.Container{{Name: "agent"}},
		}
	}
	cs := fake.NewSimpleClientset(
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "cilium", Namespace: "kube-system"},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: spec()}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cilium-operator", Namespace: "kube-system"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: spec()}}},
	)
	const vip = "fd00::10"
	if err := setCiliumAPIServer(context.Background(), cs, vip); err != nil {
		t.Fatal(err)
	}
	ds, err := cs.AppsV1().DaemonSets("kube-system").Get(context.Background(), "cilium", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := cs.AppsV1().Deployments("kube-system").Get(context.Background(), "cilium-operator", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(version.APIServerPort)
	for name, s := range map[string]corev1.PodSpec{"ds/cilium": ds.Spec.Template.Spec, "deploy/cilium-operator": dep.Spec.Template.Spec} {
		for _, c := range append(append([]corev1.Container{}, s.InitContainers...), s.Containers...) {
			env := map[string]string{}
			count := map[string]int{}
			for _, e := range c.Env {
				env[e.Name] = e.Value
				count[e.Name]++
			}
			if env["KUBERNETES_SERVICE_HOST"] != vip || count["KUBERNETES_SERVICE_HOST"] != 1 {
				t.Errorf("%s %s: KUBERNETES_SERVICE_HOST %q (%d entries), want one entry of %q", name, c.Name, env["KUBERNETES_SERVICE_HOST"], count["KUBERNETES_SERVICE_HOST"], vip)
			}
			if env["KUBERNETES_SERVICE_PORT"] != port {
				t.Errorf("%s %s: KUBERNETES_SERVICE_PORT %q, want %q", name, c.Name, env["KUBERNETES_SERVICE_PORT"], port)
			}
			if c.Name == "config" && env["KEEP"] != "me" {
				t.Errorf("%s %s: an unrelated variable was dropped", name, c.Name)
			}
		}
	}
}

// A cluster without the Cilium objects fails the repoint rather than
// reporting it done.
func TestSetCiliumAPIServerFailsWithoutCilium(t *testing.T) {
	if err := setCiliumAPIServer(context.Background(), fake.NewSimpleClientset(), "10.0.0.10"); err == nil {
		t.Fatal("repointing a cluster with no Cilium DaemonSet succeeded")
	}
}

package addons

import (
	"bytes"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func dashboardObjects(t *testing.T, m []byte) (*corev1.Service, *appsv1.Deployment) {
	t.Helper()
	if bytes.Contains(m, []byte("__STRATUM_")) {
		t.Fatalf("unsubstituted sentinel left in:\n%s", m)
	}
	var svc *corev1.Service
	var dep *appsv1.Deployment
	for _, doc := range strings.Split(string(m), "\n---\n") {
		var meta struct{ Kind string }
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatalf("parse: %v\n%s", err, doc)
		}
		switch meta.Kind {
		case "Service":
			svc = &corev1.Service{}
			if err := yaml.UnmarshalStrict([]byte(doc), svc); err != nil {
				t.Fatal(err)
			}
		case "Deployment":
			dep = &appsv1.Deployment{}
			if err := yaml.UnmarshalStrict([]byte(doc), dep); err != nil {
				t.Fatal(err)
			}
		}
	}
	if svc == nil || dep == nil {
		t.Fatal("the stack has no Service or no Deployment")
	}
	return svc, dep
}

func TestRenderDashboardDefaults(t *testing.T) {
	DashImage = "registry.example/ske/stratum-dash:9.9.9"
	t.Cleanup(func() { DashImage = "unknown" })

	svc, dep := dashboardObjects(t, RenderDashboard(DashboardSettings{NodePort: DefaultDashboardPort}))
	if svc.Spec.Type != corev1.ServiceTypeNodePort || svc.Spec.Ports[0].NodePort != DefaultDashboardPort {
		t.Errorf("service %s nodePort %d, want NodePort %d", svc.Spec.Type, svc.Spec.Ports[0].NodePort, DefaultDashboardPort)
	}
	if h := svc.Annotations[DashboardHostsKey]; h != "" {
		t.Errorf("serving hosts %q, want none", h)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != DashImage {
		t.Errorf("image %q, want %q", c.Image, DashImage)
	}
	want := []string{"--listen=:8443", "--tls-cert=/tls/tls.crt", "--tls-key=/tls/tls.key", "--session-key-file=/session/key", "--enable-node-shell=false"}
	if strings.Join(c.Args, " ") != strings.Join(want, " ") {
		t.Errorf("args %q, want %q", c.Args, want)
	}
	if s := dep.Spec.Template.Spec.Volumes[0].Secret.SecretName; s != DashboardTLSSecret {
		t.Errorf("tls secret %q, want %q", s, DashboardTLSSecret)
	}
	if dep.Namespace != DashboardNamespace || dep.Name != DashboardName {
		t.Errorf("deployment %s/%s", dep.Namespace, dep.Name)
	}
}

func TestRenderDashboardSettings(t *testing.T) {
	svc, dep := dashboardObjects(t, RenderDashboard(DashboardSettings{
		PublicURL:     "https://dash.example.com:8443",
		TLSSecretName: "my-cert",
		NodeShell:     true,
	}))
	if svc.Spec.Type != corev1.ServiceTypeClusterIP || svc.Spec.Ports[0].NodePort != 0 {
		t.Errorf("service %s nodePort %d, want ClusterIP", svc.Spec.Type, svc.Spec.Ports[0].NodePort)
	}
	if h := svc.Annotations[DashboardHostsKey]; h != "dash.example.com" {
		t.Errorf("serving hosts %q", h)
	}
	args := dep.Spec.Template.Spec.Containers[0].Args
	if got := strings.Join(args[4:], " "); got != "--public-url=https://dash.example.com:8443 --enable-node-shell=true" {
		t.Errorf("extra args %q", got)
	}
	if s := dep.Spec.Template.Spec.Volumes[0].Secret.SecretName; s != "my-cert" {
		t.Errorf("tls secret %q, want my-cert", s)
	}
}

func TestRenderDashboardTokenSecrets(t *testing.T) {
	kinds := func(m []byte) map[string]int {
		out := map[string]int{}
		for _, doc := range strings.Split(string(m), "\n---\n") {
			var meta struct {
				Kind     string
				Metadata struct{ Name string }
			}
			if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
				t.Fatalf("parse: %v\n%s", err, doc)
			}
			if meta.Kind != "" {
				out[meta.Kind]++
			}
		}
		return out
	}
	with := kinds(RenderDashboard(DashboardSettings{NodePort: DefaultDashboardPort, TokenSecrets: true}))
	without := kinds(RenderDashboard(DashboardSettings{NodePort: DefaultDashboardPort}))
	if with["Secret"] != len(DashboardTokenSecrets) || with["ClusterRoleBinding"] != 2 || with["ServiceAccount"] != 3 {
		t.Errorf("with token Secrets: %v", with)
	}
	if without["Secret"] != 0 || without["ClusterRoleBinding"] != 0 || without["ServiceAccount"] != 1 {
		t.Errorf("without token Secrets: %v", without)
	}
	m := string(RenderDashboard(DashboardSettings{TokenSecrets: true}))
	for secret, sa := range DashboardTokenSecrets {
		if !strings.Contains(m, "name: "+secret+"\n") || !strings.Contains(m, "kubernetes.io/service-account.name: "+sa+"\n") {
			t.Errorf("the stack does not carry %s for %s", secret, sa)
		}
	}
}

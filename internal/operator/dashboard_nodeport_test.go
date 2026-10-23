package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/addons"
)

func nodePortService(ns, name string, port int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{Name: "https", Port: 443, NodePort: port}}},
	}
}

// renderedDashboardService is the dashboard Service as the stack renders it.
func renderedDashboardService(t *testing.T, nodePort int) *unstructured.Unstructured {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(addons.RenderDashboard(addons.DashboardSettings{NodePort: nodePort})), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("the dashboard stack renders no Service")
			}
			t.Fatal(err)
		}
		if obj.GetKind() == "Service" {
			return obj
		}
	}
}

func serviceSpec(t *testing.T, obj *unstructured.Unstructured) (typ string, nodePorts []int64) {
	t.Helper()
	typ, _, _ = unstructured.NestedString(obj.Object, "spec", "type")
	ports, _, _ := unstructured.NestedSlice(obj.Object, "spec", "ports")
	for _, p := range ports {
		if n, ok, _ := unstructured.NestedInt64(p.(map[string]any), "nodePort"); ok {
			nodePorts = append(nodePorts, n)
		}
	}
	return typ, nodePorts
}

// TestYieldDashboardNodePort pins when the dashboard gives up its NodePort:
// only when another Service holds it, never for its own Service, and never for
// any object other than the dashboard Service.
func TestYieldDashboardNodePort(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		existing []runtime.Object
		obj      func(*testing.T) *unstructured.Unstructured
		wantType string
		wantPort bool
		wantHeld string
	}{
		{name: "a free port: applied as rendered",
			obj: func(t *testing.T) *unstructured.Unstructured { return renderedDashboardService(t, 30443) }, wantType: "NodePort", wantPort: true},
		{name: "its own Service holds the port: applied as rendered",
			existing: []runtime.Object{nodePortService(addons.DashboardNamespace, addons.DashboardName, 30443)},
			obj:      func(t *testing.T) *unstructured.Unstructured { return renderedDashboardService(t, 30443) }, wantType: "NodePort", wantPort: true},
		{name: "another Service holds the port: ClusterIP, naming the holder",
			existing: []runtime.Object{nodePortService("monitoring", "grafana", 30443)},
			obj:      func(t *testing.T) *unstructured.Unstructured { return renderedDashboardService(t, 30443) },
			wantType: "ClusterIP", wantHeld: "30443 by monitoring/grafana"},
		{name: "another Service holds a different port: applied as rendered",
			existing: []runtime.Object{nodePortService("monitoring", "grafana", 30444)},
			obj:      func(t *testing.T) *unstructured.Unstructured { return renderedDashboardService(t, 30443) }, wantType: "NodePort", wantPort: true},
		{name: "a LoadBalancer's healthCheckNodePort holds the port: ClusterIP, naming the holder",
			existing: []runtime.Object{&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ingress", Name: "lb"},
				Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyLocal,
					HealthCheckNodePort: 30443, Ports: []corev1.ServicePort{{Name: "http", Port: 80, NodePort: 31080}}},
			}},
			obj:      func(t *testing.T) *unstructured.Unstructured { return renderedDashboardService(t, 30443) },
			wantType: "ClusterIP", wantHeld: "30443 by ingress/lb"},
		{name: "nodePort 0 renders a ClusterIP Service: untouched",
			existing: []runtime.Object{nodePortService("monitoring", "grafana", 30443)},
			obj:      func(t *testing.T) *unstructured.Unstructured { return renderedDashboardService(t, 0) }, wantType: "ClusterIP"},
		{name: "a Service outside the dashboard: untouched",
			existing: []runtime.Object{nodePortService("monitoring", "grafana", 30443)},
			obj: func(t *testing.T) *unstructured.Unstructured {
				o := renderedDashboardService(t, 30443)
				o.SetNamespace("other")
				return o
			}, wantType: "NodePort", wantPort: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := k8sfake.NewClientset(tc.existing...)
			obj := tc.obj(t)
			msg, err := yieldDashboardNodePort(ctx, cs, obj)
			if err != nil {
				t.Fatal(err)
			}
			typ, ports := serviceSpec(t, obj)
			if typ != tc.wantType || (len(ports) > 0) != tc.wantPort {
				t.Errorf("type %q nodePorts %v, want type %q with a nodePort: %v", typ, ports, tc.wantType, tc.wantPort)
			}
			held := obj.GetAnnotations()[dashboardNodePortHeldKey]
			if held != tc.wantHeld {
				t.Errorf("annotation %s = %q, want %q", dashboardNodePortHeldKey, held, tc.wantHeld)
			}
			if (msg != "") != (tc.wantHeld != "") || (tc.wantHeld != "" && (!strings.Contains(msg, tc.wantHeld) || !strings.Contains(msg, "spec.dashboard.nodePort"))) {
				t.Errorf("message %q, want one naming %q and spec.dashboard.nodePort only when the port is held", msg, tc.wantHeld)
			}
		})
	}
}

// TestHeldDashboardNodePortStillAppliesTheStack pins that, with the dashboard
// NodePort held by another Service, the whole dashboard stack applies: the
// Service as a ClusterIP, and the Deployment after it. Applied as rendered, the
// apiserver would refuse the Service, and the refusal would stop the
// Deployment from applying and suspend pruning cluster-wide.
func TestHeldDashboardNodePortStillAppliesTheStack(t *testing.T) {
	r := deployFixture(t)
	if _, err := r.fakeCs.CoreV1().Services("monitoring").Create(context.Background(),
		nodePortService("monitoring", "grafana", 30443), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	applied := map[string]*unstructured.Unstructured{}
	r.fakeDyn.PrependReactor("patch", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pa := a.(clienttesting.PatchActionImpl)
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(pa.GetPatch(), obj); err != nil {
			return true, nil, err
		}
		if obj.GetKind() == "Service" && obj.GetNamespace() == addons.DashboardNamespace {
			if _, nps := serviceSpec(t, obj); len(nps) > 0 {
				return true, nil, errors.New("Service is invalid: spec.ports[0].nodePort: Invalid value: 30443: provided port is already allocated")
			}
		}
		applied[obj.GetKind()] = obj
		return true, obj, nil
	})
	data := addons.RenderDashboard(addons.DashboardSettings{NodePort: 30443, TokenSecrets: true})
	if _, _, err := r.d.applyManifestFile(context.Background(), data, addons.DashboardStack, true); err != nil {
		t.Fatalf("apply the dashboard stack: %v", err)
	}
	svc := applied["Service"]
	if svc == nil {
		t.Fatal("the dashboard Service was not applied")
	}
	if typ, nps := serviceSpec(t, svc); typ != "ClusterIP" || len(nps) != 0 {
		t.Errorf("dashboard Service applied as type %q with nodePorts %v, want a ClusterIP", typ, nps)
	}
	if applied["Deployment"] == nil {
		t.Error("the dashboard Deployment was not applied after its Service")
	}
}

// TestDashboardRegainsItsNodePortOnTheNextPass pins that a dashboard served on
// a ClusterIP is applied again every pass while its NodePort is held, logging
// the held port once, and is applied with its NodePort on the first pass after
// the port is freed, well inside the resync window that otherwise holds an
// unchanged file back.
func TestDashboardRegainsItsNodePortOnTheNextPass(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	holder := nodePortService("monitoring", "grafana", 30443)
	if _, err := r.fakeCs.CoreV1().Services("monitoring").Create(ctx, holder, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var services []*unstructured.Unstructured
	r.fakeDyn.PrependReactor("patch", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(a.(clienttesting.PatchActionImpl).GetPatch(), obj); err != nil {
			return true, nil, err
		}
		if obj.GetKind() == "Service" {
			services = append(services, obj)
		}
		return true, obj, nil
	})
	path := filepath.Join(r.dir, addons.DashboardStack, "dashboard.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, addons.RenderDashboard(addons.DashboardSettings{NodePort: 30443}), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	pass := func(want string) {
		t.Helper()
		before := len(services)
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(services) != before+1 {
			t.Fatalf("the pass applied %d dashboard Services, want 1", len(services)-before)
		}
		if typ, _ := serviceSpec(t, services[len(services)-1]); typ != want {
			t.Fatalf("the pass applied the dashboard Service as %q, want %q", typ, want)
		}
	}
	pass("ClusterIP")
	pass("ClusterIP")
	if n := strings.Count(logs.String(), "NodePort 30443 by monitoring/grafana is held"); n != 1 {
		t.Errorf("the held port was logged %d times over two passes, want once:\n%s", n, logs.String())
	}
	if err := r.fakeCs.CoreV1().Services("monitoring").Delete(ctx, "grafana", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	pass("NodePort")
	if !strings.Contains(logs.String(), "its NodePort is free again") {
		t.Errorf("the freed port was not logged:\n%s", logs.String())
	}
}

// TestDashboardRollbackAfterAYieldIsApplied replays a rollback inside the
// resync window: a rendering on a free port is applied and cached, a change to
// a held port is served on a ClusterIP, and the first rendering is restored.
// Its sum is the one cached before the yield, so unless the yield dropped that
// entry the pass would skip it and leave the dashboard on a ClusterIP with its
// own port free.
func TestDashboardRollbackAfterAYieldIsApplied(t *testing.T) {
	ctx := context.Background()
	r := deployFixture(t, cp("cp1", "10.0.0.11"))
	if _, err := r.fakeCs.CoreV1().Services("monitoring").Create(ctx,
		nodePortService("monitoring", "grafana", 30443), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var services []*unstructured.Unstructured
	r.fakeDyn.PrependReactor("patch", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(a.(clienttesting.PatchActionImpl).GetPatch(), obj); err != nil {
			return true, nil, err
		}
		if obj.GetKind() == "Service" {
			services = append(services, obj)
		}
		return true, obj, nil
	})
	path := filepath.Join(r.dir, addons.DashboardStack, "dashboard.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	pass := func(nodePort int, wantType string, wantPorts []int64) {
		t.Helper()
		if err := os.WriteFile(path, addons.RenderDashboard(addons.DashboardSettings{NodePort: nodePort}), 0o600); err != nil {
			t.Fatal(err)
		}
		before := len(services)
		r.reconcile(ctx, "10.0.0.1", "10.0.0.11")
		if len(services) != before+1 {
			t.Fatalf("nodePort %d: the pass applied %d dashboard Services, want 1", nodePort, len(services)-before)
		}
		if typ, ports := serviceSpec(t, services[len(services)-1]); typ != wantType || !slices.Equal(ports, wantPorts) {
			t.Fatalf("nodePort %d: applied as %q with nodePorts %v, want %q with %v", nodePort, typ, ports, wantType, wantPorts)
		}
	}
	pass(30444, "NodePort", []int64{30444})
	pass(30443, "ClusterIP", nil)
	pass(30444, "NodePort", []int64{30444})
}

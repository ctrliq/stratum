package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// An apply to a namespace or a CRD needs --allow-destructive, and the refusal
// gives the reason an apply is risky, not a delete's.
func TestIrreversibleKindsNeedTheDestructiveFlagToApply(t *testing.T) {
	for _, gvr := range []schema.GroupVersionResource{
		{Version: "v1", Resource: "namespaces"},
		{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
	} {
		err := WritePolicy{}.check(gvr, false)
		if err == nil || !strings.Contains(err.Error(), "--allow-destructive") {
			t.Errorf("apply %s at the write tier: %v, want a refusal naming --allow-destructive", gvr.Resource, err)
		} else if !strings.Contains(err.Error(), "rewriting") || strings.Contains(err.Error(), "deleting") {
			t.Errorf("apply %s at the write tier: %q gives a delete's reason, want an apply's", gvr.Resource, err)
		}
		if err := (WritePolicy{AllowDestructive: true}).check(gvr, false); err != nil {
			t.Errorf("apply %s with --allow-destructive: %v", gvr.Resource, err)
		}
	}
	for _, gvr := range []schema.GroupVersionResource{
		{Version: "v1", Resource: "persistentvolumes"},
		{Version: "v1", Resource: "persistentvolumeclaims"},
		{Version: "v1", Resource: "nodes"},
	} {
		if err := (WritePolicy{AllowDestructive: true}).check(gvr, false); err == nil {
			t.Errorf("apply %s with --allow-destructive was allowed; it is never writable here", gvr.Resource)
		}
	}
	for _, gvr := range []schema.GroupVersionResource{
		{Version: "v1", Resource: "configmaps"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
	} {
		if err := (WritePolicy{}).check(gvr, false); err != nil {
			t.Errorf("apply %s at the write tier: %v", gvr.Resource, err)
		}
	}
}

// A HelmChart, and the HelmChartConfig the operator merges into one, are
// refused at every tier, for every verb.
func TestHelmChartsAreNotWritableAtAnyTier(t *testing.T) {
	for _, resource := range []string{"helmcharts", "helmchartconfigs"} {
		gvr := schema.GroupVersionResource{Group: "stratum.ciq.com", Version: "v1", Resource: resource}
		for _, destructive := range []bool{false, true} {
			for _, deleting := range []bool{false, true} {
				err := WritePolicy{AllowDestructive: destructive}.check(gvr, deleting)
				if err == nil || !strings.Contains(err.Error(), "fixed policy") {
					t.Errorf("%s destructive=%v deleting=%v: %v, want the fixed refusal", resource, destructive, deleting, err)
				}
			}
		}
	}
}

// Every pod spec in a manifest is checked, wherever it is nested.
func TestPodSpecGuardsReachEveryEmbeddedPodSpec(t *testing.T) {
	containers := []any{map[string]any{"name": "c", "image": "busybox"}}
	hostPath := []any{map[string]any{"name": "root", "hostPath": map[string]any{"path": "/"}}}
	for _, tc := range []struct {
		name string
		obj  map[string]any
		want string
	}{
		{"a PodTemplate's template", map[string]any{
			"apiVersion": "v1", "kind": "PodTemplate",
			"template": map[string]any{"spec": map[string]any{"containers": containers, "hostPID": true}},
		}, "hostPID"},
		{"a pod spec inside a custom resource", map[string]any{
			"apiVersion": "example.com/v1", "kind": "Workers",
			"spec": map[string]any{"workers": []any{map[string]any{
				"template": map[string]any{"spec": map[string]any{"containers": containers, "volumes": hostPath}},
			}}},
		}, "hostPath"},
		{"a Deployment", map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": containers, "hostNetwork": true,
			}}},
		}, "hostNetwork"},
	} {
		err := checkPodTemplate(tc.obj)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want a refusal naming %s", tc.name, err, tc.want)
		}
	}

	ordinary := map[string]any{
		"apiVersion": "example.com/v1", "kind": "Workers",
		"spec": map[string]any{"workers": []any{map[string]any{
			"template": map[string]any{"spec": map[string]any{"containers": containers}},
		}}},
	}
	if err := checkPodTemplate(ordinary); err != nil {
		t.Errorf("an ordinary embedded pod spec was refused: %v", err)
	}
	schemaLike := map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"spec": map[string]any{"properties": map[string]any{
			"containers": map[string]any{"type": "array"},
			"hostPID":    map[string]any{"type": "boolean"},
		}},
	}
	if err := checkPodTemplate(schemaLike); err != nil {
		t.Errorf("a schema that only describes a pod spec was refused: %v", err)
	}
}

// A claim the cluster will create from a template (a StatefulSet's
// volumeClaimTemplates entry or an ephemeral volume) is made after this check,
// so the check runs against what such a claim could bind to among the PVs that
// exist now. Each refusal is pinned by a literal in this file, not by the
// helper the code uses, so a wording change is a test failure.
func TestClaimTemplatesCannotReachNodePathVolumes(t *testing.T) {
	refusal := "this server will not apply a pod spec that mounts %s: %s " +
		"PersistentVolume %q, whose %s source is a path on the node, so mounting it " +
		"reaches the node's own filesystem. This is a fixed policy, not a permission " +
		"that can be granted by a flag"
	templateWhat := fmt.Sprintf("the claims the set's pods will mount from volumeClaimTemplates entry %q", "data")
	ephemeralWhat := fmt.Sprintf("the claim the pod will mount from ephemeral volume %q", "scratch")
	// A template's set produces many claims, an ephemeral volume names one: the
	// pronoun in the refusal agrees with what the sentence mounts.
	templateBinds, ephemeralBinds := "they can bind to", "it can bind to"

	// nodePathPV returns a PersistentVolume with the given source, storage class
	// ("" leaves it class-less) and optional claim reservation.
	nodePathPV := func(name, class string, source corev1.PersistentVolumeSource, ref *corev1.ObjectReference) *corev1.PersistentVolume {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:               corev1.ResourceList{corev1.ResourceStorage: apiresource.MustParse("1Gi")},
				AccessModes:            []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				PersistentVolumeSource: source,
			},
		}
		if class != "" {
			pv.Spec.StorageClassName = class
		}
		pv.Spec.ClaimRef = ref
		return pv
	}
	claimRef := func(namespace, name string, uid types.UID) *corev1.ObjectReference {
		return &corev1.ObjectReference{Namespace: namespace, Name: name, UID: uid}
	}
	templateManifest := func(name string, meta map[string]any, spec map[string]any) map[string]any {
		m := map[string]any{"name": name}
		for k, v := range meta {
			m[k] = v
		}
		return map[string]any{"metadata": m, "spec": spec}
	}
	statefulSet := func(name string, templates ...map[string]any) map[string]any {
		list := make([]any, 0, len(templates))
		for _, tmpl := range templates {
			list = append(list, tmpl)
		}
		return map[string]any{
			"apiVersion": "apps/v1", "kind": "StatefulSet",
			"metadata": map[string]any{"name": name},
			"spec":     map[string]any{"volumeClaimTemplates": list},
		}
	}
	hostPath := corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}
	local := corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/mnt"}}
	nfs := corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "10.0.0.9", Path: "/exports"}}

	for _, tc := range []struct {
		name         string
		pvs          []*corev1.PersistentVolume
		obj          map[string]any
		want         string // exact refusal, "" for accepted
		wantHas      string // substring of the refusal (a wrapped failure)
		listFails    bool   // make the fake's PV list fail
		wantNoPVList bool   // assert the fake never lists PersistentVolumes
	}{
		{
			name: "a template naming a bound node-path volume is refused by name",
			pvs: []*corev1.PersistentVolume{
				nodePathPV("node-held", "local-storage", hostPath, claimRef("default", "held-claim", "uid-1")),
			},
			obj:  statefulSet("web", templateManifest("data", nil, map[string]any{"volumeName": "node-held"})),
			want: fmt.Sprintf(refusal, templateWhat, templateBinds, "node-held", "hostPath"),
		},
		{
			name: "a template whose class matches an unbound node-path volume is refused",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-cls", "local-storage", hostPath, nil)},
			obj:  statefulSet("web", templateManifest("data", nil, map[string]any{"storageClassName": "local-storage"})),
			want: fmt.Sprintf(refusal, templateWhat, templateBinds, "node-cls", "hostPath"),
		},
		{
			name: "a selector does not keep a matching node-path volume away",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-cls", "local-storage", hostPath, nil)},
			obj: statefulSet("web", templateManifest("data", nil, map[string]any{
				"storageClassName": "local-storage",
				"selector":         map[string]any{"matchLabels": map[string]any{"app": "web"}},
			})),
			want: fmt.Sprintf(refusal, templateWhat, templateBinds, "node-cls", "hostPath"),
		},
		{
			name: "an empty storageClassName matches a class-less node-path volume",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-bare", "", local, nil)},
			obj:  statefulSet("web", templateManifest("data", nil, map[string]any{"storageClassName": ""})),
			want: fmt.Sprintf(refusal, templateWhat, templateBinds, "node-bare", "local"),
		},
		{
			name: "a template with no class at all matches any node-path class",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-bare", "", local, nil)},
			obj:  statefulSet("web", templateManifest("data", nil, map[string]any{})),
			want: fmt.Sprintf(refusal, templateWhat, templateBinds, "node-bare", "local"),
		},
		{
			name: "the beta storage-class annotation supplies the template's class",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-cls", "local-storage", hostPath, nil)},
			obj: statefulSet("web", templateManifest("data",
				map[string]any{"annotations": map[string]any{corev1.BetaStorageClassAnnotation: "local-storage"}},
				map[string]any{"storageClassName": "fast"})),
			want: fmt.Sprintf(refusal, templateWhat, templateBinds, "node-cls", "hostPath"),
		},
		{
			name: "a volume pre-bound to a name the template could produce is refused",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-pre", "other", hostPath, claimRef("default", "data-web-3", ""))},
			obj:  statefulSet("web", templateManifest("data", nil, map[string]any{"storageClassName": "fast"})),
			want: fmt.Sprintf(refusal, templateWhat, templateBinds, "node-pre", "hostPath"),
		},
		{
			name: "an ephemeral volume whose class matches is refused",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-cls", "local-storage", hostPath, nil)},
			obj: map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": "worker"},
				"spec": map[string]any{
					"volumes": []any{map[string]any{
						"name":      "scratch",
						"ephemeral": map[string]any{"volumeClaimTemplate": map[string]any{"spec": map[string]any{"storageClassName": "local-storage"}}},
					}},
				},
			},
			want: fmt.Sprintf(refusal, ephemeralWhat, ephemeralBinds, "node-cls", "hostPath"),
		},
		{
			// <deployment>-<replicaset hash>-<random>-<volume>.
			name: "a Deployment's ephemeral volume reaching a volume pre-bound to its pod's claim name is refused",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-eph", "other", hostPath, claimRef("default", "web-7d9f6c-x1b2c-scratch", ""))},
			obj: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "web"},
				"spec": map[string]any{
					"template": map[string]any{"spec": map[string]any{
						"volumes": []any{map[string]any{
							"name":      "scratch",
							"ephemeral": map[string]any{"volumeClaimTemplate": map[string]any{"spec": map[string]any{"storageClassName": "fast"}}},
						}},
					}},
				},
			},
			want: fmt.Sprintf(refusal, ephemeralWhat, ephemeralBinds, "node-eph", "hostPath"),
		},
		{
			name: "a CronJob jobTemplate ephemeral volume naming a node-path volume is refused",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-cls", "local-storage", hostPath, nil)},
			obj: map[string]any{
				"apiVersion": "batch/v1", "kind": "CronJob",
				"metadata": map[string]any{"name": "nightly"},
				"spec": map[string]any{
					"jobTemplate": map[string]any{"spec": map[string]any{
						"template": map[string]any{"spec": map[string]any{
							"volumes": []any{map[string]any{
								"name":      "scratch",
								"ephemeral": map[string]any{"volumeClaimTemplate": map[string]any{"spec": map[string]any{"volumeName": "node-cls"}}},
							}},
						}},
					}},
				},
			},
			want: fmt.Sprintf(refusal, ephemeralWhat, ephemeralBinds, "node-cls", "hostPath"),
		},
		{
			name:      "a failed volume list is refused, not ignored",
			pvs:       []*corev1.PersistentVolume{nodePathPV("node-cls", "local-storage", hostPath, nil)},
			obj:       statefulSet("web", templateManifest("data", nil, map[string]any{"storageClassName": "local-storage"})),
			wantHas:   "list PersistentVolumes to check what a claim template could bind to",
			listFails: true,
		},
		{
			name: "a plain StatefulSet on a provisioned class is applied",
			pvs: []*corev1.PersistentVolume{
				nodePathPV("node-cls", "local-storage", hostPath, nil),
				nodePathPV("node-bare", "", local, nil),
				nodePathPV("node-held", "local-storage", hostPath, claimRef("default", "held-claim", "uid-1")),
			},
			obj: statefulSet("web", templateManifest("data", nil, map[string]any{"storageClassName": "local-path"})),
		},
		{
			name: "a classless template with only bound or network volumes is applied",
			pvs: []*corev1.PersistentVolume{
				nodePathPV("node-held", "local-storage", hostPath, claimRef("default", "held-claim", "uid-1")),
				nodePathPV("net-cls", "local-storage", nfs, nil),
			},
			obj: statefulSet("web", templateManifest("data", nil, map[string]any{})),
		},
		{
			name: "a class matching only bound or network volumes is applied",
			pvs: []*corev1.PersistentVolume{
				nodePathPV("node-held", "local-storage", hostPath, claimRef("default", "held-claim", "uid-1")),
				nodePathPV("net-cls", "local-storage", nfs, nil),
			},
			obj: statefulSet("web", templateManifest("data", nil, map[string]any{"storageClassName": "local-storage"})),
		},
		{
			name: "a volume pre-bound in another namespace is applied",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-pre", "other", hostPath, claimRef("other", "data-web-0", ""))},
			obj:  statefulSet("web", templateManifest("data", nil, map[string]any{"storageClassName": "fast"})),
		},
		{
			name: "a volume pre-bound to a name the template cannot produce is applied",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-pre", "other", hostPath, claimRef("default", "logs-web-0", ""))},
			obj:  statefulSet("web", templateManifest("data", nil, map[string]any{"storageClassName": "fast"})),
		},
		{
			name: "an object with no templates never lists PersistentVolumes",
			pvs:  []*corev1.PersistentVolume{nodePathPV("node-cls", "local-storage", hostPath, nil)},
			obj: map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "plain"},
				"spec": map[string]any{
					"template": map[string]any{"spec": map[string]any{
						"volumes": []any{map[string]any{
							"name":                  "d",
							"persistentVolumeClaim": map[string]any{"claimName": "not-created-yet"},
						}},
					}},
				},
			},
			wantNoPVList: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tc.pvs))
			for _, pv := range tc.pvs {
				objs = append(objs, pv)
			}
			cs := k8sfake.NewSimpleClientset(objs...)
			if tc.listFails {
				cs.PrependReactor("list", "persistentvolumes", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("list denied by the test credential")
				})
			}

			err := checkClaimedVolumes(context.Background(), cs, tc.obj, "default")
			switch {
			case tc.want != "":
				if err == nil || err.Error() != tc.want {
					t.Errorf("checkClaimedVolumes: %v, want exactly %q", err, tc.want)
				}
			case tc.wantHas != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantHas) {
					t.Errorf("checkClaimedVolumes: %v, want an error containing %q", err, tc.wantHas)
				}
			default:
				if err != nil {
					t.Errorf("checkClaimedVolumes refused an apply it must allow: %v", err)
				}
				if tc.wantNoPVList {
					for _, action := range cs.Actions() {
						if action.GetVerb() == "list" && action.GetResource().Resource == "persistentvolumes" {
							t.Errorf("PersistentVolumes were listed for an object with no claim templates: %v", action)
						}
					}
				}
			}
		})
	}
}

// A set's pods reuse an existing claim named <template>-<set>-<ordinal> by
// name, so a claim that exists and is bound is what the new pods mount, and
// the PV reservation it carries does not keep them off its volume. Each
// refusal is pinned by a literal here, as in the test above.
func TestClaimTemplatesCannotReuseAClaimBoundToANodePathVolume(t *testing.T) {
	refusal := "this server will not apply a pod spec that mounts claim %q: it is " +
		"bound to PersistentVolume %q, whose %s source is a path on the node, so mounting it " +
		"reaches the node's own filesystem. This is a fixed policy, not a permission " +
		"that can be granted by a flag"
	statefulSet := func(name string) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1", "kind": "StatefulSet",
			"metadata": map[string]any{"name": name},
			"spec": map[string]any{
				"volumeClaimTemplates": []any{
					map[string]any{"metadata": map[string]any{"name": "data"},
						// A provisioned class no PV below matches: only claim reuse can refuse.
						"spec": map[string]any{"storageClassName": "fast"}},
				},
			},
		}
	}
	hostPath := corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}
	nfs := corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "10.0.0.9", Path: "/exports"}}
	pv := func(name string, source corev1.PersistentVolumeSource, ref *corev1.ObjectReference) *corev1.PersistentVolume {
		out := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: source,
			},
		}
		out.Spec.ClaimRef = ref
		return out
	}
	claimRef := func(namespace, name string, uid types.UID) *corev1.ObjectReference {
		return &corev1.ObjectReference{Namespace: namespace, Name: name, UID: uid}
	}
	claim := func(name, volume string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: volume},
		}
	}
	withClass := func(v *corev1.PersistentVolume, class string) *corev1.PersistentVolume {
		v.Spec.StorageClassName = class
		return v
	}
	pendingWithClass := func(name, class string) *corev1.PersistentVolumeClaim {
		c := claim(name, "")
		c.Spec.StorageClassName = &class
		return c
	}
	withUID := func(c *corev1.PersistentVolumeClaim, uid types.UID) *corev1.PersistentVolumeClaim {
		c.UID = uid
		return c
	}
	// tagged sets the annotation a provisioner leaves on each PV it creates.
	tagged := func(v *corev1.PersistentVolume, provisioner string) *corev1.PersistentVolume {
		v.Annotations = map[string]string{"pv.kubernetes.io/provisioned-by": provisioner}
		return v
	}
	// at points v's hostPath or local source at dir.
	at := func(v *corev1.PersistentVolume, dir string) *corev1.PersistentVolume {
		if v.Spec.HostPath != nil {
			v.Spec.HostPath = &corev1.HostPathVolumeSource{Path: dir}
		} else {
			v.Spec.Local = &corev1.LocalVolumeSource{Path: dir}
		}
		return v
	}
	// provisioned is what local-path makes for the claim v is reserved for:
	// its annotation and /opt/local-path-provisioner/<pv>_<namespace>_<claim>.
	provisioned := func(v *corev1.PersistentVolume) *corev1.PersistentVolume {
		ref := v.Spec.ClaimRef
		return at(tagged(v, "rancher.io/local-path"),
			"/opt/local-path-provisioner/"+v.Name+"_"+ref.Namespace+"_"+ref.Name)
	}
	local := corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: "/mnt"}}
	podMounting := func(claim string) map[string]any {
		return map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": "reader"},
			"spec": map[string]any{
				"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
				"volumes": []any{map[string]any{"name": "data",
					"persistentVolumeClaim": map[string]any{"claimName": claim}}},
			},
		}
	}
	podWithEphemeral := func(pod, volume string) map[string]any {
		return map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": pod},
			"spec": map[string]any{
				"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
				"volumes": []any{map[string]any{"name": volume, "ephemeral": map[string]any{
					"volumeClaimTemplate": map[string]any{"spec": map[string]any{"storageClassName": "fast"}},
				}}},
			},
		}
	}
	// A workload whose pod template has an ephemeral volume; meta is the
	// workload's metadata.
	workloadWithEphemeral := func(kind string, meta map[string]any, volume string) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1", "kind": kind,
			"metadata": meta,
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"containers": []any{map[string]any{"name": "c", "image": "busybox"}},
				"volumes": []any{map[string]any{"name": volume, "ephemeral": map[string]any{
					"volumeClaimTemplate": map[string]any{"spec": map[string]any{"storageClassName": "fast"}},
				}}},
			}}},
		}
	}
	named := func(name string) map[string]any { return map[string]any{"name": name} }
	// The API server keeps 58 characters of a generateName, so a long
	// Deployment's pods start with only the first 58 of "<name>-".
	longName := strings.Repeat("a", 60)
	longPod := strings.Repeat("a", 58) + "x1b2c"

	for _, tc := range []struct {
		name      string
		pvs       []*corev1.PersistentVolume
		claims    []*corev1.PersistentVolumeClaim
		obj       map[string]any
		want      string // exact refusal, "" for accepted
		wantHas   string // substring of the refusal (a wrapped failure)
		listFails bool   // make the fake's PVC list fail
		directGet bool   // a claim mounted by name reads its own PV
	}{
		{
			// The hole itself: the PV is reserved by a claim with a uid, so the
			// PV reservation is not what refuses, the bound claim is.
			name:   "a bound claim the template could produce is refused",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "data-web-0", "uid-1"))},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-0", "pv-node")},
			obj:    statefulSet("web"),
			want:   fmt.Sprintf(refusal, "data-web-0", "pv-node", "hostPath"),
		},
		{
			name:   "a bound claim on network storage the template could produce is applied",
			pvs:    []*corev1.PersistentVolume{pv("pv-net", nfs, nil)},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-0", "pv-net")},
			obj:    statefulSet("web"),
		},
		{
			name:   "a bound claim the template cannot produce is applied",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, nil)},
			claims: []*corev1.PersistentVolumeClaim{claim("logs-web-0", "pv-node")},
			obj:    statefulSet("web"),
		},
		{
			name:   "an unbound claim the template could produce is applied",
			pvs:    []*corev1.PersistentVolume{pv("pv-net", nfs, nil)},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-0", "")},
			obj:    statefulSet("web"),
		},
		{
			// The set reuses the pending claim as it is, so its own class, not
			// the template's, decides which PV it can still bind.
			name:   "a pending claim whose own class matches a node-path PV is refused",
			pvs:    []*corev1.PersistentVolume{withClass(pv("pv-node", hostPath, nil), "local-storage")},
			claims: []*corev1.PersistentVolumeClaim{pendingWithClass("data-web-0", "local-storage")},
			obj:    statefulSet("web"),
			want: "this server will not apply a pod spec that mounts claim \"data-web-0\": it can " +
				"bind to PersistentVolume \"pv-node\", whose hostPath source is a path on the node, so mounting " +
				"it reaches the node's own filesystem. This is a fixed policy, not a permission that can be " +
				"granted by a flag",
		},
		{
			// Binding writes the PV's claimRef (with the claim's uid) before the
			// claim's volumeName, so a pending claim can already hold a
			// reservation that carries its uid.
			name:   "a pending claim a node-path PV is already reserved for by uid is refused",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "data-web-0", "uid-pending"))},
			claims: []*corev1.PersistentVolumeClaim{withUID(claim("data-web-0", ""), "uid-pending")},
			obj:    statefulSet("web"),
			want: "this server will not apply a pod spec that mounts claim \"data-web-0\": it can " +
				"bind to PersistentVolume \"pv-node\", whose hostPath source is a path on the node, so mounting " +
				"it reaches the node's own filesystem. This is a fixed policy, not a permission that can be " +
				"granted by a flag",
		},
		{
			// A named Pod's ephemeral claim can only be <pod>-<volume>.
			name:   "a named Pod is applied beside another Pod's node-path claim of the same volume name",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "a-scratch", "uid-a"))},
			claims: []*corev1.PersistentVolumeClaim{claim("a-scratch", "pv-node")},
			obj:    podWithEphemeral("b", "scratch"),
		},
		{
			name:   "a named Pod whose own ephemeral claim is bound to a node-path PV is refused",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "a-scratch", "uid-a"))},
			claims: []*corev1.PersistentVolumeClaim{claim("a-scratch", "pv-node")},
			obj:    podWithEphemeral("a", "scratch"),
			want:   fmt.Sprintf(refusal, "a-scratch", "pv-node", "hostPath"),
		},
		{
			// Set web makes data-web-<ordinal>; data-web-cache-0 is set web-cache's.
			name:   "a set is applied beside a node-path claim of a set whose name extends its own",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "data-web-cache-0", "uid-c"))},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-cache-0", "pv-node")},
			obj:    statefulSet("web"),
		},
		{
			name: "a set is applied beside a node-path PV reserved for another set's claim",
			pvs:  []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "data-web-cache-0", ""))},
			obj:  statefulSet("web"),
		},
		{
			// The controller writes ordinals with strconv.Itoa: no leading zero.
			name: "a set is applied beside a node-path PV reserved for an ordinal it never writes",
			pvs:  []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "data-web-01", ""))},
			obj:  statefulSet("web"),
		},
		{
			name:   "a set's ephemeral volume is applied beside another set's node-path claim of that volume",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "other-0-scratch", "uid-o"))},
			claims: []*corev1.PersistentVolumeClaim{claim("other-0-scratch", "pv-node")},
			obj:    workloadWithEphemeral("StatefulSet", named("web"), "scratch"),
		},
		{
			name:   "a set's ephemeral volume whose own pod's claim is bound to a node-path PV is refused",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "web-0-scratch", "uid-w"))},
			claims: []*corev1.PersistentVolumeClaim{claim("web-0-scratch", "pv-node")},
			obj:    workloadWithEphemeral("StatefulSet", named("web"), "scratch"),
			want:   fmt.Sprintf(refusal, "web-0-scratch", "pv-node", "hostPath"),
		},
		{
			name:   "a Deployment's ephemeral volume is applied beside another workload's node-path claim of that volume",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "other-7d9f6c-x1b2c-scratch", "uid-o"))},
			claims: []*corev1.PersistentVolumeClaim{claim("other-7d9f6c-x1b2c-scratch", "pv-node")},
			obj:    workloadWithEphemeral("Deployment", named("web"), "scratch"),
		},
		{
			name:   "a Deployment's ephemeral volume whose pod's claim is bound to a node-path PV is refused",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "web-7d9f6c-x1b2c-scratch", "uid-w"))},
			claims: []*corev1.PersistentVolumeClaim{claim("web-7d9f6c-x1b2c-scratch", "pv-node")},
			obj:    workloadWithEphemeral("Deployment", named("web"), "scratch"),
			want:   fmt.Sprintf(refusal, "web-7d9f6c-x1b2c-scratch", "pv-node", "hostPath"),
		},
		{
			name:   "a long-named Deployment's pod claim, cut where the API server cuts it, is refused",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", longPod+"-scratch", "uid-l"))},
			claims: []*corev1.PersistentVolumeClaim{claim(longPod+"-scratch", "pv-node")},
			obj:    workloadWithEphemeral("Deployment", named(longName), "scratch"),
			want:   fmt.Sprintf(refusal, longPod+"-scratch", "pv-node", "hostPath"),
		},
		{
			// With no name the pod's name is not known, so any <pod>-<volume> counts.
			name:   "a Pod with only generateName is refused by any node-path claim of its volume",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "a-scratch", "uid-a"))},
			claims: []*corev1.PersistentVolumeClaim{claim("a-scratch", "pv-node")},
			obj:    workloadWithEphemeral("Pod", map[string]any{"generateName": "b-"}, "scratch"),
			want:   fmt.Sprintf(refusal, "a-scratch", "pv-node", "hostPath"),
		},
		{
			// A custom resource's controller names its pods as it likes.
			name:   "an unknown kind's ephemeral volume is refused by any node-path claim of that volume",
			pvs:    []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "a-scratch", "uid-a"))},
			claims: []*corev1.PersistentVolumeClaim{claim("a-scratch", "pv-node")},
			obj:    workloadWithEphemeral("Runner", named("web"), "scratch"),
			want:   fmt.Sprintf(refusal, "a-scratch", "pv-node", "hostPath"),
		},
		{
			name:   "a pending claim whose own class matches no node-path PV is applied",
			pvs:    []*corev1.PersistentVolume{withClass(pv("pv-node", hostPath, nil), "local-storage")},
			claims: []*corev1.PersistentVolumeClaim{pendingWithClass("data-web-0", "fast")},
			obj:    statefulSet("web"),
		},
		{
			// volumeName binds only that volume, so an unrelated node-path PV of
			// the same class cannot take the claim.
			name: "a template pinned to a network volume is applied beside a node-path PV of its class",
			pvs: []*corev1.PersistentVolume{
				withClass(pv("pv-nfs", nfs, nil), "shared"),
				withClass(pv("pv-node", hostPath, nil), "shared"),
			},
			obj: map[string]any{
				"apiVersion": "apps/v1", "kind": "StatefulSet",
				"metadata": map[string]any{"name": "web"},
				"spec": map[string]any{"volumeClaimTemplates": []any{
					map[string]any{"metadata": map[string]any{"name": "data"},
						"spec": map[string]any{"storageClassName": "shared", "volumeName": "pv-nfs"}},
				}},
			},
		},
		{
			// Every update of a set on local-path storage looks like this.
			name:   "a set whose claim is bound to a provisioned hostPath PV is applied",
			pvs:    []*corev1.PersistentVolume{provisioned(pv("pvc-1", hostPath, claimRef("default", "data-web-0", "uid-1")))},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-0", "pvc-1")},
			obj:    statefulSet("web"),
		},
		{
			name:   "a set scaling up onto a provisioned PV reserved for its pending claim is applied",
			pvs:    []*corev1.PersistentVolume{provisioned(pv("pvc-2", hostPath, claimRef("default", "data-web-1", "uid-2")))},
			claims: []*corev1.PersistentVolumeClaim{withUID(claim("data-web-1", ""), "uid-2")},
			obj:    statefulSet("web"),
		},
		{
			name:      "a pod mounting a claim bound to a provisioned hostPath PV is applied",
			pvs:       []*corev1.PersistentVolume{provisioned(pv("pvc-3", hostPath, claimRef("default", "data", "uid-3")))},
			claims:    []*corev1.PersistentVolumeClaim{claim("data", "pvc-3")},
			obj:       podMounting("data"),
			directGet: true,
		},
		{
			// local-path makes a local PV instead when a claim asks for one.
			name:   "a set whose claim is bound to a provisioned local PV is applied",
			pvs:    []*corev1.PersistentVolume{provisioned(pv("pvc-5", local, claimRef("default", "data-web-0", "uid-5")))},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-0", "pvc-5")},
			obj:    statefulSet("web"),
		},
		{
			// The static local-volume provisioner tags PVs over disks an
			// operator chose: the annotation says who made a PV, not what it reaches.
			name: "a claim bound to another provisioner's local PV is refused",
			pvs: []*corev1.PersistentVolume{at(tagged(pv("local-pv-1", local, claimRef("default", "data-web-0", "uid-6")),
				"local-volume-provisioner-node1-abc"), "/mnt/disks/vol1")},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-0", "local-pv-1")},
			obj:    statefulSet("web"),
			want:   fmt.Sprintf(refusal, "data-web-0", "local-pv-1", "local"),
		},
		{
			// The path alone is not enough either: local-path did not make it.
			name: "a claim bound to another provisioner's PV over a local-path-shaped directory is refused",
			pvs: []*corev1.PersistentVolume{at(tagged(pv("pvc-11", hostPath, claimRef("default", "data-web-0", "uid-11")),
				"example.com/other"), "/opt/local-path-provisioner/pvc-11_default_data-web-0")},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-0", "pvc-11")},
			obj:    statefulSet("web"),
			want:   fmt.Sprintf(refusal, "data-web-0", "pvc-11", "hostPath"),
		},
		{
			name: "a claim bound to a local-path-tagged PV over another path is refused",
			pvs: []*corev1.PersistentVolume{at(tagged(pv("pvc-7", hostPath, claimRef("default", "data-web-0", "uid-7")),
				"rancher.io/local-path"), "/")},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-0", "pvc-7")},
			obj:    statefulSet("web"),
			want:   fmt.Sprintf(refusal, "data-web-0", "pvc-7", "hostPath"),
		},
		{
			// The root holds every claim's directory.
			name: "a claim bound to a local-path-tagged PV over the whole local-path directory is refused",
			pvs: []*corev1.PersistentVolume{at(tagged(pv("pvc-8", hostPath, claimRef("default", "data-web-0", "uid-8")),
				"rancher.io/local-path"), "/opt/local-path-provisioner")},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-0", "pvc-8")},
			obj:    statefulSet("web"),
			want:   fmt.Sprintf(refusal, "data-web-0", "pvc-8", "hostPath"),
		},
		{
			name: "a claim bound to a local-path PV over another claim's directory is refused",
			pvs: []*corev1.PersistentVolume{at(tagged(pv("pvc-9", hostPath, claimRef("default", "data-web-0", "uid-9")),
				"rancher.io/local-path"), "/opt/local-path-provisioner/pvc-1_default_data-web-0")},
			claims: []*corev1.PersistentVolumeClaim{claim("data-web-0", "pvc-9")},
			obj:    statefulSet("web"),
			want:   fmt.Sprintf(refusal, "data-web-0", "pvc-9", "hostPath"),
		},
		{
			// With no reservation there is no claim the directory was made for.
			name: "a pending claim matching an unreserved local-path-tagged PV is refused",
			pvs: []*corev1.PersistentVolume{withClass(at(tagged(pv("pvc-10", hostPath, nil),
				"rancher.io/local-path"), "/opt/local-path-provisioner/pvc-10_default_data"), "local-path")},
			claims: []*corev1.PersistentVolumeClaim{pendingWithClass("data", "local-path")},
			obj:    podMounting("data"),
			want: "this server will not apply a pod spec that mounts claim \"data\": it can " +
				"bind to PersistentVolume \"pvc-10\", whose hostPath source is a path on the node, so mounting " +
				"it reaches the node's own filesystem. This is a fixed policy, not a permission that can be " +
				"granted by a flag",
		},
		{
			// The pod mounts whatever the pending claim binds, so its own class
			// decides.
			name:   "a pod mounting a pending claim whose class matches a node-path PV is refused",
			pvs:    []*corev1.PersistentVolume{withClass(pv("pv-node", hostPath, nil), "local-static")},
			claims: []*corev1.PersistentVolumeClaim{pendingWithClass("data", "local-static")},
			obj:    podMounting("data"),
			want: "this server will not apply a pod spec that mounts claim \"data\": it can " +
				"bind to PersistentVolume \"pv-node\", whose hostPath source is a path on the node, so mounting " +
				"it reaches the node's own filesystem. This is a fixed policy, not a permission that can be " +
				"granted by a flag",
		},
		{
			name:   "a pod mounting a pending claim whose class matches no node-path PV is applied",
			pvs:    []*corev1.PersistentVolume{withClass(pv("pv-node", hostPath, nil), "local-static")},
			claims: []*corev1.PersistentVolumeClaim{pendingWithClass("data", "fast")},
			obj:    podMounting("data"),
		},
		{
			// A fresh claim gets a new uid, so a reservation left by a deleted
			// claim of the same name cannot take it.
			name: "a set beside a node-path PV reserved by a deleted claim's uid is applied",
			pvs:  []*corev1.PersistentVolume{pv("pv-node", hostPath, claimRef("default", "data-web-0", "uid-old"))},
			obj:  statefulSet("web"),
		},
		{
			name:      "a failed claim list is refused, not ignored",
			pvs:       []*corev1.PersistentVolume{pv("pv-net", nfs, nil)},
			obj:       statefulSet("web"),
			wantHas:   "list PersistentVolumeClaims in namespace \"default\" to check what a claim template could bind to",
			listFails: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tc.pvs)+len(tc.claims))
			for _, pv := range tc.pvs {
				objs = append(objs, pv)
			}
			for _, c := range tc.claims {
				objs = append(objs, c)
			}
			cs := k8sfake.NewSimpleClientset(objs...)
			if tc.listFails {
				cs.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("list denied by the test credential")
				})
			}

			err := checkClaimedVolumes(context.Background(), cs, tc.obj, "default")
			switch {
			case tc.want != "":
				if err == nil || err.Error() != tc.want {
					t.Errorf("checkClaimedVolumes: %v, want exactly %q", err, tc.want)
				}
			case tc.wantHas != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantHas) {
					t.Errorf("checkClaimedVolumes: %v, want an error containing %q", err, tc.wantHas)
				}
			default:
				if err != nil {
					t.Errorf("checkClaimedVolumes refused an apply it must allow: %v", err)
				}
			}
			// The PV list already holds every volume, so reused claims must be
			// checked against it, not with one read per claim.
			for _, action := range cs.Actions() {
				if !tc.directGet && action.GetVerb() == "get" && action.GetResource().Resource == "persistentvolumes" {
					t.Errorf("a reused claim's volume was read one at a time: %v", action)
				}
			}
		})
	}
}

// A manifest nested past the search bound is refused, not left unchecked.
func TestPodSpecGuardsRefuseAManifestTooDeepToCheck(t *testing.T) {
	deep := map[string]any{"containers": []any{}, "hostPID": true}
	for range podSpecSearchDepth + 1 {
		deep = map[string]any{"next": deep}
	}
	err := checkPodTemplate(map[string]any{"apiVersion": "example.com/v1", "kind": "Deep", "spec": deep})
	if err == nil || !strings.Contains(err.Error(), "levels deep") {
		t.Errorf("a manifest too deep to check: %v, want a refusal", err)
	}
}

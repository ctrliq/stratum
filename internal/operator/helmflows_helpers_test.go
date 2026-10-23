package operator

// Helm-typed setup for the flow tests in helmflows_test.go. Those tests pin the
// operator's Helm flows and must not change when stratum moves to another Helm
// major version, so they import no helm.sh package: every Helm type lives here,
// behind plain-typed helpers, and this file is the only one a Helm migration may
// edit.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	restfake "k8s.io/client-go/rest/fake"

	"helm.sh/helm/v3/pkg/action"
	chart "helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/kube"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	helmrelease "helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

// helmRecord is one stored revision of a Helm release, reduced to what the flow
// tests assert. status uses Helm's release status strings ("deployed",
// "pending-install", "pending-upgrade", ...).
type helmRecord struct {
	revision     int
	status       string
	chartVersion string
	// description is what seed stores as Helm's description. Empty means what
	// Helm writes for the status: "Upgrade complete" for deployed and
	// superseded, which served. history leaves it empty.
	description string
}

// fakeHelmOptions selects how the fake Kubernetes client behaves.
type fakeHelmOptions struct {
	// createErr, when set, fails every Kubernetes create with this error.
	createErr error
	// renderedObject makes the client report one rendered object (a ConfigMap
	// the cluster does not have yet), so an install reaches the create of the
	// chart's own objects instead of applying an empty manifest.
	renderedObject bool
}

// fakeHelm is an in-memory Helm backend: Helm's fake Kubernetes client plus
// in-memory release storage, shared by every action configuration the
// operator builds during one test.
type fakeHelm struct {
	mem *queryFaultMemory
	cfg *action.Configuration
}

// queryFaultMemory is in-memory release storage whose failAt'th Query fails
// with err, once failAt is set. Release history and the last-revision lookup
// both go through Query.
type queryFaultMemory struct {
	*driver.Memory
	queries int
	failAt  int
	err     error
}

func (m *queryFaultMemory) Query(keyvals map[string]string) ([]*helmrelease.Release, error) {
	m.queries++
	if m.queries == m.failAt {
		return nil, m.err
	}
	return m.Memory.Query(keyvals)
}

// failQuery makes the n'th release storage query after this call fail with
// err, and only that one.
func (h *fakeHelm) failQuery(n int, err error) {
	h.mem.queries, h.mem.failAt, h.mem.err = 0, n, err
}

// useFakeHelm points newActionConfigFn at a fakeHelm for the rest of the test.
// The storage driver is bound to the namespace the operator asks for, as the
// real "secret" driver is, so a release looked up in the wrong namespace is
// not found.
func useFakeHelm(t *testing.T, opts fakeHelmOptions) *fakeHelm {
	t.Helper()
	kc := &kubefake.FailingKubeClient{
		PrintingKubeClient: kubefake.PrintingKubeClient{Out: io.Discard, LogOutput: io.Discard},
		CreateError:        opts.createErr,
	}
	if opts.renderedObject {
		kc.DummyResources = absentConfigMap()
	}
	mem := &queryFaultMemory{Memory: driver.NewMemory()}
	// Helm's own log goes to the test log, debug messages included.
	cfg := &action.Configuration{Log: t.Logf}
	cfg.Releases = storage.Init(mem)
	cfg.KubeClient = kc
	cfg.Capabilities = chartutil.DefaultCapabilities
	h := &fakeHelm{mem: mem, cfg: cfg}
	prev := newActionConfigFn
	newActionConfigFn = func(_ context.Context, _, namespace string, _ *rest.ImpersonationConfig) (*action.Configuration, error) {
		mem.SetNamespace(namespace)
		return h.cfg, nil
	}
	t.Cleanup(func() { newActionConfigFn = prev })
	return h
}

// seed stores the given revisions of release namespace/name directly in
// release storage, as if earlier operator runs had left them there.
func (h *fakeHelm) seed(t *testing.T, namespace, name string, recs ...helmRecord) {
	t.Helper()
	for _, r := range recs {
		desc := r.description
		if desc == "" {
			desc = "seeded by test"
			if r.status == "deployed" || r.status == "superseded" {
				desc = "Upgrade complete"
			}
		}
		rel := &helmrelease.Release{
			Name:      name,
			Namespace: namespace,
			Version:   r.revision,
			Info:      &helmrelease.Info{Status: helmrelease.Status(r.status), Description: desc},
			Chart: &chart.Chart{Metadata: &chart.Metadata{
				APIVersion: chart.APIVersionV2, Name: name, Version: r.chartVersion,
			}},
			Config: map[string]interface{}{},
		}
		if err := h.cfg.Releases.Create(rel); err != nil {
			t.Fatalf("seed %s/%s rev %d: %v", namespace, name, r.revision, err)
		}
	}
}

// history returns every stored revision of release namespace/name in revision
// order, or nil when release storage holds none.
func (h *fakeHelm) history(t *testing.T, namespace, name string) []helmRecord {
	t.Helper()
	h.mem.SetNamespace(namespace)
	rels, err := h.cfg.Releases.History(name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil
	}
	if err != nil {
		t.Fatalf("release history %s/%s: %v", namespace, name, err)
	}
	out := make([]helmRecord, 0, len(rels))
	for _, rel := range rels {
		rec := helmRecord{revision: rel.Version}
		if rel.Info != nil {
			rec.status = rel.Info.Status.String()
		}
		if rel.Chart != nil && rel.Chart.Metadata != nil {
			rec.chartVersion = rel.Chart.Metadata.Version
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].revision < out[j].revision })
	return out
}

// absentConfigMap is a rendered-object list holding one ConfigMap whose REST
// client answers every request with 404, so Helm's pre-install ownership check
// finds no conflicting object and the install goes on to create it.
func absentConfigMap() kube.ResourceList {
	notFound := func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)),
		}, nil
	}
	return kube.ResourceList{{
		Name:      "app-cm",
		Namespace: "kube-system",
		Mapping: &meta.RESTMapping{
			Resource:         schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
			GroupVersionKind: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			Scope:            meta.RESTScopeNamespace,
		},
		Client: &restfake.RESTClient{
			GroupVersion:         schema.GroupVersion{Version: "v1"},
			NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
			Client:               restfake.CreateHTTPClient(notFound),
		},
		Object: &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: "app-cm", Namespace: "kube-system"},
		},
	}}
}

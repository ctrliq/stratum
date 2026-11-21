package operator

import (
	"context"
	"errors"
	"io"
	"log/slog"
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

	"helm.sh/helm/v4/pkg/action"
	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/kube"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	helmreleaser "helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/release/common"
	helmrelease "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
)

type helmRecord struct {
	revision     int
	status       string
	chartVersion string
	description  string
}

type fakeHelmOptions struct {
	createErr error
	// One rendered object, so an install reaches the chart's own creates.
	renderedObject bool
}

type fakeHelm struct {
	mem *queryFaultMemory
	cfg *action.Configuration
}

// Release history and last-revision lookups both go through Query.
type queryFaultMemory struct {
	*driver.Memory
	queries int
	failAt  int
	err     error
}

func (m *queryFaultMemory) Query(keyvals map[string]string) ([]helmreleaser.Releaser, error) {
	m.queries++
	if m.queries == m.failAt {
		return nil, m.err
	}
	return m.Memory.Query(keyvals)
}

func (h *fakeHelm) failQuery(n int, err error) {
	h.mem.queries, h.mem.failAt, h.mem.err = 0, n, err
}

// The storage driver binds the requested namespace, as the real secret driver does.
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
	cfg := action.NewConfiguration(action.ConfigurationSetLogger(
		slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelDebug})))
	cfg.Releases = storage.Init(mem)
	cfg.KubeClient = kc
	cfg.Capabilities = chartcommon.DefaultCapabilities
	h := &fakeHelm{mem: mem, cfg: cfg}
	prev := newActionConfigFn
	newActionConfigFn = func(_ context.Context, _, namespace string, _ *rest.ImpersonationConfig) (*action.Configuration, error) {
		mem.SetNamespace(namespace)
		return h.cfg, nil
	}
	t.Cleanup(func() { newActionConfigFn = prev })
	return h
}

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
			Info:      &helmrelease.Info{Status: common.Status(r.status), Description: desc},
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
	for _, r := range rels {
		rel, ok := r.(*helmrelease.Release)
		if !ok {
			t.Fatalf("release history %s/%s: unexpected release type %T", namespace, name, r)
		}
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

// Answers 404 so Helm's pre-install ownership check finds no conflict and the install creates.
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

package operator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
	restfake "k8s.io/client-go/rest/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/helmpath"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	helmrelease "helm.sh/helm/v4/pkg/release/v1"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/version"
)

// --------------------------- mergedValues ---------------------------------

func TestMergedValuesPrecedence(t *testing.T) {
	spec := helmSpec{
		valuesContent: "replicas: 1\nimage:\n  repo: base\n  tag: v1\nservice:\n  port: 80\n",
		set:           map[string]string{"image.tag": "v3"},
	}
	hcc := "replicas: 2\nimage:\n  tag: v2\n"

	got, err := mergedValues(spec, hcc)
	if err != nil {
		t.Fatalf("mergedValues: %v", err)
	}

	// strvals/YAML parse ints as float64.
	if v := got["replicas"]; asInt(v) != 2 {
		t.Errorf("replicas = %v, want 2", v)
	}
	img, ok := got["image"].(map[string]interface{})
	if !ok {
		t.Fatalf("image is not a map: %#v", got["image"])
	}
	if img["tag"] != "v3" {
		t.Errorf("image.tag = %v, want v3 (set wins)", img["tag"])
	}
	if img["repo"] != "base" {
		t.Errorf("image.repo = %v, want base (nested deep-merge)", img["repo"])
	}
	svc, ok := got["service"].(map[string]interface{})
	if !ok || asInt(svc["port"]) != 80 {
		t.Errorf("service.port = %#v, want 80", got["service"])
	}
}

func TestMergedValuesNoLayers(t *testing.T) {
	got, err := mergedValues(helmSpec{}, "")
	if err != nil {
		t.Fatalf("mergedValues: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty spec should yield empty values, got %#v", got)
	}
}

func TestMergedValuesBadYAML(t *testing.T) {
	_, err := mergedValues(helmSpec{valuesContent: "replicas: : : ]["}, "")
	if err == nil {
		t.Fatal("expected error on malformed valuesContent YAML")
	}
}

func TestMergeMapsReplacesSlices(t *testing.T) {
	dst := map[string]interface{}{"args": []interface{}{"a", "b"}, "keep": "yes"}
	src := map[string]interface{}{"args": []interface{}{"c"}}
	out := mergeMaps(dst, src)
	got, _ := out["args"].([]interface{})
	if len(got) != 1 || got[0] != "c" {
		t.Errorf("args = %#v, want [c] (slice replaced wholesale)", out["args"])
	}
	if out["keep"] != "yes" {
		t.Errorf("keep = %v, want yes (untouched key preserved)", out["keep"])
	}
}

// LoadArchive strips the leading path component, so Chart.yaml sits under the chart's top dir.
func makeChartTGZ(t *testing.T, name, version string) []byte {
	t.Helper()
	files := map[string]string{
		name + "/Chart.yaml": "apiVersion: v2\nname: " + name + "\nversion: " + version + "\n",
		name + "/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n" +
			"  name: {{ .Release.Name }}-cm\ndata:\n  hello: world\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for path, body := range files {
		hd := &tar.Header{Name: path, Mode: 0644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hd); err != nil {
			t.Fatalf("tar header %s: %v", path, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar write %s: %v", path, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestLoadChartFromChartContent(t *testing.T) {
	tgz := makeChartTGZ(t, "widget", "0.4.2")
	spec := helmSpec{chartContent: base64.StdEncoding.EncodeToString(tgz)}

	chrt, err := loadChart(context.Background(), spec, "ns", "name")
	if err != nil {
		t.Fatalf("loadChart(chartContent): %v", err)
	}
	if chrt.Metadata == nil {
		t.Fatal("chart has no metadata")
	}
	if chrt.Metadata.Name != "widget" {
		t.Errorf("chart name = %q, want widget", chrt.Metadata.Name)
	}
	if chrt.Metadata.Version != "0.4.2" {
		t.Errorf("chart version = %q, want 0.4.2", chrt.Metadata.Version)
	}
}

func TestLoadChartBadBase64(t *testing.T) {
	_, err := loadChart(context.Background(), helmSpec{chartContent: "!!!not-base64!!!"}, "ns", "name")
	if err == nil {
		t.Fatal("expected error decoding invalid base64 chartContent")
	}
}

func TestLoadChartNoSource(t *testing.T) {
	_, err := loadChart(context.Background(), helmSpec{}, "ns", "name")
	if err == nil {
		t.Fatal("expected error when neither chartContent nor chart is set")
	}
}

// repo fetches run through context-bound transports; the SDK's LocateChart exposes none.

// point the cache and content cache at a temp dir, so a test writes nothing outside it.
func redirectHelmCacheDir(t *testing.T) {
	t.Helper()
	prev := version.HelmCacheDir
	version.HelmCacheDir = t.TempDir()
	t.Cleanup(func() { version.HelmCacheDir = prev })
	t.Setenv("HELM_CONTENT_CACHE", filepath.Join(version.HelmCacheDir, "content"))
	t.Setenv("HELM_REPOSITORY_CACHE", filepath.Join(version.HelmCacheDir, "cache"))
	// the migration must not read the host's real legacy cache in a test.
	legacyPrev := legacyHelmCacheDir
	legacyHelmCacheDir = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { legacyHelmCacheDir = legacyPrev })
}

// the fetch must fail inside spec.timeout, not wait for Helm's own 120s getter timeout.
func TestLoadChartRepoFetchTimesOut(t *testing.T) {
	redirectHelmCacheDir(t)
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-unblock // the repo hangs on every request, index and chart alike
	}))
	t.Cleanup(func() {
		close(unblock)
		srv.Close()
	})

	spec := helmSpec{
		chart:   "hungchart",
		repo:    srv.URL,
		version: "0.0.1",
		timeout: 250 * time.Millisecond,
	}

	type load struct {
		chrt *chart.Chart
		err  error
	}
	done := make(chan load, 1)
	start := time.Now()
	go func() {
		c, err := loadChart(context.Background(), spec, "ns", "name")
		done <- load{c, err}
	}()

	select {
	case r := <-done:
		if elapsed := time.Since(start); elapsed < spec.timeout-10*time.Millisecond {
			t.Errorf("loadChart failed after %s, before the spec.timeout window (%s) expired; the fetch must keep the whole window", elapsed, spec.timeout)
		}
		if r.err == nil {
			t.Fatal("loadChart succeeded against a repository that never answers; spec.timeout did not bound the fetch")
		}
		if !strings.Contains(strings.ToLower(r.err.Error()), "timeout") {
			t.Fatalf("loadChart error = %q, want a reason that names the timeout", r.err)
		}
	case <-time.After(spec.timeout + 3*time.Second):
		t.Fatalf("loadChart still fetching after spec.timeout (%s) expired: a hung repository fetch holds the reconcile pass until Helm's own getter timeout gives up", spec.timeout)
	}
}

func TestHelmChartRepoFetchStopsOnLeadershipCancellation(t *testing.T) {
	redirectHelmCacheDir(t)
	useFakeHelm(t, fakeHelmOptions{})
	requested := make(chan struct{}, 1)
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		select {
		case requested <- struct{}{}:
		default:
		}
		<-unblock
	}))
	t.Cleanup(srv.Close)
	defer close(unblock)
	dyn := newFlowHC(t, map[string]interface{}{
		"chart": "hungchart", "repo": srv.URL, "version": "0.0.1", "timeout": "30s",
	})
	hc := getFlowHC(t, dyn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcileHelmChart(ctx, dyn, "missing-kubeconfig", hc, "", nil, flowPolicy)
	}()
	select {
	case <-requested:
	case <-time.After(3 * time.Second):
		t.Fatal("reconciler did not start the repository request")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("leadership cancellation left reconciliation waiting on the chart's 30s fetch timeout")
	}
	// join reconciliation before the test restores its shared Helm state.
	t.Cleanup(func() { <-done })
}

// the happy path keeps the timeout enforcement honest: an answering repo must fetch.
func TestLoadChartFromRepoFetch(t *testing.T) {
	redirectHelmCacheDir(t)
	tgz := makeChartTGZ(t, "widget", "1.0.0")
	var repo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.yaml":
			fmt.Fprintf(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    digest: sha256:%x\n    urls:\n    - %s/charts/widget-1.0.0.tgz\n", sha256.Sum256(tgz), repo)
		case "/charts/widget-1.0.0.tgz":
			if _, err := w.Write(tgz); err != nil {
				t.Errorf("serve chart tgz: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	repo = srv.URL

	spec := helmSpec{chart: "widget", repo: srv.URL, version: "1.0.0", timeout: 30 * time.Second}

	chrt, err := loadChart(context.Background(), spec, "ns", "name")
	if err != nil {
		t.Fatalf("loadChart from repo %s: %v", srv.URL, err)
	}
	if chrt.Metadata == nil || chrt.Metadata.Name != "widget" || chrt.Metadata.Version != "1.0.0" {
		t.Errorf("loaded chart = %+v, want widget 1.0.0 fetched from the repo", chrt.Metadata)
	}
}

// helm v4 discards slog output when no logger is set.
func TestNewActionConfigLogsToOperatorLog(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	cfg, err := newActionConfig(context.Background(), filepath.Join(t.TempDir(), "missing-kubeconfig"), "team-a", nil)
	if err != nil {
		t.Fatalf("newActionConfig: %v", err)
	}
	cfg.Logger().Debug("helm debug probe")

	if got := buf.String(); !strings.Contains(got, "level=DEBUG") || !strings.Contains(got, "helm debug probe") {
		t.Fatalf("operator log = %q, want Helm's debug message", got)
	}
}

// client-side apply needs no patch rights under requester impersonation.
func TestHelmInstallUsesClientSideApply(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

	reconcileFlow(t, dyn)

	wantDeployed(t, dyn, 1, "1.0.0")
	h.mem.SetNamespace(flowNS)
	last, err := h.cfg.Releases.Last(flowRelease)
	if err != nil {
		t.Fatalf("stored release %s/%s: %v", flowNS, flowRelease, err)
	}
	rel, ok := last.(*helmrelease.Release)
	if !ok {
		t.Fatalf("stored release %s/%s: unexpected release type %T", flowNS, flowRelease, last)
	}
	if want := string(helmrelease.ApplyMethodClientSideApply); rel.ApplyMethod != want {
		t.Fatalf("install apply method = %q, want %q (client-side)", rel.ApplyMethod, want)
	}
}

func TestNewActionConfigBindsRequestedNamespace(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: https://127.0.0.1:6443\ncontexts:\n- name: test\n  context:\n    cluster: test\n    namespace: elsewhere\ncurrent-context: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := newActionConfig(context.Background(), kubeconfig, "team-a", nil)
	if err != nil {
		t.Fatalf("newActionConfig: %v", err)
	}
	getter, ok := cfg.RESTClientGetter.(genericclioptions.RESTClientGetter)
	if !ok {
		t.Fatalf("Helm's REST client getter is %T, want a genericclioptions.RESTClientGetter", cfg.RESTClientGetter)
	}
	got, _, err := getter.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		t.Fatalf("getter namespace: %v", err)
	}
	if got != "team-a" {
		t.Fatalf("Helm client namespace = %q, want the requested team-a, not the kubeconfig context's", got)
	}
}

// app-cm's ownership read answers 403, so helm's uninstall cannot verify it.
func forbidOwnershipCheck(t *testing.T, h *fakeHelm) *kubefake.FailingKubeClient {
	t.Helper()
	kc, ok := h.cfg.KubeClient.(*kubefake.FailingKubeClient)
	if !ok {
		t.Fatalf("fake Helm kube client is %T, want *kubefake.FailingKubeClient", h.cfg.KubeClient)
	}
	objs := absentConfigMap()
	objs[0].Client = &restfake.RESTClient{
		GroupVersion:         schema.GroupVersion{Version: "v1"},
		NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		Client: restfake.CreateHTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","message":"configmaps \"app-cm\" is forbidden","code":403}`)),
			}, nil
		}),
	}
	kc.DummyResources = objs
	return kc
}

func wantUninstallIncomplete(t *testing.T, dyn dynamic.Interface, when string) {
	t.Helper()
	st := readFlowStatus(t, dyn)
	if st.phase != hcPhaseFailed ||
		!strings.Contains(st.message, "were not deleted because their ownership could not be verified") ||
		!strings.Contains(st.message, "app-cm") {
		t.Fatalf("%s: helmchart status = %+v, want Failed with Helm's not-deleted list naming app-cm", when, st)
	}
}

// helm purges the release anyway, so the finalizer must hold on the status alone.
func TestDeletionKeepsFinalizerWhenHelmCannotVerifyOwnership(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
	forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	markFlowHCDeleted(t, dyn)

	reconcileFlow(t, dyn)

	if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Fatal("finalizer removed although Helm left app-cm in place")
	}
	wantUninstallIncomplete(t, dyn, "after the uninstall")
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release %s/%s still stored, want it purged as Helm v4 does: %+v", flowNS, flowRelease, hist)
	}

	reconcileFlow(t, dyn)

	if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Fatal("next reconcile removed the finalizer once the purged release was not found")
	}
	wantUninstallIncomplete(t, dyn, "after the next reconcile")
}

// an uninstall error must not hide the unverified list from the hold.
func TestRecoverUninstallErrorKeepsTheUnverifiedList(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	kc.WaitForDeleteError = errors.New("wait for delete refused")
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

	reconcileFlow(t, dyn)

	wantUninstallIncomplete(t, dyn, "after a recovery uninstall that also returned an error")
}

func TestRecoverPendingInstallStopsWhenHelmCannotVerifyOwnership(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

	reconcileFlow(t, dyn)

	wantUninstallIncomplete(t, dyn, "after the recovery uninstall")
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release %s/%s still stored, want it purged as Helm v4 does: %+v", flowNS, flowRelease, hist)
	}

	kc.DummyResources = nil
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		if err := unstructured.SetNestedField(hc.Object, past, "status", "lastAttempt"); err != nil {
			t.Fatal(err)
		}
	})
	reconcileFlow(t, dyn)

	wantUninstallIncomplete(t, dyn, "after the next reconcile")
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("reinstalled over the objects Helm could not verify: %+v", hist)
	}

	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(hc.Object, flowChart(t, "1.1.0"), "spec", "chartContent"); err != nil {
			t.Fatal(err)
		}
	})
	reconcileFlow(t, dyn)

	wantDeployed(t, dyn, 1, "1.1.0")
}

// the spec change the recovery message asks for does nothing once deletion began.
func TestDeletionAfterIncompleteRecoveryAsksToRemoveFinalizer(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	reconcileFlow(t, dyn)
	wantUninstallIncomplete(t, dyn, "after the recovery uninstall")

	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		deleted := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		hc.SetDeletionTimestamp(&deleted)
	})
	for _, when := range []string{"after the delete", "after the next reconcile"} {
		reconcileFlow(t, dyn)

		if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
			t.Fatalf("%s: finalizer removed although Helm left app-cm in place", when)
		}
		wantUninstallIncomplete(t, dyn, when)
		if msg := readFlowStatus(t, dyn).message; !strings.Contains(msg, "then the "+helmFinalizer+" finalizer") || strings.Contains(msg, "change the spec") {
			t.Fatalf("%s: status message = %q, want the finalizer instruction instead of a spec change", when, msg)
		}
	}
}

func updateFlowHC(t *testing.T, dyn dynamic.Interface, mutate func(*unstructured.Unstructured)) {
	t.Helper()
	hc := getFlowHC(t, dyn)
	mutate(hc)
	if _, err := dyn.Resource(helmChartGVR).Namespace(flowNS).Update(context.Background(), hc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update helmchart: %v", err)
	}
}

// the status message must not list objects helm keeps on purpose; a person would delete them.
func TestUninstallIncompleteListsOnlyUnverifiableObjects(t *testing.T) {
	t.Run("deletion", func(t *testing.T) {
		h := useFakeHelm(t, fakeHelmOptions{})
		logs := captureOperatorLog(t)
		seedKeptAndUnverifiable(t, h, "deployed")
		dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
		markFlowHCDeleted(t, dyn)

		reconcileFlow(t, dyn)

		wantOnlyUnverifiableListed(t, dyn, uninstallIncompleteDelete, "after the uninstall")
		wantFullNoteLogged(t, logs, "after the uninstall")
	})

	t.Run("pending install recovery", func(t *testing.T) {
		h := useFakeHelm(t, fakeHelmOptions{})
		logs := captureOperatorLog(t)
		seedKeptAndUnverifiable(t, h, "pending-install")
		dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})

		reconcileFlow(t, dyn)

		wantOnlyUnverifiableListed(t, dyn, uninstallIncompleteRecover, "after the recovery uninstall")
		wantFullNoteLogged(t, logs, "after the recovery uninstall")

		updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
			deleted := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			hc.SetDeletionTimestamp(&deleted)
		})
		reconcileFlow(t, dyn)

		wantOnlyUnverifiableListed(t, dyn, uninstallIncompleteDelete, "after the delete")
	})
}

const keptAndOtherReleaseManifest = `---
# Source: app/templates/pvc.yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: app-data
  annotations:
    helm.sh/resource-policy: keep
---
# Source: app/templates/cm.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-cm
---
# Source: app/templates/other-cm.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: other-cm
`

// helm sorts the three objects into the three sections of its uninstall note.
func seedKeptAndUnverifiable(t *testing.T, h *fakeHelm, status string) {
	t.Helper()
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: status, chartVersion: "1.0.0"})
	h.mem.SetNamespace(flowNS)
	last, err := h.cfg.Releases.Last(flowRelease)
	if err != nil {
		t.Fatalf("stored release %s/%s: %v", flowNS, flowRelease, err)
	}
	rel, ok := last.(*helmrelease.Release)
	if !ok {
		t.Fatalf("stored release %s/%s: unexpected release type %T", flowNS, flowRelease, last)
	}
	rel.Manifest = keptAndOtherReleaseManifest
	if err := h.cfg.Releases.Update(rel); err != nil {
		t.Fatalf("store manifest of %s/%s: %v", flowNS, flowRelease, err)
	}

	kc := forbidOwnershipCheck(t, h)
	other := *absentConfigMap()[0]
	other.Name = "other-cm"
	other.Object = &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "other-cm", Namespace: flowNS},
	}
	other.Client = &restfake.RESTClient{
		GroupVersion:         schema.GroupVersion{Version: "v1"},
		NegotiatedSerializer: scheme.Codecs.WithoutConversion(),
		Client: restfake.CreateHTTPClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"other-cm","namespace":"` + flowNS + `",` +
						`"labels":{"app.kubernetes.io/managed-by":"Helm"},` +
						`"annotations":{"meta.helm.sh/release-name":"other","meta.helm.sh/release-namespace":"` + flowNS + `"}}}`)),
			}, nil
		}),
	}
	kc.DummyResources = append(kc.DummyResources, &other)
}

func captureOperatorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

func wantOnlyUnverifiableListed(t *testing.T, dyn dynamic.Interface, prefix, when string) {
	t.Helper()
	wantUninstallIncomplete(t, dyn, when)
	msg := readFlowStatus(t, dyn).message
	if !strings.HasPrefix(msg, prefix) {
		t.Fatalf("%s: status message = %q, want it to start with %q", when, msg, prefix)
	}
	for _, obj := range []string{"app-data", "other-cm"} {
		if strings.Contains(msg, obj) {
			t.Errorf("%s: status message = %q names %s, which a person following it would delete by hand; want only the objects Helm could not verify", when, msg, obj)
		}
	}
}

func wantFullNoteLogged(t *testing.T, logs *bytes.Buffer, when string) {
	t.Helper()
	for _, obj := range []string{"app-data", "app-cm", "other-cm"} {
		if !strings.Contains(logs.String(), obj) {
			t.Errorf("%s: operator log = %q, want Helm's whole uninstall note, naming %s", when, logs.String(), obj)
		}
	}
}

// an uninstall-incomplete message here would hold the finalizer and the kept volume's data.
func TestDeletionRemovesFinalizerWhenNoteListsOnlyKeptObjects(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	logs := captureOperatorLog(t)
	seedKeptAndOtherRelease(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	markFlowHCDeleted(t, dyn)
	writes := recordStatusWrites(t, dyn, 0)

	reconcileFlow(t, dyn)

	if hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
		t.Errorf("finalizer kept although Helm left only kept and other-release objects; status = %+v", readFlowStatus(t, dyn))
	}
	for _, msg := range writes.messages {
		if strings.HasPrefix(msg, uninstallIncompletePrefix) {
			t.Errorf("status write %q asks a person to remove objects Helm left on purpose", msg)
		}
	}
	for _, obj := range []string{"app-data", "other-cm"} {
		if !strings.Contains(logs.String(), obj) {
			t.Errorf("operator log = %q, want Helm's uninstall note, naming %s", logs.String(), obj)
		}
	}
}

// the fake kube client builds deletes from DummyResources, so dropping app-cm there is enough.
func seedKeptAndOtherRelease(t *testing.T, h *fakeHelm) {
	t.Helper()
	seedKeptAndUnverifiable(t, h, "deployed")
	kc, ok := h.cfg.KubeClient.(*kubefake.FailingKubeClient)
	if !ok {
		t.Fatalf("fake Helm kube client is %T, want *kubefake.FailingKubeClient", h.cfg.KubeClient)
	}
	reported := kc.DummyResources[:0]
	for _, info := range kc.DummyResources {
		if info.Name != "app-cm" {
			reported = append(reported, info)
		}
	}
	kc.DummyResources = reported
}

// helm has purged the release by then, so a lost hold write would remove the finalizer.
func TestUninstallIncompleteStatusSurvivesTransientWriteFailure(t *testing.T) {
	t.Run("deletion", func(t *testing.T) {
		h := useFakeHelm(t, fakeHelmOptions{})
		h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "deployed", chartVersion: "1.0.0"})
		forbidOwnershipCheck(t, h)
		dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
		markFlowHCDeleted(t, dyn)
		writes := recordStatusWrites(t, dyn, 1)

		reconcileFlow(t, dyn)

		wantIncompleteWriteRejectedOnce(t, writes, "after the uninstall")
		if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
			t.Fatal("finalizer removed although Helm left app-cm in place")
		}

		reconcileFlow(t, dyn)

		if !hasFinalizer(getFlowHC(t, dyn), helmFinalizer) {
			t.Fatalf("next reconcile removed the finalizer once the purged release was not found; status = %+v", readFlowStatus(t, dyn))
		}
		wantOnlyUnverifiableListed(t, dyn, uninstallIncompleteDelete, "after the next reconcile")
	})

	t.Run("pending install recovery", func(t *testing.T) {
		h := useFakeHelm(t, fakeHelmOptions{})
		h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
		kc := forbidOwnershipCheck(t, h)
		dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
		writes := recordStatusWrites(t, dyn, 1)

		reconcileFlow(t, dyn)

		wantIncompleteWriteRejectedOnce(t, writes, "after the recovery uninstall")

		// recovered apiserver, failed backoff passed: only the recorded hold stops a reinstall.
		kc.DummyResources = nil
		updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
			past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
			if err := unstructured.SetNestedField(hc.Object, past, "status", "lastAttempt"); err != nil {
				t.Fatal(err)
			}
		})
		reconcileFlow(t, dyn)

		if hist := h.history(t, flowNS, flowRelease); hist != nil {
			t.Fatalf("reinstalled over the objects Helm could not verify: %+v; status = %+v", hist, readFlowStatus(t, dyn))
		}
		wantOnlyUnverifiableListed(t, dyn, uninstallIncompleteRecover, "after the next reconcile")
	})
}

type statusWrites struct {
	messages []string
	rejected int // writes failed with "etcdserver: leader changed"
}

// fails the first failIncomplete incomplete-prefix writes with a transient etcd error.
func recordStatusWrites(t *testing.T, dyn dynamic.Interface, failIncomplete int) *statusWrites {
	t.Helper()
	fake, ok := dyn.(*dynfake.FakeDynamicClient)
	if !ok {
		t.Fatalf("dynamic client is %T, want *dynfake.FakeDynamicClient", dyn)
	}
	w := &statusWrites{}
	fake.PrependReactor("update", helmChartGVR.Resource, func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" {
			return false, nil, nil
		}
		update, ok := action.(ktesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		obj, ok := update.GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		msg, _, _ := unstructured.NestedString(obj.Object, "status", "message")
		w.messages = append(w.messages, msg)
		if strings.HasPrefix(msg, uninstallIncompletePrefix) && w.rejected < failIncomplete {
			w.rejected++
			return true, nil, apierrors.NewInternalError(errors.New("etcdserver: leader changed"))
		}
		return false, nil, nil
	})
	return w
}

func wantIncompleteWriteRejectedOnce(t *testing.T, w *statusWrites, when string) {
	t.Helper()
	if w.rejected != 1 {
		t.Fatalf("%s: rejected %d %q status writes, want 1; status writes: %q", when, w.rejected, uninstallIncompletePrefix, w.messages)
	}
}

// --------------------------- specHash -------------------------------------

func TestSpecHashStableAndSensitive(t *testing.T) {
	base := helmSpec{repo: "https://r", chart: "c", version: "1.0.0", chartContent: "AAAA"}
	vals := map[string]interface{}{"a": 1, "b": map[string]interface{}{"c": 2}}

	h1 := specHash(base, vals)
	h2 := specHash(base, vals)
	if h1 != h2 {
		t.Errorf("specHash not stable: %s != %s", h1, h2)
	}
	// map order must not matter (canonical JSON).
	valsReordered := map[string]interface{}{"b": map[string]interface{}{"c": 2}, "a": 1}
	if specHash(base, valsReordered) != h1 {
		t.Error("specHash changed under value-map reordering (not canonical)")
	}

	verBumped := base
	verBumped.version = "1.0.1"
	if specHash(verBumped, vals) == h1 {
		t.Error("specHash unchanged after version bump")
	}
	ccChanged := base
	ccChanged.chartContent = "BBBB"
	if specHash(ccChanged, vals) == h1 {
		t.Error("specHash unchanged after chartContent change")
	}
	if specHash(base, map[string]interface{}{"a": 99}) == h1 {
		t.Error("specHash unchanged after values change")
	}
}

func TestFinalizerListHelpers(t *testing.T) {
	got := addFinalizerList([]string{"other"}, helmFinalizer)
	if len(got) != 2 || got[1] != helmFinalizer {
		t.Errorf("addFinalizerList = %#v, want [other %s]", got, helmFinalizer)
	}
	if again := addFinalizerList(got, helmFinalizer); len(again) != 2 {
		t.Errorf("addFinalizerList not idempotent: %#v", again)
	}
	rem := removeFinalizerList(got, helmFinalizer)
	if len(rem) != 1 || rem[0] != "other" {
		t.Errorf("removeFinalizerList = %#v, want [other]", rem)
	}
	if noop := removeFinalizerList([]string{"other"}, helmFinalizer); len(noop) != 1 {
		t.Errorf("removeFinalizerList absent = %#v, want [other]", noop)
	}
}

func TestHasFinalizer(t *testing.T) {
	o := &unstructured.Unstructured{Object: map[string]interface{}{}}
	o.SetFinalizers([]string{"a", helmFinalizer, "b"})
	if !hasFinalizer(o, helmFinalizer) {
		t.Error("hasFinalizer should find present finalizer")
	}
	if hasFinalizer(o, "missing") {
		t.Error("hasFinalizer should not find absent finalizer")
	}
}

// --------------------------- spec parsing / defaults ----------------------

func newHC(ns, name string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": k8sutil.HelmChartGroup + "/" + k8sutil.HelmChartVersion,
		"kind":       "HelmChart",
		"metadata":   map[string]interface{}{"namespace": ns, "name": name},
		"spec":       spec,
	}}
}

func TestParseHelmSpecDefaults(t *testing.T) {
	hc := newHC("kube-system", "traefik", map[string]interface{}{
		"chart": "traefik",
		"repo":  "https://helm.traefik.io/traefik",
	})
	s := parseHelmSpec(hc)
	if s.releaseName != "traefik" {
		t.Errorf("releaseName default = %q, want traefik (CR name)", s.releaseName)
	}
	if s.targetNamespace != "kube-system" {
		t.Errorf("targetNamespace default = %q, want kube-system (CR ns)", s.targetNamespace)
	}
	if s.timeout != 5*time.Minute {
		t.Errorf("timeout default = %v, want 5m", s.timeout)
	}
}

func TestParseHelmSpecOverrides(t *testing.T) {
	hc := newHC("default", "cr", map[string]interface{}{
		"chart":           "c",
		"releaseName":     "myrel",
		"targetNamespace": "prod",
		"createNamespace": true,
		"timeout":         "90s",
		"set":             map[string]interface{}{"a.b": "c", "num": "5"},
	})
	s := parseHelmSpec(hc)
	if s.releaseName != "myrel" {
		t.Errorf("releaseName = %q, want myrel", s.releaseName)
	}
	if s.targetNamespace != "prod" {
		t.Errorf("targetNamespace = %q, want prod", s.targetNamespace)
	}
	if !s.createNamespace {
		t.Error("createNamespace = false, want true")
	}
	if s.timeout != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", s.timeout)
	}
	if s.set["a.b"] != "c" || s.set["num"] != "5" {
		t.Errorf("set = %#v, want {a.b:c, num:5}", s.set)
	}
}

func TestParseHelmSpecBadTimeoutFallsBack(t *testing.T) {
	hc := newHC("default", "cr", map[string]interface{}{"chart": "c", "timeout": "not-a-duration"})
	s := parseHelmSpec(hc)
	if s.timeout != 5*time.Minute {
		t.Errorf("bad timeout should fall back to 5m, got %v", s.timeout)
	}
}

// pins the vendored helm's strvals inference for set and setString in one table.
var setStringCases = []struct {
	name     string
	val      string
	inferred interface{} // what spec.set yields
}{
	{"decimal", "1.0", "1.0"},
	{"leadingZero", "01", "01"},
	{"exponent", "1e5", "1e5"},
	{"underscore", "1_0", "1_0"},
	{"boolTrue", "true", true},
	{"boolFalse", "false", false},
	{"null", "null", nil},
	{"zero", "0", int64(0)},
	{"integer", "42", int64(42)},
	{"negative", "-3", int64(-3)},
}

func TestMergedValuesSetStringPreservesStrings(t *testing.T) {
	for _, tc := range setStringCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergedValues(helmSpec{setString: map[string]string{"k": tc.val}}, "")
			if err != nil {
				t.Fatalf("mergedValues: %v", err)
			}
			v, ok := got["k"]
			if !ok {
				t.Fatalf("setString %q produced no key at all: %#v", tc.val, got)
			}
			if v != tc.val {
				t.Errorf("setString %q = %#v (%T), want %q (string)", tc.val, v, v, tc.val)
			}
		})
	}
}

func TestMergedValuesSetStillInfers(t *testing.T) {
	// if this regresses, setString has been wired into the wrong loop.
	for _, tc := range setStringCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergedValues(helmSpec{set: map[string]string{"k": tc.val}}, "")
			if err != nil {
				t.Fatalf("mergedValues: %v", err)
			}
			v, ok := got["k"]
			if !ok {
				t.Fatalf("set %q produced no key at all: %#v", tc.val, got)
			}
			if v != tc.inferred {
				t.Errorf("set %q = %#v (%T), want %#v (%T)", tc.val, v, v, tc.inferred, tc.inferred)
			}
		})
	}
}

func TestMergedValuesSetStringBeatsSet(t *testing.T) {
	// helm applies --set-string after --set, so the string form wins a collision.
	spec := helmSpec{
		set:       map[string]string{"image.tag": "42", "replicas": "3"},
		setString: map[string]string{"image.tag": "42"},
	}
	got, err := mergedValues(spec, "")
	if err != nil {
		t.Fatalf("mergedValues: %v", err)
	}
	img, ok := got["image"].(map[string]interface{})
	if !ok {
		t.Fatalf("image is not a map: %#v", got["image"])
	}
	if img["tag"] != "42" {
		t.Errorf("image.tag = %#v (%T), want %q (string) — setString must win the collision",
			img["tag"], img["tag"], "42")
	}
	if _, isInt := got["replicas"].(int64); !isInt {
		t.Errorf("replicas = %#v (%T), want int64 — set-only keys must keep inferring",
			got["replicas"], got["replicas"])
	}
}

func TestMergedValuesSetStringBeatsValuesContent(t *testing.T) {
	spec := helmSpec{
		valuesContent: "image:\n  tag: fromvalues\n",
		setString:     map[string]string{"image.tag": "01"},
	}
	got, err := mergedValues(spec, "image:\n  tag: fromhcc\n")
	if err != nil {
		t.Fatalf("mergedValues: %v", err)
	}
	img, _ := got["image"].(map[string]interface{})
	if img["tag"] != "01" {
		t.Errorf("image.tag = %#v, want \"01\" (setString beats valuesContent and HCC)", img["tag"])
	}
}

func TestMergedValuesPrefixOverlapIsStable(t *testing.T) {
	// 200 rounds beats the odds of a random map order succeeding once in eight.
	cases := []struct {
		name   string
		field  func(*helmSpec, map[string]string)
		prefix string
	}{
		{"set", func(s *helmSpec, m map[string]string) { s.set = m }, `set "logging.level.http": `},
		{"setString", func(s *helmSpec, m map[string]string) { s.setString = m }, `setString "logging.level.http": `},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 200; i++ {
				spec := helmSpec{}
				tc.field(&spec, map[string]string{"logging.level": "info", "logging.level.http": "debug"})
				vals, err := mergedValues(spec, "")
				if err == nil {
					t.Fatalf("round %d: mergedValues = %#v, want a stable parse error", i, vals)
				}
				if vals != nil {
					t.Errorf("round %d: vals = %#v, want nil alongside the error", i, vals)
				}
				// helm bumps may reword the panic text tail, so only its shape is asserted.
				if !strings.HasPrefix(err.Error(), tc.prefix) || !strings.Contains(err.Error(), "interface conversion") {
					t.Fatalf("round %d: error = %q, want %q... plus interface-conversion detail", i, err, tc.prefix)
				}
			}
		})
	}
}

func TestParseHelmSpecSetString(t *testing.T) {
	hc := newHC("default", "cr", map[string]interface{}{
		"chart":     "c",
		"set":       map[string]interface{}{"a": "1"},
		"setString": map[string]interface{}{"b": "2", "ver": "1.0"},
	})
	s := parseHelmSpec(hc)
	if s.setString["b"] != "2" || s.setString["ver"] != "1.0" {
		t.Errorf("setString = %#v, want {b:2, ver:1.0}", s.setString)
	}
	if s.set["a"] != "1" {
		t.Errorf("set = %#v, want {a:1}", s.set)
	}
}

func TestSpecHashCoversSetString(t *testing.T) {
	hash := func(spec helmSpec) string {
		t.Helper()
		vals, err := mergedValues(spec, "")
		if err != nil {
			t.Fatalf("mergedValues: %v", err)
		}
		return specHash(spec, vals)
	}
	base := helmSpec{chart: "c", repo: "r"}
	with := base
	with.setString = map[string]string{"image.tag": "1.0"}
	if hash(base) == hash(with) {
		t.Error("specHash unchanged by setString; a setString-only edit would not reconcile")
	}
}

// a closed CRD schema prunes undeclared spec fields silently at admission.
func TestHelmChartCRDDeclaresSetString(t *testing.T) {
	props, ok := helmChartCRDSpecSchema(t)["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("HelmChart CRD .spec has no properties block")
	}
	ss, ok := props["setString"].(map[string]interface{})
	if !ok {
		t.Fatalf("HelmChart CRD does not declare spec.setString, so the API server "+
			"will prune it at admission. Declared spec properties: %v", sortedKeys(props))
	}
	if ss["type"] != "object" {
		t.Errorf("spec.setString type = %#v, want \"object\"", ss["type"])
	}
	ap, ok := ss["additionalProperties"].(map[string]interface{})
	if !ok || ap["type"] != "string" {
		t.Errorf("spec.setString additionalProperties = %#v, want {type: string} "+
			"(mirroring spec.set)", ss["additionalProperties"])
	}
	if _, ok := props["set"].(map[string]interface{}); !ok {
		t.Error("spec.set vanished from the CRD; setString is an addition, not a replacement")
	}
}

// the CRD description is the contract `kubectl explain` prints.
func TestHelmChartCRDDescribesSetStringPrecedence(t *testing.T) {
	// the precedence chain lives on the CRD's root description, not .spec's.
	if root := string(addons.RenderHelmCRDs()); strings.Contains(root, "< .spec.set.") {
		t.Error("the HelmChart CRD description states a values-precedence chain ending " +
			"at `< .spec.set.`, so `kubectl explain helmcharts` contradicts mergedValues, " +
			"which applies .spec.setString after it")
	}
	props, _ := helmChartCRDSpecSchema(t)["properties"].(map[string]interface{})
	set, _ := props["set"].(map[string]interface{})
	setDesc, _ := set["description"].(string)
	if strings.Contains(setDesc, "highest") && !strings.Contains(setDesc, "setString") {
		t.Errorf("spec.set still describes itself as the highest precedence without "+
			"naming setString, which now overrides it. Got:\n%s", setDesc)
	}
}

// the closed schema prunes what it does not declare; a declared field nothing reads is dead.
func TestHelmChartCRDDeclaresExactlyTheSpecTheControllerReads(t *testing.T) {
	specFields := make([]string, 0, len(helmChartSpecTable))
	for _, row := range helmChartSpecTable {
		specFields = append(specFields, row.name)
	}
	if got, want := len(specFields), reflect.TypeOf(helmSpec{}).NumField(); got != want {
		t.Errorf("helmChartSpecTable holds %d rows, but helmSpec has %d fields — a helmSpec field is missing from the table, or the table is stale", got, want)
	}

	props, ok := helmChartCRDSpecSchema(t)["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("HelmChart CRD .spec has no properties block")
	}

	// controller to CRD: an undeclared field is pruned at admission.
	for _, f := range specFields {
		if _, ok := props[f]; !ok {
			t.Errorf("parseHelmSpec reads spec.%s, but the CRD does not declare it — the apiserver prunes it at admission", f)
		}
	}
	// CRD to controller: a declared field nothing reads is dead contract.
	for _, f := range sortedKeys(props) {
		if !slices.Contains(specFields, f) {
			t.Errorf("the CRD declares spec.%s, but parseHelmSpec never reads it", f)
		}
	}
	s := parseHelmSpec(newHC("kube-system", "cr", map[string]interface{}{}))
	d, declared := crdDefault(props, "createNamespace")
	if !declared || d != s.createNamespace {
		t.Errorf("spec.createNamespace: the CRD declares default %v (declared=%t), the controller defaults to %v", d, declared, s.createNamespace)
	}
	d, declared = crdDefault(props, "insecureSkipTLSVerify")
	if !declared || d != s.insecureSkipTLSVerify {
		t.Errorf("spec.insecureSkipTLSVerify: the CRD declares default %v (declared=%t), the controller defaults to %v", d, declared, s.insecureSkipTLSVerify)
	}
	d, declared = crdDefault(props, "timeout")
	dur, err := time.ParseDuration(str(d, ""))
	if !declared || err != nil {
		t.Fatalf("spec.timeout: the CRD must declare a Go duration string as its default, got %v (declared=%t): %v", d, declared, err)
	}
	if dur != s.timeout {
		t.Errorf("spec.timeout: the CRD declares default %s, the controller defaults to %s", dur, s.timeout)
	}
}

// the table, not a second copy, is the contract the CRD must match.
func TestHelmChartCRDMatchesTheSpecTable(t *testing.T) {
	props, ok := helmChartCRDSpecSchema(t)["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("HelmChart CRD .spec has no properties block")
	}
	if got, want := len(helmChartSpecTable), reflect.TypeOf(helmSpec{}).NumField(); got != want {
		t.Errorf("helmChartSpecTable has %d rows, but helmSpec has %d fields — the spec contract is declared twice, or a field is missing from the table", got, want)
	}
	rows := map[string]bool{}
	for _, row := range helmChartSpecTable {
		if rows[row.name] {
			t.Errorf("helmChartSpecTable declares spec.%s twice", row.name)
			continue
		}
		rows[row.name] = true
		if _, ok := reflect.TypeOf(helmSpec{}).FieldByName(row.name); !ok {
			t.Errorf("helmChartSpecTable row %q names no helmSpec field — parseHelmSpec reads by these rows, so the parsed value has nowhere to go", row.name)
		}
	}

	// table to CRD: same type and default, or kubectl explain lies.
	for _, row := range helmChartSpecTable {
		prop, ok := props[row.name].(map[string]interface{})
		if !ok {
			t.Errorf("the CRD does not declare spec.%s — the apiserver prunes it at admission, so parseHelmSpec never sees it", row.name)
			continue
		}
		if prop["type"] != row.kind {
			t.Errorf("spec.%s: the CRD declares type %v, the table declares kind %q", row.name, prop["type"], row.kind)
		}
		d, declared := prop["default"]
		switch {
		case declared && row.def == nil:
			t.Errorf("spec.%s: the CRD declares default %v, the table declares none", row.name, d)
		case !declared && row.def != nil:
			t.Errorf("spec.%s: the table declares default %v, the CRD declares none", row.name, row.def)
		case declared && d != row.def:
			t.Errorf("spec.%s: the CRD declares default %v, the table declares %v", row.name, d, row.def)
		}
	}
	// CRD to table: a property no row declares is contract the controller never honours.
	for _, name := range sortedKeys(props) {
		if !rows[name] {
			t.Errorf("the CRD declares spec.%s, but helmChartSpecTable does not — the controller never reads it", name)
		}
	}

	// the timeout bounds the Helm operation; the description must not promise --wait.
	timeout, ok := props["timeout"].(map[string]interface{})
	if !ok {
		t.Fatal("the CRD does not declare spec.timeout")
	}
	desc, _ := timeout["description"].(string)
	if desc == "" {
		t.Error("spec.timeout: the CRD no longer describes the timeout at all")
	}
	if strings.Contains(desc, "--wait") {
		t.Errorf("spec.timeout's description mentions --wait, but the controller never waits (Wait=false on install and upgrade): %q", desc)
	}

	// change a row's default and the parsed empty spec must follow.
	for i := range helmChartSpecTable {
		if helmChartSpecTable[i].name != "createNamespace" {
			continue
		}
		saved := helmChartSpecTable[i].def
		helmChartSpecTable[i].def = true
		defer func() { helmChartSpecTable[i].def = saved }()
		if s := parseHelmSpec(newHC("kube-system", "cr", map[string]interface{}{})); !s.createNamespace {
			t.Error("parseHelmSpec does not read helmChartSpecTable's createNamespace default: with the table default set to true, an empty spec still parses as false")
		}
		break
	}
	for i := range helmChartSpecTable {
		if helmChartSpecTable[i].name != "timeout" {
			continue
		}
		saved := helmChartSpecTable[i].def
		helmChartSpecTable[i].def = "2m"
		defer func() { helmChartSpecTable[i].def = saved }()
		if s := parseHelmSpec(newHC("kube-system", "cr", map[string]interface{}{})); s.timeout != 2*time.Minute {
			t.Errorf("parseHelmSpec does not read helmChartSpecTable's timeout default: with the table default set to 2m, an empty spec still parses as %s", s.timeout)
		}
		break
	}
}

// a misspelled name literal in parseHelmSpec reads nothing while all structures match.
func TestParseHelmSpecReadsEachRowByName(t *testing.T) {
	for _, row := range helmChartSpecTable {
		sentinel := "probe-" + row.name
		var set interface{}
		switch row.kind {
		case "boolean":
			set = true
		case "object":
			set = map[string]interface{}{"probe": sentinel}
		case "array": // valuesSecrets, the only array row: one entry named by the sentinel
			set = []interface{}{map[string]interface{}{"name": sentinel, "keys": []interface{}{"k"}}}
		default: // timeout gets a parseable duration or it reads like an unread field
			if row.name == "timeout" {
				set = "3m"
			} else {
				set = sentinel
			}
		}
		s := parseHelmSpec(newHC("kube-system", "cr", map[string]interface{}{row.name: set}))
		fv := reflect.ValueOf(s).FieldByName(row.name)
		if !fv.IsValid() {
			t.Errorf("helmChartSpecTable row %q names no helmSpec field to read into", row.name)
			continue
		}
		// Interface() panics on unexported fields; the kind accessors read fine.
		switch {
		case row.name == "timeout":
			if fv.Kind() != reflect.Int64 || fv.Int() != int64(3*time.Minute) {
				t.Errorf("spec.timeout parsed as %s, want 3m: parseHelmSpec does not read this name out of the spec map — it either misspells it or stopped parsing durations", describe(fv))
			}
		case row.kind == "string":
			if fv.Kind() != reflect.String || fv.String() != sentinel {
				t.Errorf("spec.%s parsed as %s, want %q: parseHelmSpec does not read this name out of the spec map — the CRD and the table declare a field the controller cannot see", row.name, describe(fv), sentinel)
			}
		case row.kind == "boolean":
			if fv.Kind() != reflect.Bool || !fv.Bool() {
				t.Errorf("spec.%s parsed as %s, want true: parseHelmSpec does not read this name out of the spec map", row.name, describe(fv))
			}
		case row.kind == "array":
			if fv.Kind() != reflect.Slice || fv.Len() != 1 || fv.Index(0).FieldByName("name").String() != sentinel {
				t.Errorf("spec.%s parsed as %s, want one entry named %q: parseHelmSpec does not read this name out of the spec map", row.name, describe(fv), sentinel)
			}
		default: // object rows: set and setString parse to string maps
			if fv.Kind() != reflect.Map || fv.Len() != 1 {
				t.Errorf("spec.%s parsed as %s, want one-entry map[probe:%s] — parseHelmSpec does not read this name out of the spec map", row.name, describe(fv), sentinel)
				continue
			}
			k := fv.MapKeys()[0]
			if k.String() != "probe" || fv.MapIndex(k).String() != sentinel {
				t.Errorf("spec.%s parsed to entry %s=%s, want probe=%s — the name is read but not carried intact", row.name, k.String(), fv.MapIndex(k).String(), sentinel)
			}
		}
	}
}

// names a reflect.Value without Interface(), which panics on unexported fields.
func describe(fv reflect.Value) string {
	switch fv.Kind() {
	case reflect.String:
		return "string " + fv.String()
	case reflect.Bool:
		return fmt.Sprintf("bool %t", fv.Bool())
	case reflect.Int64, reflect.Int:
		return time.Duration(fv.Int()).String()
	case reflect.Map:
		parts := make([]string, 0, fv.Len())
		for _, k := range fv.MapKeys() {
			parts = append(parts, fmt.Sprintf("%s=%q", k.String(), fv.MapIndex(k).String()))
		}
		return "map[" + strings.Join(parts, " ") + "]"
	default:
		return fv.Kind().String()
	}
}

// the YAML declares the group as literals the Go side cannot import.
func TestHelmChartCRDsCarryTheEngineIdentity(t *testing.T) {
	want := map[string]bool{
		"helmcharts." + k8sutil.HelmChartGroup:       false,
		"helmchartconfigs." + k8sutil.HelmChartGroup: false,
	}
	for _, doc := range bytes.Split(addons.RenderHelmCRDs(), []byte("\n---\n")) {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var crd map[string]interface{}
		if err := yaml.Unmarshal(doc, &crd); err != nil {
			t.Fatalf("parsing embedded CRD bundle: %v", err)
		}
		name := str(nested(crd, "metadata", "name"), "")
		seen, known := want[name]
		if !known {
			t.Errorf("the bundle ships CRD %q, which is neither helmcharts.%s nor helmchartconfigs.%s — the CRD's group no longer follows k8sutil", name, k8sutil.HelmChartGroup, k8sutil.HelmChartGroup)
			continue
		}
		if seen {
			t.Errorf("the bundle ships CRD %q twice", name)
			continue
		}
		want[name] = true
		if got := str(nested(crd, "spec", "group"), ""); got != k8sutil.HelmChartGroup {
			t.Errorf("CRD %s: spec.group = %q, want k8sutil.HelmChartGroup %q — the webhook rule and the dynamic client would target a group these CRDs do not serve", name, got, k8sutil.HelmChartGroup)
		}
		versions, ok := nested(crd, "spec", "versions").([]interface{})
		if !ok || len(versions) == 0 {
			t.Fatalf("CRD %s declares no spec.versions", name)
		}
		for _, v := range versions {
			vm, ok := v.(map[string]interface{})
			if !ok {
				t.Fatalf("CRD %s: spec.versions entry is not a map", name)
			}
			if got := str(nested(vm, "name"), ""); got != k8sutil.HelmChartVersion {
				t.Errorf("CRD %s: version %q, want k8sutil.HelmChartVersion %q", name, got, k8sutil.HelmChartVersion)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("the bundle ships no CRD %q — one of the engine's two CRDs is missing", name)
		}
	}
}

// reads the CRD bundle the operator actually ships, so it tracks the deployed artifact.
func helmChartCRDSpecSchema(t *testing.T) map[string]interface{} {
	t.Helper()
	want := "helmcharts." + k8sutil.HelmChartGroup
	for _, doc := range bytes.Split(addons.RenderHelmCRDs(), []byte("\n---\n")) {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var crd map[string]interface{}
		if err := yaml.Unmarshal(doc, &crd); err != nil {
			t.Fatalf("parsing embedded CRD bundle: %v", err)
		}
		if str(nested(crd, "metadata", "name"), "") != want {
			continue
		}
		versions, ok := nested(crd, "spec", "versions").([]interface{})
		if !ok || len(versions) == 0 {
			t.Fatalf("%s declares no spec.versions", want)
		}
		v, ok := versions[0].(map[string]interface{})
		if !ok {
			t.Fatalf("%s spec.versions[0] is not a map", want)
		}
		spec, ok := nested(v, "schema", "openAPIV3Schema", "properties", "spec").(map[string]interface{})
		if !ok {
			t.Fatalf("%s v1 schema has no properties.spec", want)
		}
		return spec
	}
	t.Fatalf("CRD %s not found in the embedded bundle", want)
	return nil
}

func sortedKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func crdDefault(props map[string]interface{}, name string) (d interface{}, declared bool) {
	prop, ok := props[name].(map[string]interface{})
	if !ok {
		return nil, false
	}
	d, declared = prop["default"]
	return d, declared
}

// TestNothing is a cheap compile-canary target (`go test -run TestNothing`).
func TestNothing(t *testing.T) {}

// the hold lives in its own status field, which every Failed write preserves.
const tenantNS = "team-a"

// requesterBinding covers the object's spec, so o must carry its final spec first.
func stampAs(o *unstructured.Unstructured, info requesterInfo, signer *requesterSigner) {
	groups, _ := json.Marshal(info.Groups)
	o.SetAnnotations(map[string]string{
		annRequester:       info.Username,
		annRequesterUID:    info.UID,
		annRequesterGroups: string(groups),
		annRequesterSig:    signer.sign(requesterBinding(o), info),
	})
}

// adopted, with a per-test uid for tests that store overlays or stamps beside it.
func newFlowHCIn(t *testing.T, ns, name, uid string, spec map[string]interface{}) *unstructured.Unstructured {
	t.Helper()
	wantNoUnwrittenIncomplete(t, "at the start of a flow test")
	forgetUnwrittenIncomplete(t)
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "stratum.ciq.com/v1",
		"kind":       "HelmChart",
		"metadata": map[string]interface{}{
			"name":       name,
			"namespace":  ns,
			"uid":        uid,
			"finalizers": []interface{}{helmFinalizer},
		},
		"spec": spec,
	}}
}

func newFlowHCC(ns, name string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "stratum.ciq.com/v1",
		"kind":       "HelmChartConfig",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns},
		"spec":       spec,
	}}
}

// lists helmcharts and helmchartconfigs, so a test can drive reconcileHelmCharts.
func newFlowDynClient(t *testing.T, hc *unstructured.Unstructured, configs ...*unstructured.Unstructured) dynamic.Interface {
	t.Helper()
	objs := make([]runtime.Object, 0, 1+len(configs))
	objs = append(objs, hc)
	for _, c := range configs {
		objs = append(objs, c)
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			helmChartGVR:       "HelmChartList",
			helmChartConfigGVR: "HelmChartConfigList",
			secretGVR:          "SecretList",
		},
		objs...,
	)
}

func reconcileAllCharts(t *testing.T, dyn dynamic.Interface, pol helmPolicy) {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "missing-kubeconfig")
	reconcileHelmCharts(context.Background(), dyn, kubeconfig, pol)
}

func getHCIn(t *testing.T, dyn dynamic.Interface, ns, name string) *unstructured.Unstructured {
	t.Helper()
	hc, err := dyn.Resource(helmChartGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get helmchart %s/%s: %v", ns, name, err)
	}
	return hc
}

func updateHCIn(t *testing.T, dyn dynamic.Interface, ns, name string, mutate func(*unstructured.Unstructured)) {
	t.Helper()
	hc := getHCIn(t, dyn, ns, name)
	mutate(hc)
	if _, err := dyn.Resource(helmChartGVR).Namespace(ns).Update(context.Background(), hc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update helmchart %s/%s: %v", ns, name, err)
	}
}

func readStatusIn(t *testing.T, dyn dynamic.Interface, ns, name string) flowStatus {
	t.Helper()
	st, _, _ := unstructured.NestedMap(getHCIn(t, dyn, ns, name).Object, "status")
	return flowStatus{
		phase:    str(st["phase"], ""),
		message:  str(st["message"], ""),
		revision: asInt(st["revision"]),
	}
}

func wantHoldEstablished(t *testing.T, dyn dynamic.Interface, ns, name, when string) {
	t.Helper()
	st := readStatusIn(t, dyn, ns, name)
	if st.phase != hcPhaseFailed ||
		!strings.Contains(st.message, "were not deleted because their ownership could not be verified") ||
		!strings.Contains(st.message, "app-cm") {
		t.Fatalf("%s: helmchart %s/%s status = %+v, want Failed with Helm's not-deleted list naming app-cm", when, ns, name, st)
	}
}

// reverting the typo to identical values must still not reinstall over unverified objects.
func TestHelmChartHoldSurvivesValuesTypoAndRevert(t *testing.T) {
	shortIncompleteWriteBackoff(t)
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0"), "valuesContent": "a: 1\n"})

	reconcileFlow(t, dyn)

	wantUninstallIncomplete(t, dyn, "after the recovery uninstall")
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release %s/%s still stored, want it purged as Helm v4 does: %+v", flowNS, flowRelease, hist)
	}

	// the typo's parse error replaces the hold's message.
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(hc.Object, "a: [\n", "spec", "valuesContent"); err != nil {
			t.Fatal(err)
		}
	})
	reconcileFlow(t, dyn)
	if st := readFlowStatus(t, dyn); !strings.HasPrefix(st.message, "values: ") {
		t.Fatalf("status = %+v, want the values parse error stamped in place of the hold's message", st)
	}

	// only the hold can stop a reinstall now.
	kc.DummyResources = nil
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(hc.Object, "a: 1\n", "spec", "valuesContent"); err != nil {
			t.Fatal(err)
		}
	})
	pastBackoff(t, dyn)
	reconcileFlow(t, dyn)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("reverting the typo reinstalled over the objects Helm could not verify: %+v; status = %+v", hist, readFlowStatus(t, dyn))
	}
	if st := readFlowStatus(t, dyn); st.phase == hcPhaseDeployed {
		t.Fatalf("status = %+v, want the hold still in place once the typo is reverted", st)
	}
}

// migration must precede any write that replaces the message-only hold.
func TestLegacyUninstallHoldSurvivesValuesTypoAndRevert(t *testing.T) {
	shortIncompleteWriteBackoff(t)
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	dyn := newFlowHC(t, map[string]interface{}{"chartContent": flowChart(t, "1.0.0"), "valuesContent": "a: 1\n"})
	reconcileFlow(t, dyn)
	wantUninstallIncomplete(t, dyn, "after the recovery uninstall")

	const oldRecoveryPrefix = uninstallIncompletePrefix + "remove these objects by hand, then change the spec to reinstall: "
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		st, _, _ := unstructured.NestedMap(hc.Object, "status")
		delete(st, holdField)
		st["message"] = oldRecoveryPrefix + strings.TrimPrefix(str(st["message"], ""), uninstallIncompleteRecover)
		if err := unstructured.SetNestedMap(hc.Object, st, "status"); err != nil {
			t.Fatal(err)
		}
	})

	// the first reconcile after an upgrade can itself hit a values parse failure.
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(hc.Object, "a: [\n", "spec", "valuesContent"); err != nil {
			t.Fatal(err)
		}
	})
	reconcileFlow(t, dyn)
	if st := readFlowStatus(t, dyn); !strings.HasPrefix(st.message, "values: ") {
		t.Fatalf("status = %+v, want the values parse error", st)
	}
	kc.DummyResources = nil
	updateFlowHC(t, dyn, func(hc *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(hc.Object, "a: 1\n", "spec", "valuesContent"); err != nil {
			t.Fatal(err)
		}
	})
	pastBackoff(t, dyn)
	reconcileFlow(t, dyn)
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("reverting the typo reinstalled over objects held by the old status: %+v", hist)
	}
	st, _, _ := unstructured.NestedMap(getFlowHC(t, dyn).Object, "status")
	if _, ok := incompleteHoldFrom(st); !ok {
		t.Fatalf("legacy hold was not migrated into status.%s: %+v", holdField, st)
	}
}

func TestDeletionIncompleteMessageLegacyRecovery(t *testing.T) {
	msg := legacyUninstallIncompleteRecover + "ConfigMap/app-cm"
	want := uninstallIncompleteDelete + "ConfigMap/app-cm"
	if got := deletionIncompleteMessage(msg); got != want {
		t.Fatalf("deletion instruction = %q, want %q", got, want)
	}
}

// the refusal replaces the hold's text; deleting then must keep the finalizer and report the hold.
func TestHelmChartDeletionKeepsHoldAfterRefusedOverlayEdit(t *testing.T) {
	shortIncompleteWriteBackoff(t)
	signer := testSigner()
	pol := helmPolicy{privilegedNamespaces: []string{"kube-system"}, signer: signer}
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, tenantNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	forbidOwnershipCheck(t, h)
	hc := newFlowHCIn(t, tenantNS, flowName, "policy-refused-edit", map[string]interface{}{"chartContent": flowChart(t, "1.0.0")})
	stampAs(hc, requesterInfo{Username: "alice", Groups: []string{"system:authenticated"}}, signer)
	dyn := newFlowDynClient(t, hc)

	reconcileAllCharts(t, dyn, pol)

	wantHoldEstablished(t, dyn, tenantNS, flowName, "after the recovery uninstall")
	if hist := h.history(t, tenantNS, flowRelease); hist != nil {
		t.Fatalf("release %s/%s still stored, want it purged as Helm v4 does: %+v", tenantNS, flowRelease, hist)
	}

	// an overlay by another requester is refused, replacing the hold's message.
	hcc := newFlowHCC(tenantNS, flowName, map[string]interface{}{"valuesContent": "b: 2\n"})
	stampAs(hcc, requesterInfo{Username: "bob", Groups: []string{"system:authenticated"}}, signer)
	if _, err := dyn.Resource(helmChartConfigGVR).Namespace(tenantNS).Create(context.Background(), hcc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create helmchartconfig: %v", err)
	}
	reconcileAllCharts(t, dyn, pol)
	if st := readStatusIn(t, dyn, tenantNS, flowName); !strings.HasPrefix(st.message, "refusing to install: the HelmChartConfig overlay") {
		t.Fatalf("status = %+v, want the refused overlay stamped in place of the hold's message", st)
	}

	// deleting the HelmChart now must keep the finalizer and report the hold.
	updateHCIn(t, dyn, tenantNS, flowName, func(hc *unstructured.Unstructured) {
		deleted := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		hc.SetDeletionTimestamp(&deleted)
	})
	reconcileAllCharts(t, dyn, pol)

	if !hasFinalizer(getHCIn(t, dyn, tenantNS, flowName), helmFinalizer) {
		t.Fatalf("finalizer removed although the deleted HelmChart was held; status = %+v", readStatusIn(t, dyn, tenantNS, flowName))
	}
	// a person deleting the HelmChart is told what to remove.
	wantHoldEstablished(t, dyn, tenantNS, flowName, "after the deletion reconcile")
}

// a List failure must not reconcile the chart with the overlays the failed List dropped.
func TestHelmChartConfigListFailureKeepsHold(t *testing.T) {
	shortIncompleteWriteBackoff(t)
	h := useFakeHelm(t, fakeHelmOptions{})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 1, status: "pending-install", chartVersion: "1.0.0"})
	kc := forbidOwnershipCheck(t, h)
	hc := newFlowHCIn(t, flowNS, flowName, "policy-config-list-failure",
		map[string]interface{}{"chartContent": flowChart(t, "1.0.0"), "valuesContent": "a: 1\n"})
	hcc := newFlowHCC(flowNS, flowName, map[string]interface{}{"valuesContent": "b: 2\n"})
	dyn := newFlowDynClient(t, hc, hcc)

	reconcileAllCharts(t, dyn, flowPolicy)

	wantUninstallIncomplete(t, dyn, "after the recovery uninstall")
	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("release %s/%s still stored, want it purged as Helm v4 does: %+v", flowNS, flowRelease, hist)
	}

	// the overlay is missing from the merge during the outage.
	kc.DummyResources = nil
	dyn.(*dynfake.FakeDynamicClient).PrependReactor("list", helmChartConfigGVR.Resource,
		func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("helmchartconfig list refused")
		})
	reconcileAllCharts(t, dyn, flowPolicy)

	if hist := h.history(t, flowNS, flowRelease); hist != nil {
		t.Fatalf("a HelmChartConfig List failure ended the hold and installed over the objects Helm could not verify, with the overlay's values dropped: %+v; status = %+v", hist, readFlowStatus(t, dyn))
	}
	if st := readFlowStatus(t, dyn); st.phase != hcPhaseFailed || !strings.HasPrefix(st.message, uninstallIncompletePrefix) {
		t.Fatalf("status = %+v, want the hold intact (Failed with %q)", st, uninstallIncompletePrefix)
	}
}

// one source of truth: version.HelmCacheDir, so the --helm-cache flag and the dir agree.
func TestOperatorHelmCacheDirIsTheVersionLayout(t *testing.T) {
	if helmCacheDir != version.HelmCacheDir {
		t.Fatalf("operator helm cache dir = %q, want version.HelmCacheDir %q — the --helm-cache flag the supervisor passes and the dir loadChart writes must come from one source of truth, not from a literal beside it", helmCacheDir, version.HelmCacheDir)
	}
	if legacyHelmCacheDir != version.LegacyHelmCacheDir {
		t.Fatalf("legacy helm cache dir = %q, want version.LegacyHelmCacheDir %q — the migration's source and uninstall's legacy target must come from one declaration, not from literals beside it", legacyHelmCacheDir, version.LegacyHelmCacheDir)
	}
	rel, err := filepath.Rel(version.DataDir, version.HelmCacheDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		t.Errorf("version.HelmCacheDir = %q is not under the data dir %q; the operator's helm cache must live inside the one state directory", version.HelmCacheDir, version.DataDir)
	}
}

// named repository resolution must use the cache and configuration under the data root.
func TestOperatorHelmCacheDerivesFromTheDataRoot(t *testing.T) {
	data, conf, run := version.DataDir, version.ConfDir, version.RunDir
	version.SetRoots(t.TempDir(), t.TempDir(), t.TempDir())
	t.Cleanup(func() { version.SetRoots(data, conf, run) })
	t.Setenv("HELM_CONTENT_CACHE", filepath.Join(version.HelmCacheDir, "content"))
	tgz := makeChartTGZ(t, "widget", "1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	cache := filepath.Join(version.HelmCacheDir, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("repositories:\n- name: local\n  url: %s\n", srv.URL)
	index := "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [widget.tgz]\n"
	for path, contents := range map[string]string{
		filepath.Join(version.HelmCacheDir, "repositories.yaml"): config,
		filepath.Join(cache, "local-index.yaml"):                 index,
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := loadChart(t.Context(), helmSpec{chart: "local/widget", version: "1.0.0", timeout: 3 * time.Second}, "web", "test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata.Name != "widget" || got.Metadata.Version != "1.0.0" {
		t.Fatalf("unexpected chart: %+v", got.Metadata)
	}
}

// repositories.yaml follows the config home, not HELM_CACHE_HOME, so loadChart sets both.
func TestHelmCacheHomeDoesNotCarryTheOperatorPaths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HELM_CACHE_HOME", dir)
	if got, want := helmpath.CachePath("repository"), filepath.Join(dir, "repository"); got != want {
		t.Errorf("helmpath.CachePath(\"repository\") = %q under HELM_CACHE_HOME=%q, want %q (a set HELM_CACHE_HOME is used exactly, with no extra path element)", got, dir, want)
	}
	if got := helmpath.ConfigPath("repositories.yaml"); strings.HasPrefix(got, dir) {
		t.Errorf("helmpath.ConfigPath(\"repositories.yaml\") = %q followed HELM_CACHE_HOME %q; repositories.yaml follows the config home, so the env var alone cannot carry the operator's cache layout", got, dir)
	}
}

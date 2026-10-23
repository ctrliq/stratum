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
	"net"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// valuesSecret is a Secret ns/name at resourceVersion rv whose data holds the
// given keys.
func valuesSecret(ns, name, rv string, data map[string]string) *unstructured.Unstructured {
	d := map[string]interface{}{}
	for k, v := range data {
		d[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]interface{}{"name": name, "namespace": ns, "uid": "uid-" + name, "resourceVersion": rv},
		"data":       d,
	}}
}

func secretsClient(objs ...runtime.Object) dynamic.Interface {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{secretGVR: "SecretList"}, objs...)
}

func TestParseValuesSecrets(t *testing.T) {
	t.Parallel()
	got := parseValuesSecrets([]interface{}{
		map[string]interface{}{"name": "a", "keys": []interface{}{"values.yaml", "more.yaml"}},
		map[string]interface{}{"name": "b"},
	})
	want := []valuesSecretRef{{name: "a", keys: []string{"values.yaml", "more.yaml"}}, {name: "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseValuesSecrets = %+v, want %+v", got, want)
	}
	if got := parseValuesSecrets(nil); len(got) != 0 {
		t.Errorf("parseValuesSecrets(nil) = %+v, want none", got)
	}
}

// Every listed key is read in order, and versions name each Secret's
// resourceVersion; a missing Secret or key, or a body that is not a values
// map, fails without quoting the Secret's content.
func TestReadValuesSecrets(t *testing.T) {
	t.Parallel()
	const secretWord = "hunter2-not-a-map"
	dyn := secretsClient(
		valuesSecret("ns", "a", "7", map[string]string{"one.yaml": "x: 1\n", "two.yaml": "x: 2\nw: 2\n", "scalar": secretWord}),
		valuesSecret("ns", "b", "9", map[string]string{"values.yaml": "z: 3\n"}),
	)
	vals, versions, err := readValuesSecrets(context.Background(), dyn, "ns", []valuesSecretRef{
		{name: "a", keys: []string{"two.yaml", "one.yaml"}},
		{name: "b", keys: []string{"values.yaml"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]interface{}{{"x": float64(2), "w": float64(2)}, {"x": float64(1)}, {"z": float64(3)}}
	if !reflect.DeepEqual(vals, want) {
		t.Errorf("values = %v, want %v, in the listed order", vals, want)
	}
	if len(versions) != 3 || !strings.Contains(versions[0], "\x007") || !strings.Contains(versions[2], "\x009") {
		t.Errorf("versions = %q, want one per key carrying its Secret's resourceVersion", versions)
	}

	for _, tc := range []struct {
		name string
		refs []valuesSecretRef
		want string
	}{
		{"missing Secret", []valuesSecretRef{{name: "nope", keys: []string{"k"}}}, "valuesSecrets[0]: read Secret ns/nope"},
		{"missing key", []valuesSecretRef{{name: "b", keys: []string{"other.yaml"}}}, `Secret ns/b has no key "other.yaml"`},
		{"not a values map", []valuesSecretRef{{name: "b", keys: []string{"values.yaml"}}, {name: "a", keys: []string{"scalar"}}}, `valuesSecrets[1]: key "scalar" of Secret ns/a is not a values.yaml map`},
		{"no keys", []valuesSecretRef{{name: "a"}}, "valuesSecrets[0]: name and keys are required"},
	} {
		_, _, err := readValuesSecrets(context.Background(), dyn, "ns", tc.refs)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), secretWord) {
			t.Errorf("%s: err %q quotes the Secret's content", tc.name, err)
		}
	}
}

// Secrets sit over valuesContent and the HelmChartConfig and under set and
// setString, later Secrets over earlier ones.
func TestMergedValuesLayersValuesSecretsBetweenTheOverlayAndSet(t *testing.T) {
	t.Parallel()
	spec := helmSpec{
		valuesContent: "a: content\nb: content\nc: content\nd: content\n",
		set:           map[string]string{"d": "set"},
	}
	got, err := mergedValues(spec, "a: hcc\nb: hcc\n",
		map[string]interface{}{"b": "secret1", "c": "secret1", "d": "secret1"},
		map[string]interface{}{"c": "secret2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]interface{}{"a": "hcc", "b": "secret1", "c": "secret2", "d": "set"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %v, want %v", got, want)
	}
}

// A chart without valuesSecrets hashes exactly as released builds did, so an
// operator upgrade does not upgrade every release.
func TestSpecHashWithoutValuesSecretsIsTheReleasedHash(t *testing.T) {
	t.Parallel()
	spec := helmSpec{repo: "https://charts.example", chart: "app", version: "1.2.3", chartContent: "Y2hhcnQ="}
	vals := map[string]interface{}{"a": map[string]interface{}{"b": "c"}, "n": float64(2)}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00", spec.repo, spec.chart, spec.version)
	cc := sha256.Sum256([]byte(spec.chartContent))
	h.Write(cc[:])
	b, _ := json.Marshal(vals)
	h.Write(b)
	if released, got := fmt.Sprintf("%x", h.Sum(nil)), specHash(spec, vals); got != released {
		t.Errorf("specHash = %s, want the released %s", got, released)
	}
}

// lastReleaseValues is the values the newest stored revision of ns/name was
// installed with.
func lastReleaseValues(t *testing.T, h *fakeHelm, ns, name string) map[string]interface{} {
	t.Helper()
	h.mem.SetNamespace(ns)
	rel, err := h.cfg.Releases.Last(name)
	if err != nil {
		t.Fatalf("last release %s/%s: %v", ns, name, err)
	}
	return rel.Config
}

// The password case: the value reaches the release from a Secret,
// never appears in the HelmChart, status included, and a change to the Secret
// upgrades the release, while a reconcile with nothing changed does not.
func TestValuesSecretsReachTheReleaseAndStayOutOfTheHelmChart(t *testing.T) {
	const password = "s3cret-pw"
	h := useFakeHelm(t, fakeHelmOptions{})
	hc := newFlowHCIn(t, flowNS, flowName, "secretvals-flow", map[string]interface{}{
		"chartContent":  flowChart(t, "1.0.0"),
		"set":           map[string]interface{}{"replicas": "2"},
		"valuesSecrets": []interface{}{map[string]interface{}{"name": "app-params", "keys": []interface{}{"values.yaml"}}},
	})
	dyn := newFlowDynClient(t, hc, valuesSecret(flowNS, "app-params", "1", map[string]string{"values.yaml": "admin:\n  password: " + password + "\n"}))

	reconcileAllCharts(t, dyn, flowPolicy)
	if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseDeployed || st.revision != 1 {
		t.Fatalf("status = %+v, want Deployed revision 1", st)
	}
	vals := lastReleaseValues(t, h, flowNS, flowRelease)
	if admin, _ := vals["admin"].(map[string]interface{}); admin["password"] != password || vals["replicas"] != int64(2) {
		t.Errorf("release values = %v, want the Secret's admin.password and spec.set's replicas", vals)
	}
	stored, _ := json.Marshal(getHCIn(t, dyn, flowNS, flowName).Object)
	if strings.Contains(string(stored), password) {
		t.Errorf("the HelmChart carries the Secret's value: %s", stored)
	}

	// The hot path skips Helm only while the release Secret exists in the
	// cluster; the fake Helm keeps releases in memory, so store it.
	if _, err := dyn.Resource(secretGVR).Namespace(flowNS).Create(context.Background(),
		valuesSecret(flowNS, "sh.helm.release.v1."+flowRelease+".v1", "1", nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcileAllCharts(t, dyn, flowPolicy)
	if st := readStatusIn(t, dyn, flowNS, flowName); st.revision != 1 {
		t.Fatalf("an unchanged reconcile moved the release to revision %d", st.revision)
	}

	updated := valuesSecret(flowNS, "app-params", "2", map[string]string{"values.yaml": "admin:\n  password: rotated\n"})
	if _, err := dyn.Resource(secretGVR).Namespace(flowNS).Update(context.Background(), updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcileAllCharts(t, dyn, flowPolicy)
	if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseDeployed || st.revision != 2 {
		t.Fatalf("status after the Secret changed = %+v, want Deployed revision 2", st)
	}
	if admin, _ := lastReleaseValues(t, h, flowNS, flowRelease)["admin"].(map[string]interface{}); admin["password"] != "rotated" {
		t.Errorf("upgraded release admin = %v, want the rotated password", admin)
	}
}

// A HelmChart applied before its Secret fails with a reason, and installs on
// the first reconcile after the Secret exists, with no failed-chart backoff.
func TestValuesSecretsWaitForAMissingSecret(t *testing.T) {
	useFakeHelm(t, fakeHelmOptions{})
	hc := newFlowHCIn(t, flowNS, flowName, "secretvals-missing", map[string]interface{}{
		"chartContent":  flowChart(t, "1.0.0"),
		"valuesSecrets": []interface{}{map[string]interface{}{"name": "app-params", "keys": []interface{}{"values.yaml"}}},
	})
	dyn := newFlowDynClient(t, hc)

	reconcileAllCharts(t, dyn, flowPolicy)
	if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseFailed || !strings.Contains(st.message, "valuesSecrets[0]: read Secret kube-system/app-params") {
		t.Fatalf("status = %+v, want Failed naming the missing Secret", st)
	}
	if _, err := dyn.Resource(secretGVR).Namespace(flowNS).Create(context.Background(),
		valuesSecret(flowNS, "app-params", "1", map[string]string{"values.yaml": "a: 1\n"}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcileAllCharts(t, dyn, flowPolicy)
	if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseDeployed {
		t.Fatalf("status after the Secret appeared = %+v, want Deployed", st)
	}
}

// A Secret-backed chart's hash covers its public inputs as written and the
// Secrets' versions. The values merged without the Secrets were not enough: a
// set entry that takes back a key a Secret overrides changes the release but
// not that merge, so the hot path skipped the upgrade.
func TestSecretSpecHash(t *testing.T) {
	t.Parallel()
	versions := []string{"s\x00values.yaml\x00uid\x001"}
	base := helmSpec{chart: "app", valuesContent: "password: fallback\n"}
	h := secretSpecHash(base, "", versions)
	takeBack := base
	takeBack.set = map[string]string{"password": "fallback"}
	if secretSpecHash(takeBack, "", versions) == h {
		t.Error("a set entry over a key the Secret overrides leaves the hash unchanged, so the upgrade is skipped")
	}
	if secretSpecHash(base, "", []string{"s\x00values.yaml\x00uid\x002"}) == h {
		t.Error("a Secret's new resourceVersion leaves the hash unchanged")
	}
	if secretSpecHash(base, "x: 1\n", versions) == h {
		t.Error("a HelmChartConfig overlay leaves the hash unchanged")
	}
	if secretSpecHash(base, "", versions) != h {
		t.Error("the hash is not stable")
	}
	if specHash(base, map[string]interface{}{"password": "fallback"}) == h {
		t.Error("a Secret-backed chart hashes like one without Secrets")
	}
}

// A configuration valid only with the Secret's values installs: valuesContent
// makes a a scalar, the Secret makes it a map, and set adds a key to that map.
// Merging the public layers alone fails on it, so neither the hash nor any
// other step may depend on that merge.
func TestValuesSecretsInstallWhatOnlyTheSecretMakesValid(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	hc := newFlowHCIn(t, flowNS, flowName, "secretvals-valid-only-with-secret", map[string]interface{}{
		"chartContent":  flowChart(t, "1.0.0"),
		"valuesContent": "a: scalar\n",
		"set":           map[string]interface{}{"a.c": "2"},
		"valuesSecrets": []interface{}{map[string]interface{}{"name": "app-params", "keys": []interface{}{"values.yaml"}}},
	})
	dyn := newFlowDynClient(t, hc, valuesSecret(flowNS, "app-params", "1", map[string]string{"values.yaml": "a:\n  b: 1\n"}))
	reconcileAllCharts(t, dyn, flowPolicy)
	if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseDeployed {
		t.Fatalf("status = %+v, want Deployed", st)
	}
	if a, _ := lastReleaseValues(t, h, flowNS, flowRelease)["a"].(map[string]interface{}); a["b"] != float64(1) || a["c"] != int64(2) {
		t.Errorf("release a = %v, want b from the Secret and c from set", a)
	}
}

// failChart is spec.chartContent for a chart named app whose template fails
// with admin.password as its message when boom is set, as a chart can.
func failChart(t *testing.T) string {
	t.Helper()
	files := map[string]string{
		"app/Chart.yaml":          "apiVersion: v2\nname: app\nversion: 1.0.0\n",
		"app/templates/cm.yaml":   "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-cm\n",
		"app/templates/fail.yaml": "{{- if .Values.boom }}{{ fail .Values.admin.password }}{{- end }}\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for path, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// Helm's error can quote values, and for a chart without valuesSecrets a
// failed install or upgrade copies it into status.message, readable wherever
// the HelmChart is. For a Secret-backed chart the status says only that it
// failed.
func TestValuesSecretsKeepHelmErrorsOutOfTheStatus(t *testing.T) {
	const password = "s3cret-in-a-helm-error"
	secretSpec := func(boom bool) map[string]interface{} {
		spec := map[string]interface{}{
			"chartContent":  failChart(t),
			"valuesSecrets": []interface{}{map[string]interface{}{"name": "app-params", "keys": []interface{}{"values.yaml"}}},
		}
		if boom {
			spec["set"] = map[string]interface{}{"boom": "true"}
		}
		return spec
	}
	wantFailedWithoutTheValue := func(t *testing.T, dyn dynamic.Interface, action string) {
		t.Helper()
		st := readStatusIn(t, dyn, flowNS, flowName)
		if st.phase != hcPhaseFailed || !strings.HasPrefix(st.message, action+" failed; Helm's error is only in the operator's log") {
			t.Errorf("status = %+v, want %s failed with the error left to the operator's log", st, action)
		}
		if stored, _ := json.Marshal(getHCIn(t, dyn, flowNS, flowName).Object); strings.Contains(string(stored), password) {
			t.Errorf("the HelmChart carries the Secret's value after a failed %s: %s", action, stored)
		}
	}
	secret := func() *unstructured.Unstructured {
		return valuesSecret(flowNS, "app-params", "1", map[string]string{"values.yaml": "admin:\n  password: " + password + "\n"})
	}

	t.Run("install", func(t *testing.T) {
		useFakeHelm(t, fakeHelmOptions{})
		dyn := newFlowDynClient(t, newFlowHCIn(t, flowNS, flowName, "secretvals-fail-install", secretSpec(true)), secret())
		reconcileAllCharts(t, dyn, flowPolicy)
		wantFailedWithoutTheValue(t, dyn, "install")
	})
	t.Run("upgrade", func(t *testing.T) {
		useFakeHelm(t, fakeHelmOptions{})
		dyn := newFlowDynClient(t, newFlowHCIn(t, flowNS, flowName, "secretvals-fail-upgrade", secretSpec(false)), secret())
		reconcileAllCharts(t, dyn, flowPolicy)
		if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseDeployed {
			t.Fatalf("status = %+v, want the first install Deployed", st)
		}
		updateHCIn(t, dyn, flowNS, flowName, func(hc *unstructured.Unstructured) { hc.Object["spec"] = secretSpec(true) })
		reconcileAllCharts(t, dyn, flowPolicy)
		wantFailedWithoutTheValue(t, dyn, "upgrade")
	})
	t.Run("without valuesSecrets the error stays in the status", func(t *testing.T) {
		useFakeHelm(t, fakeHelmOptions{})
		dyn := newFlowDynClient(t, newFlowHCIn(t, flowNS, flowName, "secretvals-fail-plain", map[string]interface{}{
			"chartContent": failChart(t),
			"set":          map[string]interface{}{"boom": "true", "admin.password": "visible"},
		}))
		reconcileAllCharts(t, dyn, flowPolicy)
		if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseFailed || !strings.Contains(st.message, "install: ") || !strings.Contains(st.message, "visible") {
			t.Errorf("status = %+v, want Helm's own error", st)
		}
	})
}

// The take-back case end to end: valuesContent sets a password, the Secret
// overrides it, and a later set entry takes it back. The release must follow.
func TestValuesSecretsUpgradeWhenSetTakesBackAKey(t *testing.T) {
	h := useFakeHelm(t, fakeHelmOptions{})
	spec := map[string]interface{}{
		"chartContent":  flowChart(t, "1.0.0"),
		"valuesContent": "password: fallback\n",
		"valuesSecrets": []interface{}{map[string]interface{}{"name": "app-params", "keys": []interface{}{"values.yaml"}}},
	}
	dyn := newFlowDynClient(t, newFlowHCIn(t, flowNS, flowName, "secretvals-take-back", spec),
		valuesSecret(flowNS, "app-params", "1", map[string]string{"values.yaml": "password: from-secret\n"}),
		valuesSecret(flowNS, "sh.helm.release.v1."+flowRelease+".v1", "1", nil))
	reconcileAllCharts(t, dyn, flowPolicy)
	if got := lastReleaseValues(t, h, flowNS, flowRelease)["password"]; got != "from-secret" {
		t.Fatalf("release password = %v, want the Secret's", got)
	}
	updateHCIn(t, dyn, flowNS, flowName, func(hc *unstructured.Unstructured) {
		hc.Object["spec"].(map[string]interface{})["set"] = map[string]interface{}{"password": "fallback"}
	})
	reconcileAllCharts(t, dyn, flowPolicy)
	if st := readStatusIn(t, dyn, flowNS, flowName); st.revision != 2 {
		t.Fatalf("status = %+v, want an upgrade to revision 2", st)
	}
	if got := lastReleaseValues(t, h, flowNS, flowRelease)["password"]; got != "fallback" {
		t.Errorf("release password = %v, want set's", got)
	}
}

// A release rendered with Secret values keeps them in Helm's history after
// spec.valuesSecrets is removed, and a recovery can roll back to it: an error
// from that rollback stays out of the status, as the status marks.
func TestValuesSecretsKeepRecoveryErrorsOutOfTheStatusAfterRemoval(t *testing.T) {
	const password = "s3cret-in-a-rollback-error"
	h := useFakeHelm(t, fakeHelmOptions{})
	dyn := newFlowDynClient(t, newFlowHCIn(t, flowNS, flowName, "secretvals-rollback", map[string]interface{}{
		"chartContent":  flowChart(t, "1.0.0"),
		"valuesSecrets": []interface{}{map[string]interface{}{"name": "app-params", "keys": []interface{}{"values.yaml"}}},
	}), valuesSecret(flowNS, "app-params", "1", map[string]string{"values.yaml": "admin:\n  password: " + password + "\n"}))
	reconcileAllCharts(t, dyn, flowPolicy)
	if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseDeployed || st.revision != 1 {
		t.Fatalf("status = %+v, want Deployed revision 1", st)
	}

	// The references go, an upgrade is cut short, and rolling back to the
	// Secret-backed revision 1 fails with an error that quotes its values.
	updateHCIn(t, dyn, flowNS, flowName, func(hc *unstructured.Unstructured) {
		hc.Object["spec"] = map[string]interface{}{"chartContent": flowChart(t, "1.1.0")}
	})
	h.seed(t, flowNS, flowRelease, helmRecord{revision: 2, status: "pending-upgrade", chartVersion: "1.1.0"})
	fakeKubeClient(t, h).UpdateError = fmt.Errorf(`Secret "app" is invalid: data: Invalid value: %q`, password)
	reconcileAllCharts(t, dyn, flowPolicy)

	st := readStatusIn(t, dyn, flowNS, flowName)
	if st.phase != hcPhaseFailed || !strings.HasPrefix(st.message, "recover pending-upgrade release failed; Helm's error is only in the operator's log") {
		t.Errorf("status = %+v, want the failed recovery with its error left to the operator's log", st)
	}
	stored := getHCIn(t, dyn, flowNS, flowName)
	if b, _ := json.Marshal(stored.Object); strings.Contains(string(b), password) {
		t.Errorf("the HelmChart carries the old revision's Secret value: %s", b)
	}
	if marked, _, _ := unstructured.NestedBool(stored.Object, "status", valuesFromSecretsField); !marked {
		t.Errorf("status.%s cleared after spec.valuesSecrets was removed; the history still holds Secret values", valuesFromSecretsField)
	}
}

// A Deployed chart keeps its release through a Secret read that may work next
// pass: marking it Failed made the next good pass upgrade it with the same
// values. A Secret that is gone still fails it.
func TestValuesSecretsDeployedChartRidesOutAFailedRead(t *testing.T) {
	spec := func() map[string]interface{} {
		return map[string]interface{}{
			"chartContent":  flowChart(t, "1.0.0"),
			"valuesSecrets": []interface{}{map[string]interface{}{"name": "app-params", "keys": []interface{}{"values.yaml"}}},
		}
	}
	wantStillDeployed := func(t *testing.T, dyn dynamic.Interface, ns, when string) {
		t.Helper()
		if st := readStatusIn(t, dyn, ns, flowName); st.phase != hcPhaseDeployed || st.revision != 1 {
			t.Errorf("%s: status = %+v, want still Deployed at revision 1", when, st)
		}
	}

	t.Run("a throttled read", func(t *testing.T) {
		useFakeHelm(t, fakeHelmOptions{})
		dyn := newFlowDynClient(t, newFlowHCIn(t, flowNS, flowName, "secretvals-throttled", spec()),
			valuesSecret(flowNS, "app-params", "1", map[string]string{"values.yaml": "a: 1\n"}),
			valuesSecret(flowNS, "sh.helm.release.v1."+flowRelease+".v1", "1", nil))
		reconcileAllCharts(t, dyn, flowPolicy)
		wantStillDeployed(t, dyn, flowNS, "after the install")

		throttle := true
		dyn.(*dynfake.FakeDynamicClient).PrependReactor("get", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
			if throttle && action.(ktesting.GetAction).GetName() == "app-params" {
				return true, nil, apierrors.NewTooManyRequests("slow down", 1)
			}
			return false, nil, nil
		})
		reconcileAllCharts(t, dyn, flowPolicy)
		wantStillDeployed(t, dyn, flowNS, "during the throttled read")
		throttle = false
		reconcileAllCharts(t, dyn, flowPolicy)
		wantStillDeployed(t, dyn, flowNS, "once the read works again")

		if err := dyn.Resource(secretGVR).Namespace(flowNS).Delete(context.Background(), "app-params", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		reconcileAllCharts(t, dyn, flowPolicy)
		if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseFailed || !strings.Contains(st.message, "not found") {
			t.Errorf("status after the Secret was deleted = %+v, want Failed naming it", st)
		}
	})
}

// Outside a privileged namespace a HelmChart that names valuesSecrets is
// refused before any Secret is read, stamped requester or not: a Secret's
// writer, who need not be able to write the HelmChart, would otherwise steer
// the release with the requester's authority.
func TestValuesSecretsAreRefusedOutsideAPrivilegedNamespace(t *testing.T) {
	signer := testSigner()
	h := useFakeHelm(t, fakeHelmOptions{})
	hc := newFlowHCIn(t, tenantNS, flowName, "secretvals-tenant", map[string]interface{}{
		"chartContent":  flowChart(t, "1.0.0"),
		"valuesSecrets": []interface{}{map[string]interface{}{"name": "app-params", "keys": []interface{}{"values.yaml"}}},
	})
	stampAs(hc, requesterInfo{Username: "alice", Groups: []string{"system:authenticated"}}, signer)
	dyn := newFlowDynClient(t, hc, valuesSecret(tenantNS, "app-params", "1", map[string]string{"values.yaml": "a: 1\n"}))
	reads := 0
	dyn.(*dynfake.FakeDynamicClient).PrependReactor("get", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.(ktesting.GetAction).GetName() == "app-params" {
			reads++
		}
		return false, nil, nil
	})
	reconcileAllCharts(t, dyn, helmPolicy{privilegedNamespaces: []string{"kube-system"}, signer: signer})
	if st := readStatusIn(t, dyn, tenantNS, flowName); st.phase != hcPhaseFailed || !strings.Contains(st.message, "spec.valuesSecrets is honoured only in a privileged namespace") {
		t.Errorf("status = %+v, want the privileged-namespace refusal", st)
	}
	if reads != 0 {
		t.Errorf("the Secret was read %d time(s) for a refused HelmChart", reads)
	}
	if hist := h.history(t, tenantNS, flowRelease); hist != nil {
		t.Errorf("release history %+v, want nothing installed", hist)
	}
}

// Only what can work unchanged on the next pass is retried; anything else
// fails the chart, so an edit that cannot work is reported, not ignored.
func TestRetryableSecretRead(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Resource: "secrets"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"429", apierrors.NewTooManyRequests("slow down", 1), true},
		{"408", apierrors.NewTimeoutError("timeout", 1), true},
		{"500", apierrors.NewInternalError(errors.New("boom")), true},
		{"503", apierrors.NewServiceUnavailable("restarting"), true},
		{"a refused connection", fmt.Errorf("read Secret: %w", &url.Error{Op: "Get", URL: "https://x", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}), true},
		{"an unexpected EOF", fmt.Errorf("read Secret: %w", io.ErrUnexpectedEOF), true},
		{"the pass's context ending", fmt.Errorf("read Secret: %w", context.DeadlineExceeded), true},
		{"not found", fmt.Errorf("read Secret: %w", apierrors.NewNotFound(gr, "x")), false},
		{"forbidden", apierrors.NewForbidden(gr, "x", errors.New("no")), false},
		{"bad request", apierrors.NewBadRequest("no"), false},
		{"a reference client-go refuses", errors.New(`resource name may not contain '/'`), false},
	} {
		if got := retryableSecretRead(tc.err); got != tc.want {
			t.Errorf("%s: retryableSecretRead = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// A name client-go would refuse before sending is reported before any read.
func TestReadValuesSecretsRefusesAnInvalidName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"app/params", "app%2Fparams", "App-Params"} {
		reads := 0
		dyn := secretsClient()
		dyn.(*dynfake.FakeDynamicClient).PrependReactor("get", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
			reads++
			return false, nil, nil
		})
		_, _, err := readValuesSecrets(context.Background(), dyn, "ns", []valuesSecretRef{{name: name, keys: []string{"values.yaml"}}})
		if err == nil || !strings.Contains(err.Error(), "is not a Secret name") || retryableSecretRead(err) {
			t.Errorf("%q: err = %v, want a refusal that is not retried", name, err)
		}
		if reads != 0 {
			t.Errorf("%q: read %d time(s), want none", name, reads)
		}
	}
}

// A Deployed chart edited to a reference that cannot work goes Failed with
// the reason, instead of staying Deployed and ignoring the edit.
func TestValuesSecretsReportAnInvalidReferenceOnADeployedChart(t *testing.T) {
	useFakeHelm(t, fakeHelmOptions{})
	dyn := newFlowDynClient(t, newFlowHCIn(t, flowNS, flowName, "secretvals-invalid-name", map[string]interface{}{
		"chartContent":  flowChart(t, "1.0.0"),
		"valuesSecrets": []interface{}{map[string]interface{}{"name": "app-params", "keys": []interface{}{"values.yaml"}}},
	}), valuesSecret(flowNS, "app-params", "1", map[string]string{"values.yaml": "a: 1\n"}),
		valuesSecret(flowNS, "sh.helm.release.v1."+flowRelease+".v1", "1", nil))
	reconcileAllCharts(t, dyn, flowPolicy)
	if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseDeployed {
		t.Fatalf("status = %+v, want Deployed", st)
	}
	updateHCIn(t, dyn, flowNS, flowName, func(hc *unstructured.Unstructured) {
		hc.Object["spec"].(map[string]interface{})["valuesSecrets"] = []interface{}{
			map[string]interface{}{"name": "app/params", "keys": []interface{}{"values.yaml"}},
		}
	})
	reconcileAllCharts(t, dyn, flowPolicy)
	if st := readStatusIn(t, dyn, flowNS, flowName); st.phase != hcPhaseFailed || !strings.Contains(st.message, `"app/params" is not a Secret name`) {
		t.Errorf("status = %+v, want Failed naming the invalid reference", st)
	}
}

// The CRD refuses a reference that is not a Secret name or key at apply time.
func TestHelmChartCRDRestrictsValuesSecretsReferences(t *testing.T) {
	t.Parallel()
	item, _ := nested(helmChartCRDSpecSchema(t), "properties", "valuesSecrets", "items", "properties").(map[string]interface{})
	namePattern := str(nested(item, "name", "pattern"), "")
	keyPattern := str(nested(item, "keys", "items", "pattern"), "")
	if namePattern == "" || keyPattern == "" {
		t.Fatalf("valuesSecrets name pattern %q, key pattern %q; want both declared", namePattern, keyPattern)
	}
	name, key := regexp.MustCompile(namePattern), regexp.MustCompile(keyPattern)
	for s, want := range map[string]bool{"app-params": true, "grafana.params": true, "app/params": false, "app%2Fparams": false, "App": false, "": false} {
		if name.MatchString(s) != want {
			t.Errorf("name pattern on %q = %t, want %t", s, !want, want)
		}
	}
	for s, want := range map[string]bool{"values.yaml": true, "a_b-c.d": true, "a/b": false, "": false} {
		if key.MatchString(s) != want {
			t.Errorf("key pattern on %q = %t, want %t", s, !want, want)
		}
	}
}

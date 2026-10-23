package dashboard

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const secretTableFixture = `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
  "columnDefinitions":[{"name":"Name"},{"name":"Type"},{"name":"Data"},{"name":"Age"}],
  "rows":[
    {"cells":["db","Opaque",2,"3d"],
     "object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1","metadata":{
       "name":"db","namespace":"app","creationTimestamp":"2026-10-01T00:00:00Z",
       "labels":{"tier":"data"},
       "annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"password\":\"aHVudGVyMg==\"}}"}}}},
    {"cells":["sh.helm.release.v1.web.v3","helm.sh/release.v1",1,"1d"],
     "object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1","metadata":{
       "name":"sh.helm.release.v1.web.v3","namespace":"default","creationTimestamp":"2026-10-02T00:00:00Z",
       "labels":{"name":"web","owner":"helm","status":"deployed","version":"3"}}}}
  ]}`

func TestSecretSummariesListAsTheUserAndReturnNoValues(t *testing.T) {
	h := newHarness(t, false)
	h.reply = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(secretTableFixture))
	}
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}

	resp := h.do(t, http.MethodGet, secretSummariesPath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: got %d (%s)", secretSummariesPath, resp.StatusCode, bodyOf(t, resp))
	}
	body := bodyOf(t, resp)

	seen := h.last(t)
	if seen.Method != http.MethodGet || seen.Path != "/api/v1/secrets" {
		t.Errorf("apiserver saw %s %s, want a cluster-wide Secret list", seen.Method, seen.Path)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer "+goodToken {
		t.Errorf("Authorization = %q, want the session's own token", got)
	}
	if got := seen.Header.Get("Accept"); got != tableAccept {
		t.Errorf("Accept = %q, want %q", got, tableAccept)
	}
	if seen.Query != "includeObject=Metadata" {
		t.Errorf("query = %q, want includeObject=Metadata", seen.Query)
	}

	for _, leak := range []string{"aHVudGVyMg==", "last-applied-configuration", "tier", "owner"} {
		if strings.Contains(body, leak) {
			t.Errorf("response carries %q; only the summary fields may reach the browser:\n%s", leak, body)
		}
	}

	var out struct {
		Items []secretSummary `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(out.Items))
	}
	db, helm := out.Items[0], out.Items[1]
	if db.Name != "db" || db.Namespace != "app" || db.Type != "Opaque" || db.Keys != 2 || db.Helm != nil {
		t.Errorf("db = %+v", db)
	}
	if db.CreationTimestamp.IsZero() {
		t.Error("creationTimestamp must survive; the SPA shows each Secret's age")
	}
	want := &helmRelease{Name: "web", Version: "3", Status: "deployed"}
	if helm.Type != helmReleaseType || helm.Keys != 1 || !reflect.DeepEqual(helm.Helm, want) {
		t.Errorf("helm = %+v (helm %+v), want release %+v", helm, helm.Helm, want)
	}
}

func TestSecretSummariesPassTheApiserversRefusalThrough(t *testing.T) {
	h := newHarness(t, false)
	h.reply = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403,
		  "message":"secrets is forbidden: User \"viewer\" cannot list resource \"secrets\" at the cluster scope"}`))
	}
	if got := h.login(t, goodToken).StatusCode; got != http.StatusOK {
		t.Fatalf("login: got %d", got)
	}

	resp := h.do(t, http.MethodGet, secretSummariesPath, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %d, want the apiserver's 403 (%s)", resp.StatusCode, bodyOf(t, resp))
	}
}

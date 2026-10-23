package operator

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ociChartRegistry serves one Helm chart the way an OCI registry does, over
// TLS: the manifest by tag and by digest, and its config and chart layer as
// blobs, under repository (e.g. "charts/widget").
func ociChartRegistry(t *testing.T, repository, name, version string, tgz []byte) *httptest.Server {
	t.Helper()
	digest := func(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }
	config := []byte(fmt.Sprintf(`{"apiVersion":"v2","name":%q,"version":%q}`, name, version))
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"mediaType": "application/vnd.cncf.helm.config.v1+json", "digest": digest(config), "size": len(config)},
		"layers": []any{
			map[string]any{"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip", "digest": digest(tgz), "size": len(tgz)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(w http.ResponseWriter, r *http.Request, mediaType string, body []byte) {
		w.Header().Set("Content-Type", mediaType)
		w.Header().Set("Docker-Content-Digest", digest(body))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}
	blobs := map[string][]byte{digest(config): config, digest(tgz): tgz}
	prefix := "/v2/" + repository + "/"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case p == "/v2/":
			w.WriteHeader(http.StatusOK)
		case p == prefix+"manifests/"+version || p == prefix+"manifests/"+digest(manifest):
			serve(w, r, "application/vnd.oci.image.manifest.v1+json", manifest)
		case strings.HasPrefix(p, prefix+"blobs/") && blobs[strings.TrimPrefix(p, prefix+"blobs/")] != nil:
			serve(w, r, "application/octet-stream", blobs[strings.TrimPrefix(p, prefix+"blobs/")])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestLoadChartFromAnOCIRegistry: every oci:// chart failed with "missing
// registry client", because the chart lookup was handed none. A chart is
// pulled from a TLS registry, verified either by repoCA or skipped with
// insecureSkipTLSVerify, and loads.
func TestLoadChartFromAnOCIRegistry(t *testing.T) {
	srv := ociChartRegistry(t, "charts/widget", "widget", "1.2.3", makeChartTGZ(t, "widget", "1.2.3"))
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	for _, tc := range []struct {
		name      string
		repoCA    string
		insecure  bool
		untrusted bool
	}{
		{"repoCA", ca, false, false},
		{"insecureSkipTLSVerify", "", true, false},
		{"an untrusted registry is refused", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectHelmCacheDir(t)
			spec := helmSpec{
				repo:                  "oci://" + strings.TrimPrefix(srv.URL, "https://") + "/charts",
				chart:                 "widget",
				version:               "1.2.3",
				repoCA:                tc.repoCA,
				insecureSkipTLSVerify: tc.insecure,
				timeout:               30 * time.Second,
			}
			chrt, err := loadChart(t.Context(), spec, "ns", "widget")
			if tc.untrusted {
				var certErr x509.UnknownAuthorityError
				if !errors.As(err, &certErr) {
					t.Fatalf("loadChart(%s) = %v, want the registry's self-signed certificate refused", spec.repo, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadChart(%s): %v", spec.repo, err)
			}
			if chrt.Metadata.Name != "widget" || chrt.Metadata.Version != "1.2.3" {
				t.Errorf("loaded %s %s, want widget 1.2.3", chrt.Metadata.Name, chrt.Metadata.Version)
			}
		})
	}
}

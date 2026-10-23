package operator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/version"
	"helm.sh/helm/v3/pkg/cli"
)

// TestRepoURLEntry pins how a spec.repo URL matches a repositories.yaml
// entry: one repository, not merely a similar one. Loose matching would send
// an administrator entry's credentials to an unrelated host or path.
func TestRepoURLEntry(t *testing.T) {
	const (
		chartsRoot = "https://h/charts"
		chartsTop  = "https://h:443"
	)
	for _, tc := range []struct {
		name    string
		repoURL string
		urls    []string
		want    string // entry URL on match, "" on nil, unreadable on error
		wantErr bool
	}{
		{name: "exact", repoURL: chartsRoot, urls: []string{chartsRoot}, want: chartsRoot},
		{name: "trailing slash", repoURL: chartsRoot, urls: []string{chartsRoot + "/"}, want: chartsRoot + "/"},
		{name: "upper-case scheme and host", repoURL: "HTTPS://H/charts", urls: []string{chartsRoot}, want: chartsRoot},
		{name: "path case differs", repoURL: chartsRoot, urls: []string{"https://h/Charts"}, want: ""},
		{name: "different port", repoURL: "https://h:8443/charts", urls: []string{"https://h:8444/charts"}, want: ""},
		{name: "default port not folded", repoURL: "https://h", urls: []string{"https://h:443"}, want: ""},
		{name: "empty path equals root", repoURL: chartsTop, urls: []string{chartsTop + "/"}, want: chartsTop + "/"},
		{name: "userinfo ignored", repoURL: "https://h/charts", urls: []string{"https://reader:secret@h/charts"}, want: "https://reader:secret@h/charts"},
		{name: "query compared exactly", repoURL: "https://h/charts", urls: []string{"https://h/charts?dry=list"}, want: ""},
		{name: "two matching entries take the first", repoURL: chartsRoot + "/a", urls: []string{chartsRoot + "/a", chartsRoot + "/a/"}, want: chartsRoot + "/a"},
		{name: "empty path segment is not cleaned", repoURL: "https://h/team//charts", urls: []string{"https://h/team/charts"}, want: ""},
		{name: "escaped dot segment is not cleaned", repoURL: "https://h/tenant/%2e%2e/charts/", urls: []string{chartsRoot}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := &cli.EnvSettings{RepositoryConfig: filepath.Join(t.TempDir(), "repositories.yaml")}
			config := "repositories: []\n"
			for i, url := range tc.urls {
				config += fmt.Sprintf("- name: entry-%d\n  url: %s\n  username: reader\n  password: secret\n", i, url)
			}
			if len(tc.urls) > 0 {
				config = strings.Replace(config, "repositories: []\n", "repositories:\n", 1)
			}
			if err := os.WriteFile(settings.RepositoryConfig, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			entry, err := repoURLEntry(tc.repoURL, settings)
			if err != nil {
				t.Fatalf("repoURLEntry(%q): %v", tc.repoURL, err)
			}
			if entry == nil {
				if tc.want != "" {
					t.Fatalf("repoURLEntry(%q) = nil, want entry %s", tc.repoURL, tc.want)
				}
				return
			}
			if tc.want == "" || entry.URL != tc.want {
				t.Fatalf("repoURLEntry(%q) = %s, want %s", tc.repoURL, entry.URL, tc.want)
			}
		})
	}
	t.Run("missing repositories.yaml", func(t *testing.T) {
		settings := &cli.EnvSettings{RepositoryConfig: filepath.Join(t.TempDir(), "repositories.yaml")}
		entry, err := repoURLEntry("https://h/charts", settings)
		if entry != nil || err != nil {
			t.Fatalf("repoURLEntry without repositories.yaml = %v, %v; want nil, nil", entry, err)
		}
	})
	t.Run("corrupt repositories.yaml", func(t *testing.T) {
		settings := &cli.EnvSettings{RepositoryConfig: filepath.Join(t.TempDir(), "repositories.yaml")}
		if err := os.WriteFile(settings.RepositoryConfig, []byte("repositories: [unclosed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		entry, err := repoURLEntry("https://h/charts", settings)
		if err == nil || entry != nil {
			t.Fatalf("repoURLEntry against a corrupt file = %v, %v; want an error", entry, err)
		}
	})
}

// TestLoadChartRepoURLPathIsComparedVerbatim pins the credential binding
// against a spec.repo path that only path cleaning would equate to the
// entry's: Helm fetches the spec.repo path as written, so a URL naming a
// different path on the server is a different repository and must be fetched
// anonymously, not with the entry's credentials.
func TestLoadChartRepoURLPathIsComparedVerbatim(t *testing.T) {
	for _, tc := range []struct {
		repoPath  string
		entryPath string // the path only path.Clean would equate to repoPath
	}{
		{repoPath: "/team//charts", entryPath: "/team/charts"},
		{repoPath: "/tenant/%2e%2e/charts/", entryPath: "/charts"},
	} {
		t.Run(tc.repoPath, func(t *testing.T) {
			redirectHelmCacheDir(t)
			tgz := makeChartTGZ(t, "widget", "1.0.0")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("entry credentials reached a repository whose URL names a different path")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/index.yaml") {
					fmt.Fprint(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [widget.tgz]\n")
					return
				}
				_, _ = w.Write(tgz)
			}))
			t.Cleanup(srv.Close)
			config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  username: reader\n  password: test-password\n", srv.URL+tc.entryPath)
			if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			chart, err := loadChart(t.Context(), helmSpec{chart: "widget", repo: srv.URL + tc.repoPath, version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
			if err != nil {
				t.Fatalf("the differently-named repository must fetch anonymously: %v", err)
			}
			if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
				t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
			}
		})
	}
}

// TestLoadChartArchiveCredentialOriginSpelling pins how the entry's
// credentials are bound when the shared origin is spelled differently in the
// archive's URL: Helm's HTTP getter compares scheme and host verbatim, so
// binding the entry's lowercased URL withholds the credentials from a
// same-origin archive whose host is upper-case in spec.repo or in the index —
// while a different host spelling stays unauthenticated even when it resolves
// to the same server.
func TestLoadChartArchiveCredentialOriginSpelling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		indexURL string // archive URL the cached index lists, relative or absolute
		specRepo func(srv string) string
		wantAuth bool
	}{
		{name: "upper-case spec.repo, relative archive", indexURL: "widget.tgz", specRepo: func(srv string) string { return upperHost(srv) }, wantAuth: true},
		{name: "upper-case absolute archive host", indexURL: "", specRepo: func(srv string) string { return upperHost(srv) }, wantAuth: true},
		{name: "entry host differs verbatim", indexURL: "", specRepo: func(srv string) string { return srv }, wantAuth: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectHelmCacheDir(t)
			tgz := makeChartTGZ(t, "widget", "1.0.0")
			var index string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/index.yaml") {
					fmt.Fprint(w, index)
					return
				}
				if tc.wantAuth {
					user, password, ok := r.BasicAuth()
					if !ok || user != "reader" || password != "test-password" {
						t.Error("same-origin archive lost its credentials to a host-spelling mismatch")
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
				} else if r.Header.Get("Authorization") != "" {
					t.Error("entry credentials reached an archive whose host spelling differs from the entry's")
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				_, _ = w.Write(tgz)
			}))
			t.Cleanup(srv.Close)
			repoURL := tc.specRepo(srv.URL)
			indexURL := tc.indexURL
			if indexURL == "" {
				indexURL = upperHost(srv.URL) + "/widget.tgz"
			}
			index = fmt.Sprintf("apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%s]\n", indexURL)
			config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  username: reader\n  password: test-password\n", repoURL)
			if err := os.MkdirAll(filepath.Join(version.HelmCacheDir, "cache"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "cache", "private-index.yaml"), []byte(index), 0o600); err != nil {
				t.Fatal(err)
			}
			chart, err := loadChart(t.Context(), helmSpec{chart: "widget", repo: repoURL, version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
			if err != nil {
				t.Fatalf("the archive fetch failed: %v", err)
			}
			if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
				t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
			}
		})
	}
}

// upperHost spells a server URL's loopback host the way an administrator's
// repository configuration might record it: DNS names are case-insensitive,
// so the same server is reachable under either spelling.
func upperHost(srv string) string {
	return strings.Replace(srv, "127.0.0.1", "LOCALHOST", 1)
}

func TestLoadChartDirectURLSkipsUnreadableRepositoryIndexes(t *testing.T) {
	for _, broken := range []string{"missing", "corrupt"} {
		for _, owned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/registered-owner=%t", broken, owned), func(t *testing.T) {
				redirectHelmCacheDir(t)
				tgz := makeChartTGZ(t, "widget", "1.0.0")
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					user, password, _ := r.BasicAuth()
					if owned && (user != "reader" || password != "test-password") {
						t.Error("the matching readable repository lost its credentials")
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					if !owned && r.Header.Get("Authorization") != "" {
						t.Error("unrelated repository credentials were forwarded")
					}
					_, _ = w.Write(tgz)
				}))
				t.Cleanup(srv.Close)
				cache := filepath.Join(version.HelmCacheDir, "cache")
				if err := os.MkdirAll(cache, 0o700); err != nil {
					t.Fatal(err)
				}
				config := "repositories:\n- name: unrelated\n  url: https://unrelated.invalid\n  username: unrelated\n  password: unrelated-password\n"
				if broken == "corrupt" {
					if err := os.WriteFile(filepath.Join(cache, "unrelated-index.yaml"), []byte("entries: [unclosed\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if owned {
					config += fmt.Sprintf("- name: private\n  url: %s\n  username: reader\n  password: test-password\n", srv.URL)
					index := fmt.Sprintf("apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%s/widget.tgz]\n", srv.URL)
					if err := os.WriteFile(filepath.Join(cache, "private-index.yaml"), []byte(index), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				chart, err := loadChart(t.Context(), helmSpec{chart: srv.URL + "/widget.tgz", timeout: 3 * time.Second}, "ns", "widget")
				if err != nil {
					t.Fatal(err)
				}
				if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" || requests.Load() == 0 {
					t.Fatalf("direct archive was not loaded from its working URL: %+v", chart.Metadata)
				}
			})
		}
	}
}

// TestLoadChartRepoURLEntryTLS pins the TLS half of a spec.repo entry: the
// repositories.yaml entry naming the repository carries the CA, the mTLS
// key pair and the credentials, and the fetch must present them — the
// server demands a client certificate and authentication on every request,
// and its self-signed certificate can only be trusted through the entry's
// caFile, since spec.repoCA is empty.
func TestLoadChartRepoURLEntryTLS(t *testing.T) {
	redirectHelmCacheDir(t)
	tgz := makeChartTGZ(t, "widget", "1.0.0")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "reader" || password != "test-password" || len(r.TLS.PeerCertificates) == 0 {
			t.Error("spec.repo request lost basic authentication or its client certificate")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/index.yaml") {
			fmt.Fprint(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [widget.tgz]\n")
			return
		}
		_, _ = w.Write(tgz)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	caPath := filepath.Join(version.HelmCacheDir, "ca.pem")
	keyPath := filepath.Join(version.HelmCacheDir, "key.pem")
	if err := os.WriteFile(caPath, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(srv.TLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  username: reader\n  password: test-password\n  caFile: %s\n  certFile: %s\n  keyFile: %s\n", srv.URL, caPath, caPath, keyPath)
	if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	chart, err := loadChart(t.Context(), helmSpec{chart: "widget", repo: srv.URL, version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
	if err != nil {
		t.Fatalf("spec.repo fetch must use the matching entry's TLS files and credentials: %v", err)
	}
	if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
		t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
	}
}

// TestLoadChartEntryClientCertStaysOnItsOrigin pins that the entry's client
// certificate follows the same origin rule as its basic auth: an archive the
// index puts on another host, or a redirect there, is fetched without the
// certificate unless the entry sets pass_credentials_all. Both servers share
// httptest's certificate, so the entry's caFile trusts both and only the
// client certificate differs.
func TestLoadChartEntryClientCertStaysOnItsOrigin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		redirect bool // the repository redirects its own archive URL to the other host
		passAll  bool
		wantCert bool
	}{
		{name: "archive on another host", wantCert: false},
		{name: "redirect to another host", redirect: true, wantCert: false},
		{name: "archive on another host with pass_credentials_all", passAll: true, wantCert: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			redirectHelmCacheDir(t)
			tgz := makeChartTGZ(t, "widget", "1.0.0")
			var otherHits, otherCerts atomic.Int32
			other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				otherHits.Add(1)
				otherCerts.Add(int32(len(r.TLS.PeerCertificates)))
				_, _ = w.Write(tgz)
			}))
			// Ask for a certificate without requiring one, so a presented
			// certificate is recorded rather than refused.
			other.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
			other.StartTLS()
			t.Cleanup(other.Close)
			archive := other.URL + "/widget.tgz"
			repoSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if len(r.TLS.PeerCertificates) == 0 {
					http.Error(w, "client certificate required", http.StatusUnauthorized)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/index.yaml") {
					ref := archive
					if tc.redirect {
						ref = "widget.tgz"
					}
					fmt.Fprintf(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%q]\n", ref)
					return
				}
				http.Redirect(w, r, archive, http.StatusFound)
			}))
			repoSrv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
			repoSrv.StartTLS()
			t.Cleanup(repoSrv.Close)

			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: repoSrv.Certificate().Raw})
			caPath := filepath.Join(version.HelmCacheDir, "ca.pem")
			keyPath := filepath.Join(version.HelmCacheDir, "key.pem")
			if err := os.WriteFile(caPath, ca, 0o600); err != nil {
				t.Fatal(err)
			}
			key, err := x509.MarshalPKCS8PrivateKey(repoSrv.TLS.Certificates[0].PrivateKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
				t.Fatal(err)
			}
			config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  caFile: %s\n  certFile: %s\n  keyFile: %s\n  pass_credentials_all: %v\n",
				repoSrv.URL, caPath, caPath, keyPath, tc.passAll)
			if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadChart(t.Context(), helmSpec{chart: "widget", repo: repoSrv.URL, version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget"); err != nil {
				t.Fatalf("chart fetch: %v", err)
			}
			if otherHits.Load() == 0 {
				t.Fatal("the archive was never fetched from the other host")
			}
			if got := otherCerts.Load() > 0; got != tc.wantCert {
				t.Errorf("other host was shown the entry's client certificate = %v, want %v", got, tc.wantCert)
			}
		})
	}
}

// TestLoadChartRepoURLProtectedIndex pins the credentials half of a spec.repo
// entry: the fetch of an index-requiring server must authenticate both the
// index.yaml and the archive with the matching entry's credentials, even
// though the entry's URL carries the trailing slash the spec.repo leaves off.
func TestLoadChartRepoURLProtectedIndex(t *testing.T) {
	redirectHelmCacheDir(t)
	tgz := makeChartTGZ(t, "widget", "1.0.0")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		user, password, _ := r.BasicAuth()
		if user != "reader" || password != "secret" {
			t.Errorf("%s went out unauthenticated", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/index.yaml") {
			fmt.Fprint(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [widget.tgz]\n")
			return
		}
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	config := fmt.Sprintf("repositories:\n- name: private\n  url: %s/charts/\n  username: reader\n  password: secret\n", srv.URL)
	if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	chart, err := loadChart(t.Context(), helmSpec{chart: "widget", repo: srv.URL + "/charts", version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
	if err != nil {
		t.Fatalf("spec.repo fetch must authenticate against the entry matching its URL: %v", err)
	}
	if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
		t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
	}
	// The index and the archive were both fetched, both authenticated.
	if requests.Load() < 2 {
		t.Fatalf("the repository saw %d requests, want the index and the archive", requests.Load())
	}
}

// TestLoadChartRepoURLEntryMixedCaseHost pins the credential binding across a
// case-only host difference: entry matching folds the scheme and host to
// lower case, so an entry recorded as http://LOCALHOST:<port>/charts/ matches
// a lower-case spec.repo — and the archive fetch must fold the entry's URL
// the same way, because Helm's HTTP getter origin check compares hosts
// verbatim and would otherwise withhold the credentials there.
func TestLoadChartRepoURLEntryMixedCaseHost(t *testing.T) {
	redirectHelmCacheDir(t)
	tgz := makeChartTGZ(t, "widget", "1.0.0")
	var index, archive atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "reader" || password != "secret" {
			t.Errorf("%s went out unauthenticated", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/index.yaml") {
			index.Add(1)
			fmt.Fprint(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [widget.tgz]\n")
			return
		}
		archive.Add(1)
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	// The entry spells the host in upper case; spec.repo is lower case. Entry
	// matching must still find it, and the archive download must still
	// authenticate with the entry's credentials.
	config := fmt.Sprintf("repositories:\n- name: private\n  url: http://LOCALHOST:%s/charts/\n  username: reader\n  password: secret\n", port)
	if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	chart, err := loadChart(t.Context(), helmSpec{chart: "widget", repo: "http://localhost:" + port + "/charts", version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
	if err != nil {
		t.Fatalf("spec.repo fetch must authenticate with the mixed-case-host entry on index and archive: %v", err)
	}
	if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
		t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
	}
	if index.Load() == 0 || archive.Load() == 0 {
		t.Fatalf("the repository saw index=%d archive=%d authenticated requests, want at least one of each", index.Load(), archive.Load())
	}
}

// TestLoadChartRepoURLUncachedRegisteredRepo pins the anonymous path: a
// spec.repo with no matching entry fetches and downloads publicly, without
// forwarding some unrelated entry's credentials — and without failing on the
// unrelated entry's missing cached index.
func TestLoadChartRepoURLUncachedRegisteredRepo(t *testing.T) {
	redirectHelmCacheDir(t)
	tgz := makeChartTGZ(t, "widget", "1.0.0")
	var archives atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials leaked to a repository no entry matches")
		}
		if strings.HasSuffix(r.URL.Path, "/index.yaml") {
			fmt.Fprint(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [widget.tgz]\n")
			return
		}
		archives.Add(1)
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	// The unrelated entry carries credentials and no cached index anywhere.
	config := "repositories:\n- name: unrelated\n  url: https://unrelated.invalid\n  username: reader\n  password: secret\n"
	if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	chart, err := loadChart(t.Context(), helmSpec{chart: "widget", repo: srv.URL, version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
	if err != nil {
		t.Fatalf("a public spec.repo must load without any cached index: %v", err)
	}
	if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
		t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
	}
	if got := archives.Load(); got != 1 {
		t.Fatalf("the archive was fetched %d times, want once", got)
	}
}

// TestLoadChartRepoURLCrossHostArchive pins where an entry's credentials may
// go when the repository's index points at another host: nowhere, unless the
// entry itself declares pass_credentials_all — the getter's origin policy,
// not the operator's own forwarding, decides.
func TestLoadChartRepoURLCrossHostArchive(t *testing.T) {
	for _, forward := range []bool{false, true} {
		t.Run(fmt.Sprintf("pass-credentials-all=%t", forward), func(t *testing.T) {
			redirectHelmCacheDir(t)
			tgz := makeChartTGZ(t, "widget", "1.0.0")
			archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if forward {
					if !ok || user != "reader" || password != "secret" {
						t.Error("explicit cross-origin credential forwarding was lost")
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
				} else if r.Header.Get("Authorization") != "" {
					t.Error("repository credentials leaked to another origin")
				}
				_, _ = w.Write(tgz)
			}))
			t.Cleanup(archive.Close)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, _ := r.BasicAuth()
				if user != "reader" || password != "secret" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				fmt.Fprintf(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%s/widget.tgz]\n", archive.URL)
			}))
			t.Cleanup(origin.Close)
			config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  username: reader\n  password: secret\n  pass_credentials_all: %t\n", origin.URL, forward)
			if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			chart, err := loadChart(t.Context(), helmSpec{chart: "widget", repo: origin.URL, version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
			if err != nil {
				t.Fatalf("cross-host archive with pass_credentials_all=%t = %v", forward, err)
			}
			if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
				t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
			}
		})
	}
}

// TestLoadChartRepoURLOwnerScanFallback pins the fallback for an archive
// fetched for a spec.repo that matched no entry: the credentials still come
// from the registered repository whose cached index lists the archive — even
// when an unrelated registered entry has no cached index at all, which must
// not end the scan.
func TestLoadChartRepoURLOwnerScanFallback(t *testing.T) {
	redirectHelmCacheDir(t)
	tgz := makeChartTGZ(t, "widget", "1.0.0")
	archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		if user != "reader" || password != "secret" {
			t.Error("the owning repository's credentials did not reach the archive")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(archive.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%s/widget.tgz]\n", archive.URL)
	}))
	t.Cleanup(origin.Close)
	cache := filepath.Join(version.HelmCacheDir, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	// The owner entry comes after one with no cached index: the scan must
	// skip that one, not stop at it.
	config := fmt.Sprintf("repositories:\n- name: unrelated\n  url: https://unrelated.invalid\n  username: nobody\n  password: nobody\n- name: private\n  url: %s\n  username: reader\n  password: secret\n", archive.URL)
	if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	index := fmt.Sprintf("apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%s/widget.tgz]\n", archive.URL)
	if err := os.WriteFile(filepath.Join(cache, "private-index.yaml"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	chart, err := loadChart(t.Context(), helmSpec{chart: "widget", repo: origin.URL, version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
	if err != nil {
		t.Fatalf("an archive whose owner is registered must be fetched with the owner's credentials even though spec.repo matched no entry: %v", err)
	}
	if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
		t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
	}
}

func TestLoadChartNamedRepositoryInsecureTLS(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		t.Run(fmt.Sprintf("insecure=%t", insecure), func(t *testing.T) {
			redirectHelmCacheDir(t)
			tgz := makeChartTGZ(t, "widget", "1.0.0")
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(tgz)
			}))
			t.Cleanup(srv.Close)
			config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  insecure_skip_tls_verify: %t\n", srv.URL, insecure)
			if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			cache := filepath.Join(version.HelmCacheDir, "cache")
			if err := os.MkdirAll(cache, 0o700); err != nil {
				t.Fatal(err)
			}
			index := "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [widget.tgz]\n"
			if err := os.WriteFile(filepath.Join(cache, "private-index.yaml"), []byte(index), 0o600); err != nil {
				t.Fatal(err)
			}
			chart, err := loadChart(t.Context(), helmSpec{chart: "private/widget", version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
			if !insecure {
				var certErr x509.UnknownAuthorityError
				if !errors.As(err, &certErr) {
					t.Fatalf("default repository TLS must reject the self-signed certificate: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
				t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
			}
		})
	}
}

func TestLoadChartDirectURLRepositoryCredentialOrigin(t *testing.T) {
	for _, forward := range []bool{false, true} {
		t.Run(fmt.Sprintf("pass-credentials-all=%t", forward), func(t *testing.T) {
			redirectHelmCacheDir(t)
			tgz := makeChartTGZ(t, "widget", "1.0.0")
			var requests atomic.Int32
			archive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				user, password, ok := r.BasicAuth()
				if forward && (!ok || user != "reader" || password != "test-password") {
					t.Error("explicit cross-origin credential forwarding was lost")
				}
				if !forward && r.Header.Get("Authorization") != "" {
					t.Error("repository credentials leaked to another origin")
				}
				_, _ = w.Write(tgz)
			}))
			t.Cleanup(archive.Close)
			origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Error("direct archive download unexpectedly fetched the repository index")
			}))
			t.Cleanup(origin.Close)
			config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  username: reader\n  password: test-password\n  pass_credentials_all: %t\n", origin.URL, forward)
			if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			cache := filepath.Join(version.HelmCacheDir, "cache")
			if err := os.MkdirAll(cache, 0o700); err != nil {
				t.Fatal(err)
			}
			index := fmt.Sprintf("apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%s/widget.tgz]\n", archive.URL)
			if err := os.WriteFile(filepath.Join(cache, "private-index.yaml"), []byte(index), 0o600); err != nil {
				t.Fatal(err)
			}
			chart, err := loadChart(t.Context(), helmSpec{chart: archive.URL + "/widget.tgz", timeout: 3 * time.Second}, "ns", "widget")
			if err != nil {
				t.Fatal(err)
			}
			if chart.Metadata.Name != "widget" || requests.Load() == 0 {
				t.Fatal("the cross-origin chart archive was not fetched")
			}
		})
	}
}

// Observe cancellation at the remote endpoint: returning to the reconciler
// while a detached SDK request continues does not stop the chart fetch.
func TestLoadChartCancelsRepositoryRequests(t *testing.T) {
	for _, stage := range []string{"index", "archive", "direct archive", "oci"} {
		for _, stop := range []string{"timeout", "leadership loss"} {
			t.Run(stage+"/"+stop, func(t *testing.T) {
				redirectHelmCacheDir(t)
				started := make(chan struct{}, 1)
				stopped := make(chan struct{}, 1)
				unblock := make(chan struct{})
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stage == "archive" && strings.HasSuffix(r.URL.Path, "/index.yaml") {
						fmt.Fprint(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [chart.tgz]\n")
						return
					}
					select {
					case started <- struct{}{}:
					default:
					}
					select {
					case <-r.Context().Done():
						select {
						case stopped <- struct{}{}:
						default:
						}
					case <-unblock:
					}
				}))
				t.Cleanup(func() {
					close(unblock)
					srv.Close()
				})
				spec := helmSpec{chart: "widget", repo: srv.URL, version: "1.0.0", timeout: 30 * time.Second, insecureSkipTLSVerify: true}
				if stage == "direct archive" {
					spec.chart, spec.repo = srv.URL+"/chart.tgz", ""
				}
				if stage == "oci" {
					spec.repo = "oci://" + strings.TrimPrefix(srv.URL, "https://") + "/charts"
				}
				if stop == "timeout" {
					spec.timeout = 200 * time.Millisecond
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := loadChart(ctx, spec, "ns", "widget")
					done <- err
				}()
				select {
				case <-started:
				case err := <-done:
					t.Fatalf("fetch returned before reaching the repository: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("fetch never reached the repository")
				}
				want := context.DeadlineExceeded
				if stop == "leadership loss" {
					want = context.Canceled
					cancel()
				}
				select {
				case err := <-done:
					if !errors.Is(err, want) {
						t.Errorf("fetch error = %v, want %v", err, want)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("fetch did not return after its context ended")
				}
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("fetch returned while its repository request was still running")
				}
			})
		}
	}
}

func TestLoadChartRepositoryTLSAndCredentials(t *testing.T) {
	for _, source := range []string{"local archive", "direct URL", "repository CA", "named repository credentials", "direct URL credentials"} {
		t.Run(source, func(t *testing.T) {
			redirectHelmCacheDir(t)
			tgz := makeChartTGZ(t, "widget", "1.0.0")
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(source, "credentials") {
					user, password, ok := r.BasicAuth()
					if !ok || user != "reader" || password != "test-password" || len(r.TLS.PeerCertificates) == 0 {
						t.Error("named repository request lost basic authentication or its client certificate")
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
				}
				if strings.HasSuffix(r.URL.Path, "/index.yaml") {
					fmt.Fprint(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [widget.tgz]\n")
					return
				}
				_, _ = w.Write(tgz)
			}))
			if strings.Contains(source, "credentials") {
				srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
			}
			srv.StartTLS()
			t.Cleanup(srv.Close)
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
			spec := helmSpec{chart: "widget", version: "1.0.0", timeout: 3 * time.Second, repoCA: string(ca)}
			switch source {
			case "local archive":
				spec.chart = filepath.Join(version.HelmCacheDir, "widget.tgz")
				if err := os.WriteFile(spec.chart, tgz, 0o600); err != nil {
					t.Fatal(err)
				}
			case "direct URL":
				spec.chart = srv.URL + "/widget.tgz"
			case "repository CA":
				spec.repo = srv.URL
			case "named repository credentials", "direct URL credentials":
				// A cached named repository carries its own CA, mTLS pair,
				// credentials and origin policy, which the downloader supplies
				// through opaque getter options.
				caPath := filepath.Join(version.HelmCacheDir, "ca.pem")
				keyPath := filepath.Join(version.HelmCacheDir, "key.pem")
				if err := os.WriteFile(caPath, ca, 0o600); err != nil {
					t.Fatal(err)
				}
				key, err := x509.MarshalPKCS8PrivateKey(srv.TLS.Certificates[0].PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
					t.Fatal(err)
				}
				config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  username: reader\n  password: test-password\n  caFile: %s\n  certFile: %s\n  keyFile: %s\n", srv.URL, caPath, caPath, keyPath)
				if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
				cache := filepath.Join(version.HelmCacheDir, "cache")
				if err := os.MkdirAll(cache, 0o700); err != nil {
					t.Fatal(err)
				}
				index := fmt.Sprintf("apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%s/widget.tgz]\n", srv.URL)
				if err := os.WriteFile(filepath.Join(cache, "private-index.yaml"), []byte(index), 0o600); err != nil {
					t.Fatal(err)
				}
				spec.chart = "private/widget"
				if source == "direct URL credentials" {
					spec.chart = srv.URL + "/widget.tgz"
				}
				spec.repoCA = "" // the CA must come from repositories.yaml
			}
			chart, err := loadChart(t.Context(), spec, "ns", "widget")
			if err != nil {
				t.Fatal(err)
			}
			if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
				t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
			}
		})
	}
}

func TestLoadChartResolvedRepositoryRetainsArchiveCredentials(t *testing.T) {
	redirectHelmCacheDir(t)
	tgz := makeChartTGZ(t, "widget", "1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.yaml" {
			fmt.Fprint(w, "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [widget.tgz]\n")
			return
		}
		u, p, _ := r.BasicAuth()
		if u != "reader" || p != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	if err := os.MkdirAll(filepath.Join(version.HelmCacheDir, "cache"), 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  username: reader\n  password: secret\n", srv.URL)
	if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	index := fmt.Sprintf("apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%s/widget.tgz]\n", srv.URL)
	if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "cache/private-index.yaml"), []byte(index), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadChart(t.Context(), helmSpec{chart: "widget", repo: srv.URL, version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
	if err != nil {
		t.Fatalf("resolved URL must retain matching repository archive credentials: %v", err)
	}
	if loaded.Metadata.Name != "widget" || loaded.Metadata.Version != "1.0.0" {
		t.Fatalf("loaded unexpected chart: %+v", loaded.Metadata)
	}
}

// TestLoadChartMigratesLegacyRepositoryState pins the upgrade carry-over: a
// node brought up by a release that kept the operator's helm cache at the
// legacy /var/lib/stratum-helm carries repositories.yaml (named repositories,
// credentials, TLS file paths) and the cached <repo>-index.yaml files there.
// Nothing in the operator downloads an index, so against the data-dir lookup
// a fetch of chart "private/widget" would fail "repo private not found" or
// "no cached repo found" where it resolved before — dropping the entry's
// credentials and TLS options on the floor. The first loadChart migrates both
// into the data dir, the fetch resolves as before, the legacy files stay
// untouched, and the cache carries only what a lookup reads: no non-index
// file. The legacy files are written 0640 and the migrated copies must carry
// that mode: the copy preserves the source's permission bits rather than
// falling back to a default.
func TestLoadChartMigratesLegacyRepositoryState(t *testing.T) {
	redirectHelmCacheDir(t)
	legacy := t.TempDir()
	prev := legacyHelmCacheDir
	legacyHelmCacheDir = legacy
	t.Cleanup(func() { legacyHelmCacheDir = prev })

	tgz := makeChartTGZ(t, "widget", "1.0.0")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		user, password, _ := r.BasicAuth()
		if user != "reader" || password != "test-password" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)

	config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  username: reader\n  password: test-password\n", srv.URL)
	index := fmt.Sprintf("apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%s/widget.tgz]\n", srv.URL)
	for path, contents := range map[string]string{
		filepath.Join(legacy, "repositories.yaml"):           config,
		filepath.Join(legacy, "cache", "private-index.yaml"): index,
		// Whatever else sits in the legacy cache is not the lookup's to read.
		filepath.Join(legacy, "cache", "notes.txt"): "not an index",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
			t.Fatal(err)
		}
	}

	chart, err := loadChart(t.Context(), helmSpec{chart: "private/widget", version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
	if err != nil {
		t.Fatalf("named-repository fetch with legacy-only state = %v; the migration must carry the configuration and the cached index", err)
	}
	if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" || requests.Load() == 0 {
		t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
	}

	// The migrated files sit in the data-dir home with the credentials and
	// index the fetch consulted, at the mode the legacy file carried.
	for path, want := range map[string]string{
		filepath.Join(version.HelmCacheDir, "repositories.yaml"):           config,
		filepath.Join(version.HelmCacheDir, "cache", "private-index.yaml"): index,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("migrated file %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("migrated %s drifted from the legacy copy", path)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o640 {
			t.Errorf("migrated %s = %o, want the legacy file's 0640 — the copy preserves the source's mode", path, fi.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(version.HelmCacheDir, "cache", "notes.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the migration carried a non-index cache file: %v", err)
	}
	// The legacy state stays: uninstall owns its removal, not the operator.
	kept, err := os.ReadFile(filepath.Join(legacy, "repositories.yaml"))
	if err != nil || string(kept) != config {
		t.Errorf("the legacy repositories.yaml was not left in place: %q, %v", kept, err)
	}
}

// TestLoadChartKeepsAnEstablishedHelmCacheHome pins the migration's refusal:
// once repositories.yaml exists in the data-dir home with content of its own,
// the legacy copy is stale — the lookup reads the established home alone, the
// migration must not overwrite it (a stale private entry must not come back
// to life), and its cached indexes must not land in the established home
// either: an index of the same name would point the lookup at another
// repository's URLs. The legacy cache here carries a private index so the
// no-carry-over assertion can actually fail. The pinned error text is Helm
// v4.3.0's own (pkg/downloader/chart_downloader.go, "repo %s not found") —
// the string is imported, not owned, so a Helm upgrade may reword it.
func TestLoadChartKeepsAnEstablishedHelmCacheHome(t *testing.T) {
	redirectHelmCacheDir(t)
	legacy := t.TempDir()
	prev := legacyHelmCacheDir
	legacyHelmCacheDir = legacy
	t.Cleanup(func() { legacyHelmCacheDir = prev })

	legacyConfig := "repositories:\n- name: private\n  url: https://legacy.invalid\n  username: reader\n  password: legacy-password\n"
	legacyIndex := "apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 9.9.9\n    apiVersion: v2\n    urls: [https://legacy.invalid/widget.tgz]\n"
	if err := os.MkdirAll(filepath.Join(legacy, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		filepath.Join(legacy, "repositories.yaml"):           legacyConfig,
		filepath.Join(legacy, "cache", "private-index.yaml"): legacyIndex,
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte("repositories: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := loadChart(t.Context(), helmSpec{chart: "private/widget", version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
	if err == nil || !strings.Contains(err.Error(), "repo private not found") {
		t.Fatalf("established-home lookup with a private chart = %v, want the established config's \"repo private not found\"", err)
	}
	if got, err := os.ReadFile(filepath.Join(version.HelmCacheDir, "repositories.yaml")); err != nil || string(got) != "repositories: []\n" {
		t.Errorf("the established repositories.yaml was replaced: %q, %v", got, err)
	}
	// loadChart creates the home's cache dir itself; the legacy state must
	// not have landed in it.
	if got, err := os.ReadFile(filepath.Join(version.HelmCacheDir, "cache", "private-index.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the established home took a cached index from the legacy state: %q, %v", got, err)
	}
}

// TestLoadChartResumesAnInterruptedMigration pins the recovery: a migration
// that landed repositories.yaml and was interrupted before the cached indexes
// must not stick. A byte-identical repositories.yaml in the data-dir home is
// this migration's own earlier copy — the config is created exclusively, so
// byte equality is the signature — and the index scan resumes on the next
// reconcile: without that, the migrated configuration names the repository,
// the lookup finds no cached index, and every fetch fails "no cached repo
// found" forever, with no recovery short of an administrator deleting the
// migrated file by hand.
func TestLoadChartResumesAnInterruptedMigration(t *testing.T) {
	redirectHelmCacheDir(t)
	legacy := t.TempDir()
	prev := legacyHelmCacheDir
	legacyHelmCacheDir = legacy
	t.Cleanup(func() { legacyHelmCacheDir = prev })

	tgz := makeChartTGZ(t, "widget", "1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, _ := r.BasicAuth()
		if user != "reader" || password != "test-password" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)

	config := fmt.Sprintf("repositories:\n- name: private\n  url: %s\n  username: reader\n  password: test-password\n", srv.URL)
	index := fmt.Sprintf("apiVersion: v1\nentries:\n  widget:\n  - name: widget\n    version: 1.0.0\n    apiVersion: v2\n    urls: [%s/widget.tgz]\n", srv.URL)
	if err := os.MkdirAll(filepath.Join(legacy, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "repositories.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "cache", "private-index.yaml"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	// What a previous, interrupted migration left: the configuration in the
	// data dir, byte-identical to the legacy copy, no index.
	if err := os.MkdirAll(version.HelmCacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version.HelmCacheDir, "repositories.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	chart, err := loadChart(t.Context(), helmSpec{chart: "private/widget", version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
	if err != nil {
		t.Fatalf("fetch after an interrupted migration = %v; the index scan must resume for the migration's own copy", err)
	}
	if chart.Metadata.Name != "widget" || chart.Metadata.Version != "1.0.0" {
		t.Fatalf("loaded unexpected chart: %+v", chart.Metadata)
	}
	if got, err := os.ReadFile(filepath.Join(version.HelmCacheDir, "cache", "private-index.yaml")); err != nil || string(got) != index {
		t.Errorf("the interrupted migration's index did not resume: %q, %v", got, err)
	}

	// The next reconcile runs the migration again, and the legacy index is
	// gone by now — uninstalled, or left a dangling symlink by a mid-delete
	// run. The migrated cache needs neither read nor copy from it: the
	// destination exists, so the load skips the legacy source however
	// unreadable it has become, instead of spending the fetch's window on a
	// copy the exclusive create then refuses — or failing the fetch on a
	// source it cannot read.
	if err := os.Remove(filepath.Join(legacy, "cache", "private-index.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(legacy, "cache", "gone.yaml"), filepath.Join(legacy, "cache", "private-index.yaml")); err != nil {
		t.Fatal(err)
	}
	chart2, err := loadChart(t.Context(), helmSpec{chart: "private/widget", version: "1.0.0", timeout: 3 * time.Second}, "ns", "widget")
	if err != nil {
		t.Fatalf("a second reconcile over a migrated cache = %v; the migration must not read a legacy index the destination already covers", err)
	}
	if chart2.Metadata.Name != "widget" || chart2.Metadata.Version != "1.0.0" {
		t.Fatalf("loaded unexpected chart: %+v", chart2.Metadata)
	}
	got2, err := os.ReadFile(filepath.Join(version.HelmCacheDir, "cache", "private-index.yaml"))
	if err != nil || string(got2) != index {
		t.Errorf("the migrated index was disturbed by the second reconcile: %q, %v", got2, err)
	}
	// Nothing was staged into the migrated cache either: it holds exactly
	// what the first load migrated.
	entries, err := os.ReadDir(filepath.Join(version.HelmCacheDir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	if want := []string{"private-index.yaml"}; !slices.Equal(names, want) {
		t.Errorf("the migrated cache holds %v, want exactly %v — the second load staged a copy the destination check should have skipped", names, want)
	}
}

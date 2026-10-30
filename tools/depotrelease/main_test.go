package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func testAssets(t *testing.T, arches ...string) string {
	t.Helper()
	directory := t.TempDir()
	for _, arch := range arches {
		digest := sha256.Sum256([]byte("binary-" + arch))
		files := map[string]string{
			"stratum-linux-" + arch:                    "binary-" + arch,
			"stratum-linux-" + arch + ".sig":           "signature-" + arch,
			"stratum-linux-" + arch + ".pem":           "certificate-" + arch,
			"stratum-linux-" + arch + ".spdx.json":     `{"spdxVersion":"SPDX-2.3"}`,
			"stratum-linux-" + arch + ".spdx.json.sig": "sbom-signature-" + arch,
			"stratum-linux-" + arch + ".spdx.json.pem": "sbom-certificate-" + arch,
			"build-info-" + arch + ".json":             fmt.Sprintf(`{"target":{"os":"linux","arch":%q},"source":{"commit":"0123456789012345678901234567890123456789"},"components":{"kubernetes":{"version":"1.37.1"}},"artifacts":{"stratum":{"size":%d,"sha256":%q}}}`, arch, len("binary-"+arch), hex.EncodeToString(digest[:])),
		}
		for name, contents := range files {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return directory
}

func TestManifestDescribesActualBytes(t *testing.T) {
	directory := testAssets(t, "amd64", "arm64")
	release, err := buildManifest(directory, "v1.37.1-stratum.0", "stable", []string{"amd64", "arm64"}, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if release.ProductKey != "stratum" || release.RepositoryID != "stratum-binaries" || release.DefinitionID != "stratum-binaries" || release.DefinitionRevision != 1 || release.ReleaseAttributes["releaseLine"] != "1.37" {
		t.Fatalf("incorrect contract binding: %+v", release)
	}
	if _, err := manifestBytes(release); err != nil {
		t.Fatal(err)
	}
	for _, record := range manifestFiles(release) {
		contents, err := os.ReadFile(localFile(directory, record))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(contents)
		if record.Size != int64(len(contents)) || record.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("incorrect metadata for %s", record.Path)
		}
	}
}

func TestDevManifestDoesNotClaimSignatures(t *testing.T) {
	directory := testAssets(t, "arm64")
	for _, suffix := range []string{".sig", ".pem", ".spdx.json"} {
		if err := os.Remove(filepath.Join(directory, "stratum-linux-arm64"+suffix)); err != nil {
			t.Fatal(err)
		}
	}
	release, err := buildManifest(directory, "dev-main-20261008-abcdef0", "dev", []string{"arm64"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(release.Variants) != 1 || release.Variants[0].Signature != nil || release.Variants[0].Certificate != nil || release.Variants[0].SBOM != nil {
		t.Fatal("dev manifest claims release signing guarantees")
	}
}

func TestManifestRejectsInvalidInputs(t *testing.T) {
	for _, releaseID := range []string{"", "../other", "release/path", "constructor"} {
		if _, err := buildManifest(testAssets(t, "amd64"), releaseID, "dev", []string{"amd64"}, time.Now()); err == nil {
			t.Fatalf("accepted unsafe release ID %q", releaseID)
		}
	}
	for _, arches := range [][]string{nil, {"amd64", "amd64"}, {"s390x"}} {
		if _, err := buildManifest(testAssets(t, "amd64"), "dev-test", "dev", arches, time.Now()); err == nil {
			t.Fatalf("accepted invalid architectures %v", arches)
		}
	}
	directory := testAssets(t, "amd64", "arm64")
	if err := os.WriteFile(filepath.Join(directory, "build-info-arm64.json"), []byte(`{"target":{"os":"linux","arch":"arm64"},"components":{"kubernetes":{"version":"1.36.4"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := buildManifest(directory, "dev-test", "dev", []string{"amd64", "arm64"}, time.Now()); err == nil {
		t.Fatal("accepted mixed Kubernetes minor releases")
	}
}

func TestDryRunAndDestinationBinding(t *testing.T) {
	directory := testAssets(t, "amd64")
	const commit = "0123456789012345678901234567890123456789"
	if err := run(directory, "dev-test", "dev", []string{"amd64"}, "dev", commit, false); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil || !bytes.Contains(body, []byte(`"schemaVersion": 1`)) {
		t.Fatalf("dry run did not produce a contract manifest: %v", err)
	}
	// Authentic sums, so only the destination rules can reject what follows.
	testReleaseChecksums(t, directory)
	for _, destination := range []struct{ release, channel, environment, source string }{
		{"dev-test", "dev", "prod", commit},
		{"dev-test", "rc", "dev", commit},
		{"dev-test", "stable", "prod", commit},
		{"dev-test", "dev", "dev", strings.Repeat("f", 40)},
	} {
		if err := run(directory, destination.release, destination.channel, []string{"amd64"}, destination.environment, destination.source, false); err == nil {
			t.Fatalf("accepted invalid destination/source %+v", destination)
		}
	}
}

func TestManifestRejectsAlteredBinary(t *testing.T) {
	directory := testAssets(t, "amd64")
	if err := os.WriteFile(filepath.Join(directory, "stratum-linux-amd64"), []byte("altered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := buildManifest(directory, "dev-test", "dev", []string{"amd64"}, time.Now()); err == nil {
		t.Fatal("accepted binary inconsistent with build-info")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPublicationUsesConfiguredHostname(t *testing.T) {
	for _, environment := range []string{"dev", "prod"} {
		t.Run(environment, func(t *testing.T) {
			t.Setenv("DEPOT_USERNAME", "test-publisher")
			t.Setenv("DEPOT_TOKEN", "test-token")
			wantHost := environment + ".mirror.example.invalid"
			t.Setenv("DEPOT_HOSTNAME", wantHost)
			stopped := errors.New("stop before publication")
			transport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = transport })
			requested := false
			http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requested = true
				if req.URL.Scheme != "https" || req.URL.Host != wantHost {
					t.Errorf("publication destination = %s; want https://%s", req.URL, wantHost)
				}
				return nil, stopped
			})
			directory := testAssets(t, "amd64")
			testReleaseChecksums(t, directory)
			err := run(directory, "v1.37.1-stratum.0", "stable", []string{"amd64"}, environment, "0123456789012345678901234567890123456789", true)
			if !requested || !errors.Is(err, stopped) {
				t.Fatalf("publication did not reach the configured hostname: %v", err)
			}
		})
	}
}

func TestPublicationRejectsInvalidHostname(t *testing.T) {
	for _, hostname := range []string{
		"", "http://mirror.example.invalid", "https://mirror.example.invalid",
		"//mirror.example.invalid", "mirror.example.invalid/", "mirror.example.invalid/path",
		"user@mirror.example.invalid", "mirror.example.invalid?x=1", "mirror.example.invalid#x",
		" mirror.example.invalid", "mirror.example.invalid\n", ":8443",
	} {
		t.Run(hostname, func(t *testing.T) {
			t.Setenv("DEPOT_USERNAME", "test-publisher")
			t.Setenv("DEPOT_TOKEN", "test-token")
			t.Setenv("DEPOT_HOSTNAME", hostname)
			transport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = transport })
			http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("invalid hostname caused a publication request")
				return nil, errors.New("unexpected publication request")
			})
			err := run(testAssets(t, "amd64"), "dev-test", "dev", []string{"amd64"}, "dev", "0123456789012345678901234567890123456789", true)
			if err == nil || !strings.Contains(err.Error(), "DEPOT_HOSTNAME") {
				t.Fatalf("invalid hostname was not rejected: %v", err)
			}
		})
	}
}

func TestPublicationRequiresSecretCredentials(t *testing.T) {
	for _, missing := range []string{"DEPOT_USERNAME", "DEPOT_TOKEN"} {
		t.Run(missing, func(t *testing.T) {
			t.Setenv("DEPOT_USERNAME", "test-publisher")
			t.Setenv("DEPOT_TOKEN", "test-token")
			t.Setenv("DEPOT_HOSTNAME", "mirror.example.invalid")
			t.Setenv(missing, "")
			err := run(testAssets(t, "amd64"), "dev-test", "dev", []string{"amd64"}, "dev", "0123456789012345678901234567890123456789", true)
			if err == nil || !strings.Contains(err.Error(), "DEPOT_USERNAME and DEPOT_TOKEN are required") {
				t.Fatalf("accepted missing %s: %v", missing, err)
			}
		})
	}
}

func testReleaseChecksums(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, entry := range entries {
		if entry.Name() == "sha256sums.txt" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(body)
		lines = append(lines, hex.EncodeToString(digest[:])+"  "+entry.Name())
	}
	sort.Strings(lines)
	if err := os.WriteFile(filepath.Join(directory, "sha256sums.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseChecksumsAuthenticateAllDownloadedAssets(t *testing.T) {
	directory := testAssets(t, "amd64", "arm64")
	testReleaseChecksums(t, directory)
	if err := verifyReleaseChecksums(directory); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"build-info-amd64.json", "stratum-linux-arm64", "stratum-linux-amd64.pem", "stratum-linux-arm64.spdx.json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, name)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(append([]byte(nil), original...), '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := verifyReleaseChecksums(directory); err == nil {
				t.Fatalf("accepted modified %s", name)
			}
			if err := os.WriteFile(path, original, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSignedReleaseAcceptsCoveredAssets(t *testing.T) {
	for _, environment := range []string{"prod", "dev"} {
		for _, channel := range []string{"stable", "rc"} {
			t.Run(environment+"/"+channel, func(t *testing.T) {
				directory := testAssets(t, "amd64", "arm64")
				testReleaseChecksums(t, directory)
				for attempt := 0; attempt < 2; attempt++ {
					if err := run(directory, "v1.37.1-stratum.0", channel, []string{"amd64", "arm64"}, environment, "0123456789012345678901234567890123456789", false); err != nil {
						t.Fatal(err)
					}
				}
				body, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
				if err != nil || !bytes.Contains(body, []byte(`"releaseLine": "1.37"`)) {
					t.Fatalf("covered %s assets did not generate a manifest: %v", environment, err)
				}
			})
		}
	}
}

func TestSignedReleaseRejectsUnauthenticatedBuildInfoBeforeManifestGeneration(t *testing.T) {
	for _, destination := range []struct{ environment, channel string }{{"prod", "stable"}, {"dev", "rc"}} {
		for _, failure := range []string{"tampered", "unlisted", "missing-index", "duplicate", "malformed"} {
			t.Run(destination.environment+"/"+destination.channel+"/"+failure, func(t *testing.T) {
				directory := testAssets(t, "amd64")
				testReleaseChecksums(t, directory)
				indexPath := filepath.Join(directory, "sha256sums.txt")
				index, err := os.ReadFile(indexPath)
				if err != nil {
					t.Fatal(err)
				}
				switch failure {
				case "tampered":
					infoPath := filepath.Join(directory, "build-info-amd64.json")
					info, err := os.ReadFile(infoPath)
					if err != nil {
						t.Fatal(err)
					}
					info = bytes.ReplaceAll(info, []byte("1.37.1"), []byte("1.36.4"))
					err = os.WriteFile(infoPath, info, 0o644)
				case "unlisted":
					var lines []string
					for _, line := range strings.Split(string(index), "\n") {
						if line != "" && !strings.HasSuffix(line, "  build-info-amd64.json") {
							lines = append(lines, line)
						}
					}
					err = os.WriteFile(indexPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
				case "missing-index":
					err = os.Remove(indexPath)
				case "duplicate":
					err = os.WriteFile(indexPath, append(index, index...), 0o644)
				case "malformed":
					err = os.WriteFile(indexPath, []byte("not-a-digest  build-info-amd64.json\n"), 0o644)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := run(directory, "v1.37.1-stratum.0", destination.channel, []string{"amd64"}, destination.environment, "0123456789012345678901234567890123456789", false); err == nil {
					t.Fatalf("accepted %s build info for %+v", failure, destination)
				}
				for _, name := range []string{"manifest.json", "stratum-linux-amd64.sha256"} {
					if _, err := os.Stat(filepath.Join(directory, name)); !os.IsNotExist(err) {
						t.Fatalf("generated %s before authenticating metadata", name)
					}
				}
			})
		}
	}
}

func TestPublicationIsCreateOnlyManifestLastAndRetrySafe(t *testing.T) {
	directory := testAssets(t, "amd64", "arm64")
	release, err := buildManifest(directory, "dev-test", "dev", []string{"amd64", "arm64"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string][]byte{}
	var created []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		username, token, ok := req.BasicAuth()
		if !ok || username != "test-publisher" || token != "test-token" {
			t.Error("publisher did not use the scoped account")
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if req.Method == http.MethodGet {
			key := strings.TrimPrefix(req.URL.Path, "/dlv2/stratum-binaries/")
			if contents, exists := stored[key]; exists {
				writer.Write(contents)
			} else {
				writer.WriteHeader(http.StatusNotFound)
			}
			return
		}
		if req.Method != http.MethodPost || !strings.HasPrefix(req.URL.Path, "/api/v1/upload/stratum/stratum-binaries/") || req.URL.Query().Get("skip_post_upload") != "true" {
			t.Errorf("unexpected write %s %s", req.Method, req.URL)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		key := strings.TrimPrefix(req.URL.Path, "/api/v1/upload/stratum/stratum-binaries/")
		if _, exists := stored[key]; exists {
			writer.WriteHeader(http.StatusConflict)
			return
		}
		contents, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		digest := sha256.Sum256(contents)
		if req.ContentLength != int64(len(contents)) || req.Header.Get("X-Sha256") != hex.EncodeToString(digest[:]) {
			t.Error("upload was not bound to its exact size and SHA256")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		stored[key] = contents
		created = append(created, key)
	}))
	defer server.Close()
	publisher := depotPublisher{client: server.Client(), origin: server.URL, username: "test-publisher", token: "test-token"}
	files := manifestFiles(release)
	if err := publisher.publish(context.Background(), directory, &release, files); err != nil {
		t.Fatal(err)
	}
	manifestPath := "releases/dev-test/manifest.json"
	if len(created) != len(files)+1 || created[len(created)-1] != manifestPath {
		t.Fatalf("manifest was not the final write: %v", created)
	}
	original := append([]byte(nil), stored[manifestPath]...)
	release.PublishedAt = time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := publisher.publish(context.Background(), directory, &release, files); err != nil {
		t.Fatal(err)
	}
	if len(created) != len(files)+1 || !bytes.Equal(original, stored[manifestPath]) {
		t.Fatal("exact retry rewrote immutable content")
	}
	missing := stored[files[0].Path]
	delete(stored, files[0].Path)
	if err := publisher.publish(context.Background(), directory, &release, files); err == nil {
		t.Fatal("completed release with missing artifact was repaired silently")
	}
	if len(created) != len(files)+1 {
		t.Fatal("incomplete completed release caused writes")
	}
	stored[files[0].Path] = missing
	release.Channel = "rc"
	if err := publisher.publish(context.Background(), directory, &release, files); err == nil {
		t.Fatal("accepted conflicting manifest")
	}
	if len(created) != len(files)+1 {
		t.Fatal("conflicting manifest caused writes")
	}
}

func TestDepotClientRequestBudgetAndCancellation(t *testing.T) {
	client := newDepotHTTPClient()
	if client.Timeout != 20*time.Minute {
		t.Fatalf("request timeout is %s, want 20m", client.Timeout)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	publisher := depotPublisher{client: client, origin: "https://example.invalid", username: "test-publisher", token: "test-token"}
	if _, err := publisher.verify(ctx, fileRecord{Path: "releases/dev-test/artifact", Size: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled transfer did not preserve its error: %v", err)
	}
}

func TestDepotClientRejectsHTTPRedirect(t *testing.T) {
	plainHTTP := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		t.Error("publisher followed a redirect to HTTP")
		writer.WriteHeader(http.StatusOK)
	}))
	defer plainHTTP.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		http.Redirect(writer, req, plainHTTP.URL, http.StatusTemporaryRedirect)
	}))
	defer secure.Close()
	client := newDepotHTTPClient()
	client.Transport = secure.Client().Transport
	response, err := client.Get(secure.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("HTTPS redirect was followed: HTTP %d", response.StatusCode)
	}
}

func TestIndependentDevAttemptsDoNotConflict(t *testing.T) {
	directory := testAssets(t, "amd64")
	first, err := buildManifest(directory, "dev-12345-1", "dev", []string{"amd64"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "stratum-linux-amd64"), []byte("rebuilt-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	infoPath := filepath.Join(directory, "build-info-amd64.json")
	info, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("rebuilt-binary"))
	var buildInfo map[string]any
	if err := json.Unmarshal(info, &buildInfo); err != nil {
		t.Fatal(err)
	}
	buildInfo["artifacts"] = map[string]any{"stratum": map[string]any{"size": len("rebuilt-binary"), "sha256": hex.EncodeToString(digest[:])}}
	info, err = json.Marshal(buildInfo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(infoPath, info, 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := buildManifest(directory, "dev-12345-2", "dev", []string{"amd64"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	firstPaths := map[string]bool{}
	for _, record := range manifestFiles(first) {
		firstPaths[record.Path] = true
	}
	for _, record := range manifestFiles(second) {
		if firstPaths[record.Path] {
			t.Fatalf("rebuilt attempt reused immutable path %s", record.Path)
		}
	}
}

func TestFailedPublicationDoesNotCreateManifest(t *testing.T) {
	for _, failure := range []string{"unauthorized", "upload", "corrupt", "conflict", "redirect"} {
		t.Run(failure, func(t *testing.T) {
			directory := testAssets(t, "amd64")
			release, err := buildManifest(directory, "dev-test", "dev", []string{"amd64"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			var writes []string
			stored := map[string][]byte{}
			if failure == "conflict" {
				stored[release.Variants[0].Artifact.Path] = []byte("different-existing-content")
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
				if failure == "unauthorized" {
					writer.WriteHeader(http.StatusUnauthorized)
					return
				}
				if failure == "redirect" {
					writer.Header().Set("Location", "/redirect-target")
					writer.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				if req.Method == http.MethodGet {
					key := strings.TrimPrefix(req.URL.Path, "/dlv2/stratum-binaries/")
					if contents, exists := stored[key]; exists {
						writer.Write(contents)
					} else {
						writer.WriteHeader(http.StatusNotFound)
					}
					return
				}
				writes = append(writes, req.URL.Path)
				if failure == "upload" {
					writer.WriteHeader(http.StatusInternalServerError)
					return
				}
				stored[strings.TrimPrefix(req.URL.Path, "/api/v1/upload/stratum/stratum-binaries/")] = []byte("corrupt-readback")
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			publisher := depotPublisher{client: client, origin: server.URL, username: "test-publisher", token: "test-token"}
			if err := publisher.publish(context.Background(), directory, &release, manifestFiles(release)); err == nil {
				t.Fatal("publication accepted a failed or conflicting upload")
			}
			for _, destination := range writes {
				if strings.HasSuffix(destination, "/manifest.json") {
					t.Fatal("failed release acquired a completion manifest")
				}
			}
		})
	}
}

func TestDepotWorkflowConfigurationLint(t *testing.T) {
	for _, name := range []string{"dev-release", "release-publish"} {
		contents, err := os.ReadFile(filepath.Join("../../.github/workflows", name+".yml"))
		if err != nil {
			t.Fatal(err)
		}
		converted, err := yaml.YAMLToJSON(contents)
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			Jobs map[string]struct {
				Needs          json.RawMessage
				If             string
				Env            map[string]string
				TimeoutMinutes int `json:"timeout-minutes"`
				Strategy       struct {
					FailFast *bool `json:"fail-fast"`
					Matrix   struct {
						Include json.RawMessage
					}
				}
				Steps []struct {
					Run string
					Env map[string]string
				}
			}
		}
		if err := json.Unmarshal(converted, &workflow); err != nil {
			t.Fatal(err)
		}
		depot, exists := workflow.Jobs["depot"]
		if !exists {
			t.Fatalf("%s has no Depot target", name)
		}
		if depot.TimeoutMinutes != 60 {
			t.Fatal("Depot job timeout must remain 60 minutes pending real throughput measurements")
		}
		expectedSecret := "${{ secrets.DEPOT_DEV_TOKEN }}"
		expectedHostname := "${{ vars.DEPOT_DEV_HOSTNAME }}"
		expectedDestination := "--environment dev"
		if name == "dev-release" {
			for _, job := range []string{"meta", "release", "build", "images"} {
				if _, exists := workflow.Jobs[job]; !exists {
					t.Fatalf("dev target removed %s", job)
				}
			}
			if !bytes.Contains(depot.Needs, []byte("build")) {
				t.Fatal("dev publication is not gated on completed builds")
			}
			if depot.Env["DEPOT_RELEASE_ID"] != "dev-${{ github.run_id }}-${{ github.run_attempt }}" {
				t.Fatal("dev Depot release ID is not unique per workflow run attempt")
			}
		} else {
			expectedSecret = "${{ secrets[matrix.token] }}"
			expectedHostname = "${{ vars[matrix.hostname] }}"
			expectedDestination = `--environment "$DEPOT_ENVIRONMENT"`
			var include []map[string]string
			if err := json.Unmarshal(depot.Strategy.Matrix.Include, &include); err != nil {
				t.Fatal(err)
			}
			legs := map[string][2]string{}
			for _, leg := range include {
				legs[leg["environment"]] = [2]string{leg["token"], leg["hostname"]}
			}
			if len(legs) != 2 || legs["prod"] != [2]string{"DEPOT_PROD_TOKEN", "DEPOT_PROD_HOSTNAME"} || legs["dev"] != [2]string{"DEPOT_DEV_TOKEN", "DEPOT_DEV_HOSTNAME"} {
				t.Fatalf("release Depot legs are not prod and dev bound to their own token and hostname variable: %v", legs)
			}
			if depot.Strategy.FailFast == nil || *depot.Strategy.FailFast {
				t.Fatal("a failing Depot leg would cancel the other")
			}
			if string(depot.Needs) != `"conformance-guard"` || !strings.Contains(depot.If, "startsWith(github.event.release.tag_name, 'v')") {
				t.Fatal("prod publication bypasses conformance or accepts dev prereleases")
			}
			if _, ok := workflow.Jobs["channels"]; ok {
				t.Fatal("release-publish runs a channels job again")
			}
			if depot.Env["RELEASE_CHANNEL"] != "${{ needs.conformance-guard.outputs.final == 'true' && 'stable' || 'rc' }}" {
				t.Fatal("Depot release channel disagrees with conformance classifier")
			}
			downloadedIndex, authenticatedIndex := false, false
			for _, step := range depot.Steps {
				if strings.Contains(step.Run, "gh release download") {
					downloadedIndex = strings.Contains(step.Run, "--pattern 'sha256sums.txt'") && strings.Contains(step.Run, "--pattern 'sha256sums.txt.sig'") && strings.Contains(step.Run, "--pattern 'sha256sums.txt.pem'")
				}
				if strings.Contains(step.Run, "cosign verify-blob dist/sha256sums.txt") {
					authenticatedIndex = downloadedIndex && strings.Contains(step.Run, "--signature dist/sha256sums.txt.sig --certificate dist/sha256sums.txt.pem") && strings.Contains(step.Run, `--certificate-identity "https://github.com/$GITHUB_REPOSITORY/.github/workflows/release.yml@refs/tags/$TAG"`) && strings.Contains(step.Run, "--certificate-oidc-issuer https://token.actions.githubusercontent.com") && strings.Contains(step.Run, "set -euo pipefail")
				}
				if strings.Contains(step.Run, "go run ./tools/depotrelease") && !authenticatedIndex {
					t.Fatal("production generator runs before authenticating the release checksum index")
				}
			}
		}
		found := false
		for _, step := range depot.Steps {
			if strings.Contains(step.Run, expectedDestination) && step.Env["DEPOT_TOKEN"] == expectedSecret && step.Env["DEPOT_HOSTNAME"] == expectedHostname && step.Env["DEPOT_USERNAME"] == "${{ secrets.DEPOT_USERNAME }}" {
				if name == "release-publish" && step.Env["DEPOT_ENVIRONMENT"] != "${{ matrix.environment }}" {
					t.Fatal("release Depot leg does not publish to its own environment")
				}
				if name == "dev-release" && !strings.Contains(step.Run, `--release "$DEPOT_RELEASE_ID"`) {
					t.Fatal("dev publisher still uses the mutable GitHub tag as its release ID")
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is not bound to its secret username, token and hostname variable", name)
		}
	}
}

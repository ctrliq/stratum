package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed binary-release-manifest.schema.json
var manifestSchema []byte

const schemaDigest = "71a967fe0e765a703854302e08163478ccda203dea6dcb3d74b221b7826441c8"

func main() {
	directory := flag.String("assets", "dist", "directory containing the release assets")
	releaseID := flag.String("release", "", "immutable release identifier")
	channel := flag.String("channel", "", "dev, rc or stable")
	arches := flag.String("arches", "amd64,arm64", "comma-separated architectures")
	environment := flag.String("environment", "", "Depot destination: dev or prod")
	source := flag.String("source", "", "expected build source commit")
	publish := flag.Bool("publish", false, "publish over HTTPS using DEPOT_HOSTNAME, DEPOT_USERNAME and DEPOT_TOKEN from the environment")
	flag.Parse()
	if err := run(*directory, *releaseID, *channel, strings.Split(*arches, ","), *environment, *source, *publish); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(directory, releaseID, channel string, arches []string, environment, source string, publish bool) error {
	if (environment != "dev" && environment != "prod") || (environment == "prod" && channel == "dev") {
		return fmt.Errorf("dev builds must target dev; rc/stable releases may target dev or prod")
	}
	// An rc/stable release is the same signed release wherever it lands, so the
	// tag and checksum checks follow the channel, not the destination.
	if channel != "dev" && !strings.HasPrefix(releaseID, "v") {
		return fmt.Errorf("rc/stable publication requires a release tag")
	}
	if !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(source) {
		return fmt.Errorf("a full source commit is required")
	}
	if channel != "dev" {
		if err := verifyReleaseChecksums(directory); err != nil {
			return err
		}
	}
	release, err := buildManifest(directory, releaseID, channel, arches, time.Now())
	if err != nil {
		return err
	}
	if release.ReleaseAttributes["sourceCommit"] != source {
		return fmt.Errorf("build assets do not match the expected source commit")
	}
	files := manifestFiles(release)
	for _, build := range release.Variants {
		names := []string{"build-info-" + build.Architecture + ".json"}
		if channel != "dev" {
			names = append(names, "stratum-linux-"+build.Architecture+".spdx.json.sig", "stratum-linux-"+build.Architecture+".spdx.json.pem")
		}
		for _, name := range names {
			record, err := describeFile(directory, releaseID, name)
			if err != nil {
				return err
			}
			files = append(files, record)
		}
	}
	if publish {
		username := os.Getenv("DEPOT_USERNAME")
		token := os.Getenv("DEPOT_TOKEN")
		if username == "" || token == "" {
			return fmt.Errorf("DEPOT_USERNAME and DEPOT_TOKEN are required for publication")
		}
		// CI supplies a bare hostname; the publisher always uses HTTPS.
		hostname := os.Getenv("DEPOT_HOSTNAME")
		origin, err := url.Parse("https://" + hostname)
		if err != nil || origin.Host != hostname || origin.Hostname() == "" {
			return fmt.Errorf("DEPOT_HOSTNAME must be a bare host name without a scheme, credentials, path, query or fragment")
		}
		client := newDepotHTTPClient()
		publisher := depotPublisher{client: client, origin: origin.String(), username: username, token: token}
		if err := publisher.publish(context.Background(), directory, &release, files); err != nil {
			return err
		}
	}
	body, err := manifestBytes(release)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), body, 0o644); err != nil {
		return err
	}
	fmt.Printf("validated %s manifest for %s (%d variants)\n", environment, releaseID, len(release.Variants))
	return nil
}

type fileRecord struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type variant struct {
	ID           string      `json:"variantId"`
	Architecture string      `json:"architecture"`
	OS           string      `json:"os"`
	Artifact     fileRecord  `json:"artifact"`
	Checksum     fileRecord  `json:"checksum"`
	Signature    *fileRecord `json:"signature,omitempty"`
	Certificate  *fileRecord `json:"certificate,omitempty"`
	SBOM         *fileRecord `json:"sbom,omitempty"`
}

type manifest struct {
	Kind               string            `json:"kind"`
	SchemaVersion      int               `json:"schemaVersion"`
	ProductKey         string            `json:"productKey"`
	RepositoryID       string            `json:"repositoryId"`
	DefinitionID       string            `json:"definitionId"`
	DefinitionRevision int               `json:"definitionRevision"`
	ReleaseID          string            `json:"releaseId"`
	PublishedAt        string            `json:"publishedAt"`
	Channel            string            `json:"channel"`
	ReleaseAttributes  map[string]string `json:"releaseAttributes"`
	Variants           []variant         `json:"variants"`
}

var safeSegment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var kubernetesVersion = regexp.MustCompile(`^(1\.[0-9]+)\.[0-9]+$`)

func buildManifest(directory, releaseID, channel string, arches []string, publishedAt time.Time) (manifest, error) {
	result := manifest{
		Kind: "binary-release-manifest", SchemaVersion: 1,
		ProductKey: "stratum", RepositoryID: "stratum-binaries",
		DefinitionID: "stratum-binaries", DefinitionRevision: 1,
		ReleaseID: releaseID, PublishedAt: publishedAt.UTC().Format(time.RFC3339),
		Channel: channel, ReleaseAttributes: map[string]string{},
	}
	if !safeSegment.MatchString(releaseID) || releaseID == "prototype" || releaseID == "constructor" {
		return result, fmt.Errorf("release ID is not a safe contract segment")
	}
	if channel != "dev" && channel != "rc" && channel != "stable" {
		return result, fmt.Errorf("unsupported release channel %q", channel)
	}
	if len(arches) == 0 || len(arches) > 2 {
		return result, fmt.Errorf("one or two architectures are required")
	}
	seen := map[string]bool{}
	for _, arch := range arches {
		if (arch != "amd64" && arch != "arm64") || seen[arch] {
			return result, fmt.Errorf("invalid or duplicate architecture %q", arch)
		}
		seen[arch] = true
		var info struct {
			Target     struct{ OS, Arch string }
			Source     struct{ Commit string }
			Components map[string]struct{ Version string }
			Artifacts  map[string]struct {
				SHA256 string `json:"sha256"`
				Size   int64  `json:"size"`
			}
		}
		infoBytes, err := os.ReadFile(filepath.Join(directory, "build-info-"+arch+".json"))
		if err != nil {
			return result, err
		}
		if err := json.Unmarshal(infoBytes, &info); err != nil {
			return result, fmt.Errorf("invalid build info: %w", err)
		}
		version := kubernetesVersion.FindStringSubmatch(info.Components["kubernetes"].Version)
		if info.Target.OS != "linux" || info.Target.Arch != arch || version == nil {
			return result, fmt.Errorf("build info does not identify Linux %s and a Kubernetes patch version", arch)
		}
		if previous := result.ReleaseAttributes["releaseLine"]; previous != "" && previous != version[1] {
			return result, fmt.Errorf("architectures disagree on Kubernetes minor")
		}
		result.ReleaseAttributes["releaseLine"] = version[1]
		for key, value := range map[string]string{"sourceCommit": info.Source.Commit, "kubernetesVersion": info.Components["kubernetes"].Version} {
			if value == "" || (result.ReleaseAttributes[key] != "" && result.ReleaseAttributes[key] != value) {
				return result, fmt.Errorf("architectures disagree on %s or it is missing", key)
			}
			result.ReleaseAttributes[key] = value
		}
		name := "stratum-linux-" + arch
		artifact, err := describeFile(directory, releaseID, name)
		if err != nil {
			return result, err
		}
		if expected := info.Artifacts["stratum"]; expected.SHA256 != artifact.SHA256 || expected.Size != artifact.Size {
			return result, fmt.Errorf("%s does not match its build-info digest/size", name)
		}
		checksumName := name + ".sha256"
		if err := os.WriteFile(filepath.Join(directory, checksumName), []byte(artifact.SHA256+"  "+name+"\n"), 0o644); err != nil {
			return result, err
		}
		checksum, err := describeFile(directory, releaseID, checksumName)
		if err != nil {
			return result, err
		}
		build := variant{ID: "linux-" + arch, Architecture: arch, OS: "linux", Artifact: artifact, Checksum: checksum}
		if channel != "dev" {
			for suffix, target := range map[string]**fileRecord{".sig": &build.Signature, ".pem": &build.Certificate, ".spdx.json": &build.SBOM} {
				record, err := describeFile(directory, releaseID, name+suffix)
				if err != nil {
					return result, err
				}
				*target = &record
			}
		}
		result.Variants = append(result.Variants, build)
	}
	return result, nil
}

func describeFile(directory, releaseID, name string) (fileRecord, error) {
	file, err := os.Open(filepath.Join(directory, name))
	if err != nil {
		return fileRecord{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fileRecord{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 9007199254740991 {
		return fileRecord{}, fmt.Errorf("%s is not a nonempty regular file within the contract size limit", name)
	}
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return fileRecord{}, err
	}
	if size != info.Size() {
		return fileRecord{}, fmt.Errorf("%s changed while hashing", name)
	}
	return fileRecord{Path: "releases/" + releaseID + "/" + name, Size: size, SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
}

func verifyReleaseChecksums(directory string) error {
	index, err := os.Open(filepath.Join(directory, "sha256sums.txt"))
	if err != nil {
		return fmt.Errorf("reading authenticated release checksums: %w", err)
	}
	defer index.Close()
	checksums := map[string]string{}
	validDigest := regexp.MustCompile(`^[a-f0-9]{64}$`)
	scanner := bufio.NewScanner(index)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || !validDigest.MatchString(fields[0]) || !safeSegment.MatchString(fields[1]) {
			return fmt.Errorf("release checksum index has a malformed entry")
		}
		name := fields[1]
		if _, duplicate := checksums[name]; duplicate {
			return fmt.Errorf("release checksum index repeats %s", name)
		}
		record, err := describeFile(directory, "verification", name)
		if err != nil {
			return err
		}
		if record.SHA256 != fields[0] {
			return fmt.Errorf("%s does not match the authenticated release checksum", name)
		}
		checksums[name] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "stratum-linux-amd64.sha256" || name == "stratum-linux-arm64.sha256" {
			continue
		}
		if strings.HasPrefix(name, "stratum-linux-") || (strings.HasPrefix(name, "build-info-") && strings.HasSuffix(name, ".json")) {
			if _, covered := checksums[name]; !covered {
				return fmt.Errorf("%s is not covered by the authenticated release checksums", name)
			}
		}
	}
	return nil
}

func manifestBytes(release manifest) ([]byte, error) {
	encoded, err := json.MarshalIndent(release, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(encoded)+1 > 128*1024 {
		return nil, fmt.Errorf("manifest exceeds the contract size limit")
	}
	digest := sha256.Sum256(manifestSchema)
	if hex.EncodeToString(digest[:]) != schemaDigest {
		return nil, fmt.Errorf("canonical manifest schema digest mismatch")
	}
	resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(manifestSchema))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("manifest-schema.json", resource); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile("manifest-schema.json")
	if err != nil {
		return nil, err
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(value); err != nil {
		return nil, fmt.Errorf("manifest violates the Depot contract: %w", err)
	}
	return append(encoded, '\n'), nil
}

func manifestFiles(release manifest) []fileRecord {
	var files []fileRecord
	for _, build := range release.Variants {
		files = append(files, build.Artifact, build.Checksum)
		for _, record := range []*fileRecord{build.Signature, build.Certificate, build.SBOM} {
			if record != nil {
				files = append(files, *record)
			}
		}
	}
	return files
}

func localFile(directory string, record fileRecord) string {
	return filepath.Join(directory, record.Path[strings.LastIndex(record.Path, "/")+1:])
}

type depotPublisher struct {
	client   *http.Client
	origin   string
	username string
	token    string
}

func newDepotHTTPClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func reportTransfer(operation, path string, size int64, started time.Time, statusCode int) {
	elapsed := time.Since(started)
	fmt.Printf("%s %s: bytes=%d elapsed=%s throughput=%.2f MB/s HTTP=%d\n", operation, path, size, elapsed.Round(time.Millisecond), float64(size)/elapsed.Seconds()/1e6, statusCode)
}

func (publisher depotPublisher) request(ctx context.Context, method, endpoint string, body io.Reader, record fileRecord) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, publisher.origin+endpoint, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(publisher.username, publisher.token)
	if method == http.MethodPost {
		req.ContentLength = record.Size
		req.Header.Set("X-Sha256", record.SHA256)
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return publisher.client.Do(req)
}

func (publisher depotPublisher) verify(ctx context.Context, record fileRecord) (bool, error) {
	started := time.Now()
	var size int64
	statusCode := 0
	fmt.Printf("readback %s: expected_bytes=%d starting\n", record.Path, record.Size)
	defer func() { reportTransfer("readback", record.Path, size, started, statusCode) }()
	response, err := publisher.request(ctx, http.MethodGet, "/dlv2/stratum-binaries/"+record.Path, nil, fileRecord{})
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	statusCode = response.StatusCode
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("readback of %s returned HTTP %d", record.Path, response.StatusCode)
	}
	digest := sha256.New()
	size, err = io.Copy(digest, io.LimitReader(response.Body, record.Size+1))
	if err != nil {
		return false, err
	}
	if size != record.Size || hex.EncodeToString(digest.Sum(nil)) != record.SHA256 {
		return false, fmt.Errorf("immutable content conflict or corrupt readback at %s", record.Path)
	}
	return true, nil
}

func (publisher depotPublisher) create(ctx context.Context, record fileRecord, open func() (io.ReadCloser, error)) error {
	exists, err := publisher.verify(ctx, record)
	if err != nil || exists {
		return err
	}
	body, err := open()
	if err != nil {
		return err
	}
	defer body.Close()
	started := time.Now()
	fmt.Printf("upload %s: size_bytes=%d starting\n", record.Path, record.Size)
	response, err := publisher.request(ctx, http.MethodPost, "/api/v1/upload/stratum/stratum-binaries/"+record.Path+"?skip_post_upload=true", body, record)
	if err != nil {
		reportTransfer("upload", record.Path, 0, started, 0)
		return err
	}
	response.Body.Close()
	var uploaded int64
	if response.StatusCode == http.StatusOK {
		uploaded = record.Size
	}
	reportTransfer("upload", record.Path, uploaded, started, response.StatusCode)
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusConflict {
		return fmt.Errorf("create of %s returned HTTP %d", record.Path, response.StatusCode)
	}
	exists, err = publisher.verify(ctx, record)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s is missing after publication", record.Path)
	}
	return nil
}

func (publisher depotPublisher) publish(ctx context.Context, directory string, release *manifest, files []fileRecord) error {
	manifestPath := "releases/" + release.ReleaseID + "/manifest.json"
	started := time.Now()
	fmt.Printf("manifest lookup %s: starting\n", manifestPath)
	response, err := publisher.request(ctx, http.MethodGet, "/dlv2/stratum-binaries/"+manifestPath, nil, fileRecord{})
	if err != nil {
		reportTransfer("manifest lookup", manifestPath, 0, started, 0)
		return err
	}
	existing, readErr := io.ReadAll(io.LimitReader(response.Body, 128*1024+1))
	response.Body.Close()
	reportTransfer("manifest lookup", manifestPath, int64(len(existing)), started, response.StatusCode)
	if readErr != nil {
		return readErr
	}
	if response.StatusCode == http.StatusOK {
		var previous manifest
		decoder := json.NewDecoder(bytes.NewReader(existing))
		decoder.DisallowUnknownFields()
		if len(existing) > 128*1024 || decoder.Decode(&previous) != nil {
			return fmt.Errorf("existing release manifest is invalid")
		}
		publishedAt, err := time.Parse(time.RFC3339, previous.PublishedAt)
		if err != nil || publishedAt.After(time.Now()) {
			return fmt.Errorf("existing release publication timestamp is invalid")
		}
		release.PublishedAt = previous.PublishedAt
		candidate, err := manifestBytes(*release)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing, candidate) {
			return fmt.Errorf("immutable release manifest conflict at %s", manifestPath)
		}
	} else if response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("manifest lookup returned HTTP %d", response.StatusCode)
	}
	if _, err := manifestBytes(*release); err != nil {
		return err
	}
	for _, record := range files {
		if response.StatusCode == http.StatusOK {
			exists, err := publisher.verify(ctx, record)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("completed release is missing %s", record.Path)
			}
			continue
		}
		if err := publisher.create(ctx, record, func() (io.ReadCloser, error) { return os.Open(localFile(directory, record)) }); err != nil {
			return err
		}
	}
	if response.StatusCode == http.StatusNotFound {
		release.PublishedAt = time.Now().UTC().Format(time.RFC3339)
	}
	body, err := manifestBytes(*release)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	record := fileRecord{Path: manifestPath, Size: int64(len(body)), SHA256: hex.EncodeToString(digest[:])}
	return publisher.create(ctx, record, func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil })
}

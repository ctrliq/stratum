package operator

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/downloader"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/helmpath"
	"helm.sh/helm/v4/pkg/registry"
	repo "helm.sh/helm/v4/pkg/repo/v1"
)

// LocateChart in the pinned Helm SDK takes no injected getters; requests follow ctx.
func locateChart(ctx context.Context, spec helmSpec, settings *cli.EnvSettings) (string, error) {
	locate := strings.TrimSpace(spec.chart)
	if spec.repo == "" {
		if _, err := os.Stat(locate); err == nil {
			return filepath.Abs(locate)
		}
		if filepath.IsAbs(locate) || strings.HasPrefix(locate, ".") {
			return "", fmt.Errorf("path %q not found", locate)
		}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	var entry *repo.Entry
	if spec.repo != "" {
		if !isOCI(spec.repo) {
			// The entry carries the TLS files and credentials for index and archive fetches.
			var err error
			entry, err = repoURLEntry(spec.repo, settings)
			if err != nil {
				return "", err
			}
		}
	} else if !isOCI(locate) {
		var err error
		entry, err = chartRepositoryEntry(locate, settings)
		if err != nil {
			return "", err
		}
	}
	tlsConfig, err := chartFetchTLSConfig(spec, entry)
	if err != nil {
		return "", err
	}
	transport.TLSClientConfig = tlsConfig
	var next http.RoundTripper = transport
	if entry != nil && len(tlsConfig.Certificates) > 0 && !entry.PassCredentialsAll {
		// The client certificate is a credential like the basic auth: withheld off-origin.
		other := transport.Clone()
		other.TLSClientConfig = tlsConfig.Clone()
		other.TLSClientConfig.Certificates = nil
		defer other.CloseIdleConnections()
		next = newOriginTransport(entry.URL, transport, other)
	}
	bound := &contextTransport{ctx: ctx, next: next}
	client := &http.Client{Transport: bound}
	registryClient, err := registry.NewClient(registry.ClientOptHTTPClient(client))
	if err != nil {
		return "", err
	}
	// WithTransport accepts only *http.Transport; protocol forwarding routes RoundTrips through ctx.
	forwarding := &http.Transport{}
	forwarding.RegisterProtocol("http", bound)
	forwarding.RegisterProtocol("https", bound)
	providers := getter.Providers{
		{Schemes: []string{"http", "https"}, New: func(options ...getter.Option) (getter.Getter, error) {
			return getter.NewHTTPGetter(append(options, getter.WithTransport(forwarding))...)
		}},
		{Schemes: []string{registry.OCIScheme}, New: func(options ...getter.Option) (getter.Getter, error) {
			return getter.NewOCIGetter(append(options, getter.WithRegistryClient(registryClient))...)
		}},
	}
	if isOCI(spec.repo) {
		locate = spec.repo + "/" + locate
	} else if spec.repo != "" {
		options := []repo.FindChartInRepoURLOption{repo.WithChartVersion(spec.version)}
		if entry != nil {
			// Authenticate the index with the entry; the transport already carries its TLS files.
			options = append(options, repo.WithUsernamePassword(entry.Username, entry.Password), repo.WithPassCredentialsAll(entry.PassCredentialsAll))
		}
		locate, err = repo.FindChartInRepoURL(spec.repo, locate, providers, options...)
		if err != nil {
			return "", err
		}
	}
	if u, err := url.Parse(locate); err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
		// The URL is already resolved; ChartDownloader would re-resolve and re-scan it.
		if settings.ContentCache == "" {
			return "", fmt.Errorf("content cache must be set")
		}
		options := []getter.Option{getter.WithURL(locate), getter.WithAcceptHeader("application/gzip,application/octet-stream")}
		if entry == nil && spec.repo != "" {
			// An index-resolved archive with no matching entry still gets the owner scan.
			var err error
			entry, err = chartRepositoryEntry(locate, settings)
			if err != nil {
				return "", err
			}
		}
		if entry != nil {
			// Bind under the archive's own spelling: the getter compares scheme and host verbatim.
			bound := normalizeRepoURL(entry.URL)
			if e, err := url.Parse(entry.URL); err == nil &&
				strings.EqualFold(u.Scheme, e.Scheme) && strings.EqualFold(u.Host, e.Host) {
				bound = u.Scheme + "://" + u.Host
			}
			options = append(options, getter.WithURL(bound), getter.WithBasicAuth(entry.Username, entry.Password), getter.WithPassCredentialsAll(entry.PassCredentialsAll))
		}
		g, err := providers.ByScheme(u.Scheme)
		if err != nil {
			return "", err
		}
		data, err := g.Get(locate, options...)
		if err != nil {
			return "", err
		}
		cache := downloader.DiskCache{Root: settings.ContentCache}
		return cache.Put(sha256.Sum256(data.Bytes()), data, downloader.CacheChart)
	}
	dl := downloader.ChartDownloader{
		Out:              io.Discard,
		Getters:          providers,
		RegistryClient:   registryClient,
		RepositoryConfig: settings.RepositoryConfig,
		RepositoryCache:  settings.RepositoryCache,
		ContentCache:     settings.ContentCache,
		Options:          []getter.Option{getter.WithRegistryClient(registryClient), getter.WithTransport(forwarding)},
	}
	path, _, err := dl.DownloadToCache(locate, spec.version)
	return path, err
}

// chartRepositoryEntry matches the downloader's owner selection for repo refs and archive URLs.
func chartRepositoryEntry(ref string, settings *cli.EnvSettings) (*repo.Entry, error) {
	file, err := repo.LoadFile(settings.RepositoryConfig)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !strings.Contains(ref, "://") {
		name, _, _ := strings.Cut(ref, "/")
		for _, entry := range file.Repositories {
			if entry.Name == name {
				return entry, nil
			}
		}
		return nil, nil
	}
	ref = normalizeRepoURL(ref)
	for _, entry := range file.Repositories {
		index, err := repo.LoadIndexFile(filepath.Join(settings.RepositoryCache, helmpath.CacheIndexFile(entry.Name)))
		if err != nil {
			continue // a direct archive does not require a repository index
		}
		for _, versions := range index.Entries {
			for _, version := range versions {
				for _, archiveURL := range version.URLs {
					if normalizeRepoURL(archiveURL) == ref {
						return entry, nil
					}
				}
			}
		}
	}
	return nil, nil
}

// repoURLEntry returns the repositories.yaml entry whose URL equals repoURL; nil, not an error.
func repoURLEntry(repoURL string, settings *cli.EnvSettings) (*repo.Entry, error) {
	file, err := repo.LoadFile(settings.RepositoryConfig)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ref := normalizeRepoURL(repoURL)
	for _, entry := range file.Repositories {
		if normalizeRepoURL(entry.URL) == ref {
			return entry, nil
		}
	}
	return nil, nil
}

// normalizeRepoURL lowercases scheme and host and drops userinfo, fragment and the trailing slash.
func normalizeRepoURL(ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.User = nil
	u.Fragment, u.RawFragment = "", ""
	escaped := strings.TrimSuffix(u.EscapedPath(), "/")
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return ref
	}
	u.Path, u.RawPath = decoded, escaped
	return u.String()
}

// originTransport sends requests to the entry's origin through own, everything else through other.
type originTransport struct {
	scheme, host string
	own, other   http.RoundTripper
}

func newOriginTransport(entryURL string, own, other http.RoundTripper) *originTransport {
	t := &originTransport{own: own, other: other}
	if u, err := url.Parse(entryURL); err == nil {
		t.scheme, t.host = u.Scheme, u.Host
	}
	return t
}

func (t *originTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.host != "" && strings.EqualFold(req.URL.Scheme, t.scheme) && strings.EqualFold(req.URL.Host, t.host) {
		return t.own.RoundTrip(req)
	}
	return t.other.RoundTrip(req)
}

func chartFetchTLSConfig(spec helmSpec, entry *repo.Entry) (*tls.Config, error) {
	cfg := &tls.Config{InsecureSkipVerify: spec.insecureSkipTLSVerify} // explicitly gated by the operator policy
	caPEM := []byte(spec.repoCA)
	if entry != nil {
		// Local repository configuration is administrator-owned; its insecure flag is theirs to set.
		cfg.InsecureSkipVerify = cfg.InsecureSkipVerify || entry.InsecureSkipTLSVerify
		if entry.CAFile != "" {
			var err error
			caPEM, err = os.ReadFile(entry.CAFile)
			if err != nil {
				return nil, err
			}
		}
		if entry.CertFile != "" && entry.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(entry.CertFile, entry.KeyFile)
			if err != nil {
				return nil, err
			}
			cfg.Certificates = []tls.Certificate{cert}
		}
	}
	if len(caPEM) > 0 {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("repository CA contains no certificates")
		}
		cfg.RootCAs = roots
	}
	return cfg, nil
}

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

// LocateChart does not expose getter injection in the pinned Helm SDK. Use
// its repository resolution and downloader directly, with requests owned by
// this reconcile, so timeout and leadership loss stop network I/O before the
// next pass starts. There is no detached download worker.
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
			// An http(s) spec.repo takes the repositories.yaml entry naming the
			// same repository: its TLS files feed the backing transport built
			// below, its credentials authenticate the index resolution and the
			// archive download — the binding a named chart reference gets from
			// its owner scan.
			var err error
			entry, err = repoURLEntry(spec.repo, settings)
			if err != nil {
				return "", err
			}
		}
	} else if !isOCI(locate) {
		// Helm's downloader adds this named repository's authentication and
		// TLS options after resolving its cached index. Preserve its TLS files
		// in the backing transport; the builtin HTTP getter below still owns
		// authentication and the passCredentialsAll origin policy.
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
		// The client certificate is a credential like the basic auth below, so
		// it follows the same origin rule: an index can point the archive, or
		// a redirect, at another host, which must not be handed the entry's
		// certificate unless the entry sets pass_credentials_all.
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
	// WithTransport accepts only *http.Transport. Its protocol forwarding
	// seam lets the builtin getter retain its authentication/header behavior
	// while the actual RoundTrip and body reads follow this reconcile's context.
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
			// Authenticate the index resolution against the matched entry. No
			// TLS options here: the getter's transport is already the one built
			// from chartFetchTLSConfig, and these options would be inert. Helm
			// binds the credentials to the entry's own URL, so they go to
			// another host only with pass_credentials_all.
			options = append(options, repo.WithUsernamePassword(entry.Username, entry.Password), repo.WithPassCredentialsAll(entry.PassCredentialsAll))
		}
		locate, err = repo.FindChartInRepoURL(spec.repo, locate, providers, options...)
		if err != nil {
			return "", err
		}
	}
	if u, err := url.Parse(locate); err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
		// The archive URL is already resolved: either an absolute spec.chart or
		// one resolved against spec.repo's index above. Download it with Helm's
		// getter and content cache directly: ChartDownloader would re-resolve
		// it through cached indexes — failing "no cached repo found" on any
		// registered repository that lacks one — and repeat the optional owner
		// scan while rejecting unrelated unreadable repository indexes again.
		if settings.ContentCache == "" {
			return "", fmt.Errorf("content cache must be set")
		}
		options := []getter.Option{getter.WithURL(locate), getter.WithAcceptHeader("application/gzip,application/octet-stream")}
		if entry == nil && spec.repo != "" {
			// An index-resolved archive with no entry matching spec.repo still
			// deserves the owner scan the downloader would have run: it binds
			// the credentials of the registered repository whose cached index
			// lists this archive, when those indexes happen to be readable.
			// chartFetchTLSConfig has already run, so a scanned owner's TLS
			// files are not applied here — the same as every other archive
			// fetch for a spec.repo without a matching entry.
			var err error
			entry, err = chartRepositoryEntry(locate, settings)
			if err != nil {
				return "", err
			}
		}
		if entry != nil {
			// Bind credentials to the repository origin, not the archive's:
			// an index can point at another host or port. Helm's getter origin
			// check compares scheme and host verbatim, so when the archive
			// names the entry's own origin, bind under the archive's spelling
			// — a lowercased entry URL would withhold the credentials from an
			// upper-case spelling of the same host — while another origin keeps
			// the normalized entry URL and stays unauthenticated unless the
			// entry sets pass_credentials_all.
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

// Match the downloader's owner selection for both repo/chart references and
// absolute archive URLs found in a registered repository's cached index. Its
// TLS options are opaque getter options, so choose the same entry before
// constructing the context-bound backing transport.
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

// repoURLEntry returns the repositories.yaml entry whose URL names the same
// repository as repoURL: equal after normalizeRepoURL, so same scheme, host,
// path and query. The first match in file order wins, mirroring Helm's own
// owner scan. No entry is not an error — a spec.repo with no matching entry
// must fetch anonymously rather than with some other repository's
// credentials — but an unreadable file is, exactly as chartRepositoryEntry
// treats one.
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

// normalizeRepoURL canonicalizes a repository URL for equality comparison:
// scheme and host are lowercased (DNS names and schemes are case-insensitive),
// userinfo and fragment are dropped so only the location identifies the
// repository, an empty path is the root, and a trailing slash is dropped so
// ".../charts" and ".../charts/" name one repository. The path is compared in
// its escaped form exactly as written: Helm fetches the spec.repo path
// verbatim, so cleaning empty or dot segments would equate a URL that
// resolves elsewhere on the server and hand the entry's credentials to it.
// Path case is significant, the query is compared exactly, and unparseable
// input is returned unchanged so a comparison simply misses.
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

// originTransport sends a request through own when it goes to the repository
// entry's origin (scheme and host, port included, compared ignoring case) and
// through other everywhere else, redirect hops included. An entry URL that does
// not parse matches nothing, so its certificate is never presented.
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
		// Local repository configuration is administrator-owned; the spec
		// flag above has already passed the operator's insecure-repo policy.
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

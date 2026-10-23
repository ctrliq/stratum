// Package kube is Stratum's Kubernetes access layer: config resolution,
// clients, and resource addressing.
package kube

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	memcached "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// Default transport settings. A tool call is answered while an operator (or
// a model) waits on it, so these are tuned to fail visibly rather than hang.
const (
	// DefaultTimeout bounds a single API request.
	DefaultTimeout = 30 * time.Second
	// DefaultQPS and DefaultBurst raise client-go's defaults (5/10), which
	// are sized for a controller's steady-state reconcile loop and are far
	// too low for the bursts of discovery a single tool call makes.
	DefaultQPS   float32 = 50
	DefaultBurst int     = 100
)

// Options configure a Connector. The zero value is usable: it auto-detects
// configuration and applies the defaults above.
type Options struct {
	// Kubeconfig is an explicit path, taking precedence over $KUBECONFIG and
	// ~/.kube/config. Empty means auto-detect.
	Kubeconfig string
	// KubeconfigPath selects an explicit local credential on every resolution,
	// overriding Kubeconfig. An empty or missing selected path is a hard error;
	// environment, home and in-cluster credentials are never fallback sources.
	KubeconfigPath func() string

	// Context selects a kubeconfig context. Empty means the file's
	// current-context.
	Context string

	// FallbackKubeconfig is tried after $KUBECONFIG and ~/.kube/config and
	// before in-cluster.
	FallbackKubeconfig string

	// Timeout bounds a single request. Zero means DefaultTimeout.
	Timeout time.Duration

	// MaxResponseBytes caps one response body. Zero means
	// DefaultMaxResponseBytes; negative disables the cap.
	MaxResponseBytes int64

	QPS   float32
	Burst int

	// Logger receives diagnostics with no caller to return them to. Nil
	// discards them. It must not write to stdout.
	Logger *slog.Logger
}

func (o Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return DefaultTimeout
	}
	return o.Timeout
}

func (o Options) maxBytes() int64 {
	if o.MaxResponseBytes == 0 {
		return DefaultMaxResponseBytes
	}
	return o.MaxResponseBytes
}

// Connector resolves Kubernetes configuration and hands out clients.
type Connector struct {
	opts Options

	mu       sync.Mutex
	clusters map[string]*Cluster
}

// NewConnector returns a Connector.
func NewConnector(o Options) *Connector {
	return &Connector{opts: o, clusters: map[string]*Cluster{}}
}

// Cluster is a connected cluster: clients, discovery, and the RESTMapper
// that turns a name an operator typed into a resource the API serves.
type Cluster struct {
	// Typed serves the built-in API groups with compile-time types.
	Typed kubernetes.Interface
	// Dynamic serves any resource, including CRDs, as unstructured data.
	// The generic toolset is built on this.
	Dynamic dynamic.Interface
	// Discovery enumerates what the cluster actually serves.
	Discovery discovery.CachedDiscoveryInterface

	// Mapper resolves a kind or resource name to a GroupVersionResource.
	Mapper meta.RESTMapper

	// Config is the resolved client configuration.
	Config *rest.Config

	// ContextName is the kubeconfig context in use, empty for in-cluster.
	ContextName string
	// Source says where the configuration came from.
	Source Source
	// ConfigPath is the kubeconfig file used, empty for in-cluster.
	ConfigPath string
	// Host is the API server URL, which is what an operator recognises a
	// cluster by when a context name is ambiguous or absent.
	Host string

	// digest is the content fingerprint of every kubeconfig source and
	// referenced credential file this cluster was built from. The connector
	// revalidates it before handing the cluster out again; in-cluster has no
	// files and digests empty.
	digest string

	refreshMu   sync.Mutex
	lastRefresh time.Time
}

// RefreshInterval is the shortest gap between two discovery refreshes
// triggered by an unresolved resource name.
const RefreshInterval = 10 * time.Second

// refreshDiscovery drops the cached discovery, at most once per
// RefreshInterval. It reports whether it actually refreshed.
func (c *Cluster) refreshDiscovery(ctx context.Context) bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if time.Since(c.lastRefresh) < RefreshInterval {
		return false
	}
	c.lastRefresh = time.Now()
	c.Discovery.Invalidate()
	return true
}

// ContextInfo describes one selectable context.
type ContextInfo struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace,omitempty"`
	User      string `json:"user,omitempty"`
	Current   bool   `json:"current"`
}

// Contexts lists the contexts available in the resolved kubeconfig.
func (c *Connector) Contexts() ([]ContextInfo, error) {
	r, err := c.resolve(c.opts.Context)
	if err != nil {
		return nil, err
	}
	if r.rawConfig == nil {
		return []ContextInfo{}, nil
	}

	current := r.rawConfig.CurrentContext
	if c.opts.Context != "" {
		current = c.opts.Context
	}

	out := make([]ContextInfo, 0, len(r.rawConfig.Contexts))
	for name, ctx := range r.rawConfig.Contexts {
		out = append(out, ContextInfo{
			Name:      name,
			Cluster:   ctx.Cluster,
			Namespace: ctx.Namespace,
			User:      ctx.AuthInfo,
			Current:   name == current,
		})
	}
	// Map iteration order is random, and a tool that lists contexts in a
	// different order on every call reads as though the cluster changed.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Connector) resolve(name string) (*resolved, error) {
	if c.opts.KubeconfigPath != nil {
		path := c.opts.KubeconfigPath()
		if path == "" {
			return nil, ErrNoConfig
		}
		return resolve(path, "", name)
	}
	return resolve(c.opts.Kubeconfig, c.opts.FallbackKubeconfig, name)
}

// Cluster connects to the named context, or the current one when name is
// empty. Results are cached per name and revalidated before reuse: every
// kubeconfig source and referenced credential file is digested again, and a
// cached cluster is handed back only while the digest matches what it was
// built from. A rewrite — even one that lands with the old mtime, as a
// staged copy over its target can — therefore rebuilds the client on the
// next call, while unchanged inputs keep the cached one and its discovery
// cache.
func (c *Connector) Cluster(name string) (*Cluster, error) {
	if name == "" {
		name = c.opts.Context
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for range configSnapshotAttempts {
		cl, err := c.cluster(name)
		if err != errConfigChanged {
			return cl, err
		}
	}
	return nil, errConfigChanged
}

// cluster runs with the connector locked. A changed input retries the whole
// resolution, including the credential reads performed by client-go.
func (c *Connector) cluster(name string) (*Cluster, error) {
	r, err := c.resolve(name)
	if err != nil {
		return nil, err
	}
	if cl, ok := c.clusters[name]; ok && cl.digest == r.digest {
		return cl, nil
	}
	// A first build for this context, or a rebuild over changed inputs: say
	// so. A rotation that rebuilds a client silently is invisible to whoever
	// is diagnosing a sudden reconnect or a TLS failure that outlived the
	// rotation — the exact scenarios the digest revalidation exists for. A
	// fingerprinted file that could not be read digested as absent, so it is
	// named here: "absent" has to be distinguishable from "rotated".
	if c.opts.Logger != nil {
		if _, ok := c.clusters[name]; ok {
			c.opts.Logger.Info("kubernetes client rebuilt: the kubeconfig inputs changed",
				"context", name, "source", string(r.source), "path", r.path)
		}
		for _, p := range r.unreadable {
			c.opts.Logger.Warn("kubeconfig input unreadable, fingerprinted as absent",
				"context", name, "path", p)
		}
	}

	cl, err := c.connect(name, r)
	// Building the transports reads CA bundles and client certificates and
	// keys. Only publish a cluster if those reads used the resolved snapshot.
	if r.rawConfig != nil {
		if after, _ := fingerprintSources(r.paths, r.rawConfig); after != r.digest {
			return nil, errConfigChanged
		}
	}
	if err != nil {
		return nil, err
	}
	c.clusters[name] = cl
	return cl, nil
}

// Invalidate drops every cached cluster so the next Cluster call re-resolves
// the configuration from the files as they now stand. An operation that may
// rewrite credentials or endpoint configuration — a CA rotation, a repoint,
// or its partial-failure and cancellation unwinding — calls it when it ends:
// whether the files changed, the operation cannot say, and only
// re-resolution can. Invalidation is one-shot, not a sticky mode: with the
// inputs stable again, reuse resumes. A cluster already handed out keeps
// working for the requests in flight against it — invalidation hands out new
// clusters, it never mutates one.
func (c *Connector) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opts.Logger != nil && len(c.clusters) > 0 {
		// An invalidation follows an operation that may have rewritten
		// credentials — a CA rotation or a repoint — and explains the
		// reconnect the next tool call makes.
		c.opts.Logger.Info("kubernetes client cache invalidated", "contexts", len(c.clusters))
	}
	c.clusters = map[string]*Cluster{}
}

// ReloadCluster rebuilds a client from the current kubeconfig and replaces
// its cached entry. It uses the same input snapshots and consistency checks
// as Cluster, so a CA rotation cannot publish a client with mixed credentials.
func (c *Connector) ReloadCluster(name string) (*Cluster, error) {
	if name == "" {
		name = c.opts.Context
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.clusters, name)
	for range configSnapshotAttempts {
		cl, err := c.cluster(name)
		if err != errConfigChanged {
			return cl, err
		}
	}
	return nil, errConfigChanged
}

func (c *Connector) connect(name string, r *resolved) (*Cluster, error) {
	cfg := rest.CopyConfig(r.rest)
	// Snapshot TLS files before constructing any clients. Client-go caches
	// transports by configuration; paths alone can reuse the previous client
	// certificate even after the connector detects a file replacement.
	// In-cluster credentials rely on client-go's native service-account CA
	// and token reloads rather than the kubeconfig fingerprint/rebuild path.
	if r.source != SourceInCluster {
		if err := rest.LoadTLSFiles(cfg); err != nil {
			return nil, fmt.Errorf("load TLS credentials: %w", err)
		}
	}
	cfg.Timeout = c.opts.timeout()
	cfg.QPS = DefaultQPS
	cfg.Burst = DefaultBurst
	if c.opts.QPS > 0 {
		cfg.QPS = c.opts.QPS
	}
	if c.opts.Burst > 0 {
		cfg.Burst = c.opts.Burst
	}

	if limit := c.opts.maxBytes(); limit > 0 {
		// WrapTransport composes, so this rides on top of whatever
		// authentication and TLS client-go has already installed rather than
		// replacing it.
		prev := cfg.WrapTransport
		cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
			if prev != nil {
				rt = prev(rt)
			}
			return &limitTransport{base: rt, limit: limit}
		}
	}

	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("typed client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}

	// Memory-cached discovery, dropped by Cluster.refreshDiscovery.
	cached := memcached.NewMemCacheClient(disco)

	// The shortcut expander is what makes "po" mean "pods".
	mapper := restmapper.NewShortcutExpander(
		restmapper.NewDeferredDiscoveryRESTMapper(cached),
		cached,
		// Deprecation warnings from discovery.
		func(msg string) {
			if c.opts.Logger != nil {
				c.opts.Logger.Warn("kubernetes api deprecation", "context", name, "warning", msg)
			}
		},
	)

	contextName := name
	if contextName == "" && r.rawConfig != nil {
		contextName = r.rawConfig.CurrentContext
	}

	return &Cluster{
		Typed:       typed,
		Dynamic:     dyn,
		Discovery:   cached,
		Mapper:      mapper,
		Config:      cfg,
		ContextName: contextName,
		Source:      r.source,
		ConfigPath:  r.path,
		Host:        cfg.Host,
		digest:      r.digest,
	}, nil
}

// ResolveResource turns a name an operator or a model would type into the
// resource to call and whether it is namespaced. A miss is retried once
// against fresh discovery.
func (c *Cluster) ResolveResource(ctx context.Context, name string) (schema.GroupVersionResource, bool, error) {
	gvr, namespaced, err := c.resolveResource(ctx, name)
	if err == nil || !c.refreshDiscovery(ctx) {
		return gvr, namespaced, err
	}
	// The mapper caches mappings of its own; only Reset clears those.
	if resettable, ok := c.Mapper.(meta.ResettableRESTMapper); ok {
		resettable.Reset()
	}
	return c.resolveResource(ctx, name)
}

func (c *Cluster) resolveResource(ctx context.Context, name string) (schema.GroupVersionResource, bool, error) {
	if name == "" {
		return schema.GroupVersionResource{}, false, fmt.Errorf("no resource named")
	}

	// ParseResourceArg understands the dotted forms; ParseKindArg is the
	// fallback for a bare or dotted Kind.
	var gvr *schema.GroupVersionResource
	var gvk *schema.GroupVersionKind
	if parsed, gr := schema.ParseResourceArg(name); parsed != nil {
		gvr = parsed
	} else {
		gvr = &schema.GroupVersionResource{Resource: gr.Resource, Group: gr.Group}
	}

	mapping, err := c.mappingForResource(ctx, *gvr)
	if err == nil {
		return mapping.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
	}

	// Not a resource — try it as a Kind.
	if parsed, gk := schema.ParseKindArg(name); parsed != nil {
		gvk = parsed
	} else {
		gvk = &schema.GroupVersionKind{Kind: gk.Kind, Group: gk.Group}
	}
	mapping, kindErr := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if kindErr != nil {
		// Report the resource error, not the kind one: "pods" is what almost
		// every caller means, so its failure is the one that explains the
		// problem. meta.IsNoMatchError covers both.
		return schema.GroupVersionResource{}, false, fmt.Errorf("unknown resource %q: %w", name, err)
	}
	return mapping.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

func (c *Cluster) mappingForResource(ctx context.Context, gvr schema.GroupVersionResource) (*meta.RESTMapping, error) {
	fullGVR, err := c.Mapper.ResourceFor(gvr)
	if err != nil {
		return nil, err
	}
	gvk, err := c.Mapper.KindFor(fullGVR)
	if err != nil {
		return nil, err
	}
	return c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
}

// APIResource describes one resource the cluster serves.
type APIResource struct {
	Group      string   `json:"group,omitempty"`
	Version    string   `json:"version"`
	Resource   string   `json:"resource"`
	Kind       string   `json:"kind"`
	Namespaced bool     `json:"namespaced"`
	ShortNames []string `json:"shortNames,omitempty"`
	Verbs      []string `json:"verbs,omitempty"`
}

// APIResources lists every resource the cluster serves, preferred version
// only.
func (c *Cluster) APIResources(ctx context.Context) ([]APIResource, error) {
	lists, err := discovery.ServerPreferredResources(c.Discovery)
	// A cluster with a broken aggregated apiserver returns both a partial list
	// and an error.
	if err != nil && len(lists) == 0 {
		return nil, fmt.Errorf("discover API resources: %w", err)
	}

	var out []APIResource
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, parseErr := schema.ParseGroupVersion(list.GroupVersion)
		if parseErr != nil {
			continue
		}
		for _, r := range list.APIResources {
			// Subresources ("pods/log", "deployments/scale") are not
			// separately listable and only add noise here.
			if strings.Contains(r.Name, "/") {
				continue
			}
			out = append(out, APIResource{
				Group:      gv.Group,
				Version:    gv.Version,
				Resource:   r.Name,
				Kind:       r.Kind,
				Namespaced: r.Namespaced,
				ShortNames: r.ShortNames,
				Verbs:      r.Verbs,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Resource < out[j].Resource
	})
	return out, nil
}

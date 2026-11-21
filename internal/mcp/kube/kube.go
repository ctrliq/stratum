// Package kube is Stratum's Kubernetes access layer: config, clients, addressing.
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

// Tuned to fail visibly rather than hang: a tool call is answered while someone waits.
const (
	DefaultTimeout = 30 * time.Second
	// client-go's 5/10 defaults are sized for a controller loop, far too low for tool calls.
	DefaultQPS   float32 = 50
	DefaultBurst int     = 100
)

// Options configure a Connector; the zero value is usable.
type Options struct {
	// Kubeconfig is an explicit path; empty means auto-detect.
	Kubeconfig string
	// KubeconfigPath selects a credential on every resolution; missing or empty is a hard error.
	KubeconfigPath func() string

	// Context selects a kubeconfig context; empty means the file's current-context.
	Context string

	// FallbackKubeconfig is tried after $KUBECONFIG and ~/.kube/config, before in-cluster.
	FallbackKubeconfig string

	Timeout time.Duration

	// MaxResponseBytes caps one response body; negative disables the cap.
	MaxResponseBytes int64

	QPS   float32
	Burst int

	// Logger receives diagnostics with no caller to return them to; never stdout.
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

// Cluster is a connected cluster: clients, discovery and RESTMapper.
type Cluster struct {
	Typed     kubernetes.Interface
	Dynamic   dynamic.Interface
	Discovery discovery.CachedDiscoveryInterfaceWithContext

	Mapper meta.RESTMapperWithContext

	Config *rest.Config

	ContextName string
	Source      Source
	ConfigPath  string
	Host        string

	// Revalidated by the connector before reuse; in-cluster digests empty.
	digest string

	refreshMu   sync.Mutex
	lastRefresh time.Time
}

// The shortest gap between two discovery refreshes, however often names miss.
const RefreshInterval = 10 * time.Second

func (c *Cluster) refreshDiscovery(ctx context.Context) bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if time.Since(c.lastRefresh) < RefreshInterval {
		return false
	}
	c.lastRefresh = time.Now()
	c.Discovery.InvalidateWithContext(ctx)
	return true
}

type ContextInfo struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace,omitempty"`
	User      string `json:"user,omitempty"`
	Current   bool   `json:"current"`
}

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
	// Random map order would read as though the cluster changed between calls.
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

// Cached per name and revalidated by digest, so a same-mtime rewrite rebuilds.
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

// Runs with the connector locked; a changed input retries the whole resolution.
func (c *Connector) cluster(name string) (*Cluster, error) {
	r, err := c.resolve(name)
	if err != nil {
		return nil, err
	}
	if cl, ok := c.clusters[name]; ok && cl.digest == r.digest {
		return cl, nil
	}
	// A silent rebuild is invisible to whoever diagnoses a reconnect or TLS failure.
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
	// Publish only if the transport credential reads used the resolved snapshot.
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

// One-shot: callers that may rewrite credentials cannot say whether they did.
func (c *Connector) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.opts.Logger != nil && len(c.clusters) > 0 {
		// Explains the reconnect the next tool call makes after a rotation or repoint.
		c.opts.Logger.Info("kubernetes client cache invalidated", "contexts", len(c.clusters))
	}
	c.clusters = map[string]*Cluster{}
}

// Uses Cluster's snapshot checks, so a rotation cannot publish mixed credentials.
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
	// Snapshot TLS files before any client: client-go caches transports by config.
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
		// WrapTransport composes, riding on the auth and TLS client-go installed.
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

	// Memory-cached discovery, dropped by refreshDiscovery.
	cached := memcached.NewMemCacheClientWithContext(discovery.ToDiscoveryInterfaceWithContext(disco))

	// The shortcut expander is what makes "po" mean "pods".
	mapper := restmapper.NewShortcutExpanderWithContext(
		restmapper.NewDeferredDiscoveryRESTMapperWithContext(cached),
		cached,
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

// ResolveResource maps a typed name; a miss is retried once against fresh discovery.
func (c *Cluster) ResolveResource(ctx context.Context, name string) (schema.GroupVersionResource, bool, error) {
	gvr, namespaced, err := c.resolveResource(ctx, name)
	if err == nil || !c.refreshDiscovery(ctx) {
		return gvr, namespaced, err
	}
	if resettable, ok := c.Mapper.(meta.ResettableRESTMapper); ok {
		resettable.Reset()
	}
	return c.resolveResource(ctx, name)
}

func (c *Cluster) resolveResource(ctx context.Context, name string) (schema.GroupVersionResource, bool, error) {
	if name == "" {
		return schema.GroupVersionResource{}, false, fmt.Errorf("no resource named")
	}

	// ParseResourceArg handles the dotted forms; a bare or dotted Kind falls through.
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

	if parsed, gk := schema.ParseKindArg(name); parsed != nil {
		gvk = parsed
	} else {
		gvk = &schema.GroupVersionKind{Kind: gk.Kind, Group: gk.Group}
	}
	mapping, kindErr := c.Mapper.RESTMappingWithContext(ctx, gvk.GroupKind(), gvk.Version)
	if kindErr != nil {
		// The resource error is the one that explains the problem.
		return schema.GroupVersionResource{}, false, fmt.Errorf("unknown resource %q: %w", name, err)
	}
	return mapping.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

func (c *Cluster) mappingForResource(ctx context.Context, gvr schema.GroupVersionResource) (*meta.RESTMapping, error) {
	fullGVR, err := c.Mapper.ResourceForWithContext(ctx, gvr)
	if err != nil {
		return nil, err
	}
	gvk, err := c.Mapper.KindForWithContext(ctx, fullGVR)
	if err != nil {
		return nil, err
	}
	return c.Mapper.RESTMappingWithContext(ctx, gvk.GroupKind(), gvk.Version)
}

type APIResource struct {
	Group      string   `json:"group,omitempty"`
	Version    string   `json:"version"`
	Resource   string   `json:"resource"`
	Kind       string   `json:"kind"`
	Namespaced bool     `json:"namespaced"`
	ShortNames []string `json:"shortNames,omitempty"`
	Verbs      []string `json:"verbs,omitempty"`
}

// APIResources lists the resources the cluster serves, preferred version only.
func (c *Cluster) APIResources(ctx context.Context) ([]APIResource, error) {
	lists, err := discovery.ServerPreferredResourcesWithContext(ctx, c.Discovery)
	// A broken aggregated apiserver returns both a partial list and an error.
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
			// Subresources are not separately listable and only add noise.
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

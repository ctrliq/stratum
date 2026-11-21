package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type Source string

const (
	SourceExplicit Source = "explicit"
	SourceEnv      Source = "KUBECONFIG"
	SourceHome     Source = "home"
	// For `stratum mcp`, the node's own admin.conf.
	SourceFallback  Source = "fallback"
	SourceInCluster Source = "in-cluster"
)

// ErrNoConfig is returned when every source in the precedence order is empty.
var ErrNoConfig = errors.New("no kubernetes configuration found")

// A rotation can replace inputs between independent reads; retry, never cache a mix.
var errConfigChanged = errors.New("kubernetes configuration changed while loading; retry the request")

const configSnapshotAttempts = 3

// rawConfig is nil for in-cluster; unreadable names files that digested as absent.
type resolved struct {
	rest       *rest.Config
	source     Source
	path       string
	rawConfig  *clientcmdapi.Config
	digest     string
	unreadable []string
	paths      []string
}

func resolve(explicit, fallback, overrideContext string) (*resolved, error) {
	for range configSnapshotAttempts {
		r, err := resolveOnce(explicit, fallback, overrideContext)
		if !errors.Is(err, errConfigChanged) {
			return r, err
		}
	}
	return nil, errConfigChanged
}

func resolveOnce(explicit, fallback, overrideContext string) (*resolved, error) {
	if explicit != "" {
		r, err := fromFiles([]string{explicit}, SourceExplicit, overrideContext)
		if err != nil {
			return nil, fmt.Errorf("kubeconfig %s: %w", explicit, err)
		}
		return r, nil
	}

	if env := os.Getenv(clientcmd.RecommendedConfigPathEnvVar); env != "" {
		paths := filepath.SplitList(env)
		r, err := fromFiles(paths, SourceEnv, overrideContext)
		if err != nil {
			return nil, fmt.Errorf("$%s: %w", clientcmd.RecommendedConfigPathEnvVar, err)
		}
		return r, nil
	}

	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName)
		if _, err := os.Stat(path); err == nil {
			r, err := fromFiles([]string{path}, SourceHome, overrideContext)
			if err != nil {
				return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
			}
			return r, nil
		}
	}

	if fallback != "" {
		if _, err := os.Stat(fallback); err == nil {
			r, err := fromFiles([]string{fallback}, SourceFallback, overrideContext)
			if err != nil {
				return nil, fmt.Errorf("kubeconfig %s: %w", fallback, err)
			}
			return r, nil
		}
	}

	// ErrNotInCluster here is "nothing configured", not a failure worth surfacing.
	cfg, err := rest.InClusterConfig()
	switch {
	case errors.Is(err, rest.ErrNotInCluster):
		return nil, ErrNoConfig
	case err != nil:
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return &resolved{rest: cfg, source: SourceInCluster}, nil
}

func fromFiles(paths []string, source Source, overrideContext string) (*resolved, error) {
	sourcesBefore, _ := fingerprintSources(paths, nil)
	rules := &clientcmd.ClientConfigLoadingRules{Precedence: paths}
	if source == SourceExplicit {
		rules.ExplicitPath = paths[0]
	}

	raw, err := rules.Load()
	sourcesAfter, digest, unreadable := fingerprintInputs(paths, raw)
	if sourcesBefore != sourcesAfter {
		return nil, errConfigChanged
	}
	if err != nil {
		return nil, err
	}
	// Load yields an empty config, not an error, for a missing Precedence file.
	if len(raw.Clusters) == 0 {
		return nil, ErrNoConfig
	}

	overrides := &clientcmd.ConfigOverrides{CurrentContext: overrideContext}
	clientConfig := clientcmd.NewNonInteractiveClientConfig(*raw, overrideContext, overrides, rules)

	restCfg, err := clientConfig.ClientConfig()
	// ClientConfig reads token files itself; the digest must match the bytes used.
	if after, _ := fingerprintSources(paths, raw); after != digest {
		return nil, errConfigChanged
	}
	if err != nil {
		return nil, err
	}

	return &resolved{
		rest:       restCfg,
		source:     source,
		path:       firstExisting(paths),
		rawConfig:  raw,
		digest:     digest,
		unreadable: unreadable,
		paths:      paths,
	}, nil
}

// $KUBECONFIG may list several paths and only some exist.
func firstExisting(paths []string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if len(paths) > 0 {
		return paths[0]
	}
	return ""
}

// The digest is over content, not mtime: a staged copy can land with the old mtime.
func fingerprintSources(paths []string, raw *clientcmdapi.Config) (string, []string) {
	_, digest, unreadable := fingerprintInputs(paths, raw)
	return digest, unreadable
}

// The source-only digest lets a caller verify both snapshots share the same inputs.
func fingerprintInputs(paths []string, raw *clientcmdapi.Config) (sources, digest string, unreadable []string) {
	h := sha256.New()
	digestOne := func(p string) {
		if err := digestFile(h, p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			unreadable = append(unreadable, p)
		}
	}
	for _, p := range paths {
		digestOne(p)
	}
	sources = hex.EncodeToString(h.Sum(nil))
	for _, p := range referencedCredentialFiles(raw, paths) {
		digestOne(p)
	}
	return sources, hex.EncodeToString(h.Sum(nil)), unreadable
}

// The path is mixed in ahead of the content, so a reordered list digests differently.
func digestFile(h io.Writer, path string) error {
	h.Write([]byte(path))
	h.Write([]byte{0})
	content, err := os.ReadFile(path)
	if err != nil {
		h.Write([]byte("absent"))
	} else {
		h.Write(content)
	}
	h.Write([]byte{0})
	return err
}

// Embedded credentials ride the kubeconfig digest; a relative ref resolves against its file.
func referencedCredentialFiles(raw *clientcmdapi.Config, sources []string) []string {
	if raw == nil {
		return nil
	}
	fallback := firstExisting(sources)
	seen := map[string]bool{}
	var out []string
	add := func(fromLocation, ref string) {
		if ref == "" {
			return
		}
		base := fromLocation
		if base == "" {
			base = fallback
		}
		if !filepath.IsAbs(ref) && base != "" {
			ref = filepath.Join(filepath.Dir(base), ref)
		}
		if !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	for _, cluster := range raw.Clusters {
		add(cluster.LocationOfOrigin, cluster.CertificateAuthority)
	}
	for _, auth := range raw.AuthInfos {
		add(auth.LocationOfOrigin, auth.ClientCertificate)
		add(auth.LocationOfOrigin, auth.ClientKey)
		add(auth.LocationOfOrigin, auth.TokenFile)
	}
	sort.Strings(out)
	return out
}

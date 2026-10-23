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

// Source says where a resolved configuration came from.
type Source string

const (
	// SourceExplicit is a kubeconfig path the caller named outright.
	SourceExplicit Source = "explicit"
	// SourceEnv is $KUBECONFIG.
	SourceEnv Source = "KUBECONFIG"
	// SourceHome is ~/.kube/config.
	SourceHome Source = "home"
	// SourceFallback is a path the host supplied as a last resort before
	// in-cluster — for `stratum mcp`, the node's own admin.conf.
	SourceFallback Source = "fallback"
	// SourceInCluster is the mounted ServiceAccount.
	SourceInCluster Source = "in-cluster"
)

// ErrNoConfig is returned when every source in the precedence order came up
// empty.
var ErrNoConfig = errors.New("no kubernetes configuration found")

// A rotation may replace inputs between the loader's independent reads.
// Retry a bounded number of times rather than caching a mixed snapshot.
var errConfigChanged = errors.New("kubernetes configuration changed while loading; retry the request")

const configSnapshotAttempts = 3

// resolved is a client configuration plus the provenance and the loaded
// kubeconfig it came from. rawConfig is nil for in-cluster, which has no
// contexts to choose between. unreadable names the fingerprinted files that
// could not be read — they digest as absent, and the caller that owns a
// logger names them so a rotation that fails mid-copy is distinguishable from
// a file that never existed.
type resolved struct {
	rest       *rest.Config
	source     Source
	path       string
	rawConfig  *clientcmdapi.Config
	digest     string
	unreadable []string
	paths      []string
}

// resolve finds a client configuration, in this precedence:
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
		// An explicit path that does not load is a hard error, never a fallthrough.
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

	// A host-supplied fallback, before in-cluster.
	if fallback != "" {
		if _, err := os.Stat(fallback); err == nil {
			r, err := fromFiles([]string{fallback}, SourceFallback, overrideContext)
			if err != nil {
				return nil, fmt.Errorf("kubeconfig %s: %w", fallback, err)
			}
			return r, nil
		}
	}

	// In-cluster last. rest.ErrNotInCluster means the ServiceAccount env and
	// mount are absent, which here is "nothing configured" rather than a
	// failure worth surfacing on its own.
	cfg, err := rest.InClusterConfig()
	switch {
	case errors.Is(err, rest.ErrNotInCluster):
		return nil, ErrNoConfig
	case err != nil:
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return &resolved{rest: cfg, source: SourceInCluster}, nil
}

// fromFiles loads a kubeconfig from a precedence list of paths and builds
// the rest.Config for the selected context.
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
	// Load() is forgiving: a missing file in Precedence yields an empty
	// config rather than an error, which would otherwise surface much later
	// as an unhelpful "no server defined".
	if len(raw.Clusters) == 0 {
		return nil, ErrNoConfig
	}

	overrides := &clientcmd.ConfigOverrides{CurrentContext: overrideContext}
	clientConfig := clientcmd.NewNonInteractiveClientConfig(*raw, overrideContext, overrides, rules)

	restCfg, err := clientConfig.ClientConfig()
	// ClientConfig reads token files separately from the kubeconfig loader.
	// The digest must describe the bytes used, not a later replacement.
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

// firstExisting reports the path a load actually came from, for diagnostics.
// $KUBECONFIG may list several and only some may exist.
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

// fingerprintSources returns a content digest over every file the resolution
// depends on: each kubeconfig source in precedence order, then every
// credential file the loaded config references by path. Merging a $KUBECONFIG
// list makes several files one input, so the digest covers the whole list,
// in order, and not just the file that happened to win a conflict. The digest
// is over content, never mtime: a rotation that copies a staged file over its
// target can land the rewrite with the old timestamp still on it, and a
// stat-based check would keep serving the stale credential. A file that is
// absent hashes as absent, so one that appears or disappears also changes the
// digest.
func fingerprintSources(paths []string, raw *clientcmdapi.Config) (string, []string) {
	_, digest, unreadable := fingerprintInputs(paths, raw)
	return digest, unreadable
}

// Keep the source-only digest as well, so the caller can verify that the
// kubeconfig loaded between the two snapshots belongs to the same inputs.
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

// digestFile folds one file into the fingerprint. The path is mixed in ahead
// of the content so a reordered source list digests differently even when two
// entries hold identical content. A file that cannot be read hashes as absent
// rather than failing the resolution it only guards, and the read error comes
// back so the caller can name the file instead of leaving "absent"
// indistinguishable from a rotation that failed mid-copy.
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

// referencedCredentialFiles lists the credential files the loaded config
// names by path: certificate-authority bundles, client certificates and
// their keys, and token files. Credentials embedded in the kubeconfig itself
// (the …-data fields and literal tokens) need no entry here — they are part
// of the kubeconfig's own content and its digest already covers them. A
// relative reference resolves against the kubeconfig file that names it, the
// way clientcmd resolves one at load time. The list is sorted and
// deduplicated so it does not depend on map iteration order.
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

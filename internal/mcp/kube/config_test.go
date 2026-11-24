package kube

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/mcp/kube/kubetest"
)

func writeKubeconfig(t *testing.T, dir, name, server string, contexts ...string) string {
	t.Helper()
	if len(contexts) == 0 {
		contexts = []string{"default"}
	}

	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\nclusters:\n")
	fmt.Fprintf(&b, "- name: c\n  cluster:\n    server: %s\n", server)
	b.WriteString("users:\n- name: u\n  user:\n    token: t\n")
	b.WriteString("contexts:\n")
	for _, ctx := range contexts {
		fmt.Fprintf(&b, "- name: %s\n  context:\n    cluster: c\n    user: u\n    namespace: ns-%s\n", ctx, ctx)
	}
	fmt.Fprintf(&b, "current-context: %s\n", contexts[0])

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Shared with the protocol tests in pkg/cli.
func isolateHome(t *testing.T) string { return kubetest.IsolateHome(t) }

func TestSelectedKubeconfigNeverFallsBack(t *testing.T) {
	for _, selected := range []string{"", "missing.conf"} {
		t.Run(selected, func(t *testing.T) {
			home := isolateHome(t)
			remote := writeKubeconfig(t, home, ".kube/config", "https://remote.example")
			t.Setenv("KUBECONFIG", remote)
			path := selected
			if path != "" {
				path = filepath.Join(t.TempDir(), path)
			}
			conn := NewConnector(Options{
				Kubeconfig: remote, FallbackKubeconfig: remote,
				KubeconfigPath: func() string { return path },
			})
			if _, err := conn.Cluster(""); err == nil {
				t.Fatal("missing selected credential fell through to a remote source")
			}
			if _, err := conn.Contexts(); err == nil {
				t.Fatal("context listing fell through to a remote source")
			}
		})
	}
}

func TestResolvePrecedence(t *testing.T) {
	t.Run("explicit beats everything", func(t *testing.T) {
		home := isolateHome(t)
		writeKubeconfig(t, home, ".kube/config", "https://home.example")
		envPath := writeKubeconfig(t, t.TempDir(), "env.yaml", "https://env.example")
		t.Setenv("KUBECONFIG", envPath)
		explicit := writeKubeconfig(t, t.TempDir(), "explicit.yaml", "https://explicit.example")

		got, err := resolve(explicit, "", "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.rest.Host != "https://explicit.example" {
			t.Errorf("host = %q, want the explicit file's", got.rest.Host)
		}
		if got.source != SourceExplicit {
			t.Errorf("source = %q, want %q", got.source, SourceExplicit)
		}
	})

	t.Run("KUBECONFIG beats home", func(t *testing.T) {
		home := isolateHome(t)
		writeKubeconfig(t, home, ".kube/config", "https://home.example")
		envPath := writeKubeconfig(t, t.TempDir(), "env.yaml", "https://env.example")
		t.Setenv("KUBECONFIG", envPath)

		got, err := resolve("", "", "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.rest.Host != "https://env.example" {
			t.Errorf("host = %q, want the $KUBECONFIG file's", got.rest.Host)
		}
		if got.source != SourceEnv {
			t.Errorf("source = %q, want %q", got.source, SourceEnv)
		}
	})

	t.Run("home when no env", func(t *testing.T) {
		home := isolateHome(t)
		writeKubeconfig(t, home, ".kube/config", "https://home.example")

		got, err := resolve("", "", "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.rest.Host != "https://home.example" {
			t.Errorf("host = %q, want ~/.kube/config's", got.rest.Host)
		}
		if got.source != SourceHome {
			t.Errorf("source = %q, want %q", got.source, SourceHome)
		}
	})

	// With no kubeconfig and no ServiceAccount, resolve reports "nothing configured".
	t.Run("nothing configured", func(t *testing.T) {
		isolateHome(t)
		// rest.InClusterConfig keys off these two.
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
		t.Setenv("KUBERNETES_SERVICE_PORT", "")

		_, err := resolve("", "", "")
		if !errors.Is(err, ErrNoConfig) {
			t.Fatalf("err = %v, want ErrNoConfig", err)
		}
	})
}

func TestExplicitMissingFileDoesNotFallThrough(t *testing.T) {
	home := isolateHome(t)
	writeKubeconfig(t, home, ".kube/config", "https://home.example")

	missing := filepath.Join(t.TempDir(), "absent.yaml")
	got, err := resolve(missing, "", "")
	if err == nil {
		t.Fatalf("resolve succeeded with host %q; want an error naming the missing file", got.rest.Host)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the missing file", err)
	}
}

func TestContextSelection(t *testing.T) {
	home := isolateHome(t)
	path := writeKubeconfig(t, home, ".kube/config", "https://home.example", "alpha", "beta")

	t.Run("override picks a non-current context", func(t *testing.T) {
		got, err := resolve(path, "", "beta")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.rawConfig.CurrentContext != "alpha" {
			t.Fatalf("fixture current-context = %q, want alpha", got.rawConfig.CurrentContext)
		}
	})

	t.Run("unknown context is an error", func(t *testing.T) {
		if _, err := resolve(path, "", "nope"); err == nil {
			t.Error("resolve accepted a context that is not in the file")
		}
	})

	t.Run("Contexts lists them, sorted, with current marked", func(t *testing.T) {
		c := NewConnector(Options{Kubeconfig: path})
		got, err := c.Contexts()
		if err != nil {
			t.Fatalf("Contexts: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d contexts, want 2: %+v", len(got), got)
		}
		if got[0].Name != "alpha" || got[1].Name != "beta" {
			t.Errorf("not sorted by name: %+v", got)
		}
		if !got[0].Current || got[1].Current {
			t.Errorf("current-context not marked on alpha alone: %+v", got)
		}
		if got[0].Namespace != "ns-alpha" {
			t.Errorf("namespace = %q, want ns-alpha", got[0].Namespace)
		}
	})
}

// Building a cluster contacts no API server, so this pin runs where envtest is skipped.
func TestCluster_ReusedWhileItsKubeconfigIsUnchanged(t *testing.T) {
	isolateHome(t)
	const server = "https://192.0.2.1:6443" // TEST-NET-1: unreachable by design
	path := writeKubeconfig(t, t.TempDir(), "cached.yaml", server, "a", "b")

	conn := NewConnector(Options{Kubeconfig: path})
	first, err := conn.Cluster("a")
	if err != nil {
		t.Fatalf("Cluster(a): %v", err)
	}
	again, err := conn.Cluster("a")
	if err != nil {
		t.Fatalf("second Cluster(a): %v", err)
	}
	if first != again {
		t.Error("the same context was rebuilt on its second call instead of being reused")
	}
	other, err := conn.Cluster("b")
	if err != nil {
		t.Fatalf("Cluster(b): %v", err)
	}
	if other == first {
		t.Error("contexts a and b share one cached cluster")
	}
	if first.Host != server {
		t.Errorf("Host = %q, want the kubeconfig's server %q", first.Host, server)
	}
}

// Stable reuse logs nothing, so a rebuild line in the log always means something.
func TestCacheEventsAreLogged(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	path := writeKubeconfig(t, dir, "logged.yaml", "https://192.0.2.1:6443", "a")

	var buf bytes.Buffer
	conn := NewConnector(Options{Kubeconfig: path, Logger: slog.New(slog.NewTextHandler(&buf, nil))})

	if _, err := conn.Cluster("a"); err != nil {
		t.Fatalf("Cluster(a): %v", err)
	}
	if _, err := conn.Cluster("a"); err != nil {
		t.Fatalf("second Cluster(a): %v", err)
	}
	if strings.Contains(buf.String(), "rebuilt") {
		t.Errorf("stable reuse logged a rebuild: %s", buf.String())
	}

	writeKubeconfig(t, dir, "logged.yaml", "https://192.0.2.2:6443", "a")
	if _, err := conn.Cluster("a"); err != nil {
		t.Fatalf("Cluster(a) after the rewrite: %v", err)
	}
	if !strings.Contains(buf.String(), "kubernetes client rebuilt") {
		t.Errorf("a rebuild left no trace in the diagnostics log: %s", buf.String())
	}

	conn.Invalidate()
	if !strings.Contains(buf.String(), "kubernetes client cache invalidated") {
		t.Errorf("Invalidate left no trace in the diagnostics log: %s", buf.String())
	}
}

// The fingerprint covers every merged entry; the load validates only the selected context.
func TestFingerprintNamesUnreadableFiles(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	// Reading a directory always fails with a non-NotExist error.
	tokenSlot := filepath.Join(dir, "tokens")
	if err := os.Mkdir(tokenSlot, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config")
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://192.0.2.1:6443
users:
- name: u
  user:
    token: t
- name: stale
  user:
    tokenFile: %s
contexts:
- name: a
  context:
    cluster: c
    user: u
- name: b
  context:
    cluster: c
    user: stale
current-context: a
`, tokenSlot)
	if err := os.WriteFile(cfg, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}

	r, err := fromFiles([]string{cfg}, SourceExplicit, "")
	if err != nil {
		t.Fatalf("fromFiles: %v", err)
	}
	if len(r.unreadable) != 1 || r.unreadable[0] != tokenSlot {
		t.Errorf("resolved.unreadable = %v, want exactly the unreadable token file %s", r.unreadable, tokenSlot)
	}
}

// An unreadable file digests as absent, but the read error still names it.
func TestDigestFileReportsUnreadable(t *testing.T) {
	dir := t.TempDir()
	slot := filepath.Join(dir, "slot")
	if err := os.Mkdir(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	if err := digestFile(h, slot); err == nil {
		t.Fatal("digestFile read a directory without an error")
	}
}

// Honouring only the first entry silently ignores the rest of the configuration.
func TestKubeconfigEnvIsAPathList(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	first := writeKubeconfig(t, dir, "first.yaml", "https://first.example", "alpha")
	second := writeKubeconfig(t, dir, "second.yaml", "https://second.example", "beta")
	t.Setenv("KUBECONFIG", first+string(os.PathListSeparator)+second)

	got, err := resolve("", "", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got.rawConfig.Contexts) != 2 {
		t.Errorf("merged config has %d contexts, want both files' 2: %v",
			len(got.rawConfig.Contexts), got.rawConfig.Contexts)
	}
}

// A control plane has no $KUBECONFIG, no ~/.kube/config, and is not a pod.
func TestFallbackKubeconfig(t *testing.T) {
	t.Run("used when nothing else is configured", func(t *testing.T) {
		isolateHome(t)
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
		t.Setenv("KUBERNETES_SERVICE_PORT", "")
		fallback := writeKubeconfig(t, t.TempDir(), "admin.conf", "https://node.example")

		got, err := resolve("", fallback, "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.rest.Host != "https://node.example" {
			t.Errorf("host = %q, want the fallback's", got.rest.Host)
		}
		if got.source != SourceFallback {
			t.Errorf("source = %q, want %q", got.source, SourceFallback)
		}
	})

	// After the operator's own configuration: if they set $KUBECONFIG, that is the cluster they mean.
	t.Run("loses to $KUBECONFIG", func(t *testing.T) {
		isolateHome(t)
		env := writeKubeconfig(t, t.TempDir(), "env.yaml", "https://env.example")
		t.Setenv("KUBECONFIG", env)
		fallback := writeKubeconfig(t, t.TempDir(), "admin.conf", "https://node.example")

		got, err := resolve("", fallback, "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.rest.Host != "https://env.example" {
			t.Errorf("host = %q, want $KUBECONFIG's", got.rest.Host)
		}
	})

	t.Run("loses to ~/.kube/config", func(t *testing.T) {
		home := isolateHome(t)
		writeKubeconfig(t, home, ".kube/config", "https://home.example")
		fallback := writeKubeconfig(t, t.TempDir(), "admin.conf", "https://node.example")

		got, err := resolve("", fallback, "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.rest.Host != "https://home.example" {
			t.Errorf("host = %q, want ~/.kube/config's", got.rest.Host)
		}
	})

	// An absent fallback is normal off a node and must fall through to in-cluster.
	t.Run("an absent fallback falls through", func(t *testing.T) {
		isolateHome(t)
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
		t.Setenv("KUBERNETES_SERVICE_PORT", "")

		_, err := resolve("", filepath.Join(t.TempDir(), "absent.conf"), "")
		if !errors.Is(err, ErrNoConfig) {
			t.Fatalf("err = %v, want ErrNoConfig", err)
		}
	})
}

func kubeconfigWithServer(server string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: %s
users:
- name: u
  user:
    token: t
contexts:
- name: default
  context:
    cluster: c
    user: u
    namespace: ns-default
current-context: default
`, server)
}

func kubeconfigYAMLFor(cluster, server, contextName string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %s
  cluster:
    server: %s
users:
- name: u
  user:
    token: t
contexts:
- name: %s
  context:
    cluster: %s
    user: u
current-context: %s
`, cluster, server, contextName, cluster, contextName)
}

func writeNamedKubeconfig(t *testing.T, path, cluster, server, contextName string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(kubeconfigYAMLFor(cluster, server, contextName)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The form an operator's managed-cluster kubeconfig takes: credentials by path.
func writeReferencingKubeconfig(t *testing.T, dir, name, server, caPath, certPath, keyPath, tokenPath string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n")
	fmt.Fprintf(&b, "    server: %s\n", server)
	if caPath != "" {
		fmt.Fprintf(&b, "    certificate-authority: %s\n", caPath)
	}
	b.WriteString("users:\n- name: u\n  user:\n")
	if certPath != "" {
		fmt.Fprintf(&b, "    client-certificate: %s\n    client-key: %s\n", certPath, keyPath)
	}
	if tokenPath != "" {
		fmt.Fprintf(&b, "    tokenFile: %s\n", tokenPath)
	}
	b.WriteString("contexts:\n- name: default\n  context:\n    cluster: c\n    user: u\n")
	b.WriteString("current-context: default\n")

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Only loadability matters: no API server is contacted, nothing must trust them.
func writeCertPair(t *testing.T, dir, name string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	if err := os.WriteFile(certPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A rotation can land with the old mtime still on it.
func rewriteSameMtime(t *testing.T, path, content string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("rewrite did not preserve mtime: before %v, after %v",
			before.ModTime(), after.ModTime())
	}
}

// The negative control: an mtime move over unchanged content.
func rewriteWithLaterMtime(t *testing.T, path, content string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	later := before.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

// Same-mtime rewrite must rebuild; an mtime move over unchanged content must not.
func TestCluster_RebuiltWhenItsKubeconfigIsRewrittenWithTheSameMtime(t *testing.T) {
	isolateHome(t)
	const server = "https://192.0.2.1:6443" // TEST-NET-1: unreachable by design
	path := writeKubeconfig(t, t.TempDir(), "admin.conf", server)

	conn := NewConnector(Options{Kubeconfig: path})
	first, err := conn.Cluster("")
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if first.Host != server {
		t.Fatalf("Host = %q, want the kubeconfig's server %q", first.Host, server)
	}

	const rewritten = "https://192.0.2.2:6443" // TEST-NET-1 as well
	rewriteSameMtime(t, path, kubeconfigWithServer(rewritten))
	again, err := conn.Cluster("")
	if err != nil {
		t.Fatalf("Cluster after the rewrite: %v", err)
	}
	if again == first {
		t.Error("a same-mtime rewrite of the kubeconfig kept serving the cached cluster")
	}
	if again.Host != rewritten {
		t.Errorf("Host = %q, want the rewritten server %q", again.Host, rewritten)
	}

	// Moving mtime without changing content must not throw away a working client.
	rewriteWithLaterMtime(t, path, kubeconfigWithServer(rewritten))
	touched, err := conn.Cluster("")
	if err != nil {
		t.Fatalf("Cluster after the touch: %v", err)
	}
	if touched != again {
		t.Error("a mtime move with unchanged content rebuilt the cluster")
	}
}

// Order decides which file wins a merge, so a swap must be visible to the fingerprint.
func TestCluster_RebuiltWhenKUBECONFIGPrecedenceSwaps(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	sep := string(os.PathListSeparator)
	first := writeKubeconfig(t, dir, "first.yaml", "https://first.example", "shared")
	second := writeKubeconfig(t, dir, "second.yaml", "https://second.example", "shared")

	t.Setenv("KUBECONFIG", first+sep+second)
	conn := NewConnector(Options{})
	won, err := conn.Cluster("shared")
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if won.Host != "https://first.example" {
		t.Fatalf("Host = %q, want the first file's server — the first file wins the merge", won.Host)
	}

	t.Setenv("KUBECONFIG", second+sep+first)
	swapped, err := conn.Cluster("shared")
	if err != nil {
		t.Fatalf("Cluster after the swap: %v", err)
	}
	if swapped == won {
		t.Error("a swapped $KUBECONFIG precedence kept serving the cached cluster")
	}
	if swapped.Host != "https://second.example" {
		t.Errorf("Host = %q, want the now-first file's server", swapped.Host)
	}

	// Each file names its own cluster: merging is per map key, so same names collide.
	t.Run("a rewrite of a later list entry rebuilds", func(t *testing.T) {
		isolateHome(t)
		dir := t.TempDir()
		head := writeNamedKubeconfig(t, filepath.Join(dir, "head.yaml"), "head-cluster", "https://head.example", "alpha")
		tail := writeNamedKubeconfig(t, filepath.Join(dir, "tail.yaml"), "tail-cluster", "https://tail.example", "beta")
		t.Setenv("KUBECONFIG", head+sep+tail)

		conn := NewConnector(Options{})
		before, err := conn.Cluster("beta")
		if err != nil {
			t.Fatalf("Cluster(beta): %v", err)
		}
		if before.Host != "https://tail.example" {
			t.Fatalf("Host = %q, want the second file's server", before.Host)
		}

		rewriteSameMtime(t, tail, kubeconfigYAMLFor("tail-cluster", "https://rewritten.example", "beta"))
		after, err := conn.Cluster("beta")
		if err != nil {
			t.Fatalf("Cluster(beta) after the rewrite: %v", err)
		}
		if after == before {
			t.Error("a same-mtime rewrite of the second $KUBECONFIG file kept serving the cached cluster")
		}
		if after.Host != "https://rewritten.example" {
			t.Errorf("Host = %q, want the rewritten file's server", after.Host)
		}
	})
}

// On a node the fallback is the production source, so it joins the fingerprint.
func TestCluster_RebuiltWhenTheFallbackKubeconfigChanges(t *testing.T) {
	isolateHome(t)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	fallback := writeKubeconfig(t, t.TempDir(), "admin.conf", "https://node.example")

	conn := NewConnector(Options{FallbackKubeconfig: fallback})
	first, err := conn.Cluster("")
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if first.Host != "https://node.example" {
		t.Fatalf("Host = %q, want the fallback's server", first.Host)
	}
	if first.Source != SourceFallback {
		t.Fatalf("Source = %q, want %q", first.Source, SourceFallback)
	}

	rewriteSameMtime(t, fallback, kubeconfigWithServer("https://repointed.example"))
	again, err := conn.Cluster("")
	if err != nil {
		t.Fatalf("Cluster after the rewrite: %v", err)
	}
	if again == first {
		t.Error("a same-mtime rewrite of the fallback kubeconfig kept serving the cached cluster")
	}
	if again.Host != "https://repointed.example" {
		t.Errorf("Host = %q, want the rewritten fallback's server", again.Host)
	}
}

// A rotation rewrites credential files while the kubeconfig itself does not change.
func TestCluster_RebuiltWhenACredentialFileChanges(t *testing.T) {
	const server = "https://192.0.2.1:6443" // TEST-NET-1: unreachable by design

	t.Run("tokenFile", func(t *testing.T) {
		isolateHome(t)
		token := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(token, []byte("token-one"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := writeReferencingKubeconfig(t, t.TempDir(), "ref.yaml", server, "", "", "", token)

		conn := NewConnector(Options{Kubeconfig: path})
		first, err := conn.Cluster("")
		if err != nil {
			t.Fatalf("Cluster: %v", err)
		}
		if first.Config.BearerToken != "token-one" {
			t.Fatalf("BearerToken = %q, want the token file's content", first.Config.BearerToken)
		}

		rewriteSameMtime(t, token, "token-two")
		again, err := conn.Cluster("")
		if err != nil {
			t.Fatalf("Cluster after the token rewrite: %v", err)
		}
		if again == first {
			t.Error("a same-mtime rewrite of the referenced tokenFile kept serving the cached cluster")
		}
		if again.Config.BearerToken != "token-two" {
			t.Errorf("BearerToken = %q, want the rewritten token", again.Config.BearerToken)
		}
	})

	t.Run("client certificate and key", func(t *testing.T) {
		isolateHome(t)
		dir := t.TempDir()
		certA, keyA := writeCertPair(t, dir, "client-a")
		certB, keyB := writeCertPair(t, dir, "client-b")
		path := writeReferencingKubeconfig(t, dir, "ref.yaml", server, "", certA, keyA, "")

		conn := NewConnector(Options{Kubeconfig: path})
		first, err := conn.Cluster("")
		if err != nil {
			t.Fatalf("Cluster: %v", err)
		}

		rewriteSameMtime(t, certA, mustRead(t, certB))
		rewriteSameMtime(t, keyA, mustRead(t, keyB))
		again, err := conn.Cluster("")
		if err != nil {
			t.Fatalf("Cluster after the credential rewrite: %v", err)
		}
		if again == first {
			t.Error("a same-mtime rewrite of the referenced client certificate and key kept serving the cached cluster")
		}
	})

	t.Run("certificate-authority", func(t *testing.T) {
		isolateHome(t)
		dir := t.TempDir()
		caA, _ := writeCertPair(t, dir, "ca-a")
		caB, _ := writeCertPair(t, dir, "ca-b")
		path := writeReferencingKubeconfig(t, dir, "ref.yaml", server, caA, "", "", "")

		conn := NewConnector(Options{Kubeconfig: path})
		first, err := conn.Cluster("")
		if err != nil {
			t.Fatalf("Cluster: %v", err)
		}

		rewriteSameMtime(t, caA, mustRead(t, caB))
		again, err := conn.Cluster("")
		if err != nil {
			t.Fatalf("Cluster after the CA rewrite: %v", err)
		}
		if again == first {
			t.Error("a same-mtime rewrite of the referenced certificate-authority file kept serving the cached cluster")
		}
	})
}

// The fingerprint alone cannot carry this: a cancelled operation leaves files untouched.
func TestCluster_InvalidatedAfterACancelledOperation(t *testing.T) {
	isolateHome(t)
	const server = "https://192.0.2.1:6443" // TEST-NET-1: unreachable by design
	path := writeKubeconfig(t, t.TempDir(), "admin.conf", server)

	conn := NewConnector(Options{Kubeconfig: path})
	captured, err := conn.Cluster("")
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if ctx.Err() == nil {
		t.Fatal("the operation's context did not cancel")
	}
	conn.Invalidate()

	fresh, err := conn.Cluster("")
	if err != nil {
		t.Fatalf("Cluster after invalidation: %v", err)
	}
	if fresh == captured {
		t.Error("invalidation after a cancelled operation left the captured cluster in the cache")
	}

	// Invalidation is one-shot: stable inputs resume reuse.
	reused, err := conn.Cluster("")
	if err != nil {
		t.Fatalf("second Cluster after invalidation: %v", err)
	}
	if reused != fresh {
		t.Error("stable inputs were not reused after the post-invalidation rebuild")
	}

	// Again with the mtime preserved.
	const repointed = "https://192.0.2.2:6443" // TEST-NET-1 as well
	rewriteSameMtime(t, path, kubeconfigWithServer(repointed))
	after, err := conn.Cluster("")
	if err != nil {
		t.Fatalf("Cluster after the rewrite: %v", err)
	}
	if after.Host != repointed {
		t.Errorf("Host = %q, want the rewritten server %q", after.Host, repointed)
	}

	if captured.Host != server {
		t.Errorf("the captured cluster's Host changed to %q", captured.Host)
	}
	if captured.Config.Host != server {
		t.Errorf("the captured cluster's config Host changed to %q", captured.Config.Host)
	}
	if captured.Typed == nil || captured.Dynamic == nil || captured.Discovery == nil || captured.Mapper == nil {
		t.Error("the captured cluster's clients were dropped by the invalidation")
	}
}

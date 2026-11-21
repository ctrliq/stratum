// Package bootstrap fetches the kube-public cluster-info ConfigMap for join and rotation consumers.
package bootstrap

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
	"github.com/ctrliq/stratum/internal/version"
)

// 1 MiB is far above any legitimate cluster-info; it bounds a hostile endpoint, not a real one.
const maxClusterInfoBytes = 1 << 20

// The only scheme check on the join path: the token must not go out in cleartext.
func requireHTTPS(server string) error {
	u, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("parse --server %q: %w", server, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("--server must be an https:// URL; %q would send the bootstrap token in cleartext", server)
	}
	return nil
}

// Vars, not consts, only so tests can shorten them; production never reassigns.
var (
	signatureWaitInterval = 2 * time.Second
	signatureWaitTimeout  = 30 * time.Second
)

var (
	reachWaitInterval = 2 * time.Second
	reachWaitTimeout  = 2 * time.Minute
)

// Var so tests can shorten it.
var fetchTimeout = 15 * time.Second

// Indirected for tests.
var fetchClusterInfo = fetchClusterInfoData

// A non-200 status, an oversized body or a malformed response is an answer, not a dial failure.
func transientDialError(err error) bool {
	for _, errno := range []syscall.Errno{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return isTimeout(err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// No flag or warn-and-continue: a gated fallback is typed by the operator the attack targets.
var unsignedClusterInfoRemedy = `
The control plane is not publishing a cluster-info signature, so this join cannot
verify that the CA it was handed is genuine, and stratum will not install an
unverified CA.

If this cluster was installed by a build that does not run the
bootstrapsigner, its kube-controller-manager was never told to. There is nothing to hand-edit: argv is
rendered on every start, so a node running a current build picks the flag up
the next time the component starts. On EVERY control-plane node:

  1. confirm the node is running a stratum build that runs the bootstrapsigner
  2. restart the controller-manager, so its argv is re-rendered
  3. verify - this must print the flag the render carries:

       grep -o -- '--controllers=[^ ]*' ` + version.ControllerManagerManifest + `

     expected:

       ` + version.ControllerManagerControllersFlag + `

Every control plane, not just one: kube-controller-manager leader-elects, so the
signer runs on whichever node holds the lease. Then re-run ` + "`stratum token create`" + ` on a
control plane and retry this join with the token it prints.`

// DiscoverCA fetches cluster-info unverified, authenticating the payload by token JWS and CA hash.
func DiscoverCA(server, token, caHash string) ([]byte, error) {
	auth, err := discoverClusterInfo(server, token)
	if err != nil {
		return nil, err
	}
	return auth.TrustBundle(caHash)
}

// DiscoverJoin returns the pinned cluster root and the kubelet client-CA mode from one payload.
func DiscoverJoin(server, token, caHash string) ([]byte, bool, error) {
	auth, err := discoverClusterInfo(server, token)
	if err != nil {
		return nil, false, err
	}
	ca, err := auth.TrustBundle(caHash)
	if err != nil {
		return nil, false, err
	}
	split, err := auth.KubeletClientCASplit()
	if err != nil {
		return nil, false, err
	}
	return ca, split, nil
}

func discoverClusterInfo(server, token string) (clusterinfo.Authenticated, error) {
	// A malformed token must fail before any request to an endpoint that cannot yet be verified.
	tok, err := clusterinfo.ParseToken(token)
	if err != nil {
		return clusterinfo.Authenticated{}, err
	}
	if err := requireHTTPS(server); err != nil {
		return clusterinfo.Authenticated{}, err
	}

	reachDeadline := time.Now().Add(reachWaitTimeout)
	unreachable := func(err error, attempts int) error {
		return fmt.Errorf("%w (control plane still unreachable after %s, %d attempts)", err, reachWaitTimeout, attempts)
	}
	var deadline time.Time // the signature budget, from the first fetch that answers
	var lastErr error      // the last transient failure, while there has been no answer
	for attempt := 1; ; attempt++ {
		timeout := fetchTimeout
		if deadline.IsZero() {
			// A spent budget must never reach the request as its timeout.
			left := time.Until(reachDeadline)
			if left <= 0 && lastErr != nil {
				return clusterinfo.Authenticated{}, unreachable(lastErr, attempt-1)
			}
			timeout = min(timeout, left) // non-positive only with no budget at all; the fetch refuses it
		}
		clipped := timeout < fetchTimeout
		data, err := fetchClusterInfo(server, timeout)
		if err != nil {
			var status *clusterInfoHTTPError
			// A newly elected VIP holder can serve before its anonymous cluster-info RBAC has caught up.
			rbacPending := errors.As(err, &status) && status.code == http.StatusForbidden
			if !transientDialError(err) && !rbacPending {
				return clusterinfo.Authenticated{}, err
			}
			left := time.Until(reachDeadline)
			if left <= 0 {
				// A clipped attempt says only that time ran out; the earlier failure says why.
				if clipped && isTimeout(err) && lastErr != nil {
					err = lastErr
				}
				return clusterinfo.Authenticated{}, unreachable(err, attempt)
			}
			lastErr = err
			fmt.Printf("  control plane not reachable yet (%v); retrying (attempt %d)...\n", err, attempt)
			time.Sleep(min(reachWaitInterval, left))
			continue
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(signatureWaitTimeout)
		}
		auth, err := clusterinfo.Authenticate(data, tok)
		if err == nil {
			return auth, nil
		}
		// A present-but-invalid signature fails immediately; only an absent one is retried.
		var missing *clusterinfo.NoSignatureError
		if !errors.As(err, &missing) {
			return clusterinfo.Authenticated{}, err
		}
		if time.Now().After(deadline.Add(-signatureWaitInterval)) {
			return clusterinfo.Authenticated{}, fmt.Errorf("%w after %s (%d attempts)\n%s",
				err, signatureWaitTimeout, attempt, unsignedClusterInfoRemedy)
		}
		fmt.Printf("  waiting for the control plane to sign cluster-info (attempt %d)...\n", attempt)
		time.Sleep(signatureWaitInterval)
	}
}

// fetchClusterInfoData performs the request and decodes the ConfigMap data map; it trusts nothing.
func fetchClusterInfoData(server string, timeout time.Duration) (map[string]string, error) {
	// http.Client reads a non-positive Timeout as none; a spent budget must never wait forever.
	if timeout <= 0 {
		return nil, fmt.Errorf("fetch cluster-info: no time left to wait for %s", server)
	}
	url := server + "/api/v1/namespaces/kube-public/configmaps/cluster-info"
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // INITIAL JOIN ONLY: no anchor exists yet, so the response is authenticated by its detached JWS and the token CA hash (DiscoverCA). An already-joined node has an anchor and uses FetchClusterCAVerified; this is the only unverified fetch in the tree.
		// The endpoint is not authenticated yet; a redirect could steer the request anywhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch cluster-info: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &clusterInfoHTTPError{code: resp.StatusCode}
	}
	// The transport skips TLS verification, so the body is attacker-controlled: bound the read.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxClusterInfoBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxClusterInfoBytes {
		return nil, fmt.Errorf(
			"cluster-info response exceeds the %d-byte limit this join reads before "+
				"authenticating anything. On a real cluster the usual cause is accumulated "+
				"bootstrap-token signatures (~112 bytes each, one per token that has not been "+
				"reaped; tokens minted by earlier releases, or with --ttl 0, never expire): "+
				"check `kubectl -n kube-system get secrets --field-selector type=bootstrap.kubernetes.io/token` "+
				"and delete the ones no longer in use",
			maxClusterInfoBytes)
	}
	return parseClusterInfoBody(body)
}

type clusterInfoHTTPError struct{ code int }

func (e *clusterInfoHTTPError) Error() string {
	return fmt.Sprintf("fetch cluster-info: HTTP %d", e.code)
}

func parseClusterInfoBody(body []byte) (map[string]string, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		return nil, fmt.Errorf("parse cluster-info ConfigMap: %w", err)
	}
	if cm.Data[clusterinfo.KubeConfigKey] == "" {
		return nil, fmt.Errorf("cluster-info ConfigMap missing data.%s", clusterinfo.KubeConfigKey)
	}
	return cm.Data, nil
}

// WriteBootstrapKubeconfig renders the bootstrap kubeconfig and writes it atomically at 0600.
func WriteBootstrapKubeconfig(server, token string, ca []byte) error {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[version.ClusterName] = &clientcmdapi.Cluster{Server: server, CertificateAuthorityData: ca}
	cfg.AuthInfos["tls-bootstrap"] = &clientcmdapi.AuthInfo{Token: token}
	cfg.Contexts["default"] = &clientcmdapi.Context{Cluster: version.ClusterName, AuthInfo: "tls-bootstrap"}
	cfg.CurrentContext = "default"
	if err := os.MkdirAll(version.ConfDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(version.BootstrapKubeconfig), 0o755); err != nil {
		return err
	}
	b, err := clientcmd.Write(*cfg)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(version.BootstrapKubeconfig, b, 0o600)
}

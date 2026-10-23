// Package bootstrap owns cluster-info transport for every consumer of the
// kube-public cluster-info ConfigMap, under two distinct trust models.
//
// For INITIAL JOIN (the worker join and the HA control-plane join) the node
// holds no trust anchor yet, so DiscoverCA fetches without verifying the peer
// and authenticates the PAYLOAD instead — against the token's detached JWS and
// the token's CA hash — then writes the TLS-bootstrap kubeconfig.
//
// For an ALREADY-JOINED node (package `certs`, during CA rotation) an anchor
// exists, so FetchClusterCAVerified verifies the PEER against that anchor and
// authenticates nothing about the payload; the caller validates it. That path
// is why no unverified fetch remains: an already-joined node never needed one.
//
// It lives in a neutral leaf package so no command package has to import
// another.
//
// The file is split along one line that matters: fetchClusterInfoData
// is TRANSPORT — it performs the request and decodes the ConfigMap's data map,
// and does nothing else. Everything that interprets those bytes lives in the
// clusterinfo subpackage behind clusterinfo.Authenticate, so the unauthenticated
// path below cannot reach the trust-bundle helper even by accident.
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

// maxClusterInfoBytes caps the pre-authentication read of the cluster-info body.
//
// 1 MiB is far above any legitimate cluster-info: the kubeconfig plus a CA bundle
// is a couple of kilobytes, and each live signing token adds a measured ~112 JSON
// bytes of jws-kubeconfig-<id>, so the cap is ~9,300 tokens away. It bounds a
// hostile endpoint, not a real one — but see the error text where it trips.
const maxClusterInfoBytes = 1 << 20

// requireHTTPS rejects a server URL that would carry the bootstrap token in
// cleartext. Nothing else on the join path checks the scheme.
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

// Signature-wait budget for a joining node. The bootstrapsigner is
// informer-driven and normally signs sub-second, but the signature is patched in
// a moment after the ConfigMap itself is written, and `stratum certs rotate`
// republishes cluster-info mid-rotation — so a MISSING signature is worth a
// bounded retry. A signature that is present and does not verify is never
// retried; see clusterinfo.Authenticate.
//
// Vars rather than consts only so tests can shorten them; production never
// reassigns them.
var (
	signatureWaitInterval = 2 * time.Second
	signatureWaitTimeout  = 30 * time.Second
)

// Reachability budget for the same fetch. A join reaches the control
// plane through --join's address, often a VIP, and a VIP that is moving or
// re-announcing, or a control plane that is restarting, answers "no route to
// host" or "connection refused" for a moment. A join has no retry of its own,
// so connection-level failures and anonymous-discovery RBAC propagation
// (HTTP 403) receive bounded retries; invalid signatures and pins still
// fail at once. Its own budget, not the signature's: waiting out the
// network must not spend the signer's time.
//
// The budget is a bound, not a target: while the control plane is unreachable,
// each fetch's timeout is capped to what is left of it, no sleep runs past it,
// and no attempt starts after it.
var (
	reachWaitInterval = 2 * time.Second
	reachWaitTimeout  = 2 * time.Minute
)

// fetchTimeout bounds one cluster-info request. A var so tests can shorten it.
var fetchTimeout = 15 * time.Second

// fetchClusterInfo is fetchClusterInfoData, indirected for tests.
var fetchClusterInfo = fetchClusterInfoData

// transientDialError reports a connection-level failure another attempt can
// outlast: nothing answered, or the connection was dropped, before any bytes
// were trusted or judged. A non-200 status, an oversized body or a malformed
// response is an answer, and retrying it changes nothing.
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

// unsignedClusterInfoRemedy is the operator-facing half of the hard-fail. There
// is deliberately no flag, environment variable or warn-and-continue that lets a
// join proceed without a verified signature: a gated fallback would be typed by
// the same operator the attack targets.
//
// The likely cause is not an attack but a control plane started without
// the signer flag. processes.go renders argv on every start, so the flag
// arrives with the next restart and there is nothing to edit.
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

// DiscoverCA fetches the kube-public cluster-info ConfigMap (TLS-insecure, since
// we don't trust the server yet), authenticates it with the bootstrap token's
// detached JWS, verifies the CA against the token CA hash, and returns the
// authenticated trust bundle. For INITIAL join.
//
// The JWS is what makes the insecure fetch safe: it authenticates the whole
// kubeconfig, where the CA-hash pin only ever attests one public key. Both run,
// and both must pass.
func DiscoverCA(server, token, caHash string) ([]byte, error) {
	auth, err := discoverClusterInfo(server, token)
	if err != nil {
		return nil, err
	}
	return auth.TrustBundle(caHash)
}

// DiscoverJoin returns both the pinned cluster root and the kubelet client-CA
// mode from the same token-authenticated cluster-info payload.
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
	// Validate the token BEFORE any network I/O: a malformed token must not
	// cause a request to an endpoint we cannot yet verify. There is no code path
	// on which an empty or unparseable token skips verification.
	tok, err := clusterinfo.ParseToken(token)
	if err != nil {
		return clusterinfo.Authenticated{}, err
	}
	// A plaintext endpoint would put this same token on the wire as a bearer
	// credential during TLS bootstrap — and it is the HMAC key the signature
	// check depends on, so leaking it lets an attacker forge cluster-info.
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
			// Still waiting for an answer: this attempt may not outlive the
			// reach budget, and none starts once it is spent. Checked here,
			// at the start of every attempt, not only after the last one
			// failed: a delay between the two would hand a spent budget —
			// a non-positive duration — to the request as its timeout.
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
			// A newly elected VIP holder can serve before its anonymous
			// cluster-info RBAC has caught up. Nothing is trusted until the
			// eventual payload passes both signature and CA pin checks.
			rbacPending := errors.As(err, &status) && status.code == http.StatusForbidden
			if !transientDialError(err) && !rbacPending {
				return clusterinfo.Authenticated{}, err
			}
			left := time.Until(reachDeadline)
			if left <= 0 {
				// An attempt the remaining budget cut short times out whatever
				// the control plane is doing, and says only that it ran out of
				// time. The failure before it says why the control plane was
				// unreachable -- "connection refused", "no route to host" --
				// so that is the one reported.
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
		// Only an ABSENT signature is retried, and only until the budget runs
		// out. A present-but-invalid signature returns immediately.
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

// fetchClusterInfoData performs the request and decodes the ConfigMap's data
// map. Transport only: it parses no kubeconfig, decodes no PEM, and returns
// nothing that has been trusted.
func fetchClusterInfoData(server string, timeout time.Duration) (map[string]string, error) {
	// http.Client takes a non-positive Timeout to mean none at all. Whatever
	// computed this one, "no time left" must never become "wait forever".
	if timeout <= 0 {
		return nil, fmt.Errorf("fetch cluster-info: no time left to wait for %s", server)
	}
	url := server + "/api/v1/namespaces/kube-public/configmaps/cluster-info"
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // INITIAL JOIN ONLY: no anchor exists yet, so the response is authenticated by its detached JWS and the token CA hash (DiscoverCA). An already-joined node has an anchor and uses FetchClusterCAVerified; this is the only unverified fetch in the tree.
		// cluster-info is one fixed endpoint and should never redirect. Following
		// one would let a server we have not yet authenticated steer this request
		// to an arbitrary host or downgrade it to plaintext.
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
	// Bounded because nothing here is trusted yet: the transport skips TLS
	// verification, so the body is attacker-controlled, and DiscoverCA re-fetches
	// on a retry loop. A real cluster-info ConfigMap is a few kilobytes; the
	// client Timeout bounds time, not bytes.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxClusterInfoBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxClusterInfoBytes {
		// Name the realistic cause. The cap is a defence against a hostile endpoint
		// streaming an unbounded body before anything is authenticated, but the way
		// a real cluster reaches it is benign accumulation: the bootstrapsigner adds
		// one jws-kubeconfig-<id> entry per live signing token, ~112 JSON bytes
		// each, and tokens minted by earlier releases never expired. That is roughly
		// 9,300 tokens ever minted. Surfacing this as a bare read error would make a
		// token-lifecycle problem look like a network fault on the joining node.
		// No byte count: the body was read through a LimitReader at the cap, so its
		// length here is always exactly the limit plus one. Printing it would tell
		// an operator they are one byte over when the endpoint may be streaming
		// megabytes — false precision in the one message whose purpose is diagnosis.
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

// parseClusterInfoBody pulls .data out of a ConfigMap JSON body.
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

// WriteBootstrapKubeconfig writes the token-auth kubeconfig kubelet uses for
// TLS bootstrap (and ha uses to download the shared certs) to
// version.BootstrapKubeconfig. It is rendered to bytes and written through
// the shared atomic writer: a kubeconfig cut in half stops the kubelet's TLS
// bootstrap from starting at all, and the file carries a token, so the 0600
// mode is atomicfile's, not a chmod after a wider window.
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

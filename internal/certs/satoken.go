package certs

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/keyutil"

	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodejob"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// ExitStaleTokens is the scan Job's exit status when it found a stale token.
const ExitStaleTokens = 10

// ErrStaleTokens is ScanTokens' error when it found such a token.
var ErrStaleTokens = errors.New("this node mounts a token signed by the old service-account key")

// Vars, so tests can shorten them.
var (
	// tokenGateWait must cover a projected token's refresh, up to ~50 of its 60 minutes.
	tokenGateWait = 60 * time.Minute
	tokenGatePoll = time.Minute
	// client-go re-reads a token file once a minute; tokenSettle waits that out.
	tokenSettle = 2 * time.Minute
	// a restarted kubelet re-issues its tokens in about a second
	kubeletReissueWait = 30 * time.Second
	kubeletReissuePoll = 2 * time.Second

	restartKubelet = func() error { return comps.Restart("kubelet") }
	runExitCode    = nodejob.RunExitCode
	jobLogs        = nodejob.Logs
)

const scanTokens = "scan-tokens"

// a scan Job restarts the kubelet, so it gets far less time than a phase Job
const (
	scanJobDeadline     = 5 * time.Minute
	scanJobPollAttempts = 60
)

// kubeletRoot is the kubelet's --root-dir; vars, so tests can point them elsewhere.
var (
	kubeletRoot = filepath.Join(version.DataDir, "kubelet")
	pkiDir      = version.PKIDir
)

// keyIDPattern is a key id: base64url, unpadded, of a SHA-256.
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// saKeyID is the kid the apiserver puts in a token it signs: base64url SHA-256 of the PKIX DER.
func saKeyID(pubPEM []byte) (string, error) {
	blk, _ := pem.Decode(pubPEM)
	if blk == nil || blk.Type != "PUBLIC KEY" {
		return "", errors.New("not a PEM PUBLIC KEY")
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// oldSAKeyID wraps fs.ErrNotExist when finalize removed the key or sign-new never ran here.
func oldSAKeyID() (string, error) {
	b, err := os.ReadFile(filepath.Join(pkiDir, pki.PreRotationBackupName("sa.pub")))
	if err != nil {
		return "", fmt.Errorf("read the old service-account public key: %w", err)
	}
	kid, err := saKeyID(b)
	if err != nil {
		return "", fmt.Errorf("old service-account public key: %w", err)
	}
	return kid, nil
}

// ok is false for anything that is not a JWT with a kid.
func tokenKeyID(token string) (kid string, ok bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return "", false
	}
	h, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	var hdr struct {
		KeyID string `json:"kid"`
	}
	if json.Unmarshal(h, &hdr) != nil {
		return "", false
	}
	return hdr.KeyID, hdr.KeyID != ""
}

// staleToken is a mounted token signed by the old key.
type staleToken struct {
	PodUID string
	Volume string
	Pod    string // namespace/name, when it could be resolved
}

func (s staleToken) String() string {
	pod := s.Pod
	if pod == "" {
		pod = "pod uid " + s.PodUID
	}
	if s.Volume == "" {
		return pod
	}
	return pod + " (volume " + s.Volume + ")"
}

func listStaleTokens(st []staleToken) string {
	s := make([]string, len(st))
	for i := range st {
		s[i] = st[i].String()
	}
	return strings.Join(s, ", ")
}

// walks by hand: filepath.Glob swallows a directory it cannot read, and one may hold a stale token
func findStaleTokens(ctx context.Context, root, oldKid string) ([]staleToken, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var stale []staleToken
	pods, err := readDirIfExists(filepath.Join(root, "pods"))
	if err != nil {
		return nil, err
	}
	for _, pod := range pods {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		volumes := filepath.Join(root, "pods", pod, "volumes", "kubernetes.io~projected")
		names, err := readDirIfExists(volumes)
		if err != nil {
			return nil, err
		}
		for _, vol := range names {
			found, err := staleInVolume(ctx, filepath.Join(volumes, vol), oldKid)
			if err != nil {
				return nil, err
			}
			for _, rel := range found {
				stale = append(stale, staleToken{PodUID: pod, Volume: vol + "/" + rel})
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return stale, nil
}

// every file is read; the scan counts only if ..data is stable across it and nothing went missing
func staleInVolume(ctx context.Context, vol, oldKid string) ([]string, error) {
	for attempt := 0; attempt < scanAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		root, err := currentGeneration(vol)
		if errors.Is(err, fs.ErrNotExist) {
			if _, serr := os.Stat(vol); errors.Is(serr, fs.ErrNotExist) {
				return nil, nil
			}
			continue // ..data is between generations
		}
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", vol, err)
		}
		scanHook()
		found, complete, err := walkTokens(ctx, root, oldKid)
		if err != nil {
			return nil, err
		}
		if after, aerr := currentGeneration(vol); complete && aerr == nil && after == root {
			return found, nil
		}
	}
	return nil, fmt.Errorf("scan %s: the volume kept changing under the scan (%d attempts), so no complete read of it could be made", vol, scanAttempts)
}

// scanHook runs between choosing a generation and reading it, so a test can replace it there.
var (
	scanAttempts = 5
	scanHook     = func() {}
)

// currentGeneration's error wraps fs.ErrNotExist for a ..data that points at nothing.
func currentGeneration(vol string) (string, error) {
	if _, err := os.Lstat(filepath.Join(vol, "..data")); errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(vol); err != nil {
			return "", err
		}
		return vol, nil
	}
	return filepath.EvalSymlinks(filepath.Join(vol, "..data"))
}

// complete is false if anything went missing: the generation was removed while it was read.
func walkTokens(ctx context.Context, root, oldKid string) (found []string, complete bool, err error) {
	complete = true
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr // expiry fails the scan; it never reads as clean
		}
		if errors.Is(err, fs.ErrNotExist) {
			complete = false
			return nil
		}
		if err != nil {
			return fmt.Errorf("scan %s: %w", p, err)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			complete = false
			return nil
		}
		if err != nil {
			return fmt.Errorf("read mounted token: %w", err)
		}
		if kid, ok := tokenKeyID(string(b)); ok && kid == oldKid {
			rel, _ := filepath.Rel(root, p)
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	return found, complete, err
}

// readDirIfExists lists a directory's entries; one that is not there has none.
func readDirIfExists(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	names := make([]string, len(ents))
	for i, e := range ents {
		names[i] = e.Name()
	}
	return names, nil
}

func scanNode(ctx context.Context, w io.Writer, oldKid string, restart bool) ([]staleToken, error) {
	return scanNodeWithFence(ctx, w, oldKid, restart, nil)
}

func scanNodeWithFence(ctx context.Context, w io.Writer, oldKid string, restart bool, confirm func(string) error) ([]staleToken, error) {
	stale, err := findStaleTokens(ctx, kubeletRoot, oldKid)
	if err != nil || len(stale) == 0 || !restart {
		return stale, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// the token read can take time, so the cluster fence is reconfirmed after it
	if confirm != nil {
		if err := confirm("restarting the kubelet to reissue tokens"); err != nil {
			return nil, err
		}
	}
	release, err := acquireNodeLock("certs rotate-ca token reissue", false)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "restarting the kubelet: %d mounted token(s) are signed by the old service-account key\n", len(stale))
	if err := restartKubelet(); err != nil {
		return nil, fmt.Errorf("restart kubelet: %w", err)
	}
	deadline := time.Now().Add(kubeletReissueWait)
	for {
		if stale, err = findStaleTokens(ctx, kubeletRoot, oldKid); err != nil || len(stale) == 0 || time.Now().After(deadline) {
			return stale, err
		}
		if err := sleepCtx(ctx, kubeletReissuePoll); err != nil {
			return nil, err
		}
	}
}

// ScanTokens is `certs rotate-ca --scan-tokens`, the per-node scan Job
func ScanTokens(w io.Writer, oldKid string, restart bool) error {
	if !keyIDPattern.MatchString(oldKid) {
		return fmt.Errorf("--old-kid %q is not a service-account key id", oldKid)
	}
	// inside the Job's own deadline, so a scan that cannot finish fails here
	ctx, cancel := context.WithTimeout(context.Background(), scanJobDeadline-time.Minute)
	defer cancel()
	stale, err := scanNode(ctx, w, oldKid, restart)
	if err != nil {
		return err
	}
	for _, s := range stale {
		fmt.Fprintf(w, "%s %s %s\n", staleLinePrefix, s.PodUID, s.Volume)
	}
	if len(stale) > 0 {
		return ErrStaleTokens
	}
	return nil
}

// staleLinePrefix starts an offender line, so the orchestrator reads them out of the Job's log.
const staleLinePrefix = "stale-token"

func parseStaleLines(log string) []staleToken {
	var out []staleToken
	for _, l := range strings.Split(log, "\n") {
		if f := strings.SplitN(strings.TrimSpace(l), " ", 3); len(f) == 3 && f[0] == staleLinePrefix {
			out = append(out, staleToken{PodUID: f[1], Volume: f[2]})
		}
	}
	return out
}

func scanNodeArgv(oldKid string, restart bool) []string {
	argv := []string{
		"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--",
		version.StratumInstallPath, "certs", "rotate-ca",
		"--scan-tokens", "--old-kid", oldKid,
		"--yes",
	}
	if restart {
		argv = append(argv, "--restart-kubelet")
	}
	return argv
}

func scanNodeJob(ctx context.Context, w io.Writer, cs kubernetes.Interface, n cnode, oldKid string, restart bool) ([]staleToken, error) {
	spec := nodejob.Spec{
		Output:         w,
		Namespace:      caNamespace,
		Name:           caJobName(n.Name, scanTokens),
		Node:           n.Name,
		LabelKey:       "stratum.ciq.com/carotate",
		Container:      "carotate",
		Argv:           scanNodeArgv(oldKid, restart),
		ActiveDeadline: scanJobDeadline,
		PollAttempts:   scanJobPollAttempts,
		Kind:           "token scan",
	}
	code, err := runExitCode(ctx, cs, spec)
	switch {
	case err != nil:
		return nil, err
	case code == 0:
		return nil, nil
	case code != ExitStaleTokens:
		return nil, fmt.Errorf("token scan Job %s failed with exit status %d (logs: kubectl -n %s logs job/%s)", spec.Name, code, spec.Namespace, spec.Name)
	}
	log, err := jobLogs(ctx, cs, spec)
	stale := parseStaleLines(log)
	if len(stale) == 0 {
		// Stale, but not who: never read that as clean.
		why := "its log had no offenders in it"
		if err != nil {
			why = err.Error()
		}
		stale = []staleToken{{Pod: "(offenders not listed: " + why + ")"}}
	}
	return stale, nil
}

// an unresolved pod is still reported by its uid
func nameStale(ctx context.Context, cs kubernetes.Interface, node string, stale []staleToken) {
	if len(stale) == 0 {
		return
	}
	pods, err := cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return
	}
	byUID := map[string]string{}
	for i := range pods.Items {
		byUID[string(pods.Items[i].UID)] = pods.Items[i].Namespace + "/" + pods.Items[i].Name
	}
	for i := range stale {
		if name, ok := byUID[stale[i].PodUID]; ok {
			stale[i].Pod = name
		}
	}
}

// An empty result is a clean node.
type tokenScanner func(ctx context.Context, n cnode, restart bool) ([]staleToken, error)

func clusterTokenScanner(w io.Writer, cs kubernetes.Interface, me, oldKid string, confirm func(string) error) tokenScanner {
	return func(ctx context.Context, n cnode, restart bool) ([]staleToken, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// each Job needs its own check: the gate can last an hour, past any earlier one
		if err := confirm("scanning tokens on node " + n.Name); err != nil {
			return nil, err
		}
		var stale []staleToken
		var err error
		if n.Name == me {
			stale, err = scanNodeWithFence(ctx, w, oldKid, restart, confirm)
		} else {
			stale, err = scanNodeJob(ctx, w, cs, n, oldKid, restart)
		}
		if err != nil {
			return nil, err
		}
		nameStale(ctx, cs, n.Name, stale)
		return stale, nil
	}
}

// returns once every node scans clean in one pass, then waits tokenSettle
func waitTokensClean(ctx context.Context, w io.Writer, nodes []cnode, scan tokenScanner) error {
	gctx, cancel := context.WithTimeout(ctx, tokenGateWait)
	defer cancel()
	timedOut := func(what string) error {
		return fmt.Errorf("after %s the old service-account key still could not be cleared for finalize: %s (clients holding a token it signed would get 401)", tokenGateWait, what)
	}
	restarted := map[string]bool{}
	for {
		var stale []string
		for _, n := range nodes {
			st, err := scan(gctx, n, false)
			if err == nil && len(st) > 0 && !restarted[n.Name] {
				restarted[n.Name] = true
				st, err = scan(gctx, n, true)
			}
			if err != nil {
				if gctx.Err() != nil && ctx.Err() == nil {
					return timedOut(strings.Join(append(stale, "the scan of "+n.Name+" did not finish"), "; "))
				}
				return fmt.Errorf("scan %s for tokens signed by the old service-account key: %w", n.Name, err)
			}
			if len(st) > 0 {
				stale = append(stale, n.Name+": "+listStaleTokens(st))
			}
		}
		if len(stale) == 0 {
			break
		}
		msg := strings.Join(stale, "; ")
		fmt.Fprintf(w, "  waiting for the kubelets to re-issue tokens signed by the old service-account key: %s\n", msg)
		if err := sleepCtx(gctx, tokenGatePoll); err != nil {
			if ctx.Err() == nil {
				return timedOut("these nodes still mount them: " + msg)
			}
			return err
		}
	}
	fmt.Fprintf(w, "  no node mounts a token signed by the old service-account key; waiting %s for clients to re-read their token files\n", tokenSettle)
	return sleepCtx(ctx, tokenSettle)
}

// no node may hold a token the old key signed while finalize is to drop that key
func finalizeTokenGateForKey(ctx context.Context, w io.Writer, cs kubernetes.Interface, nodes []cnode, me string, confirm func(string) error, oldKid string) error {
	if err := waitTokensClean(ctx, w, nodes, clusterTokenScanner(w, cs, me, oldKid, confirm)); err != nil {
		return err
	}
	warnLegacyTokenSecrets(ctx, w, cs, oldKid)
	return nil
}

// warns only: those Secrets are not mounted tokens the kubelet can re-issue
func warnLegacyTokenSecrets(ctx context.Context, w io.Writer, cs kubernetes.Interface, oldKid string) {
	stale, err := legacyStaleSecrets(ctx, cs, oldKid)
	if err != nil {
		fmt.Fprintf(w, "  WARN: could not check the %s Secrets for the old service-account key: %v\n", saTokenSecretType, err)
	}
	for _, s := range stale {
		fmt.Fprintf(w, "  WARN: Secret %s holds a token signed by the old service-account key; it stops working at finalize. Delete the Secret and create it again to get a new one.\n", s)
	}
}

func legacyStaleSecrets(ctx context.Context, cs kubernetes.Interface, oldKid string) ([]string, error) {
	sl, err := cs.CoreV1().Secrets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "type=" + saTokenSecretType})
	if err != nil {
		return nil, fmt.Errorf("list service-account token Secrets: %w", err)
	}
	var out []string
	for i := range sl.Items {
		s := &sl.Items[i]
		if s.Type != corev1.SecretType(saTokenSecretType) {
			continue
		}
		if kid, ok := tokenKeyID(string(s.Data[corev1.ServiceAccountTokenKey])); ok && kid == oldKid {
			out = append(out, s.Namespace+"/"+s.Name)
		}
	}
	return out, nil
}

// the same check for --phase=finalize: this node's tokens only, no kubelet restart
func localTokenGate(ctx context.Context, w io.Writer, o CARotateOptions) error {
	if o.SkipTokenCheck {
		return nil
	}
	oldKid, err := oldSAKeyID()
	if errors.Is(err, fs.ErrNotExist) {
		// no backup with two keys still trusted is an unidentifiable old key, not a finalized node
		n, berr := saBundleKeys()
		if berr != nil {
			return fmt.Errorf("cannot tell whether this node still trusts an old service-account key: %w (--skip-token-check with --force finalizes without the check)", berr)
		}
		if n > 1 {
			return fmt.Errorf("this node has no old service-account key (%s) but sa-bundle.pub still holds %d keys, "+
				"so it cannot tell which mounted tokens the old key signed (--skip-token-check with --force finalizes without the check)", pki.PreRotationBackupName("sa.pub"), n)
		}
		castep(w, o, "No old service-account key on this node: not checking mounted tokens")
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot tell which tokens the old service-account key signed: %w (--skip-token-check finalizes without the check)", err)
	}
	stale, err := findStaleTokens(ctx, kubeletRoot, oldKid)
	if err != nil {
		return fmt.Errorf("scan the tokens mounted on this node: %w (--skip-token-check finalizes without the check)", err)
	}
	if len(stale) == 0 {
		return nil
	}
	if host, err := os.Hostname(); err == nil {
		if cs, err := k8sutil.AdminClientset(); err == nil {
			nameStale(ctx, cs, host, stale)
		}
	}
	return fmt.Errorf("%d token(s) mounted on this node are signed by the old service-account key, which finalize drops (their clients would get 401): %s. "+
		"Restart this node's kubelet to re-issue them and run finalize again, or pass --skip-token-check", len(stale), listStaleTokens(stale))
}

// counted with the parser the apiserver loads the bundle with; unparseable is an error, not one
func saBundleKeys() (int, error) {
	b, err := os.ReadFile(filepath.Join(pkiDir, "sa-bundle.pub"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	keys, err := keyutil.ParsePublicKeysPEM(b)
	if err != nil {
		return 0, fmt.Errorf("parse sa-bundle.pub: %w", err)
	}
	return len(keys), nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Mounted ServiceAccount tokens and the old signing key. Finalize drops the old
// ServiceAccount key from the apiserver's verification set, so a token still
// signed by it gets 401 from that moment. After sign-new every token the
// apiserver issues carries the new key's id, but one that was issued before (a
// kubelet restarted while another control plane still signed with the old key)
// stays mounted until the kubelet re-issues it. This file finds those tokens on
// a node, which is what the finalize gate runs on every node.
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

// ExitStaleTokens is the exit status of `rotate-ca --scan-tokens` on a node that
// mounts a token signed by the old key. The scan Job's answer is its exit status,
// as the upgrade --check Job's is (internal/upgrade.ExitUpgradeNeeded).
const ExitStaleTokens = 10

// ErrStaleTokens is ScanTokens' error when it found such a token.
var ErrStaleTokens = errors.New("this node mounts a token signed by the old service-account key")

// Vars, so tests can shorten them.
var (
	// tokenGateWait is how long --cluster keeps checking for a node to be clean.
	// The unattended refresh of a projected token comes 80% of the way through its
	// hour, and the spike measured a worst case of about 50.5 minutes.
	tokenGateWait = 60 * time.Minute
	tokenGatePoll = time.Minute
	// tokenSettle is how long to wait once every node is clean: client-go re-reads
	// its token file once a minute.
	tokenSettle = 2 * time.Minute
	// kubeletReissueWait is how long a restarted kubelet has to re-issue the
	// tokens (the spike measured about a second).
	kubeletReissueWait = 30 * time.Second
	kubeletReissuePoll = 2 * time.Second

	restartKubelet = func() error { return comps.Restart("kubelet") }
	runExitCode    = nodejob.RunExitCode
	jobLogs        = nodejob.Logs
)

// scanTokens is the scan Job's phase name in its Job's name.
const scanTokens = "scan-tokens"

// A scan Job restarts a kubelet (up to 90s) and waits for it to re-issue, so it
// gets far less than a phase Job. Both limits move together, as for caJobDeadline.
const (
	scanJobDeadline     = 5 * time.Minute
	scanJobPollAttempts = 60
)

// kubeletRoot is the kubelet's --root-dir (internal/cluster/processes.go). Vars,
// so tests can point them at a directory.
var (
	kubeletRoot = filepath.Join(version.DataDir, "kubelet")
	pkiDir      = version.PKIDir
)

// keyIDPattern is a key id: base64url, unpadded, of a SHA-256.
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// saKeyID is the id the apiserver puts in the kid header of a token it signs with
// the key: the base64url SHA-256 of the key's PKIX DER encoding.
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

// oldSAKeyID is the id of the key sign-new retired on this node. The error wraps
// fs.ErrNotExist when there is none: finalize already removed it, or sign-new
// never ran here.
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

// tokenKeyID is the kid in a JWT's header. ok is false for anything that is not a
// JWT with one: a projected token for another issuer is not ours to judge.
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

// findStaleTokens reads every projected ServiceAccount token under the kubelet's
// root and returns those whose kid is oldKid. A token that is gone by the time it
// is read (its pod was deleted) is not stale.
//
// It walks the directories rather than filepath.Glob, which swallows a directory
// it cannot read: a pod directory that cannot be listed may hold a stale token,
// and the gate must fail on it, not pass.
//
// A scan that ran out of time is a failure, never a clean result: ctx is checked
// on entry and again before a result is returned, for every caller.
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

// staleInVolume is the files, relative to a projected volume, that hold a token
// signed by oldKid. A service-account token source writes to a path the pod spec
// chooses, nested or not, so every file is read, not just one named "token". The
// kubelet's atomic writer keeps the current files under what ..data points at,
// and the older generations beside it; only the current ones count.
//
// The kubelet can publish a new generation, and remove the one being read, at any
// moment, and both can hold the same old-key token: a scan of the removed one
// that skipped what it could not read would call a still-mounted token clean. So a
// scan counts only if ..data points at the same generation before and after it and
// nothing in it went missing; otherwise it starts again, and when it cannot get
// such a scan it fails. A volume that is gone (its pod was deleted) is clean.
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

// scanAttempts bounds staleInVolume's retries; a kubelet publishes a generation
// once in a while, not continually. scanHook runs between choosing a generation
// and reading it, so a test can replace it there.
var (
	scanAttempts = 5
	scanHook     = func() {}
)

// currentGeneration is the directory ..data points at, or vol itself for a volume
// that has no ..data (not written by the kubelet's atomic writer). Its error wraps
// fs.ErrNotExist for a ..data that points at nothing.
func currentGeneration(vol string) (string, error) {
	if _, err := os.Lstat(filepath.Join(vol, "..data")); errors.Is(err, fs.ErrNotExist) {
		if _, err := os.Stat(vol); err != nil {
			return "", err
		}
		return vol, nil
	}
	return filepath.EvalSymlinks(filepath.Join(vol, "..data"))
}

// walkTokens reads every file under root. complete is false if anything in it
// went missing: the generation was removed while it was read.
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

// readDirIfExists lists a directory's entries. A directory that is not there (a
// pod with no projected volumes, or one deleted mid-scan) has none; any other
// error is the caller's to fail on.
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

// scanNode finds this node's stale tokens. With restart it restarts the kubelet
// once if there are any, which re-issues every projected token, and scans again
// until they are gone or kubeletReissueWait has passed. A cancelled ctx fails the
// scan: it restarts nothing and never answers clean once it is done (the restart
// itself, once begun, is not interrupted); findStaleTokens enforces that.
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
	// Walking mounted tokens can take time. Reconfirm the cluster fence after
	// that read, then acquire the host fence before restarting the kubelet.
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

// ScanTokens is `certs rotate-ca --scan-tokens`, the per-node scan Job: it prints
// one line per mounted token signed by the key with id oldKid and returns
// ErrStaleTokens if there were any. With restart it restarts the kubelet
// first when there are, and reports what is left.
func ScanTokens(w io.Writer, oldKid string, restart bool) error {
	if !keyIDPattern.MatchString(oldKid) {
		return fmt.Errorf("--old-kid %q is not a service-account key id", oldKid)
	}
	// Inside the Job's own deadline (scanJobDeadline), so a scan that cannot finish
	// fails here, with its own message, rather than being killed.
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

// staleLinePrefix starts each offender line of the scan, so the orchestrator
// reads them out of the Job's log among whatever else it printed.
const staleLinePrefix = "stale-token"

func parseStaleLines(log string) []staleToken {
	var out []staleToken
	for _, l := range strings.Split(log, "\n") {
		// The volume is the rest of the line: a token path may hold a space.
		if f := strings.SplitN(strings.TrimSpace(l), " ", 3); len(f) == 3 && f[0] == staleLinePrefix {
			out = append(out, staleToken{PodUID: f[1], Volume: f[2]})
		}
	}
	return out
}

// scanNodeArgv is the argv of a node's scan Job; shell-free, as caNodeArgv is.
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

// scanNodeJob runs the scan on a remote node.
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

// nameStale fills in the namespace/name of each token's pod. Best effort: a pod
// that cannot be resolved is still reported by its uid.
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
		// Keep a Pod the caller already set (a stand-in for offenders whose uids
		// it could not read) unless the uid resolves.
		if name, ok := byUID[stale[i].PodUID]; ok {
			stale[i].Pod = name
		}
	}
}

// tokenScanner scans one node, restarting its kubelet first if restart is set and
// the node has stale tokens. An empty result is a clean node.
type tokenScanner func(ctx context.Context, n cnode, restart bool) ([]staleToken, error)

// clusterTokenScanner scans the node this runs on directly and every other node
// with a Job.
func clusterTokenScanner(w io.Writer, cs kubernetes.Interface, me, oldKid string, confirm func(string) error) tokenScanner {
	return func(ctx context.Context, n cnode, restart bool) ([]staleToken, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Even a read-only remote scan creates a privileged Job. The token gate
		// can last an hour, so the check at its entry cannot authorize later Jobs
		// or a kubelet restart after another run has taken the lease.
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

// waitTokensClean returns once a scan of every node, in one pass, finds no token
// signed by the old key, and then waits tokenSettle for the clients to re-read
// them. A node that has some gets its kubelet restarted, once, whether or not the
// restart cleared them; after that it is only scanned again, until tokenGateWait
// is up. The wait bounds the scans too, not just the passes between them. It is a
// check, not a sleep: it returns the moment every node is clean, and a node it
// cannot scan fails it.
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

// finalizeTokenGateForKey is the token arm of the finalize preflight: no node
// may drop the old key, whose id oldKid is, while a token it signed is still
// mounted anywhere.
func finalizeTokenGateForKey(ctx context.Context, w io.Writer, cs kubernetes.Interface, nodes []cnode, me string, confirm func(string) error, oldKid string) error {
	if err := waitTokensClean(ctx, w, nodes, clusterTokenScanner(w, cs, me, oldKid, confirm)); err != nil {
		return err
	}
	warnLegacyTokenSecrets(ctx, w, cs, oldKid)
	return nil
}

// warnLegacyTokenSecrets says which kubernetes.io/service-account-token Secrets
// hold a token signed by the old key. It does not block: those are not mounted
// tokens the kubelet can re-issue, and whoever uses one decides what to do.
func warnLegacyTokenSecrets(ctx context.Context, w io.Writer, cs kubernetes.Interface, oldKid string) {
	stale, err := legacyStaleSecrets(ctx, cs, oldKid)
	if err != nil {
		fmt.Fprintf(w, "  WARN: could not check the %s Secrets for the old service-account key: %v\n", saTokenSecretType, err)
	}
	for _, s := range stale {
		fmt.Fprintf(w, "  WARN: Secret %s holds a token signed by the old service-account key; it stops working at finalize. Delete the Secret and create it again to get a new one.\n", s)
	}
}

// legacyStaleSecrets is the namespace/name of each service-account token Secret
// whose token the key with id oldKid signed.
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

// localTokenGate is the same check for `--phase=finalize` on one control plane:
// this node's tokens only, no kubelet restart. A node with no old key has nothing
// to check: it finalized already, or never reached sign-new.
func localTokenGate(ctx context.Context, w io.Writer, o CARotateOptions) error {
	if o.SkipTokenCheck {
		return nil
	}
	oldKid, err := oldSAKeyID()
	if errors.Is(err, fs.ErrNotExist) {
		// No backup means finalize already ran, or sign-new never did, only if the
		// verification bundle holds a single key. PromoteSA writes the backup only
		// when it could read sa.pub, and promotes either way, so a bundle that
		// still holds two keys is a rotation whose old key cannot be identified.
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

// saBundleKeys is how many public keys sa-bundle.pub, the set the apiserver
// verifies tokens against, holds: 2 while a rotation is under way, 1 once
// finalize collapsed it. A node with no bundle holds none. It counts with the
// parser the apiserver loads the bundle with, which also takes a certificate's
// key, so a bundle it cannot parse is an error, never a count of one.
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

// sleepCtx waits d, or until ctx is cancelled.
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

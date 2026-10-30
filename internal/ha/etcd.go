package ha

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// etcdTLS builds the mTLS config to talk to a peer's etcd, using this node's
// apiserver-etcd-client cert and the etcd trust bundle (pki.EtcdTrustPEM).
//
// The bundle alone is the right trust set in every migration state, and is
// deliberately NOT unioned with the Kubernetes bundle: it already carries the
// root while any member is still root-signed, and the etcd CA alone once
// drop-root has run cluster-wide. Adding the Kubernetes root on top would keep it
// trusted for etcd SERVER identity permanently, letting any root-signed
// ServerAuth leaf with the right IP SAN impersonate an etcd endpoint here.
//
// Getting this wrong is not loud: it takes out MemberAdd/MemberRemove/
// MemberClientIPs — so HA join and snapshot rejoin — and, via the same material
// in k8sutil.EtcdctlEnv, silently stops the scheduled etcd snapshots during the
// exact window the cluster is most exposed.
func etcdTLS() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(
		filepath.Join(version.PKIDir, "apiserver-etcd-client.crt"),
		filepath.Join(version.PKIDir, "apiserver-etcd-client.key"))
	if err != nil {
		return nil, err
	}
	caPEM, err := pki.EtcdTrustPEM()
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no usable CA certificates in the etcd trust material under %s", version.PKIDir)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool}, nil
}

// etcdEndpoint builds the client URL for a control plane's etcd: the builder
// that also renders the client URL the member advertises in its static-pod
// args, so a member lookup, a health check or the migration's final
// verification cannot drift from what the member serves. The bracketing of an
// IPv6 host is the builder's: concatenating the host and port reads the port
// as another hextet of the address.
func etcdEndpoint(cpIP string) string {
	return nodestate.EtcdClientURL(cpIP)
}

// etcdClient dials the etcd of every address given. With more than one, the
// client balances across them and fails over when one is down, which is what an
// HA join wants: any live member can answer a MemberAdd.
func etcdClient(cpIPs ...string) (*clientv3.Client, error) {
	if len(cpIPs) == 0 {
		return nil, fmt.Errorf("etcd: no endpoints")
	}
	tc, err := etcdTLS()
	if err != nil {
		return nil, err
	}
	eps := make([]string, 0, len(cpIPs))
	for _, ip := range cpIPs {
		eps = append(eps, etcdEndpoint(ip))
	}
	return clientv3.New(clientv3.Config{
		Endpoints:   eps,
		TLS:         tc,
		DialTimeout: 10 * time.Second,
	})
}

// clusterClient is etcdClient for a request any member can answer: after
// connecting it syncs the endpoint list from the cluster's own membership, so a
// member that joined after the addresses were published is dialled too.
// Best-effort, because the addresses it was given already work.
func clusterClient(cpIPs []string) (*clientv3.Client, error) {
	cli, err := etcdClient(cpIPs...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = cli.Sync(ctx)
	return cli, nil
}

// memberAddSettle bounds how long MemberAdd waits out etcd's strict
// reconfiguration check.
var memberAddSettle = 90 * time.Second

// isEtcdErr matches err against one of rpctypes' client-side sentinels,
// through any amount of %w wrapping. The second arm converts a raw
// server-side status, which is what etcd's own ErrGRPC* values are.
func isEtcdErr(err error, target error) bool {
	return errors.Is(err, target) || errors.Is(rpctypes.Error(err), target)
}

// isUnhealthyCluster reports etcd's "unhealthy cluster" refusal.
func isUnhealthyCluster(err error) bool {
	return isEtcdErr(err, rpctypes.ErrUnhealthy)
}

// isNotEnoughStarted reports the strict reconfiguration check's refusal of a
// voting member-add while an earlier added member has not started yet.
func isNotEnoughStarted(err error) bool {
	return isEtcdErr(err, rpctypes.ErrMemberNotEnoughStarted)
}

// memberAddRefusals are etcd's outright refusals of a MemberAdd request
// itself: etcd answered the rejection instead of a proposal's result, so the
// request cannot have committed and the reply was not lost. ErrUnhealthy is
// the strict reconfiguration check's refusal, which MemberAdd retries
// internally and which means nothing was added; ErrMemberNotEnoughStarted is
// the same check refusing a voter while an earlier added member has not
// started, as when another controller's join is mid-flight; ErrPeerURLExist
// means a member at that URL already exists — a prior add committed. Deliberately
// absent are the possibly-lost replies: ErrTimeout ("the request timed out" —
// the proposal may have committed anyway), ErrTimeoutWaitAppliedIndex,
// ErrLeaderChanged, and deadline, cancellation, and transport errors.
// isPromoteNotYet classifies the same split for MemberPromote.
var memberAddRefusals = []error{
	rpctypes.ErrPeerURLExist,
	rpctypes.ErrUnhealthy,
	rpctypes.ErrMemberNotEnoughStarted,
	rpctypes.ErrTooManyLearners,
	rpctypes.ErrNotLeader,
	rpctypes.ErrNoLeader,
	rpctypes.ErrTooManyRequests,
	rpctypes.ErrPermissionDenied,
}

// memberAddRefused reports whether addErr is one of memberAddRefusals,
// through any amount of wrapping and from a raw server-side status too.
func memberAddRefused(addErr error) bool {
	if addErr == nil {
		return false
	}
	for _, refusal := range memberAddRefusals {
		if isEtcdErr(addErr, refusal) {
			return true
		}
	}
	return false
}

// MemberAdd adds newPeerURL to the cluster reachable at any of cpIPs' etcd as
// a learner and returns the ETCD_INITIAL_CLUSTER string the new member must
// boot with (state=existing). The controller's start promotes it to a voter
// once it has caught up (PromoteLearner).
//
// A learner, not a voter: a voter counts towards quorum from the moment it is
// added, so growing one member to two needs both for every write until the
// new etcd has started and replayed the log. The existing member's apiserver
// stalls meanwhile, and kube-vip, which renews its lease through that
// apiserver, gives up the VIP. A learner does not count towards quorum.
//
// It retries etcd's "unhealthy cluster" refusal and its one-learner-at-a-time
// refusal, and only those. etcd's strict reconfiguration check refuses a
// MemberAdd until every member has been connected for a few seconds, and a
// join that follows another one closely, which is what a scripted three-node
// bring-up does, finds the earlier joiner still a learner. Both refusals mean
// nothing was added, so retrying them is safe; any other error is returned at
// once.
func MemberAdd(cpIPs []string, newName, newPeerURL string) (string, error) {
	cli, err := clusterClient(cpIPs)
	if err != nil {
		return "", err
	}
	defer cli.Close()
	resp, err := addSettling(os.Stdout, func(ctx context.Context) (*clientv3.MemberAddResponse, error) {
		return cli.MemberAddAsLearner(ctx, []string{newPeerURL})
	}, func(err error) bool { return isUnhealthyCluster(err) || isTooManyLearners(err) },
		"stratum HA join: etcd is not accepting a new member yet (the last one is still settling or catching up); retrying")
	if err != nil {
		return "", fmt.Errorf("etcd member add: %w", err)
	}
	return initialCluster(resp.Members, newName), nil
}

// initialCluster renders members as an ETCD_INITIAL_CLUSTER string. A member that
// has not started has no name yet, so it takes unnamed.
func initialCluster(members []*etcdserverpb.Member, unnamed string) string {
	var parts []string
	for _, m := range members {
		name := m.Name
		if name == "" {
			name = unnamed
		}
		for _, pu := range m.PeerURLs {
			parts = append(parts, name+"="+pu)
		}
	}
	return strings.Join(parts, ",")
}

// MemberCount is how many members (learners included) the cluster at cpIPs'
// etcd has, as the answering member last saw it.
func MemberCount(cpIPs []string) (int, error) {
	cli, err := clusterClient(cpIPs)
	if err != nil {
		return 0, err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return countMembers(ctx, cli)
}

// memberLister is the part of the etcd client countMembers needs.
type memberLister interface {
	MemberList(context.Context, ...clientv3.OpOption) (*clientv3.MemberListResponse, error)
}

// countMembers reads the member list serializably: from the answering member's
// own copy, which it serves without quorum. The caller is the seed noticing HA
// growth in its own etcd, and a second member that joined and then went away
// leaves it without quorum, which a linearizable read would not survive.
func countMembers(ctx context.Context, c memberLister) (int, error) {
	ml, err := c.MemberList(ctx, clientv3.WithSerializable())
	if err != nil {
		return 0, err
	}
	return len(ml.Members), nil
}

// addSettling runs add until it succeeds, fails with an error retryable does
// not accept, or memberAddSettle runs out. Every error retryable accepts must
// be a refusal that added nothing, or a retry would add the member twice.
func addSettling(w io.Writer, add func(context.Context) (*clientv3.MemberAddResponse, error), retryable func(error) bool, note string) (*clientv3.MemberAddResponse, error) {
	for deadline := time.Now().Add(memberAddSettle); ; {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		resp, err := add(ctx)
		cancel()
		if !retryable(err) || time.Now().After(deadline) {
			return resp, err
		}
		fmt.Fprintln(w, note)
		time.Sleep(3 * time.Second)
	}
}

// MemberRemove drops a member by its peer URL (rollback on a failed join),
// through any of cpIPs' etcd. The join's rollback uses the same published
// endpoints as its MemberAdd: with the --join host being a VIP, removing
// through it would fail exactly when a rollback is needed.
func MemberRemove(cpIPs []string, peerURL string) error {
	cli, err := clusterClient(cpIPs)
	if err != nil {
		return err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ml, err := cli.MemberList(ctx)
	if err != nil {
		return err
	}
	for _, m := range ml.Members {
		for _, pu := range m.PeerURLs {
			if samePeerURL(pu, peerURL) {
				_, err := cli.MemberRemove(ctx, m.ID)
				return err
			}
		}
	}
	return nil
}

// samePeerURL reports whether a and b name the same etcd peer endpoint. The
// peer URL is a member's identity (memberByPeerURL, MemberRemove), and the
// same address has several valid spellings: an IPv6 literal is bracketed or
// not, its hex digits either case, its zero runs compressed or not. The
// comparison is therefore the parsed host and port — IP hosts by value, names
// case-insensitively — and a URL that does not parse matches only its own
// exact spelling.
func samePeerURL(a, b string) bool {
	ha, pa, err1 := peerHostPort(a)
	hb, pb, err2 := peerHostPort(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return pa == pb && (sameIP(ha, hb) || strings.EqualFold(ha, hb))
}

// peerHostPort splits a peer URL into its host (brackets stripped) and port.
// A URL with no port parses with an empty one, which then matches no port.
func peerHostPort(raw string) (host, port string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	return u.Hostname(), u.Port(), nil
}

// JoinBundleEtcdKey is where the apiserver stores the stratum-certs Secret, the
// encrypted shared-cert bundle a control-plane join downloads. Its existence is
// what tells an etcd reader that a join can still be made with it.
//
// This couples to the apiserver's storage layout, not to any API: "/registry"
// is kube-apiserver's default --etcd-prefix, which staticpod's apiserver args
// do not override, and "secrets/<namespace>/<name>" is how it keys a Secret
// under that prefix. A change to either one makes this key name nothing, and
// the bundle gate then reads every cluster as having no bundle published --
// which is why internal/cluster checks it against a database written by the
// etcd this repo ships.
const JoinBundleEtcdKey = "/registry/secrets/kube-system/" + certsSecret

// JoinState reports, from the etcd at cpIP, how many members (learners
// included) it has and whether a control-plane join bundle is published. A
// learner counts: it is a control plane that is joining, and it has whatever
// at-rest key the cluster had when it downloaded the shared certs. Both reads
// are linearizable (the client's default), so neither is a stale answer from a
// lagging member.
func JoinState(cpIP string) (members int, bundle bool, err error) {
	cli, err := etcdClient(cpIP)
	if err != nil {
		return 0, false, err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ml, err := cli.MemberList(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("etcd member list: %w", err)
	}
	got, err := cli.Get(ctx, JoinBundleEtcdKey, clientv3.WithCountOnly())
	if err != nil {
		return 0, false, fmt.Errorf("etcd get %s: %w", JoinBundleEtcdKey, err)
	}
	return len(ml.Members), got.Count > 0, nil
}

// encryptionKeyProofEntry is the stratum-certs data key that carries the
// bundle's at-rest-key proof. The bundle's own ciphertext authenticates the
// certificate-key it was sealed under, but the CIS key gate (internal/cluster)
// holds no certificate-key — what it holds is this node's at-rest key. So an
// upload whose bundle carries the at-rest key ALSO publishes this entry, sealed
// under the raw 32 bytes the at-rest key's base64 encodes: an AES-GCM open of
// it proves the publishing upload held that exact key. Provenance from
// ciphertext, never from an annotation — an annotation is writable by anyone
// who can write the Secret, so it authenticates nothing.
//
// The name deliberately stays outside the certs- grant-entry prefix: the
// grant prune (expiredGrantEntries, run by every mint and by the operator)
// treats every certs-<suffix> entry as a grant and would drop the proof from
// under the bundle it vouches for.
const encryptionKeyProofEntry = "encryption-key-proof"

// PublishedBundleCarriesKey answers, over the Secret API, whether a
// control-plane join bundle is published and whether it carries this node's CIS
// at-rest encryption key. It is the bundle owner's answer to the question the
// controller-start key gate (internal/cluster) asks, and it answers from the
// same Secret a join will actually download —
// not from the apiserver's storage key for it, which can trail what the API
// serves.
//
// "Published" is the Secret's existence, never a judgment of its entries: the
// multi-entry grant Secret carries one ciphertext entry per
// live grant beside the legacy certs entry, and every such state is a bundle a
// join can be made with. "Carries the key" is a decryption, not a label: an
// upload whose bundle carries the at-rest key publishes the proof entry sealed
// under that key, so the open — not an annotation anyone with Secret write can
// forge — says whether this node's key is the one a joiner would receive. A
// proof that does not open under this node's key is the answer "unkeyed", not
// an error: it is exactly what a bundle uploaded under another key, a keyless
// upload, or one from a stratum that predated the proof looks like. A Secret
// that cannot be read is an error, never an answer, because either published
// answer it could fake is wrong on one side.
//
// encryptionConfig is this node's encryption-provider config — the bytes of
// version.EncryptionConfigPath — or nil when this node holds no at-rest key
// yet. Nil is the first gate's case: there is nothing to match the proof
// against, so a published bundle can only answer unkeyed.
func PublishedBundleCarriesKey(cs kubernetes.Interface, encryptionConfig []byte) (published, carries bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sec, err := cs.CoreV1().Secrets("kube-system").Get(ctx, certsSecret, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("read the %s Secret to learn whether a join bundle is published: %w", certsSecret, err)
	}
	if len(encryptionConfig) == 0 {
		return true, false, nil
	}
	raw, err := atRestKeyFromEncryptionConfig(encryptionConfig)
	if err != nil {
		return true, false, fmt.Errorf("read the at-rest key out of this node's encryption-provider config, to match it against the published bundle: %w", err)
	}
	proof, ok := sec.Data[encryptionKeyProofEntry]
	if !ok {
		return true, false, nil
	}
	return true, atRestKeyProofOpens(proof, raw), nil
}

// atRestKeyFromEncryptionConfig returns the raw 32 bytes the base64 secret in
// this node's EncryptionConfiguration encodes — the secretbox key the apiserver
// encrypts stored Secrets with (hardening.EncryptionConfigYAML's shape), and
// so the key a keyed bundle hands out and its proof is sealed under. The
// primary key is taken strictly: the first secretbox key of the entry covering
// "secrets". A config that does not carry exactly that is an error rather than
// a guess, because opening a proof under the wrong bytes would answer "unkeyed"
// for a bundle this node's key really does carry.
func atRestKeyFromEncryptionConfig(config []byte) ([]byte, error) {
	var cfg struct {
		Resources []struct {
			Resources []string `json:"resources"`
			Providers []struct {
				Secretbox *struct {
					Keys []struct {
						Secret string `json:"secret"`
					} `json:"keys"`
				} `json:"secretbox"`
			} `json:"providers"`
		} `json:"resources"`
	}
	if err := yaml.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("parse the encryption-provider config: %w", err)
	}
	for _, res := range cfg.Resources {
		if !slices.Contains(res.Resources, "secrets") {
			continue
		}
		for _, prov := range res.Providers {
			if prov.Secretbox == nil || len(prov.Secretbox.Keys) == 0 {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(prov.Secretbox.Keys[0].Secret)
			if err != nil {
				return nil, fmt.Errorf("decode the encryption-provider config's secretbox key: %w", err)
			}
			if len(raw) != 32 {
				return nil, fmt.Errorf("the encryption-provider config's secretbox key decodes to %d bytes, not the 32 a secretbox key is", len(raw))
			}
			return raw, nil
		}
	}
	return nil, errors.New("the encryption-provider config names no secretbox key for secrets")
}

// atRestKeyProofOpens reports whether proof is an AES-GCM ciphertext that opens
// under raw — the open authenticating that the publishing upload held exactly
// this key, which is the provenance no annotation can carry.
func atRestKeyProofOpens(proof, raw []byte) bool {
	block, err := aes.NewCipher(raw)
	if err != nil {
		return false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return false
	}
	ns := gcm.NonceSize()
	if len(proof) < ns {
		return false
	}
	_, err = gcm.Open(nil, proof[:ns], proof[ns:], nil)
	return err == nil
}

// MemberClientIPs returns the host of every member's client URL (HAProxy backends).
func MemberClientIPs(cpIP string) ([]string, error) { return memberClientIPs(etcdClient(cpIP)) }

// EtcdStorage is the etcd at one address as a rollback needs it: the version
// its data is stored at, how many members its cluster has, and whether a
// downgrade is enabled.
type EtcdStorage struct {
	StorageVersion string
	Members        int
	Downgrading    bool
	// DowngradeTarget is the version an enabled downgrade is taking the
	// cluster to, "" when none is enabled.
	DowngradeTarget string
}

// EtcdStorageAt reads the storage version of the etcd member at cpIP, the size
// of its cluster and whether a downgrade is enabled. Both reads are the
// member's own view: a member of a cluster that has lost quorum still answers
// them, so it is not mistaken for one that is down.
func EtcdStorageAt(cpIP string) (EtcdStorage, error) {
	cli, err := etcdClient(cpIP)
	if err != nil {
		return EtcdStorage{}, err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := cli.Status(ctx, etcdEndpoint(cpIP))
	if err != nil {
		return EtcdStorage{}, fmt.Errorf("%w: %w", ErrEtcdNoStatus, err)
	}
	ml, err := cli.MemberList(ctx, clientv3.WithSerializable())
	if err != nil {
		return EtcdStorage{}, fmt.Errorf("etcd member list: %w", err)
	}
	out := EtcdStorage{StorageVersion: st.StorageVersion, Members: len(ml.Members)}
	if d := st.DowngradeInfo; d != nil && d.Enabled {
		out.Downgrading, out.DowngradeTarget = true, d.TargetVersion
	}
	return out, nil
}

// ErrEtcdNoStatus matches an EtcdStorageAt that failed because the member did
// not answer its status request, as opposed to a failure to set up the client
// or to read the membership of a member that did answer.
var ErrEtcdNoStatus = errors.New("etcd status")

// ErrDowngradeNotValidated matches a DowngradeEtcd that failed at its validate
// step, before it asked etcd to change anything.
var ErrDowngradeNotValidated = errors.New("etcd did not validate a downgrade")

// DowngradeEtcd asks the etcd cluster at cpIP to downgrade to target, a
// major.minor version: etcd checks that it can, then lowers the cluster
// version and migrates every member's storage down to it, so the older etcd
// can open the data. It is a cluster-wide operation, and it ends once every
// member runs the target version.
func DowngradeEtcd(cpIP, target string) error {
	cli, err := etcdClient(cpIP)
	if err != nil {
		return err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := cli.Downgrade(ctx, clientv3.DowngradeValidate, target); err != nil {
		return fmt.Errorf("%w to %s: %w", ErrDowngradeNotValidated, target, err)
	}
	if _, err := cli.Downgrade(ctx, clientv3.DowngradeEnable, target); err != nil {
		return fmt.Errorf("enable etcd's downgrade to %s: %w", target, err)
	}
	return nil
}

// MemberClientIPsVia is MemberClientIPs through any of several endpoints.
func MemberClientIPsVia(cpIPs []string) ([]string, error) {
	return memberClientIPs(clusterClient(cpIPs))
}

func memberClientIPs(cli *clientv3.Client, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ml, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ips []string
	for _, m := range ml.Members {
		for _, cu := range m.ClientURLs {
			u, err := url.Parse(cu)
			if err != nil {
				continue
			}
			h := u.Hostname()
			if h == "127.0.0.1" || h == "localhost" || seen[h] {
				continue
			}
			seen[h] = true
			ips = append(ips, h)
		}
	}
	return ips, nil
}

// MemberHostsVia returns one host per etcd member, learners included, read
// through any of several endpoints: the host of its first non-loopback client
// URL, or of its first peer URL when it has none. A member added but not yet
// started has published no client URL, and a caller accounting for every
// member must still see it. A member with neither is an error, since it
// cannot be placed on any node.
func MemberHostsVia(cpIPs []string) ([]string, error) {
	cli, err := clusterClient(cpIPs)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ml, err := cli.MemberList(ctx)
	if err != nil {
		return nil, err
	}
	return memberHosts(toEtcdMembers(ml.Members))
}

func memberHosts(ms []etcdMember) ([]string, error) {
	seen := map[string]bool{}
	var hosts []string
	for _, m := range ms {
		h := firstHost(m.ClientURLs, true)
		if h == "" {
			h = firstHost(m.PeerURLs, false)
		}
		if h == "" {
			return nil, fmt.Errorf("etcd member %x has no client or peer URL naming a host", m.ID)
		}
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts, nil
}

func firstHost(urls []string, skipLoopback bool) string {
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		h := u.Hostname()
		if h == "" || skipLoopback && (h == "127.0.0.1" || h == "localhost") {
			continue
		}
		return h
	}
	return ""
}

// etcdMember is the part of an etcd member record snapshot rejoin decides on.
// Name is empty until the member has started and published itself,
// which is how "added but never came up" is told apart from a running member.
type etcdMember struct {
	ID         uint64
	Name       string
	PeerURLs   []string
	ClientURLs []string
	IsLearner  bool
}

// etcdMembership is one member list, with the cluster it was read from.
//
// Quorum records whether the list came from a linearizable read. A member list
// read without quorum may be stale, so it is allowed to authorise only
// rejoinResume, which changes nothing. Responder is the member that answered.
type etcdMembership struct {
	ClusterID uint64
	Responder uint64
	Quorum    bool
	Members   []etcdMember
}

// memberByPeerURL is the member that advertises peerURL, or nil. The peer URL
// and not the name is the identity: an unstarted member has no name yet. The
// match is the parsed host and port (samePeerURL), so a lookup URL spelled
// differently from the recorded one still finds the member.
func (m etcdMembership) memberByPeerURL(peerURL string) *etcdMember {
	for i := range m.Members {
		for _, pu := range m.Members[i].PeerURLs {
			if samePeerURL(pu, peerURL) {
				return &m.Members[i]
			}
		}
	}
	return nil
}

func toEtcdMembers(ms []*etcdserverpb.Member) []etcdMember {
	out := make([]etcdMember, 0, len(ms))
	for _, m := range ms {
		out = append(out, etcdMember{ID: m.ID, Name: m.Name, PeerURLs: m.PeerURLs,
			ClientURLs: m.ClientURLs, IsLearner: m.IsLearner})
	}
	return out
}

// memberList reads the membership from the etcd at cpIP alone. No endpoint
// sync: rejoin asks the seed it was pointed at, and only that one.
func memberList(cpIP string) (etcdMembership, error) {
	cli, err := etcdClient(cpIP)
	if err != nil {
		return etcdMembership{}, err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ml, err := cli.MemberList(ctx)
	quorum := err == nil
	if err != nil {
		// A linearizable list needs a quorum read index, so it cannot answer
		// from the cluster rejoin exists to recover.
		sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer scancel()
		sml, serr := cli.MemberList(sctx, clientv3.WithSerializable())
		if serr != nil {
			return etcdMembership{}, err
		}
		ml = sml
	}
	return etcdMembership{ClusterID: ml.Header.ClusterId, Responder: ml.Header.MemberId, Quorum: quorum,
		Members: toEtcdMembers(ml.Members)}, nil
}

// initialClusterFor is the ETCD_INITIAL_CLUSTER a member joining with
// state=existing must boot with: every member, the joining one included,
// since etcd refuses to start when the counts differ. An unstarted member has
// no name; the joining one is given selfName, and any other gets a placeholder
// (etcd matches the list against the cluster by peer URL, so the label only has
// to be non-empty).
func initialClusterFor(members []etcdMember, selfPeerURL, selfName string) string {
	var parts []string
	for _, m := range members {
		name := m.Name
		for _, pu := range m.PeerURLs {
			if pu == selfPeerURL {
				name = selfName
			}
		}
		if name == "" {
			name = fmt.Sprintf("unstarted-%x", m.ID)
		}
		for _, pu := range m.PeerURLs {
			parts = append(parts, name+"="+pu)
		}
	}
	return strings.Join(parts, ",")
}

// isTooManyLearners is etcd refusing a second learner: it allows one at a
// time, so it means another control plane's rejoin is still catching up.
func isTooManyLearners(err error) bool {
	return isEtcdErr(err, rpctypes.ErrTooManyLearners)
}

// memberAddLearner adds peerURL to the cluster at cpIP as a non-voting learner
// and returns its member ID and the initial-cluster string it must boot with.
//
// A learner, not a voter, because of what a voting add does to a restored
// cluster: the seed is a ONE-member cluster, and adding a voter makes
// it two, with a quorum of two, before the new member has even started. Until
// it comes up the cluster can commit nothing -- including the MemberRemove that
// the old re-run advice relied on to undo it. A learner does not count towards
// quorum, so the seed keeps serving however long this node takes, and
// removing it is always possible.
func memberAddLearner(w io.Writer, cpIP, name, peerURL string) (uint64, string, error) {
	cli, err := etcdClient(cpIP)
	if err != nil {
		return 0, "", err
	}
	defer cli.Close()
	resp, err := addSettling(w, func(ctx context.Context) (*clientv3.MemberAddResponse, error) {
		return cli.MemberAddAsLearner(ctx, []string{peerURL})
	}, func(err error) bool { return isUnhealthyCluster(err) || isTooManyLearners(err) },
		"stratum snapshot rejoin: etcd is not accepting a new learner yet "+
			"(a member is still settling, or another control plane's rejoin is still catching up); retrying")
	if isTooManyLearners(err) {
		return 0, "", fmt.Errorf("etcd learner add: %w (another control plane's rejoin is still catching up; "+
			"etcd allows one learner at a time, so rejoin control planes one after another)", err)
	}
	if err != nil {
		return 0, "", fmt.Errorf("etcd learner add: %w", err)
	}
	return resp.Member.ID, initialClusterFor(toEtcdMembers(resp.Members), peerURL, name), nil
}

// memberRemoveID removes one member, through the etcd at cpIP.
func memberRemoveID(cpIP string, id uint64) error {
	cli, err := etcdClient(cpIP)
	if err != nil {
		return err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := cli.MemberRemove(ctx, id); err != nil {
		return fmt.Errorf("etcd member remove: %w", err)
	}
	return nil
}

// memberPromote makes learner id a voter: one attempt, through the etcd at
// cpIPs. No endpoint sync: the synced list would include the learner itself,
// which refuses the promote.
func memberPromote(id uint64, cpIPs ...string) error {
	cli, err := etcdClient(cpIPs...)
	if err != nil {
		return err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := cli.MemberPromote(ctx, id); err != nil {
		return fmt.Errorf("etcd member promote: %w", err)
	}
	return nil
}

// isPromoteNotYet is a promote that a later attempt can still land: etcd
// refusing it outright, or not answering at all. A promote is idempotent for
// this caller -- one that already landed comes back as isMemberNotLearner --
// so an indeterminate answer is retried rather than failed. ErrUnhealthy is
// deliberately absent: etcd never answers a promote with it.
func isPromoteNotYet(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	for _, t := range []error{
		rpctypes.ErrMemberLearnerNotReady,
		rpctypes.ErrMemberNotEnoughStarted,
		rpctypes.ErrNoLeader,
		rpctypes.ErrNotLeader,
		rpctypes.ErrLeaderChanged,
		rpctypes.ErrTimeout,
		rpctypes.ErrTimeoutWaitAppliedIndex,
	} {
		if isEtcdErr(err, t) {
			return true
		}
	}
	return false
}

// isMemberNotLearner is a promote of a member that is already a voter: an
// earlier promote landed and its answer was lost, which is success.
func isMemberNotLearner(err error) bool {
	return isEtcdErr(err, rpctypes.ErrMemberNotLearner)
}

// localEtcdStatus is what this node's own etcd says about itself.
type localEtcdStatus struct {
	ClusterID, MemberID, Leader, AppliedIndex uint64
	IsLearner                                 bool
}

// memberStatusAt asks the etcd at ip about itself. A learner answers Status
// once it is ready, so this works before promotion.
func memberStatusAt(ip string) (localEtcdStatus, error) {
	cli, err := etcdClient(ip)
	if err != nil {
		return localEtcdStatus{}, err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := cli.Status(ctx, etcdEndpoint(ip))
	if err != nil {
		return localEtcdStatus{}, err
	}
	return localEtcdStatus{ClusterID: st.Header.ClusterId, MemberID: st.Header.MemberId, Leader: st.Leader,
		AppliedIndex: st.RaftAppliedIndex, IsLearner: st.IsLearner}, nil
}

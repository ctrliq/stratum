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

// Not unioned with the Kubernetes bundle: a root-signed leaf could impersonate etcd.
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

// Same builder as the member's advertised client URL, so checks cannot drift from it.
func etcdEndpoint(cpIP string) string {
	return nodestate.EtcdClientURL(cpIP)
}

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

// Sync dials members that joined after the addresses were published; best-effort.
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

var memberAddSettle = 90 * time.Second

// Matches through any %w wrapping; the second arm converts a raw server-side status.
func isEtcdErr(err error, target error) bool {
	return errors.Is(err, target) || errors.Is(rpctypes.Error(err), target)
}

func isUnhealthyCluster(err error) bool {
	return isEtcdErr(err, rpctypes.ErrUnhealthy)
}

func isNotEnoughStarted(err error) bool {
	return isEtcdErr(err, rpctypes.ErrMemberNotEnoughStarted)
}

// Each refusal committed nothing; possibly-lost replies are deliberately absent.
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

// A learner, not a voter: a voter added to a one-member cluster needs both for quorum.
func MemberAdd(cpIPs []string, newName, newPeerURL string) (string, error) {
	cli, err := clusterClient(cpIPs)
	if err != nil {
		return "", err
	}
	defer cli.Close()
	resp, err := addSettling(os.Stdout, func(ctx context.Context) (*clientv3.MemberAddResponse, error) {
		return cli.MemberAddAsLearner(ctx, []string{newPeerURL})
	}, memberAddSettling,
		"stratum HA join: etcd is not accepting a new member yet (the last one is still settling or catching up); retrying")
	if err != nil {
		return "", fmt.Errorf("etcd member add: %w", err)
	}
	return initialCluster(resp.Members, newName), nil
}

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

type memberLister interface {
	MemberList(context.Context, ...clientv3.OpOption) (*clientv3.MemberListResponse, error)
}

// Serializable: a member that joined and left leaves the seed without quorum.
func countMembers(ctx context.Context, c memberLister) (int, error) {
	ml, err := c.MemberList(ctx, clientv3.WithSerializable())
	if err != nil {
		return 0, err
	}
	return len(ml.Members), nil
}

// Every retryable error must be a refusal that added nothing, or a retry adds twice.
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

// Rollback on a failed join; the --join host is a VIP, so remove via the add's endpoints.
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

// One address, several spellings (IPv6 brackets, case, zero runs); compare parsed.
func samePeerURL(a, b string) bool {
	ha, pa, err1 := peerHostPort(a)
	hb, pb, err2 := peerHostPort(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return pa == pb && (sameIP(ha, hb) || strings.EqualFold(ha, hb))
}

func peerHostPort(raw string) (host, port string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	return u.Hostname(), u.Port(), nil
}

// Couples to kube-apiserver's default --etcd-prefix and Secret key layout, not any API.
const JoinBundleEtcdKey = "/registry/secrets/kube-system/" + certsSecret

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

// Sealed under the at-rest key, never a forgeable annotation; off certs- so the prune spares it.
const encryptionKeyProofEntry = "encryption-key-proof"

// Answers from the Secret the API serves, not its etcd key; an unopenable proof means unkeyed.
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

// Strict: opening a proof under wrong bytes answers unkeyed for a keyed bundle.
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

// The host of every member's client URL (HAProxy backends).
func MemberClientIPs(cpIP string) ([]string, error) { return memberClientIPs(etcdClient(cpIP)) }

type EtcdStorage struct {
	StorageVersion  string
	Members         int
	Downgrading     bool
	DowngradeTarget string
}

// The member's own view, so one on a quorum-lost cluster is not mistaken for down.
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

var ErrEtcdNoStatus = errors.New("etcd status")

var ErrDowngradeNotValidated = errors.New("etcd did not validate a downgrade")

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

// A member added but not started has no client URL; its peer URL still places it.
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

// Name is empty until the member started: how added-but-never-up is told apart.
type etcdMember struct {
	ID         uint64
	Name       string
	PeerURLs   []string
	ClientURLs []string
	IsLearner  bool
}

// Without quorum the list may be stale; it may authorise only a no-op resume.
type etcdMembership struct {
	ClusterID uint64
	Responder uint64
	Quorum    bool
	Members   []etcdMember
}

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

// No endpoint sync: rejoin asks the seed it was pointed at, and only that one.
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
		// A linearizable list needs a quorum read index, which this cluster cannot give.
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

// Etcd refuses to start when the counts differ; labels are matched by peer URL.
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

func memberAddSettling(err error) bool {
	return isUnhealthyCluster(err) || isTooManyLearners(err)
}

func isTooManyLearners(err error) bool {
	return isEtcdErr(err, rpctypes.ErrTooManyLearners)
}

// A learner, not a voter: a voter on a one-member seed makes quorum 2 before it starts.
func memberAddLearner(w io.Writer, cpIP, name, peerURL string) (uint64, string, error) {
	cli, err := etcdClient(cpIP)
	if err != nil {
		return 0, "", err
	}
	defer cli.Close()
	resp, err := addSettling(w, func(ctx context.Context) (*clientv3.MemberAddResponse, error) {
		return cli.MemberAddAsLearner(ctx, []string{peerURL})
	}, memberAddSettling,
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

// No endpoint sync: the synced list includes the learner itself, which refuses promote.
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

// A landed promote returns isMemberNotLearner; ErrUnhealthy absent: etcd never answers one with it.
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

func isMemberNotLearner(err error) bool {
	return isEtcdErr(err, rpctypes.ErrMemberNotLearner)
}

type localEtcdStatus struct {
	ClusterID, MemberID, Leader, AppliedIndex uint64
	IsLearner                                 bool
}

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

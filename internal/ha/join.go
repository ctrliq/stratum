package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/internal/addons"
	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/bootstrap"
	"github.com/ctrliq/stratum/internal/certinputs"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/kubeconfig"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/profilefiles"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/version"
)

// JoinOptions for `stratum server --server --token --certificate-key`.
type JoinOptions struct {
	Server  string
	Token   string
	CAHash  string
	CertKey string
	NodeIP  string
	// spec.network.nodeIP, kept apart from NodeIP so the join logs which one won.
	ConfigNodeIP string
	Operator     bool // the stratum-operator host service here too (leader-elected)
	CertRenew    bool // install the cert auto-renew timer here too
	// The node profile's name ("" for none); CIS is its legacy spelling.
	Profile string
	CIS     bool
	// This control plane's spec.selinux: false.
	SELinuxOptOut bool
	VIP           nodestate.VIP
	// spec.network pod/service CIDRs; a value differing from the cluster's refuses the join.
	PodCIDR     string
	ServiceCIDR string
	// spec.network.clusterDomain; a value differing from the cluster's refuses the join.
	ClusterDomain string
	// spec.network.cilium; a value differing from the cluster's refuses the join.
	Cilium addons.CiliumSettings
	// spec.node, saved with the node state and rendered into the node's registration.
	NodeLabels      map[string]string
	NodeTaints      []string
	RuntimeHandlers map[string]string
}

// Refuses a differing spec.network.cilium: two blocks would swap the config on every leader change.
func checkJoinCilium(configured addons.CiliumSettings) error {
	b, err := os.ReadFile(version.CiliumSettingsPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read the cluster's Cilium settings: %w", err)
	}
	cluster, err := addons.DecodeCiliumSettings(b)
	if err != nil {
		return fmt.Errorf("this build cannot read the Cilium settings the cluster's shared certs carry (uploaded by "+
			"the control plane that created this join token): %w; join with the stratum version the cluster's control "+
			"planes run", err)
	}
	if !bytes.Equal(cluster.Encode(), configured.Encode()) {
		return fmt.Errorf("the cluster runs spec.network.cilium %s, but this control plane's config has %s; "+
			"every control plane must carry the same block, so make this one match before joining",
			describeCilium(cluster), describeCilium(configured))
	}
	return nil
}

func describeCilium(s addons.CiliumSettings) string {
	if s.IsDefault() {
		return "unset (the shipped settings)"
	}
	return string(s.Encode())
}

// Runs before certificates or etcd: the node's first restart would refuse a domain let through.
func checkJoinDomain(configured string) error {
	if _, err := netcfg.ResolveDomain(configured); err != nil {
		return fmt.Errorf("HA join: %w (no certificates were issued and etcd is untouched)", err)
	}
	return nil
}

// A differing lone range would be dropped for the cluster's, a pair would replace them here.
func checkJoinNetwork(podCIDR, serviceCIDR string) error {
	if podCIDR == "" && serviceCIDR == "" {
		return nil
	}
	recorded, err := netcfg.Load()
	if err != nil {
		return fmt.Errorf("HA join: %w (no certificates were issued and etcd is untouched)", err)
	}
	for _, f := range []struct{ field, name, configured, recorded string }{
		{"spec.network.podCIDR", "pod CIDR", podCIDR, recorded.PodCIDR},
		{"spec.network.serviceCIDR", "service CIDR", serviceCIDR, recorded.ServiceCIDR},
	} {
		if f.configured != "" && !netcfg.EqualCIDRs(f.configured, f.recorded) {
			return fmt.Errorf("HA join: %s %q does not match this cluster's %s %q recorded in %s: "+
				"the cluster's CIDRs cannot be changed after init; remove the field or set it to %q "+
				"(no certificates were issued and etcd is untouched)",
				f.field, f.configured, f.name, f.recorded, version.NetConfigPath, f.recorded)
		}
	}
	return nil
}

// Even non-member residue must be inspected before shared certificates are overwritten.
func refuseRetainedMember(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("HA join: cannot inspect %s (%w); refusing to overwrite its data or trust material", dir, err)
	}
	if len(entries) == 0 {
		return nil
	}
	return fmt.Errorf("HA join: %s holds etcd data or residue; refusing to overwrite its data or trust material. "+
		"Repair the saved node state and start with `stratum up` and no --join. To intentionally rejoin, "+
		"identify and remove the old member, if still listed, with `stratum etcdctl member remove <ID>` on a healthy "+
		"control plane, then run `stratum uninstall --yes` locally to clear its data", dir)
}

// os.Remove is atomic against a concurrent create, so a member appearing after the check survives.
func prepareEmptyEtcdDataDir(dir string) error {
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("HA join: %s is not empty or cannot be removed (%w); refusing to delete its data", dir, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("HA join: create empty etcd data directory %s: %w", dir, err)
	}
	return nil
}

// Refuses a bundle whose profile or shared secrets differ from this node's, either direction.
func requireSharedSecrets(prof profile.Profile, b bundleProfile) error {
	if b.ProfileKnown && b.Profile != prof.Name {
		return fmt.Errorf("the control plane that published this join's shared certificates runs %s, and this "+
			"control plane would run %s: every control plane of a cluster runs the same profile. Nothing has been "+
			"added to etcd. Re-run this join with %s", profileNoun(b.Profile), profileNoun(prof.Name), profileFlag(b.Profile))
	}
	want := prof.SecretNames()
	if slices.Equal(want, b.Secrets) {
		return nil
	}
	cisKey := []string{encryptionConfigEntry}
	switch {
	case len(want) == 0 && slices.Equal(b.Secrets, cisKey):
		return fmt.Errorf("the shared certificates this join downloaded carry an at-rest encryption key, so this " +
			"cluster runs the CIS profile, and a control plane joining it without --profile cis would neither read its " +
			"encrypted Secrets nor encrypt the ones it writes. Nothing has been added to etcd. Re-run " +
			"this join with --profile cis (or spec.profile: cis)")
	case slices.Equal(want, cisKey) && len(b.Secrets) == 0:
		return fmt.Errorf("--profile cis: the shared certificates this join downloaded carry no at-rest encryption key, so "+
			"this control plane would run with a key no other has, or none at all — Secrets would not be readable "+
			"across the control planes. Nothing has been added to etcd. Either the cluster does not run "+
			"the CIS profile, and this node should join it without --profile cis or --cis (and without spec.profile: cis or spec.cis: true); or it is "+
			"meant to, and CIS has to be turned on there first (see the Stratum PRO documentation, "+
			"%s), after which `stratum token create --control-plane` on a control plane that "+
			"has %s gives a join command whose bundle carries the key",
			profile.ProDocsURL, version.EncryptionConfigPath)
	case len(want) == 0:
		return fmt.Errorf("the shared certificates this join downloaded carry the shared secrets %s, so this cluster "+
			"runs a node profile that declares them, and a control plane joining it without that profile would not "+
			"hold them. Nothing has been added to etcd. Re-run this join with the cluster's --profile",
			strings.Join(b.Secrets, ", "))
	}
	carried := "none of them"
	if len(b.Secrets) > 0 {
		carried = strings.Join(b.Secrets, ", ")
	}
	return fmt.Errorf("profile %s: the shared certificates this join downloaded carry %s of the shared secrets "+
		"the profile declares (%s), so this control plane would hold secrets no other control plane has, or none "+
		"— the control planes must share them byte for byte. Nothing has been added to etcd. Either the cluster "+
		"does not run this profile, and this node should join it with the cluster's; or it is meant to, and the "+
		"profile has to be on there first, after which `stratum token create --control-plane` on a control plane "+
		"that holds them gives a join command whose bundle carries them",
		prof.Name, carried, strings.Join(want, ", "))
}

func profileNoun(name string) string {
	if name == "" {
		return "no node profile"
	}
	return fmt.Sprintf("the %s profile", name)
}

func profileFlag(name string) string {
	if name == "" {
		return "no --profile or --cis (and no spec.profile or spec.cis: true)"
	}
	return "--profile " + name
}

// Leaves exactly the bundle's shared secrets on this node, removing and reporting the rest.
func settleJoinSecrets(prof profile.Profile, b bundleProfile, applied bool, dir string, out io.Writer) error {
	mismatch := requireSharedSecrets(prof, b)
	keep := map[string]bool{}
	if mismatch == nil {
		for _, n := range b.Secrets {
			keep[n] = true
		}
	}
	names := append(profile.SharedSecretNames(), prof.SecretNames()...)
	slices.Sort(names)
	errs := []error{mismatch}
	var removed []string
	for _, name := range slices.Compact(names) {
		if keep[name] {
			continue
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			errs = append(errs, fmt.Errorf("HA join: inspect %s: %w", path, err))
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("HA join: remove %s, which this join must not keep: %w", path, err))
			continue
		}
		if err := syncJoinKeyDir(dir); err != nil {
			errs = append(errs, fmt.Errorf("HA join: removed %s, which this join must not keep, but the removal "+
				"could not be made durable (%w): after a power loss it could return, so check that it is absent "+
				"before retrying", path, err))
			continue
		}
		if applied && slices.Contains(b.Secrets, name) {
			fmt.Fprintf(out, "stratum HA join: removed %s: this join is refused, so it does not keep the %s it downloaded\n", path, secretNoun(name))
			removed = append(removed, fmt.Sprintf("the %s this join downloaded was removed from %s", secretNoun(name), path))
			continue
		}
		why := "the shared certificates do not carry it"
		if !applied {
			why = "this join is refused before it writes the shared certificates"
		}
		fmt.Fprintf(out, "stratum HA join: removed %s: %s, so it was left "+
			"by an earlier attempt on this node and is not this cluster's\n", path, why)
		removed = append(removed, fmt.Sprintf("a %s left by an earlier attempt on this node was removed from %s", secretNoun(name), path))
	}
	err := errors.Join(errs...)
	if err != nil && mismatch != nil && len(removed) > 0 && len(errs) == 1 {
		return fmt.Errorf("%w (%s)", mismatch, strings.Join(removed, "; "))
	}
	return err
}

func secretNoun(name string) string {
	if name == encryptionConfigEntry {
		return "key"
	}
	return "shared secret " + name
}

// A var so a test can fail the sync.
var syncJoinKeyDir = host.SyncDir

// A var so a test can fail it; the intent directory must be synced after the rename.
var syncJoinIntentDir = host.SyncDir

// Where the join reports operator-visible decisions; a var so tests can assert the output.
var joinOut io.Writer = os.Stdout

// Inside version.K8sConfDir, beside node.yaml, so uninstall's sweep clears it.
const intentFileName = "join-intent.json"

// The durable pre-add intent journal; a var so tests can redirect it to a temp file.
var joinIntentPath = filepath.Join(version.K8sConfDir, intentFileName)

// The journal's phase before and after the cluster's member list has answered.
const (
	intentPhasePrepared = "prepared"
	intentPhaseAdded    = "added"
)

// Read by a re-run of `stratum up --join` and by an operator, so it stays plain JSON.
type joinIntent struct {
	MemberName string        `json:"memberName"`
	PeerURL    string        `json:"peerURL"`
	MemberID   uint64        `json:"memberID"`
	Phase      string        `json:"phase"`
	Recovery   *joinRecovery `json:"recovery,omitempty"`
}

// Lets a committed member start when the cluster has no quorum; no credentials, only a CA pin.
type joinRecovery struct {
	State   nodestate.NodeState `json:"state"`
	EtcdIPs []string            `json:"etcdIPs"`
	CAHash  string              `json:"caHash"`
	// Still written beside CarriedSecrets so an older binary can resume this journal.
	CarriedKey     bool     `json:"carriedKey"`
	CarriedSecrets []string `json:"carriedSecrets,omitempty"`
}

func (r joinRecovery) carried() []string {
	if r.CarriedSecrets == nil && r.CarriedKey {
		return []string{encryptionConfigEntry}
	}
	return r.CarriedSecrets
}

// Unreadable or memberless intents are errors, never nil: absence would run a second member-add.
func readJoinIntent() (*joinIntent, error) {
	data, err := os.ReadFile(joinIntentPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the join intent file %s: %w; inspect it, and remove it by hand only once "+
			"the member it records is no longer in the cluster's member list (`stratum etcdctl member list`)",
			joinIntentPath, err)
	}
	var j joinIntent
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("the join intent file %s is not valid JSON (%v); inspect it, and remove it by "+
			"hand only once the member it records is no longer in the cluster's member list (`stratum etcdctl member list`)",
			joinIntentPath, err)
	}
	if j.MemberName == "" || j.PeerURL == "" {
		return nil, fmt.Errorf("the join intent file %s names no member (name %q, peer %q); inspect it, and "+
			"remove it by hand only once the member it records is no longer in the cluster's member list "+
			"(`stratum etcdctl member list`)", joinIntentPath, j.MemberName, j.PeerURL)
	}
	return &j, nil
}

// Durable through a crash: a lost journal reads as "no unfinished join".
func writeJoinIntent(j joinIntent) error {
	data, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("record the join intent in %s: %w", joinIntentPath, err)
	}
	dir := filepath.Dir(joinIntentPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("record the join intent in %s: %w", joinIntentPath, err)
	}
	f, err := os.CreateTemp(dir, "."+intentFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("record the join intent in %s: %w", joinIntentPath, err)
	}
	tmp := f.Name()
	// os.CreateTemp made the staging file 0600; the umask cannot widen it.
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("record the join intent in %s: %w", joinIntentPath, err)
	}
	// A rename with dirty data can land the directory entry before the content is on disk.
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("record the join intent in %s: %w", joinIntentPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("record the join intent in %s: %w", joinIntentPath, err)
	}
	if err := os.Rename(tmp, joinIntentPath); err != nil {
		return fmt.Errorf("record the join intent in %s: %w", joinIntentPath, err)
	}
	if err := syncJoinIntentDir(dir); err != nil {
		return fmt.Errorf("the join intent in %s could not be made durable: %w", joinIntentPath, err)
	}
	return nil
}

// Best-effort and never silent; the node-state save does not end the journal.
func clearJoinIntent() {
	if err := os.Remove(joinIntentPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Printf("stratum HA join: WARN: clear the join intent file %s: %v — the next join will reconcile it "+
			"as an unfinished one\n", joinIntentPath, err)
	}
}

// Ends the intent journal once a later start has confirmed the node's local etcd data.
func ClearJoinIntent() error {
	path := JoinIntentPath()
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear the join intent file %s: %w", path, err)
	}
	return nil
}

// JoinIntentPath is the intent journal's path in this node's state directory.
func JoinIntentPath() string {
	return filepath.Join(version.K8sConfDir, intentFileName)
}

// A bare `stratum up` that treated the file as absent would bootstrap over the recorded membership.
func JoinIntentSurvives() (bool, error) {
	switch _, err := os.Stat(JoinIntentPath()); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("inspect the join intent file %s: %w; it may be the recovery record of an "+
			"unfinished join, so inspect it, and remove it by hand only once the member it records is no longer "+
			"in the cluster's member list (`stratum etcdctl member list`)", JoinIntentPath(), err)
	}
}

func joinMembership(cpIPs []string) (etcdMembership, error) {
	return joinMembershipVia(cpIPs, memberList)
}

func joinMemberRemoveByID(cpIPs []string, id uint64) error {
	if len(cpIPs) == 0 {
		return errors.New("no etcd endpoints to remove the member through")
	}
	var last error
	for _, ip := range cpIPs {
		if err := memberRemoveID(ip, id); err != nil {
			last = err
			continue
		}
		return nil
	}
	return last
}

// A linearizable member list needs quorum to answer, so its success is the proof; errors refuse.
func joinQuorumHeld(cpIPs []string) error {
	cli, err := clusterClient(cpIPs)
	if err != nil {
		return fmt.Errorf("cannot reach etcd to confirm the cluster's quorum: %w", err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := cli.MemberList(ctx); err != nil {
		return fmt.Errorf("the cluster's member list does not answer a quorum read (%v), so it cannot be "+
			"confirmed that removing the member will not take away the cluster's majority", err)
	}
	return nil
}

const concurrentJoinHint = "if another control plane is joining now, wait until it is Ready and re-run this join; " +
	"join controllers one at a time"

// The post-add check catches a local writer racing the request and rolls the add back.
func addMemberIfEtcdDataDirEmpty(dir string, add func() (string, error), remove func() error) (string, error) {
	if err := refuseRetainedMember(dir); err != nil {
		return "", err
	}
	initial, err := add()
	if err != nil {
		return "", err
	}
	if err := refuseRetainedMember(dir); err != nil {
		if rollbackErr := remove(); rollbackErr != nil {
			return "", fmt.Errorf("HA join: local etcd data appeared during member-add: %w; removing the newly added member also failed: %v", err, rollbackErr)
		}
		return "", fmt.Errorf("HA join: local etcd data appeared during member-add; newly added member removed: %w", err)
	}
	return initial, nil
}

// An add error does not prove the add never committed: the member list decides, never a guess.
func reconcileAmbiguousMemberAdd(fx joinEffects, etcdIPs []string, hostname, peer string, addErr error) (string, error) {
	members, err := fx.clusterMembers(etcdIPs)
	if err != nil {
		return "", fmt.Errorf("HA join: the member-add to %s failed (%v), and the cluster's member list "+
			"cannot be read to settle whether it committed (%w); the member may or may not be in the cluster, "+
			"so inspect `stratum etcdctl member list` before doing anything else — the intent file %s keeps the "+
			"recovery record, and a re-run of the join reconciles it", peer, addErr, err, joinIntentPath)
	}
	if m := members.memberByPeerURL(peer); m != nil {
		fmt.Printf("stratum HA join: the member-add's reply was lost (%v), but the cluster's member list shows "+
			"the member at %s (ID %x): adopting it instead of adding a second one\n", addErr, peer, m.ID)
		return initialClusterFor(members.Members, peer, hostname), nil
	}
	if !members.Quorum {
		return "", fmt.Errorf("HA join: the member-add failed (%w), and a member list read without quorum cannot "+
			"prove it never committed; refusing another add and keeping %s for recovery", addErr, joinIntentPath)
	}
	fmt.Printf("stratum HA join: the member-add's reply was lost (%v) and the cluster's member list does not "+
		"show the member at %s: it never committed, so the add is retried once\n", addErr, peer)
	added, retryErr := fx.memberAdd(etcdIPs, hostname, peer)
	if retryErr != nil {
		err := fmt.Errorf("HA join: the member-add's reply was lost (%v), the member is not in the "+
			"cluster's member list, and the one retry failed: %w", addErr, retryErr)
		if isNotEnoughStarted(retryErr) || isTooManyLearners(retryErr) {
			err = fmt.Errorf("%w (%s)", err, concurrentJoinHint)
		}
		return "", err
	}
	return added, nil
}

// The join host is often the VIP, where etcd does not listen; the published endpoints win.
func joinEtcdEndpoints(published []string, joinHost, vip string) ([]string, error) {
	if len(published) > 0 {
		return published, nil
	}
	if sameIP(joinHost, vip) {
		return nil, fmt.Errorf("HA join: the cluster published no etcd endpoints and %s is the VIP, where etcd does not listen. "+
			"Re-run `stratum token create --control-plane` on a control plane running this stratum or newer, "+
			"or join via a control plane's node IP", joinHost)
	}
	return []string{joinHost}, nil
}

// The join's host and cluster effects, as one struct so guarded test sets record calls in parallel.
type joinEffects struct {
	stage                    func(root string) ([]string, error)
	hostDirs                 func() error
	prereqs                  func(host.PrereqOptions) error
	discover                 func(server, token, caHash string) ([]byte, error)
	writeBootstrapKubeconfig func(server, token string, ca []byte) error
	clearCiliumSettings      func(path string) error
	// The join's one download effect, so the recorders that override it pin the production path.
	downloadGrant      func(bootstrapKubeconfig, certKey string, check func(bundleProfile) error) (bundleGrant, error)
	verifyResumeTrust  func(caHash string) error
	persistPreparation func() error
	trustPodCIDRs      func(podCIDR string)
	resolveNodeIP      func(flag, config, state string, detect func() (string, error)) (host.NodeIPChoice, error)
	detectNodeIPs      func(dualStack bool) (string, error)
	issueCerts         func(host, nodeIP string, sans *pki.APISANs) error
	writeKubeconfigs   func(host, server string, vip bool) error
	// Runs before the member-add: it can refuse, and nothing after the add removes the member.
	writeProfileConfigs func(profile.Profile) error
	probeEtcd           func(cpIPs []string) ([]string, error)
	controllerNodeIPs   func(context.Context, string, []string) ([]string, error)
	// Refuses before the member-add: the kubeconfigs were written against this VIP.
	vipServes    func(vip string) error
	memberAdd    func(cpIPs []string, name, peerURL string) (string, error)
	memberRemove func(cpIPs []string, peerURL string) error
	// The cluster's list, not the intent file, is the truth about this join's member.
	clusterMembers   func(cpIPs []string) (etcdMembership, error)
	memberRemoveByID func(cpIPs []string, id uint64) error
	// Any error refuses the post-add member removal, fail closed.
	confirmQuorumHeld func() error
	// Unset in production; prepareJoinNodeWithRecovery fills it with the under-lock variant.
	saveState           func(st *nodestate.NodeState) error
	writeRuntimeConfigs func(st *nodestate.NodeState) error
	// A field, not a direct call, so the join guards can observe the shared assembly.
	assemble func(context.Context, NodeAssembly, AssembleRequest) (host.NodeIPChoice, error)
	// Nil takes no fence; the shared helpers stay fence-free or orchestrators would self-refuse.
	takeMaintenanceFence func() (*k8sutil.MaintenanceLease, error)
}

func productionJoinEffects() joinEffects {
	return joinEffects{
		stage:                    assets.Stage,
		hostDirs:                 host.Dirs,
		prereqs:                  host.Prereqs,
		discover:                 bootstrap.DiscoverCA,
		writeBootstrapKubeconfig: bootstrap.WriteBootstrapKubeconfig,
		clearCiliumSettings:      os.Remove,
		downloadGrant:            DownloadCerts,
		verifyResumeTrust:        verifyJoinResumeTrust,
		persistPreparation:       persistJoinPreparation,
		trustPodCIDRs:            host.TrustPodCIDRs,
		resolveNodeIP:            host.ResolveNodeIP,
		detectNodeIPs:            host.DetectNodeIPs,
		issueCerts:               pki.GenerateAll,
		writeKubeconfigs:         kubeconfig.GenerateAll,
		writeProfileConfigs:      profilefiles.Write,
		probeEtcd:                MemberClientIPsVia,
		controllerNodeIPs:        controllerNodeIPsForJoin,
		vipServes:                VIPServes,
		memberAdd:                MemberAdd,
		memberRemove:             MemberRemove,
		clusterMembers:           joinMembership,
		memberRemoveByID:         joinMemberRemoveByID,
		writeRuntimeConfigs:      runtime.PrepareNodeConfigs,
		assemble:                 AssembleNodeContext,
		takeMaintenanceFence:     takeJoinMaintenanceFence,
	}
}

// A failed save with node.yaml published must not compensate membership its record may survive.
func saveJoinStateUnderLock(release func(), st *nodestate.NodeState) error {
	err := nodestate.MutateHeld(release, func(cur *nodestate.NodeState) error { *cur = *st; return nil })
	if err != nil {
		// node.yaml may already be published; do not compensate while its record can survive.
		if _, statErr := os.Stat(nodestate.StatePath()); !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("%w: save state: %w", errJoinStatePublished, err)
		}
		return err
	}
	for _, path := range []string{nodestate.StatePath(), filepath.Dir(nodestate.StatePath())} {
		if err := syncJoinFile(path); err != nil {
			return fmt.Errorf("%w: sync %s: %w", errJoinStatePublished, path, err)
		}
	}
	return nil
}

// Lives here because internal/cluster already imports internal/ha; the reverse would be a cycle.

// What a caller hands the shared assembly; State may arrive partially built, hooks fill the rest.
type AssembleRequest struct {
	Hostname string
	// NodeIPFlag is --node-ip ("" when not given); SavedIP is "" on a fresh bring-up or join.
	NodeIPFlag, ConfigNodeIP, SavedIP string
	VIPAddress                        string
	// The endpoint every kubeconfig on this node targets, not the join URL.
	APIServer     string
	PrereqOptions host.PrereqOptions
	State         *nodestate.NodeState
}

// The shared assembly's effects, as one struct so guarded test sets record calls in parallel.
type NodeAssembly struct {
	HostDirs      func() error
	Prereqs       func(host.PrereqOptions) error
	ResolveNet    func() (netcfg.Config, error)
	TrustPodCIDRs func(podCIDR string)
	ResolveNodeIP func(flag, config, state string, detect func() (string, error)) (host.NodeIPChoice, error)
	DetectNodeIPs func(dualStack bool) (string, error)
	// Runs between the resolved node IP and the certificates, so a caller can refuse a move.
	BeforeCerts         func(choice host.NodeIPChoice) error
	IssueCerts          func(host, nodeIP string, sans *pki.APISANs) error
	WriteKubeconfigs    func(host, server string, vip bool) error
	WriteProfileConfigs func() error
	// Nil runs nothing; the join puts its last refusals here, before anything that follows.
	BeforeRuntimeConfigs func() error
	WriteRuntimeConfigs  func(st *nodestate.NodeState) error
}

// Runs the common node steps of bootstrap and join; every fallible step returns before the next.
func AssembleNode(a NodeAssembly, req AssembleRequest) (host.NodeIPChoice, error) {
	return AssembleNodeContext(context.Background(), a, req)
}

// Stops between steps when ctx ends; a step already in flight finishes and keeps its own error.
func AssembleNodeContext(ctx context.Context, a NodeAssembly, req AssembleRequest) (host.NodeIPChoice, error) {
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	k8sutil.Step(os.Stdout, "Host prerequisites")
	if err := a.HostDirs(); err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := a.Prereqs(req.PrereqOptions); err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	netc, err := a.ResolveNet()
	if err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	a.TrustPodCIDRs(netc.PodCIDR)
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	choice, err := a.ResolveNodeIP(req.NodeIPFlag, req.ConfigNodeIP, req.SavedIP,
		func() (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return a.DetectNodeIPs(netc.DualStack)
		})
	if err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := a.BeforeCerts(choice); err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	in := certinputs.New(req.Hostname, choice.IP, netc, req.VIPAddress)
	if err := a.IssueCerts(in.Host, in.NodeIP, in.SANs); err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := a.WriteKubeconfigs(req.Hostname, req.APIServer, req.VIPAddress != ""); err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	if !req.PrereqOptions.Profile.None() {
		if err := a.WriteProfileConfigs(); err != nil {
			return host.NodeIPChoice{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	if a.BeforeRuntimeConfigs != nil {
		if err := a.BeforeRuntimeConfigs(); err != nil {
			return host.NodeIPChoice{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := a.WriteRuntimeConfigs(req.State); err != nil {
		return host.NodeIPChoice{}, err
	}
	if err := ctx.Err(); err != nil {
		return host.NodeIPChoice{}, err
	}
	return choice, nil
}

func vipNodeAddress(vip string, controllerIPs []string, nodeIP string) string {
	for _, ip := range controllerIPs {
		if sameIP(vip, ip) {
			return "an existing controller's node address"
		}
	}
	for _, ip := range strings.Split(nodeIP, ",") {
		if sameIP(vip, ip) {
			return "this node's own address"
		}
	}
	return ""
}

// Compares by value, so an IPv6 VIP written in another spelling still matches.
func sameIP(a, b string) bool {
	pa, pb := net.ParseIP(a), net.ParseIP(b)
	return pa != nil && pb != nil && pa.Equal(pb)
}

func hostFromURL(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	return u.Hostname(), nil
}

// Every step before the live member-add is reversible, so a stop here leaves the cluster untouched.
func joinHalted(ctx context.Context, what string) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	return fmt.Errorf("HA join: %w before %s (nothing was added to etcd)", err, what)
}

// Readies this node as an additional control plane; the caller then starts the supervisor.
func PrepareControlPlaneJoin(ctx context.Context, o JoinOptions) (*nodestate.NodeState, error) {
	return prepareControlPlaneJoin(ctx, o, version.EtcdDataDir, productionJoinEffects())
}

// Runs the join's two phases in order: preparation, then the membership commit.
func prepareControlPlaneJoin(ctx context.Context, o JoinOptions, etcdDataDir string, fx joinEffects) (*nodestate.NodeState, error) {
	if err := joinHalted(ctx, "the join began"); err != nil {
		return nil, err
	}
	prepared, err := prepareJoinNode(ctx, o, etcdDataDir, fx)
	if errors.Is(err, errJoinIntentStale) {
		prepared, err = prepareJoinNodeWithRecovery(ctx, o, etcdDataDir, fx, false)
	}
	if err != nil {
		return nil, err
	}
	return commitJoinMembership(ctx, prepared)
}

// Only prepareJoinNode builds a populated one; the zero value is inert and the commit refuses it.
type preparedJoin struct {
	fx          joinEffects
	etcdIPs     []string
	etcdDataDir string
	peer        string
	adopted     bool
	// A new API lease could deadlock the very member whose first start restores quorum.
	offlineAdoption bool
	confirmGrant    func(context.Context) error
	journal         *joinIntent
	// The commit fills its EtcdInitialCluster from the member-add's return before saving.
	st *nodestate.NodeState
	// Ends the data-dir critical section preparation opened.
	release func()
}

// Every fallible effect that needs no member-add result, in the safety order the guards pin.
func prepareJoinNode(ctx context.Context, o JoinOptions, etcdDataDir string, fx joinEffects) (p preparedJoin, err error) {
	return prepareJoinNodeWithRecovery(ctx, o, etcdDataDir, fx, true)
}

func prepareJoinNodeWithRecovery(ctx context.Context, o JoinOptions, etcdDataDir string, fx joinEffects, useRecovery bool) (p preparedJoin, err error) {
	if o.Server == "" || o.Token == "" || o.CAHash == "" || o.CertKey == "" {
		return preparedJoin{}, fmt.Errorf("HA join needs --join, --token, --discovery-token-ca-cert-hash and --certificate-key")
	}
	// The joining control plane's profile, before anything on the host changes.
	prof, err := profile.ForNode(o.Profile)
	if err != nil {
		return preparedJoin{}, fmt.Errorf("HA join: %w", err)
	}
	release, err := etcddata.Lock(etcdDataDir)
	if err != nil {
		return preparedJoin{}, err
	}
	// On success the commit phase releases the lock; the prepared value carries it.
	defer func() {
		if err != nil {
			release()
		}
	}()
	// An under-lock save: a reacquire would refuse against the join's own descriptor.
	if fx.saveState == nil {
		fx.saveState = func(st *nodestate.NodeState) error { return saveJoinStateUnderLock(release, st) }
	}
	// Refuse before staging can alter trust material this member's recovery needs.
	if err := refuseRetainedMember(etcdDataDir); err != nil {
		return preparedJoin{}, err
	}
	preIntent, err := readJoinIntent()
	if err != nil {
		return preparedJoin{}, err
	}
	hostname, err := host.Hostname()
	if err != nil {
		return preparedJoin{}, err
	}
	cpIP, err := hostFromURL(o.Server)
	if err != nil {
		return preparedJoin{}, err
	}
	fmt.Printf("stratum HA join: host=%s via=%s\n", hostname, o.Server)

	if _, err := fx.stage(version.DataDir); err != nil {
		return preparedJoin{}, fmt.Errorf("staging payload: %w", err)
	}

	// The endpoint is bring-up's rule over the VIP, so the first restart writes the same one.
	st := &nodestate.NodeState{
		Role: "controller", Hostname: hostname,
		Server:             kubeconfig.NodeServerURL("", o.VIP.Address),
		EtcdName:           hostname,
		EtcdState:          "existing",
		EtcdPromotePending: true,
		Profile:            o.Profile,
		CIS:                o.CIS,
		VIP:                o.VIP,

		NodeLabels:      o.NodeLabels,
		NodeTaints:      o.NodeTaints,
		RuntimeHandlers: o.RuntimeHandlers,
	}

	var (
		etcdIPs        []string
		primary        string
		peer           string
		adopted        bool
		adoptedInitial string
		memberID       uint64
		carried        []string
	)

	// The download runs before the assembly: its authenticated metadata feeds the request.
	var grant *bundleGrant
	resumeRecovery := useRecovery && preIntent != nil && preIntent.Recovery != nil
	if resumeRecovery {
		// The original command may have omitted the address the grant supplied.
		if st.VIP.Address == "" {
			st.VIP.Address = preIntent.Recovery.State.VIP.Address
		}
		st.Server = kubeconfig.NodeServerURL("", st.VIP.Address)
	}
	preRan := false
	if !resumeRecovery {
		if err := fx.hostDirs(); err != nil {
			return preparedJoin{}, err
		}
		if err := fx.prereqs(host.PrereqOptions{Profile: prof, SELinuxOptOut: o.SELinuxOptOut}); err != nil {
			return preparedJoin{}, err
		}
		k8sutil.Step(os.Stdout, "Discover cluster + download shared certs")
		ca, err := fx.discover(o.Server, o.Token, o.CAHash)
		if err != nil {
			return preparedJoin{}, err
		}
		if err := fx.writeBootstrapKubeconfig(o.Server, o.Token, ca); err != nil {
			return preparedJoin{}, err
		}
		if err := fx.clearCiliumSettings(version.CiliumSettingsPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return preparedJoin{}, fmt.Errorf("clear a previous join's Cilium settings: %w", err)
		}
		g, err := fx.downloadGrant(version.BootstrapKubeconfig, o.CertKey, func(b bundleProfile) error {
			if requireSharedSecrets(prof, b) == nil {
				return nil
			}
			return settleJoinSecrets(prof, b, false, version.PKIDir, os.Stdout)
		})
		if errors.Is(err, errPartBundle) {
			return preparedJoin{}, fmt.Errorf("%w: re-run the join once the cause is fixed, and it writes the whole bundle again", err)
		}
		if err != nil {
			return preparedJoin{}, err
		}
		grant = &g
		carried = g.Secrets
		// The VIP is published, authenticated metadata, not an unchecked config value.
		vip := o.VIP.Address
		if grant.VIP != "" {
			if o.VIP.Address != "" && !sameIP(o.VIP.Address, grant.VIP) {
				return preparedJoin{}, fmt.Errorf("HA join: this control plane's config sets the cluster VIP %s, but the join "+
					"bundle's authenticated metadata records %s: the cluster's VIP is published with the shared certs, and kube-vip "+
					"holds it on one control plane at a time. Make this node's spec.vip match the cluster's and re-run the join "+
					"(no certificates were issued and etcd is untouched)", o.VIP.Address, grant.VIP)
			}
			vip = grant.VIP
		}
		st.VIP.Address = vip
		st.Server = kubeconfig.NodeServerURL("", vip)
		ips, err := joinEtcdEndpoints(grant.Published, cpIP, vip)
		if err != nil {
			return preparedJoin{}, err
		}
		etcdIPs = ips
		// The quorum gate needs the endpoints, which exist from here on.
		if fx.confirmQuorumHeld == nil {
			fx.confirmQuorumHeld = func() error { return joinQuorumHeld(etcdIPs) }
		}
		preRan = true
	}

	a := NodeAssembly{
		HostDirs: fx.hostDirs,
		Prereqs:  fx.prereqs,
		ResolveNet: func() (netcfg.Config, error) {
			if useRecovery && preIntent != nil && preIntent.Recovery != nil {
				r := preIntent.Recovery
				if r.CAHash != o.CAHash || r.State.Hostname != hostname || r.State.ProfileName() != o.Profile || r.State.CIS != o.CIS || r.State.VIP != st.VIP {
					return netcfg.Config{}, fmt.Errorf("HA join: the options differ from the preparation in %s; re-run with the recorded CA pin, hostname, profile and VIP", joinIntentPath)
				}
				if err := fx.verifyResumeTrust(r.CAHash); err != nil {
					return netcfg.Config{}, fmt.Errorf("HA join: cannot resume using retained trust: %w; keep %s for recovery", err, joinIntentPath)
				}
				if o.ClusterDomain != "" && o.ClusterDomain != r.State.Domain() {
					return netcfg.Config{}, fmt.Errorf("HA join: configured cluster domain %q differs from the recorded domain %q", o.ClusterDomain, r.State.Domain())
				}
				for _, f := range []struct{ field, name, configured, recorded string }{
					{"spec.network.podCIDR", "pod CIDR", o.PodCIDR, r.State.PodCIDR},
					{"spec.network.serviceCIDR", "service CIDR", o.ServiceCIDR, r.State.ServiceCIDR},
				} {
					if f.configured != "" && !netcfg.EqualCIDRs(f.configured, f.recorded) {
						return netcfg.Config{}, fmt.Errorf("HA join: %s %q does not match this cluster's %s %q recorded in %s: "+
							"the cluster's CIDRs cannot be changed after init; remove the field or set it to %q "+
							"and re-run to resume the join",
							f.field, f.configured, f.name, f.recorded, joinIntentPath, f.recorded)
					}
				}
				if err := checkJoinCilium(o.Cilium); err != nil {
					return netcfg.Config{}, err
				}
				if err := requireSharedSecrets(prof, bundleProfile{Secrets: r.carried()}); err != nil {
					return netcfg.Config{}, err
				}
				for _, name := range r.carried() {
					if _, err := os.ReadFile(version.ProfileSecretFile(name)); err != nil {
						return netcfg.Config{}, fmt.Errorf("HA join: retained shared secret %s cannot be read: %w", name, err)
					}
				}
				netc, err := netcfg.Parse(r.State.PodCIDR, r.State.ServiceCIDR)
				if err != nil {
					return netcfg.Config{}, err
				}
				netc.ClusterDomain = r.State.Domain()
				if err := netcfg.ValidateDomain(netc.ClusterDomain); err != nil {
					return netcfg.Config{}, err
				}
				*st = r.State
				// A journal an older build wrote has no node facts; the operator may have corrected them.
				st.NodeLabels, st.NodeTaints, st.RuntimeHandlers = o.NodeLabels, o.NodeTaints, o.RuntimeHandlers
				// An older journal records the --join URL; the current endpoint rule wins.
				st.Server = kubeconfig.NodeServerURL("", st.VIP.Address)
				etcdIPs, carried = r.EtcdIPs, r.carried()
				if len(etcdIPs) == 0 {
					return netcfg.Config{}, fmt.Errorf("HA join: no retained etcd endpoints in %s", joinIntentPath)
				}
				if fx.confirmQuorumHeld == nil {
					fx.confirmQuorumHeld = func() error { return joinQuorumHeld(etcdIPs) }
				}
				return netc, nil
			}
			// The gates check against what the download delivered, so they run after it.
			if err := checkJoinDomain(o.ClusterDomain); err != nil {
				return netcfg.Config{}, err
			}
			if err := checkJoinNetwork(o.PodCIDR, o.ServiceCIDR); err != nil {
				return netcfg.Config{}, err
			}
			if err := checkJoinCilium(o.Cilium); err != nil {
				return netcfg.Config{}, err
			}
			if err := settleJoinSecrets(prof, grant.bundleProfile, true, version.PKIDir, os.Stdout); err != nil {
				return netcfg.Config{}, err
			}

			// The cluster's CIDRs rode in with the shared certs; inherit the cluster's CIDR shape.
			netc, err := netcfg.Load()
			if err != nil {
				return netcfg.Config{}, err
			}
			st.DualStack, st.PodCIDR, st.ServiceCIDR, st.ClusterDomain =
				netc.DualStack, netc.PodCIDR, netc.ServiceCIDR, netc.ClusterDomain
			return netc, nil
		},
		TrustPodCIDRs: fx.trustPodCIDRs,
		ResolveNodeIP: fx.resolveNodeIP,
		DetectNodeIPs: fx.detectNodeIPs,
		BeforeCerts: func(choice host.NodeIPChoice) error {
			fmt.Printf("stratum HA join: node IP %s (%s)\n", choice.IP, choice.Source)
			primary = host.Primary(choice.IP)
			st.NodeIP, st.KubeletNodeIP = primary, choice.IP
			return nil
		},
		IssueCerts: func(host, nodeIP string, sans *pki.APISANs) error {
			if err := joinHalted(ctx, "generating node-local certificates"); err != nil {
				return err
			}
			k8sutil.Step(os.Stdout, "Generate node-local certs (shared CA) + kubeconfigs")
			return fx.issueCerts(host, nodeIP, sans) // loads shared ca/sa/front-proxy, mints local leaves
		},
		WriteKubeconfigs: fx.writeKubeconfigs,
		// Runs before the member-add: it can refuse, and nothing after the add removes the member.
		WriteProfileConfigs: func() error {
			if err := fx.writeProfileConfigs(prof); err != nil {
				return fmt.Errorf("profile configs: %w (nothing was added to etcd)", err)
			}
			return nil
		},
		// The join's last refusals and the pre-add intent write run here, before the member-add.
		BeforeRuntimeConfigs: func() error {
			// If anything appears after this check, os.Remove fails ENOTEMPTY, not a recursive delete.
			if err := refuseRetainedMember(etcdDataDir); err != nil {
				return err
			}
			var controllerIPs []string
			var err error
			if preIntent == nil {
				if controllerIPs, err = fx.probeEtcd(etcdIPs); err != nil {
					return fmt.Errorf("HA join: cannot reach etcd at %s: %w (nothing was added, and this node's etcd "+
						"directory is untouched; %s)", strings.Join(etcdIPs, ","), err, concurrentJoinHint)
				}
			}
			newPeer := nodestate.EtcdPeerURL(primary)
			peer = newPeer
			adopted = false
			memberID = 0
			adoptedInitial = ""
			if preIntent != nil {
				members, err := fx.clusterMembers(etcdIPs)
				if err != nil {
					return fmt.Errorf("HA join: the intent file %s records an unfinished join (member %q at %s, "+
						"phase %s), and the cluster's member list cannot be read to reconcile it (%w); re-run the join once "+
						"etcd is reachable, or remove the recorded member by hand with `stratum etcdctl member remove <ID>` "+
						"on a healthy control plane and delete %s", joinIntentPath, preIntent.MemberName, preIntent.PeerURL,
						preIntent.Phase, err, joinIntentPath)
				}
				if m := members.memberByPeerURL(preIntent.PeerURL); m != nil {
					// Adoption is only sound for the member this run would itself have added.
					if preIntent.MemberName != hostname || preIntent.PeerURL != newPeer {
						removal := fmt.Sprintf("find this node's member in `stratum etcdctl member list` by its peer URL %s "+
							"and remove it by ID on a healthy control plane", preIntent.PeerURL)
						if preIntent.MemberID != 0 {
							removal = fmt.Sprintf("`stratum etcdctl member remove %x` on a healthy control plane", preIntent.MemberID)
						}
						return fmt.Errorf("HA join: the intent file %s records an unfinished join as the member %q at %s "+
							"(ID %d, phase %s), but this run would join as %q at %s: adopting the recorded member would finish "+
							"the join into a node state etcd cannot start with, since its initial cluster names %s while this "+
							"node advertises %s. Re-run the join with the recorded member's address, or remove the recorded "+
							"member by hand — %s — and delete %s before re-running the join",
							joinIntentPath, preIntent.MemberName, preIntent.PeerURL, preIntent.MemberID, preIntent.Phase,
							hostname, newPeer, preIntent.PeerURL, newPeer, removal, joinIntentPath)
					}
					memberID, peer, adopted = m.ID, preIntent.PeerURL, true
					if preIntent.MemberID != 0 && preIntent.MemberID != m.ID {
						return fmt.Errorf("HA join: member at %s has ID %x, but %s records %x; refusing to adopt a replacement member", peer, m.ID, joinIntentPath, preIntent.MemberID)
					}
					adoptedInitial = initialClusterFor(members.Members, peer, hostname)
					if err := writeJoinIntent(joinIntent{MemberName: preIntent.MemberName, PeerURL: peer,
						MemberID: m.ID, Phase: intentPhaseAdded, Recovery: preIntent.Recovery}); err != nil {
						fmt.Printf("stratum HA join: WARN: %v\n", err)
					}
					fmt.Fprintf(joinOut, "stratum HA join: the intent file %s records the member at %s (ID %x) still in the "+
						"cluster; adopting it instead of adding a second member\n", joinIntentPath, peer, m.ID)
				} else {
					if !members.Quorum {
						return fmt.Errorf("HA join: a member list read without quorum cannot prove the recorded member is gone; refusing a fresh add and retaining %s (%s)", joinIntentPath, concurrentJoinHint)
					}
					if useRecovery && preIntent.Recovery != nil {
						// Keep the old journal until the refresh succeeds, so a failure still recovers.
						return errJoinIntentStale
					}
					// Say so before the record is replaced: the journal is the failed join's diagnostics.
					fmt.Fprintf(joinOut, "stratum HA join: the intent file %s records member %q at %s (ID %d, phase %s), but "+
						"the cluster's member list does not show it; treating the intent as stale, replacing it, and joining "+
						"as a fresh member\n", joinIntentPath, preIntent.MemberName, preIntent.PeerURL, preIntent.MemberID,
						preIntent.Phase)
					// A failed probe must not strand a published node.yaml on the member-restart path.
				}
			}
			if !adopted {
				if preIntent != nil {
					if controllerIPs, err = fx.probeEtcd(etcdIPs); err != nil {
						return fmt.Errorf("HA join: cannot reach etcd before a fresh member-add: %w (%s)", err, concurrentJoinHint)
					}
				}
				// Runs before the intent write and data-dir replacement, so a refusal leaves membership.
				if vip := st.VIP.Address; vip != "" {
					kind := vipNodeAddress(vip, controllerIPs, st.KubeletNodeIP)
					if kind == "" {
						// The bundle may predate a controller; Node records add TLS-passing secondary addresses.
						ips, err := fx.controllerNodeIPs(ctx, o.Server, controllerIPs)
						if err != nil {
							return fmt.Errorf("HA join: cannot check whether VIP %s is a controller's node address: %w (nothing was added to etcd)", vip, err)
						}
						kind = vipNodeAddress(vip, ips, st.KubeletNodeIP)
					}
					if kind != "" {
						return fmt.Errorf("HA join: spec.vip.address %s is %s, not a VIP, so nothing was added to etcd. "+
							"Set spec.vip.address in this node's config to the cluster's VIP, the address its other "+
							"controllers' kubeconfigs target, and re-run the join", vip, kind)
					}
					if err := fx.vipServes(vip); err != nil {
						if errors.Is(err, errNoClusterCA) {
							return fmt.Errorf("HA join: cannot check that the VIP %s serves this cluster, so nothing was added to etcd: %w",
								vip, err)
						}
						return fmt.Errorf("HA join: the VIP %s does not answer as this cluster's apiserver on port %d: %w\n"+
							"Nothing was added to etcd. If spec.vip.address in this node's config is wrong, set it to the "+
							"cluster's VIP, the address its other controllers' kubeconfigs target. If it is right, bring "+
							"the VIP up on the existing controllers first:\n%s\nThen re-run the join.",
							vip, version.APIServerPort, err, vipBringUpSteps(vip))
					}
				}
				if err := writeJoinIntent(joinIntent{MemberName: hostname, PeerURL: newPeer, Phase: intentPhasePrepared}); err != nil {
					return fmt.Errorf("HA join: record the join intent in %s: %w (nothing was added to etcd)",
						joinIntentPath, err)
				}
				fmt.Fprintf(joinOut, "stratum HA join: recorded the join intent in %s (member %q at %s, phase %s) before "+
					"the member-add\n", joinIntentPath, hostname, newPeer, intentPhasePrepared)
				if err := prepareEmptyEtcdDataDir(etcdDataDir); err != nil {
					return err
				}
			}
			st.EtcdInitialCluster = adoptedInitial
			return nil
		},
		WriteRuntimeConfigs: fx.writeRuntimeConfigs,
	}
	req := AssembleRequest{
		Hostname:      hostname,
		NodeIPFlag:    o.NodeIP,
		ConfigNodeIP:  o.ConfigNodeIP,
		VIPAddress:    st.VIP.Address,
		APIServer:     st.Server,
		PrereqOptions: host.PrereqOptions{Profile: prof, SELinuxOptOut: o.SELinuxOptOut},
		State:         st,
	}
	if useRecovery && preIntent != nil && preIntent.Recovery != nil {
		req.SavedIP = preIntent.Recovery.State.KubeletNodeIP
		if req.SavedIP == "" {
			req.SavedIP = preIntent.Recovery.State.NodeIP
		}
	}
	if preRan {
		// The pre-download group already ran; the assembly's host-dir and prereq hooks must not rerun it.
		a.HostDirs = func() error { return nil }
		a.Prereqs = func(host.PrereqOptions) error { return nil }
	}
	assemble := fx.assemble
	if assemble == nil {
		assemble = AssembleNodeContext
	}
	if _, err := assemble(ctx, a, req); err != nil {
		return preparedJoin{}, err
	}
	// An adopted member already has its ID, so its journal is at phase added.
	journal := &joinIntent{MemberName: hostname, PeerURL: peer, MemberID: memberID, Phase: intentPhasePrepared}
	if memberID != 0 {
		journal.Phase = intentPhaseAdded
	}
	if err := fx.persistPreparation(); err != nil {
		return preparedJoin{}, fmt.Errorf("HA join: persist preparation before membership commit: %w", err)
	}
	journal.Recovery = &joinRecovery{State: *st, EtcdIPs: etcdIPs, CAHash: o.CAHash,
		CarriedKey: slices.Contains(carried, encryptionConfigEntry), CarriedSecrets: carried}
	if err := writeJoinIntent(*journal); err != nil {
		return preparedJoin{}, err
	}
	var confirmGrant func(context.Context) error
	if grant != nil {
		confirmGrant = grant.confirmCurrent
	}
	return preparedJoin{
		fx: fx, etcdIPs: etcdIPs, etcdDataDir: etcdDataDir,
		peer: peer, adopted: adopted, journal: journal, st: st, release: release,
		offlineAdoption: adopted && resumeRecovery,
		confirmGrant:    confirmGrant,
	}, nil
}

// Runs exactly the membership effects: the member-add, then the node-state save.
func commitJoinMembership(ctx context.Context, p preparedJoin) (*nodestate.NodeState, error) {
	// No preparation ran: refuse rather than reconcile, add, or save on a guess.
	if p.fx.memberAdd == nil || p.st == nil {
		return nil, fmt.Errorf("HA join: the membership-commit phase ran on an unprepared value; " +
			"run the join's preparation phase (prepareJoinNode) first")
	}
	defer p.release()

	// Goes up after every refusing step; proven recovery of an admitted member skips it.
	var lease *k8sutil.MaintenanceLease
	if !p.offlineAdoption && p.fx.takeMaintenanceFence != nil {
		var ferr error
		lease, ferr = p.fx.takeMaintenanceFence()
		if k8sutil.JoinFenceDenied(ferr) {
			return nil, ferr
		}
		if apierrors.IsForbidden(ferr) {
			// A token minted before the lease grant has no Lease access; only a new mint repairs it.
			return nil, fmt.Errorf("HA join: this join's bootstrap token may not take the cluster's maintenance fence (%w): "+
				"the join command was minted by a stratum release that predates the member-add fence grant. Mint a new one "+
				"with `stratum token create --control-plane` on a control plane running this release — that mint grants the "+
				"fence — and re-run the join with it (nothing was added to etcd)", ferr)
		}
		if ferr != nil {
			return nil, ferr
		}
		defer lease.Release()
	}
	if err := lease.ConfirmWithin(ctx, joinFenceConfirmWait); err != nil {
		return nil, err
	}
	if p.confirmGrant != nil {
		if err := p.confirmGrant(ctx); err != nil {
			return nil, err
		}
	}

	k8sutil.Step(os.Stdout, "Join etcd (live member-add)")
	fmt.Printf("stratum HA join: etcd via %s\n", strings.Join(p.etcdIPs, ","))
	if !p.adopted {
		if err := joinHalted(ctx, "the live etcd member-add"); err != nil {
			return nil, err
		}
		// The add's return, or the adopted membership, is the initial cluster this node boots with.
		initial, err := addMemberIfEtcdDataDirEmpty(p.etcdDataDir,
			func() (string, error) {
				if err := lease.ConfirmWithin(context.Background(), joinFenceConfirmWait); err != nil {
					return "", err
				}
				added, addErr := p.fx.memberAdd(p.etcdIPs, p.journal.MemberName, p.peer)
				if addErr == nil {
					return added, nil
				}
				// A refusal etcd answered outright cannot have committed; do not reconcile it.
				if memberAddRefused(addErr) {
					if isNotEnoughStarted(addErr) || isTooManyLearners(addErr) {
						return "", fmt.Errorf("%w (%s)", addErr, concurrentJoinHint)
					}
					return "", addErr
				}
				// The reply may have been lost after the request committed; settle against the list.
				return reconcileAmbiguousMemberAdd(p.fx, p.etcdIPs, p.journal.MemberName, p.peer, addErr)
			},
			func() error { return p.fx.memberRemove(p.etcdIPs, p.peer) })
		if err != nil {
			return nil, err
		}
		p.st.EtcdInitialCluster = initial
		// A list that cannot be read leaves the journal at phase prepared; the compensation re-reads it.
		members, err := p.fx.clusterMembers(p.etcdIPs)
		if err != nil {
			fmt.Printf("stratum HA join: WARN: cannot read the cluster's member list to record the new member's "+
				"ID in %s (%v)\n", joinIntentPath, err)
		} else if m := members.memberByPeerURL(p.peer); m == nil {
			fmt.Printf("stratum HA join: WARN: the cluster's member list does not yet show the member at %s; "+
				"%s stays at phase %q\n", p.peer, joinIntentPath, intentPhasePrepared)
		} else {
			p.journal.MemberID = m.ID
			p.journal.Phase = intentPhaseAdded
			if err := writeJoinIntent(*p.journal); err != nil {
				fmt.Printf("stratum HA join: WARN: %v\n", err)
			}
		}
	}

	k8sutil.Step(os.Stdout, "Persist node state (etcd member: existing)")
	// The saved state asserts the add's membership; a failure leaves a member to compensate.
	if err := p.fx.saveState(p.st); err != nil {
		if errors.Is(err, errJoinStatePublished) {
			return nil, fmt.Errorf("HA join: %w; node state was published but its durability is unconfirmed; keeping %s and leaving membership unchanged. Re-run `stratum up --join` to reconcile and finish the save", err, joinIntentPath)
		}
		return nil, joinPostAddFailure(p.fx, p.etcdIPs, p.journal, err)
	}
	// The journal outlives the save: a crash before etcd's first WAL leaves a node.yaml `up` refuses.
	if err := ctx.Err(); err != nil {
		fmt.Printf("stratum HA join: a signal was received while the etcd member was being added; "+
			"the member-add and the node-state save were completed before stopping — "+
			"repeat the original `stratum up --join` command to resume from the retained join intent (%v)\n", err)
		return nil, err
	}
	return p.st, nil
}

// Removes the member by ID only when the member list names it and the quorum gate authorises.
func joinPostAddFailure(fx joinEffects, etcdIPs []string, j *joinIntent, cause error) error {
	recorded := fmt.Sprintf("the intent file %s keeps the member's identity and resumable phase (name %q, peer %s, ID %d, phase %s)",
		joinIntentPath, j.MemberName, j.PeerURL, j.MemberID, j.Phase)
	// member remove 0 names no member; until the list answers, the remedy is the peer-URL lookup.
	recoveryFor := func(id uint64) string {
		if id == 0 {
			return fmt.Sprintf("re-run `stratum up --join` on this node: it reconciles the intent against the cluster's "+
				"own member list before adding anything; or find this node's member in `stratum etcdctl member list` "+
				"by its peer URL %s and remove it by ID on a healthy control plane and delete %s", j.PeerURL, joinIntentPath)
		}
		return fmt.Sprintf("re-run `stratum up --join` on this node: it reconciles the intent against the cluster's "+
			"own member list before adding anything; or remove the member by hand with `stratum etcdctl member remove %x` "+
			"on a healthy control plane and delete %s", id, joinIntentPath)
	}
	recovery := recoveryFor(j.MemberID)
	id := j.MemberID
	if id == 0 {
		members, err := fx.clusterMembers(etcdIPs)
		if err != nil {
			return fmt.Errorf("HA join: %w; the member was added to etcd, but its ID cannot be learned from the "+
				"cluster's member list (%v), so removing it is refused, fail closed; %s. %s", cause, err, recorded, recovery)
		}
		m := members.memberByPeerURL(j.PeerURL)
		if m == nil {
			return fmt.Errorf("HA join: %w; the member was added to etcd, but the cluster's member list does not "+
				"show it at %s, so removing it is refused, fail closed; %s. %s", cause, j.PeerURL, recorded, recovery)
		}
		id = m.ID
		recovery = recoveryFor(id)
	}
	if err := fx.confirmQuorumHeld(); err != nil {
		return fmt.Errorf("HA join: %w; the member was added to etcd, but removing it is refused: %v; %s. %s",
			cause, err, recorded, recovery)
	}
	if err := fx.memberRemoveByID(etcdIPs, id); err != nil {
		return fmt.Errorf("HA join: %w; the member was added to etcd, but removing it by ID %d failed: %v; %s. %s",
			cause, id, err, recorded, recovery)
	}
	clearJoinIntent()
	return fmt.Errorf("HA join: %w; the just-added member was removed again by ID, so the cluster holds no member "+
		"for this node; re-run the join to retry", cause)
}

// The VIP can refuse connections while the previous control plane settles; discovery waits as long.
var joinFenceConfirmWait = 2 * time.Minute

// Read at take time: the fence goes up after preparation wrote it, this node's only credential.
func takeJoinMaintenanceFence() (*k8sutil.MaintenanceLease, error) {
	apiCfg, err := clientcmd.LoadFromFile(version.BootstrapKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("take the maintenance fence: read the bootstrap kubeconfig: %w", err)
	}
	tokenID := bootstrapTokenID(kubeconfigUserToken(apiCfg))
	if tokenID == "" {
		return nil, fmt.Errorf("take the maintenance fence: the bootstrap kubeconfig %s carries no bootstrap token "+
			"to name the fence's holder by", version.BootstrapKubeconfig)
	}
	cs, err := joinFenceClientset(version.BootstrapKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("take the maintenance fence: %w", err)
	}
	lease, err := k8sutil.MaintenanceFenceAs(cs, k8sutil.JoinMaintenanceHolder(tokenID))
	if k8sutil.JoinFenceDenied(err) {
		return nil, leftMaintenanceLeases(cs, tokenID, err)
	}
	if err != nil {
		return nil, err
	}
	return lease, nil
}

// The lease named another holder and had expired: a run that did not finish left it.
func leftMaintenanceLeases(cs kubernetes.Interface, tokenID string, denied error) error {
	own := "system:bootstrap:" + tokenID + "/"
	var held, names []string
	for _, name := range maintenanceLeaseNames {
		l, err := cs.CoordinationV1().Leases("kube-system").Get(context.Background(), name, metav1.GetOptions{})
		if err != nil || l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" || strings.HasPrefix(*l.Spec.HolderIdentity, own) {
			continue
		}
		desc := fmt.Sprintf("kube-system/%s (holder %s", name, *l.Spec.HolderIdentity)
		if l.Spec.RenewTime != nil {
			desc += fmt.Sprintf(", last renewed %s ago", time.Since(l.Spec.RenewTime.Time).Round(time.Second))
		}
		held = append(held, desc+")")
		names = append(names, name)
	}
	if len(names) == 0 {
		// Gone or taken again since the denial: name the lease the apiserver refused.
		name := "stratum-cluster-maintenance"
		var st apierrors.APIStatus
		if errors.As(denied, &st) && st.Status().Details != nil && st.Status().Details.Name != "" {
			name = st.Status().Details.Name
		}
		held, names = []string{"kube-system/" + name}, []string{name}
	}
	what, it := "the maintenance lease "+held[0]+" was", "it"
	if len(held) > 1 {
		what, it = "the maintenance leases "+strings.Join(held, " and ")+" were", "them"
	}
	return fmt.Errorf("HA join: %s left by a run that did not finish, and a join token may not take over a "+
		"maintenance lease another run holds, even an expired one (%w). Run `stratum token create --control-plane` "+
		"on any control plane — its maintenance fence takes an expired lease over and releases it — or delete %s "+
		"with `kubectl -n kube-system delete lease %s`, then re-run the join (nothing was added to etcd)",
		what, denied, it, strings.Join(names, " "))
}

// A var so tests can stand a fake apiserver in for the cluster's.
var joinFenceClientset = func(path string) (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

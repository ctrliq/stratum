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
	NodeIP  string // --node-ip ("" when not given)
	// ConfigNodeIP is spec.network.nodeIP. Kept apart from NodeIP so the join
	// logs which one won (host.ResolveNodeIP has the whole precedence).
	ConfigNodeIP string
	Operator     bool // run the stratum-operator host service on this CP too (leader-elected)
	CertRenew    bool // install the cert auto-renew timer on this CP too
	// Profile is the node profile this control plane runs, by name; "" for
	// none. CIS is its legacy spelling, true exactly for the cis profile, which
	// the join's key agreement and config writers still read.
	Profile string
	CIS     bool
	// SELinuxOptOut is this control plane's spec.selinux: false.
	SELinuxOptOut bool
	VIP           nodestate.VIP // the cluster VIP this CP joins (kube-vip election)
	// PodCIDR/ServiceCIDR are spec.network.podCIDR and serviceCIDR, "" when
	// unset. They cannot override the cluster's ranges, which ride in with the
	// shared certs: a different one refuses the join (checkJoinNetwork).
	PodCIDR     string
	ServiceCIDR string
	// ClusterDomain is spec.network.clusterDomain, "" when unset. It cannot
	// override the cluster's domain, which rides in with the shared certs: a
	// different one refuses the join (checkJoinDomain).
	ClusterDomain string
	// Cilium is this control plane's spec.network.cilium. It must be the
	// cluster's, which rides in with the shared certs: a different one refuses
	// the join (checkJoinCilium).
	Cilium addons.CiliumSettings
	// NodeLabels, NodeTaints and RuntimeHandlers are this control plane's
	// spec.node, saved with its state so the join run itself renders its
	// runtime handlers and registers its Node with them.
	NodeLabels      map[string]string
	NodeTaints      []string
	RuntimeHandlers map[string]string
}

// checkJoinCilium refuses a join whose spec.network.cilium differs from the
// cluster's. The operator applies whichever control plane leads its render, so
// two different blocks would swap the cluster's Cilium configuration on every
// leader change. The cluster's settings are the bundle entry the shared certs
// just wrote, absent when they are the shipped ones.
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

// checkJoinDomain refuses a join whose config sets a cluster domain other than
// the one the cluster uses. The cluster's domain is the netconfig the shared
// certs just delivered, so this is the first point it is known, and it runs
// before this node mints certificates or touches etcd. Unset or matching is
// fine. Without it the join would succeed on the cluster's domain and the
// node's first restart would refuse the same config (PrepareController).
func checkJoinDomain(configured string) error {
	if _, err := netcfg.ResolveDomain(configured); err != nil {
		return fmt.Errorf("HA join: %w (no certificates were issued and etcd is untouched)", err)
	}
	return nil
}

// checkJoinNetwork refuses a join whose config sets a pod or service CIDR
// other than the cluster's, each checked on its own. The cluster's ranges are
// the netconfig the shared certs just delivered, which is what the join runs
// on, and every control plane must run the same ones. Unset or matching is
// fine. Without it a lone range would be dropped for the cluster's, a pair
// would replace them on this node alone, and the node's first restart would
// be the first notice.
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

// refuseRetainedMember runs before any join-side writes. Even non-member
// residue must be inspected before shared certificates are overwritten.
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

// prepareEmptyEtcdDataDir replaces only an empty directory. os.Remove is
// atomic with respect to a concurrent create: if member/ (or any other entry)
// appears after the earlier inspection, removal fails and preserves it. Never
// recursively delete a directory whose contents may have changed under us.
func prepareEmptyEtcdDataDir(dir string) error {
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("HA join: %s is not empty or cannot be removed (%w); refusing to delete its data", dir, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("HA join: create empty etcd data directory %s: %w", dir, err)
	}
	return nil
}

// requireSharedSecrets refuses a control-plane join whose downloaded bundle
// does not match the profile this control plane runs: one uploaded by a
// control plane of another profile, or one whose shared secrets are not the
// ones the profile declares. The bundle is a snapshot taken when `stratum
// token create --control-plane` uploaded it, so one uploaded before the
// profile was turned on, or one taken while a controller was minting its
// secrets, lacks them: joining with it would give this apiserver none, or
// different ones. It asks what the bundle carried, not what is on disk: a
// secret left by an earlier failed join survives a bundle without it, and is
// not one the cluster handed out. It runs before the etcd member-add, so a
// refusal leaves the cluster untouched.
//
// The reverse is refused too: a bundle that carries a profile's secrets comes
// from a cluster that runs it, and a control plane joining it without the
// profile would start without them. For the cis profile, whose secret is the
// at-rest encryption config, that is an apiserver unable to read the
// cluster's encrypted Secrets, and writing new ones in plaintext.
//
// A bundle without the secrets has two causes, and the remedy names both: the
// cluster does not run the profile, so join it without; or it is meant to,
// and the profile has to be on there before a bundle can carry its secrets.
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

// profileNoun names a profile in a refusal, or its absence.
func profileNoun(name string) string {
	if name == "" {
		return "no node profile"
	}
	return fmt.Sprintf("the %s profile", name)
}

// profileFlag is how a join selects the profile a refusal names.
func profileFlag(name string) string {
	if name == "" {
		return "no --profile or --cis (and no spec.profile or spec.cis: true)"
	}
	return "--profile " + name
}

// settleJoinSecrets leaves this joining node with exactly the profile shared
// secrets its bundle carried, or refuses. applied says whether the bundle was
// written: a join refused before it wrote the bundle downloaded none of the
// secrets on disk, so each one there is an earlier attempt's.
//
// A bundle does not remove the optional entries it leaves out, so one without
// a secret accepted over a copy from an earlier attempt would keep that copy:
// the next `token create --control-plane` here uploads every shared secret it
// finds, and joiners would then take a stale one for the cluster's. And a join
// refused for a profile mismatch must not keep the secrets it just downloaded.
// Either way the files are removed, and each removal is said out loud: on
// stdout as the join's other steps are, and in the refusal when there is one.
// Each removal is synced to the directory before it is reported: a power loss
// could otherwise bring a secret back, and the next `token create` would
// upload it.
//
// Removing them unconditionally relies on the caller's preconditions, which
// make any secret file here this node's own leftover rather than a member's
// live secret: `stratum up --join` hands a node with saved state to the
// restart path instead of joining it (alreadyJoinedDecision), and
// prepareControlPlaneJoin refuses any etcd data under the etcd data lock
// before it downloads. A new caller must keep both preconditions.
//
// Only the join calls this. DownloadCertsAdmin (CA rotation on a joined
// control plane) applies a bundle without it, so a rotation bundle never
// removes a member's secrets.
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

// secretNoun names a shared secret in the join's messages: the cis profile's
// by what it holds.
func secretNoun(name string) string {
	if name == encryptionConfigEntry {
		return "key"
	}
	return "shared secret " + name
}

// syncJoinKeyDir is host.SyncDir behind a seam, so a test can fail it.
var syncJoinKeyDir = host.SyncDir

// syncJoinIntentDir is host.SyncDir behind a seam, so a test can fail it: the
// intent journal's directory must be synced after the journal's rename, or a
// power loss can lose the directory entry and the recovery record with it.
var syncJoinIntentDir = host.SyncDir

// joinOut is where the join reports the decisions an operator must see — which
// intent it recorded, adopted, or replaced. A writer, the way settleJoinSecrets
// takes one, so a test can assert what the join said.
var joinOut io.Writer = os.Stdout

// intentFileName is the intent journal's file name inside the state directory
// (version.K8sConfDir), where node.yaml lives too.
const intentFileName = "join-intent.json"

// joinIntentPath is the durable pre-add intent journal a control-plane join
// writes before its etcd member-add and clears once its node state is
// persisted: the member name, peer URL, and — once the cluster's member list
// has answered — the member ID and resumable phase. It lives beside node.yaml
// (under version.K8sConfDir) so the state directory's existing cleanup and
// uninstall already sweep it. A var, like the version paths, only so tests
// can redirect it to a temp file.
var joinIntentPath = filepath.Join(version.K8sConfDir, intentFileName)

// intentPhasePrepared is the journal's phase while only the member name and
// peer URL are known — before the member-add and until the cluster's member
// list has answered for this node's peer URL. intentPhaseAdded is the phase
// once that list has given the member ID.
const (
	intentPhasePrepared = "prepared"
	intentPhaseAdded    = "added"
)

// joinIntent is the intent file's on-disk JSON: the member identity a re-run
// needs to reconcile or remove this join's member, and the resumable phase it
// resumes from. Pinned as plain JSON because the file outlives the process
// that wrote it — a re-run of `stratum up --join` reads it, and so does an
// operator.
type joinIntent struct {
	MemberName string        `json:"memberName"`
	PeerURL    string        `json:"peerURL"`
	MemberID   uint64        `json:"memberID"`
	Phase      string        `json:"phase"`
	Recovery   *joinRecovery `json:"recovery,omitempty"`
}

// The prepared state and endpoints let a committed member start even when
// adding it cost the cluster quorum and the API can no longer serve Secrets.
// Credentials stay out of the journal; the local CA must match the saved pin.
type joinRecovery struct {
	State   nodestate.NodeState `json:"state"`
	EtcdIPs []string            `json:"etcdIPs"`
	CAHash  string              `json:"caHash"`
	// CarriedKey is what a journal from an earlier binary recorded: whether
	// the bundle carried the cis profile's at-rest key. It is still written,
	// beside CarriedSecrets, so that binary can resume this journal.
	CarriedKey     bool     `json:"carriedKey"`
	CarriedSecrets []string `json:"carriedSecrets,omitempty"`
}

// carried is the profile shared secrets the journaled bundle carried.
func (r joinRecovery) carried() []string {
	if r.CarriedSecrets == nil && r.CarriedKey {
		return []string{encryptionConfigEntry}
	}
	return r.CarriedSecrets
}

// readJoinIntent returns nil when no join intent is recorded. An intent that
// cannot be read, or that can be read but names no member, is an error, never
// "none": treating it as absent would run a second member-add over the very
// membership the file exists to reconcile. The rule, and the shape of the
// refusal, are the in-progress upgrade marker's
// (internal/upgrade/resume.go:50-78).
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

// writeJoinIntent records j durably, in the marker shape of
// writeUpgradeMarker (internal/upgrade/resume.go), made durable: JSON written
// through a hidden, uniquely named staging file at 0600 in the intent's own
// directory, fsynced, renamed into place, and the directory synced after the
// rename, so a crash can neither leave a torn intent nor lose the directory
// entry. The journal is this node's only record of the member between the
// member-add and the node-state save, and a lost journal reads as "no
// unfinished join" — settleJoinSecrets' reason for syncing a secret's removal.
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
	// A failed rename must not leave the staging file behind; os.CreateTemp
	// made it 0600, so the umask cannot widen it.
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("record the join intent in %s: %w", joinIntentPath, err)
	}
	// Sync the staged file before the rename: a rename whose data is still
	// dirty can land the directory entry while the content is not on disk.
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

// clearJoinIntent ends the journal: the recorded member is no longer in the
// cluster, or a failed join removed it. Best-effort, because a leftover
// intent is harmless — the next join reconciles it against the cluster's own
// member list and clears it — but never silent. The node-state save does NOT
// end the journal: it survives until a later start confirms the node's local
// etcd data (ClearJoinIntent, called by pkg/cli behind requireLocalEtcdMember).
func clearJoinIntent() {
	if err := os.Remove(joinIntentPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Printf("stratum HA join: WARN: clear the join intent file %s: %v — the next join will reconcile it "+
			"as an unfinished one\n", joinIntentPath, err)
	}
}

// ClearJoinIntent ends a join's intent journal once its work is done: the
// node's local etcd data is confirmed on a later start, so the state between
// the join's state save and etcd's first WAL no longer needs a recovery
// record. Derived from version.K8sConfDir at call time — the same path
// JoinIntentSurvives reports on. A missing file is fine; any other error is
// returned to the caller.
func ClearJoinIntent() error {
	path := JoinIntentPath()
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear the join intent file %s: %w", path, err)
	}
	return nil
}

// JoinIntentPath is the durable pre-add intent journal's path in this node's
// state directory, derived from version.K8sConfDir at call time — the path the
// caller-facing gate (JoinIntentSurvives) and the refusals that name the file
// report. The join's own reads, writes, and clear go through the package var
// joinIntentPath instead, which this package's tests redirect; the two name
// the same file because version.K8sConfDir is never reassigned outside tests.
func JoinIntentPath() string {
	return filepath.Join(version.K8sConfDir, intentFileName)
}

// JoinIntentSurvives reports whether a join intent file survives in this
// node's state directory, whatever its contents. A join that added its member
// but never persisted the node state leaves the file there as the recovery
// record, so a bare `stratum up` that treated it as absent would bootstrap a
// separate single-member cluster over the recorded membership. An unreadable
// file is an error, never "absent" — readJoinIntent's rule.
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

// joinMembership is the clusterMembers effect: the cluster's member list, read
// through the endpoints the join dials. The list, not the intent file, is the
// truth about this join's member (memberByPeerURL).
func joinMembership(cpIPs []string) (etcdMembership, error) {
	return joinMembershipVia(cpIPs, memberList)
}

// joinMemberRemoveByID is the memberRemoveByID effect: memberRemoveID through
// the join's endpoints, in turn, until one answers.
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

// joinQuorumHeld is the confirmQuorumHeld effect: the post-add member removal
// may run only when the cluster provably still has quorum. A linearizable
// member list needs a quorum of the cluster's voting members to answer, so its
// success is that proof. An endpoint that is down, a peer that does not answer,
// or a list that answers only serializably says nothing about the cluster's
// majority, and refuses the removal — fail closed, confirmQuorumLost's
// semantics (internal/ha/quorum.go:39-69): an unreadable membership must never
// authorise taking a member away, because removing a member the cluster may
// still need is worse than leaving one a re-run can reconcile.
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

// concurrentJoinHint follows the refusals a join started alongside another
// controller's meets. That controller's member-add takes the cluster from one
// voter to two before the new member starts, and until it does the cluster
// cannot answer a linearizable member listing (the probe, and the quorum read
// that reconciles a recorded intent) or accept another voter (etcd's strict
// reconfiguration check). Each refusal leaves membership as it was, so a
// re-run after the first join finishes succeeds.
const concurrentJoinHint = "if another control plane is joining now, wait until it is Ready and re-run this join; " +
	"join controllers one at a time"

// addMemberIfEtcdDataDirEmpty checks both sides of the remote member-add. The
// post-add check catches a local writer that populated the directory while the
// network request was in flight; in that case remove the just-added member so
// the failed join does not leave a voting member behind.
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

// reconcileAmbiguousMemberAdd settles a member-add whose answer was lost: an
// error from the add does not prove the add never committed, because the
// request may have been served and only the reply dropped. The commit phase
// hands over only those possibly-lost replies — a refusal etcd answered
// outright (memberAddRefused) is returned directly instead. The cluster's
// member list, not the error, decides — the rule the restore path's rejoin
// already applies to its own lost-reply ambiguity
// (internal/ha/restore.go:539-541). A member the list already shows at this
// node's peer URL is adopted and the join finishes on it without a second
// add; a member it does not show was never added, and the add is retried
// once; a list that cannot answer refuses the join, fail closed, never
// guessed, and the intent file stays as the re-run's recovery record.
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
		if isNotEnoughStarted(retryErr) {
			err = fmt.Errorf("%w (%s)", err, concurrentJoinHint)
		}
		return "", err
	}
	return added, nil
}

// joinEtcdEndpoints picks the etcd addresses a join dials for MemberAdd and for
// the MemberRemove that rolls it back.
//
// The --join host is a VIP in the documented HA shape, and etcd listens only on
// loopback and the node's own address (staticpod args), so dialling the join host
// was a MemberAdd against a port nothing binds. The cluster now
// publishes its members' client addresses beside the shared certs; those win.
// Without them — a control plane that predates published etcd addresses — the
// join host is the only candidate, which works exactly when it is a control
// plane's node IP and is refused up front when it is the VIP, rather than
// timing out inside MemberAdd.
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

// joinEffects are the host and cluster effects a control-plane join runs, as
// one struct parameter in the style of tokenSteps (internal/token/token.go):
// the join's safety order — every step that can refuse returns before the
// etcd member-add — is otherwise held only by statement order, which no test
// can observe without touching the host. A struct rather than package
// variables keeps the tests that use a recording set safe to run in parallel.
// PrepareControlPlaneJoin passes the production set; the join guards in
// join_test.go pass one that records each call.
type joinEffects struct {
	// stage unpacks the embedded payload into root (assets.Stage).
	stage func(root string) ([]string, error)
	// hostDirs creates the directory layout (host.Dirs).
	hostDirs func() error
	// prereqs checks the host prerequisites for the profile in use
	// (host.Prereqs).
	prereqs func(host.PrereqOptions) error
	// discover fetches the cluster CA from the server being joined
	// (bootstrap.DiscoverCA).
	discover func(server, token, caHash string) ([]byte, error)
	// writeBootstrapKubeconfig writes the kubeconfig the download
	// authenticates with (bootstrap.WriteBootstrapKubeconfig).
	writeBootstrapKubeconfig func(server, token string, ca []byte) error
	// clearCiliumSettings removes settings left by a previous join before the
	// new bundle supplies this cluster's settings (os.Remove).
	clearCiliumSettings func(path string) error
	// downloadGrant fetches and applies the shared certs as one grant,
	// reporting the facts the bundle's authenticated metadata carries: the etcd
	// endpoints a joiner dials, whether the bundle carried the at-rest key, the
	// cluster VIP the publishing control plane recorded, the publication's
	// generation, and the profile secrets and publishing profile the bundle
	// names, which check sees before any file is written (DownloadCerts). It is
	// the join's one download effect, so the recorders and recovery sentinels
	// that override it pin the path production runs.
	downloadGrant func(bootstrapKubeconfig, certKey string, check func(bundleProfile) error) (bundleGrant, error)
	// Resume uses retained shared trust rather than a quorum-backed API read.
	verifyResumeTrust func(caHash string) error
	// Make retained trust/configuration durable before committing membership.
	persistPreparation func() error
	// trustPodCIDRs opens the host firewall for the pod interfaces
	// (host.TrustPodCIDRs).
	trustPodCIDRs func(podCIDR string)
	// resolveNodeIP picks this node's address, falling back to detection
	// (host.ResolveNodeIP, host.DetectNodeIPs).
	resolveNodeIP func(flag, config, state string, detect func() (string, error)) (host.NodeIPChoice, error)
	detectNodeIPs func(dualStack bool) (string, error)
	// issueCerts mints this node's leaves from the shared CAs (pki.GenerateAll).
	issueCerts func(host, nodeIP string, sans *pki.APISANs) error
	// writeKubeconfigs writes the node's kubeconfigs (kubeconfig.GenerateAll).
	writeKubeconfigs func(host, server string, vip bool) error
	// writeProfileConfigs writes the config files the join's profile declares
	// and its apiserver flags reference (profilefiles.Write).
	// Runs before the member-add: it can refuse, and nothing after the add
	// removes the member again on failure.
	writeProfileConfigs func(profile.Profile) error
	// probeEtcd is the read-only member listing through the endpoints the join
	// will add itself through, returning the live controller primary addresses
	// (MemberClientIPsVia). The shared certificate bundle may predate a join.
	probeEtcd func(cpIPs []string) ([]string, error)
	// controllerNodeIPs reads all controller node addresses through the join
	// server, including secondary addresses that etcd does not advertise.
	controllerNodeIPs func(context.Context, string, []string) ([]string, error)
	// vipServes checks that the join's VIP answers as this cluster's apiserver
	// (VIPServes): a join with a VIP writes its kubeconfigs against it, so one
	// that does not serve the cluster refuses the join before the member-add.
	vipServes func(vip string) error
	// memberAdd adds this node to the cluster; memberRemove removes it again.
	memberAdd    func(cpIPs []string, name, peerURL string) (string, error)
	memberRemove func(cpIPs []string, peerURL string) error
	// clusterMembers reads the cluster's member list through the endpoints the
	// join dials. The cluster's list, not the intent file, is the truth about
	// this join's member: the join records in its intent file the ID it finds
	// there for this node's peer URL (memberByPeerURL), and compensates
	// against that list after a post-add failure.
	clusterMembers func(cpIPs []string) (etcdMembership, error)
	// memberRemoveByID removes one member by ID (memberRemoveID, etcd.go):
	// the post-add compensation for a join that failed after the member-add.
	memberRemoveByID func(cpIPs []string, id uint64) error
	// confirmQuorumHeld reports whether the cluster provably still has
	// quorum. Any error refuses the post-add member removal, fail closed
	// (confirmQuorumLost's semantics, internal/ha/quorum.go:39-69): an etcd
	// that is down, a peer that does not answer, or a member list that cannot
	// be read says nothing about the cluster's quorum, and removing a member
	// the cluster may still need is worse than leaving one a re-run can
	// reconcile, so the removal is refused and the intent file kept.
	confirmQuorumHeld func() error
	// saveState persists the node state the supervisor renders etcd's flags
	// from. The production set leaves it unset: prepareJoinNodeWithRecovery
	// fills it with the mutation owner's under-lock variant carrying the
	// capability of the data lock the join took (saveJoinStateUnderLock). The
	// join guards pass their recording set and keep it.
	saveState func(st *nodestate.NodeState) error
	// writeRuntimeConfigs writes the kubelet and containerd configs
	// (runtime.PrepareNodeConfigs).
	writeRuntimeConfigs func(st *nodestate.NodeState) error
	// assemble routes the join's common node steps through the shared node
	// assembly (AssembleNode), which cluster.PrepareController runs too: one
	// assembly for both paths, so a change to one path's node rendering reaches
	// the other. The join builds the NodeAssembly from the effects above and
	// hands the request its options resolve to. A field rather than a direct
	// call so the join guards can observe the join actually taking the shared
	// path; an effect set without one falls back to AssembleNode itself.
	assemble func(context.Context, NodeAssembly, AssembleRequest) (host.NodeIPChoice, error)
	// takeMaintenanceFence takes the cluster-wide maintenance fence
	// (takeJoinMaintenanceFence in production: both maintenance leases via
	// maintlease.AcquireCompatible, riding this joiner's bootstrap-token
	// credential) for the member-add window, from just before the live
	// member-add to the persisted node state. Member admission races the very
	// bundle and staged-key writes `stratum upgrade --cluster`, `rotate-ca
	// --cluster` and `migrate-etcd-ca` own, so a join landing mid-run refuses
	// fast, naming the live holder and its renewal age — that fast refusal
	// is the point. The take sits in this join's own commit phase, at the
	// command-level seam: the shared helpers (UploadCerts, NewCertKey, the
	// downloads) stay fence-free, because the orchestrators call them mid-run
	// and must keep working without refusing against their own fence. The
	// fence can be held and renewed only because the mint that published this
	// joiner's grant gave the control-plane bootstrap group the Lease access
	// the stratum:certs-reader Role carries. Nil (an effect set without one,
	// as the guards' recorders are) takes no fence.
	takeMaintenanceFence func() (*k8sutil.MaintenanceLease, error)
}

// productionJoinEffects is the real effect set: every field is the function
// the join called directly before the seam existed.
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

// saveJoinStateUnderLock is the join's state save: the write goes through
// nodestate's mutation owner under the data lock the join holds for its whole
// run, and the save keeps the published-state contract of saveJoinState
// (join_recovery.go): a failed save with node.yaml already on disk is
// reported as published but unconfirmed, so the join does not compensate a
// membership its record may survive, and a landed save is synced again
// before the join returns.
func saveJoinStateUnderLock(release func(), st *nodestate.NodeState) error {
	err := nodestate.MutateHeld(release, func(cur *nodestate.NodeState) error { *cur = *st; return nil })
	if err != nil {
		// A previous failed save may already have published node.yaml. Do
		// not compensate membership while that record can still survive.
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

// --- the shared node assembly ---
//
// Controller bootstrap (cluster.PrepareController) and the HA join run the same
// node steps — host directories, prerequisites, pod-CIDR trust, node-IP
// resolution, leaf certificates, kubeconfigs, the profile step, and the
// kubelet/containerd configs — through ONE assembly function, not two parallel
// bodies. The assembly lives here (the join's own package) because internal/
// cluster already imports internal/ha; the reverse would be an import cycle.
// The caller-specific steps hang off it at the three points the two paths'
// pinned orders fix: ResolveNet between the prerequisites and the pod-CIDR
// trust (the join's shared-cert download and cluster gates; the controller's
// netconfig resolution), BeforeCerts between the resolved node IP and the
// certificates (the controller's IP-move refusal and node state; the join's
// node-IP capture), and BeforeRuntimeConfigs between the profile step and the
// runtime configs (the join's retained-data re-check, etcd probe, and intent
// reconciliation; the controller has none). The join's effects struct routes
// its steps through it via fx.assemble; the controller holds it behind the
// assembleNode seam.

// AssembleRequest is what a caller hands the shared node assembly: the inputs
// the common steps consume, and the node state the runtime configs render
// from. State may be handed in partially built — the caller's hooks fill the
// fields only it knows (the netconfig's CIDRs, the resolved node IP, the
// carried membership) as the assembly resolves them — so the runtime configs
// still render the completed state.
type AssembleRequest struct {
	// Hostname is this node's name: the host the certificate and kubeconfig
	// effects run for, and the join's etcd member name.
	Hostname string
	// NodeIPFlag is --node-ip from the command line ("" when not given);
	// ConfigNodeIP is spec.network.nodeIP; SavedIP is the address a node with
	// saved state was brought up with ("" on a fresh bring-up or join). The IP
	// precedence is host.ResolveNodeIP's: flag, saved, config, $NODE_IP, then
	// detection.
	NodeIPFlag, ConfigNodeIP, SavedIP string
	// VIPAddress is the cluster VIP ("" when none): the extra apiserver SAN,
	// the kubeconfigs' VIP marker, and a controller's client-facing endpoint.
	VIPAddress string
	// APIServer is the endpoint every kubeconfig on this node targets: the
	// node state's recorded endpoint (bring-up's rule — the VIP when
	// configured, the local apiserver otherwise — which the join records
	// too), not the join URL.
	APIServer string
	// PrereqOptions are the host-preparation settings the prerequisites check
	// runs with (host.PrereqOptions): the node's profile, whose config files
	// the assembly writes, the SELinux opt-out, and whether this node was
	// brought up before.
	PrereqOptions host.PrereqOptions
	// State is the node state the runtime configs render from.
	State *nodestate.NodeState
}

// NodeAssembly is the shared node assembly's effects, as one struct parameter
// in the style of joinEffects and tokenSteps (internal/token/token.go): the
// safety order — every step that can refuse returns before the steps that
// follow — is otherwise held only by statement order, which no test can
// observe without touching the host. A struct rather than package variables
// keeps the tests that use a recording set safe to run in parallel.
//
// ResolveNet hands the assembly the cluster's network for this node; its
// CIDRs and domain feed the pod-CIDR trust, the address detection, and the
// apiserver SANs. BeforeCerts receives the resolved node IP choice before any
// certificate is issued, so a caller can refuse a move or pin the address into
// its node state first.
type NodeAssembly struct {
	// HostDirs creates the directory layout (host.Dirs).
	HostDirs func() error
	// Prereqs checks the host prerequisites for the settings in use
	// (host.Prereqs).
	Prereqs func(host.PrereqOptions) error
	// ResolveNet resolves the cluster's network for this node: the
	// controller's netconfig resolution, the join's shared-cert download and
	// cluster gates. Every one of them can refuse.
	ResolveNet func() (netcfg.Config, error)
	// TrustPodCIDRs opens the host firewall for the pod interfaces
	// (host.TrustPodCIDRs).
	TrustPodCIDRs func(podCIDR string)
	// ResolveNodeIP picks this node's address, falling back to detection
	// (host.ResolveNodeIP, host.DetectNodeIPs).
	ResolveNodeIP func(flag, config, state string, detect func() (string, error)) (host.NodeIPChoice, error)
	DetectNodeIPs func(dualStack bool) (string, error)
	// BeforeCerts runs between the resolved node IP and the certificates: the
	// controller's IP-move refusal and node-state completion, the join's
	// node-IP capture.
	BeforeCerts func(choice host.NodeIPChoice) error
	// IssueCerts mints this node's leaves from the shared CAs (pki.GenerateAll).
	IssueCerts func(host, nodeIP string, sans *pki.APISANs) error
	// WriteKubeconfigs writes the node's kubeconfigs (kubeconfig.GenerateAll).
	WriteKubeconfigs func(host, server string, vip bool) error
	// WriteProfileConfigs writes the config files the node's profile declares
	// and its apiserver flags reference (profilefiles.Write).
	// Runs only for a node with a profile, and before everything that
	// follows: it can refuse.
	WriteProfileConfigs func() error
	// BeforeRuntimeConfigs runs between the profile step and the runtime configs.
	// Nil runs nothing there; the join puts its retained-data re-check, etcd
	// probe, and intent reconciliation in it, so they still refuse before
	// anything that follows.
	BeforeRuntimeConfigs func() error
	// WriteRuntimeConfigs writes the kubelet and containerd configs
	// (runtime.PrepareNodeConfigs) on the request's node state.
	WriteRuntimeConfigs func(st *nodestate.NodeState) error
}

// AssembleNode runs the common node steps of controller bootstrap and
// control-plane join, in the order both paths' guards pin: host directories,
// prerequisites, the caller's network resolution, pod-CIDR trust, node-IP
// resolution, the caller's before-certificates hook, certificates, kubeconfigs,
// the profile step (a node with a profile only), the caller's before-runtime-configs hook, and
// the runtime configs on the caller's node state. Every fallible step returns
// before the steps that follow, and a hook's error is the error reported, so a
// caller's refusal text survives the shared assembly. The resolved node
// IP choice is returned for the caller's own use.
func AssembleNode(a NodeAssembly, req AssembleRequest) (host.NodeIPChoice, error) {
	return AssembleNodeContext(context.Background(), a, req)
}

// AssembleNodeContext stops between preparation steps when ctx ends. A step
// already in flight finishes and retains its own error; cancellation prevents
// the next step from starting, including mutations of local files or firewall.
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
	// The certificate inputs are the one constructor's (internal/certinputs):
	// the node address the assembly just resolved with the netconfig's
	// APIServerSANs and domain, the same named purposes bring-up, the join,
	// renewal and the upgrade issue under.
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

// vipNodeAddress names what vip is when it is a node's own address rather than
// a VIP: one of the existing controllers' node addresses, including secondary
// addresses that etcd does not advertise, or one
// of this node's own, given as its "v4[,v6]" node IP. It is "" for an address
// that is neither.
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

// sameIP reports whether a and b are the same address, by value rather than by
// spelling, so an IPv6 VIP written in another form still matches. An empty or
// unparsable side never matches.
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

// joinHalted refuses to start the next join step (what) once the run's
// context is done — a signal received during preflight, staging or an earlier
// step stops the join at that boundary instead of performing it. Every step
// before the live member-add is reversible, so a stop here leaves the cluster
// untouched.
func joinHalted(ctx context.Context, what string) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	return fmt.Errorf("HA join: %w before %s (nothing was added to etcd)", err, what)
}

// PrepareControlPlaneJoin readies this node as an additional control plane:
// payload, prereqs, shared certs, local leaves, live etcd member-add, and the
// persisted node state the supervisor renders etcd's flags from. The CALLER
// (stratum up --join) then starts the supervisor.
//
// ctx is the run's interrupt context (internal/interrupt), created by runUp
// before preflight and threaded through preparation: a signal received before
// the live member-add stops the join at a step boundary, never performing the
// add. From the member-add to the persisted node state the first signal is
// deliberately deferred — the policy is documented at that window.
func PrepareControlPlaneJoin(ctx context.Context, o JoinOptions) (*nodestate.NodeState, error) {
	return prepareControlPlaneJoin(ctx, o, version.EtcdDataDir, productionJoinEffects())
}

// prepareControlPlaneJoin runs the join's two phases in order: preparation,
// which runs every fallible effect that needs no member-add result and touches
// nothing membership-related, then the membership commit that consumes the
// value preparation returned.
//
// A join entered with the run already stopped — the signal arrived during
// preflight, staging or flag resolution — refuses at its first boundary,
// before the lock, staging, certificates or anything else is touched.
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

// preparedJoin is everything the membership-commit phase needs, returned by the
// preparation phase. Its fields are unexported, so no code outside internal/ha
// can build a populated one — the pattern of clusterinfo.Authenticated's
// unexported field (internal/bootstrap/clusterinfo/clusterinfo.go:118-122) —
// and the only constructor is prepareJoinNode. The zero value is inert:
// commitJoinMembership refuses it instead of acting on a guess, the way
// TrustBundle rejects an unauthenticated Authenticated.
//
// release is the etcd data-dir lock prepareJoinNode took: the commit phase
// releases it, because the critical section the join runs in spans both phases.
// A prepared value that is never committed holds the lock until the process
// exits, so a caller that prepared must always run the commit.
type preparedJoin struct {
	fx          joinEffects
	etcdIPs     []string // the endpoints the member-add and its compensation dial
	etcdDataDir string
	// peer is the peer URL this join's member lives at: the one the member-add
	// runs for, or — once adopted — the one the intent journal recorded.
	peer    string
	adopted bool // an intent-journal member was adopted; the commit adds nothing
	// offlineAdoption proves that preparation verified retained trust and
	// matched the recorded member's identity against etcd. Recovery publishes
	// only local state; requiring a new API lease could deadlock the very
	// member whose first start is needed to restore quorum.
	offlineAdoption bool
	// confirmGrant verifies the prepared trust is still published once fenced.
	confirmGrant func(context.Context) error
	// journal is the intent journal's in-memory image: what joinPostAddFailure
	// reports if the commit fails after the add.
	journal *joinIntent
	// st is the node state built before membership. The commit fills its
	// EtcdInitialCluster from the member-add's return before saving it.
	st *nodestate.NodeState
	// release ends the data-dir critical section preparation opened.
	release func()
}

// prepareJoinNode is the join's preparation phase: every fallible effect that
// needs no member-add result, in the safety order the guards pin — payload,
// then the shared node assembly (the ONE assembly cluster.PrepareController
// runs: the join's shared-cert download and cluster gates as its ResolveNet
// hook, the node-IP capture as its BeforeCerts hook, and the retained-data
// re-check, etcd probe, and intent reconciliation as its BeforeRuntimeConfigs
// hook). It never touches membership: no member is added, removed, or saved.
// The node state it hands the runtime-config write is built before the assembly
// and completed by its hooks, so the configs render from fields that are all
// known pre-add.
func prepareJoinNode(ctx context.Context, o JoinOptions, etcdDataDir string, fx joinEffects) (p preparedJoin, err error) {
	return prepareJoinNodeWithRecovery(ctx, o, etcdDataDir, fx, true)
}

// prepareJoinNodeWithRecovery threads the run's interrupt context so a signal
// received before the live member-add stops the join at a step boundary.
func prepareJoinNodeWithRecovery(ctx context.Context, o JoinOptions, etcdDataDir string, fx joinEffects, useRecovery bool) (p preparedJoin, err error) {
	if o.Server == "" || o.Token == "" || o.CAHash == "" || o.CertKey == "" {
		return preparedJoin{}, fmt.Errorf("HA join needs --join, --token, --discovery-token-ca-cert-hash and --certificate-key")
	}
	// The joining control plane's profile, for the host preparation and its
	// config files, before anything on the host changes.
	prof, err := profile.ForNode(o.Profile)
	if err != nil {
		return preparedJoin{}, fmt.Errorf("HA join: %w", err)
	}
	release, err := etcddata.Lock(etcdDataDir)
	if err != nil {
		return preparedJoin{}, err
	}
	// An error here ends the join before the member-add, so nothing needs the
	// lock held for a later phase; on success the membership-commit phase
	// releases it (the prepared value carries it).
	defer func() {
		if err != nil {
			release()
		}
	}()
	// The state save the commit phase runs goes through the mutation owner's
	// under-lock variant with this lock's capability: the join holds the data
	// lock for its whole run, and the owner writes without reacquiring —
	// which would refuse against the join's own descriptor. A caller that
	// brought its own saveState (the join guards' recording set) keeps it.
	if fx.saveState == nil {
		fx.saveState = func(st *nodestate.NodeState) error { return saveJoinStateUnderLock(release, st) }
	}
	// Refuse before staging, prerequisites, certificate download, or config
	// generation can alter the trust material needed to recover this member.
	if err := refuseRetainedMember(etcdDataDir); err != nil {
		return preparedJoin{}, err
	}
	// An intent file a previous join left behind is read before anything on
	// this node is written. One that cannot be used is an error, never absent:
	// treating it as absent would run a second member-add over the very
	// membership it exists to reconcile.
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

	// The node state is built before the assembly runs, so the assembly's
	// runtime-config write renders from it; its not-yet-known fields — the
	// netconfig's CIDRs, the resolved node IP, and the adopted membership —
	// are filled in by the caller-owned hooks as the assembly resolves them.
	// The kubeconfig endpoint is recorded here by bring-up's rule
	// (kubeconfig.NodeServerURL, the derivation serverURL pins): the VIP when
	// one is configured, the local apiserver otherwise — NOT the join URL,
	// which is only the bootstrap and discovery endpoint. Recording and
	// writing the same endpoint keeps a joined control plane's kubeconfigs
	// stable across its first restart (TestControlPlaneJoinRecordsTheBringUpEndpoint).
	// EtcdInitialCluster rides along where the join already knows it: the
	// adopted membership's string, derived from the member-list read the
	// adoption needed. A fresh join cannot know it before the add — the join
	// reads the cluster's member list only where reconciliation demands it —
	// so the commit phase fills it from the member-add's return before saving;
	// the runtime configs render no field from it.
	//
	// The server is the one every controller start writes (nodestate.ServerURL),
	// not the --join URL: the join's kubeconfigs and recorded server must be
	// what this node's first restart writes again, or that restart rewrites
	// them.
	st := &nodestate.NodeState{
		Role: "controller", Hostname: hostname,
		Server:    kubeconfig.NodeServerURL("", o.VIP.Address),
		EtcdName:  hostname,
		EtcdState: "existing",
		Profile:   o.Profile,
		CIS:       o.CIS,
		VIP:       o.VIP,

		NodeLabels:      o.NodeLabels,
		NodeTaints:      o.NodeTaints,
		RuntimeHandlers: o.RuntimeHandlers,
	}

	// The facts the hooks resolve, captured for the prepared value the
	// membership-commit phase consumes: the endpoints the member-add and its
	// compensation dial, this node's address, and the adopted membership.
	var (
		etcdIPs        []string
		primary        string
		peer           string
		adopted        bool
		adoptedInitial string
		memberID       uint64
		carried        []string
	)

	// The authenticated grant download runs BEFORE the shared node assembly:
	// its metadata is an input the assembly request consumes — the cluster
	// VIP this join inherits or refuses — and the request must carry it from
	// the start. The whole download group (the host directories and
	// prerequisites, discovery, the bootstrap kubeconfig, the stale
	// Cilium-settings clear, the download itself) runs here, in the order the
	// assembly would run it, and the assembly's already-run hooks become
	// no-ops below. A resume from retained preparation downloads nothing: its
	// ResolveNet answers from the journal.
	var grant *bundleGrant
	resumeRecovery := useRecovery && preIntent != nil && preIntent.Recovery != nil
	if resumeRecovery {
		// The original command may have omitted the address the authenticated
		// grant supplied. Resolve that omission from retained preparation before
		// the assembly request snapshots the VIP and endpoint. Explicit options
		// are still checked against the retained state in ResolveNet below.
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
		// The cluster VIP is a fact the cluster publishes about itself,
		// authenticated by the same ciphertext that carries the certificates,
		// rather than one a joining control plane takes from its own config
		// unchecked: a configured VIP that conflicts with it refuses the join
		// before any certificate is issued or etcd is touched, an unspecified
		// one inherits it into the node state and the kubeconfig endpoint rule
		// alike, and no metadata keeps the configured VIP and the no-VIP
		// shapes.
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
		// The production quorum gate needs the endpoints the join dials, which
		// exist from here on.
		if fx.confirmQuorumHeld == nil {
			fx.confirmQuorumHeld = func() error { return joinQuorumHeld(etcdIPs) }
		}
		preRan = true
	}

	a := NodeAssembly{
		HostDirs: fx.hostDirs,
		Prereqs:  fx.prereqs,
		// The join's shared-cert download and cluster gates are its ResolveNet
		// hook: they stand where the controller's netconfig resolution stands,
		// between the prerequisites and the pod-CIDR trust, and every one of
		// them can refuse, so they run before any certificate is issued.
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
				// A journal an older build wrote records the --join URL as the
				// server. The current endpoint rule decides it instead, so the resumed
				// join saves, and journals again, the endpoint its kubeconfigs
				// target.
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
			// The cluster gates run after the download, which ran in
			// preparation: the domain, network ranges and Cilium settings the
			// bundle delivered are what a configured override is checked
			// against, and the key gate asks what the download carried. Every
			// one of them can refuse, so all still stand before any
			// certificate is issued.
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

			// Cluster CIDRs rode in with the shared certs (netconfig); inherit them (a
			// configured range was checked to match them above) so this CP matches the
			// cluster's single/dual-stack shape. Then resolve node IPs: dual → "v4,v6" for
			// kubelet, v4 primary for etcd/single-IP uses.
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
		// A fresh join has no saved state, so the choice is flag, config, $NODE_IP,
		// then detection.
		BeforeCerts: func(choice host.NodeIPChoice) error {
			fmt.Printf("stratum HA join: node IP %s (%s)\n", choice.IP, choice.Source)
			primary = host.Primary(choice.IP)
			st.NodeIP, st.KubeletNodeIP = primary, choice.IP
			return nil
		},
		IssueCerts: func(host, nodeIP string, sans *pki.APISANs) error {
			// A signal received during the prerequisites or the shared-cert
			// download stops the join here, before this node mints its
			// certificates.
			if err := joinHalted(ctx, "generating node-local certificates"); err != nil {
				return err
			}
			k8sutil.Step(os.Stdout, "Generate node-local certs (shared CA) + kubeconfigs")
			return fx.issueCerts(host, nodeIP, sans) // loads shared ca/sa/front-proxy, mints local leaves
		},
		WriteKubeconfigs: fx.writeKubeconfigs,
		// The encryption key rode in on the shared-cert download in ResolveNet;
		// write the audit + admission configs the apiserver flags reference
		// before starting. Before the member-add, not after: this can refuse
		// (an unstamped binary cannot pin Pod Security), and nothing after the
		// add removes the member again on failure.
		WriteProfileConfigs: func() error {
			if err := fx.writeProfileConfigs(prof); err != nil {
				return fmt.Errorf("profile configs: %w (nothing was added to etcd)", err)
			}
			return nil
		},
		// The join's last refusals run where the assembly puts its
		// before-runtime-configs hook: the retained-data re-check, the etcd
		// probe, the intent reconciliation, and a fresh add's VIP check. The
		// intent journal, at the one place the member-add can follow it: a
		// previous join's intent is reconciled against the cluster's own member
		// list — the cluster, not the file, is the truth about membership — and
		// a fresh join records its intent before the add, so a failure or a
		// kill between the add and the state save leaves the member's identity
		// on disk.
		BeforeRuntimeConfigs: func() error {
			// Refuse retained member data again after preparation. Removal below is
			// atomic only for an empty directory: if anything appears after this check,
			// os.Remove fails with ENOTEMPTY instead of recursively deleting it.
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
					// The recorded member is still in the cluster: adopt it rather than
					// run a second member-add over the same peer URL (which etcd
					// refuses, leaving the stale member in place). Adoption is only
					// sound for the member this run would itself have added: a journal
					// for another host, or one at an address this run no longer
					// resolves, would finish the join into a node state whose initial
					// cluster names the recorded address while the node advertises the
					// resolved one — etcd refuses to start a member on an address its
					// cluster does not know. A joiner has no saved state to pin its
					// address between runs, so the refusal, not adoption, is what
					// keeps the record recoverable.
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
						// Refresh cluster trust/configuration through the API before
						// a new add. Keep the old journal until that succeeds, so a
						// failed refresh still routes published state into recovery.
						return errJoinIntentStale
					}
					// The cluster no longer lists the recorded member: the intent is
					// stale, and the join proceeds as a fresh one. Say so before the
					// record is replaced — the journal is the failed join's only
					// diagnostics until the new intent is durable.
					fmt.Fprintf(joinOut, "stratum HA join: the intent file %s records member %q at %s (ID %d, phase %s), but "+
						"the cluster's member list does not show it; treating the intent as stale, replacing it, and joining "+
						"as a fresh member\n", joinIntentPath, preIntent.MemberName, preIntent.PeerURL, preIntent.MemberID,
						preIntent.Phase)
					// Keep the recovery marker until the new prepared intent
					// atomically replaces it. A failed probe must not strand a
					// previously published node.yaml on the member-restart path.
				}
			}
			if !adopted {
				if preIntent != nil {
					if controllerIPs, err = fx.probeEtcd(etcdIPs); err != nil {
						return fmt.Errorf("HA join: cannot reach etcd before a fresh member-add: %w (%s)", err, concurrentJoinHint)
					}
				}
				// A join with a VIP wrote its kubeconfigs against the VIP, so a VIP
				// that does not answer as this cluster's apiserver — a mistyped
				// address, or one another service answers on — would leave the new
				// member's clients dialing an address that does not serve the
				// cluster. The check reads the cluster CA from those kubeconfigs,
				// and runs before the intent write and the data directory's
				// replacement, so its refusal leaves membership as it was. An
				// adopted member is already in the cluster, so only a fresh add
				// checks. A check that never reached the VIP, because no cluster
				// CA could be read from those kubeconfigs, says so and no more:
				// the address may be right, and changing it would not fix a
				// local problem. A node's own address is refused before the
				// check. An existing controller's address would pass it, because
				// every controller's apiserver certificate names its node
				// addresses, yet as the VIP it would point every kubeconfig here
				// at that one controller, and this node's kube-vip would announce
				// that controller's address. This node's own address would fail
				// it with bring-up steps that do not apply.
				if vip := st.VIP.Address; vip != "" {
					kind := vipNodeAddress(vip, controllerIPs, st.KubeletNodeIP)
					if kind == "" {
						// The live etcd probe identifies every current controller;
						// the bundle may predate a controller added later.
						// Etcd advertises only the primary address. The node
						// objects also carry secondary addresses covered by the
						// apiserver certificates, which would pass the TLS check.
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
		// Joined control planes run kubelet+containerd like every node (the control
		// plane itself is static pods under this kubelet). This can refuse, and it
		// needs no member-add result. Its writes are made durable with the
		// retained trust before the intent snapshot and membership commit.
		WriteRuntimeConfigs: fx.writeRuntimeConfigs,
	}
	// The node state carries the endpoint and VIP the request records: the
	// join derives both by bring-up's rule over the effective VIP — the
	// configured one, or the metadata's when the grant download inherited it
	// (the VIP gate above) — so the request hands the assembly what the state
	// records, and the two cannot drift.
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
		// The pre-download group ran in preparation, ahead of the assembly
		// request that consumes its metadata: the assembly's host-directory
		// and prerequisite hooks must not run the work twice. Every hook that
		// remains does its own job, so the common steps still run through
		// the shared assembly.
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
	// What the journal will record: the member's identity and resumable phase,
	// for the compensation below and for a re-run of the join. An adopted
	// member already has its ID, so its journal is at phase added.
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

// commitJoinMembership is the join's membership-commit phase: it consumes the
// opaque value prepareJoinNode returned, runs the etcd member-add, and then
// the node-state save, whose EtcdInitialCluster is the string the member-add
// returned (or the adopted membership gave) — the one write that genuinely
// needs the add's result. This phase runs exactly the membership effects —
// the member-add, then the save: everything fallible that needs no add result
// already ran in preparation, so a failure here either compensates by
// removing the member (joinPostAddFailure) or leaves the journal as the
// re-run's recovery record. The journal survives a completed save and ends
// only when a later start has confirmed the node's local etcd data, so the
// state between the save and etcd's first WAL is itself recoverable.
func commitJoinMembership(ctx context.Context, p preparedJoin) (*nodestate.NodeState, error) {
	// The zero preparedJoin is inert: no preparation ran, so there is no
	// member to add and no state to save. Refuse it rather than reconcile,
	// add, or save on a guess, the way TrustBundle rejects an unauthenticated
	// Authenticated.
	if p.fx.memberAdd == nil || p.st == nil {
		return nil, fmt.Errorf("HA join: the membership-commit phase ran on an unprepared value; " +
			"run the join's preparation phase (prepareJoinNode) first")
	}
	defer p.release()

	// The maintenance fence goes up here — after every refusing step of the
	// preparation, before the live member-add, the latest point that still
	// prevents it — and comes down when this phase returns, once the node
	// state is saved and the member-add window is over. Proven recovery of an
	// already-admitted member only publishes local state and skips admission
	// fencing: the API may lack quorum until that retained member starts.
	// A held fence refuses before anything is added, saved, or compensated,
	// and the refusal itself (holder and renewal age) is the error; the intent
	// journal written during preparation stays as the recovery record. The
	// release runs on every exit, success included: a fence left held would
	// refuse the next maintenance run until the lease self-expires.
	var lease *k8sutil.MaintenanceLease
	if !p.offlineAdoption && p.fx.takeMaintenanceFence != nil {
		var ferr error
		lease, ferr = p.fx.takeMaintenanceFence()
		if k8sutil.JoinFenceDenied(ferr) {
			// The admission policy refused the take, and the take's error
			// names the lease another run left and the way to clear it. The
			// token's Role grants the leases, so this is never the refusal
			// below for a Role that predates the grant, whatever the
			// denial's status.
			return nil, ferr
		}
		if apierrors.IsForbidden(ferr) {
			// The bootstrap token may not touch the leases: the mint that
			// printed this join command left the stratum:certs-reader Role
			// without the lease rules, and only a mint by a stratum that
			// publishes them repairs it. The journal stays, and the re-run
			// with a new command reconciles it against the member list.
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
		// The last boundary before the commit window opens: past this point
		// the join does not consult the context again until the node state is
		// saved (see the deferral policy there), so this is the latest point
		// a received signal still prevents the live member-add.
		if err := joinHalted(ctx, "the live etcd member-add"); err != nil {
			return nil, err
		}
		// The member-add and its answer are this phase's work: the initial
		// cluster this node must boot with is the string the add returns, or —
		// when the add's reply was lost — the one the cluster's member list
		// gives for the member it shows at this node's peer URL.
		initial, err := addMemberIfEtcdDataDirEmpty(p.etcdDataDir,
			func() (string, error) {
				if err := lease.ConfirmWithin(context.Background(), joinFenceConfirmWait); err != nil {
					return "", err
				}
				added, addErr := p.fx.memberAdd(p.etcdIPs, p.journal.MemberName, p.peer)
				if addErr == nil {
					return added, nil
				}
				// A refusal etcd answered outright cannot have committed: the
				// request was rejected before any proposal, so there is no lost
				// reply to reconcile — reading the member list or retrying would
				// guess around etcd's own answer. Fail with it; the journal stays
				// as the recovery record, and a re-run reconciles it.
				if memberAddRefused(addErr) {
					if isNotEnoughStarted(addErr) {
						return "", fmt.Errorf("%w (%s)", addErr, concurrentJoinHint)
					}
					return "", addErr
				}
				// The reply may have been lost after the request committed:
				// settle it against the cluster's member list, never a guess.
				return reconcileAmbiguousMemberAdd(p.fx, p.etcdIPs, p.journal.MemberName, p.peer, addErr)
			},
			func() error { return p.fx.memberRemove(p.etcdIPs, p.peer) })
		if err != nil {
			return nil, err
		}
		p.st.EtcdInitialCluster = initial
		// The add's answer is in; the cluster's member list is the truth about
		// the new member, so record the ID it gives for this node's peer URL.
		// This read needs the add's result — before it, the list cannot show
		// the member. A list that cannot be read — or that does not yet show
		// the member — leaves the journal at phase prepared: the compensation
		// below reads it again, fail closed, and a re-run reconciles by peer URL.
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
	// saveState stays after the member-add: the persisted state asserts the
	// membership the add established and carries the initial-cluster string
	// the add returned (or the adopted membership's), which does not exist
	// before the add. A failure here leaves a voting member behind, so the
	// compensation removes it when the cluster's quorum permits.
	if err := p.fx.saveState(p.st); err != nil {
		if errors.Is(err, errJoinStatePublished) {
			return nil, fmt.Errorf("HA join: %w; node state was published but its durability is unconfirmed; keeping %s and leaving membership unchanged. Re-run `stratum up --join` to reconcile and finish the save", err, joinIntentPath)
		}
		return nil, joinPostAddFailure(p.fx, p.etcdIPs, p.journal, err)
	}
	// The journal deliberately outlives the state save: etcd only starts later,
	// on the supervisor's first run, and a crash before etcd writes its WAL
	// would leave a published node.yaml with no WAL and no recovery record —
	// bare `stratum up` refuses such a node (requireLocalEtcdMember), and a
	// re-run of `stratum up --join` routes it to the member restart rather
	// than the join. With the journal in place that state resumes offline
	// instead. The journal ends when a later start has confirmed the node's
	// local etcd data: pkg/cli's member-restart path clears it once the WAL
	// gate passes, or a failed join that removed its member clears it here.
	// Deferral policy — the member-add-to-state-save commit window. From the
	// live MemberAdd to the SaveState above, nothing consults the context: a
	// stop inside that span would leave a voting member already added to
	// etcd with no node state recording it — an orphan. The journal changes
	// what an orphan costs — a re-run resumes from it — but the first signal
	// is still deferred: the window runs to completion and is observed here,
	// once the state is saved and stopping the join is safe again, so the
	// run reports the stop instead of pretending the join is still live. The
	// second signal needs no handling of its own: the interrupt handler
	// restores the signals' default action before its cancellation becomes
	// visible, so a second signal inside the window kills the process
	// exactly as it does everywhere else.
	if err := ctx.Err(); err != nil {
		fmt.Printf("stratum HA join: a signal was received while the etcd member was being added; "+
			"the member-add and the node-state save were completed before stopping — "+
			"repeat the original `stratum up --join` command to resume from the retained join intent (%v)\n", err)
		return nil, err
	}
	return p.st, nil
}

// joinPostAddFailure compensates a join that failed after its member-add: the
// state save is the one fallible write left after the add (every effect that
// needs no add result runs in preparation), and its failure leaves a voting
// etcd member for a node that never starts, so the member is removed again by
// ID. The cluster's
// member list, not the intent file, is the truth about the member: an ID the
// journal never learned is read from the list, and a member the list does not
// show is not removed on a guess. The removal runs only once the quorum gate
// has authorised it — any error there refuses it, fail closed
// (confirmQuorumLost's semantics, internal/ha/quorum.go:39-69), because
// removing a member the cluster may still need is worse than leaving one a
// re-run can reconcile. When the member is not removed, the intent file
// survives as the recovery record, and the error says how recovery proceeds.
func joinPostAddFailure(fx joinEffects, etcdIPs []string, j *joinIntent, cause error) error {
	recorded := fmt.Sprintf("the intent file %s keeps the member's identity and resumable phase (name %q, peer %s, ID %d, phase %s)",
		joinIntentPath, j.MemberName, j.PeerURL, j.MemberID, j.Phase)
	// The hand-removal remedy names the member ID only once it is known: the
	// journal may never have learned one (phase prepared), and
	// `etcdctl member remove 0` would send the operator after a member that
	// does not exist. Until the cluster's own member list gives the ID, the
	// remedy is to find this node's member there by its peer URL.
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

// joinFenceConfirmWait bounds how long the join's fence confirms wait out an
// unreachable control plane. The join reaches the cluster through the VIP,
// which can refuse connections for a moment while the previous control plane
// settles; discovery and the shared-certs read already wait as long. A var so
// a test need not.
var joinFenceConfirmWait = 2 * time.Minute

// takeJoinMaintenanceFence is the production maintenance fence: both
// maintenance leases (k8sutil.MaintenanceFenceAs, maintlease.AcquireCompatible)
// taken with this joiner's bootstrap-token credential, under the holder
// k8sutil.JoinMaintenanceHolder names that token by. The kubeconfig is read
// at take time, not at wiring time: the fence goes up in the membership-commit
// phase, after preparation has written it, and in that window it is this
// node's only working cluster credential — its kubeconfigs target the local
// apiserver the join is about to start, which does not exist yet. The take
// holds and renews only because the mint that published this joiner's grant
// gave the control-plane bootstrap group the leases access the
// stratum:certs-reader Role carries, and the join fence admission policy that
// mint put in place admits it only under that holder and never over a lease
// another run holds — a refusal leftMaintenanceLeases explains. The token is
// read from the kubeconfig's own auth info, as DownloadCerts reads it.
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

// leftMaintenanceLeases is the refusal for a fence take the join fence
// admission policy denied. maintlease writes over an existing lease only once
// its holder has stopped renewing it, and the policy lets a join token write
// over only a lease that is empty or already names that token, so the lease
// named another holder and had expired: a run that did not finish left it.
// The leases are read again to name each such lease and its holder. The way
// out is any control-plane mint, whose admin-credential fence takes an
// expired lease over and releases it, or deleting the lease by hand; the
// member-add had not started. denied, the apiserver's answer, is wrapped.
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
		// Gone or taken again since the denial: name the lease the apiserver
		// refused, which its answer carries.
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

// joinFenceClientset builds the clientset the join's maintenance fence rides
// from the bootstrap kubeconfig at path. A var so tests can stand a fake
// apiserver in for the cluster's.
var joinFenceClientset = func(path string) (kubernetes.Interface, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

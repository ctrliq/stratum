package ha

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/host"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/nodelock"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// rejoinPKIPreflight is indirected so a test can assert RejoinEtcd actually runs
// it, and runs it BEFORE anything destructive. Asserting EnsureEtcdTrustBundle's
// behaviour directly proves nothing about this call site: delete the call and
// such a test stays green while the failure it documents — etcd started against
// a --trusted-ca-file that does not exist, after the data dir is already wiped
// and the member re-added — comes back in full.
var rejoinPKIPreflight = pki.EnsureEtcdTrustBundle

// rejoinSupervisorRunning is indirected so the ordering test can run on a
// machine that is not a control plane. Without it that test skips
// everywhere, including CI, which is indistinguishable from passing.
var rejoinSupervisorRunning = comps.SupervisorRunning

var rejoinAcquireDataLock = func() (func(), error) { return etcddata.Lock(version.EtcdDataDir) }
var rejoinLoadState = nodestate.LoadState

// rejoinSeedMembers is indirected for the same reason, and carries the same
// ordering obligation: it is the probe that reaches the seed before anything
// is wiped, so one that runs after the wipe has nothing left to protect. It
// also returns what rejoin decides on: whether this node is already a member
// of the restored cluster.
var rejoinSeedMembers = memberList

var rejoinHostname = host.Hostname
var rejoinNodeIP = nodestate.NodeIP

// rejoinSeedIsThisNode reports whether the etcd that answers at the seed's address is
// this node's own etcd member: the seed is this node, however it was named. It is
// read only, and it asks the seed alone. This node's own etcd is never read: a
// survivor that restarted without quorum never becomes ready and answers nothing,
// and that is a case --force has to serve. The answering member is identified by
// its peer URLs, which are this node's addresses only if it is this node. A
// different member of the same cluster is not a refusal: that is the documented
// repair of a diverged member against its healthy majority, and the retry of a
// rejoin that joined but failed to start the apiserver, and in both the node's
// data is what --force is there to discard. Only the seed being this node would
// wipe the data the cluster runs on. A seed whose answering member cannot be
// identified is an error, and the caller refuses on an error.
var rejoinSeedIsThisNode = func(cp1IP string, own []string) (bool, error) {
	cli, err := etcdClient(cp1IP)
	if err != nil {
		return false, err
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ml, err := cli.MemberList(ctx)
	if err != nil {
		return false, err
	}
	return respondingMemberIsOwn(ml, cp1IP, own)
}

// respondingMemberIsOwn reports whether the member that produced ml, the one named
// by its header, has a peer URL on one of this node's own addresses. A member with
// no peer URL, or one whose host is not an IP address, is an error.
func respondingMemberIsOwn(ml *clientv3.MemberListResponse, cp1IP string, own []string) (bool, error) {
	if ml == nil || ml.Header == nil {
		return false, fmt.Errorf("the etcd at %s sent no member list header", cp1IP)
	}
	for _, m := range ml.Members {
		if m.ID != ml.Header.MemberId {
			continue
		}
		// A member that answers has started, and a started member advertises peer
		// URLs, each a literal IP as Stratum renders them. One that is missing, or
		// is not an IP, could be this node under a name ownAddress cannot match, so
		// it must not fall through as "not own" to the wipe.
		for _, pu := range m.PeerURLs {
			u, err := url.Parse(pu)
			if err != nil || net.ParseIP(u.Hostname()) == nil {
				return false, fmt.Errorf("the member that answered at %s advertises peer URL %q, which is not an IP address", cp1IP, pu)
			}
			if ownAddress(u.Hostname(), own...) {
				return true, nil
			}
		}
		if len(m.PeerURLs) == 0 {
			return false, fmt.Errorf("the member that answered at %s advertises no peer URL", cp1IP)
		}
		return false, nil
	}
	return false, fmt.Errorf("the member that answered at %s (%x) is not in its own member list", cp1IP, ml.Header.MemberId)
}

// ownAddress reports whether addr is any of this node's recorded addresses. An entry
// may be a comma-joined v4,v6 pair, as the kubelet node IP is on a dual-stack node.
func ownAddress(addr string, own ...string) bool {
	for _, o := range own {
		for _, ip := range strings.Split(o, ",") {
			if sameIP(addr, strings.TrimSpace(ip)) {
				return true
			}
		}
	}
	return false
}

// guardRejoinWipe runs before rejoin discards etcd data this node may still be
// serving from. Without force it needs proof that this node's own cluster has
// lost quorum: a seed that bootstrapped empty beside survivors that still hold
// one would otherwise overwrite them with a cluster holding nothing. With force
// it skips that proof, so the one thing it still refuses is the seed answering
// as this node's own member, reached by a name its recorded addresses do not
// match: that wipe would destroy the data the cluster is now running on.
func guardRejoinWipe(cp1IP string, own []string, force bool) error {
	if force {
		self, err := rejoinSeedIsThisNode(cp1IP, own)
		if err != nil {
			return fmt.Errorf("rejoin: cannot tell whether %s is this node's own etcd, so this node's data was "+
				"left alone: %w", cp1IP, err)
		}
		if self {
			return fmt.Errorf("rejoin: %s answers as this node's own etcd member: this node is the seed; run rejoin on "+
				"the OTHER control planes, with --server pointing at the node that ran `stratum snapshot restore`. "+
				"Nothing was changed", cp1IP)
		}
		return nil
	}
	if err := rejoinConfirmQuorumLost(); err != nil {
		return fmt.Errorf("rejoin: not wiping this node's etcd: %w. Its data may be newer than what %s holds, "+
			"and rejoin would destroy it. Nothing was changed. If the seed was deliberately restored from a "+
			"snapshot, or this node's cluster is known to be gone, and its data is to be discarded, re-run "+
			"with --force", err, cp1IP)
	}
	return nil
}

// rejoinHoldsMemberData reports whether this node's etcd data dir holds a
// member's data: a member directory with anything in it. A rejoin that has
// already wiped the dir and has not started etcd since leaves none, and then a
// replacement has nothing for guardRejoinWipe to protect. Anything it cannot
// read counts as data.
var rejoinHoldsMemberData = func() (bool, error) {
	entries, err := os.ReadDir(filepath.Join(version.EtcdDataDir, "member"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	return len(entries) > 0, nil
}

// The etcd operations after the preflights, indirected so the re-run paths
// below can be driven without a cluster.
var (
	rejoinLocalStatus   = memberStatusAt
	rejoinMemberHealthy = func(ip string) (localEtcdStatus, bool) {
		if !EtcdMemberStatus(ip).Healthy() {
			return localEtcdStatus{}, false
		}
		st, err := memberStatusAt(ip)
		return st, err == nil
	}
	rejoinAddLearner   = memberAddLearner
	rejoinRemoveMember = memberRemoveID
	rejoinPromote      = memberPromote
	// rejoinHeldLock is the release capability of the data lock the running
	// rejoin holds (RejoinEtcd): the save writes through the mutation owner's
	// under-lock variant with it, since reacquiring would refuse against the
	// rejoin's own descriptor. A rejoin runs alone — it takes the node
	// maintenance lock and the data lock before touching anything — so one
	// capability at a time is enough, and a nil one makes the owner refuse
	// rather than write unlocked.
	rejoinHeldLock func()
	// rejoinSaveState replaces the whole record with the state the rejoin
	// patched: it read that record under the same data lock it still holds,
	// so nothing can have changed it meanwhile.
	rejoinSaveState = func(st *nodestate.NodeState) error {
		return nodestate.MutateHeld(rejoinHeldLock, func(cur *nodestate.NodeState) error { *cur = *st; return nil })
	}
	rejoinStartPod    = staticpod.Start
	rejoinStopPod     = func(name string) error { return staticpod.Stop(name, 90*time.Second) }
	rejoinEtcdRunning = func() bool { running, err := staticpod.Running("etcd"); return running || err != nil }
	rejoinDataMark    = etcdDataMarkNow
	rejoinNow         = time.Now
	rejoinSleep       = time.Sleep
)

// etcdDataMark fingerprints this node's etcd member dir: its only progress before it is ready.
type etcdDataMark struct {
	bytes  int64
	newest time.Time
}

func etcdDataMarkNow() etcdDataMark {
	var m etcdDataMark
	_ = filepath.WalkDir(filepath.Join(version.EtcdDataDir, "member"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		m.bytes += info.Size()
		if info.ModTime().After(m.newest) {
			m.newest = info.ModTime()
		}
		return nil
	})
	return m
}

// How long rejoin waits for this node's etcd. The wait is
// on progress rather than a clock: it gives up only when this member has shown
// no progress for rejoinStallTimeout -- before it answers, no change to its
// data dir; after, no increase in its applied raft index. A member catching up
// on a large restored keyspace keeps it waiting for as long as it keeps moving,
// up to rejoinMaxWait, which only exists so that something that never
// converges still ends. A flat clock budget gives up on a slow but
// converging member (a small VM under SELinux), and the premature error
// sends the operator into the re-run that can brick the cluster.
var (
	rejoinStallTimeout = 5 * time.Minute
	rejoinMaxWait      = 30 * time.Minute
	rejoinPollInterval = 2 * time.Second
	rejoinReportEvery  = 30 * time.Second
	rejoinPromoteEvery = 10 * time.Second
)

// rejoinWipeLocalState is the first destructive step. It exists as a
// variable so a test can record whether the preflights above ran before
// it -- the ordering is the property, and neither preflight is worth
// anything if it can drift below this line.
var rejoinWipeLocalState = func(w io.Writer) error {
	k8sutil.Step(w, "rejoin: stop local apiserver + etcd")
	for _, name := range []string{"kube-apiserver", "etcd"} {
		if err := staticpod.Stop(name, 90*time.Second); err != nil {
			return fmt.Errorf("rejoin: could not stop %s: %w", name, err)
		}
	}
	if staticpod.Active("etcd") {
		return fmt.Errorf("rejoin: etcd still active after stop")
	}

	k8sutil.Step(w, "rejoin: wipe stale etcd data dir")
	if err := os.RemoveAll(version.EtcdDataDir); err != nil {
		return fmt.Errorf("rejoin: wipe %s: %w", version.EtcdDataDir, err)
	}
	return os.MkdirAll(version.EtcdDataDir, 0o700)
}

// rejoinAction is what a run does with what it finds. Rejoin has to
// be safe to re-run after a run that stopped anywhere -- a timeout, a Ctrl-C,
// a reboot -- so it decides from the seed's membership and this node's own
// etcd rather than assuming it starts from nothing.
type rejoinAction int

const (
	// rejoinFresh: this node is not a member. Wipe the stale data, add a learner.
	rejoinFresh rejoinAction = iota
	// rejoinReplace: this node has a member record that is not serving and is
	// safe to remove -- a learner, which never votes, or a voter the rest of
	// the cluster has a quorum without. Wipe, remove it, add a learner.
	rejoinReplace
	// rejoinResume: keep this node's data and member, start etcd, wait (and
	// promote a learner). Either the member is already serving as itself, or
	// it is a voter the cluster needs for quorum, which must never be wiped or
	// removed: wiping a vote the cluster is waiting on is how the restore was
	// bricked.
	rejoinResume
)

// planRejoin decides what to do, from facts and nothing else.
//
//   - not a member: fresh.
//   - this node's etcd answers as that member of that cluster: resume. It is
//     the first run's member, already ready, or done.
//   - a learner whose etcd is running but answers nothing yet: resume.
//   - any other learner that is not serving: replace. A learner has no vote,
//     so removing it can never cost quorum.
//   - a voter that is not serving: replace only if the OTHER voters that are
//     healthy are a quorum of the current voter set, since the removal has to
//     be committed by them; otherwise resume. The second is the shape a
//     voting add that timed out leaves -- a two-voter cluster whose second
//     voter is this node -- and the only way out of it is this member
//     starting.
//
// A membership read without quorum authorises only resume. It may be stale,
// and every other action removes a member or wipes a data dir on the strength
// of it -- in a cluster that by definition cannot commit the removal.
func planRejoin(seed etcdMembership, self *etcdMember, local localEtcdStatus, localErr error, localRunning bool,
	healthy func(ip string) (localEtcdStatus, bool)) (rejoinAction, string) {
	if self == nil {
		return rejoinFresh, "this node is not a member of the restored cluster"
	}
	if localErr == nil && local.ClusterID == seed.ClusterID && local.MemberID == self.ID {
		return rejoinResume, "this node's etcd is already serving as this member"
	}
	if !seed.Quorum {
		return rejoinResume, "the seed's member list was read without quorum, so it may be stale and only a resume is safe"
	}
	if self.IsLearner {
		if localErr != nil && localRunning {
			return rejoinResume, "this node's etcd is running but not answering yet, so it is still catching up"
		}
		return rejoinReplace, "this learner's etcd is not running"
	}
	voters, healthyOthers := 0, 0
	for _, m := range seed.Members {
		if m.IsLearner {
			continue
		}
		voters++
		if m.ID != self.ID && memberHealthy(m, seed.ClusterID, healthy) {
			healthyOthers++
		}
	}
	if healthyOthers >= voters/2+1 {
		return rejoinReplace, "the other voters have quorum without it"
	}
	return rejoinResume, "it is a voter the cluster needs for quorum"
}

// memberHealthy is "this member is up AND is this member of this cluster".
// Reachability alone is not enough: during a restore an endpoint can still be
// serving the pre-restore cluster, and counting it as a healthy voter is the
// direction that wipes a vote the restored cluster is waiting on.
func memberHealthy(m etcdMember, clusterID uint64, healthy func(ip string) (localEtcdStatus, bool)) bool {
	for _, cu := range m.ClientURLs {
		u, err := url.Parse(cu)
		if err != nil || u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" {
			continue
		}
		st, ok := healthy(u.Hostname())
		return ok && st.ClusterID == clusterID && st.MemberID == m.ID
	}
	return false
}

// sameInitialCluster reports whether the recorded initial-cluster already
// describes members, so a resume does not rewrite the state -- and with it the
// etcd manifest, which kubelet answers by restarting a member that may be
// halfway through starting.
func sameInitialCluster(recorded string, members []etcdMember, selfPeerURL, selfName string) bool {
	byURL := map[string]string{}
	for _, kv := range strings.Split(recorded, ",") {
		name, pu, ok := strings.Cut(kv, "=")
		if !ok {
			return false
		}
		byURL[pu] = name
	}
	n := 0
	for _, m := range members {
		for _, pu := range m.PeerURLs {
			name, ok := byURL[pu]
			if !ok || (pu == selfPeerURL && name != selfName) {
				return false
			}
			n++
		}
	}
	return n == len(byURL)
}

// rejoinStillLearner re-reads this node's role from the seed so the advice
// describes the member a re-run will find, not what this node was when the run
// started. A promote whose answer was lost leaves a voter behind while the
// caller still holds learner=true. A failed read keeps that value.
func rejoinStillLearner(cp1IP, selfPeerURL string, learner bool) bool {
	m, err := rejoinSeedMembers(cp1IP)
	if err != nil {
		return learner
	}
	self := m.memberByPeerURL(selfPeerURL)
	if self == nil {
		return learner
	}
	return self.IsLearner
}

func apiServerURL(ip string) string {
	return "https://" + net.JoinHostPort(ip, strconv.Itoa(version.APIServerPort))
}

// rejoinRerun is the re-run advice, worded for the state a failed run leaves.
// The old text promised the re-run would "clear the stale member
// and re-add", which against a voter the cluster needed was the step that
// bricked it. Re-running is now safe in every state, and the text says why.
func rejoinRerun(cp1IP string, learner bool) string {
	cmd := fmt.Sprintf("`stratum snapshot rejoin --server %s --yes`", apiServerURL(cp1IP))
	if learner {
		return "this node is an etcd learner, which has no vote, so the restored cluster's quorum is unaffected. " +
			"Re-running " + cmd + " is safe: it resumes while this member's etcd is running and replaces the learner " +
			"when it is not, asking for --force first only if this node's etcd data dir still holds member data"
	}
	return "this node is a VOTING etcd member and the cluster may need it for quorum: do not remove it or wipe " +
		"its data. etcd keeps retrying on its own; re-running " + cmd + " resumes the wait without wiping or " +
		"re-adding anything. The etcd pod's log is under /var/log/pods/kube-system_etcd-*. Only if this member " +
		"cannot be started at all: run `stratum snapshot restore` on the seed again, which discards whatever was " +
		"written since the last restore, then rejoin the other control planes"
}

// RejoinEtcd re-adds THIS control plane's etcd to a freshly-restored cluster.
// After `stratum snapshot restore` resets the seed node (cp1) to a NEW single-member
// cluster holding the restored data, every OTHER control plane runs this: it stops
// the local CP, wipes the stale (old-cluster) etcd data dir, adds this member to
// the seed's etcd as a learner, rewrites the etcd manifest as a joining member
// (state=existing), restarts, and promotes the learner once it has caught up.
// A promote a join left pending (EtcdPromotePending) stays recorded until the
// rejoin's own promote has landed, so a rejoin that stops with the member
// still a learner leaves the controller's start to promote it.
// The node's PKI + kubeconfigs are already in place — this is a rejoin, not a
// fresh join. cp1IP is the seed's IP (from --server).
//
// A re-run resumes rather than starting over: see planRejoin.
// force lifts the refusal to wipe an etcd whose cluster still has quorum; a
// path that does not wipe never needs it. The writer leads, after ctx: the
// periodic waiting report goes through it, so the CLI passes os.Stdout and
// the MCP handler the buffer its result is built from.
//
// ctx bounds the wait for this node's etcd, which can run to rejoinMaxWait and
// holds the etcd data lock for all of it -- a non-blocking flock, so every
// other stratum command that wants it fails outright meanwhile.
//
// RejoinEtcd takes the node maintenance lock (internal/nodelock) before the
// data lock and holds both for the whole run: it stops this node's control
// plane and wipes its etcd data dir, which no other maintenance operation --
// an upgrade, a rollback, a CA rotation -- must race. While one holds the
// lock, the rejoin refuses, naming the holder, before anything is touched.
func RejoinEtcd(ctx context.Context, w io.Writer, cp1IP string, force bool) error {
	hostname, err := rejoinHostname()
	if err != nil {
		return err
	}
	nodeIP, err := rejoinNodeIP()
	if err != nil {
		return err
	}
	// Match the same canonical peer URL used by join and etcd membership.
	newPeer := nodestate.EtcdPeerURL(nodeIP)
	// The node maintenance lock first, then the data lock: the data lock
	// fences only other writers of the etcd data dir, and an upgrade, rollback
	// or CA rotation takes none, so a rejoin that took it alone would run
	// beside them. While another maintenance operation holds the lock, the
	// rejoin refuses before anything is touched.
	releaseNode, err := nodelock.Acquire("rejoin")
	if err != nil {
		return err
	}
	defer releaseNode()
	release, err := rejoinAcquireDataLock()
	if err != nil {
		return err
	}
	defer release()
	// The state save writes under this run's held lock (rejoinHeldLock); it
	// is cleared again however the rejoin ends.
	rejoinHeldLock = release
	defer func() { rejoinHeldLock = nil }()

	// First, with the lock held: is this node a control plane at all? Rejoin
	// wipes the local etcd data dir, adds a member to the seed, and then
	// starts etcd from the patched state. On a worker, or on an empty node.yaml
	// (Role ""), that path would pass every preflight below and leave a voting
	// etcd member on a node still recorded as a worker. Checked
	// before the supervisor, PKI and seed preflights, so nothing about a node
	// that cannot be a rejoin target is looked at, let alone changed.
	st, err := rejoinLoadState()
	if err != nil {
		return fmt.Errorf("rejoin: %w (this node has no stratum state — was it ever a controller?)", err)
	}
	// The same for a profile this binary does not register: the renderers
	// rejoinStartPod reaches would stop on it only after the wipe and the add.
	if err := st.RequireRegisteredProfile("snapshot rejoin"); err != nil {
		return err
	}
	if sameIP(cp1IP, st.VIP.Address) {
		return fmt.Errorf("rejoin: %s is the cluster VIP, but rejoin needs the seed control plane's own address "+
			"(the node that ran `stratum snapshot restore`): etcd does not listen on the VIP. Re-run with "+
			"--server https://<seed-node-ip>:6443. Nothing was changed", cp1IP)
	}
	// The same for this node's own address. After a restore the seed prints
	// `rejoin --server https://<this-seed-ip>:6443 --yes --force` for the OTHER
	// control planes, and pasted on the seed it would pass every check below,
	// reach its own etcd, skip the quorum check with --force, and wipe the
	// restored data the cluster is now running on.
	if ownAddress(cp1IP, nodeIP, st.NodeIP, st.KubeletNodeIP) {
		return fmt.Errorf("rejoin: %s is this node's own address: this node is the seed; run rejoin on the OTHER "+
			"control planes, with --server pointing at this node. Nothing was changed", cp1IP)
	}
	if st.Role != "controller" {
		return fmt.Errorf("rejoin: this node's state (%s) records role %q, not controller; rejoin re-adds a "+
			"control plane's etcd member and would make this node a voting member it is not configured to be. "+
			"Nothing was changed", nodestate.StatePath(), st.Role)
	}

	if !rejoinSupervisorRunning() {
		return fmt.Errorf("rejoin: no stratum node agent is running on this node — start `stratum up` first (its kubelet applies the rejoined membership)")
	}
	// BEFORE anything destructive: etcd's argv points --trusted-ca-file at the
	// etcd trust bundle, and a node that has never had one would come
	// back up against a file that does not exist — after its data dir was already
	// wiped and it was re-added as a member.
	if err := rejoinPKIPreflight(); err != nil {
		return fmt.Errorf("rejoin: etcd trust material: %w", err)
	}
	// Also before anything destructive: reach the seed. The wipe below
	// destroys this node's etcd data, so without this a wrong or unreachable
	// --server destroys it and only then discovers there is nowhere to rejoin.
	// MemberList is read-only and uses the same client certs as every change
	// below, so it fails here for every reason those would.
	k8sutil.Step(w, "rejoin: reach the seed before touching local state")
	seed, err := rejoinSeedMembers(cp1IP)
	if err != nil {
		return fmt.Errorf("rejoin: cannot reach the etcd cluster at %s, so this node's data was "+
			"left alone: %w", cp1IP, err)
	}
	self := seed.memberByPeerURL(newPeer)
	if self != nil && seed.Responder == self.ID {
		return fmt.Errorf("rejoin: %s answers as this node's own etcd member %x: this node is the seed; run rejoin on "+
			"the OTHER control planes, with --server pointing at the node that ran `stratum snapshot restore`. "+
			"Nothing was changed", cp1IP, self.ID)
	}
	if !seed.Quorum && self == nil {
		return fmt.Errorf("rejoin: the etcd cluster at %s has no quorum and this node is not one of its "+
			"members, so there is nothing to resume and a new member cannot be added until the cluster "+
			"recovers; this node's data was left alone", cp1IP)
	}
	local, localErr := rejoinLocalStatus(nodeIP)
	action, why := planRejoin(seed, self, local, localErr, rejoinEtcdRunning(), rejoinMemberHealthy)
	servingAsSelf := self != nil && localErr == nil &&
		local.ClusterID == seed.ClusterID && local.MemberID == self.ID

	var id uint64
	var initial string
	learner := true
	etcdName := hostname // unique member name per CP (matches the original join)
	switch action {
	case rejoinFresh, rejoinReplace:
		held, err := rejoinHoldsMemberData()
		if err != nil {
			return fmt.Errorf("rejoin: cannot inspect %s (%w), so this node's data was left alone", version.EtcdDataDir, err)
		}
		if held {
			if err := guardRejoinWipe(cp1IP, []string{nodeIP, st.NodeIP, st.KubeletNodeIP}, force); err != nil {
				return err
			}
		}
		if action == rejoinReplace {
			k8sutil.Step(w, fmt.Sprintf("rejoin: this node's etcd member %x is not serving and is safe to remove (%s); replacing it", self.ID, why))
			// Before the wipe: the refusable step runs while the data is intact.
			if err := rejoinRemoveMember(cp1IP, self.ID); err != nil {
				return fmt.Errorf("rejoin: remove this node's stale etcd member %x via %s: %w — this node's etcd "+
					"member could not be removed, so its data was left alone; re-running rejoin is safe", self.ID, cp1IP, err)
			}
		}
		if err := rejoinWipeLocalState(w); err != nil {
			return err
		}
		k8sutil.Step(w, "rejoin: add this member to the restored cluster as a learner")
		id, initial, err = rejoinAddLearner(w, cp1IP, hostname, newPeer)
		if err != nil {
			return fmt.Errorf("rejoin: via %s: %w — this node's old etcd data was removed, and the add may "+
				"still have committed if only its reply was lost; re-running rejoin is safe: it re-reads the "+
				"membership and replaces a learner this attempt left", cp1IP, err)
		}
	case rejoinResume:
		id, learner = self.ID, self.IsLearner
		if self.Name != "" {
			etcdName = self.Name
		}
		initial = initialClusterFor(seed.Members, newPeer, etcdName)
		k8sutil.Step(w, fmt.Sprintf("rejoin: this node is already etcd member %x (%s): %s; resuming without wiping or re-adding",
			id, map[bool]string{true: "learner", false: "voter"}[learner], why))
	}

	// A serving member reads its membership from its own WAL, so rewriting a
	// stale recorded list only restarts it.
	rewriteState := st.EtcdName != etcdName || st.EtcdState != "existing"
	if !servingAsSelf {
		rewriteState = rewriteState || action != rejoinResume ||
			!sameInitialCluster(st.EtcdInitialCluster, seed.Members, newPeer, etcdName)
	}
	if rewriteState {
		k8sutil.Step(w, "rejoin: record the restored membership in node state")
		st.EtcdName = etcdName
		st.EtcdInitialCluster = initial
		st.EtcdState = "existing"
		if err := rejoinSaveState(st); err != nil {
			return err
		}
	}
	// Start re-renders etcd's manifest from the patched state; an unchanged
	// state renders an unchanged manifest, so a resumed member is not restarted.
	if err := rejoinStartPod("etcd"); err != nil {
		return err
	}
	k8sutil.Step(w, "rejoin: start etcd and wait for it to join")
	if err := waitRejoined(ctx, w, cp1IP, nodeIP, seed.ClusterID, id, learner); err != nil {
		return fmt.Errorf("rejoin: %w — %s", err, rejoinRerun(cp1IP, rejoinStillLearner(cp1IP, newPeer, learner)))
	}
	if st.EtcdPromotePending {
		st.EtcdPromotePending = false
		if err := rejoinSaveState(st); err != nil {
			return err
		}
	}
	if err := rejoinStartPod("kube-apiserver"); err != nil {
		return err
	}
	k8sutil.Step(w, "rejoin: control plane rejoined the restored cluster")
	return nil
}

// waitRejoined waits until this node's etcd serves as member id of the
// restored cluster and knows a leader, promoting it first if it is a learner.
// See rejoinStallTimeout for why the budget follows progress. A learner that
// stalls has its etcd stopped.
func waitRejoined(ctx context.Context, w io.Writer, cp1IP, nodeIP string, clusterID, id uint64, learner bool) error {
	start := rejoinNow()
	lastProgress, lastReport := start, start
	var lastPromote time.Time
	var promoteErr error
	var applied uint64
	seen := false
	mark := rejoinDataMark()
	for {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("stopped waiting for this node's etcd member %x (applied index %d): %w",
				id, applied, cerr)
		}
		now := rejoinNow()
		s, err := rejoinLocalStatus(nodeIP)
		answered := err == nil
		serving := answered && s.ClusterID == clusterID && s.MemberID == id
		if answered && !serving {
			err = fmt.Errorf("it answers as member %x of cluster %x", s.MemberID, s.ClusterID)
		}
		if serving && (!seen || s.AppliedIndex > applied) {
			seen, applied, lastProgress = true, s.AppliedIndex, now
		}
		if !answered {
			if m := rejoinDataMark(); m != mark {
				mark, lastProgress = m, now
			}
		}
		if serving && s.Leader != 0 {
			if !learner || !s.IsLearner {
				return nil
			}
			if lastPromote.IsZero() || now.Sub(lastPromote) >= rejoinPromoteEvery {
				lastPromote = now
				perr := rejoinPromote(id, cp1IP)
				switch {
				case perr == nil, isMemberNotLearner(perr):
					k8sutil.Step(w, fmt.Sprintf("rejoin: etcd member %x promoted to a voter", id))
					return nil
				case !isPromoteNotYet(perr):
					return fmt.Errorf("promote etcd learner %x via %s: %w", id, cp1IP, perr)
				}
				promoteErr = perr
			}
		}
		state := rejoinWaitState(serving, s, err, promoteErr)
		if now.Sub(lastProgress) >= rejoinStallTimeout {
			var stall error
			if !seen {
				stall = fmt.Errorf("this node's etcd has not answered as member %x, and its data dir has not changed, for %v (%s)",
					id, rejoinStallTimeout, state)
			} else {
				stall = fmt.Errorf("this node's etcd member %x made no progress for %v (applied index %d; %s)",
					id, rejoinStallTimeout, applied, state)
			}
			if !learner {
				return stall
			}
			if why := notConfirmedLearner(cp1IP, id); why != "" {
				return fmt.Errorf("%w; this node's etcd was left running, because %s, so a re-run resumes it", stall, why)
			}
			if serr := rejoinStopPod("etcd"); serr != nil {
				return fmt.Errorf("%w; stopping this learner's etcd also failed (%v), so stop it before re-running", stall, serr)
			}
			return fmt.Errorf("%w; this learner's etcd was stopped, so a re-run replaces it", stall)
		}
		if now.Sub(start) >= rejoinMaxWait {
			return fmt.Errorf("this node's etcd member %x had not finished joining after %v (applied index %d; %s); "+
				"its etcd was left running, so a re-run resumes the wait", id, rejoinMaxWait, applied, state)
		}
		if now.Sub(lastReport) >= rejoinReportEvery {
			lastReport = now
			fmt.Fprintf(w, "stratum snapshot rejoin: still waiting for etcd member %x (%v; %s; applied index %d)\n",
				id, now.Sub(start).Round(time.Second), state, applied)
		}
		rejoinSleep(rejoinPollInterval)
	}
}

// notConfirmedLearner is why the seed does not confirm that member id is still
// a learner, or "" when it does. A promote whose reply was lost may have
// committed, and stopping the member it made a voter can cost quorum.
func notConfirmedLearner(cp1IP string, id uint64) string {
	m, err := rejoinSeedMembers(cp1IP)
	switch {
	case err != nil:
		return fmt.Sprintf("the seed's member list could not be read (%v)", err)
	case !m.Quorum:
		return "the seed's member list was read without quorum"
	}
	for _, mm := range m.Members {
		if mm.ID == id {
			if mm.IsLearner {
				return ""
			}
			return "the seed lists it as a voter: a promote committed"
		}
	}
	return "the seed does not list it"
}

// rejoinWaitState is what the wait is still waiting on.
func rejoinWaitState(serving bool, s localEtcdStatus, err, promoteErr error) string {
	switch {
	case !serving:
		return fmt.Sprintf("not serving as this member: %v", err)
	case s.Leader == 0:
		return "serving, no leader yet"
	case promoteErr != nil:
		return fmt.Sprintf("learner, promote refused: %v", promoteErr)
	case s.IsLearner:
		return "learner, catching up"
	default:
		return "serving"
	}
}

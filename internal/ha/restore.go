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

// indirected so tests can pin that it runs before anything destructive
var rejoinPKIPreflight = pki.EnsureEtcdTrustBundle

// indirected so ordering tests run on a machine that is not a control plane
var rejoinSupervisorRunning = comps.SupervisorRunning

var rejoinAcquireDataLock = func() (func(), error) { return etcddata.Lock(version.EtcdDataDir) }
var rejoinLoadState = nodestate.LoadState

// indirected so tests can pin that it runs before the wipe
var rejoinSeedMembers = memberList

var rejoinHostname = host.Hostname
var rejoinNodeIP = nodestate.NodeIP

// asks the seed alone: a survivor without quorum answers nothing; a different member is the repair
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

func respondingMemberIsOwn(ml *clientv3.MemberListResponse, cp1IP string, own []string) (bool, error) {
	if ml == nil || ml.Header == nil {
		return false, fmt.Errorf("the etcd at %s sent no member list header", cp1IP)
	}
	for _, m := range ml.Members {
		if m.ID != ml.Header.MemberId {
			continue
		}
		// a non-IP peer URL must not fall through as not own: it could be this node
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

// an entry may be a comma-joined v4,v6 pair, as the kubelet node IP is on a dual-stack node
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

// indirected so tests can drive the re-run paths without a cluster
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
	// the data lock's release; reacquiring would refuse, and the lone rejoin needs only one capability
	rejoinHeldLock func()
	// the record was read under the data lock still held, so nothing can have changed it
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

// the member dir's only observable progress before etcd answers
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

// gives up only on no progress for rejoinStallTimeout; rejoinMaxWait just ends what never converges
var (
	rejoinStallTimeout = 5 * time.Minute
	rejoinMaxWait      = 30 * time.Minute
	rejoinPollInterval = 2 * time.Second
	rejoinReportEvery  = 30 * time.Second
	rejoinPromoteEvery = 10 * time.Second
)

// a var so tests can pin that the preflights run before the wipe
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

// decides from observed state, so a run stopped anywhere is safe to re-run
type rejoinAction int

const (
	rejoinFresh rejoinAction = iota
	rejoinReplace
	rejoinResume
)

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

// reachability alone lies during a restore: an endpoint can still serve the pre-restore cluster
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

// rewriting the state re-renders the etcd manifest, and kubelet restarts the member for it
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

// a promote whose reply was lost can leave a voter behind; re-read rather than trusting learner
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

// Re-adds this control plane's etcd to the restored cluster; --force lifts the quorum-wipe refusal.
func RejoinEtcd(ctx context.Context, w io.Writer, cp1IP string, force bool) error {
	hostname, err := rejoinHostname()
	if err != nil {
		return err
	}
	nodeIP, err := rejoinNodeIP()
	if err != nil {
		return err
	}
	// the canonical peer URL join and etcd membership use
	newPeer := nodestate.EtcdPeerURL(nodeIP)
	// the data lock fences only data-dir writers, so the node maintenance lock comes first
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
	rejoinHeldLock = release
	defer func() { rejoinHeldLock = nil }()

	st, err := rejoinLoadState()
	if err != nil {
		return fmt.Errorf("rejoin: %w (this node has no stratum state — was it ever a controller?)", err)
	}
	// renderers rejoinStartPod reaches would fail on it only after the wipe and the add
	if err := st.RequireRegisteredProfile("snapshot rejoin"); err != nil {
		return err
	}
	if sameIP(cp1IP, st.VIP.Address) {
		return fmt.Errorf("rejoin: %s is the cluster VIP, but rejoin needs the seed control plane's own address "+
			"(the node that ran `stratum snapshot restore`): etcd does not listen on the VIP. Re-run with "+
			"--server https://<seed-node-ip>:6443. Nothing was changed", cp1IP)
	}
	// pasted on the seed, --server <own address> would wipe the restored data under --force
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
	// etcd's argv points --trusted-ca-file at the bundle; a wiped node must not start without it
	if err := rejoinPKIPreflight(); err != nil {
		return fmt.Errorf("rejoin: etcd trust material: %w", err)
	}
	// an unreachable seed must fail before the wipe; MemberList uses the certs every change below does
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
			// the refusable step runs while the data is intact, before the wipe
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

	// a serving member reads its membership from its own WAL; rewriting would only restart it
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
	// start re-renders the manifest from the state; an unchanged state is not a restart
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

// a promote whose reply was lost may have committed; stopping the voter it made can cost quorum
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

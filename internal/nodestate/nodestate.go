// Package nodestate persists what a node IS — written at bring-up/join, read
// on every controller start, patched by rejoin/restore. A leaf package so the
// join/restore machinery (internal/ha) and the supervisor roster
// (internal/cluster) can share it without an import cycle.
package nodestate

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
	"sigs.k8s.io/yaml"
)

// VIP is the kube-vip control-plane address. Empty means
// single-controller / no VIP: clients target this node directly.
type VIP struct {
	Address   string `json:"address,omitempty"`
	Interface string `json:"interface,omitempty"` // default: kube-vip's choice, the default-route interface
	BGP       bool   `json:"bgp,omitempty"`       // BGP mode instead of ARP (routed networks)
}

// NodeState is what this node IS — written at bring-up/join, read on every
// controller start, patched by rejoin/restore. The supervisor renders etcd's
// argv from it on each (re)start, which is what lets `stratum snapshot rejoin`
// change etcd's cluster membership with a component restart instead of a
// controller restart.
type NodeState struct {
	Role               string `json:"role"` // controller | worker
	Hostname           string `json:"hostname"`
	NodeIP             string `json:"nodeIP"`        // primary (v4) IP
	KubeletNodeIP      string `json:"kubeletNodeIP"` // "v4[,v6]" as registered
	Server             string `json:"server"`        // apiserver URL clients use
	EtcdName           string `json:"etcdName,omitempty"`
	EtcdInitialCluster string `json:"etcdInitialCluster,omitempty"`
	EtcdState          string `json:"etcdState,omitempty"` // new | existing
	DualStack          bool   `json:"dualStack,omitempty"`
	PodCIDR            string `json:"podCIDR,omitempty"`
	ServiceCIDR        string `json:"serviceCIDR,omitempty"`
	// ClusterDNSIP is the kube-dns Service address, read from the cluster by a
	// worker with no recorded ServiceCIDR (one that joined before the DNS
	// address was recorded), so it still hands its pods the right resolver.
	// ServiceCIDR wins.
	ClusterDNSIP string `json:"clusterDNSIP,omitempty"`
	// ClusterDomain is spec.network.clusterDomain; empty on a node that
	// predates it, which Domain reads as the default.
	ClusterDomain string `json:"clusterDomain,omitempty"`
	// Profile is the node profile this node runs, by name; empty for none.
	// CIS is the legacy spelling of Profile "cis": both are written for one
	// release so a binary that predates Profile still reads the profile, and
	// ProfileName reads whichever a state carries.
	Profile string `json:"profile,omitempty"`
	CIS     bool   `json:"cis,omitempty"`
	// SELinuxOptOut is a worker's spec.selinux: false, read from its config
	// file when it joined. A control plane reads it from its config file on
	// every start.
	SELinuxOptOut bool `json:"selinuxOptOut,omitempty"`
	VIP           VIP  `json:"vip,omitempty"`
	// EtcdHasPeers records that this member's etcd cluster has had more than
	// one member. It is deliberately not folded into EtcdInitialCluster and
	// EtcdState, which are etcd's argv: rewriting them changes the etcd manifest,
	// so the next controller start would restart etcd.
	EtcdHasPeers bool `json:"etcdHasPeers,omitzero"`
	// ConfigPath is the cluster config file the controller was started with
	// (`stratum up --config`), recorded so other commands read the same file
	// the scheduled loops do. Empty on a node that has not restarted since it
	// began recording it; readers then use the default path.
	ConfigPath string `json:"configPath,omitempty"`
	// RuntimeHandlers is spec.node.runtimeHandlers: extra containerd runtime
	// handlers by name, each the absolute path of its OCI runtime binary.
	RuntimeHandlers map[string]string `json:"runtimeHandlers,omitempty"`
	// NodeLabels and NodeTaints are spec.node.labels and spec.node.taints:
	// what this node declares on its Node object. A taint is key[=value]:Effect.
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`
	NodeTaints []string          `json:"nodeTaints,omitempty"`
}

// CISProfile is the name of the engine's CIS profile.
const CISProfile = profile.CISName

// ProfileName is the profile this node runs, from Profile or the legacy CIS
// field; empty for none.
func (s *NodeState) ProfileName() string {
	if s.Profile != "" {
		return s.Profile
	}
	if s.CIS {
		return CISProfile
	}
	return ""
}

// normalizeProfile makes Profile and CIS agree, Profile winning, so a state
// is written and read the same way whichever field set it.
func (s *NodeState) normalizeProfile() {
	s.Profile = s.ProfileName()
	s.CIS = s.Profile == CISProfile
}

// Domain is the cluster DNS domain this node runs with.
func (s *NodeState) Domain() string {
	if s.ClusterDomain == "" {
		return netdefault.ClusterDomain
	}
	return s.ClusterDomain
}

// EtcdPeerURL is the peer URL every writer of NodeState.EtcdInitialCluster
// hands to etcd. IPv6 hosts must be bracketed both for etcd's URL validation
// and for matching the peer URLs returned by its membership API.
func EtcdPeerURL(ip string) string {
	return "https://" + net.JoinHostPort(ip, strconv.Itoa(version.EtcdPeerPort))
}

// EtcdClientURL is the client URL etcd advertises and every client of it
// dials. Built beside EtcdPeerURL so the URL a member serves and the one the
// HA machinery looks it up by cannot drift; the bracketing rule is the peer
// URL's: a bare IPv6 literal reads the port as another hextet of the address.
func EtcdClientURL(ip string) string {
	return "https://" + net.JoinHostPort(ip, strconv.Itoa(version.EtcdClientPort))
}

// StatePath is where NodeState persists.
func StatePath() string { return filepath.Join(version.K8sConfDir, "node.yaml") }

// SaveState persists the node state through the shared atomic writer (the
// unique hidden temp file, fsync at the final mode, rename, directory fsync)
// at mode 0600: node.yaml is the node's own state record, and a torn or
// world-readable one must never be what a start reads. It is the mutation
// owner's write backend: a caller that changes node.yaml records its change
// through Mutate or MutateHeld, not by saving a copy it loaded itself.
func SaveState(st *NodeState) error {
	st.normalizeProfile()
	b, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(StatePath()), 0o755); err != nil {
		return err
	}
	return atomicfile.WriteFile(StatePath(), b, 0o600)
}

// ErrUnchanged is what a mutate returns when it changed nothing: the owner
// treats it as success and writes nothing, keeping what node.yaml already
// holds. A poll that finds nothing to record — the growth recorder on a seed
// whose etcd has not grown — must not rewrite the file with the bytes it just
// read, once a minute, for as long as the node stays in that state.
var ErrUnchanged = errors.New("nodestate: the mutation changed nothing")

// Mutate is the one mutation owner for node.yaml. Every writer records its
// change by load-modify-save on the one shared file, so a writer that applies
// its change to a copy it loaded earlier — or to a fresh struct — loses
// whatever another writer recorded in between. The owner re-reads the state
// from disk, runs mutate against what it read — keeping what the writer did
// not set — and writes it back with SaveState's durable write (mode 0600). A
// mutate that returns an error writes nothing; one that returns ErrUnchanged
// changed nothing and writes nothing, as a success. A missing node.yaml is a
// fresh empty state: a first join's write has nothing to read.
//
// The read-modify-write runs under the etcd data lock (etcddata.Lock): a
// rejoin, a restore or an `up` holds that lock while it changes node.yaml, so
// while one of them runs the mutation refuses instead of writing between that
// operation's read and write. Callers retry on the next pass or start, the
// way the growth recorder's poll does. Any failure to acquire the lock refuses
// the mutation: an unavailable lock provides no exclusion against other writers.
func Mutate(mutate func(*NodeState) error) error {
	return mutateState(mutate, SaveState)
}

// MutateSave is Mutate with the caller's writer for the final persist, for a
// caller whose save sits behind a seam (internal/cluster's recordConfigPath
// hands it the package's SaveState). The lock contract is the owner's.
func MutateSave(mutate func(*NodeState) error, save func(*NodeState) error) error {
	return mutateState(mutate, save)
}

// MutateHeld is the owner's under-lock variant, for a caller already holding
// the etcd data lock — a join mid-join, a restore, the growth recorder.
// release is the capability etcddata.Lock returned to that caller; the owner
// writes without acquiring again, which would refuse: the flock is held by
// the caller's own descriptor. A nil capability means the caller does not
// hold the lock, and the mutation refuses rather than write unlocked — that
// write is what the owner exists to remove. The refusal is expected input,
// not a panic or a silent write.
func MutateHeld(release func(), mutate func(*NodeState) error) error {
	if release == nil {
		return errors.New("nodestate: MutateHeld needs the release of the held etcd data lock (etcddata.Lock's capability); nil means the caller does not hold the lock")
	}
	st, err := stateForMutation()
	if err != nil {
		return err
	}
	return applyMutation(st, mutate, SaveState)
}

// mutateState holds the etcd data lock across the read, mutation and save.
func mutateState(mutate func(*NodeState) error, save func(*NodeState) error) error {
	release, err := etcddata.Lock(version.EtcdDataDir)
	if err != nil {
		return err
	}
	defer release()

	st, err := stateForMutation()
	if err != nil {
		return err
	}
	return applyMutation(st, mutate, save)
}

// applyMutation is the owner's write half: the mutation lands on the state
// read under the lock, and a mutate that changed nothing writes nothing. Any
// other error stops the save.
func applyMutation(st *NodeState, mutate func(*NodeState) error, save func(*NodeState) error) error {
	if err := mutate(st); err != nil {
		if errors.Is(err, ErrUnchanged) {
			return nil
		}
		return err
	}
	return save(st)
}

// stateForMutation reads the state a mutation applies to: what is on disk, or
// a fresh empty state on a node that has none yet — a first join's write has
// nothing to read. State that exists but cannot be read is an error, never a
// silently fresh file.
func stateForMutation() (*NodeState, error) {
	st, err := LoadState()
	if err == nil {
		return st, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return &NodeState{}, nil
	}
	return nil, err
}

// LoadState reads the persisted node state.
func LoadState() (*NodeState, error) {
	b, err := os.ReadFile(StatePath())
	if err != nil {
		return nil, err
	}
	st := &NodeState{}
	if err := yaml.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("%s: %w", StatePath(), err)
	}
	st.normalizeProfile()
	return st, nil
}

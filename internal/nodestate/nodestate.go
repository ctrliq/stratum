// Package nodestate persists what a node is, shared by internal/ha and internal/cluster.
package nodestate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ctrliq/stratum/internal/atomicfile"
	"github.com/ctrliq/stratum/internal/boundedfile"
	"github.com/ctrliq/stratum/internal/etcddata"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
	"sigs.k8s.io/yaml"
)

// VIP is the kube-vip control-plane address. Empty means no VIP.
type VIP struct {
	Address   string `json:"address,omitempty"`
	Interface string `json:"interface,omitempty"` // empty: kube-vip's default-route choice
	BGP       bool   `json:"bgp,omitempty"`       // BGP mode instead of ARP (routed networks)
}

// NodeState is what this node is; the supervisor renders etcd's argv from it each start.
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
	// Read from the cluster by a worker with no recorded ServiceCIDR; ServiceCIDR wins.
	ClusterDNSIP string `json:"clusterDNSIP,omitempty"`
	// Empty on a node that predates it, which Domain reads as the default.
	ClusterDomain string `json:"clusterDomain,omitempty"`
	// CIS is the legacy spelling; both are written so a pre-Profile binary still reads it.
	Profile string `json:"profile,omitempty"`
	CIS     bool   `json:"cis,omitempty"`
	// A worker's spec.selinux: false, read from its config file when it joined.
	SELinuxOptOut bool `json:"selinuxOptOut,omitempty"`
	VIP           VIP  `json:"vip,omitempty"`
	// Records a learner added by a control-plane join that has not been promoted yet.
	EtcdPromotePending bool `json:"etcdPromotePending,omitzero"`
	// Not folded into EtcdInitialCluster/EtcdState: rewriting them restarts etcd.
	EtcdHasPeers bool `json:"etcdHasPeers,omitzero"`
	// The cluster config file the controller was started with; empty until recorded.
	ConfigPath string `json:"configPath,omitempty"`
	// spec.node.runtimeHandlers: extra containerd runtime handlers by name.
	RuntimeHandlers map[string]string `json:"runtimeHandlers,omitempty"`
	// spec.node.labels and spec.node.taints; a taint is key[=value]:Effect.
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`
	NodeTaints []string          `json:"nodeTaints,omitempty"`
	// The kubelet applies taints only when it creates the Node.
	NodeTaintsCheckPending bool `json:"nodeTaintsCheckPending,omitzero"`
}

// CISProfile is the name of the engine's CIS profile.
const CISProfile = profile.CISName

// ProfileName is the profile this node runs, from Profile or the legacy CIS field.
func (s *NodeState) ProfileName() string {
	if s.Profile != "" {
		return s.Profile
	}
	if s.CIS {
		return CISProfile
	}
	return ""
}

// normalizeProfile makes Profile and CIS agree, Profile winning.
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

// EtcdPeerURL is the peer URL every writer of EtcdInitialCluster hands to etcd.
func EtcdPeerURL(ip string) string {
	return "https://" + net.JoinHostPort(ip, strconv.Itoa(version.EtcdPeerPort))
}

// EtcdClientURL is the client URL etcd advertises, built beside EtcdPeerURL.
func EtcdClientURL(ip string) string {
	return "https://" + net.JoinHostPort(ip, strconv.Itoa(version.EtcdClientPort))
}

// StatePath is where NodeState persists.
func StatePath() string { return filepath.Join(version.K8sConfDir, "node.yaml") }

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

// A mutate returns this when it changed nothing, and the owner writes nothing.
var ErrUnchanged = errors.New("nodestate: the mutation changed nothing")

// Mutate re-reads the state under the etcd data lock, runs mutate on it, and writes it back.
func Mutate(mutate func(*NodeState) error) error {
	return mutateState(mutate, SaveState)
}

// MutateSave is Mutate with the caller's writer for the final persist.
func MutateSave(mutate func(*NodeState) error, save func(*NodeState) error) error {
	return mutateState(mutate, save)
}

// MutateHeld is Mutate for a caller already holding the etcd data lock; nil release refuses.
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

// The mutation lands on the state read under the lock.
func applyMutation(st *NodeState, mutate func(*NodeState) error, save func(*NodeState) error) error {
	if err := mutate(st); err != nil {
		if errors.Is(err, ErrUnchanged) {
			return nil
		}
		return err
	}
	return save(st)
}

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

func LoadState() (*NodeState, error) {
	return loadState(os.ReadFile)
}

// LoadStateContext bounds the read of the persisted state.
func LoadStateContext(ctx context.Context) (*NodeState, error) {
	return loadState(func(path string) ([]byte, error) { return boundedfile.Read(ctx, path) })
}

func loadState(read func(string) ([]byte, error)) (*NodeState, error) {
	b, err := read(StatePath())
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

package certs

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ctrliq/stratum/internal/k8sutil"
)

// etcdMigrationNodes is the only thing that keeps workers out of the phase
// loop, and it had no test. The previous attempt at covering this asserted that
// no dispatch name began with "worker" — on a fixture containing no worker, so
// it passed vacuously and would have passed against an implementation that
// migrated every node in the cluster.

func cpNode(name, ip string, cp bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if cp {
		n.Labels[k8sutil.ControlPlaneLabel] = "true"
	}
	if ip != "" {
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}
	}
	return n
}

func TestEtcdMigrationNodesSelectsControlPlanesOnly(t *testing.T) {
	cs := fake.NewSimpleClientset(
		cpNode("worker2", "10.0.0.52", false),
		cpNode("cp2", "10.0.0.2", true),
		cpNode("cp1", "10.0.0.1", true),
		cpNode("worker1", "10.0.0.51", false),
		cpNode("cp3", "10.0.0.3", true),
	)
	got, err := etcdMigrationNodes(context.Background(), cs, "cp2")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range got {
		names = append(names, n.Name)
	}
	// Initiator first — it mints the etcd CA and every follower fetches it —
	// then the rest in a stable order so a resumed run dispatches identically.
	want := []string{"cp2", "cp1", "cp3"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("node order = %v, want %v", names, want)
	}
	for _, n := range got {
		if strings.HasPrefix(n.Name, "worker") {
			t.Errorf("a worker was included: %v — workers hold no etcd leaf and receive no etcd CA", names)
		}
		if n.IP == "" {
			t.Errorf("%s has no IP, so its etcd could not be surveyed", n.Name)
		}
	}
}

func TestEtcdMigrationNodesRefusesFromAWorker(t *testing.T) {
	cs := fake.NewSimpleClientset(cpNode("cp1", "10.0.0.1", true), cpNode("worker1", "10.0.0.51", false))
	_, err := etcdMigrationNodes(context.Background(), cs, "worker1")
	if err == nil {
		t.Fatal("migration accepted a worker as the initiator")
	}
	if !strings.Contains(err.Error(), k8sutil.ControlPlaneLabel) {
		t.Errorf("refusal should name the label that decides this, got: %v", err)
	}
}

func stubEtcdMemberIPs(t *testing.T, ips []string, err error) *[]string {
	t.Helper()
	old := etcdMemberIPs
	t.Cleanup(func() { etcdMemberIPs = old })
	var asked []string
	etcdMemberIPs = func(via []string) ([]string, error) {
		asked = via
		return ips, err
	}
	return &asked
}

// A controller whose control-plane label never landed is still a voting etcd
// member. A run built from the label alone would change every other member's
// trust, survey only them, and report success while that one falls out of
// raft, so the run refuses before any phase and names the node and the remedy.
func TestEtcdMigrationRefusesAnEtcdMemberThatIsNotLabelled(t *testing.T) {
	cs := fake.NewSimpleClientset(
		cpNode("cp1", "10.0.0.1", true),
		cpNode("cp2", "10.0.0.2", false),
		cpNode("cp3", "10.0.0.3", true),
	)
	nodes, err := etcdMigrationNodes(context.Background(), cs, "cp1")
	if err != nil {
		t.Fatal(err)
	}
	asked := stubEtcdMemberIPs(t, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, nil)
	err = etcdMembersAreMigrationNodes(context.Background(), cs, nodes)
	if err == nil {
		t.Fatal("a run that leaves etcd member cp2 out was accepted")
	}
	for _, want := range []string{"10.0.0.2 (node cp2)", "stratum kubectl label node cp2 " + k8sutil.ControlPlaneLabel + "=true --overwrite"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should name %q, got: %v", want, err)
		}
	}
	if strings.Join(*asked, ",") != "10.0.0.1,10.0.0.3" {
		t.Errorf("member list read through %v, want the listed control planes' etcd", *asked)
	}
}

func TestEtcdMigrationAcceptsWhenEveryMemberIsLabelled(t *testing.T) {
	cs := fake.NewSimpleClientset(cpNode("cp1", "10.0.0.1", true), cpNode("cp2", "10.0.0.2", true), cpNode("worker1", "10.0.0.51", false))
	nodes, err := etcdMigrationNodes(context.Background(), cs, "cp1")
	if err != nil {
		t.Fatal(err)
	}
	stubEtcdMemberIPs(t, []string{"10.0.0.2", "10.0.0.1"}, nil)
	if err := etcdMembersAreMigrationNodes(context.Background(), cs, nodes); err != nil {
		t.Fatalf("every etcd member is a labelled control plane, got: %v", err)
	}
}

// A member list that cannot be read cannot confirm the run covers etcd, so
// the run does not start.
func TestEtcdMigrationRefusesWhenTheMemberListIsUnreadable(t *testing.T) {
	cs := fake.NewSimpleClientset(cpNode("cp1", "10.0.0.1", true))
	nodes, err := etcdMigrationNodes(context.Background(), cs, "cp1")
	if err != nil {
		t.Fatal(err)
	}
	stubEtcdMemberIPs(t, nil, errors.New("context deadline exceeded"))
	if err := etcdMembersAreMigrationNodes(context.Background(), cs, nodes); err == nil || !strings.Contains(err.Error(), "member list") {
		t.Fatalf("an unreadable member list should refuse, got: %v", err)
	}
}

// A control plane with no InternalIP cannot be surveyed, and the survey is the
// authority for every gate. Failing here beats silently dialling "".
func TestEtcdMigrationNodesRefusesAControlPlaneWithNoIP(t *testing.T) {
	cs := fake.NewSimpleClientset(cpNode("cp1", "10.0.0.1", true), cpNode("cp2", "", true))
	_, err := etcdMigrationNodes(context.Background(), cs, "cp1")
	if err == nil {
		t.Fatal("migration accepted a control plane with no InternalIP")
	}
	if !strings.Contains(err.Error(), "cp2") {
		t.Errorf("refusal should name the node, got: %v", err)
	}
}

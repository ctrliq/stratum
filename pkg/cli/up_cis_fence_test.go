package cli

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ctrliq/stratum/internal/cluster"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/maintlease"
	"github.com/ctrliq/stratum/internal/profile"
	"github.com/ctrliq/stratum/internal/version"
)

// cisStart is what a stubbed control-plane start saw: whether it reached the
// preparation that holds the CIS key gate, and the fence it was handed there.
type cisStart struct {
	configPath string
	prepared   bool
	lease      *k8sutil.MaintenanceLease
}

// cisColdStart lays out a control plane about to mint the CIS at-rest key on
// a restart: CIS selected, no key at version.EncryptionConfigPath, and an
// admin.conf naming server, every path under a fresh temporary tree. The
// host-mutating preparation and the supervisor are stubbed, the preparation
// recording what it was handed.
func cisColdStart(t *testing.T, server string) *cisStart {
	t.Helper()
	registerCISForTest(t)
	tmp := t.TempDir()
	kubeconfigDir := filepath.Join(tmp, "kubeconfig")
	for _, sw := range []struct {
		p *string
		v string
	}{
		{&version.DataDir, filepath.Join(tmp, "data")},
		{&version.EtcdDataDir, filepath.Join(tmp, "etcd")},
		{&version.K8sConfDir, filepath.Join(tmp, "config")},
		{&version.KubeconfigDir, kubeconfigDir},
		{&version.AdminConf, filepath.Join(kubeconfigDir, "admin.conf")},
		{&version.PKIDir, filepath.Join(tmp, "pki")},
		{&version.EncryptionConfigPath, filepath.Join(tmp, "pki", "encryption-config.yaml")},
		{&startLockPath, filepath.Join(tmp, "start.lock")},
	} {
		old := *sw.p
		*sw.p = sw.v
		t.Cleanup(func() { *sw.p = old })
	}
	if err := os.MkdirAll(kubeconfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const tpl = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
  name: stratum
contexts:
- context:
    cluster: stratum
    user: admin
  name: default
current-context: default
users:
- name: admin
  user:
    token: test
`
	if err := os.WriteFile(version.AdminConf, fmt.Appendf(nil, tpl, server), 0o600); err != nil {
		t.Fatal(err)
	}

	oldInstall, oldRun, oldPrepare, oldFence := installUnit, runControllerFn, prepareControllerFn, cisKeyMintFence
	t.Cleanup(func() {
		installUnit, runControllerFn, prepareControllerFn, cisKeyMintFence = oldInstall, oldRun, oldPrepare, oldFence
	})
	s := &cisStart{configPath: writeEmptyConfig(t, tmp)}
	installUnit = func([]string) error { return nil }
	runControllerFn = func(context.Context, *cluster.ClusterConfig, *cluster.NodeState) error { return nil }
	prepareControllerFn = func(_ context.Context, cfg *cluster.ClusterConfig, _ string, lease *k8sutil.MaintenanceLease, _ func()) (*cluster.NodeState, error) {
		if !cfg.Spec.CIS {
			t.Error("the start prepared a controller without CIS; the fixture selects it")
		}
		s.prepared, s.lease = true, lease
		return &cluster.NodeState{Role: "controller", Hostname: "cp1"}, nil
	}
	return s
}

// captureStderr runs fn with the process's stderr swapped for a pipe and
// returns what fn printed there; runUp prints its notices to os.Stderr
// itself, not to a writer a test could pass in.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		old := os.Stderr
		os.Stderr = w
		defer func() { os.Stderr = old; _ = w.Close() }()
		fn()
	}()
	return <-done
}

// TestRunUpCISStartWithNoAnsweringAPIServerReachesTheKeyGate: turning CIS on
// for a cluster's only control plane is a restart of a node whose apiserver
// only this start brings up, after a reboot or after a first --cis start that
// wrote admin.conf and failed before the key. Nothing answers through
// admin.conf then, so the fence cannot be taken, and nothing can mint a key,
// publish a bundle or admit a member through that apiserver either. The start
// goes on unfenced to the key gate, whose etcd read refuses a control plane
// with other members, and says on stderr that it skipped the fence and why.
func TestRunUpCISStartWithNoAnsweringAPIServerReachesTheKeyGate(t *testing.T) {
	s := cisColdStart(t, "https://127.0.0.1:1")
	var err error
	notice := captureStderr(t, func() {
		err = runUp(context.Background(), runUpOptions{configPath: s.configPath, cis: true})
	})
	if err != nil {
		t.Fatalf("runUp = %v, want the start to reach the key gate", err)
	}
	if !s.prepared {
		t.Fatal("runUp never reached the preparation that holds the key gate")
	}
	if s.lease != nil {
		t.Errorf("the preparation was handed fence %+v, want none: no apiserver answered to take it", s.lease)
	}
	if strings.Count(notice, "maintenance fence") != 1 || !strings.Contains(notice, "127.0.0.1:1") {
		t.Errorf("stderr = %q, want one notice that the maintenance fence was skipped, carrying the failed call", notice)
	}
}

// TestRunUpCISStartRefusesAFenceItCannotTake: an apiserver that answers
// decides the take. A lease another run holds live, or an answer refusing the
// lease call, stops the start before the key gate, with the take's own error,
// and nothing is skipped.
func TestRunUpCISStartRefusesAFenceItCannotTake(t *testing.T) {
	leases := coordinationv1.SchemeGroupVersion.WithResource("leases").GroupResource()
	for _, tc := range []struct {
		name string
		cs   func() kubernetes.Interface
		is   func(error) bool
	}{
		{"a live lease another run holds", func() kubernetes.Interface {
			holder, secs, renewed := "cp2/4242/1", int32(60), metav1.NewMicroTime(time.Now())
			return fake.NewClientset(&coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: "stratum-cluster-upgrade", Namespace: "kube-system"},
				Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &secs, RenewTime: &renewed},
			})
		}, func(err error) bool { return errors.Is(err, maintlease.ErrTaken) }},
		{"an apiserver that forbids the lease", func() kubernetes.Interface {
			cs := fake.NewClientset()
			cs.PrependReactor("get", "leases", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(leases, "stratum-cluster-upgrade", errors.New("RBAC: access denied"))
			})
			return cs
		}, apierrors.IsForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cisColdStart(t, "https://127.0.0.1:1")
			cs := tc.cs()
			cisKeyMintFence = func() (*k8sutil.MaintenanceLease, error) { return k8sutil.MaintenanceFence(cs) }
			var err error
			notice := captureStderr(t, func() {
				err = runUp(context.Background(), runUpOptions{configPath: s.configPath, cis: true})
			})
			if err == nil || !tc.is(err) {
				t.Fatalf("runUp = %v, want the take's refusal", err)
			}
			if s.prepared {
				t.Error("the start reached the key gate although the fence was refused")
			}
			if strings.Contains(notice, "maintenance fence") {
				t.Errorf("stderr = %q: a refused take is not a skipped one", notice)
			}
		})
	}
}

// TestRunUpCISStartSkipsTheFenceOnlyWhenNothingAnswers: the skip is for an
// apiserver that is not there, which is a failure to connect. Credentials that
// cannot be loaded, or an apiserver whose certificate admin.conf does not
// trust, are not that: something may be serving, so the start refuses with
// the take's own error rather than mint a key unfenced.
func TestRunUpCISStartSkipsTheFenceOnlyWhenNothingAnswers(t *testing.T) {
	const leaseURL = "https://10.0.0.10:6443/apis/coordination.k8s.io/v1/namespaces/kube-system/leases/stratum-cluster-upgrade"
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		skip  bool
	}{
		{"an admin.conf that cannot be loaded", func(t *testing.T) {
			if err := os.WriteFile(version.AdminConf, []byte("clusters: [not a kubeconfig\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"an apiserver whose certificate admin.conf does not trust", func(t *testing.T) {
			cisKeyMintFence = func() (*k8sutil.MaintenanceLease, error) {
				return nil, fmt.Errorf("read the lease kube-system/stratum-cluster-upgrade: %w",
					&url.Error{Op: "Get", URL: leaseURL, Err: x509.UnknownAuthorityError{}})
			}
		}, false},
		{"a dial that times out", func(t *testing.T) {
			cisKeyMintFence = func() (*k8sutil.MaintenanceLease, error) {
				return nil, fmt.Errorf("read the lease kube-system/stratum-cluster-upgrade: %w",
					&url.Error{Op: "Get", URL: leaseURL, Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}})
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cisColdStart(t, "https://127.0.0.1:1")
			tc.setup(t)
			var err error
			notice := captureStderr(t, func() {
				err = runUp(context.Background(), runUpOptions{configPath: s.configPath, cis: true})
			})
			if tc.skip {
				if err != nil || !s.prepared || !strings.Contains(notice, "maintenance fence") {
					t.Errorf("runUp = %v, prepared %v, stderr %q; want the fence skipped with a notice", err, s.prepared, notice)
				}
				return
			}
			if err == nil || s.prepared {
				t.Fatalf("runUp = %v, prepared %v; want the take's error before the key gate", err, s.prepared)
			}
			if strings.Contains(notice, "maintenance fence") {
				t.Errorf("stderr = %q: a refused take is not a skipped one", notice)
			}
		})
	}
}

// The fence covers every profile's shared secrets, not only the CIS key: a
// start whose profile declares a secret missing on disk is about to mint it,
// and one whose secrets are all there mints nothing and takes no fence.
func TestSharedSecretMintFenceFollowsTheSelectedProfile(t *testing.T) {
	cisColdStart(t, "https://127.0.0.1:1")
	registerProfilesForTest(t, profile.Profile{Name: "other",
		SharedSecrets: []profile.Secret{{Name: "a.yaml", Mint: func() ([]byte, error) { return []byte("a"), nil }}}})
	taken := 0
	cisKeyMintFence = func() (*k8sutil.MaintenanceLease, error) { taken++; return nil, nil }
	cfg := &cluster.ClusterConfig{}
	cfg.Spec.Profile = "other"
	if _, err := takeCISKeyMintFence(cfg); err != nil || taken != 1 {
		t.Fatalf("a missing a.yaml: fence taken %d times, err %v; want once", taken, err)
	}
	if err := os.MkdirAll(version.PKIDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(version.ProfileSecretFile("a.yaml"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := takeCISKeyMintFence(cfg); err != nil || taken != 1 {
		t.Fatalf("a.yaml on disk: fence taken %d times, err %v; want no new take", taken, err)
	}
}

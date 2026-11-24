package certs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509/pkix"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAnyLeafExpiringWithin(t *testing.T) {
	// The window arguments are arbitrary inputs, not the renewal default.
	ss := []CertStatus{
		{Name: "ca", DaysLeft: 3000, IsCA: true, Rotatable: false}, // CA near end never triggers leaf rotation
		{Name: "apiserver", DaysLeft: 300, Rotatable: true},
		{Name: "etcd", DaysLeft: 40, Rotatable: true},
	}
	if !anyLeafExpiringWithin(ss, 50) {
		t.Error("etcd at 40d should be within a 50d window")
	}
	if anyLeafExpiringWithin(ss, 30) {
		t.Error("nothing is within 30d")
	}
	// a CA that's expiring must NOT drive leaf rotation (it's not rotatable here)
	caOnly := []CertStatus{{Name: "ca", DaysLeft: 5, IsCA: true, Rotatable: false}}
	if anyLeafExpiringWithin(caOnly, 50) {
		t.Error("a non-rotatable CA must not trigger leaf rotation")
	}
}

func TestWarnDaysDefault(t *testing.T) {
	if got := warnDays(Options{}); got != version.CertRenewWindowDays {
		t.Errorf("default warnDays=%d, want version.CertRenewWindowDays=%d", got, version.CertRenewWindowDays)
	}
	if got := warnDays(Options{WarnDays: -1}); got != version.CertRenewWindowDays {
		t.Errorf("negative warnDays=%d, want the default version.CertRenewWindowDays=%d", got, version.CertRenewWindowDays)
	}
	if got := warnDays(Options{WarnDays: 10}); got != 10 {
		t.Errorf("override warnDays=%d, want 10", got)
	}
}

// Pins the one window every consumer must share, shorter than the leaf lifetime.
func TestRenewalWindowIsOneValue(t *testing.T) {
	got := warnDays(Options{})
	if got != version.CertRenewWindowDays {
		t.Errorf("warnDays(Options{}) = %d, want version.CertRenewWindowDays = %d: `certs check` and the renew timer must use the same window", got, version.CertRenewWindowDays)
	}
	window := time.Duration(got) * 24 * time.Hour
	if window >= pki.LeafValidity {
		t.Errorf("renewal window %v is not shorter than the leaf lifetime %v (pki.LeafValidity)", window, pki.LeafValidity)
	}
}

// Check reflects real leaf expiry (integration with pki via a temp PKIDir).
func TestCheckReportsLeavesAndCAs(t *testing.T) {
	prep(t)
	got, err := Check(Options{})
	if err != nil {
		t.Fatal(err)
	}
	var sawCA, sawLeaf bool
	for _, s := range got {
		if s.Name == "ca" {
			sawCA = true
			if s.Rotatable {
				t.Error("ca must be reported non-rotatable")
			}
		}
		if s.Name == "apiserver" {
			sawLeaf = true
			if !s.Rotatable {
				t.Error("apiserver must be rotatable")
			}
			if s.DaysLeft > 400 || s.DaysLeft < 300 {
				t.Errorf("apiserver DaysLeft=%d, want ~365", s.DaysLeft)
			}
		}
	}
	if !sawCA || !sawLeaf {
		t.Errorf("check missing entries: sawCA=%v sawLeaf=%v", sawCA, sawLeaf)
	}
	_ = time.Now
}

func TestCheckReportsTheKubeletClientCert(t *testing.T) {
	prep(t)
	find := func(ss []CertStatus) (CertStatus, bool) {
		for _, s := range ss {
			if s.Name == KubeletClientName {
				return s, true
			}
		}
		return CertStatus{}, false
	}

	got, err := Check(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := find(got); ok {
		t.Fatalf("no kubelet client cert on disk, yet check reports %+v", s)
	}

	ca := newTestCA(t, "ca")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(kubeletClientCertPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	for _, tc := range []struct {
		name                      string
		issued, left              time.Duration
		warnDays                  int
		wantDays                  int
		wantExpiring, wantOverdue bool
	}{
		{"inside default window before renewal is due", 330 * day, 40 * day, 0, 39, true, false},
		{"inside caller's larger window", 315 * day, 50 * day, 60, 49, true, false},
		{"past the kubelet's latest renewal point", 345 * day, 20 * day, 0, 19, true, true},
		{"expired within the last day", 365 * day, -time.Hour, 0, -1, true, true},
		{"expired", 400 * day, -2 * day, 0, -2, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			notAfter := now.Add(tc.left)
			certPEM := nodeClientCertValid(t, ca, key.Public(), pkix.Name{CommonName: "system:node:w1", Organization: []string{"system:nodes"}}, now.Add(-tc.issued), notAfter)
			if err := os.WriteFile(kubeletClientCertPath(), certPEM, 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := Check(Options{WarnDays: tc.warnDays})
			if err != nil {
				t.Fatal(err)
			}
			s, ok := find(got)
			if !ok {
				t.Fatalf("check does not report the kubelet client cert at %s", kubeletClientCertPath())
			}
			if !s.Kubelet || s.IsCA || s.Rotatable {
				t.Errorf("kubelet client row = %+v, want Kubelet and neither IsCA nor Rotatable", s)
			}
			if !s.NotAfter.Equal(notAfter.Truncate(time.Second)) {
				t.Errorf("NotAfter = %v, want %v", s.NotAfter, notAfter)
			}
			if s.DaysLeft < tc.wantDays-1 || s.DaysLeft > tc.wantDays+1 {
				t.Errorf("DaysLeft = %d, want ~%d", s.DaysLeft, tc.wantDays)
			}
			if tc.left < 0 && s.DaysLeft >= 0 {
				t.Errorf("expired kubelet client cert has DaysLeft = %d, want negative", s.DaysLeft)
			}
			if s.Expiring != tc.wantExpiring {
				t.Errorf("Expiring = %v, want %v", s.Expiring, tc.wantExpiring)
			}
			if s.RenewalOverdue != tc.wantOverdue {
				t.Errorf("RenewalOverdue = %v, want %v", s.RenewalOverdue, tc.wantOverdue)
			}
			if anyLeafExpiringWithin([]CertStatus{s}, 50) {
				t.Error("a kubelet client cert must not trigger leaf rotation, which cannot renew it")
			}
		})
	}
}

func TestRotateIgnoresUnreadableKubeletClientCert(t *testing.T) {
	check := kubeVIPNode(t)
	p := kubeletClientCertPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(Options{}); err == nil {
		t.Fatal("certs check must report the unreadable kubelet certificate")
	}
	// Fresh leaves are not due, so no control-plane restart is exercised.
	if _, err := RotateContext(context.Background(), io.Discard, Options{IfExpiringDays: 1, Quiet: true}); err != nil {
		t.Fatalf("unreadable kubelet certificate blocked managed leaf renewal: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(version.PKIDir, "kube-vip.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true}); err != nil {
		t.Fatalf("unreadable kubelet certificate blocked forced managed leaf renewal: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(version.PKIDir, "kube-vip.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("forced rotation did not reissue the managed kube-vip leaf")
	}
	check("certs rotate with unreadable kubelet cert", 1)
}

func TestCheckRefusesAnUnreadableKubeletClientCert(t *testing.T) {
	prep(t)
	if err := os.MkdirAll(filepath.Dir(kubeletClientCertPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		write func(path string) error
	}{
		{"malformed", func(p string) error { return os.WriteFile(p, []byte("not a certificate\n"), 0o600) }},
		{"dangling symlink", func(p string) error { return os.Symlink(p+".gone", p) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := kubeletClientCertPath()
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err := tc.write(p); err != nil {
				t.Fatal(err)
			}
			got, err := Check(Options{})
			if err == nil {
				t.Fatalf("check succeeded with %d rows; want an error naming %s", len(got), p)
			}
			if !strings.Contains(err.Error(), p) {
				t.Errorf("error %q does not name %s", err, p)
			}
		})
	}
}

func TestCheckRefusesADamagedManagedCert(t *testing.T) {
	for _, cert := range []string{"apiserver", "front-proxy-ca"} {
		for _, tc := range []struct {
			name  string
			write func(t *testing.T, path string) error
		}{
			{"bad PEM", func(t *testing.T, p string) error { return os.WriteFile(p, []byte("not a certificate\n"), 0o600) }},
			{"bad DER", func(t *testing.T, p string) error {
				return os.WriteFile(p, []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), 0o600)
			}},
			{"unreadable directory", func(t *testing.T, p string) error {
				if err := os.Remove(p); err != nil {
					return err
				}
				return os.Mkdir(p, 0o700) // works as root, so it is not skipped in CI
			}},
			{"mode 000", func(t *testing.T, p string) error {
				if os.Geteuid() == 0 {
					t.Skip("root reads anything")
				}
				return os.Chmod(p, 0)
			}},
		} {
			t.Run(cert+"/"+tc.name, func(t *testing.T) {
				prep(t)
				p := filepath.Join(version.PKIDir, cert+".crt")
				if err := tc.write(t, p); err != nil {
					t.Fatal(err)
				}
				rows, err := Check(Options{})
				if err == nil {
					t.Fatalf("check succeeded with %d rows; want an error naming %s", len(rows), p)
				}
				if rows != nil {
					t.Errorf("check returned %d rows beside the error; want nil", len(rows))
				}
				if !strings.Contains(err.Error(), p) {
					t.Errorf("error %q does not name %s", err, p)
				}
				if !strings.Contains(err.Error(), "cannot be read or parsed") {
					t.Errorf("error %q does not say the file cannot be read or parsed", err)
				}
				remedy := "; reissue it with `sudo /usr/local/bin/stratum certs rotate --yes`"
				// Rotation replaces the file by rename, which cannot replace a directory.
				moveAside := "; it is a directory: move it aside with `sudo mv " + shellQuote(p) + " " + shellQuote(p+".damaged") +
					"`, then reissue it with `sudo /usr/local/bin/stratum certs rotate --yes`"
				want := remedy
				if tc.name == "unreadable directory" {
					want = moveAside
				}
				if cert == "apiserver" && !strings.HasSuffix(err.Error(), want) {
					t.Errorf("error %q does not end with %q", err, want)
				}
				if cert == "front-proxy-ca" && (strings.Contains(err.Error(), remedy) || strings.Contains(err.Error(), "move it aside")) {
					t.Errorf("error %q offers a reissue remedy, but rotate cannot fix a CA", err)
				}
			})
		}
	}
}

func TestCheckRemedyOnlyWhenRotationRepairs(t *testing.T) {
	remedy := "; reissue it with `sudo /usr/local/bin/stratum certs rotate --yes`"
	damage := func(name string) string {
		t.Helper()
		p := filepath.Join(version.PKIDir, name+".crt")
		if err := os.WriteFile(p, []byte("not a certificate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	wantRemedy := func(t *testing.T, p string, want bool) {
		t.Helper()
		_, err := Check(Options{})
		if err == nil {
			t.Fatalf("check succeeded with a damaged %s; want an error", p)
		}
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not name %s", err, p)
		}
		if got := strings.Contains(err.Error(), remedy); got != want {
			t.Errorf("error %q offers the reissue remedy = %v; want %v", err, got, want)
		}
	}
	t.Run("root-signed leaf offers the remedy", func(t *testing.T) {
		prep(t)
		p := damage("apiserver")
		wantRemedy(t, p, true)
	})
	t.Run("damaged etcd leaf under an etcd CA gets no remedy", func(t *testing.T) {
		prep(t)
		if _, err := pki.EnsureCA(pki.EtcdCAName, "etcd-ca"); err != nil {
			t.Fatal(err)
		}
		p := damage("etcd")
		wantRemedy(t, p, false)
	})
	t.Run("damaged etcd leaf on a pre-migration node offers the remedy", func(t *testing.T) {
		prep(t)
		// GenerateAll lays down the etcd CA; strip it for the pre-migration state.
		for _, f := range []string{pki.EtcdCAName + ".crt", pki.EtcdCAName + ".key"} {
			if err := os.Remove(filepath.Join(version.PKIDir, f)); err != nil {
				t.Fatal(err)
			}
		}
		p := damage("etcd")
		wantRemedy(t, p, true)
	})
	t.Run("damaged apiserver-kubelet-client without split trust gets no remedy", func(t *testing.T) {
		prep(t)
		// GenerateAll sets a fresh node up split; strip it for the pre-split state.
		for _, f := range []string{pki.KubeletClientCAName + ".crt", pki.KubeletClientCAName + ".key"} {
			if err := os.Remove(filepath.Join(version.PKIDir, f)); err != nil {
				t.Fatal(err)
			}
		}
		if err := pki.WriteCABundle(pki.KubeletClientBundleBase, "ca"); err != nil {
			t.Fatal(err)
		}
		if split, err := pki.KubeletClientSplit(); err != nil || split {
			t.Fatalf("precondition: node is split=%v err=%v; want not split", split, err)
		}
		p := damage("apiserver-kubelet-client")
		wantRemedy(t, p, false)
	})
	t.Run("damaged apiserver-kubelet-client under split trust offers the remedy", func(t *testing.T) {
		prep(t)
		if _, err := pki.EnsureCA(pki.KubeletClientCAName, "kubelet-client-ca"); err != nil {
			t.Fatal(err)
		}
		if err := pki.WriteCABundle(pki.KubeletClientBundleBase, pki.KubeletClientCAName); err != nil {
			t.Fatal(err)
		}
		if split, err := pki.KubeletClientSplit(); err != nil || !split {
			t.Fatalf("precondition: node is split=%v err=%v; want split", split, err)
		}
		p := damage("apiserver-kubelet-client")
		wantRemedy(t, p, true)
	})
}

func TestCheckSkipsAnAbsentManagedCert(t *testing.T) {
	prep(t)
	for _, f := range []string{"apiserver.crt", "apiserver.key"} {
		if err := os.Remove(filepath.Join(version.PKIDir, f)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := Check(Options{})
	if err != nil {
		t.Fatalf("check failed with apiserver absent: %v", err)
	}
	for _, r := range rows {
		if r.Name == "apiserver" {
			t.Error("check reported an apiserver row for an absent certificate")
		}
	}
	if !slices.ContainsFunc(rows, func(r CertStatus) bool { return r.Name == "ca" }) {
		t.Error("check lost the ca row when apiserver was absent")
	}
}

func TestTimerRotationRefusesADamagedLeaf(t *testing.T) {
	check := kubeVIPNode(t)
	p := filepath.Join(version.PKIDir, "apiserver.crt")
	if err := os.WriteFile(p, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := RotateContext(context.Background(), io.Discard, Options{IfExpiringDays: 45, Quiet: true})
	if err == nil {
		t.Fatalf("timer rotation with a damaged leaf: rotated=%v, no error; want an error naming %s", rotated, p)
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error %q does not name %s", err, p)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("refused timer rotation rewrote the damaged leaf")
	}
	check("refused timer rotation", 1)
}

func TestForcedRotationRepairsADamagedLeaf(t *testing.T) {
	check := kubeVIPNode(t)
	if err := os.WriteFile(filepath.Join(version.PKIDir, "apiserver.crt"), []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true}); err != nil {
		t.Fatalf("forced rotation refused to repair a damaged leaf: %v", err)
	}
	if _, err := pki.CertNotAfter("apiserver"); err != nil {
		t.Fatalf("forced rotation left apiserver.crt unparsable: %v", err)
	}
	check("forced rotation with a damaged leaf", 1)
}

func TestForcedRotationNeedsADirectoryLeafMovedAside(t *testing.T) {
	check := kubeVIPNode(t)
	p := filepath.Join(version.PKIDir, "apiserver.crt")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true}); err == nil {
		t.Fatal("forced rotation replaced a directory leaf; the move-aside hint would be wrong")
	}
	if err := os.Rename(p, p+".damaged"); err != nil {
		t.Fatal(err)
	}
	if _, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true}); err != nil {
		t.Fatalf("forced rotation after moving the directory aside: %v", err)
	}
	if _, err := pki.CertNotAfter("apiserver"); err != nil {
		t.Fatalf("forced rotation left apiserver.crt unparsable: %v", err)
	}
	check("forced rotation after moving a directory leaf aside", 1)
}

func TestRotationPhaseTransitions(t *testing.T) {
	prep(t)
	if got := RotationPhase(); got[:4] != "idle" {
		t.Errorf("fresh cluster phase=%q, want idle", got)
	}
	if err := pki.RotateCAMaterial(false); err != nil {
		t.Fatal(err)
	}
	if got := RotationPhase(); got[:9] != "trust-new" {
		t.Errorf("after staging phase=%q, want trust-new", got)
	}
	if err := pki.PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := pki.PromoteSA(); err != nil {
		t.Fatal(err)
	}
	if got := RotationPhase(); got[:8] != "sign-new" {
		t.Errorf("after promote phase=%q, want sign-new (bundle still old‖new)", got)
	}
	if err := pki.FinalizeCA(); err != nil {
		t.Fatal(err)
	}
	if got := RotationPhase(); got[:4] != "idle" {
		t.Errorf("after finalize phase=%q, want idle", got)
	}
}

func TestRotationDetail(t *testing.T) {
	prep(t)
	if got := RotationDetail(); got != nil {
		t.Errorf("idle detail = %q, want none", got)
	}
	if err := os.Remove(filepath.Join(version.PKIDir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if err := pki.RotateCAMaterial(true); err != nil {
		t.Fatal(err)
	}
	want := []string{"forced: yes (rollback refused; finish it forward)", "signer: ca-new/sa/front-proxy-ca (staged, in place of a lost key)"}
	if got := RotationDetail(); !slices.Equal(got, want) {
		t.Errorf("forced trust-new detail = %q, want %q", got, want)
	}
	if err := pki.PromoteCA(); err != nil {
		t.Fatal(err)
	}
	if err := pki.PromoteSA(); err != nil {
		t.Fatal(err)
	}
	want = []string{"forced: yes (rollback refused; finish it forward)", "signer: ca/sa/front-proxy-ca (live)"}
	if got := RotationDetail(); !slices.Equal(got, want) {
		t.Errorf("forced sign-new detail = %q, want %q", got, want)
	}
	if err := pki.FinalizeCA(); err != nil {
		t.Fatal(err)
	}
	if got := RotationDetail(); got != nil {
		t.Errorf("finalized detail = %q, want none", got)
	}
}

func TestSplitCP(t *testing.T) {
	cps, workers := splitCP([]cnode{
		{Name: "cp1", IsCP: true}, {Name: "w1"}, {Name: "cp2", IsCP: true}, {Name: "w2"},
	})
	if len(cps) != 2 || len(workers) != 2 {
		t.Fatalf("splitCP=%d cps, %d workers; want 2/2", len(cps), len(workers))
	}
	if cps[0].Name != "cp1" || workers[0].Name != "w1" {
		t.Errorf("splitCP order wrong: cps[0]=%s workers[0]=%s", cps[0].Name, workers[0].Name)
	}
}

// readAll is os.ReadFile with the test's fatal, for a rotation's artifacts.
func readAll(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A rotation must stop at a step boundary so shutdown's join does not wait it out.
func TestRotateContextStopsAtAStepBoundary(t *testing.T) {
	// Joined inside each subtest, after kubeVIPNode has redirected the version dirs.

	t.Run("a context already cancelled stops before the first step", func(t *testing.T) {
		kubeVIPNode(t)
		leaf := filepath.Join(version.PKIDir, "kube-vip.crt")
		kv := version.KubeVipKubeconfig()
		beforeLeaf, beforeKV := readAll(t, leaf), readAll(t, kv)

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdout
		os.Stdout = w
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = RotateContext(ctx, io.Discard, Options{})
		os.Stdout = old
		w.Close()
		out, _ := io.ReadAll(r)

		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the stop wrapping context.Canceled", err)
		}
		if strings.Contains(string(out), "Reissue control-plane leaf certificates") {
			t.Error("the rotation started its first step although its context was already cancelled")
		}
		if after := readAll(t, leaf); !bytes.Equal(after, beforeLeaf) {
			t.Error("leaves were reissued although the context was already cancelled")
		}
		if after := readAll(t, kv); !bytes.Equal(after, beforeKV) {
			t.Error("kubeconfigs were rewritten although the context was already cancelled")
		}
	})

	t.Run("cancelled while the reissue is in flight", func(t *testing.T) {
		kubeVIPNode(t)
		leaf := filepath.Join(version.PKIDir, "kube-vip.crt")
		kv := version.KubeVipKubeconfig()
		beforeLeaf, beforeKV := readAll(t, leaf), readAll(t, kv)

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		lines := make(chan string, 64)
		go func() {
			sc := bufio.NewScanner(r)
			for sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		// The boundary prints its own step headers through the writer it is handed.
		seen := []string{}
		waitFor := func(want string) {
			t.Helper()
			deadline := time.After(10 * time.Second)
			for {
				select {
				case l, ok := <-lines:
					if !ok {
						t.Fatalf("the rotation exited before printing %q", want)
					}
					seen = append(seen, l)
					if strings.Contains(l, want) {
						return
					}
				case <-deadline:
					t.Fatalf("the rotation never printed %q", want)
				}
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := RotateContext(ctx, w, Options{}); done <- err }()

		// The reissue header means the RSA keygen step is in flight when the cancel lands.
		waitFor("Reissue control-plane leaf certificates")
		cancel()
		err = <-done
		w.Close()
		for l := range lines {
			seen = append(seen, l)
		}
		joined := strings.Join(seen, "\n")

		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the stop wrapping context.Canceled", err)
		}
		for _, later := range []string{"Rewrite kubeconfigs (endpoint preserved)", "Restart control-plane units"} {
			if strings.Contains(joined, later) {
				t.Errorf("the rotation started a later step after its context ended: %q was printed", later)
			}
		}
		// The safe point is the boundary after the in-flight step, not an abort inside it.
		if after := readAll(t, leaf); bytes.Equal(after, beforeLeaf) {
			t.Error("the reissue in flight was aborted inside the step; it must finish and stop at the boundary after it")
		}
		if after := readAll(t, kv); !bytes.Equal(after, beforeKV) {
			t.Error("the kubeconfig rewrite started although the context ended at the reissue's boundary")
		}
	})
}

func TestRotateContextRunsTheWholeSequenceWhileTheContextLives(t *testing.T) {
	check := kubeVIPNode(t)
	before := readAll(t, filepath.Join(version.PKIDir, "kube-vip.crt"))
	if _, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true}); err != nil {
		t.Fatalf("RotateContext: %v", err)
	}
	if after := readAll(t, filepath.Join(version.PKIDir, "kube-vip.crt")); bytes.Equal(after, before) {
		t.Error("the leaves were not reissued; the context threading broke the rotation itself")
	}
	check("rotate context", 1)
}

func TestSubjectDrift(t *testing.T) {
	kubeVIPNode(t)
	orig := subjectDrift
	t.Cleanup(func() { subjectDrift = orig })
	want := []string{"admin (CN=admin,O=system:masters)"}
	subjectDrift = func(host, nodeIP string) ([]string, error) {
		if nodeIP != "192.168.99.12" {
			t.Errorf("drift checked for %q, want the node's recorded address", nodeIP)
		}
		return want, nil
	}
	if got, err := SubjectDrift(); err != nil || !slices.Equal(got, want) {
		t.Errorf("SubjectDrift = %q, %v; want %q", got, err, want)
	}
}

func TestRotate_ReportsWhetherItRotated(t *testing.T) {
	t.Run("nothing within the window is reported, not only printed", func(t *testing.T) {
		prep(t)
		leaf := filepath.Join(version.PKIDir, "kube-vip.crt")
		before := readAll(t, leaf)

		rotated, err := RotateContext(context.Background(), io.Discard, Options{IfExpiringDays: 30, Quiet: true})
		if err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		if rotated {
			t.Error("Rotate reported a rotation although no leaf is within 30 days of expiry")
		}
		if after := readAll(t, leaf); !bytes.Equal(after, before) {
			t.Error("the no-op run reissued a leaf")
		}
	})

	t.Run("a completed rotation is reported", func(t *testing.T) {
		kubeVIPNode(t)
		leaf := filepath.Join(version.PKIDir, "kube-vip.crt")
		before := readAll(t, leaf)

		rotated, err := RotateContext(context.Background(), io.Discard, Options{Quiet: true})
		if err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		if !rotated {
			t.Error("Rotate reported no rotation although it reissued the leaves")
		}
		if after := readAll(t, leaf); bytes.Equal(after, before) {
			t.Error("the forced run did not reissue the leaves; the test proves nothing")
		}
	})
}

func TestRotateRefusesANegativeWindow(t *testing.T) {
	prep(t)
	leaf := filepath.Join(version.PKIDir, "kube-vip.crt")
	before := readAll(t, leaf)

	rotated, err := RotateContext(context.Background(), io.Discard, Options{IfExpiringDays: -1, Quiet: true})
	const want = "certs: rotation not started: the if-expiring window must be 0 (rotate now) or a number of days above 0, not -1"
	if err == nil || err.Error() != want {
		t.Fatalf("Rotate with a -1 day window: err = %v; want %q", err, want)
	}
	if rotated {
		t.Error("Rotate reported a rotation for a refused run")
	}
	if after := readAll(t, leaf); !bytes.Equal(after, before) {
		t.Error("the refused run reissued a leaf")
	}
}

// Every MCP-called operation reports progress through an io.Writer first, never os.Stdout.

// captureRealStdout captures the process's actual stdout while fn runs.
func captureRealStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(&buf, r) }()

	fn()

	os.Stdout = saved
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String()
}

func TestRotateContext_WritesItsProgressToTheWriter(t *testing.T) {
	kubeVIPNode(t)

	var buf bytes.Buffer
	var (
		rotated bool
		err     error
	)
	onStdout := captureRealStdout(t, func() {
		rotated, err = RotateContext(context.Background(), &buf, Options{})
	})
	if err != nil {
		t.Fatalf("RotateContext: %v", err)
	}
	if !rotated {
		t.Fatal("RotateContext reported no rotation; the test proves nothing")
	}
	for _, want := range []string{"Reissue control-plane leaf certificates", "certs: rotated"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the rotation's progress is missing %q; the writer it was handed has:\n%s", want, buf.String())
		}
	}
	if strings.Contains(onStdout, "Reissue control-plane leaf certificates") {
		t.Errorf("the rotation printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

func TestRotateCA_WritesItsProgressToTheWriter(t *testing.T) {
	base := t.TempDir()
	redirectVar(t, &version.K8sConfDir, filepath.Join(base, "kubernetes"))    // no manifests: a worker
	redirectVar(t, &version.KubeconfigDir, filepath.Join(base, "kubeconfig")) // no kubelet.kubeconfig
	redirectVar(t, &version.PKIDir, filepath.Join(base, "pki"))
	redirectVar(t, &version.NodeUpgradeLockPath, filepath.Join(base, "upgrade.lock"))

	var buf bytes.Buffer
	var err error
	onStdout := captureRealStdout(t, func() {
		err = RotateCA(&buf, CARotateOptions{Phase: PhaseTrustNew})
	})
	if err == nil || !strings.Contains(err.Error(), "read kubelet kubeconfig endpoint") {
		t.Fatalf("RotateCA = %v; the fixture worker must refuse at the kubelet kubeconfig endpoint", err)
	}
	if !strings.Contains(buf.String(), "Refresh CA trust bundle from cluster-info") {
		t.Errorf("the phase's first step never reached the writer it was handed; it has:\n%s", buf.String())
	}
	if strings.Contains(onStdout, "Refresh CA trust bundle") {
		t.Errorf("the phase printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

func TestClusterCARotation_WritesItsProgressToTheWriter(t *testing.T) {
	prep(t)
	redirectVar(t, &version.AdminConf, "missing-admin.conf")
	b := &recordingBuilder{cs: fake.NewSimpleClientset(thisCPNode(t)...)}

	var buf bytes.Buffer
	var err error
	onStdout := captureRealStdout(t, func() {
		err = RotateCAClusterContext(context.Background(), &buf, CARotateOptions{Quiet: true}, b.build)
	})
	if err == nil {
		t.Fatal("the fixture run should halt at the initiator's own phase; it reported success")
	}
	if strings.Contains(err.Error(), "load admin kubeconfig") {
		t.Fatalf("the run built a client from admin.conf instead of the injected builder: %v", err)
	}
	if !strings.Contains(buf.String(), "CA rotation across") {
		t.Errorf("the cluster rotation's step header never reached the writer it was handed; it has:\n%s", buf.String())
	}
	if strings.Contains(onStdout, "CA rotation across") {
		t.Errorf("the cluster rotation printed on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

func TestEtcdCAMigration_RunsOnTheWriterItIsHanded(t *testing.T) {
	prep(t)
	redirectVar(t, &version.AdminConf, "missing-admin.conf")
	redirectVar(t, &version.K8sConfDir, filepath.Join(t.TempDir(), "kubernetes"))
	b := &recordingBuilder{cs: fake.NewSimpleClientset(thisCPNode(t)...)}

	var buf bytes.Buffer
	var err error
	onStdout := captureRealStdout(t, func() {
		err = MigrateEtcdCAClusterContext(context.Background(), &buf, EtcdCAOptions{Quiet: true}, b.build)
	})
	if err == nil || !strings.Contains(err.Error(), "InternalIP") {
		t.Fatalf("the fixture run should halt at the node roster (the fixture control planes carry no InternalIP): %v", err)
	}
	if strings.Contains(err.Error(), "load admin kubeconfig") {
		t.Fatalf("the run built a client from admin.conf instead of the injected builder: %v", err)
	}
	if onStdout != "" {
		t.Errorf("the migration wrote on the process's real stdout; it must report through the writer it was handed:\n%s", onStdout)
	}
}

func TestSaveRenewalRecordReplacesTheRecord(t *testing.T) {
	version.K8sConfDir = t.TempDir()
	first := RenewalRecord{AttemptedAt: time.Now().Add(-time.Hour), Result: "etcd restart gate"}
	if err := SaveRenewalRecord(first); err != nil {
		t.Fatal(err)
	}
	recordPath := RenewalRecordPath()
	before := readAll(t, recordPath)
	reader := recordPath + ".reader"
	if err := os.Link(recordPath, reader); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(reader) })

	second := RenewalRecord{AttemptedAt: time.Now()}
	if err := SaveRenewalRecord(second); err != nil {
		t.Fatal(err)
	}

	got, err := LoadRenewalRecord()
	if err != nil {
		t.Fatal(err)
	}
	if got.Result != "" || !got.AttemptedAt.Equal(second.AttemptedAt) {
		t.Errorf("LoadRenewalRecord = %+v, want the second attempt with no result", got)
	}
	if after := readAll(t, reader); !bytes.Equal(after, before) {
		t.Error("the save rewrote the record in place; a reader holding the old file sees the new one")
	}
	if fi, err := os.Stat(recordPath); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("renewal.yaml perms = %o, want 600", fi.Mode().Perm())
	}
}

func TestSaveRenewalRecordLeavesNoStrayTempOnFailure(t *testing.T) {
	version.K8sConfDir = t.TempDir()
	if err := os.MkdirAll(RenewalRecordPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveRenewalRecord(RenewalRecord{AttemptedAt: time.Now()}); err == nil {
		t.Fatal("SaveRenewalRecord wrote over a directory")
	}
	entries, err := os.ReadDir(version.K8sConfDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "renewal.yaml" || !entries[0].IsDir() {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the state dir holds %v, want only the record — the failed save left a temp copy", names)
	}
}

func TestPodRestartOrderIsTheStaticPodNames(t *testing.T) {
	if !slices.Equal(podRestartOrder, staticpod.Names) {
		t.Errorf("podRestartOrder = %v, want staticpod.Names %v in start order", podRestartOrder, staticpod.Names)
	}
}

func TestRestartOrdersAreInitializedFromTheirOwners(t *testing.T) {
	t.Parallel() // host-free: parses only its own file
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "certs.go", nil, 0)
	if err != nil {
		t.Fatalf("parse certs.go: %v", err)
	}
	for _, tc := range []struct{ name, want string }{
		{"podRestartOrder", "staticpod.Names"},
		{"hostRestartOrder", "hostRestartOrderFrom(comps.Units)"},
	} {
		init := varInit(f, tc.name)
		if init == nil {
			t.Errorf("certs.go declares no %s; the rotation's restart order stays a declared value this test can pin", tc.name)
			continue
		}
		if got := types.ExprString(init); got != tc.want {
			t.Errorf("%s is initialized from %s, want %s: the restart order must come from its owner, not a hand-written copy that drifts when the owner's set changes", tc.name, got, tc.want)
		}
	}
}

// varInit returns the initializing expression of a package-level var, or nil.
func varInit(f *ast.File, name string) ast.Expr {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name == name && i < len(vs.Values) {
					return vs.Values[i]
				}
			}
		}
	}
	return nil
}

func TestHostRestartOrderIsDerivedFromTheUnits(t *testing.T) {
	if !slices.Equal(hostRestartOrder, hostRestartOrderFrom(comps.Units)) {
		t.Errorf("hostRestartOrder = %v, want the derivation applied to the roster: hostRestartOrderFrom(comps.Units) = %v", hostRestartOrder, hostRestartOrderFrom(comps.Units))
	}

	var want []string
	for _, u := range comps.Units {
		if u != "containerd" {
			want = append(want, u)
		}
	}
	got := hostRestartOrderFrom(comps.Units)
	if !slices.Equal(got, want) {
		t.Errorf("hostRestartOrderFrom(comps.Units) = %v, want %v (comps.Units without containerd, in Units order)", got, want)
	}
	if len(got) == 0 || got[0] != "kubelet" || got[len(got)-1] != "operator" {
		t.Errorf("hostRestartOrderFrom(comps.Units) = %v, want kubelet first and the operator last (it reads super-admin.conf)", got)
	}

	// A synthetic roster member must flow through the derivation.
	augmented := append(slices.Clone(comps.Units), "synthetic-probe")
	wantSynthetic := append(slices.Clone(want), "synthetic-probe")
	if got := hostRestartOrderFrom(augmented); !slices.Equal(got, wantSynthetic) {
		t.Errorf("hostRestartOrderFrom(%v) = %v, want %v — a unit added to comps.Units must reach the rotation's restart set in Units order", augmented, got, wantSynthetic)
	}
}

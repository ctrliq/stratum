package ha

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/netcfg/netdefault"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
)

// bundleHost points the PKI dir, node state dir and netconfig path at temp
// locations and writes the mandatory shared certs, so collectBundle can run
// without a cluster. The netconfig file itself is left for each test to write.
func bundleHost(t *testing.T) {
	t.Helper()
	origPKI, origConf, origNet := version.PKIDir, version.K8sConfDir, version.NetConfigPath
	version.PKIDir, version.K8sConfDir = t.TempDir(), t.TempDir()
	version.NetConfigPath = filepath.Join(version.PKIDir, "netconfig")
	t.Cleanup(func() { version.PKIDir, version.K8sConfDir, version.NetConfigPath = origPKI, origConf, origNet })
	for _, n := range sharedCerts {
		if err := os.WriteFile(filepath.Join(version.PKIDir, n), []byte("content of "+n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A control plane that lost netconfig and has not restarted still
// shares the cluster's domain, recovered from node state, so a joiner that sets
// no clusterDomain does not recover cluster.local and join on the wrong domain.
func TestCollectBundleSharesTheRecoveredDomainWhenNetConfigIsLost(t *testing.T) {
	bundleHost(t)
	if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", ClusterDomain: "fuzz.internal"}); err != nil {
		t.Fatal(err)
	}
	files, err := collectBundle(false)
	if err != nil {
		t.Fatal(err)
	}
	want := netdefault.PodCIDR + "\n" + netdefault.ServiceCIDR + "\nfuzz.internal\n"
	if got := string(files[netconfigEntry]); got != want {
		t.Errorf("bundle netconfig = %q, want %q", got, want)
	}
	// What the joiner's DownloadCerts writes is a netconfig it reads back as that domain.
	joiner := t.TempDir()
	if _, err := applyBundle(joiner, files); err != nil {
		t.Fatal(err)
	}
	version.NetConfigPath = filepath.Join(joiner, "netconfig")
	if c, err := netcfg.Load(); err != nil || c.ClusterDomain != "fuzz.internal" {
		t.Errorf("joiner Load = %q, %v; want fuzz.internal", c.ClusterDomain, err)
	}
}

// An existing netconfig is shared as written, and a legacy two-line one is shared
// as two lines, which is also what a default-domain Save writes.
func TestCollectBundleSharesTheExistingNetConfig(t *testing.T) {
	bundleHost(t)
	if err := netcfg.Save("10.42.0.0/16", "10.43.0.0/16", "fuzz.internal"); err != nil {
		t.Fatal(err)
	}
	files, err := collectBundle(false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(files[netconfigEntry]), "10.42.0.0/16\n10.43.0.0/16\nfuzz.internal\n"; got != want {
		t.Errorf("bundle netconfig = %q, want %q", got, want)
	}

	if err := os.WriteFile(version.NetConfigPath, []byte("10.42.0.0/16\n10.43.0.0/16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if files, err = collectBundle(false); err != nil {
		t.Fatal(err)
	}
	if got, want := string(files[netconfigEntry]), "10.42.0.0/16\n10.43.0.0/16\n"; got != want {
		t.Errorf("legacy bundle netconfig = %q, want %q", got, want)
	}
}

// A first-start-shaped host with neither a file nor node state still shares the
// defaults, as the bundle did before it was built from Load.
func TestCollectBundleSharesTheDefaultsWithNoRecordedState(t *testing.T) {
	bundleHost(t)
	files, err := collectBundle(false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(files[netconfigEntry]), netdefault.PodCIDR+"\n"+netdefault.ServiceCIDR+"\n"; got != want {
		t.Errorf("bundle netconfig = %q, want %q", got, want)
	}
}

// A netconfig that cannot be read or validated, or a node state that cannot be
// read, stops the upload with an error; it is never shared as the default.
func TestCollectBundleFailsInsteadOfDefaulting(t *testing.T) {
	t.Run("corrupt netconfig", func(t *testing.T) {
		bundleHost(t)
		if err := os.WriteFile(version.NetConfigPath, []byte("one line only\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		files, err := collectBundle(false)
		if err == nil || !strings.Contains(err.Error(), "network config") {
			t.Errorf("collectBundle = %v, %v; want an error naming the network config", files, err)
		}
	})
	t.Run("unreadable node state with netconfig lost", func(t *testing.T) {
		bundleHost(t)
		if err := os.WriteFile(nodestate.StatePath(), []byte("{not yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		if files, err := collectBundle(false); err == nil {
			t.Errorf("collectBundle = %v, nil error; want an error", files)
		}
	})
}

// A bad cluster domain in node state, with netconfig lost, is refused on
// the donor by Load, ResolveDomain and collectBundle, so nothing is uploaded for
// a joiner to fail on after it has downloaded the CA. A space and a newline
// would otherwise reach the netconfig line every joiner reads.
func TestCollectBundleRefusesABadDomainInNodeState(t *testing.T) {
	for name, domain := range map[string]string{"a space": "a b", "a newline": "a\nb"} {
		t.Run(name, func(t *testing.T) {
			bundleHost(t)
			if err := nodestate.SaveState(&nodestate.NodeState{Role: "controller", ClusterDomain: domain}); err != nil {
				t.Fatal(err)
			}
			if c, err := netcfg.Load(); err == nil {
				t.Errorf("Load = %+v, nil error; want a refusal of node-state domain %q", c, domain)
			}
			if got, err := netcfg.ResolveDomain(""); err == nil {
				t.Errorf("ResolveDomain = %q, nil error; want a refusal of node-state domain %q", got, domain)
			}
			files, err := collectBundle(false)
			if err == nil || files != nil {
				t.Errorf("collectBundle = %v, %v; want no bundle and an error for node-state domain %q", files, err, domain)
			}
			if _, statErr := os.Stat(version.NetConfigPath); statErr == nil {
				t.Error("a netconfig was written for a bad domain")
			}
		})
	}
}

// TestCollectBundleSharesTheRecordedRangesWhenNetConfigIsLost: a custom-range,
// custom-domain control plane that lost netconfig shares the ranges and domain
// its node state records, not the default ranges, with joining control planes.
func TestCollectBundleSharesTheRecordedRangesWhenNetConfigIsLost(t *testing.T) {
	bundleHost(t)
	if err := nodestate.SaveState(&nodestate.NodeState{
		Role: "controller", PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16", ClusterDomain: "fuzz.internal",
	}); err != nil {
		t.Fatal(err)
	}
	files, err := collectBundle(false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(files[netconfigEntry]), "10.42.0.0/16\n10.43.0.0/16\nfuzz.internal\n"; got != want {
		t.Errorf("bundle netconfig = %q, want %q", got, want)
	}
}

// TestCollectBundleRotatingLeavesOutALostOldKey: the CA rotation's re-upload
// goes ahead without an old signing key a forced rotation went ahead without,
// but only when the rotation is recorded as forced and that key's staged
// replacement is there. Any other missing
// entry, and every missing entry outside a rotation, still stops the upload.
func TestCollectBundleRotatingLeavesOutALostOldKey(t *testing.T) {
	want := map[string]string{
		"ca.key":             "ca-new.key",
		"front-proxy-ca.key": "front-proxy-ca-new.key",
		"sa.key":             "sa-new.key",
	}
	if !maps.Equal(stagedReplacements, want) {
		t.Fatalf("stagedReplacements = %v, want %v", stagedReplacements, want)
	}
	for old, staged := range want {
		t.Run(old, func(t *testing.T) {
			bundleHost(t)
			if err := os.Remove(filepath.Join(version.PKIDir, old)); err != nil {
				t.Fatal(err)
			}
			if _, err := collectBundle(true); err == nil {
				t.Error("collectBundle(true) went ahead with no staged replacement")
			}
			if err := os.WriteFile(filepath.Join(version.PKIDir, staged), []byte("staged"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := collectBundle(true); err == nil {
				t.Error("collectBundle(true) left out " + old + " for a rotation not recorded as forced")
			}
			markForced(t)
			if _, err := collectBundle(false); err == nil {
				t.Error("collectBundle(false) went ahead without " + old)
			}
			files, err := collectBundle(true)
			if err != nil {
				t.Fatalf("collectBundle(true): %v", err)
			}
			if _, ok := files[old]; ok {
				t.Errorf("the bundle carries %s", old)
			}
			if string(files[staged]) != "staged" {
				t.Errorf("the bundle does not carry %s", staged)
			}
		})
	}
	t.Run("a missing certificate", func(t *testing.T) {
		bundleHost(t)
		if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-new.key"), []byte("staged"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(version.PKIDir, "ca.crt")); err != nil {
			t.Fatal(err)
		}
		if _, err := collectBundle(true); err == nil {
			t.Error("collectBundle(true) went ahead without ca.crt")
		}
	})
}

// TestCollectBundleAfterAForcedRotationThatLostSAPub: a forced trust-new on a
// control plane that lost sa.pub but kept sa.key leaves material the rotation's
// re-upload can share, so the run does not halt at the upload on every resume.
func TestCollectBundleAfterAForcedRotationThatLostSAPub(t *testing.T) {
	bundleHost(t)
	for _, f := range []string{"ca.crt", "ca.key", "front-proxy-ca.crt", "front-proxy-ca.key", "sa.key", "sa.pub"} {
		if err := os.Remove(filepath.Join(version.PKIDir, f)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pki.NewCA("ca", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	if _, err := pki.NewCA("front-proxy-ca", "front-proxy-ca"); err != nil {
		t.Fatal(err)
	}
	if err := pki.NewSAKeypair("sa"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(version.PKIDir, "sa.pub")); err != nil {
		t.Fatal(err)
	}
	if err := pki.RotateCAMaterial(true); err != nil {
		t.Fatalf("RotateCAMaterial(true): %v", err)
	}
	if _, err := collectBundle(true); err != nil {
		t.Fatalf("collectBundle(true): %v", err)
	}
}

// TestRequireControlPlanePKIRotating: the staged root key stands in for a lost
// one only on a forced rotation's re-upload, never for `token create`.
func TestRequireControlPlanePKIRotating(t *testing.T) {
	bundleHost(t)
	if err := os.Remove(filepath.Join(version.PKIDir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if err := requireControlPlanePKI(true); err == nil {
		t.Error("requireControlPlanePKI(true) passed with no root key at all")
	}
	if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-new.key"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireControlPlanePKI(true); err == nil {
		t.Error("requireControlPlanePKI(true) passed for a rotation not recorded as forced")
	}
	markForced(t)
	if err := requireControlPlanePKI(true); err != nil {
		t.Errorf("requireControlPlanePKI(true) = %v with ca-new.key staged by a forced rotation", err)
	}
	if err := requireControlPlanePKI(false); err == nil {
		t.Error("requireControlPlanePKI(false) passed without ca.key")
	}
}

func markForced(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(version.PKIDir, "carotate-forced"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A downloaded bundle that carries ca-bundle.crt moves the kubelet's
// clientCAFile with it; one that does not leaves the kubelet's bundle alone.
// HA joins, follower rotation fetches and etcd-CA migration fetches all apply a
// bundle this way, and none of them rewrites the kubelet's bundle elsewhere.
func TestApplyDownloadedBundleSyncsTheKubeletClientBundle(t *testing.T) {
	t.Run("carries the kubelet's bundle from a split cluster", func(t *testing.T) {
		bundleHost(t)
		files := uploadSet(t)
		files["ca-bundle.crt"] = []byte("the cluster's root bundle\n")
		files[pki.KubeletClientBundleBase+"-bundle.crt"] = []byte("the kubelet-client CA\n")
		if _, err := applyDownloadedBundle(files); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(pki.KubeletClientBundlePath()); err != nil || string(got) != "the kubelet-client CA\n" {
			t.Errorf("kubelet clientCAFile = %q, %v; want the downloaded kubelet bundle, not the root", got, err)
		}
	})
	t.Run("carries ca-bundle.crt only", func(t *testing.T) {
		bundleHost(t)
		files := uploadSet(t)
		files["ca-bundle.crt"] = []byte("the cluster's root bundle\n")
		for f := range kubeletClientEntries {
			delete(files, f)
		}
		if err := os.WriteFile(pki.KubeletClientBundlePath(), []byte("stale\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := applyDownloadedBundle(files); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(pki.KubeletClientBundlePath()); err != nil || string(got) != "the cluster's root bundle\n" {
			t.Errorf("kubelet clientCAFile = %q, %v; want the downloaded ca-bundle.crt", got, err)
		}
	})
	t.Run("no ca-bundle.crt", func(t *testing.T) {
		bundleHost(t)
		files := uploadSet(t)
		delete(files, "ca-bundle.crt")
		delete(files, pki.KubeletClientBundleBase+"-bundle.crt")
		if err := os.WriteFile(pki.KubeletClientBundlePath(), []byte("kept\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := applyDownloadedBundle(files); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(pki.KubeletClientBundlePath()); err != nil || string(got) != "kept\n" {
			t.Errorf("kubelet clientCAFile = %q, %v; want it left alone", got, err)
		}
	})
}

// The kubelet client-CA entries are shared only once the kubelets trust a
// kubelet-client CA: split, or part way through the migration. A cluster that
// predates the split shares exactly what an earlier release did, so an older
// joiner, which refuses entries it does not know, can still join it.
func TestCollectBundleSharesTheKubeletClientCAOnlyOnceTheKubeletsTrustIt(t *testing.T) {
	for _, tc := range []struct {
		name         string
		kubeletTrust []string
		shared       bool
	}{
		{"predates the split", []string{"root"}, false},
		{"migrating", []string{pki.KubeletClientCAName, "root"}, true},
		{"split", []string{pki.KubeletClientCAName}, true},
	} {
		bundleHost(t)
		if _, err := pki.NewCA("root", "stratum-ca"); err != nil {
			t.Fatal(err)
		}
		if err := pki.WriteCABundle("ca", "root"); err != nil {
			t.Fatal(err)
		}
		if tc.kubeletTrust[0] == pki.KubeletClientCAName {
			for _, n := range []string{pki.KubeletClientCAName, pki.KubeletClientCAName + "-new"} {
				if _, err := pki.NewCA(n, "stratum-kubelet-client-ca"); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := pki.WriteCABundle(pki.KubeletClientBundleBase, tc.kubeletTrust...); err != nil {
			t.Fatal(err)
		}
		files, err := collectBundle(false)
		if err != nil {
			t.Fatal(err)
		}
		for f := range kubeletClientEntries {
			if _, ok := files[f]; ok != tc.shared {
				t.Errorf("%s: bundle carries %s = %v, want %v", tc.name, f, ok, tc.shared)
			}
		}
	}
}

func TestCollectBundleRequiresEverySplitKubeletClientEntry(t *testing.T) {
	for missing, required := range kubeletClientEntries {
		if !required {
			continue
		}
		t.Run(missing, func(t *testing.T) {
			bundleHost(t)
			if _, err := pki.NewCA("root", "stratum-ca"); err != nil {
				t.Fatal(err)
			}
			if err := pki.WriteCABundle("ca", "root"); err != nil {
				t.Fatal(err)
			}
			if _, err := pki.NewCA(pki.KubeletClientCAName, "stratum-kubelet-client-ca"); err != nil {
				t.Fatal(err)
			}
			if err := pki.WriteCABundle(pki.KubeletClientBundleBase, pki.KubeletClientCAName); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(version.PKIDir, missing)); err != nil {
				t.Fatal(err)
			}
			if _, err := collectBundle(false); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("collectBundle with missing %s = %v, want a refusal naming the file", missing, err)
			}
		})
	}
}

// Outside a rotation a split cluster has no staged kubelet-client-ca-new, and
// the bundle is collected without it.
func TestCollectBundleSharesASplitClusterOutsideARotation(t *testing.T) {
	bundleHost(t)
	if _, err := pki.NewCA("root", "stratum-ca"); err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteCABundle("ca", "root"); err != nil {
		t.Fatal(err)
	}
	if _, err := pki.NewCA(pki.KubeletClientCAName, "stratum-kubelet-client-ca"); err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteCABundle(pki.KubeletClientBundleBase, pki.KubeletClientCAName); err != nil {
		t.Fatal(err)
	}
	files, err := collectBundle(false)
	if err != nil {
		t.Fatal(err)
	}
	for f, required := range kubeletClientEntries {
		if _, ok := files[f]; ok != required {
			t.Errorf("bundle carries %s = %v, want %v", f, ok, required)
		}
	}
}

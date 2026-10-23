package certs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestCheckWorkerReissueConfig(t *testing.T) {
	for _, name := range []string{"valid", "split flags", "relative credentials", "legacy launch", "wrong cert-dir", "wrong kubeconfig", "duplicate flag", "different credential", "embedded credential", "missing context", "missing auth", "missing kubeconfig", "malformed kubeconfig", "expired bootstrap token", "no bootstrap kubeconfig"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			paths := trustPaths{kubeletKubeconfig: filepath.Join(dir, "kubelet.conf"), bootstrapKubeconfig: filepath.Join(dir, "bootstrap.conf")}
			certPath := filepath.Join(dir, "kubelet-client-current.pem")
			if err := os.WriteFile(certPath, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			config := clientcmdapi.NewConfig()
			config.CurrentContext = "worker"
			config.Contexts["worker"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "worker"}
			config.Clusters["cluster"] = &clientcmdapi.Cluster{Server: "https://127.0.0.1:6443"}
			auth := &clientcmdapi.AuthInfo{ClientCertificate: certPath, ClientKey: certPath}
			config.AuthInfos["worker"] = auth
			bootstrap := config.DeepCopy()
			bootstrap.AuthInfos["worker"] = &clientcmdapi.AuthInfo{Token: "test-token"}
			args := []string{"kubelet", "--cert-dir=" + dir, "--kubeconfig=" + paths.kubeletKubeconfig, "--bootstrap-kubeconfig=" + paths.bootstrapKubeconfig}
			wantErr := true
			switch name {
			case "valid":
				wantErr = false
			case "split flags":
				args = []string{"kubelet", "--cert-dir", dir, "--kubeconfig", paths.kubeletKubeconfig, "--bootstrap-kubeconfig", paths.bootstrapKubeconfig}
				wantErr = false
			case "relative credentials":
				auth.ClientCertificate, auth.ClientKey = filepath.Base(certPath), filepath.Base(certPath)
				wantErr = false
			case "legacy launch":
				args = []string{"kubelet", "--kubeconfig=" + paths.kubeletKubeconfig}
			case "wrong cert-dir":
				args[1] = "--cert-dir=/other"
			case "wrong kubeconfig":
				args[2] = "--kubeconfig=/other"
			case "duplicate flag":
				args = append(args, "--cert-dir="+dir)
			case "different credential":
				auth.ClientCertificate = filepath.Join(dir, "other.pem")
			case "embedded credential":
				auth.ClientCertificateData = []byte("old cert")
			case "missing context":
				config.CurrentContext = "absent"
			case "missing auth":
				delete(config.AuthInfos, "worker")
			case "expired bootstrap token", "no bootstrap kubeconfig":
				bootstrap.AuthInfos["worker"] = &clientcmdapi.AuthInfo{}
				wantErr = false
			}
			if name != "missing kubeconfig" {
				if err := clientcmd.WriteToFile(*config, paths.kubeletKubeconfig); err != nil {
					t.Fatal(err)
				}
			}
			if name == "malformed kubeconfig" {
				if err := os.WriteFile(paths.kubeletKubeconfig, []byte("[invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if name != "no bootstrap kubeconfig" {
				if err := clientcmd.WriteToFile(*bootstrap, paths.bootstrapKubeconfig); err != nil {
					t.Fatal(err)
				}
			}
			if err := checkWorkerReissueConfig(args, paths, certPath); (err != nil) != wantErr {
				t.Fatalf("check error=%v, wantErr=%v", err, wantErr)
			}
			if name != "no client cert on disk" {
				if data, err := os.ReadFile(certPath); err != nil || string(data) != "unchanged" {
					t.Fatalf("preflight modified certificate: %q, %v", data, err)
				}
			}
		})
	}
}

// TestCARotationGuard pins the guard: a direct `stratum certs rotate-ca
// --phase=trust-new` must refuse to stage a second, divergent CA on a node
// already mid-rotation (which would split the trust domain), while the follower
// CP path (fetchShared) stays exempt because it legitimately arrives with the
// initiator's ca-new already staged. Every refusal must name the recovery path.
func TestCARotationGuard(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fetchShared bool
		staged      bool
		bundleCAs   int
		wantErr     bool
	}{
		{"follower is exempt even when material is already staged", true, true, 2, false},
		{"an already-staged rotation is refused", false, true, 1, true},
		{"a >1-CA trust bundle is refused", false, false, 2, true},
		{"a clean node proceeds", false, false, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := caRotationGuard(tc.fetchShared, tc.staged, tc.bundleCAs, "cp1")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a refusal, got nil — a divergent CA could be staged")
				}
				if !strings.Contains(err.Error(), "--rollback") {
					t.Errorf("a refusal must point the operator at the recovery command: %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected refusal: %v", err)
			}
		})
	}
}

// TestCARotateFlags_Validate pins the CA-rotation argument rule where both
// front ends must reach it: a request that combines cluster-wide orchestration
// with a single phase is refused, because a one-phase request would otherwise
// be turned into a full rotation (or the reverse) by a flag-handling mistake.
// The table mirrors TestMigrateEtcdCAFlagValidation, which pins the same rule
// for the etcd-CA migration's flags.
func TestCARotateFlags_Validate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		f       CARotateFlags
		wantErr string // substring; "" means accepted
	}{
		{"cluster run", CARotateFlags{Cluster: true}, ""},
		{"single-node trust-new", CARotateFlags{Phase: PhaseTrustNew}, ""},
		{"single-node sign-new", CARotateFlags{Phase: PhaseSignNew}, ""},
		{"single-node finalize", CARotateFlags{Phase: PhaseFinalize}, ""},
		{"rollback", CARotateFlags{Rollback: true}, ""},
		{"single-node trust-new with fetch-shared", CARotateFlags{Phase: PhaseTrustNew, FetchShared: true}, ""},

		{"cluster and phase", CARotateFlags{Cluster: true, Phase: PhaseSignNew}, "not both"},
		{"fetch-shared with the cluster run", CARotateFlags{Cluster: true, FetchShared: true},
			"--fetch-shared only applies to --phase=trust-new"},
		{"fetch-shared on the wrong phase", CARotateFlags{Phase: PhaseSignNew, FetchShared: true},
			"--fetch-shared only applies to --phase=trust-new"},
		{"fetch-shared with rollback", CARotateFlags{Rollback: true, FetchShared: true},
			"--fetch-shared only applies to --phase=trust-new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.f.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want accepted, got: %v", err)
			case tc.wantErr == "":
				return
			case err == nil:
				t.Fatalf("want an error containing %q, got accepted", tc.wantErr)
			case !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestCARotateFlags_Options pins that the flags build the options the two
// entry points take, so a front end cannot assemble CARotateOptions by hand
// and drift from what the validator just accepted.
func TestCARotateFlags_Options(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    CARotateFlags
		want CARotateOptions
	}{
		{"cluster run", CARotateFlags{Cluster: true}, CARotateOptions{Cluster: true}},
		{"single-node phase with force", CARotateFlags{Phase: PhaseFinalize, Force: true}, CARotateOptions{Phase: PhaseFinalize, Force: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.f.Options(); got != tc.want {
				t.Errorf("%s: Options() = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

// TestCARotatePhases pins the exported CA-rotation phase list the front ends
// share: exactly the three phases RotateCA runs, in the only order they may
// run, spelled as literals so a rename or a dropped phase fails here rather
// than passing because the list and the assertion were built from the same
// constants.
func TestCARotatePhases(t *testing.T) {
	want := []string{"trust-new", "sign-new", "finalize"}
	if len(CARotatePhases) != len(want) {
		t.Fatalf("CARotatePhases = %v, want %v", CARotatePhases, want)
	}
	for i, phase := range want {
		if CARotatePhases[i] != phase {
			t.Errorf("CARotatePhases[%d] = %q, want %q", i, CARotatePhases[i], phase)
		}
	}
	if CARotatePhases[0] != PhaseTrustNew || CARotatePhases[1] != PhaseSignNew || CARotatePhases[2] != PhaseFinalize {
		t.Errorf("CARotatePhases = %v has drifted from the phases RotateCA runs (%s, %s, %s)",
			CARotatePhases, PhaseTrustNew, PhaseSignNew, PhaseFinalize)
	}
}

// TestRerunOfAForcedRotationWithoutForceNamesTheForcedResume: a rotation
// staged with --force can be neither resumed without the flag (the re-run
// would un-force it) nor rolled back (pki.RollbackCA refuses every forced
// rotation), so the refusal must name the forced resume instead of sending the
// operator to a --rollback that refuses in turn.
func TestRerunOfAForcedRotationWithoutForceNamesTheForcedResume(t *testing.T) {
	prepPinned(t)
	if err := pki.RotateCAMaterial(true); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"the --cluster entry check": checkClusterRotationState(CARotateOptions{}, "cp1"),
		"the trust-new re-run":      checkCARotationState(CARotateOptions{Phase: PhaseTrustNew}, "cp1"),
	} {
		if err == nil {
			t.Fatalf("%s let a forced rotation resume without --force, which would un-force it", name)
		}
		if !strings.Contains(err.Error(), "rotate-ca --cluster --force --yes") {
			t.Errorf("%s: %v, want it to name the forced resume", name, err)
		}
		if strings.Contains(err.Error(), "--rollback") {
			t.Errorf("%s: %v names --rollback, which refuses every forced rotation", name, err)
		}
	}
}

func TestCheckCARotationState(t *testing.T) {
	old := version.PKIDir
	t.Cleanup(func() { version.PKIDir = old })
	version.PKIDir = t.TempDir()
	if _, err := pki.NewCA("ca", "test-ca"); err != nil {
		t.Fatal(err)
	}
	cert, err := os.ReadFile(filepath.Join(version.PKIDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	badBlock := "-----BEGIN CERTIFICATE-----\n!!!\n-----END CERTIFICATE-----\n"
	for _, tc := range []struct {
		name, bundle                                         string
		missing, directory, force, follower, staged, wantErr bool
		forcedMarker                                         bool
	}{
		{name: "single CA", bundle: string(cert)},
		{name: "two CAs", bundle: string(cert) + string(cert), wantErr: true},
		{name: "force cannot bypass two CAs", bundle: string(cert) + string(cert), force: true, wantErr: true},
		{name: "missing", missing: true, wantErr: true},
		{name: "forced missing recovery", missing: true, force: true},
		{name: "read error", directory: true, wantErr: true},
		{name: "force cannot hide read error", directory: true, force: true, wantErr: true},
		{name: "empty", wantErr: true},
		{name: "garbage", bundle: "broken", wantErr: true},
		{name: "corrupt before valid", bundle: badBlock + string(cert), wantErr: true},
		{name: "corrupt after valid", bundle: string(cert) + badBlock, wantErr: true},
		{name: "invalid DER", bundle: "-----BEGIN CERTIFICATE-----\neA==\n-----END CERTIFICATE-----", wantErr: true},
		{name: "forced corrupt", bundle: "broken", force: true, wantErr: true},
		{name: "forced staged missing", missing: true, force: true, staged: true, wantErr: true},
		{name: "follower downloads shared state", missing: true, follower: true},
		{name: "a forced rotation resumes with --force", bundle: string(cert) + string(cert), force: true, staged: true, forcedMarker: true},
		{name: "a forced rotation does not resume without --force", bundle: string(cert) + string(cert), staged: true, forcedMarker: true, wantErr: true},
		{name: "--force does not resume a rotation staged without it", bundle: string(cert) + string(cert), force: true, staged: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version.PKIDir = t.TempDir()
			path := filepath.Join(version.PKIDir, "ca-bundle.crt")
			if tc.directory {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if !tc.missing {
				if err := os.WriteFile(path, []byte(tc.bundle), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.forcedMarker {
				if err := os.WriteFile(filepath.Join(version.PKIDir, "carotate-forced"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.staged {
				for _, ext := range []string{"crt", "key"} {
					if err := os.WriteFile(filepath.Join(version.PKIDir, "ca-new."+ext), cert, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			err := checkCARotationState(CARotateOptions{Force: tc.force, FetchShared: tc.follower}, "cp1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("guard error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.staged {
				if _, err := os.Stat(filepath.Join(version.PKIDir, "ca-new.crt")); !os.IsNotExist(err) {
					t.Fatalf("guard mutated staged material: %v", err)
				}
			}
		})
	}
}

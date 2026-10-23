package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/version"
)

// writeFakeBin drops an executable shell script at version.BinDir/name that
// exits with code, redirecting version.BinDir to a temp dir for the
// duration of the test (it's a var precisely so tests can do this).
func writeFakeBin(t *testing.T, name string, code int) {
	t.Helper()
	dir := t.TempDir()
	orig := version.BinDir
	version.BinDir = dir
	t.Cleanup(func() { version.BinDir = orig })

	script := "#!/bin/sh\nexit " + strconv.Itoa(code) + "\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestRunMulticallPropagatesExitCode pins runMulticall's contract: the
// kubectl/etcdctl passthrough returns *ErrExitCode with the child's exact
// code and never calls os.Exit itself — pkg/cli is a public, importable
// package, so an os.Exit would terminate an importer's whole process
// instead of returning an observable error the caller can see and handle.
func TestRunMulticallPropagatesExitCode(t *testing.T) {
	for _, code := range []int{1, 2, 137} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			writeFakeBin(t, "kubectl", code)
			err := runMulticall("kubectl", nil)
			var ec *ErrExitCode
			if !errors.As(err, &ec) {
				t.Fatalf("runMulticall = %v (%T), want *ErrExitCode", err, err)
			}
			if ec.Code != code {
				t.Errorf("ec.Code = %d, want %d", ec.Code, code)
			}
		})
	}
}

func TestRunMulticallSuccessReturnsNil(t *testing.T) {
	writeFakeBin(t, "kubectl", 0)
	if err := runMulticall("kubectl", nil); err != nil {
		t.Fatalf("runMulticall = %v, want nil", err)
	}
}

// writeEnvDumpBin drops an executable at version.BinDir/name that writes its
// own environment to a file, and returns that file's path. It is the sibling of
// writeFakeBin for the questions that are about the CHILD'S ENVIRONMENT rather
// than its exit code.
//
// The dump goes to a file rather than stdout on purpose. runMulticall assigns
// cmd.Stdout = os.Stdout inside the function, so capturing stdout means swapping
// the process's own os.Stdout for a pipe; the pattern used elsewhere in this repo
// for that (internal/upgrade/node_test.go) reads the pipe only after closing the
// write end, which deadlocks once the child writes more than the pipe buffer. An
// environment dump is comfortably under that today, which is exactly the kind of
// margin that disappears silently.
//
// The path is passed through the environment rather than argv because the
// commands under test are DisableFlagParsing passthroughs: anything in argv is
// the child's business, and a test that relies on argv would be pinning the
// wrong contract.
func writeEnvDumpBin(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	orig := version.BinDir
	version.BinDir = dir
	t.Cleanup(func() { version.BinDir = orig })

	out := filepath.Join(dir, "env.out")
	script := "#!/bin/sh\nenv > \"$STRATUM_TEST_ENV_OUT\"\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STRATUM_TEST_ENV_OUT", out)
	return out
}

// childEnv runs the command and returns the environment its child actually saw.
// exec.Cmd de-duplicates Env before spawning, so each key appears once and the
// map needs no last-wins handling of its own — that de-duplication is precisely
// the mechanism the precedence rule relies on.
func childEnv(t *testing.T, cmd *cobra.Command, outFile string) map[string]string {
	t.Helper()
	if err := cmd.RunE(nil, nil); err != nil {
		t.Fatalf("command returned %v", err)
	}
	b, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("the fake binary wrote no environment: %v", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env
}

// redirectPKI points version.PKIDir at a temp dir so the injected credential
// paths are deterministic and do not depend on the machine running the test.
func redirectPKI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := version.PKIDir
	version.PKIDir = dir
	t.Cleanup(func() { version.PKIDir = orig })
	return dir
}

// `stratum etcdctl` has to be usable as typed. etcd is rendered TLS-only with
// --client-cert-auth (internal/staticpod/args.go), while an unconfigured etcdctl
// opens a PLAINTEXT connection to 127.0.0.1:2379 — without the injected
// credentials every `stratum etcdctl` subcommand would fail on every node.
func TestEtcdctlCommandInjectsMTLSEnv(t *testing.T) {
	pki := redirectPKI(t)
	out := writeEnvDumpBin(t, "etcdctl")

	env := childEnv(t, newEtcdctlCommand(), out)

	// Not ETCDCTL_API: the bundled etcdctl is v3-only and prints
	// `unrecognized environment variable ETCDCTL_API=3` if it is set, which
	// would land in front of the operator on every invocation.
	if v, ok := env["ETCDCTL_API"]; ok {
		t.Errorf("ETCDCTL_API=%q was injected; etcdctl 3.x warns about it on every run", v)
	}
	for k, want := range map[string]string{
		"ETCDCTL_ENDPOINTS": fmt.Sprintf("https://127.0.0.1:%d", version.EtcdClientPort),
		"ETCDCTL_CERT":      pki + "/apiserver-etcd-client.crt",
		"ETCDCTL_KEY":       pki + "/apiserver-etcd-client.key",
	} {
		if got := env[k]; got != want {
			t.Errorf("child saw %s=%q, want %q", k, got, want)
		}
	}
	// CACERT is resolved through pki.EtcdTrustFile, whose preference order is
	// that package's business and is pinned by its own tests. All this asserts
	// is that a trust file was named at all.
	if env["ETCDCTL_CACERT"] == "" {
		t.Error("child saw no ETCDCTL_CACERT, so etcdctl cannot verify the server")
	}
}

// The operator is the authority. Injecting OVER an explicit ETCDCTL_ENDPOINTS
// would silently redirect `member remove <ID>` from the peer the operator named
// to the local node — removing the wrong member from the cluster. That is a
// worse defect than a missing default, so it gets its own test.
//
// This is also the case that distinguishes the two possible precedence
// schemes: under all-or-nothing deference the injected CACERT below would be
// absent, and the operator would have to supply all five variables to use one.
func TestEtcdctlCommandDefersToOperatorEnv(t *testing.T) {
	redirectPKI(t)
	out := writeEnvDumpBin(t, "etcdctl")
	t.Setenv("ETCDCTL_ENDPOINTS", "https://10.0.0.5:2379")

	env := childEnv(t, newEtcdctlCommand(), out)

	if got := env["ETCDCTL_ENDPOINTS"]; got != "https://10.0.0.5:2379" {
		t.Errorf("the operator's endpoint was overridden: got %q", got)
	}
	if env["ETCDCTL_CACERT"] == "" {
		t.Error("setting one variable should not withdraw the rest: no ETCDCTL_CACERT")
	}
}

// etcdctl refuses a flag and its environment variable together, so a
// default injected for a flag the operator also passed made the command fail
// ("conflicting environment variable"). The flag withdraws its own default —
// only that one; the mTLS credentials stay so the named member is reachable.
func TestEtcdctlDefaultsYieldToFlags(t *testing.T) {
	env := []string{
		"ETCDCTL_ENDPOINTS=https://127.0.0.1:2379",
		"ETCDCTL_CACERT=/pki/ca.crt",
		"ETCDCTL_CERT=/pki/c.crt",
		"ETCDCTL_KEY=/pki/c.key",
	}
	keys := func(kvs []string) string {
		var ks []string
		for _, kv := range kvs {
			k, _, _ := strings.Cut(kv, "=")
			ks = append(ks, strings.TrimPrefix(k, "ETCDCTL_"))
		}
		return strings.Join(ks, ",")
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no flags: every default", []string{"endpoint", "health"}, "ENDPOINTS,CACERT,CERT,KEY"},
		{"--endpoints=x", []string{"--endpoints=https://10.0.0.5:2379", "endpoint", "status"}, "CACERT,CERT,KEY"},
		{"--endpoints x (separate value)", []string{"--endpoints", "https://10.0.0.5:2379", "endpoint", "status"}, "CACERT,CERT,KEY"},
		{"flag after the subcommand", []string{"endpoint", "status", "--endpoints=https://10.0.0.5:2379"}, "CACERT,CERT,KEY"},
		{"all four flags", []string{"--endpoints=e", "--cacert=a", "--cert=c", "--key=k", "get", "x"}, ""},
		{"after --, it is data, not a flag", []string{"put", "--", "--endpoints=literal-value"}, "ENDPOINTS,CACERT,CERT,KEY"},
		{"an unrelated flag", []string{"--command-timeout=3s", "endpoint", "health"}, "ENDPOINTS,CACERT,CERT,KEY"},
		{"a flag that merely starts with a known name", []string{"--keyfile=x"}, "ENDPOINTS,CACERT,CERT,KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keys(etcdctlDefaults(env, tc.args)); got != tc.want {
				t.Errorf("kept %q, want %q", got, tc.want)
			}
		})
	}
}

// End to end through the real command: `stratum etcdctl --endpoints <peer>`
// reaches the child with the operator's endpoint on argv, no injected
// ETCDCTL_ENDPOINTS to conflict with it, and the credentials still injected.
func TestEtcdctlCommandEndpointsFlagDoesNotConflict(t *testing.T) {
	redirectPKI(t)
	out := writeEnvDumpBin(t, "etcdctl")
	if err := newEtcdctlCommand().RunE(nil, []string{"--endpoints=https://10.0.0.5:2379", "endpoint", "status"}); err != nil {
		t.Fatalf("command returned %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	env := string(b)
	if strings.Contains(env, "ETCDCTL_ENDPOINTS=") {
		t.Error("ETCDCTL_ENDPOINTS was injected alongside --endpoints; etcdctl refuses the pair")
	}
	for _, k := range []string{"ETCDCTL_CACERT=", "ETCDCTL_CERT=", "ETCDCTL_KEY="} {
		if !strings.Contains(env, k) {
			t.Errorf("%s was withdrawn too; the named member would be unreachable without it", strings.TrimSuffix(k, "="))
		}
	}
}

// On a control-plane node kubectl should just work.
func TestKubectlCommandInjectsKubeconfigWhenAdminConfExists(t *testing.T) {
	out := writeEnvDumpBin(t, "kubectl")
	conf := filepath.Join(t.TempDir(), "admin.conf")
	if err := os.WriteFile(conf, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := version.AdminConf
	version.AdminConf = conf
	t.Cleanup(func() { version.AdminConf = orig })

	if got := childEnv(t, newKubectlCommand(), out)["KUBECONFIG"]; got != conf {
		t.Errorf("child saw KUBECONFIG=%q, want %q", got, conf)
	}
}

// A WORKER has no admin.conf. Naming a file that does not exist is worse than
// naming nothing: kubectl stops at the missing file instead of falling through
// to its own resolution, so the operator loses the config they do have.
func TestKubectlCommandStaysPassthroughWithoutAdminConf(t *testing.T) {
	out := writeEnvDumpBin(t, "kubectl")
	orig := version.AdminConf
	version.AdminConf = filepath.Join(t.TempDir(), "absent.conf")
	t.Cleanup(func() { version.AdminConf = orig })
	t.Setenv("KUBECONFIG", "")
	os.Unsetenv("KUBECONFIG")

	if got, ok := childEnv(t, newKubectlCommand(), out)["KUBECONFIG"]; ok && got != "" {
		t.Errorf("KUBECONFIG=%q was injected for a node with no admin.conf", got)
	}
}

// Same authority rule as etcdctl.
func TestKubectlCommandDefersToOperatorKubeconfig(t *testing.T) {
	out := writeEnvDumpBin(t, "kubectl")
	conf := filepath.Join(t.TempDir(), "admin.conf")
	if err := os.WriteFile(conf, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := version.AdminConf
	version.AdminConf = conf
	t.Cleanup(func() { version.AdminConf = orig })
	t.Setenv("KUBECONFIG", "/home/someone/.kube/other-cluster")

	if got := childEnv(t, newKubectlCommand(), out)["KUBECONFIG"]; got != "/home/someone/.kube/other-cluster" {
		t.Errorf("the operator's KUBECONFIG was overridden: got %q", got)
	}
}

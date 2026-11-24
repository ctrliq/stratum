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

// pkg/cli is importable: a child failure must return *ErrExitCode, never os.Exit.
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

// the path rides in the env, not argv; the dump is a file, not stdout, to avoid a pipe deadlock.
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

// exec dedups Env last-wins, which is the mechanism the precedence rule relies on.
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

func redirectPKI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := version.PKIDir
	version.PKIDir = dir
	t.Cleanup(func() { version.PKIDir = orig })
	return dir
}

// etcd is rendered TLS-only, so without the injected mTLS env etcdctl cannot connect.
func TestEtcdctlCommandInjectsMTLSEnv(t *testing.T) {
	pki := redirectPKI(t)
	out := writeEnvDumpBin(t, "etcdctl")

	env := childEnv(t, newEtcdctlCommand(), out)

	// etcdctl 3.x warns about ETCDCTL_API on every run.
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
	// the trust file's exact name is pki's business, pinned by its own tests.
	if env["ETCDCTL_CACERT"] == "" {
		t.Error("child saw no ETCDCTL_CACERT, so etcdctl cannot verify the server")
	}
}

// injecting over the operator's endpoint could remove the wrong etcd member.
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

// etcdctl refuses a flag paired with its env var ("conflicting environment variable").
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

// naming a missing KUBECONFIG file is worse than none: kubectl stops at it.
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

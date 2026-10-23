package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/version"
)

func newKubectlCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "kubectl [args...]",
		Short:              "Run the embedded kubectl",
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMulticall("kubectl", args, kubeconfigDefault()...)
		},
	}
}

func newEtcdctlCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "etcdctl [args...]",
		Short:              "Run the embedded etcdctl",
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMulticall("etcdctl", args, etcdctlDefaults(k8sutil.EtcdctlEnv(), args)...)
		},
	}
}

// etcdctlEnvForFlag maps each etcdctl flag whose value stratum injects to the
// environment variable it injects it as.
var etcdctlEnvForFlag = map[string]string{
	"--endpoints": "ETCDCTL_ENDPOINTS",
	"--cacert":    "ETCDCTL_CACERT",
	"--cert":      "ETCDCTL_CERT",
	"--key":       "ETCDCTL_KEY",
}

// etcdctlDefaults drops every injected default whose flag the caller passed.
//
// The injected ETCDCTL_* variables are DEFAULTS (runMulticall: the caller's
// environment wins). The caller's flags have to win too, and for a
// sharper reason: etcdctl refuses a flag and its environment variable together
// ("conflicting environment variable ... is shadowed by corresponding
// command-line flag"), so `stratum etcdctl --endpoints https://peer:2379 ...`
// was unusable as typed — the natural way to ask one member for its own view of
// the cluster. Only the conflicting default is withdrawn: passing
// --endpoints keeps the injected mTLS credentials, which is what makes a single
// member reachable at all.
//
// Scanning stops at "--": what follows is data for the subcommand, not flags.
func etcdctlDefaults(env, args []string) []string {
	passed := map[string]bool{}
	for _, a := range args {
		if a == "--" {
			break
		}
		name, _, _ := strings.Cut(a, "=")
		if v, ok := etcdctlEnvForFlag[name]; ok {
			passed[v] = true
		}
	}
	if len(passed) == 0 {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !passed[k] {
			out = append(out, kv)
		}
	}
	return out
}

// kubeconfigDefault names admin.conf for the embedded kubectl, but only when
// that file is actually there. A worker node has no admin.conf, and pointing
// KUBECONFIG at a file that does not exist is worse than leaving it unset:
// kubectl stops at the missing file rather than falling through to its own
// resolution, so the operator loses the config they did have.
//
// Note what this does NOT preserve on a control-plane node. kubectl's fallback
// to ~/.kube/config lives inside kubectl, not in the environment, so it does
// not participate in the precedence rule below — naming admin.conf as a default
// suppresses it. That is the same choice k8sutil.Kubectl already makes for
// every internal caller, and `stratum kubectl` on a control-plane node should
// mean this cluster; an operator who wants another one sets KUBECONFIG, which
// still wins.
func kubeconfigDefault() []string {
	if _, err := os.Stat(version.AdminConf); err != nil {
		return nil
	}
	return []string{"KUBECONFIG=" + version.AdminConf}
}

// runMulticall execs a component binary (e.g. kubectl/etcdctl) with the
// caller's args + stdio. On a nonzero child exit it returns *ErrExitCode
// carrying the exact code, rather than calling os.Exit directly: pkg/cli is
// a public, importable package, so os.Exit here would terminate a host
// an embedding binary's whole process without letting it observe the failure,
// run cleanup, or apply its own exit policy. The child's stdio is inherited
// directly, so it has already told the operator whatever it needed to.
//
// env supplies DEFAULTS, not overrides: anything already set in the real
// environment wins. os/exec documents that where Cmd.Env holds duplicate keys
// "only the last value in the slice for each duplicate key is used", so putting
// env first and os.Environ() last is the whole of the rule — no key scanning.
//
// The direction matters more than it looks. Appended last instead, an injected
// ETCDCTL_ENDPOINTS would silently override an operator running
// `ETCDCTL_ENDPOINTS=https://peer:2379 stratum etcdctl member remove <ID>`,
// redirecting a quorum-changing command at the local node and removing the
// wrong member — a silent wrong answer in place of a loud failure.
func runMulticall(bin string, args []string, env ...string) error {
	path := filepath.Join(version.BinDir, bin)
	if _, err := os.Stat(path); err != nil {
		if bin == "etcdctl" {
			return fmt.Errorf("etcdctl not found at %s — it is not part of the embedded payload; "+
				"stage it there (from an etcd release tarball, for example)", path)
		}
		return fmt.Errorf("%s not found at %s (bring the node up first, or stage %s)", bin, path, bin)
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(append([]string(nil), env...), os.Environ()...)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &ErrExitCode{Code: ee.ExitCode()}
		}
		return fmt.Errorf("%s: %w", bin, err)
	}
	return nil
}

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

var etcdctlEnvForFlag = map[string]string{
	"--endpoints": "ETCDCTL_ENDPOINTS",
	"--cacert":    "ETCDCTL_CACERT",
	"--cert":      "ETCDCTL_CERT",
	"--key":       "ETCDCTL_KEY",
}

// etcdctl refuses a flag and its env var together; a passed flag withdraws only its default.
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

// kubectl stops at a missing KUBECONFIG rather than falling through, so name it only if it exists.
func kubeconfigDefault() []string {
	if _, err := os.Stat(version.AdminConf); err != nil {
		return nil
	}
	return []string{"KUBECONFIG=" + version.AdminConf}
}

// *ErrExitCode, not os.Exit: pkg/cli is importable. env entries are defaults; os.Environ() wins.
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

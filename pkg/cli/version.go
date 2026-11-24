package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/assets"
	"github.com/ctrliq/stratum/internal/comps"
	"github.com/ctrliq/stratum/internal/rtcheck"
	"github.com/ctrliq/stratum/internal/version"
)

func newVersionCommand(cfg rootConfig) *cobra.Command {
	var components, build bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the release version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// the cluster upgrade reads the build ID from the staged binary to pin every node
			if build {
				fmt.Println(version.BuildID)
				return nil
			}
			fmt.Printf("%s %s/%s\n", cfg.displayVersion, runtime.GOOS, runtime.GOARCH)
			if !components {
				return nil
			}
			// the WithComponentVersions override (deprecated) wins over the stamp, field by field
			for _, c := range [][2]string{
				{"kubernetes", strings.TrimPrefix(version.K8sVersion, "v")},
				{"containerd", cmp.Or(cfg.components.Containerd, version.ContainerdVersion, "unknown")},
				{"runc", cmp.Or(cfg.components.Runc, version.RuncVersion, "unknown")},
				{"etcd", strings.TrimPrefix(version.EtcdVersion, "v")},
				{"kube-vip", cmp.Or(cfg.components.KubeVip, version.KubeVipVersion, "unknown")},
				{"cilium", cmp.Or(cfg.components.Cilium, version.CiliumVersion, "unknown")},
				{"coredns", cmp.Or(cfg.components.CoreDNS, version.CorednsVersion, "unknown")},
			} {
				fmt.Printf("  %-14s %s\n", c[0], c[1])
			}
			fmt.Printf("  %-14s %s\n", "payload", describePayload())
			if w := runtimeMismatch(strings.TrimPrefix(version.ContainerdSock, "unix://"), filepath.Join(version.BinDir, "containerd")); w != "" {
				fmt.Println(w)
			}
			if w := supervisorMismatch(); w != "" {
				fmt.Println(w)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&components, "components", false, "also print the version of every embedded component")
	cmd.Flags().BoolVar(&build, "build", false, "print only this binary's build ID (the source it was built from)")
	_ = cmd.Flags().MarkHidden("build")
	return cmd
}

// a hint on a version listing, not a probe: silent when either side cannot be read
func runtimeMismatch(sock, bin string) string {
	if !sockLive(sock) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	installed, err := rtcheck.Installed(ctx, bin)
	if err != nil {
		return ""
	}
	serving, err := rtcheck.Serving(ctx, sock)
	if err != nil {
		return ""
	}
	return runtimeMismatchWarning(serving, installed, sock)
}

func runtimeMismatchWarning(serving, installed, sock string) string {
	if rtcheck.Same(serving, installed) {
		return ""
	}
	return fmt.Sprintf("WARN: containerd %s is installed but the runtime serving %s reports %s — another containerd, likely an orphan from an earlier run, holds the socket", installed, sock, serving)
}

// silent when no supervisor answers or it cannot be read (a root-only socket)
func supervisorMismatch() string {
	cur, err := runningSupervisor()
	if err != nil || !cur.Stale() {
		return ""
	}
	return supervisorMismatchWarning(cur)
}

func supervisorMismatchWarning(cur comps.RunningSupervisor) string {
	return fmt.Sprintf("WARN: the stratum supervisor (pid %d) runs %s, not the installed %s, so the processes it starts "+
		"get the previous build's arguments and paths until it restarts: restart `stratum up` on this node",
		cur.PID, cur.Exe, version.StratumInstallPath)
}

// indirected for tests
var runningSupervisor = comps.Supervisor

// answers without extracting the payload
func describePayload() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	names, err := assets.Names(exe)
	if errors.Is(err, assets.ErrNoPayload) {
		return "none — this build resolves components from the staging root or PATH"
	}
	if err != nil {
		return "unreadable: " + err.Error()
	}
	var bins, imgs []string
	for _, n := range names {
		if strings.HasPrefix(n, assets.BinDir+"/") {
			bins = append(bins, path.Base(n))
		} else {
			imgs = append(imgs, path.Base(n))
		}
	}
	return fmt.Sprintf("%d binaries + %d image archive(s) — airgap-complete (%s)",
		len(bins), len(imgs), strings.Join(bins, " "))
}

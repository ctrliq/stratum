package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/registry"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/version"
)

func newRegistryCommand() *cobra.Command {
	return newGroupCommand("registry", "Containerd registry mirror configuration",
		newRegistryApplyCommand(), newRegistryCheckCommand())
}

func newRegistryApplyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "apply",
		Short: "Apply registries.yaml as containerd certs.d mirrors",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := runtime.ApplyRegistry()
			if err != nil {
				return err
			}
			for _, w := range res.Warnings {
				fmt.Printf("WARN: %s\n", w)
			}
			if !res.Configured {
				fmt.Printf("%s: %s\n", version.RegistriesConfig, registry.NothingConfigured)
				return nil
			}
			fmt.Printf("applied registry configuration for: %s\n", strings.Join(res.Registries, ", "))
			if res.Restarted {
				fmt.Println("containerd restarted to enable certs.d (config_path)")
			}
			return nil
		},
	}
}

func newRegistryCheckCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Preview registry mirror configuration without applying it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := registry.Load(version.RegistriesConfig)
			if err != nil {
				return err
			}
			if registry.WildcardCredentialsDropped(c) {
				fmt.Printf("WARN: %s\n", registry.WildcardCredentialsIgnored)
			}
			regs := registry.Registries(c)
			if len(regs) == 0 {
				fmt.Printf("%s: %s\n", version.RegistriesConfig, registry.NothingConfigured)
				return nil
			}
			// Same predicate as apply, so check and apply cannot disagree on what is configured.
			printRows := func(cfg registry.RegConfig, name string) {
				if h := describeAuth(cfg.Auth); h != "" {
					fmt.Printf("    %s: auth=%s\n", name, h)
				}
				// Match the renderer: client cert/key render only as a pair.
				if cfg.TLS.CAFile != "" || cfg.TLS.CertFile != "" || cfg.TLS.KeyFile != "" || cfg.TLS.InsecureSkipVerify {
					fmt.Printf("    %s: tls ca=%q cert=%q key=%q skip_verify=%v\n", name, cfg.TLS.CAFile, cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.InsecureSkipVerify)
				}
			}
			for _, reg := range regs {
				m, mirrored := c.Mirrors[reg]
				if mirrored {
					fmt.Printf("%s -> %s\n", reg, strings.Join(m.Endpoint, ", "))
				} else {
					fmt.Printf("%s (direct)\n", reg)
				}
				if cfg, ok := c.Configs[reg]; ok {
					cfg, _ = registry.Effective(reg, cfg)
					printRows(cfg, reg)
				}
				if !mirrored {
					continue
				}
				for _, ep := range m.Endpoint {
					if registry.EndpointHost(ep) == reg {
						continue
					}
					cfg, ok := c.Configs[registry.EndpointHost(ep)]
					if !ok {
						continue
					}
					printRows(cfg, ep)
				}
				if len(m.Rewrite) > 0 {
					fmt.Printf("    WARN: rewrite rules present but not supported by upstream containerd certs.d\n")
				}
			}
			return nil
		},
	}
}

// Never prints the secret itself.
func describeAuth(a registry.Auth) string {
	switch {
	case a.IdentityToken != "":
		return "bearer(identitytoken)"
	case a.Auth != "":
		return "basic(auth)"
	case a.Username != "":
		return "basic(user " + a.Username + ")"
	}
	return ""
}

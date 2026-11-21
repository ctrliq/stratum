package cli

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/k8sutil"
	mcpsrv "github.com/ctrliq/stratum/internal/mcp"
	"github.com/ctrliq/stratum/internal/mcp/kube"
	"github.com/ctrliq/stratum/internal/version"
)

func newMCPCommand(cfg rootConfig) *cobra.Command {
	var (
		allowWrite       bool
		allowDestructive bool
		transport        string
		addr             string
		insecureListen   bool
		tokenFile        string
		kubeconfig       string
		kubeContext      string
		requestTimeout   time.Duration
	)

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve Stratum's tools over the Model Context Protocol",
		Long: "Serve Stratum's tools over the Model Context Protocol.\n\n" +
			"Read-only by default. Tools that change anything are not merely\n" +
			"refused without --allow-write or --allow-destructive: they are not\n" +
			"registered, so they never appear in the client's tool list.\n\n" +
			"resource_delete and resource_apply are the exceptions. They register\n" +
			"at the write tier, so they are listed with --allow-write, but refuse\n" +
			"some targets unless --allow-destructive was given as well:\n" +
			"resource_delete refuses namespaces, persistent volumes and claims,\n" +
			"nodes and custom resource definitions, and resource_apply refuses\n" +
			"creating or changing a namespace or a custom resource definition.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := mcpsrv.Options{
				Version:          cfg.displayVersion,
				AllowWrite:       allowWrite,
				AllowDestructive: allowDestructive,
				Transport:        transport,
				Addr:             addr,
				InsecureListen:   insecureListen,
				TokenFile:        tokenFile,
			}

			log := opts.NewLogger()
			opts.Logger = log

			// NewConnector contacts nothing, so the server boots against an absent cluster.
			conn, nodeConn := newConnectors(kubeconfig, kubeContext, requestTimeout, log)

			srv := mcpsrv.NewServer(opts)
			reg := newRegistrar(srv, allowWrite, allowDestructive)
			if err := registerAll(reg, cfg, conn, nodeConn); err != nil {
				return err
			}

			// What was actually admitted, not what was asked for.
			names := make([]string, 0, len(cfg.toolSets))
			for _, ts := range cfg.toolSets {
				if ts != nil {
					names = append(names, ts.Name())
				}
			}
			if kubeconfig == "" && os.Getenv("KUBECONFIG") == "" {
				log.Warn("no kubeconfig given; the generic Kubernetes tools may fall back to this node's "+
					"admin.conf, which is cluster-admin. Point --kubeconfig at a least-privilege "+
					"credential for anything but local troubleshooting",
					"fallback", version.AdminConf)
			}

			log.Info("MCP tools registered",
				"count", len(reg.Registered()), "tools", reg.Registered(), "sets", names)

			// cmd.Context() is Background for every stratum run; without this Ctrl-C kills mid-session.
			ctx, stop := interrupt.DerivedContext(cmd.Context())
			defer stop()

			return mcpsrv.Serve(ctx, srv, opts)
		},
	}

	f := cmd.Flags()
	f.BoolVar(&allowWrite, "allow-write", false,
		"register tools that mutate recoverably (snapshots, tokens, registry config)")
	f.BoolVar(&allowDestructive, "allow-destructive", false,
		"register tools that can take a cluster down or lose data")
	f.StringVar(&transport, "transport", "stdio", "stdio or http")
	f.StringVar(&addr, "addr", mcpsrv.DefaultAddr,
		"listen address for --transport=http; must be loopback unless --insecure-listen")
	f.BoolVar(&insecureListen, "insecure-listen", false,
		"permit a non-loopback --addr: the HTTP transport has no TLS, so only with something in front of it that terminates TLS")
	f.StringVar(&tokenFile, "token-file", "",
		"for --transport=http: file, mode 0600, holding the bearer token every request must carry (required)")
	f.StringVar(&kubeconfig, "kubeconfig", "",
		"explicit kubeconfig path (otherwise $KUBECONFIG, ~/.kube/config, this node's "+
			version.AdminConf+", then in-cluster)")
	f.StringVar(&kubeContext, "context", "",
		"kubeconfig context to use by default; tools may name another per call")
	f.DurationVar(&requestTimeout, "request-timeout", kube.DefaultTimeout,
		"per-request timeout for Kubernetes API calls")

	return cmd
}

// conn is operator-scoped; nodeConn stands on admin.conf, never the operator's env.
func newConnectors(kubeconfig, kubeContext string, requestTimeout time.Duration, log *slog.Logger) (conn, nodeConn *kube.Connector) {
	conn = kube.NewConnector(kube.Options{
		Kubeconfig: kubeconfig,
		Context:    kubeContext,
		Timeout:    requestTimeout,
		// No $KUBECONFIG or ~/.kube/config exists on a node; cluster-admin, hence Secrets refused.
		FallbackKubeconfig: version.AdminConf,
		Logger:             log,
	})
	nodeConn = kube.NewConnector(kube.Options{
		// The node-local tools' credential, never the operator's --kubeconfig.
		KubeconfigPath: k8sutil.InternalAdminConf,
		Timeout:        requestTimeout,
		Logger:         log,
	})
	return conn, nodeConn
}

// Both connectors are required, so a node-credential tool cannot ride the wrong credential.
func registerAll(r *Registrar, cfg rootConfig, conn, nodeConn *kube.Connector) error {
	r.nodeConn = nodeConn
	// Derived from the Registrar rather than passed in beside it.
	policy := mcpsrv.WritePolicy{AllowDestructive: r.Allows(TierDestructive)}
	for _, d := range mcpsrv.Catalog(cfg.displayVersion, conn, nodeConn, policy) {
		r.addCatalogTool(d)
	}
	for _, ts := range cfg.toolSets {
		if ts == nil {
			continue
		}
		ts.Register(r)
	}
	return r.err()
}

// catalogTier maps a catalog descriptor's tier onto the registrar's.
func catalogTier(t mcpsrv.Tier) (Tier, bool) {
	switch t {
	case mcpsrv.TierRead:
		return TierRead, true
	case mcpsrv.TierWrite:
		return TierWrite, true
	case mcpsrv.TierDestructive:
		return TierDestructive, true
	}
	return TierRead, false
}

// Built from the descriptor's tier, so a catalog row cannot contradict its annotations.
func (r *Registrar) addCatalogTool(d mcpsrv.Tool) {
	if r == nil {
		return
	}
	tier, ok := catalogTier(d.Tier)
	if !ok {
		r.mismatches = append(r.mismatches,
			fmt.Errorf("tool %q carries tier %s: want read, write or destructive", d.Name, d.Tier))
		return
	}
	if d.Add == nil {
		r.mismatches = append(r.mismatches,
			fmt.Errorf("tool %q carries no handler: the tool would register and never answer", d.Name))
		return
	}
	var t *mcp.Tool
	switch tier {
	case TierRead:
		t = ReadTool(d.Name, d.Description)
	case TierWrite:
		t = WriteTool(d.Name, d.Description)
		// The tier stays write: the handler refuses some targets without --allow-destructive.
		if d.Destructive {
			t = DestructiveTool(d.Name, d.Description)
		}
	case TierDestructive:
		t = DestructiveTool(d.Name, d.Description)
	}
	if err := checkTier(tier, t); err != nil {
		r.mismatches = append(r.mismatches, err)
		return
	}
	if !r.Allows(tier) {
		return
	}
	if r.byName[d.Name] {
		r.mismatches = append(r.mismatches,
			fmt.Errorf("tool %q is registered twice: the second registration would silently replace the first", d.Name))
		return
	}
	if r.byName == nil {
		r.byName = make(map[string]bool)
	}
	r.byName[d.Name] = true

	d.Add(r.srv, t)
	r.registered = append(r.registered, d.Name)
	// Every admitted tool must declare which Kubernetes credential it uses.
	if r.credentials == nil {
		r.credentials = make(map[string]string)
	}
	r.credentials[d.Name] = string(d.Credential)
}

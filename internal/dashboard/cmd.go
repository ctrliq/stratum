package dashboard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ctrliq/stratum/dash"
	"github.com/ctrliq/stratum/internal/interrupt"
	"github.com/ctrliq/stratum/internal/version"
)

// Command returns `stratum dash`.
//
// It is registered by cmd/stratum/main.go rather than inside
// pkg/cli.NewRootCommand, and that is not a style choice: pkg/cli is the one
// package a downstream build is permitted to import, so registering dash
// there would compile the dashboard server, the proxy and the embedded SPA
// into every embedding binary, which must not carry them.
func Command() *cobra.Command {
	var (
		listen        string
		kubeconfig    string
		publicURL     string
		tlsCert       string
		tlsKey        string
		nodeShell     bool
		sessionTTL    time.Duration
		allowInsecure bool
		sessionKey    string
	)
	cmd := &cobra.Command{
		Use:   "dash",
		Short: "Serve the Stratum dashboard for this cluster",
		Long: `stratum dash — serve the Stratum dashboard for this cluster.

The dashboard authenticates users by ServiceAccount bearer-token passthrough:
you paste a token, the cluster validates it, and every request the dashboard
makes on your behalf carries that token. What you can see and change is exactly
your own RBAC — the dashboard holds no identity of its own and needs no cluster
permissions.

Get a token for yourself with:

  kubectl create token <serviceaccount> -n <namespace>

By default the server listens on loopback with plain HTTP, which is the same
trust model as ` + "`kubectl proxy`" + `. To serve any other address, pass
--tls-cert and --tls-key: the session cookie carries a live cluster credential
and must not cross a network in the clear.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), options{
				Listen:        listen,
				Kubeconfig:    kubeconfig,
				PublicURL:     publicURL,
				TLSCert:       tlsCert,
				TLSKey:        tlsKey,
				NodeShell:     nodeShell,
				NodeShellSet:  cmd.Flags().Changed("enable-node-shell"),
				SessionTTL:    sessionTTL,
				AllowInsecure: allowInsecure,
				SessionKey:    sessionKey,
				Out:           cmd.OutOrStdout(),
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&listen, "listen", "127.0.0.1:8443", "address to listen on")
	f.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig to reach the apiserver (default: in-cluster, then $KUBECONFIG, ~/.kube/config, "+version.AdminConf+")")
	f.StringVar(&publicURL, "public-url", "", "the scheme://host[:port] users reach the dashboard on, when it sits behind a proxy or ingress")
	f.StringVar(&tlsCert, "tls-cert", "", "PEM certificate chain to serve TLS with")
	f.StringVar(&tlsKey, "tls-key", "", "PEM private key for --tls-cert")
	f.BoolVar(&nodeShell, "enable-node-shell", true,
		"allow the node shell: a privileged hostPID pod that nsenters into the host, i.e. a root shell on the node. The caller's own RBAC still has to permit it; --enable-node-shell=false withholds it")
	f.DurationVar(&sessionTTL, "session-ttl", defaultTTL, "how long a session lasts before the user signs in again")
	f.BoolVar(&allowInsecure, "insecure-http", false,
		"serve a non-loopback address over plain HTTP anyway, for a deployment that terminates TLS in front of this process. Requires --public-url, so the session cookie's Secure flag follows the scheme users actually arrive on")
	f.StringVar(&sessionKey, "session-key-file", "",
		"file holding the session secret (at least 32 bytes), so sessions survive a restart and are shared by replicas (default: a random in-memory secret)")
	return cmd
}

type options struct {
	Listen        string
	Kubeconfig    string
	PublicURL     string
	TLSCert       string
	TLSKey        string
	NodeShell     bool
	NodeShellSet  bool
	SessionTTL    time.Duration
	AllowInsecure bool
	SessionKey    string
	Out           io.Writer
}

// nodeShellImageOrErr resolves the image the node-shell debug pod runs, and
// fails closed when the feature is enabled but this build has no bundled image
// stamped. The image comes from version.NsenterImage (the mirrored,
// payload-carried busybox); an unstamped build leaves that at the ":unknown"
// sentinel. A public registry is no fallback either: containerd resolves an
// unqualified reference against docker.io, which the airgap payload does not
// carry, so refuse at startup. When the feature is off no image is served —
// the field stays empty so /api/v1/config never discloses the internal image
// name to a client that cannot use the feature anyway.
func nodeShellImageOrErr(enabled bool, image string) (string, error) {
	if !enabled {
		return "", nil
	}
	if !nodeShellImageStamped(image) {
		return "", fmt.Errorf("--enable-node-shell needs the bundled node-shell image, but this build has none stamped (%q). "+
			"Rebuild from a release so build/versions.mk stamps the image, or pass --enable-node-shell=false", image)
	}
	return image, nil
}

// nodeShellFallsBack reports whether the default-on node shell turns off for want of an image.
func nodeShellFallsBack(enabled, explicit bool, image string) bool {
	return enabled && !explicit && !nodeShellImageStamped(image)
}

func nodeShellImageStamped(image string) bool {
	return image != "" && !strings.HasSuffix(image, ":unknown")
}

func run(ctx context.Context, o options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	serveTLS := o.TLSCert != "" || o.TLSKey != ""
	if serveTLS && (o.TLSCert == "" || o.TLSKey == "") {
		return errors.New("--tls-cert and --tls-key must be given together")
	}
	// The session cookie carries a live bearer token, so plaintext is only
	// acceptable where nothing can observe it. Refusing up front beats
	// discovering it in a packet capture.
	if !serveTLS && !o.AllowInsecure && !loopbackOnly(o.Listen) {
		return fmt.Errorf("refusing to serve %s over plain HTTP: the session cookie carries a bearer token. "+
			"Pass --tls-cert/--tls-key, listen on loopback, or --insecure-http if TLS is terminated in front of this process", o.Listen)
	}

	// --insecure-http means something in front terminates TLS, so the browser
	// IS on HTTPS and the session cookie must be marked Secure — but this
	// process cannot know the scheme it was reached on unless it is told.
	// Without that, the cookie carrying a live bearer token goes out without
	// Secure, and the browser will then also send it over plain HTTP to the
	// same host. --public-url is what the origin check already wants, so it
	// is required here rather than adding a flag to say "yes, really".
	if o.AllowInsecure && !loopbackOnly(o.Listen) && o.PublicURL == "" {
		return errors.New("--insecure-http requires --public-url: the session cookie carries a bearer token, and its Secure flag " +
			"(and the CSRF origin check) have to follow the scheme users actually reach the dashboard on")
	}

	cfg, err := restConfig(o.Kubeconfig)
	if err != nil {
		return err
	}
	tgt, err := localTarget(cfg)
	if err != nil {
		return err
	}
	secret, err := loadSessionSecret(o.SessionKey)
	if err != nil {
		return fmt.Errorf("session secret: %w", err)
	}
	secureCookie := reachedOverHTTPS(serveTLS, o.PublicURL)
	sess, err := newSessions(secret, o.SessionTTL, secureCookie)
	if err != nil {
		return fmt.Errorf("session layer: %w", err)
	}

	log := slog.Default()
	// Reachable only by naming an http:// public URL, which is an explicit
	// statement that users arrive over plaintext. Refusing would block a
	// deployment an operator may have reasons for; saying nothing would hide
	// that a bearer token is travelling in a cookie any plaintext request to
	// the same host will also carry.
	if !secureCookie && !loopbackOnly(o.Listen) {
		log.Warn("the session cookie will NOT be marked Secure because --public-url is not https; "+
			"it carries a bearer token and the browser will send it over plain HTTP to this host",
			"publicURL", o.PublicURL, "listen", o.Listen)
	}
	origin, err := publicOrigin(o.PublicURL)
	if err != nil {
		return err
	}
	if nodeShellFallsBack(o.NodeShell, o.NodeShellSet, version.NsenterImage) {
		log.Warn("the node shell is on by default but this build has no node-shell image stamped, so it is off; "+
			"pass --enable-node-shell to make that an error instead", "image", version.NsenterImage)
		o.NodeShell = false
	}
	nodeShellImage, err := nodeShellImageOrErr(o.NodeShell, version.NsenterImage)
	if err != nil {
		return err
	}
	srv := &server{
		base:           cfg,
		sessions:       sess,
		proxy:          newProxy(tgt, o.NodeShell, log),
		spa:            newSPAHandler(dash.FS()),
		origin:         originPolicy{PublicOrigin: origin},
		nodeShell:      o.NodeShell,
		nodeShellImage: nodeShellImage,
		https:          secureCookie,
		version:        version.StratumVersion,
		log:            log,
		logins:         rate.NewLimiter(5, 10),
	}

	var certs *certReloader
	if serveTLS {
		if certs, err = newCertReloader(o.TLSCert, o.TLSKey, log); err != nil {
			return err
		}
	}

	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.Listen, err)
	}
	httpSrv := &http.Server{
		Handler: srv.routes(),
		// No WriteTimeout and no ReadTimeout: an exec stream and a log follow
		// are long-lived by design. ReadHeaderTimeout still bounds a client
		// that connects and dribbles headers.
		ReadHeaderTimeout: 20 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	scheme := "http"
	if serveTLS {
		scheme = "https"
	}
	fmt.Fprintf(o.Out, "stratum dash: serving %s://%s (apiserver %s, SPA embedded=%t, node shell=%t)\n",
		scheme, ln.Addr(), cfg.Host, dash.Embedded, o.NodeShell)
	if !dash.Embedded {
		fmt.Fprintln(o.Out, "stratum dash: built with -tags noui — the API and the Kubernetes proxy work, but there is no UI to serve")
	}

	ctx, stop := interrupt.DerivedContext(ctx)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		if serveTLS {
			httpSrv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.get}
			errc <- httpSrv.ServeTLS(ln, "", "")
			return
		}
		errc <- httpSrv.Serve(ln)
	}()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdown)
	}
}

// restConfig resolves how to reach the apiserver: in-cluster first (so the
// same command works as a Deployment), then the usual kubeconfig precedence
// with stratum's own admin.conf appended as a last resort, so running this on
// a control-plane node needs no flags.
func restConfig(explicit string) (*rest.Config, error) {
	if explicit == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if explicit != "" {
		rules.ExplicitPath = explicit
	} else {
		rules.Precedence = append(rules.Precedence, version.AdminConf)
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, nil
}

// loopbackOnly reports whether addr can only be reached from this host. An
// unspecified address (":8443", "0.0.0.0:8443") is not loopback, and neither
// is an empty host — that is the case this guard exists for.
func loopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost"
}

// reachedOverHTTPS reports whether the BROWSER arrives over TLS, which is not
// the same question as whether this process terminates it. It decides the
// session cookie's Secure flag and HSTS, and both have to follow the scheme
// users actually use:
//
//   - we serve TLS ourselves: yes, plainly.
//   - something in front terminates TLS: only --public-url can say so, which
//     is why --insecure-http requires it off loopback. Getting this wrong puts
//     a bearer token in a cookie without Secure, and the browser then also
//     sends it over plain HTTP to the same host.
//   - an http:// public URL: the operator has said users arrive over
//     plaintext, so no — and run() warns about it.
//   - the loopback default: no, and a Secure cookie would be dropped by some
//     browsers over http://127.0.0.1 anyway. Nothing observes the connection.
func reachedOverHTTPS(serveTLS bool, publicURL string) bool {
	if serveTLS {
		return true
	}
	_, scheme, ok := canonicalOrigin(publicURL)
	return ok && scheme == "https"
}

// publicOrigin reduces --public-url to the scheme://host[:port] the origin
// policy compares against, rejecting a value it could not use.
func publicOrigin(publicURL string) (string, error) {
	if publicURL == "" {
		return "", nil
	}
	c, _, ok := canonicalOrigin(publicURL)
	if !ok {
		return "", fmt.Errorf("--public-url %q is not an http(s) URL", publicURL)
	}
	return c, nil
}

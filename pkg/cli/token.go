package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ctrliq/stratum/internal/ha"
	"github.com/ctrliq/stratum/internal/k8sutil"
	"github.com/ctrliq/stratum/internal/netcfg"
	"github.com/ctrliq/stratum/internal/nodestate"
	"github.com/ctrliq/stratum/internal/token"
)

func newTokenCommand() *cobra.Command {
	return newGroupCommand("token", "Manage join tokens", newTokenCreateCommand(), newTokenListCommand())
}

// defaultServerURL returns server unchanged when --server names one, else the
// join URL a remote grant names, by netcfg.GrantServerURL: the cluster's VIP
// when this node records one, else this node's own address — the address in
// this node's apiserver cert, not the default route. GrantServerURL's doc
// lists every address it refuses rather than prints, because no remote joiner
// could dial it; its refusal here names --server, the operator's way to name
// another endpoint. The MCP token_create derives its URL by the same rule, so
// both front ends print the same join line, but it takes no URL from its
// caller, so only this front end adds the hint.
func defaultServerURL(server string) (string, error) {
	if server != "" {
		return server, nil
	}
	ip, err := nodestate.NodeIP()
	if err != nil {
		return "", err
	}
	url, err := netcfg.GrantServerURL(ip)
	if err != nil {
		return "", fmt.Errorf("%w. To name another endpoint, such as a load balancer, "+
			"run `stratum token create --server https://<endpoint>:6443`", err)
	}
	return url, nil
}

func newTokenCreateCommand() *cobra.Command {
	var server string
	var controlPlane bool
	ttl := token.DefaultTokenTTL
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint a join token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if ttl < 0 {
				return fmt.Errorf("--ttl must be >= 0 (0 mints a non-expiring token); got %s", ttl)
			}
			url, err := defaultServerURL(server)
			if err != nil {
				return err
			}
			// Before a token exists, so a failure here mints nothing to revoke.
			extra, err := workerJoinExtra()
			if err != nil {
				return err
			}
			joinExtra, err := profileJoinFlag()
			if err != nil {
				return err
			}
			return createToken(url, controlPlane, ttl, tokenCmd{
				// token.Create takes its client as a parameter: the CLI mints
				// through the admin client from this node's admin.conf, the
				// credential `stratum token create` has always used.
				create: func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (string, string, *metav1.OwnerReference, error) {
					cs, err := k8sutil.AdminClientset()
					if err != nil {
						return "", "", nil, err
					}
					return token.Create(w, cs, serverURL, ttl, controlPlane, confirm)
				},
				rekeyGuard:       rekeyGuard,
				maintenanceFence: k8sutil.AdminMaintenanceFence,
				newCertKey:       ha.NewCertKey,
				uploadCerts:      ha.UploadCerts,
				writeKeyFile:     writeHACertKeyFile,
				revoke:           token.RevokeOnFailure,
				out:              os.Stdout,
				workerExtra:      extra,
				joinExtra:        joinExtra,
			})
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&server, "server", "", "apiserver URL joiners will use (default: the cluster's VIP when one is configured, else this node's address)")
	fs.BoolVar(&controlPlane, "control-plane", false, "mint a control-plane join token instead: it can read the shared certs, so this uploads them and prints only the HA join command (mint a separate token for workers)")
	fs.DurationVar(&ttl, "ttl", token.DefaultTokenTTL, "token lifetime before it is auto-removed (e.g. 24h, 168h); 0 mints a NON-EXPIRING token (opt-in)")
	return cmd
}

// newTokenListCommand surfaces each live bootstrap token's remaining lifetime, so
// an operator can tell which join tokens are still valid and when they age out.
// Mirrors `stratum certs check`.
func newTokenListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List join tokens and their remaining lifetime",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			infos, err := token.List()
			if err != nil {
				return err
			}
			if len(infos) == 0 {
				fmt.Fprintln(os.Stdout, "No join tokens.")
				return nil
			}
			now := time.Now()
			fmt.Fprintf(os.Stdout, "%-10s %-22s %s\n", "TOKEN-ID", "EXPIRES", "REMAINING")
			for _, in := range infos {
				switch {
				case in.Malformed:
					fmt.Fprintf(os.Stdout, "%-10s %-22s %s\n", in.ID, "invalid", "treated as expired")
				case in.Expires == nil:
					fmt.Fprintf(os.Stdout, "%-10s %-22s %s\n", in.ID, "never", "-")
				case in.Expires.After(now):
					fmt.Fprintf(os.Stdout, "%-10s %-22s %s\n", in.ID,
						in.Expires.UTC().Format(time.RFC3339), remaining(in.Expires.Sub(now)))
				default:
					fmt.Fprintf(os.Stdout, "%-10s %-22s %s\n", in.ID,
						in.Expires.UTC().Format(time.RFC3339), "expired")
				}
			}
			return nil
		},
	}
}

// remaining renders a duration to a coarse, human "2h13m"-style string, floored at
// minute resolution — a join token's remaining life is never interesting to the second.
func remaining(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	return d.Truncate(time.Minute).String()
}

// tokenCmd is the effects `stratum token create` performs, injected so the
// credential-lifecycle guarantee is testable without a cluster.
type tokenCmd struct {
	create func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (tok, hash string, owner *metav1.OwnerReference, err error)
	// rekeyGuard is the one re-key entry point (certs.GuardRekey in
	// production, the same package var `certs rotate --rekey-shared`
	// consults): a control-plane mint re-keys the shared cert material, and a
	// worker mint republishes cluster-info from local PKI, so both must
	// refuse while a CA rotation or etcd-CA migration is staging the key the
	// flow's follower control planes decrypt stratum-certs with — a worker
	// mint from a controller that has not joined the active rotation would
	// replace the published old‖new trust with old-only. Injected for
	// testing, like every other effect here.
	rekeyGuard func() error
	// maintenanceFence takes the cluster-wide maintenance fence
	// (k8sutil.AdminMaintenanceFence in production: both maintenance leases,
	// via maintlease.AcquireCompatible) for the length of the mint — a
	// worker mint too: every mint republishes cluster-info and the kubelet
	// client CA from this node's local PKI (token.Create →
	// ensureClusterInfo), so a worker mint from a controller the rotation
	// has not reached would roll the published old‖new trust back to
	// old-only. It is the fence `stratum upgrade --cluster`, `rotate-ca
	// --cluster` and `migrate-etcd-ca` hold for their whole runs, so a mint
	// that lands mid-run refuses fast, naming the live holder and its
	// renewal age: re-keying the shared bundle under a running rotation
	// locks the rotation's follower control planes out of it, and
	// republishing trust under one strands joiners pinned to the staged CA.
	// Taken at this command-level seam, never inside internal/ha: the
	// orchestrators call those helpers mid-run and must keep working without
	// refusing against their own fence. Injected for testing, like every
	// other effect here.
	maintenanceFence func() (*k8sutil.MaintenanceLease, error)
	newCertKey       func() (string, error)
	uploadCerts      func(certKey string, owner *metav1.OwnerReference, confirm func(context.Context) error) error
	// writeKeyFile persists the shared-cert decryption key to a 0600 file and
	// returns its path, so the join command can reference the file instead of
	// printing the key to stdout. Injected for testing.
	writeKeyFile func(certKey string) (path string, err error)
	revoke       func(tok string, cause error) error
	out          io.Writer
	// workerExtra is appended to the worker join line after the CA pin: the
	// flags a worker needs to match this cluster's network: --pod-cidr and
	// --service-cidr, each only on a non-default range.
	// Empty on a default cluster, so the printed line is unchanged there.
	workerExtra string
	// joinExtra is appended to both join lines: the flags every joining node
	// needs to match this cluster's profile, today only --profile <name> on
	// a cluster that runs one. Without it a worker joins without the profile and its kubelet runs
	// without the CIS settings, seccompDefault among them.
	joinExtra string
}

// workerJoinExtra renders the flags a worker must be given to match this
// cluster's network, from the join flags token.LoadJoinFlags derives: this
// package keeps no network-range rule of its own, only the rendering, so the
// CLI and the MCP token_create tool cannot disagree about the flags a joining
// node needs.
//
// A missing netconfig is recovered from node state by the shared derivation,
// and any other error fails token creation instead of printing a line without
// --service-cidr: the join would succeed, and the worker's kubelet would then
// point pods at 10.96.0.10 on a cluster whose DNS is elsewhere, with nothing
// saying why.
func workerJoinExtra() (string, error) {
	flags, err := token.LoadJoinFlags()
	if err != nil {
		return "", err
	}
	var extra string
	// Keep the cluster's pod range in worker state for compatibility with
	// earlier builds. The current firewall binding is interface-scoped.
	if flags.PodCIDR != "" {
		extra += " --pod-cidr " + shellQuote(flags.PodCIDR)
	}
	if flags.ServiceCIDR != "" {
		extra += " --service-cidr " + shellQuote(flags.ServiceCIDR)
	}
	if flags.ClusterDomain != "" {
		extra += " --cluster-domain " + shellQuote(flags.ClusterDomain)
	}
	return extra, nil
}

// profileJoinFlag returns " --profile <name>" when token.LoadJoinFlags says
// this cluster runs a profile, for the join lines token create prints. A
// worker reads no cluster config, so the flag on its join line is the only
// way it learns the cluster's profile. A node state that cannot be read fails
// token creation instead of printing a line that silently joins a node
// without the profile.
var profileJoinFlag = func() (string, error) {
	flags, err := token.LoadJoinFlags()
	if err != nil {
		return "", err
	}
	if flags.Profile != "" {
		return " --profile " + flags.Profile, nil
	}
	return "", nil
}

// writeHACertKeyFile writes the shared-cert decryption key to a fresh 0600 file
// (os.CreateTemp is 0600 and unique) and returns its path. It runs only for
// `token create --control-plane`, which has to run on a control plane (it
// uploads that node's CA material; see ha.UploadCerts).
// The caller prints the path, not the key. On any failure it leaves no partial
// file behind.
func writeHACertKeyFile(certKey string) (string, error) {
	if strings.ContainsAny(os.TempDir(), "\r\n") {
		return "", fmt.Errorf("temporary directory must not contain line breaks (join commands use a single-line handoff)")
	}
	f, err := os.CreateTemp("", "stratum-ha-certificate-key-*")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(certKey + "\n"); err != nil {
		f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// tokenID returns the id half of a "<id>.<secret>" bootstrap token, for the
// kubectl-delete hint on a non-expiring token.
func tokenID(tok string) string {
	if i := strings.IndexByte(tok, '.'); i >= 0 {
		return tok[:i]
	}
	return tok
}

// shellQuote preserves one literal argument in POSIX shells, including quotes,
// whitespace, substitutions, and command separators. Never evaluate the value.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// createToken mints a join token and prints the join instructions, printing
// NOTHING until every step has succeeded.
//
// The mint → certificate phase → handover → rollback sequence is
// token.Issue's, the one owner both front ends route through: this command
// injects its effects (token.Create, ha.NewCertKey, ha.UploadCerts and
// token.RevokeOnFailure) and delivers the grant by
// printing the join instructions. Ownership follows the delivery Issue
// reports: while nothing has reached the operator the failing command's
// rollback revokes the minted token, and once the token itself has been
// printed the credential stays — revoking a token already on the terminal
// would replace a stranded credential with output that lies.
//
// The join flags are rendered before the mint (workerExtra and joinExtra,
// from token.LoadJoinFlags), so a cluster whose network configuration cannot
// be read fails before a token exists.
//
// ttl is the token lifetime (0 = never expires). On the --control-plane path
// the grant's certificate key is written to a 0600 file and only the PATH is
// printed; the upload that makes it usable runs before the handover, so the
// printed line never names a file nothing can decrypt.
func createToken(url string, controlPlane bool, ttl time.Duration, c tokenCmd) error {
	var lease *k8sutil.MaintenanceLease
	// Every mint republishes cluster trust from this node's local PKI
	// (token.Create → ensureClusterInfo), so the fence is not
	// control-plane-scoped: a worker mint from a controller the rotation has
	// not reached must refuse fast against a live maintenance run too.
	if c.maintenanceFence != nil {
		var err error
		lease, err = c.maintenanceFence()
		if err != nil {
			return err
		}
		defer lease.Release()
	}
	_, err := token.Issue(os.Stdout, token.IssueDeps{
		Confirm:     func() error { return lease.Confirm(context.Background()) },
		RekeyGuard:  c.rekeyGuard,
		Create:      c.create,
		NewCertKey:  c.newCertKey,
		UploadCerts: c.uploadCerts,
		Revoke:      c.revoke,
	}, url, ttl, controlPlane, token.JoinFlags{}, func(grant token.Grant) (bool, error) {
		return printJoinInstructions(grant, controlPlane, ttl, c)
	})
	return err
}

// printJoinInstructions renders the grant into the join instructions and
// writes them in one write. It reports whether the credential itself — the
// token — reached the writer, which is the delivery token.Issue holds
// ownership on.
//
// The print order is load-bearing, not an accident. Issue runs the
// certificate phase before the handover, so a failure in that phase exits
// non-zero without having handed the operator a live, non-expiring
// credential — token.Create's own rollback disarms when it returns, so
// nothing covers that window. The write below is the FIRST thing that
// leaves this process with the credential.
//
// Because errors here return through the handover callback instead of
// os.Exit-ing directly, every failure below must RETURN — the callback's
// reported delivery is the only signal that decides the rollback.
func printJoinInstructions(grant token.Grant, controlPlane bool, ttl time.Duration, c tokenCmd) (tokenHandedOver bool, err error) {
	tok, hash := grant.Token, grant.CACertHash
	var certKeyPath string
	if controlPlane {
		// Write the key to a 0600 file and hand over the PATH, never the key
		// itself, so the CA-decrypting certificate-key stays out of stdout, CI
		// logs and scrollback. Done before any handover so a failure
		// rolls the token back like every other pre-handover error.
		var werr error
		if certKeyPath, werr = c.writeKeyFile(grant.CertKey); werr != nil {
			return false, fmt.Errorf("write certificate-key file: %w", werr)
		}
	}
	// The key file exists only so the join line can name it. If the write
	// fails before the path itself reached the operator, the file holds a key
	// nobody knows about: remove it rather than leave it undisclosed.
	var keyHandedOver bool
	defer func() {
		if err != nil && certKeyPath != "" && !keyHandedOver {
			if cleanupErr := os.Remove(certKeyPath); cleanupErr != nil && !os.IsNotExist(cleanupErr) {
				err = errors.Join(err, fmt.Errorf("remove undisclosed certificate-key file %s: %w", certKeyPath, cleanupErr))
			}
		}
	}()

	// The verbs here are the ones the root command actually dispatches —
	// `up --worker` and `up --join`, per each command's usage text.
	// A control-plane token may read stratum-certs, so it is never offered as a
	// worker join.
	var b bytes.Buffer
	tokenEnd, keyEnd := 0, 0
	if !controlPlane {
		fmt.Fprintf(&b, "\nWorker join:\n\n  stratum up --worker --server %s --token %s", grant.ServerURL, tok)
		tokenEnd = b.Len()
		fmt.Fprintf(&b, " --discovery-token-ca-cert-hash %s%s%s\n", hash, c.workerExtra, c.joinExtra)
	} else {
		fmt.Fprintf(&b, "\nControl-plane (HA) join:\n\n  stratum up --join %s --token %s", grant.ServerURL, tok)
		tokenEnd = b.Len()
		fmt.Fprintf(&b, " --discovery-token-ca-cert-hash %s --certificate-key-file %s", hash, shellQuote(certKeyPath))
		// The key's path is handed over once it is written; what follows it on
		// the line is not part of that handover.
		keyEnd = b.Len()
		b.WriteString(c.joinExtra)
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "\nThe certificate-key (it decrypts the shared cluster CA material) was written to\n"+
			"  %s\n"+
			"with mode 0600, and is deliberately NOT printed here so it stays out of logs and scrollback.\n"+
			"Copy that file to the joining control-plane node (e.g. scp), pass it as --certificate-key-file\n"+
			"above, and delete it from both nodes once the node is Ready.\n", certKeyPath)
		fmt.Fprintf(&b, "\nThis token can read the shared cluster CA material, so use it only for a control-plane join.\n"+
			"For a worker, mint a separate token with `stratum token create`.\n")
	}
	if ttl <= 0 {
		fmt.Fprintf(&b, "\nThis token does NOT expire (--ttl 0). Delete it by hand once the join is done:\n"+
			"  kubectl -n kube-system delete secret %s\n", "bootstrap-token-"+tokenID(tok))
	} else {
		fmt.Fprintf(&b, "\nThis token expires in %s (tokencleaner removes it automatically). "+
			"See remaining tokens with `stratum token list`.\n", ttl)
	}

	// Printed for BOTH paths: the worker and the HA join line each carry --token,
	// so the safe-token guidance must not sit in a worker-only branch — otherwise
	// copying the printed HA command still leaks the token into ps/shell history.
	// Both mechanisms named here are wired in pkg/cli/up.go
	// (resolveToken); tokenEnvVar rather than a literal so this text cannot
	// outlive the variable it names.
	fmt.Fprintf(&b, "\n(to keep the token out of `ps`/shell history, pass --token-file <path>,\n"+
		"or export %s, instead of the --token flag)\n", tokenEnvVar)

	// Rendered once and written once, because the write IS the handover and a
	// discarded write error would make "the call returned" stand in for "the
	// operator has the credential". Reachable: `stratum token create` to a full
	// filesystem, a quota-exceeded home, or a dead NFS mount.
	n, werr := c.out.Write(b.Bytes())
	tokenHandedOver = n >= tokenEnd
	keyHandedOver = keyEnd > 0 && n >= keyEnd
	if werr == nil && n != b.Len() {
		werr = io.ErrShortWrite
	}
	switch {
	case werr == nil:
		return tokenHandedOver, nil
	case n == 0:
		// Nothing was handed over, so the owner's rollback is unambiguously right.
		return tokenHandedOver, fmt.Errorf("could not print the join command: %w", werr)
	default:
		if keyHandedOver {
			werr = fmt.Errorf("%w\nCertificate-key file retained at %s; securely copy it if needed and delete it after joining or abandoning this join", werr, certKeyPath)
		}
		if tokenHandedOver {
			werr = fmt.Errorf("%w\n\nWARNING: a live bootstrap token was handed over. "+
				"It stays valid until its expiration (or forever, if minted with --ttl 0); "+
				"find and remove it by hand if abandoning the join:\n\n"+
				"  kubectl -n kube-system get secrets --field-selector type=bootstrap.kubernetes.io/token\n", werr)
		}
		return tokenHandedOver, fmt.Errorf("the join command was only partially printed (%d of %d bytes): %w", n, b.Len(), werr)
	}
}

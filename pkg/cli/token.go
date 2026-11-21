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

func remaining(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	return d.Truncate(time.Minute).String()
}

// tokenCmd holds the effects of `stratum token create`, injected for testability.
type tokenCmd struct {
	create           func(w io.Writer, serverURL string, ttl time.Duration, controlPlane bool, confirm func(context.Context) error) (tok, hash string, owner *metav1.OwnerReference, err error)
	rekeyGuard       func() error
	maintenanceFence func() (*k8sutil.MaintenanceLease, error)
	newCertKey       func() (string, error)
	uploadCerts      func(certKey string, owner *metav1.OwnerReference, confirm func(context.Context) error) error
	writeKeyFile     func(certKey string) (path string, err error)
	revoke           func(tok string, cause error) error
	out              io.Writer
	workerExtra      string
	joinExtra        string
}

func workerJoinExtra() (string, error) {
	flags, err := token.LoadJoinFlags()
	if err != nil {
		return "", err
	}
	var extra string
	// The pod range stays in worker state for compatibility with earlier builds.
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

func tokenID(tok string) string {
	if i := strings.IndexByte(tok, '.'); i >= 0 {
		return tok[:i]
	}
	return tok
}

// shellQuote keeps value one literal argument in POSIX shells. Never evaluate it.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func createToken(url string, controlPlane bool, ttl time.Duration, c tokenCmd) error {
	var lease *k8sutil.MaintenanceLease
	// Every mint republishes cluster trust, so a worker mint must refuse mid-rotation too.
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

func printJoinInstructions(grant token.Grant, controlPlane bool, ttl time.Duration, c tokenCmd) (tokenHandedOver bool, err error) {
	tok, hash := grant.Token, grant.CACertHash
	var certKeyPath string
	if controlPlane {
		var werr error
		if certKeyPath, werr = c.writeKeyFile(grant.CertKey); werr != nil {
			return false, fmt.Errorf("write certificate-key file: %w", werr)
		}
	}
	// A key file whose path never reached the operator would be undisclosed: remove it.
	var keyHandedOver bool
	defer func() {
		if err != nil && certKeyPath != "" && !keyHandedOver {
			if cleanupErr := os.Remove(certKeyPath); cleanupErr != nil && !os.IsNotExist(cleanupErr) {
				err = errors.Join(err, fmt.Errorf("remove undisclosed certificate-key file %s: %w", certKeyPath, cleanupErr))
			}
		}
	}()

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

	// Printed on both paths: each join line carries --token, not only the worker's.
	fmt.Fprintf(&b, "\n(to keep the token out of `ps`/shell history, pass --token-file <path>,\n"+
		"or export %s, instead of the --token flag)\n", tokenEnvVar)

	// One write: the write is the credential handover, so a partial write must be reported.
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

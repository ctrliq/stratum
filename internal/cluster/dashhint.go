package cluster

import (
	"net"
	"strconv"

	"github.com/ctrliq/stratum/internal/version"
)

// Names this node's address, not the VIP: every node answers the NodePort.
func dashboardHint(cfg *ClusterConfig, nodeIP string) []string {
	if !cfg.DashboardEnabled() {
		return nil
	}
	s := cfg.DashboardSettings()
	if s.NodePort == 0 && s.PublicURL == "" {
		return []string{
			"dashboard: ClusterIP only; forward it to your machine with " +
				"`sudo " + version.StratumInstallPath + " kubectl -n stratum-dashboard port-forward svc/stratum-dashboard 8443:443` " +
				"and open https://localhost:8443",
			signInHint(s.TokenSecrets),
		}
	}
	addr := s.PublicURL
	if addr == "" {
		host := nodeIP
		if host == "" {
			host = "<node address>"
		}
		addr = "https://" + net.JoinHostPort(host, strconv.Itoa(s.NodePort)) + " (any node's address works)"
	}
	return []string{"dashboard: " + addr, signInHint(s.TokenSecrets)}
}

func signInHint(tokenSecrets bool) string {
	if tokenSecrets {
		return "dashboard sign-in token: sudo " + version.StratumInstallPath + " kubectl -n stratum-dashboard get secret " +
			"stratum-dashboard-admin-token -o jsonpath='{.data.token}' | base64 -d; echo"
	}
	return "dashboard sign-in: use a ServiceAccount token, e.g. " +
		"`sudo " + version.StratumInstallPath + " kubectl create token <serviceaccount> -n <namespace> --duration=8h`"
}

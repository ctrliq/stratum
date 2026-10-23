package addons

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

//go:embed manifests/dashboard.yaml
var dashboardYAML []byte

//go:embed manifests/dashboard-tokens.yaml
var dashboardTokensYAML []byte

// The in-cluster dashboard's fixed names, shared with the operator that keeps
// its Secrets.
const (
	DashboardStack         = "dashboard"
	DashboardNamespace     = "stratum-dashboard"
	DashboardName          = "stratum-dashboard"
	DashboardTLSSecret     = "stratum-dashboard-tls"
	DashboardSessionSecret = "stratum-dashboard-session"
	DashboardCASecret      = "stratum-dashboard-ca"
	DashboardHostsKey      = "stratum.ciq.com/serving-hosts"
	DefaultDashboardPort   = 30443
)

// DashboardTokenSecrets maps each sign-in token Secret the stack carries to its
// ServiceAccount.
var DashboardTokenSecrets = map[string]string{
	"stratum-dashboard-admin-token":  "stratum-dashboard-admin",
	"stratum-dashboard-viewer-token": "stratum-dashboard-viewer",
}

// DashImage is the dashboard image the payload carries, stamped at link time.
var DashImage = "unknown"

// DashboardSettings is spec.dashboard as the render takes it.
type DashboardSettings struct {
	NodePort      int
	PublicURL     string
	TLSSecretName string
	NodeShell     bool
	TokenSecrets  bool
}

// RenderDashboard renders the dashboard stack. A zero NodePort gives a
// ClusterIP Service.
func RenderDashboard(s DashboardSettings) []byte {
	m := bytes.ReplaceAll(dashboardYAML, []byte("__STRATUM_DASH_IMAGE__"), []byte(DashImage))
	if s.NodePort == 0 {
		m = bytes.ReplaceAll(m, []byte("    nodePort: __STRATUM_DASH_NODEPORT__\n"), nil)
		m = bytes.ReplaceAll(m, []byte("__STRATUM_DASH_SERVICE_TYPE__"), []byte("ClusterIP"))
	} else {
		m = bytes.ReplaceAll(m, []byte("__STRATUM_DASH_NODEPORT__"), []byte(strconv.Itoa(s.NodePort)))
		m = bytes.ReplaceAll(m, []byte("__STRATUM_DASH_SERVICE_TYPE__"), []byte("NodePort"))
	}
	secret := s.TLSSecretName
	if secret == "" {
		secret = DashboardTLSSecret
	}
	m = bytes.ReplaceAll(m, []byte("__STRATUM_DASH_TLS_SECRET__"), []byte(secret))

	var hosts string
	if u, err := url.Parse(s.PublicURL); err == nil && s.PublicURL != "" {
		hosts = u.Hostname()
	}
	m = bytes.ReplaceAll(m, []byte("__STRATUM_DASH_HOSTS__"), []byte(hosts))

	var args []string
	if s.PublicURL != "" {
		args = append(args, "--public-url="+s.PublicURL)
	}
	args = append(args, "--enable-node-shell="+strconv.FormatBool(s.NodeShell))
	var lines []string
	for _, a := range args {
		q, _ := json.Marshal(a)
		lines = append(lines, "        - "+string(q)+"\n")
	}
	m = bytes.ReplaceAll(m, []byte("        - __STRATUM_DASH_EXTRA_ARGS__\n"), []byte(strings.Join(lines, "")))
	if s.TokenSecrets {
		m = append(m, dashboardTokensYAML...)
	}
	return m
}

// Package trustaudit evaluates this node's etcd and kubelet client trust against the live components.
package trustaudit

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/ctrliq/stratum/internal/pki"
	"github.com/ctrliq/stratum/internal/runtime"
	"github.com/ctrliq/stratum/internal/staticpod"
	"github.com/ctrliq/stratum/internal/version"
)

// Result is the outcome of one audit.
type Result int

const (
	// Pass means the audit verified the property.
	Pass Result = iota + 1
	// Fail means the audit found the property violated, or could not read what it must.
	Fail
	// Exception means the audit could not verify the property and reports it unverified.
	Exception
)

// Verdict is an audit's result and the detail an operator reads.
type Verdict struct {
	Result Result
	Detail string
}

const kubeletProbeTimeout = 5 * time.Second

func cancelled(ctx context.Context) (Verdict, bool) {
	if err := ctx.Err(); err != nil {
		return Verdict{Exception, "unverified: the audit was cancelled before it finished: " + err.Error()}, true
	}
	return Verdict{}, false
}

func fileMissing(path string) bool {
	_, err := os.Lstat(path)
	return os.IsNotExist(err)
}

var etcdProbe = pki.EtcdRejectsRootSignedClient

// EtcdTrust reports whether this node's running etcd refuses a client certificate the cluster root signs.
func EtcdTrust(ctx context.Context) Verdict {
	if v, ok := cancelled(ctx); ok {
		return v
	}
	if fileMissing(staticpod.Path("etcd")) {
		return Verdict{Exception, "etcd.yaml not found — run the audit on a control-plane node (`stratum up` renders the static pod manifests)"}
	}
	trust := pki.ReadEtcdTrust()
	type probe struct {
		rejected bool
		err      error
	}
	run := etcdProbe
	done := make(chan probe, 1)
	go func() {
		rejected, err := run()
		done <- probe{rejected, err}
	}()
	select {
	case <-ctx.Done():
		v, _ := cancelled(ctx)
		return v
	case p := <-done:
		return etcdTrustVerdict(trust, p.rejected, p.err)
	}
}

func etcdTrustVerdict(trust pki.EtcdTrust, rejected bool, probeErr error) Verdict {
	switch {
	case trust.Err != nil:
		return Verdict{Fail,
			"cannot read this node's etcd trust artifacts, so a Kubernetes root anchor left behind by a CA rotation cannot be ruled out: " + trust.Err.Error()}
	case probeErr != nil:
		return Verdict{Exception,
			"could not probe the local etcd, so this is UNVERIFIED (not a pass): " + probeErr.Error()}
	case rejected && trust.TrustsRoot():
		return Verdict{Fail,
			fmt.Sprintf("the live root is rejected, but etcd's trust bundle still carries %d Kubernetes root anchor(s) "+
				"left behind by a CA rotation — any certificate signed by one is still full etcd read/write. "+
				"On-disk state: %s. Remediate with `%s --cluster --yes`, which drives the migration across every control plane",
				trust.StaleAnchors+boolToInt(trust.TrustsLiveRoot), trust.State(), pki.EtcdMigrateCommand)}
	case rejected:
		return Verdict{Pass,
			"verified against the running etcd: a cluster-root-signed client certificate is rejected"}
	default:
		return Verdict{Fail,
			fmt.Sprintf("the running etcd ACCEPTS a cluster-root-signed client certificate — any kubelet cert is full etcd read/write. "+
				"On-disk state: %s. Remediate with `%s --cluster --yes`, which drives the migration across every control plane",
				trust.State(), pki.EtcdMigrateCommand)}
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type kubeletConf struct {
	Authentication struct {
		X509 struct {
			ClientCAFile string `json:"clientCAFile"`
		} `json:"x509"`
	} `json:"authentication"`
}

// KubeletClientTrust reports whether this node's kubelet trusts a client CA distinct from the cluster root.
func KubeletClientTrust() Verdict {
	confPath := runtime.KubeletConfigPath()
	if fileMissing(confPath) {
		return Verdict{Exception, "kubelet.conf not found — `stratum up` renders it on every node"}
	}
	raw, err := os.ReadFile(confPath)
	if err != nil {
		return Verdict{Fail, "cannot read kubelet.conf to evaluate this control: " + err.Error()}
	}
	var conf kubeletConf
	if err := yaml.Unmarshal(raw, &conf); err != nil {
		return Verdict{Fail, "cannot parse kubelet.conf to evaluate this control: " + err.Error()}
	}
	bundle := pki.KubeletClientBundlePath()
	if got := conf.Authentication.X509.ClientCAFile; got != bundle {
		return Verdict{Fail, fmt.Sprintf("kubelet.conf's clientCAFile is %q, not %s", got, bundle)}
	}
	if _, err := os.Stat(bundle); err != nil {
		return Verdict{Fail, "cannot read the kubelet's client CA bundle: " + err.Error()}
	}
	t, err := pki.ReadKubeletClientTrust()
	if err != nil {
		return Verdict{Fail, "cannot classify the kubelet's client CA bundle: " + err.Error()}
	}
	return kubeletClientTrustVerdict(t)
}

func kubeletClientTrustVerdict(t pki.KubeletClientTrust) Verdict {
	bundle := filepath.Base(pki.KubeletClientBundlePath())
	switch {
	case len(t.Roots) > 0:
		return Verdict{Fail, fmt.Sprintf(
			"%s carries %d CA(s) from ca-bundle.crt, so any certificate the cluster root signs, admin's and every kubelet's included, reaches this kubelet's API. "+
				"Migration state: %s. Remediate with `%s --cluster --yes` on a control plane",
			bundle, len(t.Roots), t.State(), pki.KubeletMigrateCommand)}
	case len(t.Other) > 0:
		return Verdict{Fail, fmt.Sprintf("%s carries %d CA(s) that are neither in ca-bundle.crt nor a kubelet-client CA: %s",
			bundle, len(t.Other), subjects(t.Other))}
	case len(t.Anchors) == 0:
		return Verdict{Fail, bundle + " carries no kubelet-client CA, so the kubelet trusts no client"}
	}
	return Verdict{Pass, fmt.Sprintf("%s carries %d kubelet-client CA(s) and no CA from ca-bundle.crt", bundle, len(t.Anchors))}
}

func subjects(cs []*x509.Certificate) string {
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Subject.String()
	}
	return strings.Join(names, "; ")
}

var kubeletProbe = pki.ProbeKubeletClientAuth

// KubeletsRefuseRoot reports whether every node's running kubelet refuses a root-signed client certificate and accepts apiserver-kubelet-client.
func KubeletsRefuseRoot(ctx context.Context, cs kubernetes.Interface) Verdict {
	root, err := pki.RootSignedClientCert()
	if err != nil {
		return Verdict{Exception, "cannot mint a root-signed client certificate, so this is UNVERIFIED (run on a control plane): " + err.Error()}
	}
	apiserver, err := pki.LeafKeyPair("apiserver-kubelet-client")
	if err != nil {
		return Verdict{Fail, "cannot load apiserver-kubelet-client: " + err.Error()}
	}
	roots, err := pki.KubeletServingRoots()
	if err != nil {
		return Verdict{Fail, "cannot read the kubelets' serving trust: " + err.Error()}
	}
	nl, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Verdict{Fail, "could not list nodes to evaluate: " + err.Error()}
	}
	return kubeletsRefuseRootVerdict(ctx, nl.Items, roots, root, apiserver)
}

func kubeletsRefuseRootVerdict(ctx context.Context, nodes []corev1.Node, roots []*x509.Certificate, root, apiserver tls.Certificate) Verdict {
	if len(nodes) == 0 {
		return Verdict{Exception, "the apiserver lists no nodes, so this is UNVERIFIED (not a pass)"}
	}
	var open, broken, unknown []string
	for i := range nodes {
		n := &nodes[i]
		ip := internalIP(n)
		if ip == "" {
			unknown = append(unknown, n.Name+" (no InternalIP)")
			continue
		}
		if ctx.Err() != nil {
			unknown = append(unknown, fmt.Sprintf("%s (%v)", n.Name, ctx.Err()))
			continue
		}
		addr := net.JoinHostPort(ip, strconv.Itoa(version.KubeletPort))
		code, err := probeKubelet(ctx, addr, roots, root)
		switch {
		case err != nil:
			unknown = append(unknown, fmt.Sprintf("%s (%v)", n.Name, err))
			continue
		case code != 401:
			open = append(open, fmt.Sprintf("%s (HTTP %d)", n.Name, code))
		}
		code, err = probeKubelet(ctx, addr, roots, apiserver)
		switch {
		case err != nil:
			unknown = append(unknown, fmt.Sprintf("%s (%v)", n.Name, err))
		case code == 401:
			broken = append(broken, n.Name)
		}
	}
	var fails []string
	if len(open) > 0 {
		fails = append(fails, fmt.Sprintf("the kubelet ACCEPTS a root-signed client certificate on %s; remediate with `%s --cluster --yes`",
			strings.Join(open, ", "), pki.KubeletMigrateCommand))
	}
	if len(broken) > 0 {
		fails = append(fails, "the kubelet refuses apiserver-kubelet-client on "+strings.Join(broken, ", ")+", so kubectl logs and exec to it fail")
	}
	switch {
	case len(fails) > 0:
		return Verdict{Fail, strings.Join(fails, "; ")}
	case len(unknown) > 0:
		return Verdict{Exception, "could not probe every kubelet, so this is UNVERIFIED (not a pass): " + strings.Join(unknown, ", ")}
	}
	return Verdict{Pass, fmt.Sprintf("verified against the running kubelets: all %d refuse a root-signed client certificate and accept apiserver-kubelet-client", len(nodes))}
}

func probeKubelet(ctx context.Context, addr string, roots []*x509.Certificate, client tls.Certificate) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, kubeletProbeTimeout)
	defer cancel()
	return kubeletProbe(ctx, addr, roots, client)
}

func internalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

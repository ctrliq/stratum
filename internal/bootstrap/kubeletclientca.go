package bootstrap

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/ctrliq/stratum/internal/bootstrap/clusterinfo"
)

const kubeletClientCAPath = "/api/v1/namespaces/kube-public/configmaps/" + clusterinfo.KubeletClientCAConfigMap

// FetchKubeletClientCA anonymously fetches the kubelet client-CA bundle a split
// cluster publishes, over TLS verified against roots. found is false on a 404 or
// 403; the caller may treat that as legacy only when signed cluster-info says so.
func FetchKubeletClientCA(server string, roots []byte) (bundle []byte, found bool, err error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, false, fmt.Errorf("kubelet client CA: parse server %q: %w", server, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, false, fmt.Errorf("kubelet client CA: server %q must be an HTTPS URL", server)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(roots) {
		return nil, false, fmt.Errorf("kubelet client CA: the cluster CA bundle holds no usable certificate to verify the apiserver against")
	}
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}},
	}
	resp, err := client.Get(server + kubeletClientCAPath)
	if err != nil {
		return nil, false, fmt.Errorf("kubelet client CA: fetch from %s: %w", server, err)
	}
	defer resp.Body.Close()
	if resp.TLS == nil {
		return nil, false, fmt.Errorf("kubelet client CA: response from %s was not received over verified TLS", server)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusForbidden:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("kubelet client CA: fetch from %s: HTTP %d", server, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxClusterInfoBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("kubelet client CA: read response from %s: %w", server, err)
	}
	if len(body) > maxClusterInfoBytes {
		return nil, false, fmt.Errorf("kubelet client CA: response from %s exceeds %d bytes", server, maxClusterInfoBytes)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		return nil, false, fmt.Errorf("kubelet client CA: parse the %s ConfigMap: %w", clusterinfo.KubeletClientCAConfigMap, err)
	}
	bundle = []byte(cm.Data[clusterinfo.KubeletClientCAKey])
	if err := checkCABundle(bundle); err != nil {
		return nil, false, fmt.Errorf("kubelet client CA: the %s ConfigMap from %s: %w", clusterinfo.KubeletClientCAConfigMap, server, err)
	}
	return bundle, true, nil
}

func checkCABundle(b []byte) error {
	n := 0
	for {
		trimmed := bytes.TrimLeft(b, " \t\r\n")
		if len(trimmed) == 0 {
			break
		}
		// The kubelet's PEM parser recognizes a BEGIN line only at offset zero
		// or after a newline. These bytes are installed unchanged on the worker.
		if skipped := b[:len(b)-len(trimmed)]; len(skipped) > 0 && skipped[len(skipped)-1] != '\n' {
			return fmt.Errorf("certificate %d has whitespace immediately before its PEM block", n+1)
		}
		if !bytes.HasPrefix(trimmed, []byte("-----BEGIN ")) {
			return fmt.Errorf("holds data that is not PEM after certificate %d", n)
		}
		blk, remainder := pem.Decode(trimmed)
		if blk == nil {
			return fmt.Errorf("holds a malformed PEM block after certificate %d", n)
		}
		// pem.Decode may skip a malformed BEGIN block and return a later one.
		// The kubelet would parse fewer CAs than this validation counted.
		consumed := trimmed[:len(trimmed)-len(remainder)]
		if bytes.Count(consumed, []byte("-----BEGIN ")) != 1 {
			return fmt.Errorf("holds a malformed PEM block before certificate %d", n+1)
		}
		if len(blk.Headers) > 0 {
			return fmt.Errorf("certificate %d carries PEM headers", n+1)
		}
		if blk.Type != "CERTIFICATE" {
			return fmt.Errorf("holds a %s block, not only certificates", blk.Type)
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return fmt.Errorf("certificate %d: %w", n+1, err)
		}
		if !c.IsCA {
			return fmt.Errorf("certificate %d (%s) is not a CA", n+1, c.Subject)
		}
		b = remainder
		n++
	}
	if n == 0 {
		return fmt.Errorf("holds no certificate")
	}
	return nil
}

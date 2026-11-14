package k8sutil

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/ctrliq/stratum/internal/boundedfile"
	"github.com/ctrliq/stratum/internal/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// The evidence reader constructs a fresh client for each read, including the
// kubelet's current rotated certificate, without leaving client-go any deferred
// credential-file reads outside the request's metadata bounds.
func kubeletClientsetContext(ctx context.Context) (kubernetes.Interface, error) {
	path := filepath.Join(version.KubeconfigDir, "kubelet.kubeconfig")
	b, err := boundedfile.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	raw, err := clientcmd.Load(b)
	if err != nil {
		return nil, err
	}
	for _, cluster := range raw.Clusters {
		cluster.LocationOfOrigin = path
	}
	for _, user := range raw.AuthInfos {
		user.LocationOfOrigin = path
	}
	if err := clientcmd.ResolveLocalPaths(raw); err != nil {
		return nil, err
	}
	load := func(path *string, data *[]byte) error {
		if *path != "" && len(*data) == 0 {
			b, err := boundedfile.Read(ctx, *path)
			if err != nil {
				return err
			}
			*data = b
		}
		*path = ""
		return nil
	}
	if current := raw.Contexts[raw.CurrentContext]; current != nil {
		if cluster := raw.Clusters[current.Cluster]; cluster != nil {
			if err := load(&cluster.CertificateAuthority, &cluster.CertificateAuthorityData); err != nil {
				return nil, err
			}
		}
		if user := raw.AuthInfos[current.AuthInfo]; user != nil {
			// Engine-written kubelet credentials are certificates or tokens.
			// Auth plugins can perform I/O with a separate lifecycle.
			if user.Exec != nil || user.AuthProvider != nil {
				return nil, errors.New("node identity requires kubelet file credentials")
			}
			if err := load(&user.ClientCertificate, &user.ClientCertificateData); err != nil {
				return nil, err
			}
			if err := load(&user.ClientKey, &user.ClientKeyData); err != nil {
				return nil, err
			}
			if user.TokenFile != "" {
				b, err := boundedfile.Read(ctx, user.TokenFile)
				if err != nil && (user.Token == "" || ctx.Err() != nil) {
					return nil, err
				}
				// Like client-go's file token source, prefer a usable file
				// token and fall back to the inline token on read failure.
				if token := strings.TrimSpace(string(b)); err == nil && token != "" {
					user.Token = token
				}
				if user.Token == "" {
					return nil, errors.New("node identity token file is empty")
				}
			}
			user.TokenFile = ""
		}
	}
	cfg, err := clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg.Timeout = 15 * time.Second
	return kubernetes.NewForConfig(cfg)
}

// Command apipoller is the in-cluster API client check-ha-rotate-ca.sh runs
// through a CA rotation. It calls the API once a second with its
// ServiceAccount token and logs one line per call: ok, x509 for a certificate
// verification failure, 401 for a token the API server rejects, error for
// anything else.
//
// It reads the cluster CA once, at start, as a client that pins its trust then
// does. client-go would otherwise re-read ca.crt on rotation and hide the
// x509 failure this check exists to catch.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "apipoller:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	ca, err := os.ReadFile(cfg.TLSClientConfig.CAFile)
	if err != nil {
		return err
	}
	cfg.TLSClientConfig.CAFile, cfg.TLSClientConfig.CAData = "", ca
	cfg.Timeout = 5 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	ns := os.Getenv("POD_NAMESPACE")
	for ; ; time.Sleep(time.Second) {
		_, err := cs.CoreV1().ConfigMaps(ns).Get(context.Background(), "kube-root-ca.crt", metav1.GetOptions{})
		switch {
		case err == nil:
			fmt.Println("ok")
		case strings.Contains(err.Error(), "x509:"):
			fmt.Println("x509", err)
		case apierrors.IsUnauthorized(err):
			fmt.Println("401", err)
		default:
			fmt.Println("error", err)
		}
	}
}

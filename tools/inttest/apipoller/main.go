// Command apipoller is the in-cluster API probe check-ha-rotate-ca.sh runs through a CA rotation.
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
	// Pinned CA: client-go re-reading ca.crt on rotation would hide the x509 failure.
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

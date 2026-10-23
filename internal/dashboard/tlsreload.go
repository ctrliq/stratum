package dashboard

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certReloader serves the key pair at certFile/keyFile and picks up a new one
// when either file changes, so a renewed Secret takes effect without a restart.
type certReloader struct {
	certFile, keyFile string
	recheck           time.Duration
	now               func() time.Time
	log               *slog.Logger

	mu        sync.Mutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	checkedAt time.Time
	lastErr   string
}

func newCertReloader(certFile, keyFile string, log *slog.Logger) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, recheck: 10 * time.Second, now: time.Now, log: log}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certReloader) load() error {
	cm, km, err := r.mtimes()
	if err != nil {
		return err
	}
	pair, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load --tls-cert/--tls-key: %w", err)
	}
	r.cert, r.certMod, r.keyMod = &pair, cm, km
	return nil
}

func (r *certReloader) mtimes() (time.Time, time.Time, error) {
	c, err := os.Stat(r.certFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--tls-cert: %w", err)
	}
	k, err := os.Stat(r.keyFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--tls-key: %w", err)
	}
	return c.ModTime(), k.ModTime(), nil
}

// get keeps serving the last good pair when a reload fails, since a half-written
// update is expected while the kubelet swaps a Secret volume. A failure is
// logged once per distinct error, so a pair that never loads is visible without
// a line every recheck.
func (r *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); now.Sub(r.checkedAt) >= r.recheck {
		r.checkedAt = now
		r.reload()
	}
	return r.cert, nil
}

func (r *certReloader) reload() {
	cm, km, err := r.mtimes()
	if err == nil && cm.Equal(r.certMod) && km.Equal(r.keyMod) {
		return
	}
	if err == nil {
		err = r.load()
	}
	if err != nil {
		if msg := err.Error(); msg != r.lastErr {
			r.lastErr = msg
			r.log.Warn("could not reload the TLS certificate; still serving the previous one",
				"cert", r.certFile, "key", r.keyFile, "err", err)
		}
		return
	}
	r.lastErr = ""
	attrs := []any{"cert", r.certFile}
	if leaf := r.cert.Leaf; leaf != nil {
		attrs = append(attrs, "notAfter", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	r.log.Info("reloaded the TLS certificate", attrs...)
}

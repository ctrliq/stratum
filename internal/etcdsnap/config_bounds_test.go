package etcdsnap

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSnapshotConfigRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"fifo", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snapshot.conf")
			if kind == "fifo" {
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, bytes.Repeat([]byte("# padding\n"), 210000), 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := loadConfigFrom(path, DefaultConfig()); done <- err }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("unsafe snapshot config was accepted")
				}
			case <-time.After(500 * time.Millisecond):
				t.Error("FIFO snapshot config blocked the reader")
				f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.Write([]byte("RETAIN=3\n"))
				_ = f.Close()
				<-done
			}
		})
	}
}

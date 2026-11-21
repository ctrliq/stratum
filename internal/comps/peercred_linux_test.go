package comps

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPeerPIDIsTheListener(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	pid, err := peerPID(sock)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("peerPID = %d, %v; want this process, %d", pid, err, os.Getpid())
	}
}

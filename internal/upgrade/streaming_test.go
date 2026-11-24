package upgrade

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Stand-in for the ~660MB stratum binary; buffering it once blows the budget below.
const payloadSize = 128 << 20

const heapBudget = 8 << 20

func bigFile(t *testing.T, p string, fill byte) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(payloadSize); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{fill}, payloadSize-1); err != nil {
		t.Fatal(err)
	}
}

func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestInstallDoesNotBufferThePayload(t *testing.T) {
	dir := t.TempDir()
	dst, src := filepath.Join(dir, "stratum"), filepath.Join(dir, "next")
	bigFile(t, dst, 'a')
	bigFile(t, src, 'b')

	var bak string
	if n := allocated(func() {
		var err error
		if bak, err = backupAndInstall(io.Discard, dst, src, resumeTS); err != nil {
			t.Fatal(err)
		}
	}); n > heapBudget {
		t.Errorf("backupAndInstall allocated %dMB for a %dMB binary; want a stream", n>>20, payloadSize>>20)
	}
	if n := allocated(func() {
		if err := atomicInstall(io.Discard, bak, dst); err != nil {
			t.Fatal(err)
		}
	}); n > heapBudget {
		t.Errorf("rollback install allocated %dMB for a %dMB binary; want a stream", n>>20, payloadSize>>20)
	}
	if n := allocated(func() {
		if err := atomicInstall(io.Discard, bak, dst); err != nil {
			t.Fatal(err)
		}
	}); n > heapBudget {
		t.Errorf("no-op install allocated %dMB for a %dMB binary; want a stream", n>>20, payloadSize>>20)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, payloadSize)
	want[payloadSize-1] = 'a'
	if !bytes.Equal(got, want) {
		t.Error("rollback did not restore the original binary")
	}
}

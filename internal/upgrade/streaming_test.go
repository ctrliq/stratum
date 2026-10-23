package upgrade

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// payloadSize stands in for the ~660MB stratum binary: large enough that
// buffering it even once blows the budget below.
const payloadSize = 128 << 20

// heapBudget is what a streaming copy may allocate in total: buffers, not files.
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

// allocated reports the bytes fn allocates on the heap.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestInstallDoesNotBufferThePayload: installing the stratum binary, backing up
// the one it replaces, and restoring it must stream. Holding the whole file
// (several times over) OOM-killed a failed upgrade's rollback on a 3GiB worker.
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
		if err := atomicInstall(io.Discard, bak, dst); err != nil { // the rollback
			t.Fatal(err)
		}
	}); n > heapBudget {
		t.Errorf("rollback install allocated %dMB for a %dMB binary; want a stream", n>>20, payloadSize>>20)
	}
	// Unchanged content: the compare must not read the files whole either.
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

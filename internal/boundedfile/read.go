// Package boundedfile reads small local metadata files without accepting
// streams or allocating memory proportional to an unchecked file size.
package boundedfile

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"
)

// MaxBytes bounds each metadata file, including files that grow during a read.
const MaxBytes = 1 << 20

// Read requires a regular file and checks cancellation between reads. Opening
// nonblocking before checking the descriptor's type also rejects a FIFO swapped
// into place during the open. As with other local filesystem operations, the
// kernel must return from each syscall before cancellation can be observed.
func Read(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: metadata must be a regular file", path)
	}
	if info.Size() > MaxBytes {
		return nil, fmt.Errorf("%s: metadata exceeds %d bytes", path, MaxBytes)
	}
	b, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(b) > MaxBytes {
		return nil, fmt.Errorf("%s: metadata exceeds %d bytes", path, MaxBytes)
	}
	return b, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

package mcp

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// TestTrimLog covers trimLog itself, which only ever sees a buffer the
// reader has already capped. The cap as pod_logs applies it is
// TestReadLogTail.
func TestTrimLog(t *testing.T) {
	t.Run("a short log is untouched", func(t *testing.T) {
		got := trimLog([]byte("one\ntwo\nthree\n"))
		if got.Truncated {
			t.Error("a short log was reported as truncated")
		}
		if got.Logs != "one\ntwo\nthree\n" {
			t.Errorf("logs = %q", got.Logs)
		}
		if got.Lines != 3 {
			t.Errorf("lines = %d, want 3", got.Lines)
		}
	})

	t.Run("exactly the cap is not truncated", func(t *testing.T) {
		if got := trimLog(make([]byte, MaxLogBytes)); got.Truncated {
			t.Error("a log of exactly MaxLogBytes was reported as truncated")
		}
	})

	// A cut mid-line reads as corruption, so the partial first line goes.
	t.Run("no partial first line", func(t *testing.T) {
		got := trimLog([]byte(strings.Repeat("x", MaxLogBytes) + "\ntail\n"))
		if !got.Truncated {
			t.Fatal("not truncated")
		}
		if strings.HasPrefix(got.Logs, "x") {
			t.Errorf("result opens mid-line: %q", got.Logs[:20])
		}
	})
}

// TestReadLogTail covers the cap at the boundary pod_logs uses: a stream,
// not a buffer. A container log arrives oldest-first, so reading from the
// front keeps the wrong end.
func TestReadLogTail(t *testing.T) {
	t.Run("a short log is returned whole", func(t *testing.T) {
		got, err := readLogTail(strings.NewReader("one\ntwo\nthree\n"))
		if err != nil {
			t.Fatalf("readLogTail: %v", err)
		}
		if got.Truncated {
			t.Error("a short log was reported as truncated")
		}
		if got.Logs != "one\ntwo\nthree\n" {
			t.Errorf("logs = %q", got.Logs)
		}
	})

	t.Run("over the cap keeps the end", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(strings.Repeat("OLD LINE\n", MaxLogBytes/9+100))
		b.WriteString("THE CAUSE\n")

		got, err := readLogTail(strings.NewReader(b.String()))
		if err != nil {
			t.Fatalf("readLogTail: %v", err)
		}
		if !got.Truncated {
			t.Fatal("an oversized log was not reported as truncated")
		}
		if !strings.HasSuffix(got.Logs, "THE CAUSE\n") {
			t.Error("the last line was dropped; the cap kept the beginning of an oldest-first stream")
		}
		if len(got.Logs) > MaxLogBytes {
			t.Errorf("kept %d bytes, over the %d cap", len(got.Logs), MaxLogBytes)
		}
		if got.Note == "" {
			t.Error("truncated with no note")
		}
	})

	// A byte at a time is the worst case for a buffer that slides.
	t.Run("the same answer however the stream is chunked", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(strings.Repeat("OLD LINE\n", MaxLogBytes/9+100))
		b.WriteString("THE CAUSE\n")

		whole, err := readLogTail(strings.NewReader(b.String()))
		if err != nil {
			t.Fatalf("readLogTail: %v", err)
		}
		dribbled, err := readLogTail(iotest.OneByteReader(strings.NewReader(b.String())))
		if err != nil {
			t.Fatalf("readLogTail dribbled: %v", err)
		}
		if dribbled.Logs != whole.Logs {
			t.Errorf("a one-byte-at-a-time stream gave %d bytes, a whole one gave %d",
				len(dribbled.Logs), len(whole.Logs))
		}
	})

	t.Run("a stream error is reported, not silently truncated", func(t *testing.T) {
		r := io.MultiReader(strings.NewReader("some output\n"), iotest.ErrReader(errBoom))
		if _, err := readLogTail(r); !errors.Is(err, errBoom) {
			t.Errorf("err = %v, want it to wrap errBoom", err)
		}
	})
}

var errBoom = errors.New("boom")

func TestListOptionsLimit(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, DefaultListLimit},
		{-5, DefaultListLimit},
		{10, 10},
		{MaxListLimit, MaxListLimit},
		{MaxListLimit + 1, MaxListLimit},
		{1 << 20, MaxListLimit},
	} {
		if got := (ListOptions{Limit: tc.in}).limit(); got != tc.want {
			t.Errorf("limit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// An untruncated list must not carry truncation noise, or a model learns to
// ignore the field.
func TestNewListMeta(t *testing.T) {
	full := newListMeta(3, 100, "")
	if full.Truncated || full.Note != "" || full.Continue != "" {
		t.Errorf("a complete list reported truncation: %+v", full)
	}

	part := newListMeta(100, 100, "tok")
	if !part.Truncated {
		t.Error("a continue token did not set truncated")
	}
	if part.Continue != "tok" || part.Note == "" {
		t.Errorf("incomplete envelope: %+v", part)
	}
}

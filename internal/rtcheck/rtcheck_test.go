package rtcheck

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// versionReply is a CRI VersionResponse: version(1), runtime_name(2),
// runtime_version(3), runtime_api_version(4).
func versionReply(runtimeVersion string) []byte {
	var b []byte
	for i, s := range []string{"0.1.0", "containerd", runtimeVersion, "v1"} {
		b = protowire.AppendTag(b, protowire.Number(i+1), protowire.BytesType)
		b = protowire.AppendString(b, s)
	}
	return b
}

// fakeCRI serves the CRI Version call on a unix socket, as the runtime that
// holds the socket would.
func fakeCRI(t *testing.T, runtimeVersion string) string {
	t.Helper()
	return fakeCRIWith(t, func() ([]byte, error) { return versionReply(runtimeVersion), nil })
}

// fakeCRIWith serves reply's result (or error) to every Version call.
func fakeCRIWith(t *testing.T, reply func() ([]byte, error)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cri-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.ForceServerCodecV2(rawCodec{}), grpc.UnknownServiceHandler(func(_ any, st grpc.ServerStream) error {
		if m, _ := grpc.MethodFromServerStream(st); m != "/runtime.v1.RuntimeService/Version" {
			t.Errorf("unexpected method %q", m)
		}
		var req []byte
		if err := st.RecvMsg(&req); err != nil {
			return err
		}
		b, err := reply()
		if err != nil {
			return err
		}
		return st.SendMsg(b)
	}))
	go srv.Serve(ln)
	t.Cleanup(srv.Stop)
	return sock
}

func TestServing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := Serving(ctx, "unix://"+fakeCRI(t, "v2.3.5+stratum"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "v2.3.5+stratum" {
		t.Errorf("Serving = %q, want v2.3.5+stratum", got)
	}
}

// TestServingCleansWhatTheSocketSays: the reply is attacker-shaped as far as a
// terminal is concerned. Control characters and escapes never reach the caller,
// and neither does more than maxReplyText bytes, in the version or in the error
// text a server sends.
func TestServingCleansWhatTheSocketSays(t *testing.T) {
	const esc = "\x1b[2J\x1b]0;pwned\x07\r\n\x00"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	printable := func(t *testing.T, what, s string) {
		t.Helper()
		for _, r := range s {
			if !unicode.IsPrint(r) {
				t.Errorf("%s %q holds non-printable %q", what, s, r)
			}
		}
		if len(s) > maxReplyText+len("...")+256 { // the error adds its own short prefix
			t.Errorf("%s is %d bytes", what, len(s))
		}
	}

	t.Run("version", func(t *testing.T) {
		got, err := Serving(ctx, fakeCRI(t, "v2.3.5"+esc))
		if err != nil {
			t.Fatal(err)
		}
		printable(t, "version", got)
		if Same(got, "v2.3.5") || !strings.HasPrefix(got, "v2.3.5") {
			t.Errorf("cleaned version = %q; want v2.3.5 plus visible junk that cannot match", got)
		}
	})
	t.Run("oversized version", func(t *testing.T) {
		got, err := Serving(ctx, fakeCRI(t, "v2.3.5"+strings.Repeat("A", 100_000)))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > maxReplyText+len("...") || !strings.HasSuffix(got, "...") {
			t.Errorf("oversized version came back as %d bytes %q; want it capped and marked", len(got), got)
		}
	})
	t.Run("invalid UTF-8", func(t *testing.T) {
		got, err := Serving(ctx, fakeCRI(t, "v2.3.5\xff\xfe"))
		if err != nil {
			t.Fatal(err)
		}
		printable(t, "version", got)
	})
	t.Run("error text", func(t *testing.T) {
		sock := fakeCRIWith(t, func() ([]byte, error) {
			return nil, status.Error(codes.Internal, esc+strings.Repeat("B", 100_000))
		})
		_, err := Serving(ctx, sock)
		if err == nil {
			t.Fatal("Serving succeeded against an erroring runtime")
		}
		printable(t, "error", err.Error())
		if !strings.Contains(err.Error(), "Internal") {
			t.Errorf("error %q lost the status code", err)
		}
	})
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"v2.4.1+stratum": "v2.4.1+stratum",
		"a\x1bb\nc":      "a?b?c",
		"é ok":           "é ok",
		"x\xffy":         "x?y",
		"":               "",
	} {
		if got := clean(in); got != want {
			t.Errorf("clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := clean(strings.Repeat("é", 200)); len(got) > maxReplyText+3 || !strings.HasSuffix(got, "...") {
		t.Errorf("multi-byte input not capped on a rune boundary: %d bytes", len(got))
	}
}

func TestServingNothingListening(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if v, err := Serving(ctx, filepath.Join(t.TempDir(), "none.sock")); err == nil {
		t.Fatalf("Serving with no listener = %q, want error", v)
	}
}

func TestRuntimeVersionMalformed(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":     nil,
		"no field":  protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "x"),
		"truncated": {0x1a, 0x09, 'v'},
	} {
		if v, err := runtimeVersion(b); err == nil {
			t.Errorf("%s: runtimeVersion = %q, want error", name, v)
		}
	}
}

func TestInstalled(t *testing.T) {
	for _, tc := range []struct{ name, out, want string }{
		{"containerd", "containerd github.com/containerd/containerd/v2 v2.4.1+stratum abc123", "v2.4.1+stratum"},
		{"kubelet", "Kubernetes v1.37.1-stratum", "v1.37.1-stratum"},
	} {
		bin := filepath.Join(t.TempDir(), tc.name)
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '"+tc.out+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := Installed(context.Background(), bin)
		if err != nil || got != tc.want {
			t.Errorf("%s: Installed = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	if _, err := Installed(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("Installed of a missing binary succeeded")
	}
}

func TestSame(t *testing.T) {
	if !Same("v2.4.1+stratum", "2.4.1+stratum") || Same("2.3.5+stratum", "v2.4.1+stratum") {
		t.Error("Same ignores a leading v and nothing else")
	}
}

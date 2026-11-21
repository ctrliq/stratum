// Package rtcheck compares the installed containerd with the one serving on the CRI socket.
package rtcheck

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
)

// Installed parses the version token out of containerd or Kubernetes `--version` output.
func Installed(ctx context.Context, bin string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", bin, err)
	}
	for _, f := range strings.Fields(string(out)) {
		if v := strings.TrimPrefix(f, "v"); v != "" && unicode.IsDigit(rune(v[0])) {
			return f, nil
		}
	}
	return "", fmt.Errorf("%s --version: no version in %q", bin, strings.TrimSpace(string(out)))
}

// Same compares two versions ignoring a leading "v": the CRI reports it with or without one.
func Same(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// rawCodec carries requests and replies as raw bytes, so no generated CRI client is needed.
type rawCodec struct{}

func (rawCodec) Marshal(v any) (mem.BufferSlice, error) {
	return mem.BufferSlice{mem.SliceBuffer(v.([]byte))}, nil
}

func (rawCodec) Unmarshal(data mem.BufferSlice, v any) error {
	*v.(*[]byte) = data.Materialize()
	return nil
}

func (rawCodec) Name() string { return "proto" }

var _ encoding.CodecV2 = rawCodec{}

// Serving reports whatever answers the CRI socket; its reply is untrusted and is cleaned.
func Serving(ctx context.Context, sock string) (string, error) {
	cc, err := grpc.NewClient("unix://"+strings.TrimPrefix(sock, "unix://"),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodecV2(rawCodec{})))
	if err != nil {
		return "", err
	}
	defer cc.Close()
	var reply []byte
	if err := cc.Invoke(ctx, "/runtime.v1.RuntimeService/Version", []byte{}, &reply, grpc.WaitForReady(true)); err != nil {
		st := status.Convert(err)
		return "", fmt.Errorf("CRI Version on %s: %v: %s", sock, st.Code(), clean(st.Message()))
	}
	v, err := runtimeVersion(reply)
	if err != nil {
		return "", err
	}
	return clean(v), nil
}

// maxReplyText caps any string taken from the runtime's reply.
const maxReplyText = 128

// clean makes a runtime-supplied string safe to print.
func clean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsPrint(r) || r == utf8.RuneError {
			r = '?'
		}
		if b.Len()+utf8.RuneLen(r) > maxReplyText {
			b.WriteString("...")
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

func runtimeVersion(b []byte) (string, error) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return "", protowire.ParseError(n)
		}
		b = b[n:]
		if num == 3 && typ == protowire.BytesType {
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return "", protowire.ParseError(n)
			}
			return string(v), nil
		}
		if n = protowire.ConsumeFieldValue(num, typ, b); n < 0 {
			return "", protowire.ParseError(n)
		}
		b = b[n:]
	}
	return "", fmt.Errorf("CRI Version reply carries no runtime_version")
}

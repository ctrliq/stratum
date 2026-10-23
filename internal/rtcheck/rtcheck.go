// Package rtcheck compares what is installed on disk with what is actually
// serving: a node can run a different containerd than the one on disk when an
// orphan from an earlier run holds the CRI socket.
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

// Installed is the version `bin --version` prints: the first token that starts
// with a digit once a leading "v" is dropped. It matches both
// "containerd github.com/containerd/containerd/v2 v<containerd version>+stratum <rev>" and
// "Kubernetes v<k8s version>-stratum".
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

// Same compares two versions ignoring a leading "v": containerd's CRI reports
// its version with or without one depending on the caller.
func Same(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// rawCodec carries the CRI request and reply as bytes, so asking the runtime
// for its version needs no generated CRI client (and no new module).
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

// Serving asks the CRI runtime on the unix socket for its version
// (runtime.v1.RuntimeService/Version, an empty request) and returns
// VersionResponse.runtime_version. Whatever answers the socket is what this
// reports, so an orphan holding it shows up as its own version.
//
// Whatever answers the socket is not trusted: the version and any error text
// it sends back are printed to an operator's terminal, so both are cleaned
// (clean) before they leave this package, and a reply that is not a plain
// version cannot match the installed one.
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
		// status.Convert keeps the server-supplied message apart from our own
		// wording, so it can be cleaned; a ctx error converts to its own code.
		st := status.Convert(err)
		return "", fmt.Errorf("CRI Version on %s: %v: %s", sock, st.Code(), clean(st.Message()))
	}
	v, err := runtimeVersion(reply)
	if err != nil {
		return "", err
	}
	return clean(v), nil
}

// maxReplyText caps any string taken from the runtime's reply. Real versions
// are a few dozen bytes.
const maxReplyText = 128

// clean makes s safe to print: every rune that is not printable (control
// characters, terminal escapes, invalid UTF-8) becomes '?', and the result is
// cut to maxReplyText bytes, marked "..." when it was longer.
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

// runtimeVersion pulls runtime_version (field 3) out of a VersionResponse.
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

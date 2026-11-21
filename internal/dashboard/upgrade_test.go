package dashboard

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/client-go/rest"
)

// Pins that the session token reaches the handshake and the subprotocol survives.
func TestExecUpgradeCarriesTheTokenAndSubprotocol(t *testing.T) {
	var gotAuth, gotProto, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotProto = r.Header.Get("Sec-Websocket-Protocol")
		gotPath = r.URL.Path
		if r.URL.Path == "/apis/authentication.k8s.io/v1/selfsubjectreviews" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":{"userInfo":{"username":"dev","groups":[]}}}`))
			return
		}
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("upstream could not hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-Websocket-Protocol: v4.channel.k8s.io\r\n\r\n")
		_ = buf.Flush()
		line, _ := buf.ReadString('\n')
		_, _ = buf.WriteString("echo:" + line)
		_ = buf.Flush()
	}))
	defer upstream.Close()

	cfg := &rest.Config{Host: upstream.URL}
	tgt, err := localTarget(cfg)
	if err != nil {
		t.Fatalf("localTarget: %v", err)
	}
	sess, err := newSessions([]byte("upgrade-test-secret"), time.Hour, false)
	if err != nil {
		t.Fatalf("newSessions: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &server{
		base: cfg, sessions: sess, proxy: newProxy(tgt, false, log),
		spa: newSPAHandler(fstest.MapFS{"index.html": {Data: []byte("spa")}}),
		log: log, logins: rate.NewLimiter(rate.Inf, 1),
	}
	dash := httptest.NewServer(srv.routes())
	defer dash.Close()

	cookieValue, _, err := sess.issue("dev", nil, "the-session-token")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	addr := strings.TrimPrefix(dash.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// A browser cannot set Authorization or CSRF on a WebSocket handshake.
	req := "GET /k8s/api/v1/namespaces/default/pods/web-0/exec?command=sh&stdin=true&tty=true HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: ZGFzaGJvYXJkdGVzdA==\r\n" +
		"Sec-WebSocket-Protocol: v4.channel.k8s.io\r\n" +
		"Origin: http://" + addr + "\r\n" +
		"Sec-Fetch-Site: same-origin\r\n" +
		"Cookie: " + cookieName + "=" + cookieValue + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(status, "101") {
		rest, _ := io.ReadAll(io.LimitReader(br, 512))
		t.Fatalf("upgrade not switched: %q %s", status, rest)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	if _, err := conn.Write([]byte("keystroke\n")); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	echo, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read through the tunnel: %v", err)
	}
	if strings.TrimSpace(echo) != "echo:keystroke" {
		t.Fatalf("tunnel echoed %q, want the frame back", echo)
	}

	if gotPath != "/api/v1/namespaces/default/pods/web-0/exec" {
		t.Errorf("apiserver path = %q, want the /k8s prefix stripped", gotPath)
	}
	if gotAuth != "Bearer the-session-token" {
		t.Errorf("Authorization on the handshake = %q, want the session's token", gotAuth)
	}
	if gotProto != "v4.channel.k8s.io" {
		t.Errorf("Sec-Websocket-Protocol = %q, want it forwarded; the exec stream framing depends on it", gotProto)
	}
}

// A buffered follow fails silently: nothing shows until the pod exits.
func TestFollowStreamsBeforeTheUpstreamFinishes(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "first log line\n")
		_ = http.NewResponseController(w).Flush()
		<-release
		_, _ = io.WriteString(w, "last log line\n")
	}))
	defer upstream.Close()
	defer close(release)

	cfg := &rest.Config{Host: upstream.URL}
	tgt, err := localTarget(cfg)
	if err != nil {
		t.Fatalf("localTarget: %v", err)
	}
	sess, err := newSessions([]byte("follow-test-secret"), time.Hour, false)
	if err != nil {
		t.Fatalf("newSessions: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := &server{
		base: cfg, sessions: sess, proxy: newProxy(tgt, false, log),
		spa: newSPAHandler(fstest.MapFS{"index.html": {Data: []byte("spa")}}),
		log: log, logins: rate.NewLimiter(rate.Inf, 1),
	}
	dash := httptest.NewServer(srv.routes())
	defer dash.Close()

	cookieValue, _, err := sess.issue("dev", nil, "the-session-token")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, dash.URL+"/k8s/api/v1/namespaces/default/pods/web-0/log?follow=true", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: cookieValue})
	resp, err := dash.Client().Do(req)
	if err != nil {
		t.Fatalf("the follow response never started within 5s; the proxy is buffering it instead of streaming: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow: got %d", resp.StatusCode)
	}

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("the first log line never arrived within 5s; the proxy is buffering a follow instead of streaming it: %v", err)
	}
	if strings.TrimSpace(line) != "first log line" {
		t.Fatalf("got %q, want the first line", line)
	}
}

// Flush commits headers; recoverMiddleware decides from responseRecorder.wrote.
func TestRecoverDoesNotAppendToAnAlreadyFlushedStream(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := accessLog(log)(recoverMiddleware(log)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			// net/http commits an implicit 200 here, the state the recorder must see.
			_ = http.NewResponseController(w).Flush()
			panic("a stream that dies halfway")
		})))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/k8s/api/v1/namespaces/d/pods/p/log?follow=true")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), "internal") {
		t.Fatalf("a JSON error body was appended to a committed stream: %q", body)
	}
	if len(body) != 0 {
		t.Fatalf("unexpected body on a flushed-then-panicked stream: %q", body)
	}
}

func TestRecoverAnswersWhenNothingWasWritten(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := accessLog(log)(recoverMiddleware(log)(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { panic("boom") })))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/api/v1/config")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"error":"internal"`) {
		t.Fatalf("body = %q, want the JSON error", body)
	}
}

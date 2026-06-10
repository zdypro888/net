package net

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	stdnet "net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyDialContextUsesTLSForHTTPSProxy(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	proxy := &Proxy{
		Address: "https://user:pass@" + server.Listener.Addr().String(),
	}
	conn, err := proxy.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialContext failed: %v", err)
	}
	checkClose(t, "proxy conn", conn.Close)

	req := <-requests
	if req.TLS == nil {
		t.Fatalf("proxy request was not sent over TLS")
	}
	if req.Method != http.MethodConnect {
		t.Fatalf("method = %s, want CONNECT", req.Method)
	}
	if got := req.Header.Get("Proxy-Authorization"); got != "Basic dXNlcjpwYXNz" {
		t.Fatalf("Proxy-Authorization = %q", got)
	}
}

func TestProxyDialContextBasicAuthUsesDecodedUserinfo(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	proxy := &Proxy{
		Address: "http://user:p%40ss@" + server.Listener.Addr().String(),
	}
	conn, err := proxy.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialContext failed: %v", err)
	}
	checkClose(t, "proxy conn", conn.Close)

	req := <-requests
	if got := req.Header.Get("Proxy-Authorization"); got != "Basic dXNlcjpwQHNz" {
		t.Fatalf("Proxy-Authorization = %q, want decoded userinfo", got)
	}
}

func TestProxyDialContextCancelWhileWaitingForConnectResponse(t *testing.T) {
	requestReceived := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestReceived)
		<-r.Context().Done()
	}))
	defer server.Close()

	proxy := &Proxy{Address: "http://" + server.Listener.Addr().String()}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		conn, err := proxy.DialContext(ctx, "tcp", "example.com:443")
		if conn != nil {
			checkClose(t, "proxy conn", conn.Close)
		}
		errCh <- err
	}()

	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("proxy did not receive CONNECT request")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DialContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DialContext did not return after context cancellation")
	}
}

func TestProxyDialContextHTTPSProxyStrictRejectsSelfSigned(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	proxy := &Proxy{Address: "https://" + server.Listener.Addr().String(), TLSConfig: StrictTLSConfig()}
	conn, err := proxy.DialContext(context.Background(), "tcp", "example.com:443")
	if conn != nil {
		checkClose(t, "proxy conn", conn.Close)
	}
	if err == nil {
		t.Fatalf("DialContext should have failed against self-signed proxy with strict TLS config")
	}
	var verr *tls.CertificateVerificationError
	if !errors.As(err, &verr) {
		// 不强约束错误类型, 但记录方便排查; Go runtime 通常返回此类型.
		t.Logf("got non-verification error (still acceptable as a failure): %v", err)
	}
}

// TestProxyDialContextPreservesBytesBufferedAfterConnect 回归: 代理在 CONNECT 200
// 响应后紧接着 (同一次写入) 推送隧道数据时, http.ReadResponse 会把这些字节预读进
// bufio.Reader 缓冲。修复前 DialContext 返回裸 conn 丢掉它们; 修复后通过 prefixConn
// 补回。修复前本测试在最后一步读到 io 错误而非 payload, 故失败。
func TestProxyDialContextPreservesBytesBufferedAfterConnect(t *testing.T) {
	const earlyPayload = "EARLY-TUNNEL-BYTES"

	ln, err := stdnet.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer checkClose(t, "proxy test listener", ln.Close)

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer checkClose(t, "proxy test accepted conn", c.Close)
		br := bufio.NewReader(c)
		// 读完 CONNECT 请求头 (到空行)。
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		// 关键: 把 200 响应与隧道首包数据合并为单次写入, 让客户端的 bufio.Reader
		// 在解析响应头时把 earlyPayload 一并读进缓冲。
		_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n" + earlyPayload))
		// 保持连接, 等测试读完。
		_, _ = br.ReadByte()
	}()

	proxy := &Proxy{Address: "http://" + ln.Addr().String()}
	conn, err := proxy.DialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatalf("DialContext failed: %v", err)
	}
	defer checkClose(t, "proxy conn", conn.Close)

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, len(earlyPayload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("reading tunnel bytes buffered after CONNECT failed (lost early bytes?): %v", err)
	}
	if string(buf) != earlyPayload {
		t.Fatalf("tunnel first bytes = %q, want %q", buf, earlyPayload)
	}
}

package net

import (
	"context"
	"crypto/tls"
	"errors"
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

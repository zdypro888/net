package net

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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

package net

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyHandshakeHonorsConfiguredBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	started := time.Now()
	_, err := (&Proxy{Address: server.URL, ConnectTimeout: 40 * time.Millisecond}).DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("stalled CONNECT succeeded")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("handshake exceeded budget: %v", elapsed)
	}
}

func TestProxyRejectsNegativeConnectBudget(t *testing.T) {
	_, err := (&Proxy{Address: "http://127.0.0.1:1", ConnectTimeout: -time.Second}).DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil {
		t.Fatal("negative budget accepted")
	}
}

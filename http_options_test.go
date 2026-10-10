package net

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 强制出口替换拨号器后仍须保留调用方的响应头预算。
func TestHTTPHeaderBudgetSurvivesDirectRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	h, err := NewHTTPWithOptions(nil, HTTPOptions{HeaderTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Dispose()
	policy, err := NewNetworkPolicy(NetworkPolicyConfig{Default: NetworkRoute{Mode: "direct", DialTimeout: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	response, err := h.RequestMethod(ContextWithNetworkPolicy(ctx, policy), server.URL, "GET", nil, nil)
	if response != nil {
		response.Close()
	}
	if err == nil || ctx.Err() != nil {
		t.Fatal("header budget ignored", err)
	}
}

// 父 context 的更短期限必须优先于所有配置阶段预算。
func TestHTTPBudgetHonorsParentDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	h, err := NewHTTPWithOptions(nil, HTTPOptions{HeaderTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Dispose()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	response, err := h.RequestMethod(ctx, server.URL, "GET", nil, nil)
	if response != nil {
		response.Close()
	}
	if err == nil || ctx.Err() != context.DeadlineExceeded {
		t.Fatal("caller deadline ignored", err)
	}
}

func TestHTTPAndRouteRejectInvalidBudgets(t *testing.T) {
	for _, options := range []HTTPOptions{{DialTimeout: -1}, {HeaderTimeout: -1}, {ExpectTimeout: -1}, {TLSHandshakeTimeout: -1}, {RequestTimeout: -1}, {IdleTimeout: -1}} {
		if h, err := NewHTTPWithOptions(nil, options); err == nil {
			h.Dispose()
			t.Fatal("invalid budget accepted")
		}
	}
	for _, route := range []NetworkRoute{{Mode: "direct", DialTimeout: -1}, {Mode: "inherit", DialTimeout: time.Second}, {Mode: "proxy", DialTimeout: time.Second, Proxy: &Proxy{Address: "http://localhost"}}} {
		if p, err := NewNetworkPolicy(NetworkPolicyConfig{Default: route}); err == nil {
			p.CloseIdleConnections()
			t.Fatal("invalid route budget accepted")
		}
	}
}

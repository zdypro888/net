package net

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
)

type ContextKey int

const (
	ContextHTTPKey ContextKey = iota
)

var ErrHTTPNotInContext = errors.New("http not in context")

type requestTransportKey struct{}

// ContextWithTransport 为当前请求链指定传输出口，不修改共享 HTTP 客户端。
// 多个调用方可在同一连接池上并发选择不同出口；认证、重定向和响应处理仍由 HTTP 负责。
func ContextWithTransport(ctx context.Context, transport http.RoundTripper) context.Context {
	return context.WithValue(ctx, requestTransportKey{}, transport)
}

func Context(ctx context.Context, h *HTTP) context.Context {
	return context.WithValue(ctx, ContextHTTPKey, h)
}

func FromContext(ctx context.Context) *HTTP {
	if hi := ctx.Value(ContextHTTPKey); hi != nil {
		h, _ := hi.(*HTTP)
		return h
	}
	return nil
}

func SetProxy(ctx context.Context, proxy func(*http.Request) (*url.URL, error), storeCache bool) error {
	if h := FromContext(ctx); h != nil {
		return h.ConfigureProxy(proxy, storeCache)
	}
	return ErrHTTPNotInContext
}

func SetProxyDial(ctx context.Context, dialContext func(ctx context.Context, network, addr string) (net.Conn, error), storeCache bool) error {
	if h := FromContext(ctx); h != nil {
		return h.ConfigureProxyDial(dialContext, storeCache)
	}
	return ErrHTTPNotInContext
}

func SetCookie(ctx context.Context, c http.CookieJar) error {
	if h := FromContext(ctx); h != nil {
		h.ConfigureCookie(c)
		return nil
	}
	return ErrHTTPNotInContext
}

func GetCookie(ctx context.Context) http.CookieJar {
	if h := FromContext(ctx); h != nil {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return h.client.Jar
	}
	return nil
}

func SetTimeout(ctx context.Context, timeout time.Duration) error {
	if h := FromContext(ctx); h != nil {
		h.ConfigureTimeout(timeout)
		return nil
	}
	return ErrHTTPNotInContext
}

func SetRedirect(ctx context.Context, r func(req *http.Request, via []*http.Request) error) error {
	if h := FromContext(ctx); h != nil {
		h.ConfigureRedirect(r)
		return nil
	}
	return ErrHTTPNotInContext
}

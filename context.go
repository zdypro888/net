package net

import (
	"context"
	"net"
	"net/http"
	"net/url"
)

type ContextKey int

const (
	ContextProxyKey  ContextKey = iota
	ContextHTTPKey   ContextKey = iota
	ContextCookieKey ContextKey = iota
)

type ProxyDelegate interface {
	DialContext(context.Context, string, string) (net.Conn, error)
	ProxyURL(*http.Request) (*url.URL, error)
	OnError(context.Context, error) error
}

func ContextWithProxy(ctx context.Context, proxy ProxyDelegate) context.Context {
	return context.WithValue(ctx, ContextProxyKey, proxy)
}

func ContextProxyValue(ctx context.Context) ProxyDelegate {
	if pi := ctx.Value(ContextProxyKey); pi != nil {
		return pi.(ProxyDelegate)
	}
	return nil
}

func ContextWithHTTP(ctx context.Context, h *HTTP) context.Context {
	return context.WithValue(ctx, ContextHTTPKey, h)
}

func ContextHTTPValue(ctx context.Context) *HTTP {
	if hi := ctx.Value(ContextHTTPKey); hi != nil {
		return hi.(*HTTP)
	}
	return nil
}

func ContextWithCookie(ctx context.Context, c http.CookieJar) context.Context {
	return context.WithValue(ctx, ContextCookieKey, c)
}

func ContextCookieValue(ctx context.Context) http.CookieJar {
	if ci := ctx.Value(ContextCookieKey); ci != nil {
		return ci.(http.CookieJar)
	}
	return nil
}

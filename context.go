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

type ProxyDelegate interface {
	DialContext(context.Context, string, string) (net.Conn, error)
	ProxyURL(*http.Request) (*url.URL, error)
	OnError(context.Context, error) error
}

func Context(ctx context.Context, h *HTTP) context.Context {
	return context.WithValue(ctx, ContextHTTPKey, h)
}

func FromContext(ctx context.Context) *HTTP {
	if hi := ctx.Value(ContextHTTPKey); hi != nil {
		return hi.(*HTTP)
	}
	return nil
}

func SetProxy(ctx context.Context, proxy ProxyDelegate) error {
	if h := FromContext(ctx); h != nil {
		h.ConfigureProxy(proxy)
		return nil
	}
	return ErrHTTPNotInContext
}

func GetProxy(ctx context.Context) ProxyDelegate {
	if h := FromContext(ctx); h != nil {
		return h.proxyDelegate
	}
	return nil
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

func SetResponse(ctx context.Context, r ResponseDelegate) error {
	if h := FromContext(ctx); h != nil {
		h.ConfigureResponse(r)
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

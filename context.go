package net

import (
	"context"
	"net"
	"net/http"
	"net/url"
)

//GobalProxy 通用代理
var GobalProxy *Proxy

type ProxyContext interface {
	context.Context
	GetProxyURL(req *http.Request) (*url.URL, error)
	Dial(network, address string) (net.Conn, error)
	GetProxyError(error) error
}

type CookieContext interface {
	context.Context
	GetCookie(url string) http.CookieJar
}

type contextProxy struct {
	context.Context
	Proxy *Proxy
}

func (ctx *contextProxy) GetProxyURL(req *http.Request) (*url.URL, error) {
	return ctx.Proxy.GetProxyURL(req)
}
func (ctx *contextProxy) Dial(network, address string) (net.Conn, error) {
	return ctx.Proxy.DialContext(ctx.Context, network, address)
}
func (ctx *contextProxy) GetProxyError(err error) error {
	return err
}

func WithProxy(proxy *Proxy) context.Context {
	return ContextWithProxy(context.Background(), proxy)
}
func ContextWithProxy(ctx context.Context, proxy *Proxy) context.Context {
	return &contextProxy{Context: ctx, Proxy: proxy}
}

type contextCookie struct {
	context.Context
	Jar http.CookieJar
}

func (ctx *contextCookie) GetCookie(url string) http.CookieJar {
	return ctx.Jar
}

func WithCookie(jar http.CookieJar) context.Context {
	return ContextWithCookie(context.Background(), jar)
}
func ContextWithCookie(ctx context.Context, jar http.CookieJar) context.Context {
	return &contextCookie{Context: ctx, Jar: jar}
}

package net

import "context"

//GobalProxy 通用代理
var GobalProxy *Proxy

type ProxyContext interface {
	context.Context
	WithProxy() *Proxy
}

type CookieContext interface {
	context.Context
	WithCookie() CookieJar
}

type Context struct {
	context.Context
	Proxy *Proxy
	Jar   CookieJar
}

func (ctx *Context) WithProxy() *Proxy {
	return ctx.Proxy
}
func (ctx *Context) WithCookie() CookieJar {
	return ctx.Jar
}

func WithProxy(proxy *Proxy) *Context {
	return ContextWithProxy(context.Background(), proxy)
}
func WithCookie(jar CookieJar) *Context {
	return ContextWithCookie(context.Background(), jar)
}
func ContextWithProxy(ctx context.Context, proxy *Proxy) *Context {
	return &Context{
		Context: ctx,
		Proxy:   proxy,
	}
}
func ContextWithCookie(ctx context.Context, jar CookieJar) *Context {
	return &Context{
		Context: ctx,
		Jar:     jar,
	}
}
func ContextWithProxyCookie(ctx context.Context, proxy *Proxy, jar CookieJar) *Context {
	return &Context{
		Context: ctx,
		Proxy:   proxy,
		Jar:     jar,
	}
}

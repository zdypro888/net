package net

import (
	"context"
	"net/http"
)

//GobalProxy 通用代理
var GobalProxy *Proxy

type ProxyContext interface {
	context.Context
	GetProxy() *Proxy
	SetProxy(*Proxy)
}

type CookieContext interface {
	context.Context
	GetCookie() http.CookieJar
	SetCookie(http.CookieJar)
}

type Context struct {
	context.Context
	Proxy *Proxy
	Jar   http.CookieJar
}

func (ctx *Context) GetProxy() *Proxy {
	return ctx.Proxy
}
func (ctx *Context) SetProxy(proxy *Proxy) {
	ctx.Proxy = proxy
}
func (ctx *Context) GetCookie() http.CookieJar {
	return ctx.Jar
}
func (ctx *Context) SetCookie(jar http.CookieJar) {
	ctx.Jar = jar
}

func WithProxy(proxy *Proxy) context.Context {
	return ContextWithProxy(context.Background(), proxy)
}
func WithCookie(jar http.CookieJar) context.Context {
	return ContextWithCookie(context.Background(), jar)
}
func ContextWithProxy(ctx context.Context, proxy *Proxy) context.Context {
	return &Context{Context: ctx, Proxy: proxy}
}
func ContextWithCookie(ctx context.Context, jar http.CookieJar) context.Context {
	return &Context{Context: ctx, Jar: jar}
}
func ContextWithProxyCookie(ctx context.Context, proxy *Proxy, jar http.CookieJar) context.Context {
	return &Context{Context: ctx, Proxy: proxy, Jar: jar}
}

package net

import (
	"context"
	"net"
	"net/http"
	"net/url"
)

type ContextDo interface {
	context.Context
	Do(*http.Response) (*http.Response, error)
}

type ContextProxy interface {
	context.Context
	GetProxyURL(req *http.Request) (*url.URL, error)
	Dial(network, address string) (net.Conn, error)
	GetProxyError(error) error
}

type ContextCookie interface {
	context.Context
	GetCookie(url string) http.CookieJar
}

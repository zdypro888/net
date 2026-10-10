package net

import (
	"errors"
	"time"
)

// HTTPOptions 是 HTTP/1、HTTP/2 客户端的本地阶段预算；零值使用兼容默认，负值拒绝。
// 阶段 deadline 不得超过请求 context/总超时，不影响 TLS 证书验证或协议重试语义。
type HTTPOptions struct {
	DialTimeout         time.Duration
	HeaderTimeout       time.Duration
	ExpectTimeout       time.Duration
	TLSHandshakeTimeout time.Duration
	RequestTimeout      time.Duration
	IdleTimeout         time.Duration
}

func (o HTTPOptions) normalized() (HTTPOptions, error) {
	if o.DialTimeout < 0 || o.HeaderTimeout < 0 || o.ExpectTimeout < 0 || o.TLSHandshakeTimeout < 0 || o.RequestTimeout < 0 || o.IdleTimeout < 0 {
		return o, errors.New("HTTP budgets must be non-negative")
	}
	if o.DialTimeout == 0 {
		o.DialTimeout = defaultHTTPDialTimeout
	}
	if o.HeaderTimeout == 0 {
		o.HeaderTimeout = defaultHTTPHeaderTimeout
	}
	if o.ExpectTimeout == 0 {
		o.ExpectTimeout = defaultHTTPExpectTimeout
	}
	if o.TLSHandshakeTimeout == 0 {
		o.TLSHandshakeTimeout = defaultHTTPTLSHandshakeTimeout
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = defaultHTTPRequestTimeout
	}
	if o.IdleTimeout == 0 {
		o.IdleTimeout = defaultHTTPIdleTimeout
	}
	return o, nil
}

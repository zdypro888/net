package net

import (
	"crypto/tls"
	"net/http"

	"github.com/quic-go/quic-go/http3"
)

func NewHTTP3(config *tls.Config) *HTTP {
	if config == nil {
		config = DefaultTLSConfig()
	}
	transport := &http3.Transport{
		TLSClientConfig: config.Clone(),
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   defaultHTTPRequestTimeout,
	}
	h := &HTTP{
		transport: transport,
		client:    client,
	}
	return h
}

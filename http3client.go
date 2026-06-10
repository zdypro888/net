package net

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/quic-go/quic-go/http3"
)

func NewHTTP3(config *tls.Config) *HTTP {
	if config == nil {
		config = DefaultTLSConfig()
	}
	transport := &http3.Transport{
		TLSClientConfig: config,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   120 * time.Second,
	}
	h := &HTTP{
		transport: transport,
		client:    client,
	}
	return h
}

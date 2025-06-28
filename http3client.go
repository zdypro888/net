package net

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/quic-go/quic-go/http3"
)

func NewHTTP3(config *tls.Config) *HTTP {
	if config == nil {
		config = &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS11,
			MaxVersion:         tls.VersionTLS13,
		}
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
	// client.CheckRedirect = h.redirect
	return h
}

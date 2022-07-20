package net

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
)

func Http3Client() *http.Client {
	roundTripper := &http3.RoundTripper{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false,
		},
		QuicConfig: &quic.Config{},
	}
	return &http.Client{Transport: roundTripper}
}

type withCloseIdleConnections struct {
	*http3.RoundTripper
}

func (transport *withCloseIdleConnections) CloseIdleConnections() {
	transport.Close()
}

func NewHTTP3() *HTTP {
	transport := &withCloseIdleConnections{
		RoundTripper: &http3.RoundTripper{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				MinVersion:         tls.VersionTLS11,
				MaxVersion:         tls.VersionTLS13,
			},
			QuicConfig: &quic.Config{},
		},
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

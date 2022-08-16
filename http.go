package net

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/lucas-clemente/quic-go/http3"
	"golang.org/x/net/http2"
)

// ResponseDelegate 结果处理
type ResponseDelegate interface {
	Response(context.Context, *http.Response) (*http.Response, error)
}

// Response 请求返回
type Response struct {
	Code   int
	Header http.Header
	Body   []byte
}

func (res *Response) Error() string {
	return fmt.Sprintf("response status code: %d", res.Code)
}

type HTTP struct {
	transport        http.RoundTripper
	client           *http.Client
	proxyDelegate    ProxyDelegate
	responseDelegate ResponseDelegate
}

func NewHTTP() *HTTP {
	transport := &http.Transport{
		Dial: (&net.Dialer{
			Timeout: 20 * time.Second,
		}).Dial,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS11,
			MaxVersion:         tls.VersionTLS13,
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

func (h *HTTP) Dispose() {
	switch transport := h.transport.(type) {
	case *http.Transport:
		transport.CloseIdleConnections()
	case *http3.RoundTripper:
		transport.Close()
	}
}

func (h *HTTP) ConfigureV2() error {
	switch transport := h.transport.(type) {
	case *http.Transport:
		return http2.ConfigureTransport(transport)
	case *http3.RoundTripper:
		return errors.New("quic protocol can not set to http2.0")
	}
	return nil
}

func (h *HTTP) ConfigureCookie(cookies http.CookieJar) {
	h.client.Jar = cookies
}

func (h *HTTP) ConfigureProxy(delegate ProxyDelegate) error {
	h.proxyDelegate = delegate
	switch transport := h.transport.(type) {
	case *http.Transport:
		transport.Proxy = h.proxyDelegate.ProxyURL
	case *http3.RoundTripper:
		return errors.New("quic protocol can not set proxy")
	}
	return nil
}

func (h *HTTP) ConfigureResponse(delegate ResponseDelegate) {
	h.responseDelegate = delegate
}

func (h *HTTP) Request(ctx context.Context, url string, headers http.Header, body io.ReadSeeker) (*Response, error) {
	var method string
	if body == nil {
		method = "GET"
	} else {
		method = "POST"
	}
	return h.RequestMethod(ctx, url, method, headers, body)
}

func (h *HTTP) RequestMethod(ctx context.Context, url string, method string, headers http.Header, body io.ReadSeeker) (*Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		request.Header = http.Header(headers).Clone()
	}
	var currentOffset int64
	if body != nil {
		currentOffset, _ = body.Seek(0, io.SeekCurrent)
	}
	if jar := ContextCookieValue(ctx); jar != nil {
		h.ConfigureCookie(jar)
	}
	var response *http.Response
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if body != nil {
			body.Seek(currentOffset, io.SeekStart)
		}
		if response, err = h.client.Do(request); err != nil {
			h.client.CloseIdleConnections()
			if h.proxyDelegate == nil {
				return nil, err
			} else if err = h.proxyDelegate.OnError(ctx, err); err != nil {
				return nil, err
			}
		} else if h.responseDelegate != nil {
			if response, err = h.responseDelegate.Response(ctx, response); err != nil {
				return nil, err
			} else if response != nil {
				break
			}
		} else {
			break
		}
	}
	defer response.Body.Close()
	data, err := ReadResponse(response)
	return &Response{
		Code:   response.StatusCode,
		Header: response.Header,
		Body:   data,
	}, err
}

// ReadResponse 读取 response body
func ReadResponse(response *http.Response) ([]byte, error) {
	var err error
	var body []byte
	switch response.Header.Get("Content-Encoding") {
	case "gzip":
		var reader *gzip.Reader
		reader, err = gzip.NewReader(response.Body)
		if err != nil {
			return nil, err
		}
		body, err = io.ReadAll(reader)
	case "br":
		reader := brotli.NewReader(response.Body)
		body, err = io.ReadAll(reader)
	default:
		body, err = io.ReadAll(response.Body)
	}
	return body, err
}

var ErrContextNotContainHTTP = errors.New("context not contain http")

func Request(ctx context.Context, url string, headers http.Header, body io.ReadSeeker) (*Response, error) {
	if h := ContextHTTPValue(ctx); h != nil {
		return h.Request(ctx, url, headers, body)
	}
	return nil, ErrContextNotContainHTTP
}

func RequestMethod(ctx context.Context, url string, method string, headers http.Header, body io.ReadSeeker) (*Response, error) {
	if h := ContextHTTPValue(ctx); h != nil {
		return h.RequestMethod(ctx, url, method, headers, body)
	}
	return nil, ErrContextNotContainHTTP
}

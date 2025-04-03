package net

import (
	"bytes"
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
	"github.com/quic-go/quic-go/http3"
	"github.com/zdypro888/utils"
	"golang.org/x/net/http2"
)

var ErrContextNotContainHTTP = errors.New("context not contain http")

// ResponseDelegate 结果处理
type ResponseDelegate interface {
	Response(context.Context, *http.Request, *http.Response) (bool, error)
}

// Response 请求返回
type Response struct {
	*http.Response
	reader io.Reader
}

func (response *Response) Error() string {
	return fmt.Sprintf("%s(%d)", response.Status, response.StatusCode)
}

func (response *Response) Read(p []byte) (int, error) {
	if response.Body == nil {
		return 0, io.EOF
	}
	if response.reader == nil {
		switch response.Header.Get("Content-Encoding") {
		case "gzip":
			var err error
			if response.reader, err = gzip.NewReader(response.Body); err != nil {
				return 0, err
			}
		case "br":
			response.reader = brotli.NewReader(response.Body)
		default:
			response.reader = response.Body
		}
	}
	return response.reader.Read(p)
}

func (response *Response) Close() error {
	if response.Body != nil {
		if response.reader != nil {
			if closer, ok := response.reader.(io.Closer); ok {
				closer.Close()
			}
		}
		return response.Body.Close()
	}
	return nil
}

func (res *Response) Data() ([]byte, error) {
	defer res.Close()
	return io.ReadAll(res)
}

func NewReader(data []byte) io.Reader {
	if len(data) == 0 {
		return nil
	}
	return bytes.NewReader(data)
}

type HTTP struct {
	transport        http.RoundTripper
	client           *http.Client
	proxyDelegate    ProxyDelegate
	responseDelegate ResponseDelegate
}

func NewHTTP(config *tls.Config) *HTTP {
	if config == nil {
		config = &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS11,
			MaxVersion:         tls.VersionTLS13,
		}
	}
	transport := &http.Transport{
		Dial: (&net.Dialer{
			Timeout: 20 * time.Second,
		}).Dial,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		TLSClientConfig:       config,
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
	case *http3.Transport:
		transport.Close()
	}
}

func (h *HTTP) ConfigureV2() error {
	switch transport := h.transport.(type) {
	case *http.Transport:
		return http2.ConfigureTransport(transport)
	case *http3.Transport:
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
	case *http3.Transport:
		return errors.New("quic protocol can not set proxy")
	}
	return nil
}

func (h *HTTP) ConfigureTimeout(timeout time.Duration) {
	h.client.Timeout = timeout
}

func (h *HTTP) ConfigureDebug() error {
	switch transport := h.transport.(type) {
	case *http.Transport:
		transport.Proxy = HTTPDebugProxy.ProxyURL
	case *http3.Transport:
		return errors.New("quic protocol can not set proxy")
	}
	return nil
}

func (h *HTTP) ConfigureResponse(delegate ResponseDelegate) {
	h.responseDelegate = delegate
}

func (h *HTTP) ConfigureRedirect(checkRedirect func(req *http.Request, via []*http.Request) error) {
	h.client.CheckRedirect = checkRedirect
}

func (h *HTTP) Request(ctx context.Context, url string, headers http.Header, body io.Reader) (*Response, error) {
	var method string
	if body == nil {
		method = "GET"
	} else {
		method = "POST"
	}
	return h.RequestMethod(ctx, url, method, headers, body)
}

func (h *HTTP) RequestMethod(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*Response, error) {
	var err error
	var retry bool
	var request *http.Request
	var response *http.Response
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if request, err = http.NewRequestWithContext(ctx, method, url, body); err != nil {
			return nil, err
		}
		if body != nil {
			switch v := body.(type) {
			case *utils.Reader:
				request.ContentLength = v.Size()
				snapshot := v.Temporary()
				request.GetBody = func() (io.ReadCloser, error) {
					return snapshot, nil
				}
			}
		}
		if headers != nil {
			request.Header = http.Header(headers).Clone()
		}
		if response, err = h.client.Do(request); err != nil {
			h.client.CloseIdleConnections()
			if h.proxyDelegate == nil {
				return nil, err
			} else if err = h.proxyDelegate.OnError(ctx, err); err != nil {
				return nil, err
			}
		} else if h.responseDelegate != nil {
			if retry, err = h.responseDelegate.Response(ctx, request, response); err != nil {
				return nil, err
			} else if !retry {
				break
			}
		} else {
			break
		}
	}
	// defer response.Body.Close()
	return &Response{Response: response}, err
}

func Request(ctx context.Context, url string, headers http.Header, body io.Reader) (*Response, error) {
	if h := FromContext(ctx); h != nil {
		return h.Request(ctx, url, headers, body)
	}
	return nil, ErrContextNotContainHTTP
}

func RequestMethod(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*Response, error) {
	if h := FromContext(ctx); h != nil {
		return h.RequestMethod(ctx, url, method, headers, body)
	}
	return nil, ErrContextNotContainHTTP
}

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
	"net/url"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/quic-go/quic-go/http3"
	"github.com/zdypro888/utils"
	"golang.org/x/net/http2"
)

var ErrContextNotContainHTTP = errors.New("context not contain http")

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
	transport  http.RoundTripper
	client     *http.Client
	proxyURL   func(*http.Request) (*url.URL, error)
	proxyDial  func(ctx context.Context, network, addr string) (net.Conn, error)
	OnResponse func(ctx context.Context, req *http.Request, res *http.Response, err error) (*http.Response, error, bool)
	AutoRetry  int
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

func (h *HTTP) ConfigureProxy(proxy func(*http.Request) (*url.URL, error), storeCache bool) error {
	if storeCache {
		h.proxyURL = proxy
	}
	switch transport := h.transport.(type) {
	case *http.Transport:
		transport.Proxy = proxy
	case *http3.Transport:
		return errors.New("quic protocol can not set proxy")
	}
	return nil
}

func (h *HTTP) ConfigureDebug() error {
	return h.ConfigureProxy(HTTPDebugProxy.ProxyURL, false)
}

func (h *HTTP) ConfigureProxyDial(dialContext func(ctx context.Context, network, addr string) (net.Conn, error), storeCache bool) error {
	if storeCache {
		h.proxyDial = dialContext
	}
	switch transport := h.transport.(type) {
	case *http.Transport:
		transport.DialContext = dialContext
	case *http3.Transport:
		return errors.New("quic protocol can not set proxy")
	}
	return nil
}

func (h *HTTP) ConfigureProxyClear() {
	switch transport := h.transport.(type) {
	case *http.Transport:
		transport.Proxy = nil
		transport.DialContext = nil
	}
}

func (h *HTTP) ConfigureProxyReset() {
	switch transport := h.transport.(type) {
	case *http.Transport:
		transport.Proxy = h.proxyURL
		transport.DialContext = h.proxyDial
	}
}

func (h *HTTP) ConfigureTimeout(timeout time.Duration) {
	h.client.Timeout = timeout
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

func (h *HTTP) requestMethodDo(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	// 扩展 body 处理
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
	response, err := h.client.Do(request)
	var closeIdleConn bool
	if h.OnResponse != nil {
		response, err, closeIdleConn = h.OnResponse(ctx, request, response, err)
	}
	if err != nil || closeIdleConn {
		h.client.CloseIdleConnections()
	}
	if err != nil {
		return nil, err
	}
	return &Response{Response: response}, nil
}

func (h *HTTP) RequestMethod(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*Response, error) {
	if h.AutoRetry == 0 {
		return h.requestMethodDo(ctx, url, method, headers, body)
	}
	var err error
	var retryReader *utils.Reader
	if body != nil {
		switch v := body.(type) {
		case *bytes.Buffer:
			retryReader, _ = utils.NewReader(v.Bytes())
		case *utils.Reader:
			retryReader = v.Temporary()
		default:
			if retryReader, err = utils.NewReader(body); err != nil {
				return h.requestMethodDo(ctx, url, method, headers, body)
			}
		}
	}
	var response *Response
	for i := h.AutoRetry; i > 0; i-- {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var reqBody io.Reader
		if retryReader != nil {
			reqBody = retryReader.Temporary()
		}
		if response, err = h.requestMethodDo(ctx, url, method, headers, reqBody); err != nil {
			continue
		} else {
			break
		}
	}
	return response, err
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

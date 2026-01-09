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

func (h *HTTP) requestWithRequest(ctx context.Context, request *http.Request) (*Response, error) {
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

// requestMethodDo 发送请求
func (h *HTTP) requestMethodDo(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		// 如果是 utils.Reader 则扩展处理
		if breader, ok := body.(*utils.Reader); ok {
			defer breader.Close()
			request.ContentLength = breader.Size()
			request.Body = breader.Temporary()
			request.GetBody = func() (io.ReadCloser, error) {
				return breader.Temporary(), nil // 快照, 用于301重定向等场景
			}
		}
	}
	if headers != nil {
		request.Header = http.Header(headers).Clone()
	}
	return h.requestWithRequest(ctx, request)
}

func (h *HTTP) RequestMethod(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*Response, error) {
	if h.AutoRetry == 0 {
		return h.requestMethodDo(ctx, url, method, headers, body)
	}
	var err error
	var breader *utils.Reader
	if body != nil {
		if bodyCloser, ok := body.(io.Closer); ok {
			defer bodyCloser.Close()
		}
		switch v := body.(type) {
		case *bytes.Buffer:
			// 应该不会发生错误, 因为 bytes.Buffer 不会出错
			if breader, err = utils.NewReader(v.Bytes()); err != nil {
				return nil, err
			}
		case *utils.Reader:
			// 创建快照用作body重试
			breader = v.Temporary()
		default:
			// 其他类型都尝试读取到内存中
			if breader, err = utils.NewReader(v); err != nil {
				// 转换 body 失败, 则不进行重试
				return h.requestMethodDo(ctx, url, method, headers, body)
			}
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, url, breader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		request.Header = http.Header(headers).Clone()
	}
	var response *Response
	for i := h.AutoRetry; i > 0; i-- {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if breader != nil {
			request.ContentLength = breader.Size()
			request.Body = breader.Temporary()
			request.GetBody = func() (io.ReadCloser, error) {
				return breader.Temporary(), nil
			}
		}
		if response, err = h.requestWithRequest(ctx, request); err != nil {
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

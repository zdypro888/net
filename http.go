package net

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/quic-go/quic-go/http3"
	"github.com/zdypro888/utils"
	"golang.org/x/net/http2"
)

// DefaultRetryBackoff 是 HTTP.RequestMethod 默认的 retry 间隔.
// 指数 + 抖动, 100ms * 2^attempt, capped 5s. attempt 从 0 起计数 (即 0 = 第一次重试).
// RUN-5 修复: 旧实现 retry 之间 0 sleep, 服务端 5xx 风暴时立即捶 N 次.
// 暴露为变量便于 caller 用 HTTP.ConfigureRetryBackoff 覆盖.
func DefaultRetryBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := time.Duration(100*(1<<uint(attempt))) * time.Millisecond
	if base > 5*time.Second {
		base = 5 * time.Second
	}
	// 加 0~25% jitter, 防止多 client 同步 retry 雷暴.
	jitter := time.Duration(rand.Int64N(int64(base / 4)))
	return base + jitter
}

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
		var err error
		if response.reader != nil {
			if closer, ok := response.reader.(*gzip.Reader); ok {
				err = closer.Close()
			}
		}
		return errors.Join(err, response.Body.Close())
	}
	return nil
}

func (res *Response) Data() (data []byte, err error) {
	defer func() {
		err = errors.Join(err, res.Close())
	}()
	return io.ReadAll(res)
}

func NewReader(data []byte) io.Reader {
	if len(data) == 0 {
		return nil
	}
	return bytes.NewReader(data)
}

type HTTP struct {
	transport    http.RoundTripper
	client       *http.Client
	proxyURL     func(*http.Request) (*url.URL, error)
	proxyDial    func(ctx context.Context, network, addr string) (net.Conn, error)
	retryBackoff func(attempt int) time.Duration
	// OnResponse / AutoRetry 保留为导出字段以兼容下游 (iauth/imadrid 直接赋值);
	// 推荐用 Configure* setter. 必须在调用 Request 前设置好, 之后只读.
	OnResponse func(ctx context.Context, req *http.Request, res *http.Response, err error) (*http.Response, error, bool)
	AutoRetry  int
}

// ConfigureOnResponse 设置响应回调 (推荐用法). 与直接赋值 HTTP.OnResponse 等价.
func (h *HTTP) ConfigureOnResponse(fn func(ctx context.Context, req *http.Request, res *http.Response, err error) (*http.Response, error, bool)) {
	h.OnResponse = fn
}

// ConfigureAutoRetry 设置自动重试次数 (推荐用法).
func (h *HTTP) ConfigureAutoRetry(n int) {
	h.AutoRetry = n
}

// ConfigureRetryBackoff 设置 retry 间隔. fn=nil 时恢复默认 DefaultRetryBackoff.
// fn 在 init 期设, retry path 只读, 无并发保护.
func (h *HTTP) ConfigureRetryBackoff(fn func(attempt int) time.Duration) {
	h.retryBackoff = fn
}

// DefaultTLSConfig 返回 NewHTTP/NewHTTP3 在 caller 传 nil 时使用的默认 TLS 配置.
// 项目历史默认是跳过证书校验; 需要严格校验时显式传 StrictTLSConfig().
func DefaultTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS11,
		MaxVersion:         tls.VersionTLS13,
	}
}

// StrictTLSConfig 返回显式严格校验的 TLS 配置.
func StrictTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
	}
}

func NewHTTP(config *tls.Config) *HTTP {
	if config == nil {
		config = DefaultTLSConfig()
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
		if err := transport.Close(); err != nil {
			slog.Warn("net.HTTP Dispose http3 transport close failed", slog.Any("err", err))
		}
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
func (h *HTTP) requestMethodDo(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (response *Response, err error) {
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		// 如果是 utils.Reader 则扩展处理
		if breader, ok := body.(*utils.Reader); ok {
			defer func() {
				err = errors.Join(err, breader.Close())
			}()
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

func (h *HTTP) RequestMethod(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (response *Response, err error) {
	// AutoRetry <= 0 表示不重试, 单发一次. 旧实现只判 ==0, 负值会落进下方
	// for i:=total;i>0 循环体一次不执行, 返回 (nil,nil) 让 caller NPE; 这里收敛.
	if h.AutoRetry <= 0 {
		return h.requestMethodDo(ctx, url, method, headers, body)
	}
	var breader *utils.Reader
	if body != nil {
		if bodyCloser, ok := body.(io.Closer); ok {
			defer func() {
				err = errors.Join(err, bodyCloser.Close())
			}()
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
		body = breader
		defer func() {
			err = errors.Join(err, breader.Close())
		}()
	}
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		request.Header = http.Header(headers).Clone()
	}
	backoff := h.retryBackoff
	if backoff == nil {
		backoff = DefaultRetryBackoff
	}
	total := h.AutoRetry
	for i := total; i > 0; i-- {
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
			// RUN-5: 还有可重试次数才 sleep; 最后一次失败不 sleep 直接返回.
			if i > 1 {
				attempt := total - i // 0,1,2...
				delay := backoff(attempt)
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return nil, ctx.Err()
				}
			}
			continue
		} else {
			break
		}
	}
	if err != nil {
		// OPS-2: retry 耗尽才 warn 一次, 避免 retry 中途风暴 log.
		slog.Warn("net.HTTP RequestMethod exhausted retries",
			slog.String("method", method),
			slog.String("url", url),
			slog.Int("retries", total),
			slog.Any("err", err))
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

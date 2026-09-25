package net

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/quic-go/quic-go/http3"
	"github.com/zdypro888/utils"
)

// DefaultRetryBackoff 是 HTTP.RequestMethod 默认的 retry 间隔.
// 指数 + 抖动, 100ms * 2^attempt, capped 5s. attempt 从 0 起计数 (即 0 = 第一次重试).
// RUN-5 修复: 旧实现 retry 之间 0 sleep, 服务端 5xx 风暴时立即捶 N 次.
// 可通过 HTTP.ConfigureRetryBackoff 覆盖。
func DefaultRetryBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	const maxShiftBeforeCap = 6 // 100ms<<6 = 6.4s, the first value capped to 5s.
	var base time.Duration
	if attempt >= maxShiftBeforeCap {
		base = 5 * time.Second
	} else {
		base = time.Duration(100*(1<<uint(attempt))) * time.Millisecond
	}
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
	reader        io.Reader
	readerErr     error
	closeDecoders []func()
	readMu        sync.Mutex
	closed        atomic.Bool
	closeOnce     sync.Once
	closeErr      error
}

func (response *Response) Error() string {
	return fmt.Sprintf("%s(%d)", response.Status, response.StatusCode)
}

func (response *Response) Read(p []byte) (int, error) {
	response.readMu.Lock()
	defer func() {
		if response.closed.Load() {
			response.releaseDecoders()
		}
		response.readMu.Unlock()
	}()
	if response.closed.Load() {
		return 0, http.ErrBodyReadAfterClose
	}
	if response.Body == nil {
		return 0, io.EOF
	}
	if response.readerErr != nil {
		return 0, response.readerErr
	}
	if response.reader == nil {
		reader, err := response.decodeBody()
		if err != nil {
			response.releaseDecoders()
			response.readerErr = err
			return 0, err
		}
		response.reader = reader
	}
	return response.reader.Read(p)
}

// decodeBody 依 RFC 9110 逆序移除 Content-Encoding，调用方持有 readMu。
// 与调用方实际发送的 gzip/deflate/br/zstd 协商列表保持一致，未知编码返回明确错误。
func (response *Response) decodeBody() (io.Reader, error) {
	var reader io.Reader = response.Body
	encodings := strings.Split(strings.Join(response.Header.Values("Content-Encoding"), ","), ",")
	for i := len(encodings) - 1; i >= 0; i-- {
		switch encoding := strings.ToLower(strings.TrimSpace(encodings[i])); encoding {
		case "", "identity":
		case "gzip", "x-gzip":
			decoded, err := gzip.NewReader(reader)
			if err != nil {
				return nil, err
			}
			response.closeDecoders = append(response.closeDecoders, func() { _ = decoded.Close() })
			reader = decoded
		case "deflate":
			decoded, err := zlib.NewReader(reader)
			if err != nil {
				return nil, err
			}
			response.closeDecoders = append(response.closeDecoders, func() { _ = decoded.Close() })
			reader = decoded
		case "br":
			reader = brotli.NewReader(reader)
		case "zstd":
			// 每个响应同步解码，避免为普通 HTTP 流额外启动后台解码 worker。
			decoded, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1))
			if err != nil {
				return nil, err
			}
			response.closeDecoders = append(response.closeDecoders, decoded.Close)
			reader = decoded
		default:
			return nil, fmt.Errorf("unsupported HTTP content encoding %q", encoding)
		}
	}
	return reader, nil
}

// releaseDecoders 必须在读取结束后或读锁内调用，不能让 Close 与解码器内部状态竞争。
func (response *Response) releaseDecoders() {
	for i := len(response.closeDecoders) - 1; i >= 0; i-- {
		response.closeDecoders[i]()
	}
	response.closeDecoders = nil
	response.reader = nil
}

// Close 可与 Read 并发，先关闭底层 Body 解除阻塞；不能并发修改 gzip/ Brotli 解码状态。
func (response *Response) Close() error {
	response.closeOnce.Do(func() {
		response.closed.Store(true)
		if response.Body != nil {
			response.closeErr = response.Body.Close()
		}
		// 正在读取时由 Read 收尾释放解码器，Close 无需等待被阻塞的 Read。
		if response.readMu.TryLock() {
			response.releaseDecoders()
			response.readMu.Unlock()
		}
	})
	return response.closeErr
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

func safeURLForLog(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func safeErrorForLog(requestURL string, err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if requestURL != "" {
		msg = strings.ReplaceAll(msg, requestURL, safeURLForLog(requestURL))
	}
	if urlErr, ok := errors.AsType[*url.Error](err); ok && urlErr.URL != "" {
		msg = strings.ReplaceAll(msg, urlErr.URL, safeURLForLog(urlErr.URL))
	}
	return msg
}

// HTTP 的 Configure 方法可与请求并发；每次请求持有独立配置快照。
// 调用方传入的 CookieJar、TLS 回调和拨号函数仍须满足各自的并发契约。
type HTTP struct {
	mu        sync.RWMutex
	transport http.RoundTripper
	client    *http.Client
	// baseDial 是 NewHTTP 创建时的基础拨号函数 (20s 拨号超时, 响应 ctx 取消/deadline).
	// ConfigureProxyClear / ConfigureProxyReset(未存代理拨号时) 恢复到它, 保证清除代理后
	// 拨号超时语义不丢. NewHTTP3 (http3.Transport) 不走 DialContext, 该字段为 nil 且不被使用.
	baseDial     func(ctx context.Context, network, addr string) (net.Conn, error)
	proxyURL     func(*http.Request) (*url.URL, error)
	proxyDial    func(ctx context.Context, network, addr string) (net.Conn, error)
	retryBackoff func(attempt int) time.Duration
	// OnResponse / AutoRetry 保留为导出字段以兼容旧调用；
	// 推荐用 Configure* setter. 必须在调用 Request 前设置好, 之后只读.
	OnResponse func(ctx context.Context, req *http.Request, res *http.Response, err error) (*http.Response, error, bool)
	AutoRetry  int
}

// ConfigureOnResponse 设置响应回调 (推荐用法). 与直接赋值 HTTP.OnResponse 等价.
func (h *HTTP) ConfigureOnResponse(fn func(ctx context.Context, req *http.Request, res *http.Response, err error) (*http.Response, error, bool)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.OnResponse = fn
}

// ConfigureAutoRetry 设置最多尝试次数（包含第一次）；<=0 表示只发送一次。
func (h *HTTP) ConfigureAutoRetry(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.AutoRetry = n
}

// ConfigureRetryBackoff 设置 retry 间隔. fn=nil 时恢复默认 DefaultRetryBackoff.
// 每次请求固定使用开始时的退避配置。
func (h *HTTP) ConfigureRetryBackoff(fn func(attempt int) time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retryBackoff = fn
}

// DefaultTLSConfig 默认验证证书链与主机名；调试代理须显式配置受信任 CA。
func DefaultTLSConfig() *tls.Config {
	return StrictTLSConfig()
}

// StrictTLSConfig 返回验证服务器身份且至少使用 TLS 1.2 的配置。
func StrictTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

const (
	defaultHTTPDialTimeout         = 20 * time.Second
	defaultHTTPHeaderTimeout       = 20 * time.Second
	defaultHTTPExpectTimeout       = 5 * time.Second
	defaultHTTPTLSHandshakeTimeout = 30 * time.Second
	defaultHTTPRequestTimeout      = 120 * time.Second
	defaultHTTPIdleTimeout         = 90 * time.Second // 与标准库默认连接池的空闲回收窗口一致。
)

// NewHTTP 创建独立连接池，nil TLS 配置使用安全默认值。
func NewHTTP(config *tls.Config) *HTTP {
	if config == nil {
		config = DefaultTLSConfig()
	}
	// 基础拨号用 DialContext (而非废弃的 Transport.Dial): 拨号阶段同样响应
	// ctx 取消与 deadline, 保留原 20s 拨号超时语义.
	baseDial := (&net.Dialer{
		Timeout: defaultHTTPDialTimeout,
	}).DialContext
	transport := &http.Transport{
		DialContext:           baseDial,
		ResponseHeaderTimeout: defaultHTTPHeaderTimeout,
		ExpectContinueTimeout: defaultHTTPExpectTimeout,
		TLSHandshakeTimeout:   defaultHTTPTLSHandshakeTimeout,
		TLSClientConfig:       config.Clone(),
		IdleConnTimeout:       defaultHTTPIdleTimeout,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   defaultHTTPRequestTimeout,
	}
	h := &HTTP{
		transport: transport,
		client:    client,
		baseDial:  baseDial,
	}
	return h
}

// Dispose 关闭当前连接池中的空闲连接；HTTP/3 会关闭整个 QUIC transport。
func (h *HTTP) Dispose() {
	h.mu.RLock()
	transport := h.transport
	h.mu.RUnlock()
	switch transport := transport.(type) {
	case *http.Transport:
		transport.CloseIdleConnections()
	case *http3.Transport:
		if err := transport.Close(); err != nil {
			slog.Warn("net.HTTP Dispose http3 transport close failed", slog.Any("err", err))
		}
	}
}

// configureTransport 在副本上修改配置后替换连接池，防止改写正在被请求使用的 Transport。
// 旧请求继续使用原配置，后续请求不会复用代理切换前的连接。
// 调用方须持有 h.mu 写锁；fn 出错时不发布任何配置。
func (h *HTTP) configureTransport(fn func(*http.Transport) error) error {
	transport, ok := h.transport.(*http.Transport)
	if !ok {
		return errors.New("HTTP transport does not support TCP configuration")
	}
	replacement := transport.Clone()
	if fn != nil {
		if err := fn(replacement); err != nil {
			return err
		}
	}
	h.transport = replacement
	h.client.Transport = replacement
	transport.CloseIdleConnections()
	return nil
}

// ResetConnections 为后续请求换用全新连接池；已发出的请求继续完成。
func (h *HTTP) ResetConnections() error {
	if h == nil {
		return errors.New("HTTP client is unavailable")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.configureTransport(nil)
}

// ConfigureV2 启用标准库的 HTTP/2 协商，避免第三方 TLSNextProto 闭包跨池共享连接。
func (h *HTTP) ConfigureV2() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.configureTransport(func(t *http.Transport) error {
		t.ForceAttemptHTTP2 = true
		return nil
	})
}

func (h *HTTP) ConfigureCookie(cookies http.CookieJar) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.client.Jar = cookies
}

func (h *HTTP) ConfigureProxy(proxy func(*http.Request) (*url.URL, error), storeCache bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.configureTransport(func(t *http.Transport) error { t.Proxy = proxy; return nil }); err != nil {
		return err
	}
	if storeCache {
		h.proxyURL = proxy
	}
	return nil
}

func (h *HTTP) ConfigureDebug() error {
	return h.ConfigureProxy(HTTPDebugProxy.ProxyURL, false)
}

func (h *HTTP) ConfigureProxyDial(dialContext func(context.Context, string, string) (net.Conn, error), storeCache bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.configureTransport(func(t *http.Transport) error {
		t.DialContext = dialContext
		if t.DialContext == nil {
			t.DialContext = h.baseDial
		}
		return nil
	}); err != nil {
		return err
	}
	if storeCache {
		h.proxyDial = dialContext
	}
	return nil
}

// ConfigureProxyClear 暂停代理并更换连接池，保留缓存以供 ConfigureProxyReset 恢复。
func (h *HTTP) ConfigureProxyClear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = h.configureTransport(func(t *http.Transport) error {
		t.Proxy, t.DialContext = nil, h.baseDial
		return nil
	})
}

// ConfigureProxyReset 以新连接池恢复缓存的代理配置。
func (h *HTTP) ConfigureProxyReset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = h.configureTransport(func(t *http.Transport) error {
		t.Proxy, t.DialContext = h.proxyURL, h.proxyDial
		if t.DialContext == nil {
			t.DialContext = h.baseDial
		}
		return nil
	})
}

func (h *HTTP) ConfigureTimeout(timeout time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.client.Timeout = timeout
}

// ConfigureResponseHeaderTimeout 设置后续请求等待响应头的上限。
func (h *HTTP) ConfigureResponseHeaderTimeout(timeout time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = h.configureTransport(func(t *http.Transport) error { t.ResponseHeaderTimeout = timeout; return nil })
}

func (h *HTTP) ConfigureRedirect(checkRedirect func(*http.Request, []*http.Request) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
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

// RequestMethod 保留 AutoRetry 的历史含义：正数为最多尝试次数，非正数为一次。
// 自动重发仅限可重放的幂等请求；非幂等操作应由业务方明确判断结果后重试。
// 每次尝试使用独立 Request/Body，避免 Transport 异步关闭旧 body 时污染下一次发送。
func (h *HTTP) RequestMethod(ctx context.Context, rawURL, method string, headers http.Header, body io.Reader) (response *Response, err error) {
	h.mu.RLock()
	client := *h.client
	total, backoff, onResponse := h.AutoRetry, h.retryBackoff, h.OnResponse
	h.mu.RUnlock()
	if transport, ok := ctx.Value(requestTransportKey{}).(http.RoundTripper); ok && transport != nil {
		client.Transport = transport
	}
	client.Transport = NetworkTransport(ctx, client.Transport)
	if total <= 0 {
		total = 1
	}
	request, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		request.Header = headers.Clone()
	}
	if reader, ok := body.(*utils.Reader); ok {
		// utils.Temporary 默认从起点开始；HTTP body 必须从传入 reader 的当前位置发送。
		start, remaining := reader.Size()-reader.UnLen(), reader.UnLen()
		request.ContentLength = remaining
		request.GetBody = func() (io.ReadCloser, error) {
			if remaining == 0 {
				return http.NoBody, nil
			}
			snapshot := reader.Temporary()
			_, err := snapshot.Seek(start, io.SeekStart)
			return snapshot, err
		}
		if request.Body, err = request.GetBody(); err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, reader.Close()) }()
	}
	// 无 GetBody 的流保持单次流式发送，不能猜测 seek 能力或无界读入内存。
	if request.Body != nil && request.Body != http.NoBody && request.GetBody == nil {
		total = 1
	}
	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
	default:
		if _, idempotent := request.Header["Idempotency-Key"]; !idempotent {
			if _, idempotent = request.Header["X-Idempotency-Key"]; !idempotent {
				total = 1
			}
		}
	}
	if backoff == nil {
		backoff = DefaultRetryBackoff
	}
	for attempt := 0; attempt < total; attempt++ {
		current := request.Clone(ctx)
		if attempt > 0 && request.GetBody != nil {
			if current.Body, err = request.GetBody(); err != nil {
				return nil, err
			}
		}
		result, requestErr := client.Do(current)
		original := result
		closeIdle := false
		if onResponse != nil {
			result, requestErr, closeIdle = onResponse(ctx, current, result, requestErr)
		}
		if result == nil && requestErr == nil {
			requestErr = errors.New("HTTP response callback returned neither response nor error")
		}
		if requestErr != nil {
			// 回调把成功响应转换成错误时，库仍负责释放该次响应，不能把连接占到重试结束。
			if original != nil && original.Body != nil {
				requestErr = errors.Join(requestErr, original.Body.Close())
			}
			if result != nil && result != original && result.Body != nil {
				requestErr = errors.Join(requestErr, result.Body.Close())
			}
		}
		if requestErr != nil || closeIdle {
			client.CloseIdleConnections()
		}
		if requestErr == nil {
			return &Response{Response: result}, nil
		}
		err = requestErr
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if attempt+1 < total {
			timer := time.NewTimer(backoff(attempt))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
		}
	}
	if total > 1 {
		slog.Warn("net.HTTP RequestMethod exhausted retries",
			slog.String("method", request.Method), slog.String("url", safeURLForLog(rawURL)),
			slog.Int("attempts", total), slog.String("err", safeErrorForLog(rawURL, err)))
	}
	return nil, err
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

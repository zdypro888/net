package net

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"io"

	"errors"
	"github.com/zdypro888/utils"
	gonet "net"
	"net/http"
	"net/http/httptest"
	stdurl "net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSafeURLForLogRedactsSensitiveParts(t *testing.T) {
	got := safeURLForLog("https://user:pass@example.com:8443/path/to?q=token&signature=secret#fragment")
	want := "https://example.com:8443/path/to"
	if got != want {
		t.Fatalf("safeURLForLog() = %q, want %q", got, want)
	}
}

func TestSafeErrorForLogRedactsURLErrorURL(t *testing.T) {
	rawURL := "https://user:pass@example.com/path?q=token&signature=secret#fragment"
	got := safeErrorForLog(rawURL, &stdurl.Error{
		Op:  "Get",
		URL: rawURL,
		Err: errors.New("dial failed"),
	})
	for _, secret := range []string{"user:pass", "token", "signature=secret", "fragment"} {
		if strings.Contains(got, secret) {
			t.Fatalf("safeErrorForLog() leaked %q in %q", secret, got)
		}
	}
	if !strings.Contains(got, "https://example.com/path") {
		t.Fatalf("safeErrorForLog() = %q, want sanitized URL", got)
	}
}

func TestDefaultRetryBackoffLargeAttemptDoesNotPanic(t *testing.T) {
	for _, attempt := range []int{6, 60, 1000} {
		got := DefaultRetryBackoff(attempt)
		if got < 5*time.Second || got >= 6250*time.Millisecond {
			t.Fatalf("DefaultRetryBackoff(%d) = %v, want [5s, 6.25s)", attempt, got)
		}
	}
}

// TestRequestMethodNegativeAutoRetryDoesNotReturnNilNil 是 D-P2-2 的回归测试.
// 旧实现 RequestMethod 仅判 AutoRetry==0; 负值会落进 for i:=total;i>0 循环体
// 一次不执行的分支, 返回 (nil, nil) 让 caller 拿到既无响应又无错误的状态而 NPE.
// 修复后 AutoRetry<=0 都按单发处理, 必须返回非 nil response 或非 nil error.
func TestRequestMethodNegativeAutoRetryDoesNotReturnNilNil(t *testing.T) {
	payloads := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		payloads <- string(data)
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, "response")
		_ = zw.Close()
	}))
	defer server.Close()

	for _, tc := range []struct {
		name, method          string
		body                  func() io.Reader
		retries, wantAttempts int
		idempotent            bool
		wantBody              string
	}{
		{name: "negative", method: http.MethodGet, retries: -1, wantAttempts: 1},
		{name: "get retry", method: http.MethodGet, retries: 3, wantAttempts: 3},
		{name: "bytes offset", method: http.MethodPut, retries: 3, wantAttempts: 3, wantBody: "payload", body: func() io.Reader {
			r := bytes.NewReader([]byte("prefixpayload"))
			_, _ = r.Seek(6, io.SeekStart)
			return r
		}},
		{name: "string offset", method: http.MethodPut, retries: 3, wantAttempts: 3, wantBody: "payload", body: func() io.Reader {
			r := strings.NewReader("prefixpayload")
			_, _ = r.Seek(6, io.SeekStart)
			return r
		}},
		{name: "utils offset", method: http.MethodPut, retries: 3, wantAttempts: 3, wantBody: "payload", body: func() io.Reader {
			r, _ := utils.NewReader([]byte("prefixpayload"))
			_, _ = r.Seek(6, io.SeekStart)
			return r
		}},
		{name: "stream sent once", method: http.MethodPut, retries: 3, wantAttempts: 1, wantBody: "payload", body: func() io.Reader {
			return io.NopCloser(strings.NewReader("payload"))
		}},
		{name: "post not repeated", method: http.MethodPost, retries: 3, wantAttempts: 1, wantBody: "payload", body: func() io.Reader { return strings.NewReader("payload") }},
		{name: "idempotency key", method: http.MethodPost, retries: 3, wantAttempts: 3, idempotent: true, wantBody: "payload", body: func() io.Reader { return strings.NewReader("payload") }},
	} {
		h := NewHTTP(nil)
		h.ConfigureAutoRetry(tc.retries)
		h.ConfigureRetryBackoff(func(int) time.Duration { return 0 })
		attempts := 0
		var rejected *http.Response
		h.ConfigureOnResponse(func(_ context.Context, _ *http.Request, res *http.Response, err error) (*http.Response, error, bool) {
			if err != nil {
				return res, err, false
			}
			attempts++
			if tc.retries > 0 && attempts < 3 {
				rejected = res
				return nil, errors.New("retryable response"), false
			}
			return res, nil, false
		})
		var body io.Reader
		if tc.body != nil {
			body = tc.body()
		}
		headers := make(http.Header)
		headers.Set("Accept-Encoding", "gzip") // 显式编码由 Response 解压，覆盖包装器自身。
		if tc.idempotent {
			headers.Set("Idempotency-Key", "test-key")
		}
		res, err := h.RequestMethod(context.Background(), server.URL, tc.method, headers, body)
		if res == nil && err == nil {
			t.Fatalf("%s: returned nil, nil", tc.name)
		}
		if res != nil {
			data, readErr := res.Data()
			if readErr != nil || string(data) != "response" {
				t.Errorf("%s: decoded body=%q error=%v", tc.name, data, readErr)
			}
			if _, readErr := res.Read(make([]byte, 1)); !errors.Is(readErr, http.ErrBodyReadAfterClose) {
				t.Errorf("%s: read after Close=%v", tc.name, readErr)
			}
		}
		if attempts != tc.wantAttempts {
			t.Errorf("%s: attempts=%d, want %d", tc.name, attempts, tc.wantAttempts)
		}
		for range attempts {
			if got := <-payloads; got != tc.wantBody {
				t.Errorf("%s: body=%q, want %q", tc.name, got, tc.wantBody)
			}
		}
		if rejected != nil {
			if _, readErr := rejected.Body.Read(make([]byte, 1)); readErr == nil {
				t.Errorf("%s: rejected response body remains open", tc.name)
			}
			_ = rejected.Body.Close()
		}
		h.Dispose()
	}
	// 响应钩子不能制造表面成功但内部 Response 为 nil 的结果。
	h := NewHTTP(nil)
	defer h.Dispose()
	h.ConfigureOnResponse(func(context.Context, *http.Request, *http.Response, error) (*http.Response, error, bool) {
		return nil, nil, false
	})
	res, err := h.Request(context.Background(), server.URL, nil, nil)
	if res != nil || err == nil {
		t.Fatalf("invalid response callback returned response=%v err=%v", res, err)
	}
	<-payloads
	// 在 gzip 初始化/读取阻塞时 Close 必须中断底层 IO，且不能与解码器初始化竞争。
	pr, pw := io.Pipe()
	defer pw.Close()
	wrapped := &Response{Response: &http.Response{Body: pr, Header: http.Header{"Content-Encoding": {"gzip"}}}}
	readDone := make(chan error, 1)
	go func() { _, err := wrapped.Read(make([]byte, 1)); readDone <- err }()
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write([]byte("payload"))
	_ = zw.Close()
	if _, err := pw.Write(compressed.Bytes()[:10]); err != nil {
		t.Fatal(err)
	}
	_ = wrapped.Close()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("blocked gzip read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt blocked gzip read")
	}

	for _, encoding := range []string{"identity", "gzip", "deflate", "br", "zstd", "gzip, br", "ZSTD, GZip"} {
		want := []byte("content encoding round trip")
		data := want
		for _, coding := range strings.Split(encoding, ",") {
			var out bytes.Buffer
			var writer io.WriteCloser
			switch strings.ToLower(strings.TrimSpace(coding)) {
			case "identity":
				continue
			case "gzip":
				writer = gzip.NewWriter(&out)
			case "deflate":
				writer = zlib.NewWriter(&out)
			case "br":
				writer = brotli.NewWriter(&out)
			case "zstd":
				var err error
				writer, err = zstd.NewWriter(&out, zstd.WithEncoderConcurrency(1))
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := writer.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			data = append([]byte(nil), out.Bytes()...)
		}
		res := &Response{Response: &http.Response{Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{"Content-Encoding": {encoding}}}}
		got, err := res.Data()
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("encoding %q: data=%q err=%v", encoding, got, err)
		}
	}

}

// TestConfigureProxyDialClearResetPreservesBaseDial 回归 B1:
// 旧实现 NewHTTP 用废弃的 Transport.Dial 做基础拨号 (拨号期不响应 ctx),
// ConfigureProxyClear 把 DialContext 置 nil 后靠 Dial 兜底. 迁移到 DialContext
// 后必须保证: Clear 恢复基础拨号 (而非 nil/丢失 20s 超时), Reset 在有缓存代理
// 拨号时还原代理拨号、无缓存时回基础拨号.
func TestConfigureProxyDialClearResetPreservesBaseDial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	h := NewHTTP(nil)
	defer h.Dispose()
	transport, ok := h.transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", h.transport)
	}
	//lint:ignore SA1019 回归断言: NewHTTP 不应再设置废弃的 Transport.Dial
	if transport.Dial != nil { //nolint:staticcheck // 同上, 仅作回归断言
		t.Fatal("NewHTTP still sets deprecated Transport.Dial")
	}
	if transport.DialContext == nil {
		t.Fatal("NewHTTP did not set Transport.DialContext")
	}

	var proxyDials atomic.Int32
	countingDial := func(ctx context.Context, network, addr string) (gonet.Conn, error) {
		proxyDials.Add(1)
		return (&gonet.Dialer{Timeout: 20 * time.Second}).DialContext(ctx, network, addr)
	}
	if err := h.ConfigureProxyDial(countingDial, true); err != nil {
		t.Fatalf("ConfigureProxyDial failed: %v", err)
	}
	doRequest := func(stage string) {
		res, err := h.Request(context.Background(), server.URL, nil, nil)
		if err != nil {
			t.Fatalf("%s: request failed: %v", stage, err)
		}
		if _, err := res.Data(); err != nil {
			t.Fatalf("%s: read body failed: %v", stage, err)
		}
	}

	doRequest("proxy dial active")
	if proxyDials.Load() != 1 {
		t.Fatalf("proxy dials after ConfigureProxyDial = %d, want 1", proxyDials.Load())
	}

	h.ConfigureProxyClear()
	if transport.DialContext == nil {
		t.Fatal("ConfigureProxyClear left DialContext nil; base dial (20s timeout) lost")
	}
	doRequest("after clear")
	if proxyDials.Load() != 1 {
		t.Fatalf("proxy dials after ConfigureProxyClear = %d, want 1 (base dial expected)", proxyDials.Load())
	}

	h.ConfigureProxyReset()
	doRequest("after reset")
	if proxyDials.Load() != 2 {
		t.Fatalf("proxy dials after ConfigureProxyReset = %d, want 2 (cached proxy dial restored)", proxyDials.Load())
	}

	// 无缓存代理拨号时 Reset 回基础拨号, 与 Clear 等价.
	h.proxyDial = nil
	h.ConfigureProxyReset()
	if transport.DialContext == nil {
		t.Fatal("ConfigureProxyReset with no cached proxy dial left DialContext nil")
	}
	doRequest("after reset without cache")
	if proxyDials.Load() != 2 {
		t.Fatalf("proxy dials after Reset without cache = %d, want 2", proxyDials.Load())
	}
}

func TestResetConnectionsReplacesPoolAndPreservesDialer(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	h := NewHTTP(server.Client().Transport.(*http.Transport).TLSClientConfig)
	if err := h.ConfigureV2(); err != nil {
		t.Fatal(err)
	}
	defer h.Dispose()
	var dials atomic.Int32
	dial := func(ctx context.Context, network, addr string) (gonet.Conn, error) {
		dials.Add(1)
		return (&gonet.Dialer{Timeout: 20 * time.Second}).DialContext(ctx, network, addr)
	}
	if err := h.ConfigureProxyDial(dial, true); err != nil {
		t.Fatalf("configure dialer: %v", err)
	}
	request := func() {
		response, err := h.Request(context.Background(), server.URL, nil, nil)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if response.ProtoMajor != 2 {
			t.Errorf("protocol=%s, want HTTP/2", response.Proto)
		}
		if _, err := response.Data(); err != nil {
			t.Fatalf("read response: %v", err)
		}
	}

	request()
	request()
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials before reset = %d, want 1", got)
	}
	if err := h.ResetConnections(); err != nil {
		t.Fatalf("reset connections: %v", err)
	}
	request()
	if got := dials.Load(); got != 2 {
		t.Fatalf("dials after reset = %d, want 2", got)
	}
	// 配置更新与请求同时进行：旧连接完成当前请求，新请求取得一致的配置快照。
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 12 {
				res, err := h.Request(context.Background(), server.URL, nil, nil)
				if err != nil {
					t.Errorf("concurrent request: %v", err)
					return
				}
				if _, err := res.Data(); err != nil {
					t.Errorf("concurrent body: %v", err)
					return
				}
			}
		})
	}
	workers.Go(func() {
		for range 12 {
			h.ConfigureTimeout(5 * time.Second)
			h.ConfigureOnResponse(nil)
			h.ConfigureRetryBackoff(nil)
			h.ConfigureAutoRetry(1)
			h.ConfigureProxyClear()
			h.ConfigureProxyReset()
			if err := h.ResetConnections(); err != nil {
				t.Errorf("concurrent reset: %v", err)
				return
			}
		}
	})
	workers.Wait()

}

package net

import (
	"context"
	"errors"
	gonet "net"
	"net/http"
	"net/http/httptest"
	stdurl "net/url"
	"strings"
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	h := NewHTTP(nil)
	defer h.Dispose()
	h.AutoRetry = -1

	resp, err := h.RequestMethod(context.Background(), server.URL, http.MethodGet, nil, nil)
	if err == nil && resp == nil {
		t.Fatalf("RequestMethod with negative AutoRetry returned (nil, nil); caller would NPE")
	}
	if resp != nil {
		if err := resp.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
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

	var proxyDials int
	countingDial := func(ctx context.Context, network, addr string) (gonet.Conn, error) {
		proxyDials++
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
		h.client.CloseIdleConnections() // 强制下次请求重新拨号
	}

	doRequest("proxy dial active")
	if proxyDials != 1 {
		t.Fatalf("proxy dials after ConfigureProxyDial = %d, want 1", proxyDials)
	}

	h.ConfigureProxyClear()
	if transport.DialContext == nil {
		t.Fatal("ConfigureProxyClear left DialContext nil; base dial (20s timeout) lost")
	}
	doRequest("after clear")
	if proxyDials != 1 {
		t.Fatalf("proxy dials after ConfigureProxyClear = %d, want 1 (base dial expected)", proxyDials)
	}

	h.ConfigureProxyReset()
	doRequest("after reset")
	if proxyDials != 2 {
		t.Fatalf("proxy dials after ConfigureProxyReset = %d, want 2 (cached proxy dial restored)", proxyDials)
	}

	// 无缓存代理拨号时 Reset 回基础拨号, 与 Clear 等价.
	h.proxyDial = nil
	h.ConfigureProxyReset()
	if transport.DialContext == nil {
		t.Fatal("ConfigureProxyReset with no cached proxy dial left DialContext nil")
	}
	doRequest("after reset without cache")
	if proxyDials != 2 {
		t.Fatalf("proxy dials after Reset without cache = %d, want 2", proxyDials)
	}
}

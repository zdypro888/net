package net

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	stdurl "net/url"
	"strings"
	"testing"
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

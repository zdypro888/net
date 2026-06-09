package net

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

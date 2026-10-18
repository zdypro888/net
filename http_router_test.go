package net

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fixtureRouter func(*http.Request) (*http.Response, error)

func (fn fixtureRouter) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

// 请求、标准客户端快照和后续传输配置必须保留同一会话包装器。
func TestHTTPRouterSurvivesConfigurationAndSnapshot(t *testing.T) {
	h := NewHTTP(nil)
	calls, closed := 0, 0
	h.ConfigureRequestRouter(func(http.RoundTripper) http.RoundTripper {
		return fixtureRouter(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("local")), Request: r}, nil
		})
	}, func() { closed++ })
	h.ConfigureResponseHeaderTimeout(0)
	result, err := h.RequestMethod(t.Context(), "http://example.invalid/", "GET", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = result.Data()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://example.invalid/", nil)
	response, err := h.ClientSnapshot().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if calls != 2 {
		t.Fatalf("router called %d times", calls)
	}
	h.Dispose()
	if closed != 1 {
		t.Fatal("owned router was not disposed")
	}
}

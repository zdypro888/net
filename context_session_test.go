package net

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

type sessionTransport func(*http.Request) (*http.Response, error)

func (f sessionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 同一会话复用 Cookie，但不读取或修改共享客户端的账号 Cookie。
func TestHTTPSessionCookieIsolation(t *testing.T) {
	h := NewHTTP(nil)
	defer h.Dispose()
	original, _ := cookiejar.New(nil)
	u, _ := url.Parse("https://example.test")
	original.SetCookies(u, []*http.Cookie{{Name: "shared", Value: "private"}})
	h.ConfigureCookie(original)
	isolated, _ := cookiejar.New(nil)
	calls := 0
	ctx := ContextWithTransport(Context(context.Background(), h), sessionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		want := ""
		if calls == 2 {
			want = "session=one"
		}
		if r.Header.Get("Cookie") != want {
			t.Fatalf("cookie=%q want=%q", r.Header.Get("Cookie"), want)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": {"session=one; Path=/"}}, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	}))
	ctx = ContextWithHTTPSession(ctx, isolated, func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse })
	for range 2 {
		r, e := Request(ctx, u.String(), nil, nil)
		if e != nil {
			t.Fatal(e)
		}
		r.Close()
	}
	if h.ClientSnapshot().Jar != original || len(original.Cookies(u)) != 1 || original.Cookies(u)[0].Name != "shared" {
		t.Fatal("shared cookie jar changed")
	}
}
func TestHTTPSessionRedirectPolicy(t *testing.T) {
	h := NewHTTP(nil)
	defer h.Dispose()
	calls := 0
	ctx := ContextWithTransport(Context(context.Background(), h), sessionTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://untrusted.test"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}))
	ctx = ContextWithHTTPSession(ctx, nil, func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse })
	r, e := Request(ctx, "https://example.test", nil, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if r.StatusCode != 302 || calls != 1 {
		t.Fatalf("status=%d requests=%d", r.StatusCode, calls)
	}
	if h.ClientSnapshot().CheckRedirect != nil {
		t.Fatal("shared redirect policy changed")
	}
}

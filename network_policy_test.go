package net

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNetworkPolicyIsolatedRoutesAndNoFallback(t *testing.T) {
	var direct, proxied atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1); w.WriteHeader(204) }))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		if r.Method != "CONNECT" {
			t.Error("proxy bypassed CONNECT")
		}
		w.WriteHeader(502)
	}))
	defer proxy.Close()
	config := NetworkPolicyConfig{Default: NetworkRoute{Mode: "proxy", Proxy: &Proxy{Address: proxy.URL}}, Steps: map[string]NetworkRoute{"public": {Mode: "direct"}}}
	policy, err := NewNetworkPolicy(config)
	if err != nil {
		t.Fatal(err)
	}
	defer policy.CloseIdleConnections()
	// 创建后修改原配置不得更改策略；否则不同任务会复用错误出口。
	config.Default.Proxy.Address = "http://127.0.0.1:1"
	config.Steps["public"] = NetworkRoute{Mode: "proxy"}
	h := NewHTTP(nil)
	defer h.Dispose()
	ctx := ContextWithNetworkPolicy(Context(context.Background(), h), policy)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			response, err := h.RequestMethod(WithNetworkStep(ctx, "public"), target.URL, "GET", nil, nil)
			if err != nil {
				t.Error(err)
				return
			}
			response.Close()
		})
		wg.Go(func() {
			response, err := h.RequestMethod(WithNetworkStep(ctx, "private"), target.URL, "GET", nil, nil)
			if response != nil {
				response.Close()
			}
			if err == nil {
				t.Error("failed proxy fell back to direct")
			}
		})
	}
	wg.Wait()
	if direct.Load() != 8 || proxied.Load() != 8 {
		t.Fatalf("routes direct=%d proxy=%d", direct.Load(), proxied.Load())
	}
	// 显式路由不能污染原 HTTP 客户端；脱离上下文后恢复其原行为。
	response, err := h.RequestMethod(context.Background(), target.URL, "GET", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Close()
}
func TestNetworkPolicyRejectsInvalidConfiguration(t *testing.T) {
	for _, r := range []NetworkRoute{{Mode: "typo"}, {Mode: "proxy"}, {Mode: "direct", Proxy: &Proxy{Address: "http://localhost"}}, {Mode: "proxy", Proxy: &Proxy{Address: "ftp://localhost"}}} {
		if p, err := NewNetworkPolicy(NetworkPolicyConfig{Default: r}); err == nil {
			p.CloseIdleConnections()
			t.Fatalf("accepted invalid route %+v", r)
		}
	}
}

// 请求覆盖必须跟随重定向重新匹配，不能把首次请求的直连设置泄漏给后续目标。
func TestHTTPRequestRoutesRedirectAndMethod(t *testing.T) {
	var targetHits, proxyHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/finish?token=temporary", http.StatusFound)
			return
		}
		w.WriteHeader(204)
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyHits.Add(1); w.WriteHeader(502) }))
	defer proxy.Close()
	policy, err := NewNetworkPolicy(NetworkPolicyConfig{Default: NetworkRoute{Mode: "direct"}, Requests: []HTTPRequestRoute{
		{Method: "GET", URL: target.URL + "/finish", Route: NetworkRoute{Mode: "proxy", Proxy: &Proxy{Address: proxy.URL}}},
		{Method: "GET", URL: target.URL + "/finish", Step: "allowed", Route: NetworkRoute{Mode: "direct"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.CloseIdleConnections()
	h := NewHTTP(nil)
	defer h.Dispose()
	ctx := ContextWithNetworkPolicy(context.Background(), policy)
	r, err := h.RequestMethod(ctx, target.URL+"/start", "GET", nil, nil)
	if r != nil {
		r.Close()
	}
	if err == nil {
		t.Fatal("redirect bypassed request proxy")
	}
	for _, test := range []struct {
		ctx    context.Context
		method string
	}{{ctx, "POST"}, {WithNetworkStep(ctx, "allowed"), "GET"}} {
		r, err := h.RequestMethod(test.ctx, target.URL+"/finish?token=x", test.method, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
	}
	if targetHits.Load() != 3 || proxyHits.Load() != 1 {
		t.Fatalf("target=%d proxy=%d", targetHits.Load(), proxyHits.Load())
	}
}

func TestHTTPRequestRouteValidation(t *testing.T) {
	for _, rules := range [][]HTTPRequestRoute{
		{{Method: "GET", URL: "https://example.com/path?secret=x"}},
		{{Method: "GET", URL: "https://user:secret@example.com/path"}},
		{{Method: "BAD\x01", URL: "https://example.com/path"}},
		{{Method: "GET", URL: "https://example.com/path"}, {Method: "get", URL: "https://EXAMPLE.com:443/path"}},
	} {
		p, err := NewNetworkPolicy(NetworkPolicyConfig{Requests: rules})
		if err == nil {
			p.CloseIdleConnections()
			t.Fatal("invalid request routes accepted")
		}
	}
}

// 同一地址的两轮交互必须独立选路，且不能改动签名头和请求正文。
func TestOperationRoutesAndRedactedObserver(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "signed-bytes" || r.Header.Get("X-Signature") != "unchanged" {
			t.Error("protocol bytes changed")
		}
		calls.Add(1)
		w.WriteHeader(204)
	}))
	defer server.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer proxy.Close()
	p, err := NewNetworkPolicy(NetworkPolicyConfig{Default: NetworkRoute{Mode: "direct"}, Operations: map[string]NetworkRoute{"complete": {Mode: "proxy", Proxy: &Proxy{Address: strings.Replace(proxy.URL, "http://", "http://user:secret@", 1)}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseIdleConnections()
	var events []NetworkEvent
	ctx := ContextWithNetworkObserver(ContextWithNetworkPolicy(context.Background(), p), func(e NetworkEvent) { events = append(events, e) })
	h := NewHTTP(nil)
	defer h.Dispose()
	for _, op := range []string{"init", "complete"} {
		response, err := h.RequestMethod(WithNetworkOperation(ctx, op), server.URL+"/private-user?token=secret", "POST", http.Header{"X-Signature": []string{"unchanged"}}, strings.NewReader("signed-bytes"))
		if response != nil {
			response.Close()
		}
		if (op == "complete") != (err != nil) {
			t.Fatalf("%s: %v", op, err)
		}
	}
	if calls.Load() != 1 || len(events) != 2 || events[1].Match != "operation:complete" || events[1].Error == "" {
		t.Fatalf("bad routing: %#v", events)
	}
	encoded, _ := json.Marshal(events)
	for _, secret := range []string{"secret", "private-user", "signed-bytes", "unchanged"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("trace disclosed secret")
		}
	}
}

func TestObserverWorksWithoutPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	var event NetworkEvent
	ctx := ContextWithNetworkObserver(context.Background(), func(e NetworkEvent) { event = e })
	h := NewHTTP(nil)
	defer h.Dispose()
	r, err := h.RequestMethod(ctx, server.URL, "GET", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if event.Status != 204 || event.Exit != "caller-transport" {
		t.Fatalf("missing default trace: %#v", event)
	}
}

func TestRedirectDoesNotInheritOperationOverride(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/end", 302)
			return
		}
		t.Error("redirect bypassed proxy")
		w.WriteHeader(204)
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer proxy.Close()
	p, err := NewNetworkPolicy(NetworkPolicyConfig{Default: NetworkRoute{Mode: "proxy", Proxy: &Proxy{Address: proxy.URL}}, Operations: map[string]NetworkRoute{"start": {Mode: "direct"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseIdleConnections()
	h := NewHTTP(nil)
	defer h.Dispose()
	r, err := h.RequestMethod(WithNetworkOperation(ContextWithNetworkPolicy(context.Background(), p), "start"), target.URL+"/start", "GET", nil, nil)
	if r != nil {
		r.Close()
	}
	if err == nil {
		t.Fatal("redirect inherited direct operation route")
	}
}

package net

import (
	"context"
	"fmt"
	rawnet "net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NetworkRoute 明确选择沿用调用方出口、强制直连或指定代理；代理失败不会降级直连。
type NetworkRoute struct {
	Mode  string `json:"mode"`
	Proxy *Proxy `json:"proxy,omitempty"`
}

// NetworkPolicyConfig 为整个流程设置默认出口，并按步骤覆盖。空模式等同 inherit。
// HTTPRequestRoute 按方法及不含查询参数的接口地址匹配；Step 可区分同接口在不同阶段的调用。
type HTTPRequestRoute struct {
	Method string       `json:"method"`
	URL    string       `json:"url"`
	Step   string       `json:"step,omitempty"`
	Route  NetworkRoute `json:"route"`
}

type NetworkPolicyConfig struct {
	Operations map[string]NetworkRoute `json:"operations,omitempty"`
	Requests   []HTTPRequestRoute      `json:"requests,omitempty"`
	Default    NetworkRoute            `json:"default"`
	Steps      map[string]NetworkRoute `json:"steps,omitempty"`
}

type networkPolicyKey struct{}
type networkStepKey struct{}
type networkOperationKey struct{}
type networkRouteState struct {
	route     NetworkRoute
	transport *http.Transport
	rule      string
}

// NetworkPolicy 是配置的独立快照；一轮流程共用连接池，结束后关闭空闲连接。
type requestRouteKey struct{ method, endpoint, step string }

type NetworkPolicy struct {
	operations map[string]networkRouteState
	requests   map[requestRouteKey]networkRouteState
	fallback   networkRouteState
	steps      map[string]networkRouteState
}

// NewNetworkPolicy 校验并冻结所有出口，避免并发修改共享 HTTP 客户端。
func NewNetworkPolicy(config NetworkPolicyConfig) (*NetworkPolicy, error) {
	p := &NetworkPolicy{steps: make(map[string]networkRouteState), requests: make(map[requestRouteKey]networkRouteState), operations: make(map[string]networkRouteState)}
	var err error
	if p.fallback, err = newNetworkRoute(config.Default); err != nil {
		return nil, fmt.Errorf("default network route: %w", err)
	}
	for step, route := range config.Steps {
		if step == "" {
			p.CloseIdleConnections()
			return nil, fmt.Errorf("network step is empty")
		}
		state, e := newNetworkRoute(route)
		if e != nil {
			p.CloseIdleConnections()
			return nil, fmt.Errorf("network step %q: %w", step, e)
		}
		p.steps[step] = state
	}
	for name, route := range config.Operations {
		if name == "" {
			p.CloseIdleConnections()
			return nil, fmt.Errorf("HTTP operation is empty")
		}
		state, err := newNetworkRoute(route)
		if err != nil {
			p.CloseIdleConnections()
			return nil, fmt.Errorf("HTTP operation %q: %w", name, err)
		}
		p.operations[name] = state
	}
	for i, rule := range config.Requests {
		u, err := url.Parse(rule.URL)
		method := strings.ToUpper(strings.TrimSpace(rule.Method))
		_, requestErr := http.NewRequest(method, rule.URL, nil)
		if err != nil || requestErr != nil || strings.ContainsAny(rule.URL, "?#") || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || method == "" || strings.ContainsAny(method, " ()<>@,;:\"/[]?={}\t\r\n") {
			p.CloseIdleConnections()
			return nil, fmt.Errorf("invalid HTTP request route at index %d: use a method and absolute HTTP URL without credentials, query or fragment", i)
		}
		key := requestRouteKey{method, requestEndpoint(u), rule.Step}
		if _, exists := p.requests[key]; exists {
			p.CloseIdleConnections()
			return nil, fmt.Errorf("duplicate HTTP request route at index %d", i)
		}
		state, err := newNetworkRoute(rule.Route)
		if err != nil {
			p.CloseIdleConnections()
			return nil, fmt.Errorf("HTTP request route %d: %w", i, err)
		}
		state.rule = fmt.Sprintf("request:%d", i+1)
		p.requests[key] = state
	}
	return p, nil
}
func newNetworkRoute(route NetworkRoute) (networkRouteState, error) {
	s := networkRouteState{route: route}
	switch route.Mode {
	case "", "inherit":
		if route.Proxy != nil {
			return s, fmt.Errorf("inherit route cannot contain a proxy")
		}
		return s, nil
	case "direct":
		if route.Proxy != nil {
			return s, fmt.Errorf("direct route cannot contain a proxy")
		}
	case "proxy":
		if route.Proxy == nil || route.Proxy.Address == "" {
			return s, fmt.Errorf("proxy route requires a proxy")
		}
		proxy := *route.Proxy
		if proxy.TLSConfig != nil {
			proxy.TLSConfig = proxy.TLSConfig.Clone()
		}
		u, err := proxy.resolve()
		if err != nil {
			return s, err
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h", "ws", "wss":
		default:
			return s, fmt.Errorf("unsupported proxy scheme")
		}
		s.route.Proxy = &proxy
	default:
		return s, fmt.Errorf("unknown network route mode %q", route.Mode)
	}
	// 新连接池明确清除环境代理，不继承其它任务的代理或自定义拨号器。
	s.transport = &http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second, DialContext: (&rawnet.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext}
	if s.route.Proxy != nil {
		s.transport.DialContext = s.route.Proxy.DialContext
	}
	return s, nil
}

// ContextWithNetworkPolicy 只影响该上下文及其子调用，不修改全局网络状态。
func ContextWithNetworkPolicy(ctx context.Context, p *NetworkPolicy) context.Context {
	return context.WithValue(ctx, networkPolicyKey{}, p)
}

// WithNetworkStep 为 HTTP 和原始 TCP 调用标记出口步骤，嵌套步骤可独立覆盖。
func WithNetworkStep(ctx context.Context, step string) context.Context {
	return context.WithValue(ctx, networkStepKey{}, step)
}

// WithNetworkOperation 仅标记一次具体 HTTP 交互；在发送点使用，不能包住含子请求的整个握手。
func WithNetworkOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, networkOperationKey{}, operation)
}

func selectedNetworkRoute(ctx context.Context) (networkRouteState, bool) {
	p, _ := ctx.Value(networkPolicyKey{}).(*NetworkPolicy)
	if p == nil {
		return networkRouteState{}, false
	}
	step, _ := ctx.Value(networkStepKey{}).(string)
	s, ok := p.steps[step]
	if !ok {
		s = p.fallback
	}
	return s, true
}

// NetworkRouteFromContext 返回出口快照，供 APNs 等非 HTTP 通道复用代理选择。
func NetworkRouteFromContext(ctx context.Context) (NetworkRoute, bool) {
	s, ok := selectedNetworkRoute(ctx)
	r := s.route
	if r.Proxy != nil {
		copy := *r.Proxy
		r.Proxy = &copy
	}
	return r, ok
}

// requestEndpoint 忽略查询参数，避免临时 token 进入配置；路径保持大小写及转义语义。
func requestEndpoint(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		host = strings.ToLower(u.Hostname())
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + host + path
}

type networkRoundTripper struct {
	policy    *NetworkPolicy
	step      string
	operation string
	observer  *networkObserver
	fallback  http.RoundTripper
}

// RoundTrip 在实际发送时选择出口，因此重定向后的请求也按目标方法和地址匹配。
// 匹配顺序：具体交互、带阶段的请求规则、通用请求规则、阶段出口、默认出口；inherit 始终指调用方原出口。
func (t *networkRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	key := requestRouteKey{r.Method, requestEndpoint(r.URL), t.step}
	operation := t.operation
	// 重定向不是原握手交互，按目标接口规则重新匹配。
	if r.Response != nil {
		operation = ""
	}
	state, ok := t.policy.operations[operation]
	match := "operation:" + operation
	if !ok {
		state, ok = t.policy.requests[key]
		match = state.rule
	}
	if !ok {
		key.step = ""
		state, ok = t.policy.requests[key]
		match = state.rule
	}
	if !ok {
		state, ok = t.policy.steps[t.step]
		match = "step:" + t.step
	}
	if !ok {
		state = t.policy.fallback
		match = "default"
	}
	transport := t.fallback
	if state.transport != nil {
		transport = state.transport
	}
	started := time.Now()
	response, err := transport.RoundTrip(r)
	if t.observer != nil {
		event := NetworkEvent{Operation: operation, EndpointID: endpointID(r.URL), Step: t.step, Method: r.Method, Host: r.URL.Hostname(), Match: match, Mode: state.route.Mode, DurationMS: time.Since(started).Milliseconds()}
		if event.Mode == "" || event.Mode == "inherit" {
			event.Mode = "inherit"
			// 任意调用方 transport 可能使用临时代理或自定义拨号，不能根据顶层配置猜测出口。
			event.Exit = "caller-transport"
		} else {
			event.Exit = NetworkRouteLabel(state.route)
		}
		if response != nil {
			event.Status = response.StatusCode
		}
		if err != nil {
			event.Error = "transport"
			if r.Context().Err() != nil {
				event.Error = "cancelled"
			}
			if e, ok := err.(interface{ Timeout() bool }); ok && e.Timeout() {
				event.Error = "timeout"
			}
		}
		t.observer.report(event)
	}
	return response, err
}

// NetworkTransport 为普通 HTTP 客户端安装逐请求路由，不修改共享客户端。
func NetworkTransport(ctx context.Context, fallback http.RoundTripper) http.RoundTripper {
	p, _ := ctx.Value(networkPolicyKey{}).(*NetworkPolicy)
	observer, _ := ctx.Value(networkObserverKey{}).(*networkObserver)
	if p == nil && observer == nil {
		return fallback
	}
	if p == nil {
		p = &NetworkPolicy{}
	}
	if fallback == nil {
		fallback = http.DefaultTransport
	}
	// 同一调用链再次包装时保留真正的原出口，避免 inherit 落回旧规则。
	if previous, ok := fallback.(*networkRoundTripper); ok {
		fallback = previous.fallback
	}
	step, _ := ctx.Value(networkStepKey{}).(string)
	operation, _ := ctx.Value(networkOperationKey{}).(string)
	return &networkRoundTripper{policy: p, step: step, operation: operation, observer: observer, fallback: fallback}
}

// CloseIdleConnections 释放本轮策略创建的空闲连接，不中断正在执行的请求。
func (p *NetworkPolicy) CloseIdleConnections() {
	if p == nil {
		return
	}
	if p.fallback.transport != nil {
		p.fallback.transport.CloseIdleConnections()
	}
	for _, s := range p.operations {
		if s.transport != nil {
			s.transport.CloseIdleConnections()
		}
	}
	for _, s := range p.requests {
		if s.transport != nil {
			s.transport.CloseIdleConnections()
		}
	}
	for _, s := range p.steps {
		if s.transport != nil {
			s.transport.CloseIdleConnections()
		}
	}
}
